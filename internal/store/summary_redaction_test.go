package store_test

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

const summaryToken = "ghp_abcdefghijklmnopqrstuvwxyz1234567890"

func boundaryText(limit int, value string) string {
	return strings.Repeat("界", limit-11) + " " + value + " trailing text"
}

func assertSummaryText(t *testing.T, text, fragment string, limit int) {
	t.Helper()
	if strings.Contains(text, fragment) || !strings.Contains(text, "[redacted]") || utf8.RuneCountInString(text) > limit {
		t.Fatalf("summary did not scrub before its %d-character bound: %q", limit, text)
	}
}

func TestReviewPRSummaryDoesNotExposeCutToken(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	pr := model.PRObservation{PR: model.PullRequest{Number: 42, State: "open", Title: boundaryText(200, summaryToken), Body: "full private PR body"}}
	must(t, s.Put("pr", "42", pr))
	task := task()
	task.Proposal.Title = boundaryText(200, summaryToken)
	task.Error = str(boundaryText(512, summaryToken))
	must(t, s.Put("task", task.ID, task))
	cycle := cycleFor(task)
	cycle.Error = task.Error
	must(t, s.Put("cycle", cycle.ID, cycle))
	for _, kind := range []string{"pr", "task", "cycle"} {
		page, err := s.HistoryPage(kind, store.HistoryQuery{})
		must(t, err)
		if len(page.Items) != 1 {
			t.Fatalf("%s history has %d records", kind, len(page.Items))
		}
		fields := decodeMap(t, page.Items[0])
		if kind == "pr" {
			fields = fields["pr"].(map[string]any)
			if _, found := fields["body"]; found {
				t.Fatal("PR summary exposed the body")
			}
		}
		if title, ok := fields["title"].(string); ok {
			assertSummaryText(t, title, "ghp_abcdef", 200)
		}
		if message, ok := fields["error"].(string); ok {
			assertSummaryText(t, message, "ghp_abcdef", 512)
		}
	}
	dashboard, err := s.Dashboard()
	must(t, err)
	for _, raw := range append(append(dashboard.Tasks, dashboard.Cycles...), dashboard.PRs...) {
		if strings.Contains(string(raw), "ghp_abcdef") {
			t.Fatal("dashboard bypassed the safe summary projection")
		}
	}
	saved, err := store.Get[model.PRObservation](s, "pr", "42")
	must(t, err)
	if saved.PR.Title != pr.PR.Title || saved.PR.Body != pr.PR.Body {
		t.Fatal("presentation redaction modified the raw PR observation")
	}
}

func TestSummaryMigrationRestoresCompleteText(t *testing.T) {
	t.Parallel()
	path := testutil.GoldenState(t, "0.2.0", filepath.Join(t.TempDir(), "state.db"))
	db := raw(t, path)
	if queryInt(t, db, "PRAGMA user_version") != 8 {
		t.Fatal("historical v0.2.0 fixture no longer describes schema 8")
	}
	step, err := os.ReadFile("migrations/009-decision-identity.sql")
	must(t, err)
	exec(t, db, string(step))
	exec(t, db, "PRAGMA user_version=9")
	// Exercise the historical triggers, then upgrade the exact predecessor
	// schema. Migration must repair only the private summary projections.
	exec(t, db, "UPDATE records SET data=json_set(data,'$.pr.title',?1) WHERE kind='pr'", boundaryText(200, summaryToken))
	exec(t, db, "UPDATE records SET data=json_set(data,'$.proposal.title',?1,'$.error',?2) WHERE kind='task'", boundaryText(200, summaryToken), boundaryText(512, summaryToken))
	exec(t, db, "UPDATE records SET data=json_set(data,'$.error',?1) WHERE kind='cycle'", boundaryText(512, summaryToken))
	before := recordDump(t, db)
	notifications := queryInt(t, db, "SELECT count(*) FROM notification_outbox")
	s := open(t, path)
	if s.Upgraded() == nil || s.Upgraded().From != 9 || s.Upgraded().To != store.ReleaseVersion() || len(backups(t, path)) != 1 {
		t.Fatalf("missing verified predecessor upgrade: %+v", s.Upgraded())
	}
	if diff, same := equalStrings(before, recordDump(t, db)); !same {
		t.Fatalf("summary migration changed raw records: %s", diff)
	}
	if queryInt(t, db, "SELECT count(*) FROM notification_outbox") != notifications {
		t.Fatal("summary migration replayed notification triggers")
	}
	for _, kind := range []string{"pr", "task", "cycle"} {
		page, err := s.HistoryPage(kind, store.HistoryQuery{})
		must(t, err)
		if len(page.Items) == 0 {
			t.Fatalf("golden has no %s records", kind)
		}
		for _, item := range page.Items {
			if strings.Contains(string(item), "ghp_abcdef") || !strings.Contains(string(item), "[redacted]") {
				t.Fatalf("%s projection was not rebuilt from whole text: %s", kind, item)
			}
		}
	}
	// Future writes must use the repaired triggers as well.
	pr := model.PRObservation{PR: model.PullRequest{Number: 999, Title: boundaryText(200, summaryToken), State: "open"}}
	must(t, s.Put("pr", "999", pr))
	pr.PR.Title += " updated"
	must(t, s.Put("pr", "999", pr))
	var retained string
	must(t, db.QueryRow("SELECT json_extract(summary,'$.pr.title') FROM record_meta WHERE kind='pr' AND id='999'").Scan(&retained))
	if retained != pr.PR.Title {
		t.Fatal("upgraded insert/update triggers still cut private source fields")
	}
}

func TestSummaryUsesReadTimeEnvironment(t *testing.T) {
	const secret = "rotated-custom-credential-7c9412ab"
	const probePath = "OCTOMUS_SUMMARY_PROBE_PATH"
	if path := os.Getenv(probePath); path != "" {
		s := open(t, path)
		page, err := s.HistoryPage("pr", store.HistoryQuery{})
		must(t, err)
		title := decodeMap(t, page.Items[0])["pr"].(map[string]any)["title"].(string)
		assertSummaryText(t, title, "rotated-cu", 200)
		return
	}
	path := statePath(t)
	s := open(t, path)
	pr := model.PRObservation{PR: model.PullRequest{Number: 42, State: "open", Title: boundaryText(200, secret)}}
	must(t, s.Put("pr", "42", pr))
	must(t, s.Close())
	// The writer did not know this value was a secret. A new process knows it
	// at read time; persisted redaction or a saved prefix would both be wrong.
	cmd := osexec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSummaryUsesReadTimeEnvironment$", "-test.count=1")
	cmd.Env = append(os.Environ(), probePath+"="+path, "OCTOMUS_REVIEW_SECRET="+secret)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("read with replacement environment: %v\n%s", err, output)
	}
}
