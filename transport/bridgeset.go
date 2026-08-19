package transport

import (
	"sync"

	"github.com/pokt-network/pocket-ap/websockets"
)

// bridgeSet tracks the live bridges on one listener.
//
// It exists because WebSocket connections outlive requests: http.Server.Shutdown
// waits for handlers to return, and a bridge handler blocks on Done() until the
// socket closes — which, for an idle subscription, is never. Shutdown would sit
// there until its deadline while the client sat there unaware. Closing the
// bridges first means every client gets a proper close frame telling it to
// reconnect.
type bridgeSet struct {
	mu      sync.Mutex
	bridges map[*websockets.Bridge]struct{}
}

func newBridgeSet() *bridgeSet {
	return &bridgeSet{bridges: map[*websockets.Bridge]struct{}{}}
}

func (s *bridgeSet) add(b *websockets.Bridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bridges[b] = struct{}{}
}

func (s *bridgeSet) remove(b *websockets.Bridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bridges, b)
}

func (s *bridgeSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bridges)
}

// shutdownAll closes every live bridge. Shutdown is idempotent, so racing with a
// bridge that is already going down on its own is harmless.
func (s *bridgeSet) shutdownAll() {
	s.mu.Lock()
	live := make([]*websockets.Bridge, 0, len(s.bridges))
	for b := range s.bridges {
		live = append(live, b)
	}
	s.mu.Unlock()

	// Outside the lock: Shutdown blocks on close-frame writes, and each bridge's
	// handler calls remove() as it unwinds, which needs the same lock.
	for _, b := range live {
		b.Shutdown(websockets.ErrBridgeContextCanceled)
	}
}
