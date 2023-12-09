// TODO: pkg comment
package panics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// TODO: add a way to scope a Context to a single call

// Why NotifyContext: provides a context that is cancelled on any panic.
// If propagated then this will cancel any inflight requests/work helping
// to prepare for exit.

// Maximum amount of time to wait for a panic to be flushed to STDERR before
// handling the panic.
const writeTimeout = 100 * time.Millisecond

var (
	panicked     atomic.Bool
	noPrintTrace atomic.Bool
	delivering   atomic.Int32 // TODO: rename
	firstPanic   atomic.Pointer[Error]
	output       atomic.Pointer[writer]
)

var stderrWriter = writer{w: os.Stderr}

// immediately initialize output before any init() functions run
var _ = func() struct{} {
	output.Store(&stderrWriter)
	return struct{}{}
}()

// A writer safely wraps an io.Writer with a concrete type so that we
// can use it with atomic.Pointer (output var). A nil *writer is safe
// to use and makes writes a no-op.
type writer struct {
	w io.Writer
}

func (w *writer) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	return w.w.Write(p)
}

// SetOutput sets the io.Writer used to immediately write a panic and its stack
// trace. By default panics are written to os.Stderr. Calling SetOutput with a
// nil io.Writer disables the printing of panics.
//
// To convert a logger to an io.Writer a wrapper can be used like the one
// below:
//
//	type LogWrapper struct {
//		// Optionally, replace this with your logger of choice
//		log *log.Logger
//	}
//
//	func (w *LogWrapper) Write(p []byte) (int, error) {
//		// Optionally, replace with your log method of choice
//		w.log.Printf("%s\n")
//		return len(p), nil
//	}
//
// [SetPrintStackTrace] can also be used to disable to the printing of panics.
func SetOutput(w io.Writer) {
	switch {
	case w == nil || w == io.Discard:
		output.Store(nil)
	case w == os.Stderr:
		output.Store(&stderrWriter)
	default:
		// Handle the case where w is a nil pointer type that implements io.Writer
		// since even though the underlying value is nil w won't be.
		if v := reflect.ValueOf(w); v.Kind() == reflect.Ptr && v.IsNil() {
			output.Store(nil)
		} else {
			output.Store(&writer{w})
		}
	}
}

// SetPrintStackTrace sets if a panic and its stack trace should be immediately
// printed when a panic is detected and returns the previous value. By default
// this is enabled and the panic and its trace are written to os.Stderr.
//
// [SetOutput] sets the io.Writer the panic and its stack trace are written to.
func SetPrintStackTrace(printTrace bool) bool {
	return noPrintTrace.Swap(!printTrace)
}

// A Error is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type Error struct {
	value     any
	stack     []byte
	recovered bool
}

// Error returns the value panic() was called with followed by a newline and
// the stack trace captured at the site of the panic.
func (e *Error) Error() string {
	return fmt.Sprintf("%v\n\n%s", e.value, e.stack)
}

// Stack returns the stack trace that was captured during program panic.
func (e *Error) Stack() []byte { return e.stack }

// Value returns the value that panic was called with.
func (e *Error) Value() any { return e.value }

// TODO: DELETE
//
// WARN: we might not have panicked with an [error] so do we want this??
func (p *Error) Unwrap() error {
	// TODO: maybe only unwrap if value is an error
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

// TODO: rename to "First" or something to make it clearer that the panic
// was the first panic since all of our panics are recovered.
//
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
	return int64(n), err
}

