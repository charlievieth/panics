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
	"runtime/debug"
	"sync"
	"testing"
	"time"

	// bigstack helps generate large stack traces by having a very long import path
	"github.com/charlievieth/panics/internal/bigstack/pkg/pkg1/pkg2/pkg3/pkg4/bigstack"
)

func resetHandlers() {
	handlers.Lock()
	defer handlers.Unlock()
	for k := range handlers.m {
		delete(handlers.m, k)
	}
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

func TestHugeStack(t *testing.T) {
	// t.Skip("DELETE ME") // WARN
	testSetup(t)
	done := make(chan struct{})
	Go(func() {
		defer close(done)
		bigstack.Panic(100)
	})
	select {
	case <-Done():
	case <-time.After(time.Second):
		t.Error("deferred function did not run")
	}
	err := context.Cause(ctx)
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("error should have type: %T; got: %T", perr, err)
	}
	const caller = "github.com/charlievieth/panics/internal/bigstack/pkg/pkg1/pkg2/pkg3/pkg4/bigstack.Panic.func"
	n := bytes.Count(perr.Stack(), []byte(caller))
	if n < 20 {
		t.Error("")
	}
	re := regexp.MustCompile(`(?m)^\s*created by github\.com/charlievieth/panics\.Go in goroutine \d+`)
	if !re.Match(perr.Stack()) {
		t.Errorf("stack trace does not match regex: `%s`:\n\n## Stack trace:\n%s\n###\n",
			re, perr.Stack())
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
			if recover() == nil {
				t.Error("Notify(nil) should panic")
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

func testNotifyStopped(t *testing.T, ctx context.Context, timeout time.Duration) {
	ch := ctx.(*panicsCtx).ch
	if ch == nil {
		t.Fatal("nil Context channel")
	}

	stopped := func() bool {
		handlers.Lock()
		defer handlers.Unlock()
		_, ok := handlers.m[ch]
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
		case now := <-to.C:
			if stopped() {
				return
			}
			t.Fatal("failed to Stop monitoring channel after cancelling Context:",
				now.Sub(start))
		}
	}
}

// See if we can trigger a deadlock
func TestNotifyContextWait(t *testing.T) {
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
		// numGoroutine := runtime.NumGoroutine()

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
				t.Errorf("got Error %v; want: %v", e.Value(), testErr)
			}

		// TODO: if we wait for the Context's to be canceled we don't need to wait
		// case <-time.After(time.Millisecond * 100):
		default:
			// Wait a moment for the monitoring the goroutine
			// to receive the notification.
			t.Fatal("Context not canceled after panic")
		}

		// if n := runtime.NumGoroutine() - numGoroutine; n > 0 {
		// 	t.Fatalf("Leaked %d goroutines", n)
		// }

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

		cancel() // Cancel immediately
		defer cancel1()

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

func TestNotifyContext_NumGoroutine(t *testing.T) {
	testSetup(t)
	// panic("EXIT")
	// t.Fatal(runtime.NumGoroutine())
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
	for i := 0; i < b.N; i++ {
		_ = DoneX()
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
