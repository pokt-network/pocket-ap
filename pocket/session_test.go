package pocket

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

func init() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) }

// fakeNode stands in for the chain. It counts calls, because the interesting
// behaviour here is when we DON'T talk to the node.
type fakeNode struct {
	mu sync.Mutex

	sessions     map[string]*sessiontypes.Session // by serviceID
	sessionCalls int
	sessionErr   error

	height     int64
	heightCall int
	heightErr  error

	app      *apptypes.Application
	appCalls int
	appErr   error

	// onSession runs inside GetSession, for racing the cache.
	onSession func()
}

func (f *fakeNode) GetSession(_ context.Context, serviceID, _ string) (*sessiontypes.Session, error) {
	f.mu.Lock()
	f.sessionCalls++
	err, on := f.sessionErr, f.onSession
	s := f.sessions[serviceID]
	f.mu.Unlock()

	if on != nil {
		on()
	}
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("no session for " + serviceID)
	}
	return s, nil
}

func (f *fakeNode) GetApp(_ context.Context, _ string) (*apptypes.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appCalls++
	if f.appErr != nil {
		return nil, f.appErr
	}
	return f.app, nil
}

func (f *fakeNode) GetCurrentBlockHeight(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heightCall++
	if f.heightErr != nil {
		return 0, f.heightErr
	}
	return f.height, nil
}

func (f *fakeNode) counts() (sessions, heights, apps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionCalls, f.heightCall, f.appCalls
}

func (f *fakeNode) setHeight(h int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.height = h
}

// sessionEnding builds a session for serviceID that ends at endHeight.
func sessionEnding(serviceID, id string, endHeight int64) *sessiontypes.Session {
	s := session(serviceID, map[string]map[sharedtypes.RPCType]string{
		"supA": {sharedtypes.RPCType_JSON_RPC: "https://a/rpc"},
	})
	s.SessionId = id
	s.Header.SessionEndBlockHeight = endHeight
	return s
}

func newFakeManager(node *fakeNode) *SessionManager {
	sm, err := NewSessionManager(node, []ServiceApp{{ServiceID: "svc", AppAddr: "pokt1app"}})
	if err != nil {
		panic(err)
	}
	return sm
}

// newFakeManagerFor builds a manager over several (service, app) pairs.
func newFakeManagerFor(t *testing.T, node *fakeNode, apps ...ServiceApp) *SessionManager {
	t.Helper()
	sm, err := NewSessionManager(node, apps)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	return sm
}

// --- caching / rotation ------------------------------------------------------

// The cache is what keeps a gRPC call off the relay hot path. If it stops
// holding, every relay pays a round trip and nothing fails loudly.
func TestSession_CachesAfterFirstFetch(t *testing.T) {
	node := &fakeNode{
		sessions: map[string]*sessiontypes.Session{"svc": sessionEnding("svc", "s1", 100)},
		height:   50,
	}
	sm := newFakeManager(node)

	for i := 0; i < 5; i++ {
		got, err := sm.Session(context.Background(), "svc")
		if err != nil {
			t.Fatalf("Session: %v", err)
		}
		if got.ID != "s1" {
			t.Fatalf("session = %q, want s1", got.ID)
		}
	}
	if n, _, _ := node.counts(); n != 1 {
		t.Errorf("GetSession called %d times, want 1 — the cache is not holding", n)
	}
}

// A fresh process has an empty cache and a zero height. `call` depends on this:
// it never starts the poller, so height stays 0 and the cache always misses —
// which must still produce a session rather than looking expired forever.
func TestSession_ColdMissFetchesWithoutAPolledHeight(t *testing.T) {
	node := &fakeNode{
		sessions: map[string]*sessiontypes.Session{"svc": sessionEnding("svc", "s1", 100)},
		// height deliberately 0: no poll has run.
	}
	sm := newFakeManager(node)

	got, err := sm.Session(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.ID != "s1" {
		t.Errorf("session = %q, want s1", got.ID)
	}
	if _, h, _ := node.counts(); h != 0 {
		t.Errorf("GetCurrentBlockHeight called %d times, want 0 — a cache miss must not poll", h)
	}
}

// Rotation is driven by height crossing EndBlockHeight. The boundary is
// inclusive: EndBlockHeight is the session's last block, so at that height it is
// done.
func TestSession_RotationBoundary(t *testing.T) {
	tests := []struct {
		name        string
		height      int64
		wantRefetch bool
	}{
		{"well inside the session", 50, false},
		{"one block before the end", 99, false},
		{"exactly at the end height", 100, true},
		{"past the end", 101, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &fakeNode{
				sessions: map[string]*sessiontypes.Session{"svc": sessionEnding("svc", "s1", 100)},
				height:   10,
			}
			sm := newFakeManager(node)

			// Warm the cache while the session is live.
			if _, err := sm.Session(context.Background(), "svc"); err != nil {
				t.Fatalf("warm: %v", err)
			}
			// Move the chain and re-ask.
			node.setHeight(tt.height)
			sm.latestBlockHeight.Store(tt.height)
			if _, err := sm.Session(context.Background(), "svc"); err != nil {
				t.Fatalf("Session: %v", err)
			}

			n, _, _ := node.counts()
			refetched := n > 1
			if refetched != tt.wantRefetch {
				t.Errorf("height %d vs end 100: refetched = %v, want %v", tt.height, refetched, tt.wantRefetch)
			}
		})
	}
}

