package panics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// bigstack helps generate large stack traces by having a very long import path
	"github.com/charlievieth/panics/internal/bigstack/pkg/pkg1/pkg2/pkg3/pkg4/bigstack"
)

func resetHandlers() {
	panicsWaitUntilIdle()

	handlers.Lock()
	defer handlers.Unlock()
	for c := range handlers.mctx {
		c.cancel(nil)
	}
	for _, c := range handlers.stoppingCtx {
		c.cancel(nil)
	}
	handlers.m = nil
	handlers.stopping = nil
	handlers.mctx = nil
	handlers.stoppingCtx = nil

	firstPanic.Store(nil)
	panicked.Store(false)
}

func testSetup(t testing.TB) {
	testNoLog.Store(true)
	resetHandlers()
	ctx, cancelFn = context.WithCancelCause(context.Background())
	t.Cleanup(func() {
		testNoLog.Store(false)
		resetHandlers()
		ctx, cancelFn = context.WithCancelCause(context.Background())
	})
}

var testErr = errors.New("test error")

func testCapturePanic(t *testing.T, fn func(call func())) {
	testSetup(t)
	fn(func() { panic(testErr) })
	select {
	case <-Done():
		// Ok
	case <-time.After(time.Second):
		t.Fatal("panic did not cancel Context")
	}
	err := context.Cause(Context())
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error should have type: %T; got: %T", perr, err)
	}
	if perr == nil {
		t.Fatal("nil error")
	}
	if !reflect.DeepEqual(perr.Value(), testErr) {
		t.Errorf("Value() = %v; want: %v", err, testErr)
	}
	if !reflect.DeepEqual(perr.Unwrap(), testErr) {
		t.Errorf("Unwrap() = %v; want: %v", err, testErr)
	}
	if len(perr.Stack()) == 0 {
		t.Error("empty stack trace")
	}
}

func TestCapturePanic(t *testing.T) {
	testCapturePanic(t, Capture)
}

func TestGoPanic(t *testing.T) {
	testCapturePanic(t, Go)
}

func TestCapturePanicNilFunc(t *testing.T) {
	testSetup(t)
	ctx := Context()
	Capture(nil)
	select {
	case <-ctx.Done():
		// Ok
	default:
		t.Error("panic did not cancel Context")
	}
	err := context.Cause(ctx)
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error should have type: %T; got: %T", perr, err)
	}
	if perr == nil {
		t.Fatal("nil error")
	}
	if _, ok := perr.Value().(runtime.Error); !ok {
		t.Errorf("expected runtime.Error got: %T", err)
	}
}

func TestCaptureAllocs(t *testing.T) {
	testSetup(t)
	allocs := testing.AllocsPerRun(100, func() {
		Capture(func() {})
	})
	if allocs != 0 {
		t.Errorf("Capture should not allocate: got %.2f allocs per run", allocs)
	}
}

func TestGoRuntimeGoexit(t *testing.T) {
	testSetup(t)
	done := make(chan struct{})
	Go(func() {
		defer close(done)
		runtime.Goexit()
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("deferred function did not run")
	}
	select {
	case <-Done():
		t.Error("runtime.Goexit should not be considered a panic")
	default:
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) {
	return f(p)
}

// Test that slow writes to STDERR do not prevent us from handling the panic
// and cancelling the context.
func TestHandlePanicSlowStderr(t *testing.T) {
	t.Cleanup(func() {
		stderr = os.Stderr
		testSetup(t) // replace context on exit
	})

	const delay = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), delay)
	t.Cleanup(cancel)
	stderr = writerFunc(func(p []byte) (int, error) {
		<-ctx.Done()
		return len(p), nil
	})

	Go(func() {
		panic(testErr)
	})

	start := time.Now()
	select {
	case <-Done():
		// Ok
	case now := <-time.After(delay * 5):
		t.Fatal("timed out after:", now.Sub(start))
	}
}

func TestHandlePanicPanicWriter(t *testing.T) {
	t.Cleanup(func() {
		stderr = os.Stderr
		testSetup(t) // replace context on exit
	})

	const delay = 10 * time.Millisecond
	stderr = writerFunc(func(_ []byte) (int, error) {
		panic("write")
	})

	Go(func() {
		panic(testErr)
	})

	start := time.Now()
	select {
	case <-Done():
		// Ok
	case now := <-time.After(delay * 5):
		t.Fatal("timed out after:", now.Sub(start))
	}
}

