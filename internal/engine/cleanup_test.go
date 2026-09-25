package engine

// Cleanup ownership regressions: managed-directory removal runs with the
// scheduler gate released behind an exclusive in-memory (kind, id) claim, so
// unrelated controls stay responsive while conflicting workspace users get an
// explicit conflict. The injected removal seam makes the
// admission → hold → release ordering deterministic; bounded waits are
// deadlock detectors for controls that must complete, not timing
// measurements. None of this is production latency evidence.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const cleanupOldTimestamp = "2020-01-01T00:00:00Z"

// removalBarrier is the WithWorkspaceRemoval seam used as a deterministic
// hold: removing the blocked path signals entered, then finishes through the
// real managed-directory checks once Release is called. Other paths delegate
// immediately.
type removalBarrier struct {
	blocked     string
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newRemovalBarrier(t *testing.T, blocked string) *removalBarrier {
	t.Helper()
	b := &removalBarrier{blocked: filepath.Clean(blocked), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(b.Release)
	return b
}

func (b *removalBarrier) remove(root, path string) error {
	if path == b.blocked {
		b.enterOnce.Do(func() { close(b.entered) })
		<-b.release
	}
	return workspace.RemoveOwnedDir(root, path)
}

// wait confirms removal of the blocked path was admitted and is in flight.
func (b *removalBarrier) wait(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("cleanup did not reach the held removal")
	}
}

// Release lets the held removal finish; idempotent so cleanup paths can call
// it unconditionally.
func (b *removalBarrier) Release() {
	b.releaseOnce.Do(func() { close(b.release) })
}

// completesDuring runs an operation while a removal is held at the barrier
// and returns its result. The bounded wait fails on the pre-fix behavior,
// where removal held the scheduler gate and this call could never finish.
func completesDuring(t *testing.T, label string, run func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s blocked behind workspace cleanup", label)
		return nil
	}
}

