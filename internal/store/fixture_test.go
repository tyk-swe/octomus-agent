package store_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/jsoncompat"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// task mirrors tests/common/mod.rs: a queued task with a grounded accepted
// proposal against fixture/project.
func task() model.Task {
	c := config.Default()
	c.GitHubRepo = "fixture/project"
	return model.Task{
		ID:      model.ID(),
		CycleID: "cycle",
		Proposal: model.Proposal{
			ID:            "a",
			Title:         "Concrete improvement",
			Problem:       "Missing behavior",
			Benefit:       "Useful behavior",
			Scope:         "one file",
			Evidence:      []string{"README.md"},
			Category:      "features",
			Target:        "main",
			Tier:          "M",
			Dependencies:  []string{},
			Prompt:        "Implement the documented behavior",
			Decision:      model.DecisionAccepted,
			Reason:        "Grounded",
			ProblemKey:    "",
			RelevantPaths: []string{},
			Reconsiders:   []string{},
		},
		Status:          model.StatusQueued,
		Route:           config.NewRoute("fixture", "low"),
		Config:          c,
		SourceRevision:  "source",
		ComparisonBase:  "source",
		DefaultRevision: "source",
		Branch:          "octomus/work",
		Workspace:       "",
		Sessions:        []model.Session{},
		Reviews:         []model.ReviewRound{},
		Verification:    []model.Verification{},
		CreatedAt:       model.Now(),
		UpdatedAt:       model.Now(),
		SupersededBy:    []string{},
		Supersedes:      []string{},
	}
}

// reviewTask mirrors the tests/review_regressions.rs fixture: a stable problem key
// and mixed-case repository.
func reviewTask() model.Task {
	t := task()
	t.CycleID = "original-cycle"
	t.Proposal.ProblemKey = "stable-problem"
	t.Config.GitHubRepo = "Fixture/Project"
	return t
}

// cycleFor mirrors tests/review_regressions.rs: a running cycle carrying the
// task's proposal in its repository.
func cycleFor(t model.Task) model.Cycle {
	return model.Cycle{
		Mode:        model.CycleModeExecution,
		ID:          model.ID(),
		Number:      1,
		Status:      model.CycleRunning,
		StartedAt:   model.Now(),
		Proposals:   []model.Proposal{t.Proposal},
		Assessments: []any{},
		Sessions:    []model.Session{},
		Repository:  t.Config.GitHubRepo,
	}
}

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.db")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// raw opens a second, plain connection for test-side inspection or legacy schema
// setup. It is closed with the test.
func raw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

// canonical returns the compact, key-sorted JSON of a value so tests compare
// records by content regardless of source type, keeping every number's
// spelling so 60.0 and 60 remain distinct the way serde_json emits them.
func canonical(t *testing.T, value any) string {
	t.Helper()
	data, err := jsoncompat.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	return canonical(t, a) == canonical(t, b)
}

// generic decodes a value through JSON into a generic map for field lookups.
func generic(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := jsoncompat.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func str(s string) *string { return &s }