func TestNestedPanic(t *testing.T) {
	t.Skip("DELETE ME")
	testSetup(t)
	// defer testSetup(t)

	if false {
		func() {
			defer func() {
				panic("P2")
			}()
			panic("P1")
		}()
		return
	}
	Capture(func() {
		defer Capture(func() {
			panic("P2")
		})
		panic("P1")
	})
	panic(CapturedPanic())

	// fmt.Println(context.Cause(Context()))
}

func TestNotify(t *testing.T) {
	testSetup(t)

	cleanup := func(chs []chan *Error) {
		for _, c := range chs {
			Stop(c)
		}
	}

	t.Run("Buffered", func(t *testing.T) {
		chs := make([]chan *Error, 4)
		for i := range chs {
			chs[i] = make(chan *Error, 1)
			Notify(chs[i])
		}
		defer cleanup(chs)

		Capture(func() { panic(testErr) })
		for i, c := range chs {
			select {
			case e := <-c:
				if !reflect.DeepEqual(e.Value(), testErr) {
					t.Errorf("%d: got Error %v; want: %v", i, e.Value(), testErr)
				}
				select {
				case <-c:
					t.Errorf("%d: multiple sends on channel", i)
				default:
				}
			default:
				t.Errorf("%d: failed to send on channel", i)
			}
		}

		// Test Stop
		for i := range chs {
			if i&1 != 0 {
				Stop(chs[i])
			}
		}
		Capture(func() { panic(testErr) })
		for i, c := range chs {
			stopped := i&1 != 0
			select {
			case <-c:
				if stopped {
					t.Errorf("%d: send on stopped channel", i)
				}
			default:
				if !stopped {
					t.Errorf("%d: failed to send on channel", i)
				}
			}
		}
	})

	t.Run("NonBlocking", func(t *testing.T) {
		// use an unbuffered channel so that sends block
		chs := make([]chan *Error, 4)
		for i := range chs {
			chs[i] = make(chan *Error)
			Notify(chs[i])
		}
		defer cleanup(chs)

		Capture(func() { panic(testErr) })
		for i, c := range chs {
			select {
			case <-c:
				t.Errorf("%d: blocked channel should be ignored", i)
			default:
			}
		}
	})

	t.Run("NilChannel", func(t *testing.T) {
		defer func() {
			e := recover()
			if e == nil {
				t.Error("Notify(nil) should panic")
			}
			want := "panics: Notify using nil channel"
			got, _ := e.(string)
			if got != want {
				t.Errorf("expected panic %q got: %q", want, got)
			}
		}()
		Notify(nil)
	})

	// Make sure Stop doesn't error if the channel is nil or
	// was not registered with Notify

	t.Run("StopNil", func(t *testing.T) {
		Stop(nil)
	})

	t.Run("StopUntracked", func(t *testing.T) {
		Stop(make(chan *Error))
	})
}

// WARN: this is a dumb test
func TestNotifyMany(t *testing.T) {
	t.Skip("DELETE ME")
	testSetup(t)

	// done := make(chan struct{})
	var n atomic.Int64
	chs := make([]chan *Error, 4)
	var cwg sync.WaitGroup
	for i := range chs {
		chs[i] = make(chan *Error, 1)
		Notify(chs[i])
		cwg.Add(1)
		go func(ch chan *Error) {
			defer cwg.Done()
			i := 0
			for range ch {
				n.Add(1)
				i++
				if i >= 4 {
					Stop(ch)
					return
				}
			}
		}(chs[i])
	}
	defer func() {
		for _, c := range chs {
			Stop(c)
		}
	}()

	start := make(chan struct{})
	var pwg sync.WaitGroup
	for i := 0; i < 32; i++ {
		pwg.Add(1)
		go func(i int) {
			defer pwg.Done()
			<-start
			for j := 0; j < 16; j++ {
				Capture(func() { panic(fmt.Errorf("error %d:%d", i, j)) })
			}
		}(i)
	}

	close(start)
	pwg.Wait()
	for _, ch := range chs {
		close(ch)
	}
	cwg.Wait()
	t.Error("N:", n.Load())
}

// TODO: this is now more complicated that it needs to be
func testNotifyStopped(t *testing.T, ctx context.Context, timeout time.Duration) {
	// Test that the Context is cancelled and removed
	// from handlers.
	stopped := func() bool {
		select {
		case <-ctx.Done():
		default:
			return false
		}
		handlers.Lock()
		defer handlers.Unlock()
		_, ok := handlers.mctx[ctx.(*panicsCtx)]
		return !ok
	}

	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	to := time.NewTimer(timeout)
	defer to.Stop()

	start := time.Now()
	for {
		select {
		case <-tick.C:
			if stopped() {
				return
			}
		case <-to.C:
			if stopped() {
				return
			}
			t.Fatal("failed to Stop monitoring channel after cancelling Context:",
				time.Since(start))
		}
	}
}

