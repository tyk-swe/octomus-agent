package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestUsageReportCountsEverySavedTaskAndOnlyLedgerAdmissions(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDB := filepath.Join(dataDir, stateDBName)
	// Open creates the current version-7 schema, including the admission ledger.
	s, err := store.Open(stateDB)
	if err != nil {
		t.Fatal(err)
	}
	completed := "2026-09-12T01:00:00Z"
	cycle := model.Cycle{
		ID: "cycle-usage", Number: 1, Status: model.CycleCompleted,
		StartedAt: "2026-09-12T00:00:00Z", CompletedAt: &completed,
		Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: "fixture/project",
	}
	if err := s.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	type taskCase struct {
		id     string
		tier   string
		status model.Status
		ledger uint64
	}
	tasks := []taskCase{
		{"queued-xs", "XS", model.StatusQueued, 0},
		{"queued-s", "S", model.StatusQueued, 0},
		{"executing-s", "S", model.StatusExecuting, 2},
		{"published-s", "S", model.StatusPublished, 1},
		{"blocked-m", "M", model.StatusBlocked, 1},
		{"failed-l", "L", model.StatusFailed, 0},
		{"cancelled-xl", "XL", model.StatusCancelled, 0},
	}
	cfg := config.Default()
	for _, tc := range tasks {
		task := model.Task{
			ID: tc.id, CycleID: cycle.ID,
			Proposal: model.Proposal{ID: tc.id, Title: tc.id, Tier: tc.tier},
			Status:   tc.status, Route: cfg.Tiers[tc.tier], Config: cfg,
			Sessions: []model.Session{}, Reviews: []model.ReviewRound{},
			Verification: []model.Verification{}, CreatedAt: cycle.StartedAt, UpdatedAt: completed,
		}
		if err := s.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
		for i := uint64(0); i < tc.ledger; i++ {
			if err := s.ReserveSession(0, store.NewAdmission(cycle.ID, &task.ID, "executor", task.Route)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.ReserveSession(0, store.NewAdmission(cycle.ID, nil, "discovery", cfg.Roles["discovery"])); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(stateDB)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--data-dir", dataDir, "--usage-report"}, func(string) (string, bool) { return "", false }, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("usage report: code=%d stderr=%q", code, stderr.String())
	}
	var report struct {
		SchemaVersion      uint32 `json:"schema_version"`
		HasAdmissionLedger bool   `json:"has_admission_ledger"`
		Tasks              []struct {
			ID         string       `json:"id"`
			Tier       string       `json:"tier"`
			Status     model.Status `json:"status"`
			Admissions uint64       `json:"admissions"`
		} `json:"tasks"`
		Tiers []struct {
			Tier          string `json:"tier"`
			ObservedTasks int    `json:"observed_tasks"`
			Admissions    uint64 `json:"admissions"`
		} `json:"tiers"`
		Admissions []struct {
			TaskID *string `json:"task_id"`
		} `json:"admissions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || !report.HasAdmissionLedger || len(report.Tasks) != len(tasks) || len(report.Admissions) != 5 {
		t.Fatalf("unexpected report shape or inventory: %s", stdout.String())
	}
	ledgerByTask := map[string]uint64{}
	planning := 0
	for _, admission := range report.Admissions {
		if admission.TaskID == nil {
			planning++
		} else {
			ledgerByTask[*admission.TaskID]++
		}
	}
	if planning != 1 {
		t.Fatalf("planning admissions = %d, want 1", planning)
	}
	expected := map[string]taskCase{}
	for _, tc := range tasks {
		expected[tc.id] = tc
	}
	type totals struct {
		observed   int
		admissions uint64
	}
	byTier := map[string]totals{}
	seen := map[string]bool{}
	for _, row := range report.Tasks {
		tc, ok := expected[row.ID]
		if !ok || seen[row.ID] || row.Tier != tc.tier || row.Status != tc.status || row.Admissions != tc.ledger || ledgerByTask[row.ID] != tc.ledger {
			t.Fatalf("unexpected task row or admission ledger for %q: %+v", row.ID, row)
		}
		seen[row.ID] = true
		total := byTier[row.Tier]
		total.observed++
		total.admissions += ledgerByTask[row.ID]
		byTier[row.Tier] = total
	}
	if len(report.Tiers) != len(config.Tiers()) {
		t.Fatalf("tier rows = %d, want %d", len(report.Tiers), len(config.Tiers()))
	}
	seenTiers := map[string]bool{}
	for _, row := range report.Tiers {
		if seenTiers[row.Tier] || row.ObservedTasks != byTier[row.Tier].observed || row.Admissions != byTier[row.Tier].admissions {
			t.Fatalf("tier %q does not match saved task rows and admission ledger: %+v, want %+v", row.Tier, row, byTier[row.Tier])
		}
		seenTiers[row.Tier] = true
	}
	for _, tier := range config.Tiers() {
		if !seenTiers[tier] {
			t.Fatalf("missing tier %q", tier)
		}
	}
	after, err := os.ReadFile(stateDB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("usage report changed the state database")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "service.lock")); !os.IsNotExist(err) {
		t.Fatalf("usage report took the service lock: %v", err)
	}
}
