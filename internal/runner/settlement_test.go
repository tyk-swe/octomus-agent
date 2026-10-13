package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// Only ownership settlement is exercised: protocol methods belong to the adapter-specific behavior tests.
type settlementAdapter struct {
	Adapter
	err    error
	closed int
	record *model.SandboxRecord
}

func (a *settlementAdapter) Close() error                          { a.closed++; return a.err }
func (a *settlementAdapter) SandboxEvidence() *model.SandboxRecord { return a.record }

func TestRootReplacementSettlesEvidenceBeforeStartingAnotherChild(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unconfirmed"}[fails], func(t *testing.T) {
			old := &settlementAdapter{record: &model.SandboxRecord{ImageID: "sha256:old-root", Runs: 1}}
			if fails {
				old.err = &sandbox.Error{Err: errors.New("unconfirmed child end"), Started: true}
			}
			connections := 0
			clients := New(context.Background(), config.Default(), func(context.Context, config.Backend, config.Config, string) (Adapter, error) {
				connections++
				if connections == 1 {
					return old, nil
				}
				return &settlementAdapter{}, nil
			})
			defer clients.Close()
			if _, err := clients.Client(config.BackendCodex, "old-root"); err != nil {
				t.Fatal(err)
			}
			_, err := clients.Client(config.BackendCodex, "new-root")
			if errors.Is(err, model.BlockedRunnerUnavailable) != fails || old.closed != 1 {
				t.Fatalf("replacement settlement = %v; closed=%d", err, old.closed)
			}
			if fails && connections != 1 {
				t.Fatal("new child started before the old ownership settled")
			}
			if record := clients.TakeEvidence(); record == nil || record.ImageID != "sha256:old-root" || record.Runs != 1 {
				t.Fatalf("replacement lost the released child's evidence: %+v", record)
			}
			if clients.TakeEvidence() != nil {
				t.Fatal("released evidence was accounted twice")
			}
		})
	}
}

// A child that does not end within the cleanup budget did not end in a way Octomus can confirm, so it is the
// sandbox's failure and never the program's result. Its owner must then refuse replacement work and report a runner
// failure, keeping whatever the released child's sandbox recorded.
func TestUnsettledChildEndIsASandboxFailure(t *testing.T) {
	t.Parallel()
	stuck := joinOwnedWithin(50*time.Millisecond, make(chan error, 1), make(chan struct{}), "Codex app-server did not exit during cleanup")
	if !sandbox.Infrastructure(stuck) || !strings.Contains(stuck.Error(), "did not exit during cleanup") {
		t.Fatalf("unsettled child end = %v; want the sandbox's own failure", stuck)
	}
	old := &settlementAdapter{err: stuck, record: &model.SandboxRecord{ImageID: "sha256:stuck-root", Runs: 1}}
	connections := 0
	clients := New(context.Background(), config.Default(), func(context.Context, config.Backend, config.Config, string) (Adapter, error) {
		connections++
		if connections == 1 {
			return old, nil
		}
		return &settlementAdapter{}, nil
	})
	defer clients.Close()
	if _, err := clients.Client(config.BackendCodex, "old-root"); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Client(config.BackendCodex, "new-root"); !errors.Is(err, model.BlockedRunnerUnavailable) {
		t.Fatalf("replacement after an unsettled child end = %v; want a runner failure", err)
	}
	if connections != 1 {
		t.Fatal("new child started before the old ownership settled")
	}
	if record := clients.TakeEvidence(); record == nil || record.ImageID != "sha256:stuck-root" || record.Runs != 1 {
		t.Fatalf("unsettled child lost its evidence: %+v", record)
	}
}
