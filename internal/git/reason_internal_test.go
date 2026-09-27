package git

import (
	"errors"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// TestReasonHelpersKeepMessageReasonAndCause pins the shape of typed
// publication refusals that task errors, the dashboard and notifications show:
// the detailed message first, then the reason sentence, then any cause; the
// reason classifies the refusal unless the cause carries a deeper one, and the
// cause stays reachable.
func TestReasonHelpersKeepMessageReasonAndCause(t *testing.T) {
	reason := model.BlockedReasonRemoteConflict
	refusal := blocked(reason, "Invalid PR creation URL")
	if want := "Invalid PR creation URL: " + reason.Error(); refusal.Error() != want {
		t.Fatalf("blocked text = %q; want %q", refusal.Error(), want)
	}
	if got := model.BlockedReasonFromError(refusal); got != reason {
		t.Fatalf("blocked reason = %v; want %v", got, reason)
	}
	if !errors.Is(refusal, reason) {
		t.Fatal("blocked must wrap its reason")
	}

	cause := errors.New("invalid digit")
	wrapped := reasoned(reason, "Missing created PR number", cause)
	if want := "Missing created PR number: " + reason.Error() + ": invalid digit"; wrapped.Error() != want {
		t.Fatalf("reasoned text = %q; want %q", wrapped.Error(), want)
	}
	if got := model.BlockedReasonFromError(wrapped); got != reason {
		t.Fatalf("reasoned reason = %v; want %v", got, reason)
	}
	if !errors.Is(wrapped, reason) || !errors.Is(wrapped, cause) {
		t.Fatal("reasoned must wrap both its reason and its cause")
	}

	// A cause that is itself a typed refusal is the deeper, more specific
	// classification.
	nested := reasoned(reason, "Outer", blocked(model.BlockedReasonStaleBase, "Inner"))
	if got := model.BlockedReasonFromError(nested); got != model.BlockedReasonStaleBase {
		t.Fatalf("nested reason = %v; want the cause's %v", got, model.BlockedReasonStaleBase)
	}
}
