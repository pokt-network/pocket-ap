package health

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

// stubReporter stands in for pocket.SessionManager.
type stubReporter struct {
	height     int64
	lastPollAt time.Time
	lastErr    error
	interval   time.Duration
	sessions   []*domain.Session
}

func (s stubReporter) PollState() (int64, time.Time, error) { return s.height, s.lastPollAt, s.lastErr }
func (s stubReporter) PollInterval() time.Duration          { return s.interval }
func (s stubReporter) CachedSessions() []*domain.Session    { return s.sessions }

var now = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func newServer(t *testing.T, r Reporter, stats *Stats) *Server {
	t.Helper()
	if stats == nil {
		stats = NewStats(now.Add(-time.Minute))
	}
	return New("127.0.0.1:0", []AppInfo{{Address: "pokt1app", ServiceID: "svc-a"}}, r, stats)
}

func TestSnapshot_HealthyWhenPollIsFresh(t *testing.T) {
	r := stubReporter{
		height:     476800,
		lastPollAt: now.Add(-2 * time.Second),
		interval:   10 * time.Second,
	}
	got := newServer(t, r, nil).snapshot(now)

	if got.Status != "ok" {
		t.Errorf("status = %q (%s), want ok", got.Status, got.Reason)
	}
	if got.Chain.BlockHeight != 476800 {
		t.Errorf("block height = %d", got.Chain.BlockHeight)
	}
	if got.Chain.HeightAgeSeconds != 2 {
		t.Errorf("height age = %v, want 2", got.Chain.HeightAgeSeconds)
	}
	if len(got.Apps) != 1 || got.Apps[0].Address != "pokt1app" || got.Apps[0].ServiceID != "svc-a" {
		t.Errorf("apps = %+v, want one app pokt1app on svc-a", got.Apps)
	}
	if got.UptimeSeconds != 60 {
		t.Errorf("uptime = %v, want 60", got.UptimeSeconds)
	}
}

// A stale height is the whole point of the endpoint: the poller has stopped
// tracking the chain, so sessions will not rotate and relays are about to break.
func TestSnapshot_DegradedWhenHeightIsStale(t *testing.T) {
	r := stubReporter{
		height:     476800,
		lastPollAt: now.Add(-45 * time.Second), // > 3x the 10s interval
		interval:   10 * time.Second,
		lastErr:    errors.New("node unreachable"),
	}
	got := newServer(t, r, nil).snapshot(now)

	if got.Status != "degraded" {
		t.Errorf("status = %q, want degraded", got.Status)
	}
	if !strings.Contains(got.Reason, "stale") {
		t.Errorf("reason = %q, want it to explain staleness", got.Reason)
	}
	if got.Chain.LastPollError != "node unreachable" {
		t.Errorf("last poll error = %q", got.Chain.LastPollError)
	}
}

// Exactly at the bound is still healthy; one tick past is not.
func TestSnapshot_StalenessBoundary(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"just inside the bound", 30 * time.Second, "ok"},
		{"just past the bound", 31 * time.Second, "degraded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := stubReporter{lastPollAt: now.Add(-tt.age), interval: 10 * time.Second}
			if got := newServer(t, r, nil).snapshot(now); got.Status != tt.want {
				t.Errorf("age %v: status = %q, want %q", tt.age, got.Status, tt.want)
			}
		})
	}
}

// Start() polls synchronously before listeners come up, so a zero lastPollAt
// means the very first poll failed — we have never known the chain head.
func TestSnapshot_DegradedWhenNoPollHasEverSucceeded(t *testing.T) {
	r := stubReporter{interval: 10 * time.Second, lastErr: errors.New("dial refused")}
	got := newServer(t, r, nil).snapshot(now)

	if got.Status != "degraded" {
		t.Errorf("status = %q, want degraded", got.Status)
	}
	if !strings.Contains(got.Reason, "no successful") {
		t.Errorf("reason = %q, want it to say no poll has succeeded", got.Reason)
	}
}

// Sessions are fetched lazily on first relay, so a configured service with no
// session yet must still be listed, and must not read as broken.
func TestSnapshot_ConfiguredServiceWithoutSessionIsListedNotBroken(t *testing.T) {
	r := stubReporter{lastPollAt: now, interval: 10 * time.Second}
	got := newServer(t, r, nil).snapshot(now)

	if got.Status != "ok" {
		t.Errorf("status = %q, want ok — no session yet just means nothing relayed", got.Status)
	}
	if len(got.Services) != 1 {
		t.Fatalf("services = %d, want the configured one listed", len(got.Services))
	}
	if got.Services[0].SessionCached {
		t.Error("SessionCached = true, want false")
	}
	if got.Services[0].ServiceID != "svc-a" {
		t.Errorf("service id = %q", got.Services[0].ServiceID)
	}
}

func TestSnapshot_ReportsCachedSession(t *testing.T) {
	r := stubReporter{
		lastPollAt: now,
		interval:   10 * time.Second,
		sessions: []*domain.Session{{
			ID:             "sess-1",
			ServiceID:      "svc-a",
			EndBlockHeight: 476800,
			Endpoints:      make([]domain.Endpoint, 37),
		}},
	}
	got := newServer(t, r, nil).snapshot(now)

	svc := got.Services[0]
	if !svc.SessionCached || svc.SessionID != "sess-1" {
		t.Errorf("session = %+v, want the cached one reported", svc)
	}
	if svc.SessionEndHeight != 476800 || svc.Endpoints != 37 {
		t.Errorf("end height = %d, endpoints = %d", svc.SessionEndHeight, svc.Endpoints)
	}
}

