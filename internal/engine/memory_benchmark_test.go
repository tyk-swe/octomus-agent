package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// BenchmarkPlanningMemory measures the complete refresh, including SQLite decoding
// and real Git processes. The history changes an unrelated file after recording
// decisions, as happens when later cycles revisit the same parts of a repository.
func BenchmarkPlanningMemory(b *testing.B) {
	// TestMain installs a fixture dispatcher; use the real binary directly here
	// so its shell startup does not inflate the production refresh cost.
	b.Setenv("PATH", "/usr/bin:/bin")
	for _, test := range []struct {
		name          string
		count, groups int
	}{
		{"20_records_5_path_sets", 20, 5},
		{"100_records_10_path_sets", 100, 10},
		{"100_records_100_path_sets", 100, 100},
		{"100_pathless_records", 100, 0},
	} {
		b.Run(test.name, func(b *testing.B) {
			root := b.TempDir()
			repo := filepath.Join(root, "repo")
			if err := os.MkdirAll(repo, 0o755); err != nil {
				b.Fatal(err)
			}
			git := func(args ...string) string {
				b.Helper()
				output, err := gitCommand(repo, args...).CombinedOutput()
				if err != nil {
					b.Fatalf("git %v: %v: %s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git("init", "-b", "main")
			git("config", "user.name", "Fixture")
			git("config", "user.email", "fixture@example.test")
			for i := range 100 {
				dir := filepath.Join(repo, fmt.Sprintf("package-%03d", i))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					b.Fatal(err)
				}
				for _, name := range []string{"source.go", "README.md"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("fixture %d %s\n", i, name)), 0o644); err != nil {
						b.Fatal(err)
					}
				}
				if i%10 == 9 {
					git("add", ".")
					git("commit", "-qm", fmt.Sprintf("Add packages through %d", i))
				}
			}
			recorded := git("rev-parse", "HEAD")
			cfg := testConfig(repo)
			state, err := store.Open(filepath.Join(root, "state.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = state.Close() })
			ctx := context.Background()
			for i := range test.count {
				paths := []string{}
				if test.groups > 0 {
					prefix := fmt.Sprintf("package-%03d/", i%test.groups)
					paths = []string{prefix + "source.go", prefix + "README.md"}
				}
				fingerprint, err := decisionFingerprint(ctx, cfg, recorded, paths)
				if err != nil {
					b.Fatal(err)
				}
				record := decisionRecord{
					ID: fmt.Sprintf("decision-%03d", i), CycleMode: model.CycleModeExecution,
					Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch,
					ProblemKey: fmt.Sprintf("problem-%03d", i), RelevantPaths: paths,
					Decision: model.DecisionRejected, Reason: "No measured benefit",
					SourceRevision: recorded, ContextFingerprint: fingerprint,
					ReconsiderAfter: "2999-01-01T00:00:00Z", CycleID: fmt.Sprintf("cycle-%03d", i/10),
				}
				if err := state.Put("decision", record.ID, durableDecisionMap(record)); err != nil {
					b.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("New cycle\n"), 0o644); err != nil {
				b.Fatal(err)
			}
			git("add", "unrelated.txt")
			git("commit", "-qm", "Change unrelated content")
			grounding := model.Grounding{Revision: git("rev-parse", "HEAD")}
			app := New(state, root)
			b.Cleanup(app.Shutdown)
			b.ResetTimer()
			for b.Loop() {
				memory, err := app.planningMemory(ctx, cfg, grounding)
				if err != nil {
					b.Fatal(err)
				}
				if len(memory.decisions) != test.count {
					b.Fatalf("decisions = %d; want %d", len(memory.decisions), test.count)
				}
				for _, record := range memory.decisions {
					if record.ReconsiderationDue != (test.groups == 0) {
						b.Fatalf("decision %s reconsideration_due = %v", record.ID, record.ReconsiderationDue)
					}
				}
			}
		})
	}
}