// discardableTask builds an archived, terminal task with an owned workspace
// directory under dataDir/tasks/<id> — eligible for explicit discard and for
// retention cleanup.
func discardableTask(t *testing.T, cfg config.Config, dataDir, id string) model.Task {
	t.Helper()
	task := queuedTask(cfg, id, cfg.DefaultBranch, cfg.BranchPrefix+id)
	task.Status = model.StatusBlocked
	task.UpdatedAt = cleanupOldTimestamp
	task.Lifecycle.ArchivedAt = stringPointer(cleanupOldTimestamp)
	task.Workspace = filepath.Join(dataDir, "tasks", id, "workspace")
	if err := os.MkdirAll(task.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.Workspace, "evidence.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	return task
}

// discardableCycle builds a completed, archived cycle with an owned planning
// directory under dataDir/cycles/<id>.
func discardableCycle(t *testing.T, dataDir string) model.Cycle {
	t.Helper()
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: model.ID(), Number: 1, Status: model.CycleCompleted,
		StartedAt: cleanupOldTimestamp, CompletedAt: stringPointer(cleanupOldTimestamp),
		Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: "fixture/project",
	}
	cycle.Lifecycle.ArchivedAt = stringPointer(cleanupOldTimestamp)
	dir := filepath.Join(dataDir, "cycles", cycle.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "planning.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cycle
}

// discardableBaseline builds a terminal check with an owned clone under
// dataDir/baselines/<id>.
func discardableBaseline(t *testing.T, cfg config.Config, dataDir string) model.BaselineCheck {
	t.Helper()
	check := makeCheck(cfg, model.BaselineStatusFailed)
	check.CompletedAt = stringPointer(model.Now())
	dir := filepath.Join(dataDir, "baselines", check.ID, "workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "artifact"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	return check
}

func cleanupEvents(t *testing.T, state *store.Store, id string) []model.Event {
	t.Helper()
	events, err := state.Events(&id)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := []model.Event{}
	for _, event := range events {
		if event.Kind == "cleanup_error" {
			cleanup = append(cleanup, event)
		}
	}
	return cleanup
}

// The ticket's core regression: an operator discard admitted and held inside
// managed removal must not hold the scheduler gate. Pause completes through
// the operator boundary, the claimed task conflicts every action including a
// second discard, an unrelated discard proceeds independently, and a
// concurrent lifecycle write survives finalization. On the pre-fix
// gate-holding removal the pause wait deadlocks and fails.
func TestDiscardReleasesTheGateAndClaimsTheWorkspace(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	task := discardableTask(t, cfg, dataDir, "held-task")
	other := discardableTask(t, cfg, dataDir, "other-task")
	for _, seeded := range []model.Task{task, other} {
		if err := state.Put("task", seeded.ID, seeded); err != nil {
			t.Fatal(err)
		}
	}
	barrier := newRemovalBarrier(t, filepath.Join(dataDir, "tasks", task.ID))
	app := New(state, dataDir, WithWorkspaceRemoval(barrier.remove))
	t.Cleanup(app.Shutdown)

	discarded := make(chan error, 1)
	go func() { discarded <- app.TaskAction(context.Background(), task.ID, "discard") }()
	barrier.wait(t)

	if err := completesDuring(t, "pause", func() error {
		_, err := app.ControlAction("pause")
		return err
	}); err != nil {
		t.Fatalf("pause during held removal: %v", err)
	}
	// The claimed target conflicts every action — including a second
	// cleanup — without waiting on the held removal.
	for _, action := range []string{"discard", "archive", "cancel", "retry"} {
		if err := app.TaskAction(context.Background(), task.ID, action); err == nil || !IsActionConflict(err) {
			t.Fatalf("%s on a claimed task = %v; want a conflict", action, err)
		}
	}
	if err := app.TaskAction(context.Background(), other.ID, "discard"); err != nil {
		t.Fatalf("unrelated discard during held removal: %v", err)
	}
	// Unrelated lifecycle evidence landing mid-removal must survive
	// finalization: the mark is applied to the current durable record, not a
	// stale copy of it.
	current, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || current == nil {
		t.Fatalf("reload task: %v", err)
	}
	current.Error = stringPointer("late unrelated evidence")
	if err := state.Put("task", current.ID, *current); err != nil {
		t.Fatal(err)
	}

	barrier.Release()
	if err := <-discarded; err != nil {
		t.Fatalf("discard: %v", err)
	}
	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("discarded record: %+v, %v", saved, err)
	}
	if saved.Error == nil || *saved.Error != "late unrelated evidence" {
		t.Fatalf("finalization clobbered a concurrent write: %+v", saved.Error)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "tasks", task.ID)); !os.IsNotExist(err) {
		t.Fatalf("owned task directory still present: %v", err)
	}
	otherSaved, err := store.Get[model.Task](state, "task", other.ID)
	if err != nil || otherSaved == nil || otherSaved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("unrelated discard: %+v, %v", otherSaved, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "tasks", other.ID)); !os.IsNotExist(err) {
		t.Fatalf("unrelated task directory still present: %v", err)
	}
	if app.cleanupClaimed(cleanupTask, task.ID) || app.cleanupClaimed(cleanupTask, other.ID) {
		t.Fatal("cleanup claims leaked after completion")
	}
}

