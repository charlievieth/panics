package panics

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func testSetup(t testing.TB) {
	ctx, cancelFn = context.WithCancelCause(context.Background())
	testNoLog.Store(true)
	t.Cleanup(func() {
		testNoLog.Store(false)
		ctx, cancelFn = context.WithCancelCause(context.Background())
	})
}

// func TestCancelParentContext(t *testing.T) {
// 	testSetup(t)
// 	parent := Context()
// 	_, cancel := context.WithCancel(parent)
// 	cancel()
// 	select {}
// }

var testErr = errors.New("test error")

func TestCapturePanic(t *testing.T) {
	testSetup(t)
	ctx := Context()
	Capture(func() {
		panic(testErr)
	})
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
	if !reflect.DeepEqual(perr.Value(), testErr) {
		t.Errorf("Value() = %v; want: %v", err, testErr)
	}
	if !reflect.DeepEqual(perr.Unwrap(), testErr) {
		t.Errorf("Unwrap() = %v; want: %v", err, testErr)
	}
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

func TestGoFuncRuntimeGoexit(t *testing.T) {
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

func TestHugeStack(t *testing.T) {
	t.Skip("DELETE ME") // WARN
	testSetup(t)
	done := make(chan struct{})
	f := func() {
		panic(testErr)
	}
	for i := 0; i < 500; i++ {
		fn := f
		f = func() {
			fn()
		}
	}
	Go(func() {
		defer close(done)
		f()
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
	t.Error(err)
}
