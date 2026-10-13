package git

import (
	"context"
	"errors"
	"testing"
)

func TestCheckoutGateCancelledWriterWakesReaders(t *testing.T) {
	released := make(chan struct{})
	g := &checkoutGate{
		readers:  1,
		released: released,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := g.acquire(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled writer = %v; want context.Canceled", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("cancelled writer did not wake readers")
	}
}
