package git_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
)

func TestMaintenanceFootprintUsesCompleteTrustedDiff(t *testing.T) {
	t.Parallel()
	cfg, root := fixtureRoot(t)
	ctx := context.Background()
	source := realGit(t, cfg.Repository, "rev-parse", "main")
	work := filepath.Join(root, "data", "tasks", "footprint", "workspace")
	if err := gitops.CloneAt(ctx, cfg, work, source); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "old.txt"), "a\nb\n")
	base, err := gitops.Snapshot(ctx, cfg, work, "Set up footprint fixture")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "old.txt"), "a\nc\n")
	writeFile(t, filepath.Join(work, "new.txt"), "d\n")
	head, err := gitops.Snapshot(ctx, cfg, work, "Change footprint fixture")
	if err != nil {
		t.Fatal(err)
	}
	got, err := gitops.ReadMaintenanceFootprint(ctx, cfg, work, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.ChangedLines == nil || *got.ChangedLines != 3 ||
		got.ChangedFiles == nil || *got.ChangedFiles != 2 || len(got.ManualReasons) != 0 {
		t.Fatalf("trusted footprint = %+v; want 3 lines / 2 paths", got)
	}
	if got.ComparisonBase != base || got.Revision != head {
		t.Fatalf("trusted footprint lost revision binding: %+v", got)
	}
	writeFile(t, filepath.Join(work, ".gitattributes"), "* -diff\n")
	again, err := gitops.ReadMaintenanceFootprint(ctx, cfg, work, base, head)
	if err != nil || again.ChangedLines == nil || *again.ChangedLines != 3 {
		t.Fatalf("work-tree attributes changed frozen diff measurement: %+v, %v", again, err)
	}
	trapDir, trap := hostileTrap(t)
	if err := os.Remove(filepath.Join(work, ".git")); err != nil {
		t.Fatal(err)
	}
	plantHostileGitDir(t, work, trap)
	again, err = gitops.ReadMaintenanceFootprint(ctx, cfg, work, base, head)
	if err != nil || again.ChangedLines == nil || *again.ChangedLines != 3 ||
		again.ChangedFiles == nil || *again.ChangedFiles != 2 {
		t.Fatalf("hostile work-tree git changed trusted footprint: %+v, %v", again, err)
	}
	assertTrapUntouched(t, trapDir)
}

func TestMaintenanceFootprintCountsBothRenamePaths(t *testing.T) {
	t.Parallel()
	cfg, root := fixtureRoot(t)
	ctx := context.Background()
	source := realGit(t, cfg.Repository, "rev-parse", "main")
	work := filepath.Join(root, "data", "tasks", "rename-footprint", "workspace")
	if err := gitops.CloneAt(ctx, cfg, work, source); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "before.txt"), "unchanged\n")
	base, err := gitops.Snapshot(ctx, cfg, work, "Set up rename fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(work, "before.txt"), filepath.Join(work, "after.txt")); err != nil {
		t.Fatal(err)
	}
	head, err := gitops.Snapshot(ctx, cfg, work, "Rename fixture file")
	if err != nil {
		t.Fatal(err)
	}
	got, err := gitops.ReadMaintenanceFootprint(ctx, cfg, work, base, head)
	if err != nil || got.ChangedLines == nil || *got.ChangedLines != 2 ||
		got.ChangedFiles == nil || *got.ChangedFiles != 2 || len(got.ManualReasons) != 0 {
		t.Fatalf("rename footprint = %+v, %v; want 2 lines / 2 paths", got, err)
	}
}

func TestMaintenanceFootprintIncludesEarlierPRChanges(t *testing.T) {
	t.Parallel()
	cfg, root := fixtureRoot(t)
	ctx := context.Background()
	base := realGit(t, cfg.Repository, "rev-parse", "main")
	work := filepath.Join(root, "data", "tasks", "accumulated-footprint", "workspace")
	if err := gitops.CloneAt(ctx, cfg, work, base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "earlier.txt"), strings.Repeat("existing maintenance\n", 600))
	source, err := gitops.Snapshot(ctx, cfg, work, "Earlier maintenance")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "followup.txt"), "one-line follow-up\n")
	head, err := gitops.Snapshot(ctx, cfg, work, "Small maintenance follow-up")
	if err != nil {
		t.Fatal(err)
	}
	full, err := gitops.ReadMaintenanceFootprint(ctx, cfg, work, base, head)
	if err != nil || full.ChangedLines == nil || *full.ChangedLines != 601 ||
		full.ChangedFiles == nil || *full.ChangedFiles != 2 {
		t.Fatalf("full PR footprint = %+v, %v; want 601 lines / 2 paths", full, err)
	}
	incremental, err := gitops.ReadMaintenanceFootprint(ctx, cfg, work, source, head)
	if err != nil || incremental.ChangedLines == nil || *incremental.ChangedLines != 1 ||
		incremental.ChangedFiles == nil || *incremental.ChangedFiles != 1 {
		t.Fatalf("incremental fixture = %+v, %v; want 1 line / 1 path", incremental, err)
	}
}

func TestMaintenanceFootprintDoesNotCountBinaryAsZero(t *testing.T) {
	t.Parallel()
	cfg, root := fixtureRoot(t)
	ctx := context.Background()
	base := realGit(t, cfg.Repository, "rev-parse", "main")
	work := filepath.Join(root, "data", "tasks", "binary-footprint", "workspace")
	if err := gitops.CloneAt(ctx, cfg, work, base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "data.bin"), "one\x00two\n")
	head, err := gitops.Snapshot(ctx, cfg, work, "Binary maintenance fixture")
	if err != nil {
		t.Fatal(err)
	}
	got, err := gitops.ReadMaintenanceFootprint(ctx, cfg, work, base, head)
	if err != nil || !got.Complete || got.ChangedLines != nil ||
		got.ChangedFiles == nil || *got.ChangedFiles != 1 || len(got.ManualReasons) == 0 {
		t.Fatalf("binary became small zero-line authority: %+v, %v", got, err)
	}
}
