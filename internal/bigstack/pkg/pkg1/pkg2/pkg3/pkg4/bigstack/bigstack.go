// bigstack helps generate large stack traces by having a very long import path.

package bigstack

import "errors"

var Error = errors.New("bigstack: test panic")

func Panic(n int) {
	if n < 1 {
		panic("non-positive size")
	}
	f := func() {
		panic(Error)
	}
	for i := 0; i < n; i++ {
		fn := f
		f = func() {
			fn()
		}
	}
	f()
}