// Retention removes task, cycle and baseline workspaces through the same
// ownership contract: the gate is not held across deletion, a target claimed
// by the running cleanup conflicts a duplicate operator discard, a concurrent
// pass skips it silently, and every candidate still ends honestly marked.
func TestRetentionCleansUpOffTheGateAndSkipsClaimedTargets(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	cfg.RetainCompletedDays = 1
	saveSettings(t, state, cfg, model.DefaultControl())

	task := discardableTask(t, cfg, dataDir, "retained-task")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	cycle := discardableCycle(t, dataDir)
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	check := discardableBaseline(t, cfg, dataDir)
	if err := state.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}

	barrier := newRemovalBarrier(t, filepath.Join(dataDir, "tasks", task.ID))
	app := New(state, dataDir, WithWorkspaceRemoval(barrier.remove))
	t.Cleanup(app.Shutdown)

	done := make(chan error, 1)
	go func() { done <- app.retention(cfg) }()
	barrier.wait(t)

	if err := completesDuring(t, "pause", app.Pause); err != nil {
		t.Fatalf("pause during retention removal: %v", err)
	}
	if err := completesDuring(t, "duplicate discard", func() error {
		return app.TaskAction(context.Background(), task.ID, "discard")
	}); err == nil || !IsActionConflict(err) {
		t.Fatalf("discard of a retention-claimed task = %v; want a conflict", err)
	}
	// A second retention pass must not wait on the held target or report it as
	// a cleanup failure; other candidates proceed independently.
	if err := completesDuring(t, "concurrent retention", func() error { return app.retention(cfg) }); err != nil {
		t.Fatalf("concurrent retention: %v", err)
	}

	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("retention: %v", err)
	}
	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("retained task not discarded: %+v, %v", saved, err)
	}
	savedCycle, err := store.Get[model.Cycle](state, "cycle", cycle.ID)
	if err != nil || savedCycle == nil || savedCycle.Lifecycle.DiscardedAt == nil {
		t.Fatalf("retained cycle not discarded: %+v, %v", savedCycle, err)
	}
	savedCheck, err := store.Get[model.BaselineCheck](state, "baseline", check.ID)
	if err != nil || savedCheck == nil || !savedCheck.WorkspaceRemoved || savedCheck.CleanupError != nil {
		t.Fatalf("retained baseline not cleaned: %+v, %v", savedCheck, err)
	}
	for _, dir := range []string{
		filepath.Join(dataDir, "tasks", task.ID),
		filepath.Join(dataDir, "cycles", cycle.ID),
		filepath.Join(dataDir, "baselines", check.ID),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("retention left %s behind: %v", dir, err)
		}
	}
	for _, id := range []string{task.ID, cycle.ID, check.ID} {
		if events := cleanupEvents(t, state, id); len(events) != 0 {
			t.Fatalf("claimed cleanup recorded a failure: %+v", events)
		}
	}
}

// A claimed cycle refuses a second cleanup and any other action while its
// planning directory is held inside removal; controls stay responsive and the
// in-flight discard remains tracked service work that Shutdown waits out and
// lets finalize.
func TestCycleDiscardClaimsConflictsAndShutdownWaits(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	cycle := discardableCycle(t, dataDir)
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	barrier := newRemovalBarrier(t, filepath.Join(dataDir, "cycles", cycle.ID))
	app := New(state, dataDir, WithWorkspaceRemoval(barrier.remove))

	done := make(chan error, 1)
	go func() { done <- app.CycleAction(cycle.ID, "discard") }()
	barrier.wait(t)

	if err := completesDuring(t, "pause", app.Pause); err != nil {
		t.Fatalf("pause during cycle removal: %v", err)
	}
	for _, action := range []string{"discard", "archive"} {
		if err := app.CycleAction(cycle.ID, action); err == nil || !IsActionConflict(err) {
			t.Fatalf("%s on a claimed cycle = %v; want a conflict", action, err)
		}
	}

	// The held discard is owned service work: shutdown cannot return while it
	// is still inside removal, and it must finish finalizing afterwards.
	shutdownDone := make(chan struct{})
	go func() {
		app.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned while an owned removal was still held")
	case <-time.After(200 * time.Millisecond):
	}
	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("cycle discard: %v", err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown did not finish after the held removal released")
	}
	saved, err := store.Get[model.Cycle](state, "cycle", cycle.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("shutdown returned before the cycle was marked discarded: %+v, %v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "cycles", cycle.ID)); !os.IsNotExist(err) {
		t.Fatalf("cycle directory still present: %v", err)
	}
	if err := app.CycleAction(cycle.ID, "archive"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cycle action after shutdown = %v; want cancellation", err)
	}
}

// Baseline cleanup claims the check, dedupes a second cleanup instead of
// waiting, keeps the terminal-check cancel refusal honest, and applies only
// the cleanup fields to the current durable record so a write that lands
// mid-removal survives.
func TestBaselineCleanupClaimsSkipsDuplicatesAndPreservesConcurrentWrites(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	check := discardableBaseline(t, cfg, dataDir)
	if err := state.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}
	barrier := newRemovalBarrier(t, filepath.Join(dataDir, "baselines", check.ID))
	app := New(state, dataDir, WithWorkspaceRemoval(barrier.remove))
	t.Cleanup(app.Shutdown)

	done := make(chan error, 1)
	go func() { done <- app.CleanupBaseline(&check) }()
	barrier.wait(t)

	if err := completesDuring(t, "pause", app.Pause); err != nil {
		t.Fatalf("pause during baseline cleanup: %v", err)
	}
	if err := completesDuring(t, "duplicate cleanup", func() error {
		duplicate := check
		return app.CleanupBaseline(&duplicate)
	}); err != nil {
		t.Fatalf("duplicate baseline cleanup = %v; want deduped success", err)
	}
	var conflict *BaselineConflict
	if err := app.CancelBaseline(check.ID); err == nil || !errors.As(err, &conflict) {
		t.Fatalf("cancel on a terminal claimed check = %v; want baseline conflict", err)
	}
	// A write landing mid-removal — e.g. the worker's last evidence — must
	// survive finalization.
	current, err := store.Get[model.BaselineCheck](state, "baseline", check.ID)
	if err != nil || current == nil {
		t.Fatalf("reload check: %v", err)
	}
	current.Commands = append(current.Commands, model.BaselineCommand{
		Command: "late", Success: true, Output: "kept", CreatedAt: model.Now(),
	})
	if err := state.Put("baseline", check.ID, *current); err != nil {
		t.Fatal(err)
	}

	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("baseline cleanup: %v", err)
	}
	saved, err := store.Get[model.BaselineCheck](state, "baseline", check.ID)
	if err != nil || saved == nil || !saved.WorkspaceRemoved || saved.CleanupError != nil {
		t.Fatalf("baseline cleanup outcome: %+v, %v", saved, err)
	}
	if len(saved.Commands) != 1 || saved.Commands[0].Command != "late" {
		t.Fatalf("finalization clobbered a concurrent write: %+v", saved.Commands)
	}
	if !check.WorkspaceRemoved {
		t.Fatal("caller record did not reflect the recorded outcome")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "baselines", check.ID)); !os.IsNotExist(err) {
		t.Fatalf("baseline clone still present: %v", err)
	}
	if app.cleanupClaimed(cleanupBaseline, check.ID) {
		t.Fatal("baseline cleanup claim leaked after completion")
	}
}