// See if we can trigger a deadlock
func TestNotifyContextWait(t *testing.T) {
	t.Skip("TODO: we can delete this - doesn't add any coverage")
	testSetup(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cancelsFuncs := make([]context.CancelFunc, 0, 256)
			defer func() {
				for _, c := range cancelsFuncs {
					c()
				}
			}()
			for {
				select {
				case <-stop:
					return
				default:
					_, cancel := NotifyContext(context.Background())
					cancelsFuncs = append(cancelsFuncs, cancel)
				}
			}
		}()
	}

	close(start)
	go func() {
		<-time.After(time.Millisecond * 5)
		close(stop)
	}()
	Capture(func() { panic(testErr) })

	wg.Wait()
}

func TestNotifyContext(t *testing.T) {
	testSetup(t)

	t.Run("Panic", func(t *testing.T) {

		// WARN: fixing the WaitGroup race means we might leave a goroutine
		// around for a very brief amount of time
		numGoroutine := runtime.NumGoroutine()

		ctx, cancel := NotifyContext(context.Background())
		defer cancel()

		Capture(func() { panic(testErr) })
		select {
		case <-ctx.Done():
			e := ContextPanicError(ctx)
			if e == nil {
				t.Fatal("nil Context cause")
			}
			if !reflect.DeepEqual(e.Value(), testErr) {
				t.Errorf("got context cause: %v; want: %v", e.Value(), testErr)
			}
		default:
			// Wait a moment for the monitoring the goroutine
			// to receive the notification.
			t.Fatal("Context not canceled after panic")
		}

		// Make sure the monitoring goroutine eventually exists.
		for i := 0; i < 100; i++ {
			if runtime.NumGoroutine()-numGoroutine <= 0 {
				break
			}
			time.Sleep(time.Millisecond * 10)
		}
		if n := runtime.NumGoroutine() - numGoroutine; n > 0 {
			t.Errorf("Leaked %d goroutines", n)
		}

		testNotifyStopped(t, ctx, 0)
	})

	t.Run("Canceled", func(t *testing.T) {
		ctx, cancel := NotifyContext(context.Background())
		cancel() // Cancel immediately

		Capture(func() { panic(testErr) })
		select {
		case <-ctx.Done():
			e := ContextPanicError(ctx)
			if e != nil {
				t.Fatal("Cause should be nil when canceled")
			}
			if !errors.Is(context.Cause(ctx), context.Canceled) {
				t.Errorf("context.Cause(ctx) = %v; want: %v",
					context.Cause(ctx), context.Canceled)
			}
		default:
			t.Fatal("Context not canceled after panic")
		}

		testNotifyStopped(t, ctx, 0)
	})

	t.Run("ParentCanceled", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		ctx, cancel1 := NotifyContext(parent)
		defer cancel1()

		cancel() // Cancel parent immediately

		Capture(func() { panic(testErr) })
		select {
		case <-ctx.Done():
			e := ContextPanicError(ctx)
			if e != nil {
				t.Fatal("Cause should be nil when canceled")
			}
			if !errors.Is(context.Cause(ctx), context.Canceled) {
				t.Errorf("context.Cause(ctx) = %v; want: %v",
					context.Cause(ctx), context.Canceled)
			}
		default:
			t.Fatal("Context not canceled after panic")
		}

		testNotifyStopped(t, ctx, time.Millisecond*100)
	})
}

func generatePanics(t testing.TB, numPanics, callDepth int, panicErr error) (*sync.WaitGroup, func()) {
	if panicErr == nil {
		panicErr = testErr
	}
	if numPanics <= 0 {
		numPanics = runtime.NumCPU() * 8
	}
	start := make(chan struct{})
	stop := make(chan struct{})
	wg := new(sync.WaitGroup)
	ready := new(sync.WaitGroup)
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})
	for i := 0; i < numPanics; i++ {
		wg.Add(1)
		ready.Add(1)
		go func() {
			defer wg.Done()
			// Use a deep stack to make handling panics slow
			fn := bigstack.Func(callDepth, func() {
				panic(panicErr)
			})
			ready.Done()
			select {
			case <-stop:
				return
			case <-start:
				Capture(fn)
			}
		}()
	}
	ready.Wait()
	return wg, func() { close(start) }
}