// After rotation the NEW session must be served and cached, not the stale one.
func TestSession_RotationServesTheNewSession(t *testing.T) {
	node := &fakeNode{
		sessions: map[string]*sessiontypes.Session{"svc": sessionEnding("svc", "old", 100)},
		height:   10,
	}
	sm := newFakeManager(node)

	if _, err := sm.Session(context.Background(), "svc"); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Chain moves past the end, and the node now returns a new session.
	node.mu.Lock()
	node.sessions["svc"] = sessionEnding("svc", "new", 200)
	node.mu.Unlock()
	sm.latestBlockHeight.Store(100)

	got, err := sm.Session(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.ID != "new" {
		t.Fatalf("session = %q, want the rotated one", got.ID)
	}
	if got.EndBlockHeight != 200 {
		t.Errorf("end height = %d, want 200", got.EndBlockHeight)
	}

	// And the new one is now cached.
	before, _, _ := node.counts()
	if _, err := sm.Session(context.Background(), "svc"); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if after, _, _ := node.counts(); after != before {
		t.Error("the rotated session was not cached")
	}
}

// Sessions are keyed per service: one service rotating must not evict another's.
func TestSession_CacheIsPerService(t *testing.T) {
	node := &fakeNode{
		sessions: map[string]*sessiontypes.Session{
			"svc-a": sessionEnding("svc-a", "a1", 100),
			"svc-b": sessionEnding("svc-b", "b1", 100),
		},
		height: 10,
	}
	sm := newFakeManagerFor(t, node, ServiceApp{ServiceID: "svc-a", AppAddr: "pokt1app"}, ServiceApp{ServiceID: "svc-b", AppAddr: "pokt1app"})

	a, err := sm.Session(context.Background(), "svc-a")
	if err != nil {
		t.Fatalf("svc-a: %v", err)
	}
	b, err := sm.Session(context.Background(), "svc-b")
	if err != nil {
		t.Fatalf("svc-b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("both services got the same session")
	}
	if a.ServiceID != "svc-a" || b.ServiceID != "svc-b" {
		t.Errorf("service ids crossed: %q / %q", a.ServiceID, b.ServiceID)
	}
}

func TestSession_FetchErrorPropagates(t *testing.T) {
	node := &fakeNode{sessionErr: errors.New("node unreachable")}
	sm := newFakeManager(node)

	if _, err := sm.Session(context.Background(), "svc"); err == nil {
		t.Fatal("Session hid a fetch failure")
	}
}

// A session with no header cannot be signed against — the signer reads the
// header — so it must be rejected here rather than blowing up later.
func TestSession_RejectsSessionWithNilHeader(t *testing.T) {
	node := &fakeNode{
		sessions: map[string]*sessiontypes.Session{"svc": {SessionId: "s1"}}, // no Header
	}
	sm := newFakeManager(node)

	if _, err := sm.Session(context.Background(), "svc"); err == nil {
		t.Fatal("a session with no header was accepted")
	}
}

// A failed fetch must not poison the cache with a nil entry.
func TestSession_FailedFetchIsNotCached(t *testing.T) {
	node := &fakeNode{sessionErr: errors.New("down")}
	sm := newFakeManager(node)

	_, _ = sm.Session(context.Background(), "svc")

	// Node recovers.
	node.mu.Lock()
	node.sessionErr = nil
	node.sessions = map[string]*sessiontypes.Session{"svc": sessionEnding("svc", "s1", 100)}
	node.mu.Unlock()

	got, err := sm.Session(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Session after recovery: %v", err)
	}
	if got.ID != "s1" {
		t.Errorf("session = %q, want s1", got.ID)
	}
}

// Concurrent relays must not corrupt the cache. (They WILL all fetch on a cold
// miss — Session has no singleflight — which is exactly why the WS expiry
// watcher reads the height instead of calling Session.)
func TestSession_ConcurrentAccessIsSafe(t *testing.T) {
	node := &fakeNode{
		sessions: map[string]*sessiontypes.Session{"svc": sessionEnding("svc", "s1", 100)},
		height:   10,
	}
	sm := newFakeManager(node)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := sm.Session(context.Background(), "svc")
			if err != nil || got.ID != "s1" {
				t.Errorf("Session = %v, %v", got, err)
			}
		}()
	}
	wg.Wait()
}

