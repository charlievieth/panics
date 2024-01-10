package panics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// bigstack helps generate large stack traces by having a very long import path
	"github.com/charlievieth/panics/internal/bigstack/pkg/pkg1/pkg2/pkg3/pkg4/bigstack"
)

func resetHandlers(t testing.TB) {
	panicsWaitUntilIdle()

	handlers.Lock()
	defer handlers.Unlock()

	// Test that we don't leak references when deleting elements from
	// the stopping* slices.
	if cap(handlers.stopping) > 0 {
		a := handlers.stopping
		a = a[len(a):cap(a)]
		for i, c := range a {
			if c != nil {
				t.Errorf("Leaked %T: handlers.stopping[%d] = %v; want: %v", c, i, c, nil)
			}
		}
	}
	if cap(handlers.stoppingCtx) > 0 {
		a := handlers.stoppingCtx
		a = a[len(a):cap(a)]
		for i, c := range a {
			if c != nil {
				t.Errorf("Leaked %T: handlers.stoppingCtx[%d] = %v; want: %v", c, i, c, nil)
			}
		}
	}

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
	panicCount.Store(0)

	SetOutput(nil)
	IncludeAllStackTraces(false)
}

func testSetup(t testing.TB) {
	goroutines := runtime.NumGoroutine()
	t.Cleanup(func() {
		// Make sure all tests cleanup their goroutines
		now := time.Now()
		for i := 0; time.Since(now) <= time.Second; i++ {
			n := runtime.NumGoroutine()
			if n <= goroutines {
				return
			}
			runtime.Gosched() // let goroutines exit
			if i >= 10 {
				// Stop busy spinning and actually sleep between checks
				time.Sleep(5 * time.Millisecond)
			}
		}
		if n := runtime.NumGoroutine(); n > goroutines {
			t.Errorf("leaked %d goroutines", n-goroutines)
		}
	})

	resetHandlers(t)
	t.Cleanup(func() {
		resetHandlers(t)
	})
}

var testErr = errors.New("test error")

func testNotify(t testing.TB) <-chan *Error {
	ch := make(chan *Error, 1)
	Notify(ch)
	t.Cleanup(func() {
		Stop(ch)
	})
	return ch
}

func testCapturePanic(t *testing.T, fn func(call func())) {
	testSetup(t)
	ch := testNotify(t)
	fn(func() { panic(testErr) })
	select {
	case <-ch:
		// Ok
	case <-time.After(time.Second):
		// WARN: remove time.After
		t.Fatal("panic did not cancel Context")
	}
	err := First()
	if err == nil {
		t.Fatal("nil error")
	}
	if !reflect.DeepEqual(err.Value(), testErr) {
		t.Errorf("Value() = %v; want: %v", err, testErr)
	}
	if !reflect.DeepEqual(err.Unwrap(), testErr) {
		t.Errorf("Unwrap() = %v; want: %v", err, testErr)
	}
	if len(err.Stack()) == 0 {
		t.Error("empty stack trace")
	}
}

func TestCapturePanic(t *testing.T) {
	testCapturePanic(t, func(fn func()) { Capture(fn) })
}

func TestGoPanic(t *testing.T) {
	testCapturePanic(t, Go)
}

func TestGoWGPanic(t *testing.T) {
	var wg sync.WaitGroup
	testCapturePanic(t, func(fn func()) {
		GoWG(&wg, fn)
	})
}

func TestGoWGNilWaitGroup(t *testing.T) {
	const want = "panics: cannot call GoWG with nil sync.WaitGroup"
	defer func() {
		e := recover()
		s, _ := e.(string)
		if s != want {
			t.Fatalf("expected panic: %q; got: %v", want, e)
		}
	}()
	GoWG(nil, func() {})
}

func TestCapturePanicError(t *testing.T) {
	testSetup(t)
	err := Capture(func() { panic(testErr) }).(*Error)
	if !reflect.DeepEqual(err.Value(), testErr) {
		t.Errorf("Value() = %v; want: %v", err, testErr)
	}
	if err.ID() != 1 {
		t.Errorf("ID() = %d; want: %d", err.ID(), 1)
	}
}

func TestCaptureAllStackTraces(t *testing.T) {
	testSetup(t)
	var buf bytes.Buffer
	SetOutput(&buf)
	IncludeAllStackTraces(true)

	numGoroutine := runtime.NumGoroutine()
	wg, start := generatePanics(t, 32, 1, testErr)
	start()

	wg.Wait()
	err := First()
	if err == nil {
		t.Fatal("First() == nil")
	}

	n := strings.Count(err.Stack(), "goroutine") - numGoroutine
	if n < 32 {
		t.Fatalf("Captured stack trace should include at least %d goroutines found: %d\n%s",
			32, n, err.Stack())
	}
}

