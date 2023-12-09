package panics_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/charlievieth/panics"
)

func ExampleCapture_waitGroup() {
	panics.SetPrintStackTrace(false)

	ctx, cancel := panics.NotifyContext(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		// Defer the call to wg.Done until after panics.Capture returns
		// otherwise Done will be called before the panic is handled and
		// wg.Wait may return before the context is canceled.
		defer wg.Done()
		panics.Capture(func() {
			panic("abort")
		})
	}()
	wg.Wait()

	select {
	case <-ctx.Done():
		fmt.Println("canceled")
	default:
		fmt.Println("failed to cancel context after panic")
	}

	// Output:
	// canceled
}

func Example_goWG() {
	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)
	var wg sync.WaitGroup
	panics.GoWG(&wg, func() {
		panic("here")
	})
	wg.Wait()
	// If GoWG is not used then ch may not have been notified of the panic
	// before wg.Wait returns.
	fmt.Println(<-ch)
}
