//go:build go1.21
// +build go1.21

package panics

import (
	"context"
	"testing"
)

func TestNotifyContextWithoutCancel(t *testing.T) {
	testSetup(t)

	t.Run("", func(t *testing.T) {
		parent, cancel1 := NotifyContext(context.Background())
		defer cancel1()

		// If the parent is already registed we don't register the child
		// (since the parent will cancel it), WithoutCancel breaks this
		// so test that we handle it.
		ctx, cancel2 := NotifyContext(context.WithoutCancel(parent))
		defer cancel2()

		Capture(func() { panic(testErr) })
		if !contextCancelled(ctx) {
			t.Error("failed to notify child context created with context.WithoutCancel")
		}
		if !contextCancelled(parent) {
			t.Fatal("failed to notify parent context")
		}
	})

	t.Run("", func(t *testing.T) {
		grandparent, cancel1 := NotifyContext(context.Background())
		defer cancel1()

		// Use WithoutCancel to break connection between grandparent/parent,
		// but make sure parent.Done() returns a non-nil channel.
		parent, cancel2 := context.WithCancel(context.WithoutCancel(grandparent))
		defer cancel2()

		ctx, cancel3 := NotifyContext(parent)
		defer cancel3()

		cancel1()
		if contextCancelled(ctx) {
			t.Fatal("grandparent canceled ctx")
		}

		Capture(func() { panic(testErr) })
		if !contextCancelled(ctx) {
			t.Error("failed to notify child context created with context.WithoutCancel")
		}
	})
}
