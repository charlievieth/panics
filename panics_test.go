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
	panicked.Store(false)
}

func testSetup(t testing.TB) {
	SetOutput(nil)
	resetHandlers(t)
	t.Cleanup(func() {
		SetOutput(os.Stderr)
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
	testCapturePanic(t, Capture)
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

func TestCaptureValue(t *testing.T) {
	testSetup(t)
	i := CaptureValue(func() int {
		return 1
	})
	if i != 1 {
		t.Errorf("CaptureValue() = %d; want: %d", i, 1)
	}
	ctx, cancel := NotifyContext(context.Background())
	defer cancel()
	j := CaptureValue(func() int {
		panic("here")
		// unreachable
	})
	if j != 0 {
		t.Errorf("CaptureValue() = %d; want: %d", j, 0)
	}
	select {
	case <-ctx.Done():
		// Ok
	default:
		// WARN: remove time.After
		t.Fatal("panic did not cancel Context")
	}
}

func TestCaptureValues(t *testing.T) {
	testSetup(t)
	i, err := CaptureValues(func() (int, error) {
		return 1, testErr
	})
	if i != 1 || err != testErr {
		t.Errorf("CaptureValues() = %d, %v; want: %d, %v", i, err, 1, testErr)
	}
	ctx, cancel := NotifyContext(context.Background())
	defer cancel()
	j, err := CaptureValues(func() (int, error) {
		panic("here")
		// unreachable
	})
	if j != 0 || err != nil {
		t.Errorf("CaptureValues() = %d, %v; want: %d, %v", j, err, 0, nil)
	}
	select {
	case <-ctx.Done():
		// Ok
	default:
		// WARN: remove time.After
		t.Fatal("panic did not cancel Context")
	}
}

// WARN: rename
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
	for !panicked.Load() {
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

	wg, start := generatePanics(t, -1, 16, want)

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

func TestFirstPanicHard(t *testing.T) {
	want := errors.New("first panic")
	test := func(t *testing.T, num int) {
		t.Run(fmt.Sprintf("%d", num), func(t *testing.T) {
			testSetup(t)
			hit := 0
			for i := 0; i < 100; i++ {
				firstPanic.Store(nil)
				panicked.Store(false)
				var first atomic.Bool
				wg, start := createGoroutines(t, num, func() {
					if first.CompareAndSwap(false, true) {
						panic(want)
					}
					panic(testErr)
				})
				start()
				wg.Wait()
				if First().Value() == want {
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

	sizes := []int{1, runtime.NumCPU(), runtime.NumCPU() * 2, runtime.NumCPU() * 8}
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
}

func BenchmarkHandlePanic(b *testing.B) {
	b.Cleanup(func() {
		// WARN: we should not be doing this in cleanup !!
		testSetup(b) // replace context on exit
	})
	e := Error{
		value:     testErr,
		stack:     bytes.Repeat([]byte("a"), 128),
		recovered: false,
	}
	SetOutput(nopWriter{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		handlePanic(&e, WriteTimeout)
	}
}

func BenchmarkCapture(b *testing.B) {
	testSetup(b)
	b.Run("NoPanic", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			Capture(func() {})
		}
	})
	b.Run("Panic", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			Capture(func() { bigstack.Panic(16) })
			// Capture(func() { panic(1) })
		}
	})
}

// TODO: this is only here for my own interest
func BenchmarkStackTrace(b *testing.B) {
	b.ReportAllocs()
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