// TODO: we should use the first panic when if busy and no panic
// has been captured yet.
//
// Test that we wait to process any in-flight panics when cancelling
// a notified Context.
func TestNotifyContextStop(t *testing.T) {
	testSetup(t)

	// Use a huge stack
	want := errors.New("first panic test")

	wg, start := generatePanics(t, -1, 16, want)
	ctx, cancel := NotifyContext(context.Background())
	defer cancel()

	start()
	for !panicked.Load() {
	}
	cancel()
	e := ContextPanicError(ctx)
	if e == nil {
		t.Fatalf("cancel cause should be a %T got: %#v",
			(*Error)(nil), context.Cause(ctx))
	}
	if e.Value() != want {
		t.Errorf("Value() = %v; want: %v", e.Value(), want)
	}

	// TODO: It would be nice if we could do this, but that
	// might be too difficult to be worth it.
	if e.Recovered() {
		// t.Fatal("Context not canceled with the first Error")
		t.Log("TODO: Context not canceled with the first Error")
	}

	wg.Wait()
}

// Test that First always returns the first panic and not
// a latter one.
func TestFirstPanic(t *testing.T) {
	testSetup(t)

	want := errors.New("first panic test")

	wg, start := generatePanics(t, 64, 16, want)

	ch := make(chan *Error, 128)
	Notify(ch)
	defer Stop(ch)

	start()
	var e *Error
	for {
		// If panicking make sure First waits until firstPanic is set.
		if panicked.Load() && First() == nil {
			t.Fatal("First returned nil despite a panic being actively processed")
		}
		if e = First(); e != nil {
			break
		}
	}
	wg.Wait()

	if ee := First(); ee != e {
		t.Fatalf("overwrote first panic: (%[1]p)%[1]v with: (%[2]p)%[2]v", e, ee)
	}
	if e.Recovered() {
		t.Errorf("Recovered() = %t; want: %t", true, false)
	}
	if e.Value() != want {
		t.Errorf("Value() = %v; want: %v", e.Value(), want)
	}
}

// WARN WARN WARN
func TestDoneX(t *testing.T) {

	t.Skip("FIXME")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 1_000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(time.Microsecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					select {
					case <-DoneX():
					default:
					}
				case <-stop:
					return
				}
			}
		}()
	}
	tick := time.NewTicker(time.Millisecond * 100)
	defer tick.Stop()
	to := time.NewTimer(time.Second * 5)
	start := time.Now()
Loop:
	for {
		select {
		case now := <-tick.C:
			c := created.Load()
			f := finalized.Load()
			fmt.Printf("%s: live: %d created: %d finalized: %d\n",
				now.Sub(start), c-f, c, f)
		case <-to.C:
			break Loop
		}
	}
	close(stop)
	wg.Wait()

	fmt.Print("\n# Cleanup:\n\n")
	{
		tick := time.NewTicker(time.Millisecond * 500)
		defer tick.Stop()
		to := time.NewTimer(time.Second * 5)
		start := time.Now()
		for {
			runtime.GC()
			select {
			case now := <-tick.C:
				c := created.Load()
				f := finalized.Load()
				fmt.Printf("%s: live: %d created: %d finalized: %d\n",
					now.Sub(start), c-f, c, f)
			case <-to.C:
				return
			}
		}
	}
}

func BenchmarkDoneX(b *testing.B) {
	b.Skip("DELETE ME")
	for i := 0; i < b.N; i++ {
		_ = DoneX()
		if i != 0 && i%1024*1024 == 0 {
			b.StopTimer()
			// handlers.m = nil
			for c := range handlers.m {
				Stop(c)
			}
			runtime.GC()
			b.StartTimer()
		}
	}
}

func BenchmarkHandlePanic(b *testing.B) {
	b.Cleanup(func() {
		stderr = os.Stderr
		testSetup(b) // replace context on exit
	})
	stack := debug.Stack()
	for len(stack) < 1024*1024 {
		stack = append(stack, '\n', '\n')
		stack = append(stack, stack...)
	}
	_ = stack // WARN
	e := Error{
		value: testErr,
		// stack:     stack, // WARN
		stack:     bytes.Repeat([]byte("a"), 128),
		recovered: false,
	}
	stderr = io.Discard
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		handlePanic(&e, time.Second)
	}
}

func BenchmarkCapture(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Capture(func() {})
	}
}

// TODO: this is only here for my own interest
func BenchmarkStackTrace(b *testing.B) {
	bigstack.Panic(16)

	stack := func(all bool) []byte {
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
		return buf
	}
	f := bigstack.Func(16, func() {
		_ = stack(false)
	})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			f()
		}
	})
	// for i := 0; i < b.N; i++ {
	// 	f()
	// 	// _ = stack(false)
	// }
}
