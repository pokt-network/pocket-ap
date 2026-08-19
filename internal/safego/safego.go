// Package safego runs work on a goroutine that cannot take the process down.
//
// net/http recovers a panic in the goroutine serving a request, so a bad type
// assertion deep in a handler costs one 500 and a log line. That protection
// ends at the goroutine boundary, and pocket-ap crosses it constantly: the
// WebSocket bridge routes every frame on its own goroutine, the block poller
// runs on another, each listener on another still. None of them had any
// recovery, so one malformed supplier frame ended the process for every
// listener and every app.
//
// That is not hypothetical here. pocket/pubkey.go documents the live instance:
// the SDK reports "this account never signed a transaction" as (nil, nil), a
// nil cryptotypes.PubKey boxed into a nil any blew up a single-value type
// assertion, and because ValidateFrame runs in the bridge's own goroutine the
// panic was fatal to the whole binary. That specific bug is fixed; the class it
// belongs to was not covered at all.
//
// Crash-fast is a defensible policy. What is not defensible is applying it by
// accident, to whichever subset of failures happens to land on a detached
// goroutine. This package makes the choice uniform.
//
// LIFT: SAGE internal/safego (8873ea1), minus its metrics wiring — pocket-ap
// exports no metrics by doctrine, so the counter is read through Panics() by
// the health handler instead. GoCtx is omitted: nothing here needs it.
package safego

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
)

// panics counts recovered panics across the process.
//
// It exists because containment without visibility is its own failure mode: a
// panic that is caught, logged and never counted turns a loud crash into a
// quiet one, and the operator learns nothing until the second symptom. /health
// reads this, so a contained panic still shows up somewhere an operator looks.
var panics atomic.Uint64

// Panics returns how many panics have been recovered since start.
//
// Any non-zero value is worth acting on: nothing in this repo is expected to
// panic, and a recovered panic means a frame, a relay or a background task was
// abandoned partway.
func Panics() uint64 {
	return panics.Load()
}

// ErrPanic marks an error produced by recovering a panic rather than by
// anything the request, the supplier or the network did.
var ErrPanic = errors.New("recovered panic")

// Go runs fn on a new goroutine, recovering and logging any panic.
//
// name identifies the work in the log line and should be stable — it is what an
// operator greps for. logger may be nil, in which case slog.Default is used:
// losing the log line is not a reason to lose the recovery.
func Go(logger *slog.Logger, name string, fn func()) {
	go Run(logger, name, fn)
}

// Run is Go without the goroutine: it runs fn on the calling goroutine under
// the same recovery.
//
// Use it for a loop body. Wrapping the whole loop instead would contain the
// panic and still leave a dead ticker — a stopped block poller that logged
// once, which reads as a healthy process serving relays against a session the
// chain retired.
func Run(logger *slog.Logger, name string, fn func()) {
	defer Recover(logger, name)
	fn()
}

// Recover is the deferred half of Run, for call sites that must keep their own
// `go func()` — to capture a loop variable, close over a WaitGroup, or order
// their own defers. Call it as the FIRST deferred statement:
//
//	go func() {
//		defer Recover(logger, "transport.serve")
//		defer wg.Done()
//		…
//	}()
//
// Defers run last-in-first-out, so listing it first means it runs last and can
// still contain a panic raised by one of the other defers.
func Recover(logger *slog.Logger, name string) {
	record(recover(), logger, name, "recovered from panic on a background goroutine")
}

// Call runs fn on the calling goroutine and turns a panic into an error
// wrapping ErrPanic.
//
// This is the variant for work whose failure the caller already knows how to
// handle. The WebSocket bridge is the reason it exists: a panic while
// processing a frame recurs on the next frame from the same supplier, so
// recovering and continuing is an endless stack trace per frame. Converting it
// to an error lets the bridge do what it does with any other processing
// failure — shut down, so the client reconnects onto a different supplier.
func Call(logger *slog.Logger, name string, fn func() error) (err error) {
	defer func() {
		if r := record(recover(), logger, name, "recovered from panic while handling work"); r != nil {
			err = fmt.Errorf("%w in %s: %v", ErrPanic, name, r)
		}
	}()
	return fn()
}

// record is the shared body of Recover and Call. It returns the recovered value
// so Call can build an error from it, and nil when there was no panic.
func record(r any, logger *slog.Logger, name, msg string) any {
	if r == nil {
		return nil
	}
	panics.Add(1)
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error(msg,
		"work", name,
		"panic", r,
		"stack", string(debug.Stack()),
	)
	return r
}
