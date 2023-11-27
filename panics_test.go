package panics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	// bigstack helps generate large stack traces by having a very long import path
	"github.com/charlievieth/panics/internal/bigstack/pkg/pkg1/pkg2/pkg3/pkg4/bigstack"
)

func testSetup(t testing.TB) {
	ctx, cancelFn = context.WithCancelCause(context.Background())
	testNoLog.Store(true)
	t.Cleanup(func() {
		testNoLog.Store(false)
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