func TestStats_CountsPerService(t *testing.T) {
	stats := NewStats(now)
	stats.Observe("supA", relay.Outcome{ServiceID: "svc-a", Success: true, Latency: 100 * time.Millisecond})
	stats.Observe("supB", relay.Outcome{ServiceID: "svc-a", Success: true, Latency: 300 * time.Millisecond})
	stats.Observe("supC", relay.Outcome{ServiceID: "svc-a", Success: false, Latency: 200 * time.Millisecond, Err: errors.New("boom")})
	// A different service must not contaminate svc-a's counters.
	stats.Observe("supD", relay.Outcome{ServiceID: "svc-b", Success: false, Latency: time.Second, Err: errors.New("other")})

	r := stubReporter{lastPollAt: now, interval: 10 * time.Second}
	got := newServer(t, r, stats).snapshot(now)

	svc := got.Services[0]
	if svc.Attempts != 3 || svc.Successes != 2 || svc.Failures != 1 {
		t.Errorf("attempts=%d successes=%d failures=%d, want 3/2/1", svc.Attempts, svc.Successes, svc.Failures)
	}
	// (100+300+200)/3 = 200ms
	if svc.MeanLatencyMillis != 200 {
		t.Errorf("mean latency = %v ms, want 200", svc.MeanLatencyMillis)
	}
	if svc.LastError != "boom" {
		t.Errorf("last error = %q, want boom (not svc-b's)", svc.LastError)
	}
}

func TestStats_UntouchedServiceHasZeroCounters(t *testing.T) {
	stats := NewStats(now)
	r := stubReporter{lastPollAt: now, interval: 10 * time.Second}
	got := newServer(t, r, stats).snapshot(now)

	svc := got.Services[0]
	if svc.Attempts != 0 || svc.Successes != 0 || svc.Failures != 0 {
		t.Errorf("counters = %+v, want all zero", svc)
	}
	if svc.LastError != "" {
		t.Errorf("last error = %q, want empty", svc.LastError)
	}
}

// Counters are in-memory and per-process. A fresh Stats starts at zero: nothing
// is loaded from anywhere, because nothing is ever persisted.
func TestStats_StartsEmpty(t *testing.T) {
	if n := len(NewStats(now).byServiceID); n != 0 {
		t.Errorf("new Stats already tracks %d services, want 0", n)
	}
}

// The handler stamps its own time.Now(), so these two use real time rather than
// the fixed clock the snapshot tests drive. Staleness logic is covered there;
// what matters here is the wiring: status code, content type, parseable body.
func TestHandler_200WhenHealthy(t *testing.T) {
	r := stubReporter{lastPollAt: time.Now(), interval: 10 * time.Second}
	srv := newServer(t, r, nil)

	rec := httptest.NewRecorder()
	srv.handleHealth(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q", body.Status)
	}
}

// 503 is what makes this usable as a readiness probe: a load balancer must stop
// sending traffic to a proxy that cannot relay.
func TestHandler_503WhenDegraded(t *testing.T) {
	r := stubReporter{lastPollAt: time.Now().Add(-time.Hour), interval: 10 * time.Second}
	srv := newServer(t, r, nil)

	rec := httptest.NewRecorder()
	srv.handleHealth(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	// The body must still parse: a prober needs the reason, not just the code.
	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("degraded body is not valid JSON: %v", err)
	}
	if body.Reason == "" {
		t.Error("degraded response gave no reason")
	}
}

// The path is namespaced so it can never be mistaken for a proxied service's own
// /health route.
func TestHealthPathIsNamespaced(t *testing.T) {
	if HealthPath != "/pocket-ap/health" {
		t.Errorf("HealthPath = %q, want /pocket-ap/health", HealthPath)
	}
}

// Routing through the real mux, over a real listener.
func TestServer_RoutesOverARealListener(t *testing.T) {
	r := stubReporter{height: 476800, lastPollAt: time.Now(), interval: 10 * time.Second}
	ts := httptest.NewServer(newServer(t, r, nil).Handler())
	defer ts.Close()

	t.Run("namespaced path serves health", func(t *testing.T) {
		resp, err := http.Get(ts.URL + HealthPath)
		if err != nil {
			t.Fatalf("GET %s: %v", HealthPath, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("code = %d, want 200", resp.StatusCode)
		}
		var body Response
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Chain.BlockHeight != 476800 {
			t.Errorf("block height = %d", body.Chain.BlockHeight)
		}
	})

	// A bare /health must NOT answer: that is the whole point of namespacing, and
	// silently serving both would defeat it.
	t.Run("bare /health is not served", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("code = %d, want 404 — /health must not be an alias", resp.StatusCode)
		}
		hint, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(hint), HealthPath) {
			t.Errorf("404 body = %q, want it to point at %s", hint, HealthPath)
		}
	})

	t.Run("unknown path 404s with a hint", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/nonsense")
		if err != nil {
			t.Fatalf("GET /nonsense: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("code = %d, want 404", resp.StatusCode)
		}
	})
}
