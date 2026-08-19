package safego

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// Every assertion here is about the process still being alive to make it. A test
// that fails because the binary died reports as a panic, not a failure, so the
// "did it contain" half is proved by the test returning at all; what the
// assertions add is that the work was accounted for rather than swallowed.

func TestRun_ContainsAPanicAndCounts(t *testing.T) {
	before := Panics()

	Run(discardLogger(), "unit", func() { panic("boom") })

	if got := Panics() - before; got != 1 {
		t.Errorf("Panics() delta = %d, want 1", got)
	}
}

func TestRun_NoPanicDoesNotCount(t *testing.T) {
	before := Panics()

	ran := false
	Run(discardLogger(), "unit", func() { ran = true })

	if !ran {
		t.Error("fn did not run")
	}
	if got := Panics() - before; got != 0 {
		t.Errorf("Panics() delta = %d, want 0 — a clean run must not look like a recovery", got)
	}
}

func TestGo_ContainsAPanicOnItsOwnGoroutine(t *testing.T) {
	before := Panics()

	// The goroutine is the whole point: this is the boundary net/http's own
	// recovery does not cross.
	var wg sync.WaitGroup
	wg.Add(1)
	Go(discardLogger(), "unit", func() {
		defer wg.Done()
		panic("boom")
	})
	wg.Wait()

	if got := Panics() - before; got != 1 {
		t.Errorf("Panics() delta = %d, want 1", got)
	}
}

// Recover has to survive a nil logger, because the alternative is a call site
// that skips the recovery rather than passing one.
func TestRun_NilLoggerStillRecovers(t *testing.T) {
	before := Panics()

	Run(nil, "unit", func() { panic("boom") })

	if got := Panics() - before; got != 1 {
		t.Errorf("Panics() delta = %d, want 1", got)
	}
}

// The distinction Call exists for: a caller waiting on a result must get one.
// Recovering and returning nothing would trade a crashed process for a hung
// caller, which is not an improvement.
func TestCall_TurnsAPanicIntoAnErrPanic(t *testing.T) {
	before := Panics()

	err := Call(discardLogger(), "unit", func() error { panic("boom") })

	if err == nil {
		t.Fatal("Call returned nil after a panic; the caller would treat the work as successful")
	}
	if !errors.Is(err, ErrPanic) {
		t.Errorf("error %v does not wrap ErrPanic, so a caller cannot tell 'our code is broken' from 'the supplier is broken'", err)
	}
	if got := Panics() - before; got != 1 {
		t.Errorf("Panics() delta = %d, want 1", got)
	}
}

func TestCall_PassesThroughTheRealError(t *testing.T) {
	sentinel := errors.New("the supplier said no")

	err := Call(discardLogger(), "unit", func() error { return sentinel })

	if !errors.Is(err, sentinel) {
		t.Errorf("Call() = %v, want the fn's own error", err)
	}
	if errors.Is(err, ErrPanic) {
		t.Error("an ordinary error was reported as a panic")
	}
}

func TestCall_SuccessReturnsNil(t *testing.T) {
	if err := Call(discardLogger(), "unit", func() error { return nil }); err != nil {
		t.Errorf("Call() = %v, want nil", err)
	}
}

// Recover is documented as safe to defer unconditionally. If it counted a
// no-panic return, /health would report recoveries on a process that never had
// one and the count would be worthless.
func TestRecover_OnACleanReturnIsANoOp(t *testing.T) {
	before := Panics()

	func() { defer Recover(discardLogger(), "unit") }()

	if got := Panics() - before; got != 0 {
		t.Errorf("Panics() delta = %d, want 0", got)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
