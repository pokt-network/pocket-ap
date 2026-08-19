// Package health serves the admin endpoint: an honest answer to "can this proxy
// actually relay right now?", plus counters for what it has done since it
// started.
//
// Nothing here is stored. The counters live in memory for the lifetime of the
// process and are gone on restart; nothing is written to disk and nothing is
// sent anywhere. That is deliberate — an observation pipeline lives in SAGE and
// is intentionally absent from pocket-ap (see CLAUDE.md). This is process
// introspection, not observability infrastructure.
//
// The endpoint never calls the full node. Probes are frequent, and a health
// check that made a network call would turn every prober into load on the node,
// so it reports what the block poller already knows.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/internal/safego"
	"github.com/pokt-network/pocket-ap/relay"
	"github.com/pokt-network/pocket-ap/transport"
)

// stalenessFactor decides when a height counts as stale: the poller refreshes
// every PollInterval, so missing several in a row means the full node is
// unreachable or the poller is wedged.
const stalenessFactor = 3

// Listener hardening, same reasoning as the relay listeners.
const (
	readHeaderTimeout = 10 * time.Second
	maxHeaderBytes    = 1 << 20 // 1 MiB
)

// Reporter is the slice of the session manager /health reads.
// Implemented by pocket.SessionManager.
type Reporter interface {
	PollState() (height int64, lastPollAt time.Time, lastPollErr error)
	PollInterval() time.Duration
	CachedSessions() []*domain.Session
}

// Stats counts relay outcomes in memory. It implements relay.Observer, so it
// learns from the same feed a QoS selector would.
type Stats struct {
	mu          sync.RWMutex
	byServiceID map[domain.ServiceID]*serviceStats
	startedAt   time.Time
}

type serviceStats struct {
	Attempts   uint64
	Successes  uint64
	Failures   uint64
	latencySum time.Duration
	lastErr    string
	lastErrAt  time.Time
}

// NewStats returns an empty counter set stamped with the process start time.
func NewStats(startedAt time.Time) *Stats {
	return &Stats{byServiceID: map[domain.ServiceID]*serviceStats{}, startedAt: startedAt}
}

// Compile-time assertion: Stats is wired in as a relay.Observer.
var _ relay.Observer = (*Stats)(nil)

// Observe implements relay.Observer. It only touches counters under a mutex, so
// it does not block the relay — the contract every Observer has to keep.
func (s *Stats) Observe(_ domain.EndpointAddr, o relay.Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc := s.byServiceID[o.ServiceID]
	if svc == nil {
		svc = &serviceStats{}
		s.byServiceID[o.ServiceID] = svc
	}

	svc.Attempts++
	svc.latencySum += o.Latency
	if o.Success {
		svc.Successes++
		return
	}
	svc.Failures++
	if o.Err != nil {
		svc.lastErr = o.Err.Error()
		svc.lastErrAt = time.Now()
	}
}

// Server is the admin listener. It binds its own port rather than mounting on a
// relay listener: transport.HTTP proxies every path it receives, so adding
// /health there would steal a route from the service being proxied — a Cosmos
// REST backend really does serve /health.
type Server struct {
	addr     string
	hosts    transport.HostPolicy
	apps     []AppInfo
	reporter Reporter
	stats    *Stats
	logger   *slog.Logger
	srv      *http.Server
}

// AppInfo is one configured app and the service it is staked for. The list
// replaces a single app address: with multi-app there is no "the" app, and the
// pairing is what an operator actually needs to see — which key is paying for
// which service.
type AppInfo struct {
	Address   string
	ServiceID domain.ServiceID
}

// New builds the admin server. Services are reported in the order the apps are
// given.
func New(addr string, apps []AppInfo, reporter Reporter, stats *Stats) *Server {
	return &Server{
		addr: addr,
		// Same rebinding defence as the relay listeners. This endpoint is read-only
		// and spends no relays, but it reports the app address, live session IDs,
		// endpoint counts and per-service counters — and a rebound page is
		// same-origin to the browser, so it would otherwise be readable by any site
		// the operator happens to visit. An admin port is not a reason to skip the
		// check; it is a reason to apply it.
		hosts:    transport.HostPolicy{BindAddr: addr},
		apps:     apps,
		reporter: reporter,
		stats:    stats,
		logger:   slog.Default().With("component", "health", "addr", addr),
	}
}

