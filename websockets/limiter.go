package websockets

import "sync/atomic"

// DefaultMaxConnections caps concurrent WebSocket connections when a listener
// says nothing.
//
// Deliberately far above normal load: this is a backstop against runaway, and a
// ceiling that trips during ordinary traffic would be worse than none. Matches
// SAGE and PATH.
const DefaultMaxConnections = 10000

// ConnectionLimiter bounds how many WebSocket connections are held open at once.
//
// A WebSocket connection is nothing like an HTTP request: it is long-lived, and
// each costs several goroutines, two TCP sockets and read/write buffers. Nothing
// else counts them. Rate-limiting upgrade *attempts* at the edge does not help,
// because the cost is in connections that never leave — clients that connect and
// simply stay grow goroutines and file descriptors without bound.
//
// The exposure is worse here than in a plain proxy: transport.WS.handle blocks
// on <-bridge.Done(), so every live connection also pins an http.Server handler
// goroutine for its whole life.
//
// A nil *ConnectionLimiter means no limit: every method is nil-safe and Acquire
// always succeeds, so the limiter stays optional without every caller and test
// having to construct one.
//
// LIFT: SAGE websockets/limiter.go (b826630), itself a port of PATH dd6ad12a
// with 4c3fe58c's CAS loop folded in.
type ConnectionLimiter struct {
	max    int64
	active atomic.Int64
}

// NewConnectionLimiter returns a limiter capping concurrent connections at max.
// A max <= 0 returns nil, which disables limiting.
func NewConnectionLimiter(max int) *ConnectionLimiter {
	if max <= 0 {
		return nil
	}
	return &ConnectionLimiter{max: int64(max)}
}

// Acquire reserves a slot. True means one was reserved and the caller must
// Release exactly once; false means the limiter is full, and the caller must
// reject the connection and must NOT Release.
//
// A nil limiter always returns true.
func (l *ConnectionLimiter) Acquire() bool {
	if l == nil {
		return true
	}
	// CAS loop rather than add-then-rollback: only increment when strictly below
	// the cap, so the counter never overshoots even for an instant. An optimistic
	// add would let a concurrent Active() read report more than max — wrong
	// exactly when something is reading it to decide whether to reject.
	for {
		cur := l.active.Load()
		if cur >= l.max {
			return false
		}
		if l.active.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release frees a slot from a successful Acquire. Exactly once per successful
// Acquire — a leak here ratchets the cap down to zero. A nil limiter is a no-op.
func (l *ConnectionLimiter) Release() {
	if l == nil {
		return
	}
	l.active.Add(-1)
}

// Active reports how many slots are currently held. A nil limiter reports 0.
func (l *ConnectionLimiter) Active() int64 {
	if l == nil {
		return 0
	}
	return l.active.Load()
}
