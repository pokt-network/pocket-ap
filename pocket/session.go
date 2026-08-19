package pocket

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/internal/safego"
	"github.com/pokt-network/pocket-ap/relay"
)

// Compile-time assertion: SessionManager satisfies the SessionSource seam.
var _ relay.SessionSource = (*SessionManager)(nil)

// blockPollInterval is how often we poll chain head. Shannon blocks are ~1min,
// so 10s is responsive to session boundaries without hammering the node.
const blockPollInterval = 10 * time.Second

// SessionManager fetches, caches, and rotates sessions for the configured app.
// Rotation is driven by a background block-height poller, keeping a gRPC call
// off the relay hot path.
//
// LIFT: SAGE protocol/shannon/sessions.go. The SessionExpiredEvent machinery
// (channel, expiredNotified, emitExpiryEvents) is intentionally dropped — it
// only serves WebSocket bridges, which are phase-2.
type SessionManager struct {
	fn nodeClient
	// appByService is the whole of multi-app support in this type. A session
	// belongs to (service, app), and poktroll allows an app exactly one service,
	// so a service picks out its app with no ambiguity and no policy to decide.
	appByService map[domain.ServiceID]string
	services     []domain.ServiceID // configured order, for /health
	logger       *slog.Logger

	sessionCache      sync.Map // "serviceID:appAddr" -> *domain.Session
	latestBlockHeight atomic.Int64
	stopPoller        chan struct{}

	// Poll outcome, for /health. A failing poller is the leading indicator that
	// this proxy is about to stop working: the height goes stale, Session() stops
	// noticing expiry, and relays start going to dead sessions. Recording it here
	// lets /health answer honestly without making a network call of its own.
	pollMu      sync.RWMutex
	lastPollAt  time.Time // last SUCCESSFUL poll
	lastPollErr error     // most recent failure, nil once a poll succeeds
}

// PollState reports the block poller's view of the chain: the newest height it
// saw, when it last succeeded, and the most recent error. A zero lastPollAt
// means no poll has ever succeeded.
func (sm *SessionManager) PollState() (height int64, lastPollAt time.Time, lastPollErr error) {
	sm.pollMu.RLock()
	defer sm.pollMu.RUnlock()
	return sm.latestBlockHeight.Load(), sm.lastPollAt, sm.lastPollErr
}

// PollInterval is how often the poller refreshes the height. /health uses it to
// decide what counts as stale.
func (sm *SessionManager) PollInterval() time.Duration { return blockPollInterval }

// LatestBlockHeight returns the newest chain head the poller has seen, or 0 when
// no poll has ever succeeded.
//
// This is what a WebSocket bridge watches to notice its session ending. It is a
// plain atomic load on purpose: the alternative — having each bridge re-check
// via Session() — would make every bridge miss the cache at the same instant and
// fire its own GetSession, since Session() has no singleflight. One atomic read
// per bridge cannot stampede.
//
// Returning 0 while the poller is down is deliberate: 0 never looks expired, so
// losing sight of the chain does not tear down live bridges on a guess. If the
// session really has ended, the miner rejects the frames and the bridge closes
// on the validation failure instead.
func (sm *SessionManager) LatestBlockHeight() int64 { return sm.latestBlockHeight.Load() }

// CachedSessions returns the sessions held right now. Sessions are fetched
// lazily on first relay, so an empty result means "nothing relayed yet", not
// "broken".
func (sm *SessionManager) CachedSessions() []*domain.Session {
	var out []*domain.Session
	sm.sessionCache.Range(func(_, v any) bool {
		if s, ok := v.(*domain.Session); ok {
			out = append(out, s)
		}
		return true
	})
	return out
}

// ServiceApp binds a service to the app whose stake pays for its relays.
//
// One per service: an application stakes for exactly one service (poktroll's
// ValidateAppServiceConfigs), so two apps on the same service would be stake
// rotation — a different feature, with a policy question this type does not
// answer. NewSessionManager rejects it rather than silently keeping one.
type ServiceApp struct {
	ServiceID domain.ServiceID
	AppAddr   string
}

// NewSessionManager wires a session manager to a full node for a set of apps.
//
// Duplicate services are an error: the caller built an impossible request, and
// dropping one silently would route half the traffic to an app the operator
// thought they had configured.
func NewSessionManager(fn nodeClient, apps []ServiceApp) (*SessionManager, error) {
	appByService := make(map[domain.ServiceID]string, len(apps))
	services := make([]domain.ServiceID, 0, len(apps))
	for _, a := range apps {
		if prev, dup := appByService[a.ServiceID]; dup {
			return nil, fmt.Errorf("sessions: service %s is claimed by two apps (%s and %s) — one app per service, app rotation is not supported", a.ServiceID, prev, a.AppAddr)
		}
		appByService[a.ServiceID] = a.AppAddr
		services = append(services, a.ServiceID)
	}
	return &SessionManager{
		fn:           fn,
		appByService: appByService,
		services:     services,
		logger:       slog.Default().With("component", "sessions"),
		stopPoller:   make(chan struct{}),
	}, nil
}

