// bigstack helps generate large stack traces by having a very long import path.

package bigstack

import "errors"

var Error = errors.New("bigstack: test panic")

// Func returns func fn wrapped by n outer functions.
func Func(n int, fn func()) func() {
	if n < 1 {
		panic("non-positive size")
	}
	f := func() {
		fn()
	}
	for i := 0; i < n; i++ {
		ff := f
		f = func() {
			ff()
		}
	}
	return f
}

// Panic panics with a callstack at least n calls deep.
func Panic(n int) {
	Func(n, func() { panic(Error) })()
}
