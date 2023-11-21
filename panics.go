package panics

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
)

var ctx, cancelFn = func() (context.Context, context.CancelCauseFunc) {
	return context.WithCancelCause(context.Background())
}()

// A Error is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type Error struct {
	value any
	stack []byte
}

// Error implements error interface.
func (p *Error) Error() string {
	return fmt.Sprintf("%v\n\n%s", p.value, p.stack)
}

// Stack returns the stack trace that was capturing during program panic.
func (p *Error) Stack() []byte {
	return append([]byte(nil), p.stack...) // return a copy
}

// Value returns the value that the program panicked with.
func (p *Error) Value() any {
	return p.value
}

// WARN: we might not have panicked with an [error] so do we want this??
func (p *Error) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// Context returns a non-nill [context.Context] that is closed if the program
// panics. The panic and a stack trace captured at the panic site can be
// retrieve using [context.Cause] which will return an error with type [*Error].
func Context() context.Context { return ctx }

// Done is a convenience function that returns the Done channel of the panics
// package [context.Contex]. It will be closed if the program panics.
func Done() <-chan struct{} { return ctx.Done() }

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

var testNoLog atomic.Bool

// TODO: handle nested panics and nested uses of panics.Go
func Capture(fn func()) {
	defer func() {
		if e := recover(); e != nil {
			// TODO: do we need to clean up the stack ???
			// Capture the stack here
			buf := make([]byte, 8*1024)
			for {
				n := runtime.Stack(buf, includeAllGoroutines())
				if n < len(buf) {
					buf = buf[:n]
					break
				}
				buf = make([]byte, 2*len(buf))
			}
			// TODO: handle writing to STDERR blocking ???
			defer cancelFn(&Error{value: e, stack: buf})
			if !testNoLog.Load() {
				fmt.Fprintf(os.Stderr, "panic: %v\n\n%s\n", e, buf)
			}
		}
	}()
	fn()
}

// TODO: handle nested panics and nested uses of panics.Go
func Go(fn func()) { go Capture(fn) }