// --- poll state (what /health and the WS watcher read) -----------------------

func TestPollState_BeforeAnyPoll(t *testing.T) {
	sm := newFakeManager(&fakeNode{})

	height, lastPollAt, err := sm.PollState()
	if height != 0 {
		t.Errorf("height = %d, want 0", height)
	}
	if !lastPollAt.IsZero() {
		t.Errorf("lastPollAt = %v, want zero — /health reports 'no successful poll yet' on this", lastPollAt)
	}
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestPollState_AfterSuccess(t *testing.T) {
	node := &fakeNode{height: 476800}
	sm := newFakeManager(node)

	sm.pollBlockHeight(context.Background())

	height, lastPollAt, err := sm.PollState()
	if height != 476800 {
		t.Errorf("height = %d, want 476800", height)
	}
	if lastPollAt.IsZero() {
		t.Error("lastPollAt is zero after a successful poll")
	}
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if sm.LatestBlockHeight() != 476800 {
		t.Errorf("LatestBlockHeight = %d, want 476800", sm.LatestBlockHeight())
	}
}

// A failed poll keeps the last good height and lastPollAt, so /health can report
// HOW STALE it is rather than pretending it knows nothing.
func TestPollState_FailureKeepsLastGoodHeightAndReportsError(t *testing.T) {
	node := &fakeNode{height: 100}
	sm := newFakeManager(node)
	sm.pollBlockHeight(context.Background())

	_, firstPollAt, _ := sm.PollState()

	node.mu.Lock()
	node.heightErr = errors.New("node unreachable")
	node.mu.Unlock()
	sm.pollBlockHeight(context.Background())

	height, lastPollAt, err := sm.PollState()
	if height != 100 {
		t.Errorf("height = %d, want the last good 100 retained", height)
	}
	if !lastPollAt.Equal(firstPollAt) {
		t.Error("lastPollAt moved on a FAILED poll — /health would report the height as fresh")
	}
	if err == nil {
		t.Error("PollState reports no error after a failed poll")
	}
}

// Recovery must clear the error, or /health stays degraded forever.
func TestPollState_SuccessClearsAPreviousError(t *testing.T) {
	node := &fakeNode{heightErr: errors.New("down")}
	sm := newFakeManager(node)
	sm.pollBlockHeight(context.Background())

	if _, _, err := sm.PollState(); err == nil {
		t.Fatal("expected an error after a failed poll")
	}

	node.mu.Lock()
	node.heightErr = nil
	node.height = 200
	node.mu.Unlock()
	sm.pollBlockHeight(context.Background())

	height, _, err := sm.PollState()
	if err != nil {
		t.Errorf("err = %v, want nil after recovery — /health would stay degraded", err)
	}
	if height != 200 {
		t.Errorf("height = %d, want 200", height)
	}
}

// The WS expiry watcher's safety rests on this: 0 never looks expired, so losing
// the poller must not tear down live bridges.
func TestLatestBlockHeight_ZeroWhenNoPollSucceeded(t *testing.T) {
	sm := newFakeManager(&fakeNode{heightErr: errors.New("down")})
	sm.pollBlockHeight(context.Background())

	if got := sm.LatestBlockHeight(); got != 0 {
		t.Errorf("LatestBlockHeight = %d, want 0 — a dead poller must not look like universal expiry", got)
	}
}

func TestPollInterval(t *testing.T) {
	if got := newFakeManager(&fakeNode{}).PollInterval(); got != blockPollInterval {
		t.Errorf("PollInterval = %v, want %v", got, blockPollInterval)
	}
}

// --- Start / poller ----------------------------------------------------------

// Start polls once synchronously, so the first relay hits a warm height rather
// than racing the ticker.
func TestStart_PollsOnceSynchronously(t *testing.T) {
	node := &fakeNode{height: 500}
	sm := newFakeManager(node)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// No sleeping: the synchronous poll has already happened when Start returns.
	if got := sm.LatestBlockHeight(); got != 500 {
		t.Errorf("height = %d immediately after Start, want 500 — the first poll is not synchronous", got)
	}
}

// A full node that is down at startup must not stop the proxy: /health reports
// it, and the poller keeps retrying.
func TestStart_SurvivesAFailedFirstPoll(t *testing.T) {
	sm := newFakeManager(&fakeNode{heightErr: errors.New("down")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sm.Start(ctx); err != nil {
		t.Errorf("Start failed because the node was down: %v — the proxy should come up and report degraded", err)
	}
}

func TestStart_PollerStopsOnContextCancel(t *testing.T) {
	node := &fakeNode{height: 1}
	sm := newFakeManager(node)

	ctx, cancel := context.WithCancel(context.Background())
	if err := sm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	// Goroutine exit is asynchronous; what matters is that it stops polling.
	_, before, _ := node.counts()
	time.Sleep(50 * time.Millisecond)
	_, after, _ := node.counts()
	if after > before+1 {
		t.Errorf("poller kept polling after cancel: %d -> %d", before, after)
	}
}

// --- CachedSessions (what /health lists) -------------------------------------

func TestCachedSessions_EmptyBeforeAnyRelay(t *testing.T) {
	sm := newFakeManager(&fakeNode{})
	if got := sm.CachedSessions(); len(got) != 0 {
		t.Errorf("CachedSessions = %d, want 0 — sessions are fetched lazily", len(got))
	}
}

func TestCachedSessions_ListsWhatIsHeld(t *testing.T) {
	node := &fakeNode{
		sessions: map[string]*sessiontypes.Session{
			"svc-a": sessionEnding("svc-a", "a1", 100),
			"svc-b": sessionEnding("svc-b", "b1", 100),
		},
		height: 10,
	}
	sm := newFakeManagerFor(t, node, ServiceApp{ServiceID: "svc-a", AppAddr: "pokt1app"}, ServiceApp{ServiceID: "svc-b", AppAddr: "pokt1app"})

	if _, err := sm.Session(context.Background(), "svc-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := sm.Session(context.Background(), "svc-b"); err != nil {
		t.Fatal(err)
	}

	got := sm.CachedSessions()
	if len(got) != 2 {
		t.Fatalf("CachedSessions = %d, want 2", len(got))
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["a1"] || !ids["b1"] {
		t.Errorf("CachedSessions = %v, want both sessions", ids)
	}
}

// --- toDomainSession ---------------------------------------------------------

// domain.Session.Raw carries the poktroll session so the signer can read the
// SessionHeader without domain importing poktroll. Lose it and every signature
// fails at "session missing raw header".
func TestToDomainSession_CarriesRawForTheSigner(t *testing.T) {
	raw := sessionEnding("svc", "s1", 476840)
	got := toDomainSession("svc", "pokt1app", raw)

	if got.ID != "s1" || got.ServiceID != "svc" || got.AppAddr != "pokt1app" {
		t.Errorf("session = %+v", got)
	}
	if got.EndBlockHeight != 476840 {
		t.Errorf("end height = %d, want 476840", got.EndBlockHeight)
	}
	back, ok := got.Raw.(*sessiontypes.Session)
	if !ok {
		t.Fatal("Raw does not hold a *sessiontypes.Session — the signer cannot read the header")
	}
	if back.Header == nil || back.Header.SessionEndBlockHeight != 476840 {
		t.Error("Raw lost the session header")
	}
	if len(got.Endpoints) != 1 {
		t.Errorf("endpoints = %d, want 1", len(got.Endpoints))
	}
}

// --- Signer app cache --------------------------------------------------------

// The app is fetched once and reused: it is needed for every signature, so an
// uncached fetch would put a gRPC round trip on the signing path.
func TestSignerGetApp_CachesAfterFirstFetch(t *testing.T) {
	node := &fakeNode{app: &apptypes.Application{Address: "pokt1app"}}
	s, err := NewSigner(throwawayKey, &FullNode{})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	s.fn = node

	for i := 0; i < 5; i++ {
		app, err := s.getApp(context.Background(), "pokt1app")
		if err != nil {
			t.Fatalf("getApp: %v", err)
		}
		if app.Address != "pokt1app" {
			t.Fatalf("app = %+v", app)
		}
	}
	if _, _, n := node.counts(); n != 1 {
		t.Errorf("GetApp called %d times, want 1 — the app cache is not holding", n)
	}
}

func TestSignerGetApp_ErrorPropagatesAndIsNotCached(t *testing.T) {
	node := &fakeNode{appErr: errors.New("not staked")}
	s, err := NewSigner(throwawayKey, &FullNode{})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	s.fn = node

	if _, err := s.getApp(context.Background(), "pokt1app"); err == nil {
		t.Fatal("getApp hid a fetch failure")
	}

	node.mu.Lock()
	node.appErr = nil
	node.app = &apptypes.Application{Address: "pokt1app"}
	node.mu.Unlock()

	if _, err := s.getApp(context.Background(), "pokt1app"); err != nil {
		t.Errorf("getApp after recovery: %v — a failed fetch poisoned the cache", err)
	}
}
