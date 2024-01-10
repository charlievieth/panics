package panics_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/charlievieth/panics"
)

func ExampleFirst() {
	panics.PrintStackTrace(false)

	panics.Capture(func() { panic("my panic") })
	fmt.Println(panics.First().Value())

	// Output:
	// my panic
}

func ExampleNotify() {
	panics.PrintStackTrace(false)

	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)

	panics.Capture(func() { panic("my panic") })
	e := <-ch
	fmt.Println(e.Value())

	// Output:
	// my panic
}

func ExampleNotifyContext() {
	panics.PrintStackTrace(false)

	ctx, cancel := panics.NotifyContext(context.Background())
	defer cancel()

	panics.Capture(func() { panic("my panic") })
	select {
	case <-ctx.Done():
		e, _ := context.Cause(ctx).(*panics.Error)
		fmt.Println(e.Value())
	default:
		panic("failed to capture panic!")
	}

	// Output:
	// my panic
}

func ExampleCapture() {
	panics.PrintStackTrace(false) // quiet output

	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)

	panics.Capture(func() {
		panic("my panic")
	})

	e := <-ch
	fmt.Println(e.Value())

	// Output:
	// my panic
}

func ExampleCapture_waitGroup() {
	panics.PrintStackTrace(false)

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

func ExampleGoWG() {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		i := i
		panics.GoWG(&wg, func() {
			fmt.Println(i)
		})
	}
	wg.Wait()

	// Unordered output:
	// 0
	// 1
	// 2
	// 3
}

func ExampleGoWG_panic() {
	panics.PrintStackTrace(false)

	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)

	var wg sync.WaitGroup
	panics.GoWG(&wg, func() {
		panic("here")
	})
	wg.Wait()

	// If GoWG is not used then ch may not have been notified of the panic
	// before wg.Wait returns.
	select {
	case <-ch:
		fmt.Println("ch was notified of the panic")
	default:
		fmt.Println("error: Wait returned before ch was notified")
	}

	// Output:
	// ch was notified of the panic
}

// WARN WARN WARN WRAN
// func ExampleSetOutput() {
// 	// Use an arbitrary function as the panic output
// 	ctx, cancel := panics.NotifyContext(context.Background())
// 	defer cancel()
//
// 	var n int
// 	panics.SetOutput(panics.WriterFunc(func(p []byte) {
// 		n++
// 	}))
// 	for i := 0; i < 5; i++ {
// 		panics.Capture(func() { panic("my panic") })
// 	}
// 	<-ctx.Done()
// 	fmt.Printf("captured %d panics\n", n)
//
// 	// Output:
// 	// captured 5 panics
// }

// func ExampleSetOutput_slowWriter() {
// 	// Use an arbitrary function as the panic output
// 	var n int
// 	panics.SetOutput(panics.WriterFunc(func(p []byte) {
// 		n++
// 	}))
// 	for i := 0; i < 5; i++ {
// 		panics.Capture(func() { panic("my panic") })
// 	}
// 	fmt.Printf("captured %d panics\n", n)
//
// 	// Output:
// 	// captured 5 panics
// }

func ExampleWriterFunc() {
	// This example shows how you can use an arbitrary logger as
	// the panic output.

	ctx, cancel := panics.NotifyContext(context.Background())
	defer cancel()

	panics.SetOutput(panics.WriterFunc(func(p []byte) {
		log.Printf("Panic output:\n#########\n%s\n#########", p)
	}))

	panics.Capture(func() { panic("my panic") })
	<-ctx.Done() // Wait for the panic to be handled.
}

func ExampleError_writerTo() {
	panics.PrintStackTrace(false)

	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)
	panics.Capture(func() { panic("here") })

	e := <-ch
	e.WriteTo(os.Stdout)
}

func Example_incorrectUsage() {
	// This example shows why a program *should* always initiate shutdown when
	// a panic is detected as this example will often (timing dependent) exit
	// due to a unhandled "all goroutines are asleep - deadlock!" panic.
	//
	// The point here is: once an unhandled panic occurs the program is in a
	// unreasonable state and since that state cannot be reasoned about you
	// *should* immediately begin termination. Put differently, if you had
	// the foresight to understand the state that triggered the panic you
	// would have prevented that panic from occurring in the first place.
	//
	// The issue here is trivial and could be solved by deferring the call to
	// mutex.Unlock, but the point stands.

	panics.PrintStackTrace(false) // quiet output

	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)

	var (
		wg sync.WaitGroup
		mu sync.Mutex
		n  int
	)
	for i := 0; i < 4; i++ {
		i := i
		panics.GoWG(&wg, func() {
			mu.Lock()
			n++
			if i == 0 {
				panic("abort")
			}
			mu.Unlock()
			wg.Done()
		})
	}
	// The call to Wait() will often panic with due to deadlock because once
	// the first goroutine panics the remaining goroutines will not be able
	// to acquire the lock and thus deadlock.
	wg.Wait()
}

