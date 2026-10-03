package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

func TestHostVerifyRunsBashPipefailInTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	out, _, err := sandbox.Verify(context.Background(), sandbox.Host{}, dir, "pwd; false | true", 30, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out.Stdout.Bytes)); got != dir {
		t.Fatalf("verification cwd = %q; want %q", got, dir)
	}
	if out.Status.Success() {
		t.Fatal("pipefail must fail a pipeline whose first command fails")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := sandbox.Verify(ctx, sandbox.Host{}, dir, "true", 30, true); !errors.Is(err, process.ErrCancelled) {
		t.Fatalf("cancelled verification = %v; want ErrCancelled", err)
	}
}