// A failed removal records a redacted cleanup error, never marks the check
// removed, stays a cleanup candidate and retries cleanly once the removal
// works.
func TestBaselineCleanupFailureRecordsARedactedErrorAndRetries(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	check := discardableBaseline(t, cfg, dataDir)
	if err := state.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dataDir, "baselines", check.ID)
	var fail atomic.Bool
	fail.Store(true)
	app := New(state, dataDir, WithWorkspaceRemoval(func(root, path string) error {
		if path == target && fail.Swap(false) {
			return errors.New("removal failed after reading bearer fixturesecrettoken123")
		}
		return workspace.RemoveOwnedDir(root, path)
	}))
	t.Cleanup(app.Shutdown)

	if err := app.CleanupBaseline(&check); err != nil {
		t.Fatalf("cleanup error must be recorded, not returned: %v", err)
	}
	if check.WorkspaceRemoved || check.CleanupError == nil {
		t.Fatalf("failed cleanup marked as removed: %+v", check)
	}
	if strings.Contains(*check.CleanupError, "fixturesecrettoken123") || !strings.Contains(*check.CleanupError, "[redacted]") {
		t.Fatalf("cleanup error was not redacted: %q", *check.CleanupError)
	}
	saved, err := store.Get[model.BaselineCheck](state, "baseline", check.ID)
	if err != nil || saved == nil || saved.WorkspaceRemoved || saved.CleanupError == nil {
		t.Fatalf("durable cleanup outcome: %+v, %v", saved, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("failed cleanup still removed the directory: %v", err)
	}
	candidates, err := state.BaselineCleanupCandidates()
	if err != nil {
		t.Fatal(err)
	}
	remaining := false
	for _, candidate := range candidates {
		remaining = remaining || candidate.ID == check.ID
	}
	if !remaining {
		t.Fatal("failed cleanup left the candidate list")
	}

	if err := app.CleanupBaseline(&check); err != nil {
		t.Fatalf("retry after a failed cleanup: %v", err)
	}
	saved, err = store.Get[model.BaselineCheck](state, "baseline", check.ID)
	if err != nil || saved == nil || !saved.WorkspaceRemoved || saved.CleanupError != nil {
		t.Fatalf("retried cleanup outcome: %+v, %v", saved, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("retried cleanup left the directory: %v", err)
	}
	if app.cleanupClaimed(cleanupBaseline, check.ID) {
		t.Fatal("failed cleanup leaked its claim")
	}
}

// A failed operator discard reports the error without marking the record, and
// the same failure through retention lands as a redacted cleanup event while
// the record stays a candidate — a later pass completes it.
func TestCleanupFailureLeavesTaskACandidateAndRetries(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	cfg.RetainCompletedDays = 1
	saveSettings(t, state, cfg, model.DefaultControl())
	task := discardableTask(t, cfg, dataDir, "failing-task")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dataDir, "tasks", task.ID)
	var fail atomic.Bool
	fail.Store(true)
	app := New(state, dataDir, WithWorkspaceRemoval(func(root, path string) error {
		if path == target && fail.Swap(false) {
			return errors.New("removal failed after reading bearer fixturesecrettoken123")
		}
		return workspace.RemoveOwnedDir(root, path)
	}))
	t.Cleanup(app.Shutdown)

	err := app.TaskAction(context.Background(), task.ID, "discard")
	if err == nil || IsActionConflict(err) {
		t.Fatalf("failed discard = %v; want a plain removal error", err)
	}
	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt != nil {
		t.Fatalf("failed discard marked the record: %+v, %v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(task.Workspace, "evidence.txt")); err != nil {
		t.Fatalf("failed discard removed evidence: %v", err)
	}
	if app.cleanupClaimed(cleanupTask, task.ID) {
		t.Fatal("failed discard leaked its claim")
	}

	// The same failure through retention records a redacted cleanup_error
	// event and leaves the record a candidate.
	fail.Store(true)
	if err := app.retention(cfg); err != nil {
		t.Fatalf("retention: %v", err)
	}
	events := cleanupEvents(t, state, task.ID)
	if len(events) != 1 || strings.Contains(events[0].Message, "fixturesecrettoken123") || !strings.Contains(events[0].Message, "[redacted]") {
		t.Fatalf("retention failure not recorded redacted: %+v", events)
	}
	saved, err = store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt != nil {
		t.Fatalf("failed retention marked the record: %+v, %v", saved, err)
	}
	ids, err := state.CleanupCandidates("task", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	remaining := false
	for _, id := range ids {
		remaining = remaining || id == task.ID
	}
	if !remaining {
		t.Fatal("failed cleanup left the candidate list")
	}

	if err := app.retention(cfg); err != nil {
		t.Fatalf("retried retention: %v", err)
	}
	saved, err = store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("retried cleanup did not mark the record: %+v, %v", saved, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("retried cleanup left the directory: %v", err)
	}
}

// An owner-path refusal is preserved end to end: the foreign directory is not
// removed, the record is not marked, and the refusal is reported rather than
// claimed as success.
func TestDiscardRefusesAnUnownedPathWithoutMarking(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	task := discardableTask(t, cfg, dataDir, "unowned")
	// The recorded workspace points outside the task's owned directory.
	external := t.TempDir()
	task.Workspace = filepath.Join(external, "workspace")
	if err := os.MkdirAll(task.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	app := New(state, dataDir)
	t.Cleanup(app.Shutdown)

	if err := app.TaskAction(context.Background(), task.ID, "discard"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("unowned discard = %v; want an owner-path refusal", err)
	}
	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt != nil {
		t.Fatalf("refused discard marked the record: %+v, %v", saved, err)
	}
	if _, err := os.Stat(task.Workspace); err != nil {
		t.Fatalf("refused discard removed the foreign path: %v", err)
	}
	if app.cleanupClaimed(cleanupTask, task.ID) {
		t.Fatal("refused discard leaked its claim")
	}
}

// A claim excludes every workspace user, not only a second cleanup: retry,
// cancel and reconcile conflict on claimed records, and execution admission
// leaves a claimed queued task queued until the claim is released.
func TestCleanupClaimConflictsTaskActionsAndExecutionAdmission(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	saveSettings(t, state, cfg, control)

	blocked := queuedTask(cfg, "claimed-blocked", cfg.DefaultBranch, "octomus/claimed-blocked")
	blocked.Status = model.StatusBlocked
	reconcilable := queuedTask(cfg, "claimed-reconcile", cfg.DefaultBranch, "octomus/claimed-reconcile")
	reconcilable.Status = model.StatusBlocked
	reconcilable.BlockedReason = blockedReasonPtr(model.BlockedReasonPublicationUncertain)
	reconcilable.OutputCommit = stringPointer("output")
	queued := queuedTask(cfg, "claimed-queued", "octomus/existing", "octomus/existing")
	for _, task := range []model.Task{blocked, reconcilable, queued} {
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan string, 4)
	app := New(state, dataDir, WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, task model.Task) error {
		started <- task.ID
		<-ctx.Done()
		return ctx.Err()
	})))
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()

	app.gate.Lock()
	claimed := app.claimCleanup(cleanupTask, blocked.ID) &&
		app.claimCleanup(cleanupTask, reconcilable.ID) &&
		app.claimCleanup(cleanupTask, queued.ID)
	app.gate.Unlock()
	if !claimed {
		t.Fatal("fresh claims were not free")
	}

	for _, action := range []string{"retry", "cancel", "archive"} {
		err := app.TaskAction(context.Background(), blocked.ID, action)
		if err == nil || !IsActionConflict(err) || !strings.Contains(err.Error(), "cleanup") {
			t.Fatalf("%s on a claimed task = %v; want the cleanup conflict", action, err)
		}
	}
	// Reconcile is eligible on this record, so the claim itself is the refusal.
	if err := app.TaskAction(context.Background(), reconcilable.ID, "reconcile"); err == nil ||
		!IsActionConflict(err) || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("reconcile on a claimed task = %v; want the cleanup conflict", err)
	}

	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		t.Fatalf("claimed queued task dispatched: %s", id)
	default:
	}
	saved, err := store.Get[model.Task](state, "task", queued.ID)
	if err != nil || saved == nil || saved.Status != model.StatusQueued {
		t.Fatalf("claimed queued task changed: %+v, %v", saved, err)
	}

	app.releaseCleanup(cleanupTask, queued.ID)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		if id != queued.ID {
			t.Fatalf("dispatched unexpected task %s", id)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("released claim still blocks dispatch")
	}
	saved, err = store.Get[model.Task](state, "task", queued.ID)
	if err != nil || saved == nil || !saved.Status.Active() {
		t.Fatalf("released queued task did not dispatch: %+v, %v", saved, err)
	}
}