// TODO: export ???
// TODO: rename
func WaitFor(ch <-chan *Error) <-chan *Error {
	// Can't wait for un-buffered channels.
	if len(ch) == 0 && cap(ch) > 0 {
		panicsWaitUntilIdle()
	}
	return ch
}

func TestWaitFor(t *testing.T) {
	testSetup(t)
	ch := make(chan *Error, 1)
	Notify(ch)
	hits := 0
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		Go(func() {
			defer wg.Done()
			panic(1)
		})
		wg.Wait()
		select {
		case <-WaitFor(ch):
			hits++
		default:
			t.Error("missed panic:", i)
		}
	}
	if hits != 10 {
		t.Errorf("missed %d/%d panics", 10-hits, 10)
	}
}

func TestCapturePanicNilFunc(t *testing.T) {
	testSetup(t)
	ch := testNotify(t)
	Capture(nil)
	select {
	case <-ch:
		// Ok
	default:
		t.Error("panic did not cancel Context")
	}
	err := First()
	if err == nil {
		t.Fatal("nil error")
	}
	if _, ok := err.Value().(runtime.Error); !ok {
		t.Errorf("expected runtime.Error got: %T", err)
	}
}

// func TestNestedCapture(t *testing.T) {
// 	t.Skip("FIXME")
// 	testSetup(t)
// 	panicked := Capture(func() {
// 		Capture(func() {
// 			panic(testErr)
// 		})
// 	})
// 	if !panicked {
// 		t.Fatal("WAT:", panicked)
// 	}
// }

func TestCaptureAllocs(t *testing.T) {
	testSetup(t)
	allocs := testing.AllocsPerRun(100, func() {
		Capture(func() {})
	})
	if allocs != 0 {
		t.Errorf("Unexpected number of allocations, got %f, want 0", allocs)
	}
}

// Test that slow writes to STDERR do not prevent us from handling the panic
// and cancelling the context.
func TestHandlePanicSlowStderr(t *testing.T) {
	t.Cleanup(func() {
		testSetup(t) // replace context on exit
	})

	const delay = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), delay)
	t.Cleanup(cancel)
	SetOutput(WriterFunc(func(p []byte) { <-ctx.Done() }))

	ch := testNotify(t)
	Go(func() {
		panic(testErr)
	})

	start := time.Now()
	select {
	case <-ch:
		// Ok
	case now := <-time.After(delay * 5):
		t.Fatal("timed out after:", now.Sub(start))
	}
}