// HealthPath is where the endpoint lives.
//
// Namespaced under /pocket-ap/ deliberately. The admin listener has its own port
// and proxies nothing, so a bare /health would not collide today — but this path
// is unambiguous in logs and dashboards, it stays correct if someone later fronts
// this with a shared ingress, and it can never be confused with a /health route
// belonging to a service being proxied (Cosmos REST backends really serve one).
const HealthPath = "/pocket-ap/health"

// Handler builds the admin routes. Split out from Serve so tests can drive the
// real routing without binding a port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, s.guard(s.handleHealth))
	// Anything else on the admin port is a mistake worth explaining: the path is
	// namespaced, so a bare /health lands here.
	mux.HandleFunc("/", s.guard(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pocket-ap admin: nothing here. try "+HealthPath, http.StatusNotFound)
	}))
	return mux
}

// Serve starts the admin listener and blocks until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	s.srv = &http.Server{
		Addr:    s.addr,
		Handler: s.Handler(),
		// Bounds a client that opens a connection and dawdles over the headers.
		// No ReadTimeout/WriteTimeout: those would sever long-lived streams and
		// websockets, which are the point of this listener.
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	s.logger.Info("health endpoint listening")
	errCh := make(chan error, 1)
	// Call rather than Recover, same as the relay listeners: a panic has to reach
	// errCh or Serve blocks forever on a channel nothing writes to.
	go func() { errCh <- safego.Call(s.logger, "health.listen", s.srv.ListenAndServe) }()

	select {
	case <-ctx.Done():
		return s.Close(context.Background())
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// Close gracefully shuts the admin listener down.
func (s *Server) Close(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// guard rejects a request whose Host we do not answer to, before it reaches any
// handler. See the hosts field for why an admin port still needs this.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts.Allows(r.Host) {
			s.logger.Warn("rejected admin request for an unrecognised Host", "host", r.Host)
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// Response is the /health body.
type Response struct {
	Status string `json:"status"` // "ok" | "degraded"
	Reason string `json:"reason,omitempty"`
	// Apps replaced a single app_address field when multi-app landed. Even with
	// one app it is the more useful shape: the address alone never said which
	// service that key was paying for.
	Apps          []AppHealth     `json:"apps"`
	UptimeSeconds float64         `json:"uptime_seconds"`
	Chain         ChainHealth     `json:"chain"`
	Services      []ServiceHealth `json:"services"`
	// RecoveredPanics is the reason safego counts at all. Containing a panic on a
	// detached goroutine turns a loud crash into a silent one, so the count has
	// to surface somewhere an operator already looks. Omitted while zero, which
	// is the only value a healthy process ever has: nothing here is expected to
	// panic, and a non-zero value means a frame, a relay or a background task was
	// abandoned partway. It does NOT set degraded — the work that was abandoned
	// decides that (a dead poller shows up as a stale height either way), and a
	// long-lived process must not sit at 503 forever over one recovered frame.
	RecoveredPanics uint64 `json:"recovered_panics,omitempty"`
}

// AppHealth pairs a configured app with the service it signs for.
type AppHealth struct {
	Address   string `json:"address"`
	ServiceID string `json:"service_id"`
}

// ChainHealth is what the block poller knows. HeightAgeSeconds is the honest
// signal: if it climbs past the staleness bound, sessions stop rotating.
type ChainHealth struct {
	BlockHeight      int64   `json:"block_height"`
	HeightAgeSeconds float64 `json:"height_age_seconds"`
	PollIntervalSecs float64 `json:"poll_interval_seconds"`
	LastPollError    string  `json:"last_poll_error,omitempty"`
}

// ServiceHealth pairs a configured service with its cached session (if any) and
// its counters. Counters are since process start and are not persisted.
type ServiceHealth struct {
	ServiceID string `json:"service_id"`

	SessionID        string `json:"session_id,omitempty"`
	SessionEndHeight int64  `json:"session_end_height,omitempty"`
	Endpoints        int    `json:"endpoints,omitempty"`
	SessionCached    bool   `json:"session_cached"`

	Attempts          uint64  `json:"attempts"`
	Successes         uint64  `json:"successes"`
	Failures          uint64  `json:"failures"`
	MeanLatencyMillis float64 `json:"mean_latency_ms,omitempty"`
	LastError         string  `json:"last_error,omitempty"`
	// LastErrorAgeSeconds accompanies LastError because the message alone is
	// misleading: it gives no way to tell a failure a second ago from one three
	// days ago, on a counter set that never resets while the process lives.
	LastErrorAgeSeconds float64 `json:"last_error_age_seconds,omitempty"`
}

// handleHealth reports readiness. It returns 503 when the proxy cannot be
// trusted to relay, which makes it a readiness probe — NOT a liveness probe.
// Restarting the process will not fix an unreachable full node, so wiring this
// to a liveness check just produces a restart loop during an upstream outage.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := s.snapshot(time.Now())

	code := http.StatusOK
	if resp.Status != "ok" {
		code = http.StatusServiceUnavailable
	}

	body, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		http.Error(w, "encode health: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}

// snapshot builds the response. Split from the handler so it is testable without
// a live listener.
func (s *Server) snapshot(now time.Time) Response {
	height, lastPollAt, lastPollErr := s.reporter.PollState()
	interval := s.reporter.PollInterval()

	chain := ChainHealth{
		BlockHeight:      height,
		PollIntervalSecs: interval.Seconds(),
	}
	if lastPollErr != nil {
		chain.LastPollError = lastPollErr.Error()
	}

	status, reason := "ok", ""
	switch {
	case lastPollAt.IsZero():
		// Start() polls synchronously before listeners come up, so this means the
		// very first poll failed: we have never known the chain head.
		status, reason = "degraded", "no successful block-height poll yet"
	default:
		age := now.Sub(lastPollAt)
		chain.HeightAgeSeconds = age.Seconds()
		if bound := time.Duration(stalenessFactor) * interval; age > bound {
			status = "degraded"
			reason = fmt.Sprintf("block height is stale: last poll %s ago, expected every %s", age.Truncate(time.Second), interval)
		}
	}

	// Index cached sessions so a configured service with no session yet still
	// appears (lazily fetched on first relay, so absent != broken).
	sessions := map[domain.ServiceID]*domain.Session{}
	for _, sess := range s.reporter.CachedSessions() {
		sessions[sess.ServiceID] = sess
	}

	out := Response{
		Status:        status,
		Reason:        reason,
		Apps:          make([]AppHealth, 0, len(s.apps)),
		UptimeSeconds: now.Sub(s.stats.startedAt).Seconds(),
		Chain:         chain,
		Services:      make([]ServiceHealth, 0, len(s.apps)),

		RecoveredPanics: safego.Panics(),
	}

	for _, app := range s.apps {
		out.Apps = append(out.Apps, AppHealth{Address: app.Address, ServiceID: string(app.ServiceID)})
	}

	for _, app := range s.apps {
		id := app.ServiceID
		sh := ServiceHealth{ServiceID: string(id)}
		if sess, ok := sessions[id]; ok {
			sh.SessionCached = true
			sh.SessionID = sess.ID
			sh.SessionEndHeight = sess.EndBlockHeight
			sh.Endpoints = len(sess.Endpoints)
		}
		s.stats.fill(id, &sh, now)
		out.Services = append(out.Services, sh)
	}
	return out
}

// fill copies the counters for one service into sh.
func (s *Stats) fill(id domain.ServiceID, sh *ServiceHealth, now time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	svc := s.byServiceID[id]
	if svc == nil {
		return
	}
	sh.Attempts = svc.Attempts
	sh.Successes = svc.Successes
	sh.Failures = svc.Failures
	sh.LastError = svc.lastErr
	if !svc.lastErrAt.IsZero() {
		sh.LastErrorAgeSeconds = now.Sub(svc.lastErrAt).Seconds()
	}
	if svc.Attempts > 0 {
		sh.MeanLatencyMillis = float64(svc.latencySum.Microseconds()) / float64(svc.Attempts) / 1000
	}
}
