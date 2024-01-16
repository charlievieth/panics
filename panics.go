// Package panics allows for panics to be safely handled and notified on.
// It provides helper functions that can safely handle and recover from
// unhandled panics and an API similar to [os/signal] for panic notifications.
package panics

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// TODO(charlie): considerations / future work
//
// 	* Add an internal Error logger to report write errors, timeouts, or
// 	  any other error that we can't directly surface to the user

// Explain the reasoning behind NotifyContext, which provides a context
// that is cancelled on any panic. If propagated then this will cancel
// any inflight requests/work helping to prepare for exit.

var (
	noPrintTrace   atomic.Bool
	allStackTraces atomic.Bool
	panicCount     atomic.Uint64 // number of captured panics
	delivering     atomic.Int32  // panics being actively handled/delivered
	firstPanic     atomic.Pointer[Error]
	output         atomic.Pointer[writer]
	writeTimeout   atomic.Int64
)

var stderrWriter = writer{w: os.Stderr}

// immediately initialize output before any init() functions run
var _ = func() struct{} {
	output.Store(&stderrWriter)
	// Initialize writeTimeout default to 100ms
	writeTimeout.Store(int64(100 * time.Millisecond))
	return struct{}{}
}()

// A writer safely wraps an [io.Writer] with a concrete type so that we
// can use it with atomic.Pointer (output var). A nil *writer is safe
// to use and makes writes a no-op.
type writer struct {
	// TODO(charlie): Consider adding a mutex to allow users to supply
	// non-thread-safe writers and because it would allow for us to
	// wait until writes complete.
	w io.Writer
}

func (w *writer) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	return w.w.Write(p)
}

// SetOutput sets the [io.Writer] that a captured panic and its stack trace are
// immediately written to with [Error.WriteTo]. The format is the same as an
// uncaught panic. The Writer must be safe for concurrent use. By default,
// panics are written to [os.Stderr].
//
// If writing the panic takes more than the timeout (configurable via the [WriteTimeout] function)
// all registered channels and contexts will be notified / canceled before the write completes.
//
// Calling SetOutput with a nil [io.Writer] or calling [PrintStackTrace] with
// false disables the printing of panics.
//
// The [WriterFunc] type can be used to set an arbitrary logger or function as
// the output.
func SetOutput(w io.Writer) {
	switch {
	case w == nil || w == io.Discard:
		output.Store(nil)
	case w == os.Stderr:
		output.Store(&stderrWriter)
	default:
		// Handle interface shenanigans where w is non-nil despite the
		// underlying value being nil. This is a programming error and
		// normally should be dealt with harshly (panic), but considering
		// that we use this to handle panics we need to play it safe here.
		if v := reflect.ValueOf(w); v.Kind() == reflect.Ptr && v.IsNil() {
			output.Store(nil)
		} else {
			output.Store(&writer{w})
		}
	}
}

// PrintStackTrace sets if a panic and its stack trace should be immediately
// printed when a panic is detected and returns the previous value. By default
// this is enabled and the panic and its trace are written to [os.Stderr].
//
// [SetOutput] sets the io.Writer the panic and its stack trace are written to.
func PrintStackTrace(printTrace bool) (prev bool) {
	return noPrintTrace.Swap(!printTrace)
}

// IncludeAllStackTraces sets if the stack trace captured when a panic occurs
// should include the stack trace of all other goroutines in addition to
// the stack trace of the goroutine that caused the panic and returns the
// previous value.
//
// Be careful since the size of the stack trace when all other goroutines are
// included can be quite large (I've seen +30MB on large services).
func IncludeAllStackTraces(allTraces bool) (prev bool) {
	return allStackTraces.Swap(allTraces)
}

// WriteTimeout sets the maximum amount of time to wait for a panic to be written
// to output before handling the panic and signalling any registered channels
// or canceling any registered contexts.
//
// This prevents a slow or blocked writer from blocking the handling of panics.
func WriteTimeout(timeout time.Duration) (prev time.Duration) {
	return time.Duration(writeTimeout.Swap(int64(timeout)))
}

// A Error is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type Error struct {
	stack string // stack trace captured at the site of the panic
	value any    // value panic() was called with
	id    uint64 // id of the panic - first is 1
}

// Error returns the value panic was called with followed by two newlines and
// the stack trace captured at the site of the panic. The returned string is
// not prefixed with "panic: ".
func (e *Error) Error() string {
	return fmt.Sprintf("%v\n\n%s", e.value, e.stack)
}

// Stack returns the stack trace that was captured at the site of the panic.
func (e *Error) Stack() string { return e.stack }