func TestHandlePanicPanicWriter(t *testing.T) {
	t.Cleanup(func() {
		testSetup(t) // replace context on exit
	})

	const delay = 10 * time.Millisecond
	SetOutput(WriterFunc(func(_ []byte) { panic("write") }))

	ch := testNotify(t)
	Go(func() {
		panic(testErr)
	})

	start := time.Now()
	select {
	case <-ch:
		// Ok
	case now := <-time.After(delay * 5):
		t.Fatal("timed out after:", now.Sub(start))
	}
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

func TestNotifyContext(t *testing.T) {
	t.Run("Panic", func(t *testing.T) {
		testSetup(t)

		// WARN: fixing the WaitGroup race means we might leave a goroutine
		// around for a very brief amount of time
		numGoroutine := runtime.NumGoroutine()

		ctx, cancel := NotifyContext(context.Background())
		defer cancel()

		Capture(func() { panic(testErr) })
		select {
		case <-ctx.Done():
			e := ContextError(ctx)
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
		start := time.Now()
		for time.Since(start) < time.Second {
			if runtime.NumGoroutine()-numGoroutine <= 0 {
				break
			}
			runtime.Gosched()
		}
		if n := runtime.NumGoroutine() - numGoroutine; n > 0 {
			t.Errorf("Leaked %d goroutines", n)
		}

		testNotifyStopped(t, ctx, 0)
	})

	t.Run("Canceled", func(t *testing.T) {
		testSetup(t)

		ctx, cancel := NotifyContext(context.Background())
		cancel() // Cancel immediately

		Capture(func() { panic(testErr) })
		select {
		case <-ctx.Done():
			e := ContextError(ctx)
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
		testSetup(t)

		parent, cancel := context.WithCancel(context.Background())
		ctx, cancel1 := NotifyContext(parent)
		defer cancel1()

		cancel() // Cancel parent immediately

		Capture(func() { panic(testErr) })
		select {
		case <-ctx.Done():
			e := ContextError(ctx)
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

	t.Run("NilContext", func(t *testing.T) {
		defer func() {
			if e := recover(); e == nil {
				t.Fatal("NotifyContext(nil) should panic")
			}
		}()
		NotifyContext(nil)
	})
}

func contextCancelled(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func TestNotifyContextReuse(t *testing.T) {
	testSetup(t)
	ctx1, cancel1 := NotifyContext(context.Background())
	// Put a non *panicsCtx in the middle to make
	// sure our Value() method really works.
	ctx2, cancel2 := context.WithCancel(ctx1)
	ctx3, cancel3 := NotifyContext(ctx2)
	ctx4, cancel4 := context.WithCancel(ctx3)
	cancel3()
	if !contextCancelled(ctx4) {
		t.Fatal("failed to cancel ctx4")
	}
	t.Cleanup(func() {
		for _, c := range []context.CancelFunc{cancel1, cancel2, cancel3, cancel4} {
			c()
		}
	})
	if n := len(handlers.mctx); n != 1 {
		t.Fatalf("len(handlers.mctx) = %d; want: %d", n, 1)
	}
	cancel2()
	if !contextCancelled(ctx2) {
		t.Fatal("failed to cancel ctx2")
	}
	if !contextCancelled(ctx3) {
		t.Fatal("failed to cancel ctx3")
	}
	if contextCancelled(ctx1) {
		t.Fatal("canceled ctx2")
	}
	cancel1()
	if !contextCancelled(ctx1) {
		t.Fatal("failed to cancel ctx1")
	}
	if !contextCancelled(ctx2) {
		t.Fatal("failed to cancel ctx2")
	}
}

func TestNotifyContextReuseNotifiedParent(t *testing.T) {
	testSetup(t)

	ctx1, cancel1 := NotifyContext(context.Background())
	defer cancel1()

	Capture(func() { panic(testErr) })
	if !contextCancelled(ctx1) {
		t.Fatal("failed to cancel context")
	}

	// ctx2 should not be canceled because the panic that canceled
	// ctx1 occurs before the creation of ctx2
	ctx2, cancel2 := NotifyContext(ctx1)
	defer cancel2()

	// ctx1 has been removed from the handlers and ctx2 was not
	// registered because it is a child of ctx1
	if n := len(handlers.mctx); n != 0 {
		t.Fatalf("len(handlers.mctx) = %d; want: %d", n, 1)
	}
	if !contextCancelled(ctx2) {
		t.Fatal("failed to cancel child context")
	}
}

// Test that when the parent is already registered with NotifyContext we
// don't register it but instead return a copy.
func TestNotifyContextAllocs(t *testing.T) {
	testSetup(t)
	ctx, cancel := NotifyContext(context.Background())
	t.Cleanup(cancel)
	allocs := testing.AllocsPerRun(100, func() {
		_, cancel := NotifyContext(ctx)
		cancel()
	})
	want := 3.0
	if allocs != want {
		t.Fatalf("Unexpected number of allocations, got %.2f, want %.2f", allocs, want)
	}
}

func TestNotifyContextGoroutines(t *testing.T) {
	testSetup(t)

	ctx, cancel := NotifyContext(context.Background())
	t.Cleanup(cancel)

	goroutines := runtime.NumGoroutine()
	cancelFuncs := make([]context.CancelFunc, 100)
	for i := range cancelFuncs {
		_, cancelFuncs[i] = NotifyContext(ctx)
	}
	t.Cleanup(func() {
		for _, c := range cancelFuncs {
			c()
		}
	})

	// Since the contexts we're creating are the children of a cancelCtx
	// we should not be creating an goroutines.
	//
	// Use a loop since we need to handle the case of other tests leaking
	// a goroutine.
	tick := time.NewTicker(10 * time.Millisecond)
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= goroutines {
			return
		}
		<-tick.C
		runtime.GC()
	}
	if n := runtime.NumGoroutine(); n > goroutines {
		t.Errorf("runtime.NumGoroutine() = %d; want: %d\n\n## Stack:\n%s\n##",
			n, goroutines, stackTrace())
	}
}

func stackTrace() []byte {
	buf := make([]byte, 4096)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	return buf
}

// createGoroutines creates count goroutines that will wait until the returned
// start function is called to call fn via Capture. The returned WaitGroup
// should be waited on to ensure all the goroutines exited.
func createGoroutines(t testing.TB, count int, fn func()) (_ *sync.WaitGroup, start func()) {
	if count <= 0 {
		count = runtime.NumCPU() * 8
	}
	startCh := make(chan struct{})
	stop := make(chan struct{})
	wg := new(sync.WaitGroup)
	ready := new(sync.WaitGroup)
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})
	for i := 0; i < count; i++ {
		wg.Add(1)
		ready.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			select {
			case <-stop:
				return
			case <-startCh:
				Capture(fn)
			}
		}()
	}
	ready.Wait()
	return wg, func() { close(startCh) }
}

func generatePanics(t testing.TB, numPanics, callDepth int, panicErr any) (*sync.WaitGroup, func()) {
	if panicErr == nil {
		panicErr = testErr
	}
	if numPanics <= 0 {
		numPanics = runtime.NumCPU() * 8
	}
	return createGoroutines(t, numPanics, bigstack.Func(callDepth, func() {
		panic(panicErr)
	}))
}

// Test that we wait to process any in-flight panics when cancelling
// a notified Context.
//
// TODO: It would be really nice if we could set the cancel cause of
// all Contexts created before we panicked to the first panic itself,
// but I'm afraid the coordination required for this would add too
// much overhead.
func TestNotifyContextStop(t *testing.T) {
	testSetup(t)

	// Use a huge stack
	want := errors.New("first panic test")

	wg, start := generatePanics(t, -1, 16, want)
	ctx, cancel := NotifyContext(context.Background())
	defer cancel()

	start()
	for panicCount.Load() == 0 {
	}
	cancel()
	e := ContextError(ctx)
	if e == nil {
		t.Fatalf("cancel cause should be a %T got: %#v",
			(*Error)(nil), context.Cause(ctx))
	}
	if e.Value() != want {
		t.Errorf("Value() = %v; want: %v", e.Value(), want)
	}

	// TODO: It would be nice if we could do this, but that
	// might be too difficult to be worth it.
	if !e.First() {
		t.Log("TODO: Context not canceled with the first Error")
	}

	wg.Wait()
}

// Test that First always returns the first panic and not
// a latter one.
func TestFirstPanic(t *testing.T) {
	testSetup(t)

	want := errors.New("first panic test")

	wg, start := generatePanics(t, -1, 16, want)

	ch := make(chan *Error, 128)
	Notify(ch)
	defer Stop(ch)

	start()
	var e *Error
	for {
		// If panicking make sure First waits until firstPanic is set.
		if panicCount.Load() != 0 && First() == nil {
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
	if !e.First() {
		t.Errorf("First() = %t; want: %t", false, true)
	}
	if e.Value() != want {
		t.Errorf("Value() = %v; want: %v", e.Value(), want)
	}
}

// TODO: rename
func TestFirstPanicHard(t *testing.T) {
	// Stress test how often the panic returned by First is actually
	// the first panic that occured. We don't control the runtime so
	// if multiple panics occur at the exact same time the first
	// panic that we handle might not be exactly the first one that
	// occured.

	test := func(t *testing.T, num int) {
		t.Run(fmt.Sprintf("%d", num), func(t *testing.T) {
			testSetup(t)
			hit := 0
			var u atomic.Uint32
			for i := 0; i < 100; i++ {
				firstPanic.Store(nil)
				panicCount.Store(0)
				u.Store(0)
				wg, start := createGoroutines(t, num, func() {
					panic(u.Swap(1))
				})
				start()
				wg.Wait()
				if First().Value().(uint32) == 0 {
					hit++
				}
			}
			if hit <= 90 {
				t.Errorf("First() only returned the first panic in %d/100 tests", hit)
			} else {
				t.Logf("%d/100", hit)
			}
		})
	}

	sizes := []int{
		1,
		runtime.NumCPU(),
		runtime.NumCPU() * 2,
	}
	if !testing.Short() {
		sizes = append(sizes, runtime.NumCPU()*8)
		sizes = append(sizes, runtime.NumCPU()*16)
	}
	if runtime.GOMAXPROCS(-1) != runtime.NumCPU() {
		sizes = append(sizes, runtime.GOMAXPROCS(-1), runtime.GOMAXPROCS(-1)*2)
		sort.Ints(sizes)
	}
	for _, n := range sizes {
		test(t, n)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestSetOutput(t *testing.T) {
	testSetup(t)

	equal := func(w1, w2 *writer) bool {
		if w1 != nil && w2 != nil {
			return *w1 == *w2
		}
		return w1 == w2
	}

	tests := []struct {
		wr   io.Writer
		want *writer
	}{
		{nil, nil},
		{io.Discard, nil},
		{os.Stderr, &stderrWriter},
		{(*bytes.Buffer)(nil), nil},
		{(*os.File)(nil), nil},
		{nopWriter{}, &writer{w: nopWriter{}}},
	}
	for i, test := range tests {
		SetOutput(test.wr)
		if got := output.Load(); !equal(got, test.want) {
			t.Errorf("%d: SetOutput(%#v) = %+v; want: %+v", i, test.wr, got, test.want)
		}
		SetOutput(nil)
	}
}

func TestSetOutputNilWriter(t *testing.T) {
	testSetup(t)

	var wr *bytes.Buffer = nil
	SetOutput(wr)
	fmt.Fprintln(output.Load(), "DO NOT PANIC")
}

func TestSetOutputPanickingWriter(t *testing.T) {
	testSetup(t)

	// Make sure we ignore any panics while writing
	SetOutput(WriterFunc(func(p []byte) {
		panic("nope 1")
	}))
	Capture(func() { panic("panic") })
}

func TestPanicsContext(t *testing.T) {
	test := func(t *testing.T, parent context.Context, want string, replace func(string) string) {
		t.Helper()
		ctx, cancel := NotifyContext(parent)
		defer cancel()
		got := ctx.(fmt.Stringer).String()
		orig := got
		if replace != nil {
			got = replace(got)
		}
		if got != want {
			t.Logf("Original: %q", orig)
			t.Errorf("(%T).String() = %q; want: %q", ctx, got, want)
		}
	}

	ctx1 := context.Background()
	test(t, ctx1, "panics.NotifyContext(context.Background)", nil)

	ctx2, cancel2 := context.WithCancel(ctx1)
	defer cancel2()
	test(t, ctx2, "panics.NotifyContext(context.Background.WithCancel)", nil)

	ctx3, cancel3 := context.WithTimeout(ctx2, time.Second)
	defer cancel3()
	replace := func(s string) string {
		return regexp.MustCompile(`\(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+[^)]+\)`).
			ReplaceAllString(s, "")
	}
	test(t, ctx3, "panics.NotifyContext(context.Background.WithCancel.WithDeadline)", replace)

	{
		// Make sure that our short-circuiting of canceled parent contexts
		// does not change the string representation.
		parent, cancel := NotifyContext(context.Background())
		c1, f1 := NotifyContext(parent)
		cancel()
		c2, f2 := NotifyContext(parent)
		defer f1()
		defer f2()
		s1 := c1.(fmt.Stringer).String()
		s2 := c2.(fmt.Stringer).String()
		if s1 != s2 {
			t.Errorf("(%T).String() = %q; (%T).String() = %q", c1, s1, c2, s2)
		}
	}
}

func TestErrorUnwrap(t *testing.T) {
	var tests = []struct {
		err  Error
		want error
	}{
		{Error{value: nil}, nil},
		{Error{value: 1}, nil},
		{Error{value: "foo"}, nil},
		{Error{value: testErr}, testErr},
	}
	for _, test := range tests {
		got := test.err.Unwrap()
		if got != test.want {
			t.Errorf("%+v.Unwrap() = %v; want: %v", test.err, got, test.want)
		}
	}
}

func BenchmarkCapture(b *testing.B) {
	testSetup(b)
	for i := 0; i < b.N; i++ {
		Capture(func() {
			return
		})
	}
}

func BenchmarkNotifyContext(b *testing.B) {
	b.Run("NoParent", func(b *testing.B) {
		testSetup(b)
		for i := 0; i < b.N; i++ {
			_, f := NotifyContext(context.Background())
			f()
		}
	})
	b.Run("ParentRegistered", func(b *testing.B) {
		testSetup(b)
		parent, cancel := NotifyContext(context.Background())
		defer cancel()
		for i := 0; i < b.N; i++ {
			_, f := NotifyContext(parent)
			f()
		}
	})
	b.Run("ParentCanceled", func(b *testing.B) {
		testSetup(b)
		parent, cancel := NotifyContext(context.Background())
		cancel()
		for i := 0; i < b.N; i++ {
			_, f := NotifyContext(parent)
			f()
		}
	})
}