// First returns the Error of the first panic or nil if no panics were captured.
func First() *Error {
	if e := firstPanic.Load(); e != nil {
		return e
	}
	if !panicked.Load() {
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

	// TODO: document why we have these
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
			n := len(handlers.stoppingCtx)
			if n > 1 {
				handlers.stoppingCtx[i] = handlers.stoppingCtx[n-1]
				handlers.stoppingCtx[n-1] = nil // remove reference
			} else {
				handlers.stoppingCtx[i] = nil
			}
			handlers.stoppingCtx = handlers.stoppingCtx[:n-1]
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
// If multiple panics occur concurrently the order in which captured panics are
// sent to c is undefined. This is because we send to c after collecting a stack
// trace at the panic site, which takes an indeterminate amount of time.
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
	handlers.stopping = append(handlers.stopping, ch)
	handlers.Unlock()

	panicsWaitUntilIdle()

	handlers.Lock()
	for i, c := range handlers.stopping {
		if c == ch {
			p := &handlers.stopping[len(handlers.stopping)-1]
			handlers.stopping = append(handlers.stopping[:i], handlers.stopping[i+1:]...)
			*p = nil // remove reference
			break
		}
	}
	handlers.Unlock()
}

// WARN: do we want to handle sending on a closed channel ???
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
// The stop function releases resources associated with it, so code should
// call stop as soon as the operations running in this Context complete.
// It also waits for any in-progress panics to be handled before returning
// so users should check the cancel cause with [context.Cause] to see if the
// context was canceled by a panic after stop returns.
func NotifyContext(parent context.Context) (_ context.Context, stop context.CancelFunc) {
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

type panicsCtx struct {
	context.Context
	cancel context.CancelCauseFunc
}

func (c *panicsCtx) stop() {
	// WARN: this causes us to take the slow handlers.stopped path !!!
	stopCtx(c)
	c.cancel(nil)
}

// WARN: a nil return value here is a valid error !!!
// ^^^^^^ This should not be a *huge* concern ^^^^^^
//
// TODO: rename
func ContextPanicError(ctx context.Context) *Error {
	if ctx == nil {
		return nil
	}
	e, _ := context.Cause(ctx).(*Error)
	return e
}

func (c *panicsCtx) String() string {
	// We know that the type of c.Context is context.cancelCtx, and we know that the
	// String method of cancelCtx returns a string that ends with ".WithCancel".
	name := c.Context.(fmt.Stringer).String()
	name = name[:len(name)-len(".WithCancel")]
	return "panics.NotifyContext(" + name + ")"
}

// TODO: Consider adding an AfterFunc with signature:
// 	AfterFunc(fn func(e *Error)) (stop func() bool) {}

// TODO: we probably don't want to do this and instead leave it to the user
// to re-panic and let the runtime handle this for us.
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

// TODO: add comment
func handlePanic(e *Error, timeout time.Duration) {
	// WARN: do we want to delay processing until after the panic is written ???
	defer process(e)

	if noPrintTrace.Load() {
		return
	}
	wr := output.Load()
	if wr == nil {
		return
	}

	// If logging is enabled, do not block on it.
	done := make(chan struct{})
	go func(wr io.Writer, done chan<- struct{}) {
		defer func() {
			close(done)
			_ = recover() // ignore panic
		}()
		_, _ = e.WriteTo(wr) // ignore error
	}(wr, done)

	to := time.NewTimer(timeout)
	select {
	case <-to.C:
	case <-done:
	}
	to.Stop()
}

// Capture calls function fn directly, not in a goroutine, and safely recovers
// from any panic that occurs during its execution.
//
// If fn panics all channels registered with [Notify] will be notified and
// Contexts created with [NotifyContext] canceled before Capture returns.
//
// If stack trace printing is enabled (default yes), the panic and its stack
// trace are immediately written to the writer configured by [SetOutput]
// (default os.Stderr) in the same format as Go writes an unhandled panic.
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
func Capture(fn func()) {
	defer func() {
		if e := recover(); e != nil {
			first := panicked.CompareAndSwap(false, true) // first panic
			delivering.Add(1)
			defer delivering.Add(-1)

			// TODO: remove this
			//
			// Only include all goroutines for the first panic.
			//
			// NOTE: runtime.Stack() is extremely expensive and has to
			// stop-the-world so we should maybe mention that as another
			// reason that not stopping after the first panic is a bad
			// idea.
			//
			all := first && includeAllGoroutines()
			buf := make([]byte, 2048)
			for {
				// TODO: cap stack size
				n := runtime.Stack(buf, all)
				if n < len(buf) {
					buf = buf[:n]
					break
				}
				buf = make([]byte, 2*len(buf))
			}

			err := &Error{value: e, stack: buf, recovered: !first}
			if first {
				firstPanic.CompareAndSwap(nil, err)
			}
			handlePanic(err, writeTimeout)
		}
	}()
	fn()
}

// CaptureValues invokes fn and returns the value returned by fn. Any panic
// that occurs during the execution of fn will be safely recovered from by
// [Capture].
func CaptureValue[T any](fn func() T) (v T) {
	Capture(func() { v = fn() })
	return v
}

// CaptureValues invokes fn and returns the values returned by fn. Any panic
// that occurs during the execution of fn will be safely recovered from by
// [Capture].
func CaptureValues[T1, T2 any](fn func() (T1, T2)) (v1 T1, v2 T2) {
	Capture(func() { v1, v2 = fn() })
	return v1, v2
}

// Go runs func fn in a goroutine using [Capture].
//
// Note: Deferred functions in fn are called before the panic handler returns.
func Go(fn func()) { go Capture(fn) }

// GoWG increments the WaitGroup counter, runs func fn in a goroutine using
// [Capture], then decrements the WaitGroup counter. Any panic that occurs
// in fn will be handled before the WaitGroup is decremented.
//
// This function is provided because decrementing the WaitGroup counter in
// fn will occur before the panic handler returns.
//
//	ch := make(chan *panics.Error, 1)
//	panics.Notify(ch)
//	var wg sync.WaitGroup
//	panics.GoWG(&wg, func() {
//		panic("here")
//	})
//	wg.Wait()
//	// If GoWG is not used then ch may not have been notified of the panic
//	// before wg.Wait returns.
//	fmt.Println(<-ch)
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

// WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN
// WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN
func Exit() {
	if e := First(); e != nil {
		// Make sure we exit and 2 is the exit code used by Go
		// when a panic occurs this call should be unreachable.
		defer os.Exit(2)
		panic(e.Error())
	}
	os.Exit(0)
}

// func CaptureCtx(parent context.Context, fn func(ctx context.Context)) (canceled bool) {
// 	// WARN: context.WithoutCancel can break this !!!
// 	ctx, cancel := NotifyContext(parent)
// 	defer cancel()
// 	if ctx.Err() != nil {
//
// 	}
// 	done := make(chan struct{})
// 	Go(func() {
// 		defer close(done)
// 		fn(ctx)
// 	})
// 	select {
// 	case <-Done():
// 		canceled = true
// 		<-done
// 	case <-done:
// 	}
// 	return canceled
// }
