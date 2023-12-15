//go:build go1.21
// +build go1.21

package panics

import (
	"context"
	"testing"
)

func TestNotifyContextWithoutCancel(t *testing.T) {
	testSetup(t)

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
}
