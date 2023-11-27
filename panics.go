package panics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

// Maximum amount of time to wait for a panic to be flushed to STDERR before
// handling the panic.
const writeTimeout = 100 * time.Millisecond

var (
	panicked  atomic.Bool
	testNoLog atomic.Bool
	stderr    io.Writer = os.Stderr
)

// TODO: remove if not used
//
// panicsCtxKey is the key that identifies a context that is a child
// of our global context.
type panicsCtxKey struct{}

var ctx, cancelFn = func() (context.Context, context.CancelCauseFunc) {
	parent, cancel := context.WithCancelCause(context.Background())
	ctx := context.WithValue(parent, panicsCtxKey{}, parent)
	return ctx, cancel
}()

// A Error is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type Error struct {
	value     any
	stack     []byte
	recovered bool
}

// Error returns an error that contains the value passed to panic and a
// stack trace captured at the site of the panic.
func (e *Error) Error() string {
	return fmt.Sprintf("%v\n\n%s", e.value, e.stack)
}

// Stack returns the stack trace that was capturing during program panic.
func (e *Error) Stack() []byte {
	return e.stack
}

// Value returns the value that the program panicked with.
// Value returns the value that panic was called with.
func (e *Error) Value() any {
	return e.value
}

// TODO: DELETE
//
// WARN: we might not have panicked with an [error] so do we want this??
func (p *Error) Unwrap() error {
	if p.value == nil {
		return nil
	}
	switch v := p.value.(type) {
	case error:
		return v
	case string:
		return errors.New(v)
	case []byte:
		// Not sure why you'd panic with a []byte but handle it.
		return errors.New(string(v))
	case fmt.Stringer:
		return errors.New(v.String())
	default:
		return fmt.Errorf("%v", v)
	}
}

// Recovered returns if the panic occurred after the initial panic. The panic
// may occur in any goroutine not just the one that created the initial panic.
func (e *Error) Recovered() bool {
	return e.recovered
}

// WriteTo writes the panic (with the "panic: " prefix) and its stack trace to w.
// The output will closely match that of an uncaught panic.
//
// If writing a panic at program exit w should be [os.Stderr] to match the
// default behavior.
func (e *Error) WriteTo(w io.Writer) (int64, error) {
	var suffix string
	if e.recovered {
		suffix = " [recovered]"
	}
	n, err := fmt.Fprintf(w, "panic: %v%s\n\n%s\n", e.value, suffix, e.stack)
	if err == nil {
		if f, _ := w.(interface{ Sync() error }); f != nil {
			err = f.Sync()
		}
	}
	return int64(n), err
}

// Context returns a non-nill [context.Context] that is closed if the program
// panics. The panic and a stack trace captured at the panic site can be
// retrieve using [context.Cause] which will return an error with type [*Error].
func Context() context.Context { return ctx }

// Done is a convenience function that returns the Done channel of the panics
// package's [context.Contex]. It will be closed if a panic was captured.
func Done() <-chan struct{} { return ctx.Done() }

// CapturedPanic returns the [*Error] of the first captured panic or nil if no
// panics have been captured.
func CapturedPanic() *Error {
	err, _ := context.Cause(ctx).(*Error)
	return err
}

// WARN: do we want this ???
//
// TODO: describe that this is a wrapper around context.AfterFunc
//
// AfterFunc arranges to call f in its own goroutine after ctx is done
// (cancelled or timed out).
// If ctx is already done, AfterFunc calls f immediately in its own goroutine.
//
// Multiple calls to AfterFunc on a context operate independently;
// one does not replace another.
//
// Calling the returned stop function stops the association of ctx with f.
// It returns true if the call stopped f from being run.
// If stop returns false,
// either the context is done and f has been started in its own goroutine;
// or f was already stopped.
// The stop function does not wait for f to complete before returning.
// If the caller needs to know whether f is completed,
// it must coordinate with f explicitly.
//
// If ctx has a "AfterFunc(func()) func() bool" method,
// AfterFunc will use it to schedule the call.
func AfterFunc(fn func(e *Error)) (stop func() bool) {
	return context.AfterFunc(ctx, func() {
		fn(CapturedPanic())
	})
}

func includeAllGoroutines() bool {
	s := os.Getenv("GOTRACEBACK")
	switch s {
	case "all", "system", "crash", "1", "2":
		return true
	}
	if runtime.GOOS == "windows" && s == "wer" {
		return true
	}
	return false
}

// SetPrintStackTrace sets if a stack trace should be immediately printed
// when a panic is detected.
func SetPrintStackTrace(printTrace bool) bool {
	panic("IMPLEMENT")
	// return testNoLog.Swap(printTrace)
}

// handlePanic writes a panic to STDERR then cancels the Context. If writing
// to STDERR blocks the Context will be closed after timeout.
func handlePanic(e *Error, timeout time.Duration) {
	// WARN: not doing this immediately prevents other goroutines from
	// realizing that we've already panicked.
	//
	// Delay cancelling the Context until after we've printed the panic
	// to STDERR otherwise the program may exit before we finish printing.
	defer cancelFn(e)
	if testNoLog.Load() {
		return
	}

	done := make(chan struct{})
	go func(done chan<- struct{}, stderr io.Writer) {
		defer func() {
			close(done)
			_ = recover() // ignore panic
		}()
		_, _ = e.WriteTo(stderr) // ignore error
	}(done, stderr)

	to := time.NewTimer(timeout)
	select {
	case <-to.C:
	case <-done:
	}
	to.Stop()
}

// TODO: expand on this comment
//
// Capture calls function fn directly, not in a goroutine, and safely recovers
// from any panic that occurs during its execution.
func Capture(fn func()) {
	defer func() {
		if e := recover(); e != nil {
			first := panicked.CompareAndSwap(false, true) // first panic
			// Only include all goroutines for the first panic.
			all := first && includeAllGoroutines()
			buf := make([]byte, 2048)
			for {
				// TODO: cap stack size
				n := runtime.Stack(buf, all)
				if n < len(buf) {
					buf = buf[:n]
					break
				}
				buf = append(buf, make([]byte, len(buf))...)
			}
			handlePanic(&Error{value: e, stack: buf, recovered: !first}, writeTimeout)
		}
	}()
	fn()
}

// Go runs func fn in a goroutine using [Capture].
func Go(fn func()) { go Capture(fn) }

// func GoCtx(parent context.Context, fn func(ctx context.Context)) {
// 	ctx1, cancel1 := context.WithCancel(Context())
// 	ctx2, cancel2 := context.WithCancel(ctx1)
// 	Go(func() {
// 		defer cancel()
// 		fn(ctx)
// 	})
// }

func CaptureCtx(parent context.Context, fn func(ctx context.Context)) (canceled bool) {
	// WARN: context.WithoutCancel can break this !!!
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if panicked.Load() {

	}
	// select {
	// case <-Done():
	// 	cancel()
	// 	Capture(func() { fn(ctx) })
	// default:
	// }
	done := make(chan struct{})
	Go(func() {
		defer close(done)
		fn(ctx)
	})
	select {
	case <-Done():
		canceled = true
		<-done
	case <-done:
	}
	return canceled
}