// Value returns the value that panic was called with.
func (e *Error) Value() any { return e.value }

// Unwrap returns the value that triggered the panic if that value is an error,
// otherwise nil is returned.
func (e *Error) Unwrap() error {
	if e.value == nil {
		return nil
	}
	err, _ := e.value.(error)
	return err
}

// ID returns the 1 based ID of this panic. The ID is incremented every time a
// panic is captured and the first panic has an ID of 1.
func (e *Error) ID() uint64 { return e.id }

// First returns if this is the first captured panic.
func (e *Error) First() bool { return e.ID() == 1 }

// WriteTo writes the panic (with prefix: "panic: ") and its stack trace to w.
// The output will closely match that of an uncaught panic.
//
// If writing a panic at program exit w should be [os.Stderr] to match the
// default behavior.
func (e *Error) WriteTo(w io.Writer) (int64, error) {
	var suffix string
	n, err := fmt.Fprintf(w, "panic: %v%s\n\n%s\n", e.value, suffix, e.stack)
	return int64(n), err
}

// First returns the [Error] for the first captured panic or nil if no panics
// were captured.
//
// If multiple panics occur at the exact same time, there is a very slight
// chance that the returned [Error] may not be from the first panic.
// This unlikely in any real world program.
func First() *Error {
	if e := firstPanic.Load(); e != nil {
		return e
	}
	if panicCount.Load() == 0 {
		return nil
	}
	// We're actively handling a panic. Wait until the stack trace has been
	// collected and the Error is stored in firstPanic.
	for {
		if e := firstPanic.Load(); e != nil {
			return e
		}
		runtime.Gosched()
	}
}

var handlers struct {
	sync.Mutex
	// Map of channels to be notified when a panic is captured
	m map[chan<- *Error]struct{}
	// Map of context.Contexts to be canceled when a panic is captured
	mctx map[*panicsCtx]struct{}
	// stopping* are used to handle pending panics during a call to stop*.
	// Not a map because entries are few and live here only very briefly.
	stopping    []chan<- *Error
	stoppingCtx []*panicsCtx
}

// TODO: organize this code

func stopCtx(c *panicsCtx) {
	handlers.Lock()
	_, ok := handlers.mctx[c]
	if ok {
		delete(handlers.mctx, c)
	}
	if !ok || c.Err() != nil {
		handlers.Unlock()
		return
	}
	// Handle pending panics
	handlers.stoppingCtx = append(handlers.stoppingCtx, c)
	handlers.Unlock()

	panicsWaitUntilIdle()

	handlers.Lock()
	for i, cc := range handlers.stoppingCtx {
		if cc == c {
			a := handlers.stoppingCtx
			handlers.stoppingCtx = append(handlers.stoppingCtx[:i], handlers.stoppingCtx[i+1:]...)
			a[len(a)-1] = nil
			break
		}
	}
	handlers.Unlock()
}

// Notify causes package panics to relay any captured panics to c.
//
// Package panics will not block sending to c: the caller must ensure
// that c has sufficient buffer space to keep up with the expected
// panic rate. For a channel used for notification of just one panic value,
// a buffer of size 1 is sufficient.
//
// Chanel c must not be closed before a call to [Stop].
//
// If multiple panics occur simultaneously the order in which captured panics
// are sent to c is undefined. This is because we send to c after collecting
// a stack trace at the panic site, which takes an indeterminate amount of time.
// The [First] function can be used to get the first panic.
//
// Programs should begin an orderly shutdown after the first panic is received.
func Notify(c chan<- *Error) {
	if c == nil {
		panic("panics: Notify using nil channel")
	}
	handlers.Lock()
	if handlers.m == nil {
		handlers.m = make(map[chan<- *Error]struct{})
	}
	handlers.m[c] = struct{}{}
	handlers.Unlock()
}

// Wait until there are no more panics waiting to be processed.
func panicsWaitUntilIdle() {
	// We set panicked=true before incrementing delivering, which is a race
	// condition that First handles - so call it here.
	if First() == nil {
		return
	}
	for delivering.Load() != 0 {
		runtime.Gosched()
	}
}

// Stop causes package panics to stop relaying captured panics to c.
// When Stop returns, it is guaranteed that c will receive no more panics.
func Stop(ch chan<- *Error) {
	handlers.Lock()
	if _, ok := handlers.m[ch]; !ok {
		handlers.Unlock()
		return
	}
	delete(handlers.m, ch)
	handlers.stopping = append(handlers.stopping, ch) // handle pending panics
	handlers.Unlock()

	panicsWaitUntilIdle()

	handlers.Lock()
	for i, c := range handlers.stopping {
		if c == ch {
			a := handlers.stopping
			handlers.stopping = append(handlers.stopping[:i], handlers.stopping[i+1:]...)
			a[len(a)-1] = nil
			break
		}
	}
	handlers.Unlock()
}