// A removal abandoned mid-delete — the durable state a crash leaves — is not
// marked, holds no claim across a restart, and the restarted service finishes
// the partial tree through the normal cleanup path.
func TestInterruptedCleanupLeavesNoClaimAndRetriesAfterRestart(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	cfg.RetainCompletedDays = 1
	saveSettings(t, state, cfg, model.DefaultControl())
	task := discardableTask(t, cfg, dataDir, "interrupted-task")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	check := discardableBaseline(t, cfg, dataDir)
	if err := state.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(dataDir, "tasks", task.ID)
	// The interrupted removal deleted part of the tree before dying.
	first := New(state, dataDir, WithWorkspaceRemoval(func(root, path string) error {
		if path == owner {
			if err := os.Remove(filepath.Join(task.Workspace, "evidence.txt")); err != nil {
				return err
			}
			return errors.New("removal interrupted")
		}
		return workspace.RemoveOwnedDir(root, path)
	}))
	if err := first.TaskAction(context.Background(), task.ID, "discard"); err == nil {
		t.Fatal("interrupted removal reported success")
	}
	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt != nil {
		t.Fatalf("interrupted cleanup marked the record: %+v, %v", saved, err)
	}
	if _, err := os.Stat(task.Workspace); err != nil {
		t.Fatalf("partial tree vanished before restart: %v", err)
	}
	first.Shutdown()

	// The restarted service inherits no claim and no false completion; the
	// next cleanup pass finishes the partial tree honestly.
	restarted := New(state, dataDir)
	t.Cleanup(restarted.Shutdown)
	if err := restarted.retention(cfg); err != nil {
		t.Fatalf("post-restart retention: %v", err)
	}
	saved, err = store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("post-restart cleanup did not complete: %+v, %v", saved, err)
	}
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Fatalf("partial tree survived post-restart cleanup: %v", err)
	}
	savedCheck, err := store.Get[model.BaselineCheck](state, "baseline", check.ID)
	if err != nil || savedCheck == nil || !savedCheck.WorkspaceRemoved {
		t.Fatalf("post-restart baseline cleanup did not complete: %+v, %v", savedCheck, err)
	}
	if events := cleanupEvents(t, state, task.ID); len(events) != 0 {
		t.Fatalf("retried cleanup recorded a failure: %+v", events)
	}
}