// Services lists the configured services in config order.
func (sm *SessionManager) Services() []domain.ServiceID { return sm.services }

// AppAddrFor returns the app address that signs for a service.
func (sm *SessionManager) AppAddrFor(serviceID domain.ServiceID) (string, bool) {
	addr, ok := sm.appByService[serviceID]
	return addr, ok
}

// Start polls chain head once synchronously (so the first relay hits a warm
// height), then launches the background poller.
// LIFT: sessions.go:88 StartBlockPoller.
func (sm *SessionManager) Start(ctx context.Context) error {
	sm.pollBlockHeight(ctx)

	safego.Go(sm.logger, "pocket.sessions.blockPoller", func() {
		ticker := time.NewTicker(blockPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Per tick, not around the loop. A panic that killed the ticker
				// would freeze the height rather than stop the process, and a
				// frozen height does not look like an outage: Session() only
				// consults it on a cache hit, so it would keep serving a session
				// the chain has already retired and every relay would fail at the
				// miner. /health's staleness check would be the only witness.
				safego.Run(sm.logger, "pocket.sessions.pollBlockHeight", func() {
					sm.pollBlockHeight(ctx)
				})
			case <-sm.stopPoller:
				return
			case <-ctx.Done():
				return
			}
		}
	})

	sm.logger.Info("block poller started", "interval", blockPollInterval.String())
	return nil
}

// Stop halts the background poller.
func (sm *SessionManager) Stop() { close(sm.stopPoller) }

// pollBlockHeight refreshes the cached chain head that drives rotation, and
// records the outcome for /health.
func (sm *SessionManager) pollBlockHeight(ctx context.Context) {
	height, err := sm.fn.GetCurrentBlockHeight(ctx)

	sm.pollMu.Lock()
	defer sm.pollMu.Unlock()

	if err != nil {
		sm.logger.Warn("block poll failed", "error", err)
		// Keep the last good height and lastPollAt: /health reports how stale it
		// is, which is more useful than pretending we know nothing.
		sm.lastPollErr = err
		return
	}
	sm.latestBlockHeight.Store(height)
	sm.lastPollAt = time.Now()
	sm.lastPollErr = nil
}

// Session returns the current session for a service, refreshing from the full
// node when the cached session's end height has been crossed.
// LIFT: sessions.go:201 getSession + :225 refreshSession + :269 getOrCreateEndpoints.
func (sm *SessionManager) Session(ctx context.Context, serviceID domain.ServiceID) (*domain.Session, error) {
	appAddr, ok := sm.appByService[serviceID]
	if !ok {
		// Better than fetching a session for the wrong app: with several apps
		// configured, "which key pays for this?" has no default answer.
		return nil, fmt.Errorf("session: no app configured for service %s (configured: %v)", serviceID, sm.services)
	}
	key := string(serviceID) + ":" + appAddr

	if cached, ok := sm.sessionCache.Load(key); ok {
		s := cached.(*domain.Session)
		if sm.latestBlockHeight.Load() < s.EndBlockHeight {
			return s, nil
		}
		sm.logger.Info("session expired, refreshing",
			"service_id", serviceID, "session_id", s.ID,
			"end_height", s.EndBlockHeight, "current_height", sm.latestBlockHeight.Load(),
		)
	}

	raw, err := sm.fn.GetSession(ctx, string(serviceID), appAddr)
	if err != nil {
		return nil, fmt.Errorf("session: fetch %s: %w", serviceID, err)
	}
	if raw.Header == nil {
		return nil, fmt.Errorf("session: %s returned session with nil header", serviceID)
	}

	ds := toDomainSession(serviceID, appAddr, raw)
	sm.sessionCache.Store(key, ds)
	sm.logger.Info("session fetched",
		"service_id", serviceID, "session_id", ds.ID, "app", appAddr,
		"end_height", ds.EndBlockHeight, "endpoints", len(ds.Endpoints),
	)
	return ds, nil
}

// toDomainSession converts a raw poktroll session into the domain view, keeping
// the raw session in .Raw for the signer (it reads the SessionHeader).
func toDomainSession(serviceID domain.ServiceID, appAddr string, raw *sessiontypes.Session) *domain.Session {
	return &domain.Session{
		ID:             raw.SessionId,
		ServiceID:      serviceID,
		AppAddr:        appAddr,
		EndBlockHeight: raw.Header.SessionEndBlockHeight,
		Endpoints:      endpointsFromSession(raw),
		Raw:            raw,
	}
}
