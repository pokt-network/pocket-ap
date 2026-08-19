package pocket

import (
	"context"
	"strings"
	"testing"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// The session cache is keyed "service:app", so multi-app is only real if the
// fetch uses the app that owns the service. Getting this wrong is quiet: the
// session comes back for the WRONG app, signing then fails against a session
// that was never issued to it.
func TestSession_FetchesWithTheServicesOwnApp(t *testing.T) {
	node := &recordingSessionNode{fakeNode: fakeNode{
		sessions: map[string]*sessiontypes.Session{
			"svc-a": sessionEnding("svc-a", "a1", 100),
			"svc-b": sessionEnding("svc-b", "b1", 100),
		},
		height: 10,
	}}
	sm, err := NewSessionManager(node, []ServiceApp{
		{ServiceID: "svc-a", AppAddr: "pokt1a"},
		{ServiceID: "svc-b", AppAddr: "pokt1b"},
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	a, err := sm.Session(context.Background(), "svc-a")
	if err != nil {
		t.Fatalf("svc-a: %v", err)
	}
	b, err := sm.Session(context.Background(), "svc-b")
	if err != nil {
		t.Fatalf("svc-b: %v", err)
	}

	if a.AppAddr != "pokt1a" || b.AppAddr != "pokt1b" {
		t.Errorf("sessions carry apps %q and %q, want pokt1a and pokt1b", a.AppAddr, b.AppAddr)
	}
	if got := node.appFor("svc-a"); got != "pokt1a" {
		t.Errorf("GetSession(svc-a) asked for app %q, want pokt1a", got)
	}
	if got := node.appFor("svc-b"); got != "pokt1b" {
		t.Errorf("GetSession(svc-b) asked for app %q, want pokt1b", got)
	}
}

// With several apps configured there is no default answer to "whose stake pays
// for this?", so a service nobody staked for must fail loudly rather than fall
// through to an arbitrary app.
func TestSession_UnknownServiceIsAnError(t *testing.T) {
	node := &fakeNode{sessions: map[string]*sessiontypes.Session{
		"svc-a": sessionEnding("svc-a", "a1", 100),
	}}
	sm, err := NewSessionManager(node, []ServiceApp{{ServiceID: "svc-a", AppAddr: "pokt1a"}})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	_, err = sm.Session(context.Background(), "svc-nope")
	if err == nil {
		t.Fatal("Session succeeded for a service with no configured app")
	}
	if !strings.Contains(err.Error(), "svc-nope") {
		t.Errorf("error = %v, want it to name the service", err)
	}
	if sessions, _, _ := node.counts(); sessions != 0 {
		t.Errorf("GetSession was called %d times for an unconfigured service", sessions)
	}
}

// Two apps on one service is stake rotation, which this type does not implement.
// Silently keeping one would send half the traffic to an app the operator
// believes is configured.
func TestNewSessionManager_RejectsTwoAppsOnOneService(t *testing.T) {
	_, err := NewSessionManager(&fakeNode{}, []ServiceApp{
		{ServiceID: "svc", AppAddr: "pokt1a"},
		{ServiceID: "svc", AppAddr: "pokt1b"},
	})
	if err == nil {
		t.Fatal("NewSessionManager accepted two apps for one service")
	}
	for _, want := range []string{"pokt1a", "pokt1b", "svc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// /health reports services in config order, so the order has to survive.
func TestSessionManager_KeepsConfiguredServiceOrder(t *testing.T) {
	sm, err := NewSessionManager(&fakeNode{}, []ServiceApp{
		{ServiceID: "svc-z", AppAddr: "pokt1z"},
		{ServiceID: "svc-a", AppAddr: "pokt1a"},
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	got := sm.Services()
	if len(got) != 2 || got[0] != "svc-z" || got[1] != "svc-a" {
		t.Errorf("Services() = %v, want config order", got)
	}
	if addr, ok := sm.AppAddrFor("svc-a"); !ok || addr != "pokt1a" {
		t.Errorf("AppAddrFor(svc-a) = (%q, %v)", addr, ok)
	}
}

// recordingSessionNode remembers which app each session was fetched for.
type recordingSessionNode struct {
	fakeNode
	asked map[string]string // serviceID -> appAddr
}

func (r *recordingSessionNode) GetSession(ctx context.Context, serviceID, appAddr string) (*sessiontypes.Session, error) {
	r.mu.Lock()
	if r.asked == nil {
		r.asked = map[string]string{}
	}
	r.asked[serviceID] = appAddr
	r.mu.Unlock()
	return r.fakeNode.GetSession(ctx, serviceID, appAddr)
}

func (r *recordingSessionNode) appFor(serviceID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked[serviceID]
}
