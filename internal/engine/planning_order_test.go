package engine

import (
	"context"
	"errors"
	"testing"
)

func TestPlanningOrderPreservesClaimsAcrossFailedPreflights(t *testing.T) {
	var order planningOrder
	first, second, failed, last := order.enqueue(), order.enqueue(), order.enqueue(), order.enqueue()
	defer first.release()
	defer second.release()
	defer last.release()
	// A later failed check does not let its successor jump ahead of either
	// earlier check, even if all of their HTTP work already finished.
	failed.release()
	select {
	case <-second.ready:
		t.Fatal("fast preflight overtook the first repository")
	case <-last.ready:
		t.Fatal("failed preflight let later work overtake the first repository")
	default:
	}
	first.release()
	if err := second.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-last.ready:
		t.Fatal("later work overtook the second repository")
	default:
	}
	second.release()
	if err := last.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPlanningOrderCancellationDoesNotBlockSuccessors(t *testing.T) {
	var order planningOrder
	first, cancelled, last := order.enqueue(), order.enqueue(), order.enqueue()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cancelled.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	cancelled.release()
	first.release()
	defer last.release()
	if err := last.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