// TODO: do we want to handle sending on a closed channel ???
func process(e *Error) {
	handlers.Lock()
	defer handlers.Unlock()
	// notify channels
	for c := range handlers.m {
		// send Error but do not block for it
		select {
		case c <- e:
		default:
		}
	}
	for _, c := range handlers.stopping {
		select {
		case c <- e:
		default:
		}
	}
	// cancel Contexts with error
	for c := range handlers.mctx {
		c.cancel(e)
	}
	// optimized map clear
	for c := range handlers.mctx {
		delete(handlers.mctx, c)
	}
	for _, c := range handlers.stoppingCtx {
		c.cancel(e)
	}
}

// NotifyContext returns a copy of the parent context that is marked done
// (its Done channel is closed) when any panic is captured, when the parent
// context's Done channel is closed, or the returned stop function is called,
// whichever happens first. The captured panic may occur in any part of the program.
//
// If the context is marked done because of a panic, its cancel cause will be
// the [*Error] recorded for that panic (via [context.WithCancelCause]).
// The cause can be retrieved by calling [context.Cause] on the canceled Context
// or on any of its derived Contexts.
//
// If multiple panics occur concurrently, there is no guarantee which panic
// will cancel the returned context and be set as its cancel cause.
// [First] can be used to find the first captured panic.
//
// The stop function cancels the Context and releases resources associated with
// it, so code should call stop as soon as the operations running in this Context
// complete. It also waits for any in-progress panics to be handled before returning
// so users should check the cancel cause with [context.Cause] or [ContextError]
// to see if the context was canceled by a panic after stop returns.
//
// See the [Capture] documentation for a full description of how panics are
// handled.
//
// NotifyContext will panic if parent is nil.
func NotifyContext(parent context.Context) (_ context.Context, stop context.CancelFunc) {
	if parent == nil {
		panic("panics: cannot create context from nil parent")
	}
	if p, _ := parent.Value(&panicsCtxKey).(*panicsCtx); p != nil {
		if done := p.Done(); done != nil && done == parent.Done() {
			// Parent context is registered with NotifyContext
			// and was not created with context.WithoutCancel.
			ctx, cancel := context.WithCancel(parent)
			return &panicsCtx{Context: ctx}, cancel
		}
	}
	ctx, causeFn := context.WithCancelCause(parent)
	c := &panicsCtx{
		Context: ctx,
		cancel:  causeFn,
	}
	handlers.Lock()
	if handlers.mctx == nil {
		handlers.mctx = make(map[*panicsCtx]struct{})
	}
	handlers.mctx[c] = struct{}{}
	handlers.Unlock()
	if ctx.Err() == nil {
		go func() {
			<-c.Done()
			stopCtx(c)
		}()
	}
	return c, c.stop
}

var panicsCtxKey int

type panicsCtx struct {
	context.Context
	cancel context.CancelCauseFunc
}

func (c *panicsCtx) Value(key any) any {
	if key == &panicsCtxKey {
		return c
	}
	return c.Context.Value(key)
}

func (c *panicsCtx) stop() {
	// NB: This causes us to take the slow handlers.stopped code path.
	stopCtx(c)
	c.cancel(nil)
}

// ContextError returns the [*Error] the ctx was canceled with or nil if
// the ctx was not canceled due to a panic.
//
// This is essentially a wrapper around [context.Cause].
func ContextError(ctx context.Context) *Error {
	if ctx == nil {
		return nil
	}
	e, _ := context.Cause(ctx).(*Error)
	return e
}

func (c *panicsCtx) String() string {
	// We know that the type of c.Context is context.cancelCtx,
	// and we know that the String method of cancelCtx returns
	// a string that ends with ".WithCancel".
	name := c.Context.(fmt.Stringer).String()
	name = name[:len(name)-len(".WithCancel")]
	return "panics.NotifyContext(" + name + ")"
}