func Example_correctUsage() {
	// This example shows the correct usage of the panics package and how to
	// handle unhandled panics generally - and that is to immediately initiate
	// program termination.

	// Disable for the example since it polutes the output.
	panics.PrintStackTrace(false)

	// The context created by panics.NotifyContext should be the parent of
	// all subsequent contexts used by the program so that all subsequent
	// contexts are marked done when a panic occurs.
	parent, cancel := panics.NotifyContext(context.Background())
	defer cancel()

	// spin up some "worker" goroutines
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		panics.Go(func() {
			defer wg.Done()
			// Create a context that is a child of parent - much like a normal
			// program would when creating contexts for network requests, or
			// whatnot.
			ctx, cancel := context.WithTimeout(parent, time.Second)
			defer cancel()

			fmt.Printf("%d: start\n", i)
			<-ctx.Done()
			fmt.Printf("%d: stopping\n", i)
		})
	}

	// Panic to cancel all notified contexts
	panics.Go(func() { panic("abort") })

	wg.Wait()

	// Handle the printing of the panic and its stack trace before exit.
	if err := parent.Err(); err != nil {
		fmt.Println("captured panic:", panics.ContextError(parent).Value())
	} else {
		fmt.Println("error: failed to cancel context")
	}

	// Unordered output:
	// 0: start
	// 0: stopping
	// 1: start
	// 1: stopping
	// 2: start
	// 2: stopping
	// 3: start
	// 3: stopping
	// captured panic: abort
}

func Example_signalHandling() {
	// This example is essentially the same as the "CorrectUsage" example,
	// but shows how a notified context can and probably should be used
	// with signal handling.

	panics.PrintStackTrace(false)

	// The context created by panics.NotifyContext should be the parent of
	// all subsequent contexts used by the program. If signal handling is
	// used then this context should be canceled when a terminating signal
	// is received.
	ctx, cancel := panics.NotifyContext(context.Background())
	defer cancel()

	// If the Interrupt signal is received then cancel the parent context
	// and restore the default signal handling behavior so that a subsequent
	// Interrupt signal terminates the program.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
		cancel()        // Cancel all contexts
		signal.Stop(ch) // Restore the default behavior
	}()

	// spin up some "worker" goroutines
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		panics.Go(func() {
			defer wg.Done()
			fmt.Printf("%d: start\n", i)
			<-ctx.Done()
			fmt.Printf("%d: stopping\n", i)
		})
	}

	// Panic to cancel all notified contexts
	panics.Go(func() { panic("abort") })

	wg.Wait()

	// Unordered output:
	// 0: start
	// 0: stopping
	// 1: start
	// 1: stopping
	// 2: start
	// 2: stopping
	// 3: start
	// 3: stopping
}

func Example_signalHandlingContext() {
	// This example is essentially the same as the "CorrectUsage" example,
	// but shows how a notified context can and probably should be used
	// with signal handling.

	panics.PrintStackTrace(false)

	// The context created by panics.NotifyContext should be the parent of
	// all subsequent contexts used by the program. If signal handling is
	// used then this context should be canceled when a terminating signal
	// is received.
	parent, cancel := panics.NotifyContext(context.Background())
	defer cancel()

	// ctx is marked done either when a panic is captured, the os.Interrupt
	// signal is received, or cancel is called, whichever happens first.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()

	go func() {
		<-ctx.Done()
		// ctx was likely canceled due to a signal and not a panic so restore
		// the default signal handling behavior.
		if panics.First() == nil {
			stop()
		}
	}()

	// spin up some "worker" goroutines
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		panics.Go(func() {
			defer wg.Done()
			fmt.Printf("%d: start\n", i)
			<-ctx.Done()
			fmt.Printf("%d: stopping\n", i)
		})
	}

	// Panic to cancel all notified contexts
	panics.Go(func() { panic("abort") })

	wg.Wait()

	// Unordered output:
	// 0: start
	// 0: stopping
	// 1: start
	// 1: stopping
	// 2: start
	// 2: stopping
	// 3: start
	// 3: stopping
}
