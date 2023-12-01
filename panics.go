package panics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Maximum amount of time to wait for a panic to be flushed to STDERR before
// handling the panic.
const writeTimeout = 100 * time.Millisecond

var (
	panicked   atomic.Bool
	testNoLog  atomic.Bool  // TODO: make this configurable
	delivering atomic.Int32 // TODO: rename
	firstPanic atomic.Pointer[Error]
	stderr     io.Writer = os.Stderr
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
	m    map[chan<- *Error]struct{}
	mctx map[*panicsCtx]struct{}

	// TODO: document why we have these
	stopping    []chan<- *Error
	stoppingCtx []*panicsCtx
}

// TODO: organize this code

func stopCtx(c *panicsCtx) {
	handlers.Lock()
	if _, ok := handlers.mctx[c]; !ok {
		handlers.Unlock()
		return
	}
	delete(handlers.mctx, c)
	if c.Err() != nil {
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
			p := &handlers.stoppingCtx[len(handlers.stoppingCtx)-1]
			handlers.stoppingCtx = append(handlers.stoppingCtx[:i], handlers.stoppingCtx[i+1:]...)
			*p = nil // clear reference
			break
		}
	}
	handlers.Unlock()
}

// TODO: if a panic already occurred should we immediately send it on the channel??
func Notify(ch chan<- *Error) {
	if ch == nil {
		panic("panics: Notify using nil channel")
	}
	handlers.Lock()
	if handlers.m == nil {
		handlers.m = make(map[chan<- *Error]struct{})
	}
	handlers.m[ch] = struct{}{}
	handlers.Unlock()
}

// Wait until there are no more panics waiting to be processed.
func panicsWaitUntilIdle() {
	for delivering.Load() != 0 {
		runtime.Gosched()
	}
}

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
			handlers.stopping = append(handlers.stopping[:i], handlers.stopping[i+1:]...)
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

// TODO: note that we set the cancel cause
//
// NOTE: that in the event of multiple concurrent panics the panic
// the Context is cancelled with is non-deterministic.
func NotifyContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	c := &panicsCtx{
		Context: ctx,
		cancel:  cancel,
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

// WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN
var (
	created   atomic.Int64
	finalized atomic.Int64
)

// WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN
//
// Find a way to make this work without leaking !!!
// Maybe we can intern the channel ???
//
// WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN WARN
func DoneX() <-chan *Error {
	c := make(chan *Error, 1)
	// WARN
	if e := firstPanic.Load(); e != nil {
		c <- e
		return c
	}
	Notify(c)
	created.Add(1)
	runtime.SetFinalizer(&c, func(c *chan *Error) {
		finalized.Add(1)
		Stop(*c)
	})
	return c
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

	// WARN WARN WARN WARN WARN
	process(e) // process immediately

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
//
// If fn panics all channels will be notified and Contexts canceled before
// Capture ends.
func Capture(fn func()) {
	defer func() {
		if e := recover(); e != nil {
			first := panicked.CompareAndSwap(false, true) // first panic
			delivering.Add(1)
			defer delivering.Add(-1)

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

			err := &Error{value: e, stack: buf, recovered: !first}
			if first {
				firstPanic.CompareAndSwap(nil, err)
			}
			handlePanic(err, writeTimeout)
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