func handlePanic(e *Error, timeout time.Duration) {
	// Delay processing until writing the panic to output, if any.
	defer process(e)

	if noPrintTrace.Load() {
		return
	}
	wr := output.Load()
	if wr == nil {
		return
	}

	// If logging is enabled, do not indefinitely block on it.
	done := make(chan struct{})
	go func(wr *writer, done chan<- struct{}) {
		defer func() {
			close(done)
			_ = recover() // ignore panic
		}()
		_, _ = e.WriteTo(wr) // ignore error
	}(wr, done)

	to := time.NewTimer(timeout)
	select {
	case <-to.C:
		// Timed out waiting for write to complete.
	case <-done:
		to.Stop()
	}
}

// Capture calls function fn directly, not in a goroutine, safely recovers
// from any panic that occurs during its execution, and an error of type
// [*Error] if fn panicked. If fn panics, all channels registered with [Notify]
// will be notified and Contexts created with [NotifyContext] will be canceled
// before Capture returns.
//
// If stack trace printing is enabled via [PrintStackTrace] (default true),
// the panic and its stack trace are immediately written to the writer configured
// by [SetOutput] (default [os.Stderr]) in the same format as Go writes an unhandled panic. The
// panics package will wait up to a specified timeout (configurable via the [WriteTimeout] function)
// for the write to finish before notifying any registered channels or Contexts of the panic
// (otherwise a blocked writer will prevent panic handling/notification).
// Therefore, programs should not rely on this behavior alone to print the panic
// and its stack trace before exit. Instead, programs should ensure that they
// print any captured panics they care about before exiting.
// The [IncludeAllStackTraces] function controls if the stack trace includes
// all goroutines and not just the one that caused the panic.
//
// Capture is a low-level function and most users probably want [Go] or [GoWG]
// since those functions run fn in a goroutine.
//
// Note: Deferred functions in fn are called before the panic handler returns.
// Therefore, if code relies on the panic handler completing before any deferred
// functions run (e.g. [sync.WaitGroup.Done]) the defer needs to occur
// outside of fn. The below example shows how to correctly do this with
// a [sync.WaitGroup].
//
//	var wg sync.WaitGroup
//	wg.Add(1)
//	go func() {
//		defer wg.Done()
//		Capture(fn)
//	}()
//	wg.Wait()
//
// The [GoWG] function is provided to correctly handle the common use-case of
// waiting on a [sync.WaitGroup].
func Capture(fn func()) (panicErr error) {
	defer func() {
		if e := recover(); e != nil {
			id := panicCount.Add(1)
			delivering.Add(1)
			defer delivering.Add(-1)

			all := allStackTraces.Load()
			n := 4096
			if all {
				// Capturing all stack traces is incredibly expensive since it
				// requires stopping the world and the trace is generally large
				// so start with a larger buffer.
				//
				// TODO: consider capping the size of the captured stack trace
				// since I've seen this exceed 30MB in large services.
				n = 64 * 1024
			}
			buf := make([]byte, n)
			for {
				n := runtime.Stack(buf, all)
				if n < len(buf) {
					buf = buf[:n]
					break
				}
				buf = make([]byte, 2*len(buf))
			}

			err := &Error{
				value: e,
				stack: unsafe.String(unsafe.SliceData(buf), len(buf)),
				id:    id,
			}
			if id == 1 {
				firstPanic.Store(err)
			}
			handlePanic(err, time.Duration(writeTimeout.Load()))

			panicErr = err
		}
	}()
	fn()
	return panicErr
}

// Go runs func fn in a goroutine using [Capture].
//
// Note: Deferred functions in fn are called before the panic handler (see
// the [Capture] docs for an explanation).
func Go(fn func()) { go Capture(fn) }

// GoWG increments the WaitGroup counter, runs func fn in a goroutine using
// [Capture], then decrements the WaitGroup counter. Any panic that occurs
// in fn will be handled before the WaitGroup is decremented.
//
// This function is provided because decrementing the WaitGroup counter in
// fn will occur before the panic handler.
//
// GoWG will panic if wg is nil since that is a hard programming error that
// will lead to hard to debug synchronization issues.
func GoWG(wg *sync.WaitGroup, fn func()) {
	if wg == nil {
		panic("panics: cannot call GoWG with nil sync.WaitGroup")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		Capture(fn)
	}()
}

// The WriterFunc type is an adapter that allows ordinary functions to implement
// the [io.Writer] interface. This makes it easy to use an arbitrary logger as
// the argument to [SetOutput].
//
// The below example shows how to use a [log.Logger] as the panic output:
//
//	panics.SetOutput(panics.WriterFunc(func(p []byte) {
//		log.Printf("captured panic: %s", p)
//	}))
type WriterFunc func(p []byte)

// Write calls f(p).
func (w WriterFunc) Write(p []byte) (int, error) {
	w(p)
	return len(p), nil
}
