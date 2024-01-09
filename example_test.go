package panics_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/charlievieth/panics"
)

func ExampleFirst() {
	panics.SetPrintStackTrace(false)

	panics.Capture(func() { panic("my panic") })
	fmt.Println(panics.First().Value())

	// Output:
	// my panic
}

func ExampleNotify() {
	panics.SetPrintStackTrace(false)

	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)

	panics.Capture(func() { panic("my panic") })
	e := <-ch
	fmt.Println(e.Value())

	// Output:
	// my panic
}

func ExampleNotifyContext() {
	panics.SetPrintStackTrace(false)

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
	panics.SetPrintStackTrace(false) // quiet output

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
	panics.SetPrintStackTrace(false)

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
	panics.SetPrintStackTrace(false)

	ch := make(chan *panics.Error, 1)
	panics.Notify(ch)
	panics.Capture(func() { panic("here") })

	e := <-ch
	e.WriteTo(os.Stdout)
}
