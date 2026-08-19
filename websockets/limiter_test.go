package websockets

import (
	"sync"
	"testing"
)

func TestConnectionLimiter_NilIsUnlimited(t *testing.T) {
	var l *ConnectionLimiter
	for i := 0; i < 100; i++ {
		if !l.Acquire() {
			t.Fatal("nil limiter refused an acquire; nil must mean no limit")
		}
	}
	l.Release() // must not panic
	if got := l.Active(); got != 0 {
		t.Errorf("nil limiter Active() = %d, want 0", got)
	}
}

// A max of 0 or below disables limiting, which is what makes the "0 means the
// default" decision belong at the config layer rather than here.
func TestNewConnectionLimiter_NonPositiveDisables(t *testing.T) {
	for _, max := range []int{0, -1, -1000} {
		if l := NewConnectionLimiter(max); l != nil {
			t.Errorf("NewConnectionLimiter(%d) = %v, want nil", max, l)
		}
	}
}

func TestConnectionLimiter_RefusesAtCapAndRecoversOnRelease(t *testing.T) {
	l := NewConnectionLimiter(2)

	if !l.Acquire() {
		t.Fatal("limiter refused the first slot within its cap")
	}
	if !l.Acquire() {
		t.Fatal("limiter refused the second slot within its cap")
	}
	if l.Acquire() {
		t.Fatal("limiter granted a third slot with a cap of 2")
	}
	if got := l.Active(); got != 2 {
		t.Errorf("Active() = %d, want 2", got)
	}

	l.Release()
	if !l.Acquire() {
		t.Fatal("limiter did not free a slot after Release")
	}
}

// A refused Acquire must not consume a slot. If it did, the effective cap would
// ratchet down to zero under load — the failure mode is silent and permanent.
func TestConnectionLimiter_RejectionDoesNotConsumeASlot(t *testing.T) {
	l := NewConnectionLimiter(1)

	if !l.Acquire() {
		t.Fatal("first acquire refused")
	}
	for i := 0; i < 50; i++ {
		if l.Acquire() {
			t.Fatal("limiter granted beyond its cap")
		}
	}
	if got := l.Active(); got != 1 {
		t.Fatalf("Active() = %d after 50 rejections, want 1", got)
	}

	l.Release()
	if got := l.Active(); got != 0 {
		t.Errorf("Active() = %d after releasing the only slot, want 0", got)
	}
}

// The CAS loop exists so the counter never overshoots even transiently: an
// add-then-rollback would let a concurrent reader see more than max, which is
// wrong exactly when something is reading it to decide whether to reject.
func TestConnectionLimiter_NeverOvershootsUnderConcurrency(t *testing.T) {
	const (
		cap        = 8
		goroutines = 200
	)
	l := NewConnectionLimiter(cap)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		peak    int64
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !l.Acquire() {
				return
			}
			mu.Lock()
			granted++
			if a := l.Active(); a > peak {
				peak = a
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if granted != cap {
		t.Errorf("granted = %d, want exactly %d", granted, cap)
	}
	if peak > cap {
		t.Errorf("Active() peaked at %d, above the cap of %d — the counter overshot", peak, cap)
	}
}
