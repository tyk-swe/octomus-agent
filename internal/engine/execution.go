package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func (a *App) superviseTask(ctx context.Context, task model.Task) error {
	if a.ctx.Err() != nil {
		// Admission may commit after shutdown begins. This worker has not
		// started execution, so defer it without consuming a retry attempt.
		a.gate.Lock()
		current, err := store.Get[model.Task](a.Store, "task", task.ID)
		if err != nil || current == nil || !current.Status.Active() {
			a.gate.Unlock()
			return err
		}
		operatorCancelled, err := a.Store.Marked("cancel", task.ID)
		if err == nil && !operatorCancelled {
			err = a.transition(current, model.StatusQueued)
		}
		a.gate.Unlock()
		if err != nil || !operatorCancelled {
			return err
		}
		task = *current
	}
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	limit := task.ExecutionConfig().TaskTimeout()
	result, executeErr := runJoined(ctx, workCancel, limit, "Task worker panicked", func() error {
		return a.execute(workCtx, &task)
	})
	if executeErr == nil {
		return nil
	}
	if task.Status == model.StatusPublished {
		_ = a.Store.Event(task.ID, "error", executeErr.Error())
		return executeErr
	}
	timedOut := result.Expired && !result.AlreadyCancelled
	taskErr := executeErr
	if result.Expired {
		taskErr = errors.New("Task time limit exceeded")
	}
	message := taskErr.Error()
	shuttingDown := a.ctx.Err() != nil
	operatorCancelled, _ := a.Store.Marked("cancel", task.ID)
	if shuttingDown && !operatorCancelled && !timedOut && task.Status.Active() && workspace.Initialized(task) {
		if err := a.saveTask(&task); err != nil {
			_ = a.Store.Event(task.ID, "worker_error", redact.Error(err))
		}
		return a.Store.Event(task.ID, "interrupted", message)
	}
	status := model.StatusBlocked
	if workCtx.Err() != nil && !timedOut && task.OutputCommit == nil && (operatorCancelled || a.ctx.Err() == nil) {
		status = model.StatusCancelled
	}
	if status == model.StatusCancelled {
		task.BlockedReason = nil
		task.Error = new("Cancelled by the operator")
	} else {
		recordTaskError(&task, taskErr)
		if result.Expired {
			task.BlockedReason = new(model.BlockedTimeout)
		}
	}
	model.FailRunning(task.Sessions, *task.Error)
	if err := a.transition(&task, status); err != nil {
		_ = a.Store.Event(task.ID, "worker_error", redact.Error(err))
	}
	return a.Store.Event(task.ID, "error", message)
}

func runJoined(ctx context.Context, cancel context.CancelFunc, limit time.Duration, panicked string, fn func() error) (process.Deadline[error], error) {
	done := make(chan struct{})
	var fnErr error
	result := process.WithDeadline(ctx, cancel, limit, func() (err error) {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("%s: %v", panicked, recovered)
			}
			fnErr = err
		}()
		return fn()
	})
	<-done
	return result, fnErr
}

func (a *App) execute(ctx context.Context, task *model.Task) error {
	cfg := task.ExecutionConfig()
	task.Error = nil
	task.BlockedReason = nil
	if task.RepairRounds == nil {
		// Older records have only the review-based budget estimate. Retain
		// that conservative allowance, then count completed repairs directly.
		task.RepairRounds = new(task.AttemptReviews())
	}
	if err := a.saveTask(task); err != nil {
		return err
	}
	if task.OutputCommit != nil {
		return a.publishReviewed(ctx, task, *task.OutputCommit)
	}
	if err := a.retryPreflight(ctx, task); err != nil {
		return err
	}
	client := a.runners(ctx, cfg, task.ID)
	defer func() { _ = client.Close() }()
	if err := a.validateRoutes(client, cfg, false); err != nil {
		return fmt.Errorf("%w: %w", model.BlockedRunnerUnavailable, err)
	}
	admissionReserved := task.ExecutionSession == nil
	if admissionReserved {
		if err := a.initializeTask(ctx, task); err != nil {
			return err
		}
	}
	ws := task.Workspace
	if _, err := workspace.GitDir(ws); err != nil {
		return model.BlockedWorkspaceInvalid
	}
	if task.ComparisonBase == "" {
		return fmt.Errorf("Comparison base was not persisted; cancel this task and rediscover: %w", model.BlockedWorkspaceInvalid)
	}
	if err := a.runExecutor(ctx, task, client, admissionReserved); err != nil {
		return err
	}
	for {
		revision, err := gitops.Snapshot(ctx, cfg, ws, task.Proposal.Title)
		if err != nil {
			return err
		}
		if revision == task.SourceRevision {
			return fmt.Errorf("No changes were committed on top of the source revision: %w", model.BlockedVerificationFailed)
		}
		names, err := gitops.WorkGit(ctx, cfg, ws, []string{"diff", "--name-only", "--ignore-submodules=none", task.SourceRevision, revision})
		if err != nil {
			return err
		}
		if names == "" {
			return fmt.Errorf("The change set is empty against the source revision: %w", model.BlockedVerificationFailed)
		}
		review, err := a.reviewRevision(ctx, task, client, revision)
		if err != nil {
			return err
		}
		qualified := review.Clean()
		if qualified {
			round := task.Reviews[len(task.Reviews)-1]
			if round.Maintenance != nil && !round.Maintenance.Qualifies {
				qualified = false
			}
		}
		verificationErrors := []string{}
		if qualified {
			verificationErrors, err = a.verifyRevision(ctx, task, revision)
			if err != nil {
				return err
			}
			if len(verificationErrors) == 0 {
				if err := requireDefaultRevision(ctx, cfg, task.DefaultRevision); err != nil {
					return err
				}
				return a.publishReviewed(ctx, task, revision)
			}
		}
		if *task.RepairRounds >= cfg.MaxRepairRounds {
			return fmt.Errorf("Repair budget exhausted (max_repair_rounds %d): %w", cfg.MaxRepairRounds, model.BlockedVerificationFailed)
		}
		if progress := task.RepairProgress; progress != nil {
			if progress.Revision != revision {
				progress.NoProgressRounds = 0
			} else if progress.AwaitingReview {
				progress.NoProgressRounds++
			}
			progress.Revision = revision
			progress.AwaitingReview = false
			// Consume the completed repair before another turn can start. Recovery may review this same revision
			// again after a shutdown here, but must not count that repair a second time.
			if err := a.saveTask(task); err != nil {
				return err
			}
			if progress.NoProgressRounds >= cfg.MaxNoProgressRounds {
				return fmt.Errorf("Repairs made no progress on the reviewed revision (max_no_progress_rounds %d): %w", cfg.MaxNoProgressRounds, model.BlockedVerificationFailed)
			}
		}
		if round := task.Reviews[len(task.Reviews)-1]; round.Maintenance != nil && !round.Maintenance.Qualifies && len(review.Findings) == 0 {
			review.Findings = []model.Finding{{Title: "Maintenance scope assessment", Detail: round.Maintenance.Reason, Priority: "P1"}}
		}
		if err := a.repair(ctx, task, client, review, verificationErrors); err != nil {
			return err
		}
	}
}

func (a *App) publishReviewed(ctx context.Context, task *model.Task, revision string) error {
	cfg := task.ExecutionConfig()
	var footprint *model.MaintenanceFootprint
	var footprintErr error
	if cfg.DeliveryMode == config.DeliveryModeMaintenance {
		if task.MaintenanceFootprint != nil && task.MaintenanceFootprint.Revision == revision &&
			task.MaintenanceFootprint.ComparisonBase == task.ComparisonBase {
			footprint = task.MaintenanceFootprint
		} else {
			frozen, err := gitops.ReadMaintenanceFootprint(ctx, cfg, task.Workspace, task.ComparisonBase, revision)
			footprint = &frozen
			footprintErr = err
		}
	}
	// The checkpoint and operator eligibility share the gate. A successful
	// cancellation must win before a fresh output commit authorizes publication;
	// after the checkpoint, a refused cancellation must not stop the worker.
	err := func() error {
		a.gate.Lock()
		defer a.gate.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		live, err := a.Config()
		if err != nil {
			return err
		}
		if task.Config.DeliveryMode != live.DeliveryMode ||
			!slices.Contains(task.Config.EffectiveCategories(), task.Proposal.Category) {
			return fmt.Errorf("Task %s was planned under an incompatible delivery mode or category: %w", task.ID, model.BlockedInvalidPlan)
		}
		if task.Config.DeliveryMode == config.DeliveryModeMaintenance {
			if footprintErr != nil {
				_ = a.Store.Event(task.ID, "maintenance", "Footprint measurement is incomplete; the delivery can publish but stays manual: "+redact.Error(footprintErr))
			}
			task.MaintenanceFootprint = footprint
		}
		if !task.ReviewAuthorizes(revision) {
			return fmt.Errorf("Publication requires a clean maintenance-qualified review at the output revision: %w", model.BlockedInvalidReview)
		}
		previousStatus, previousOutput, previousUpdated := task.Status, task.OutputCommit, task.UpdatedAt
		task.OutputCommit = &revision
		task.Status = model.StatusPublishing
		if err := a.saveTask(task); err != nil {
			// A failed write cannot authorize later publication or override a
			// cancellation accepted after this gate is released.
			task.Status, task.OutputCommit, task.UpdatedAt = previousStatus, previousOutput, previousUpdated
			return err
		}
		// The checkpoint is durable even if its separate status event fails.
		return a.Store.Event(task.ID, "status", statusLabel(model.StatusPublishing))
	}()
	if err != nil {
		return err
	}
	p, err := gitops.Publish(ctx, *task)
	if err != nil {
		return err
	}
	return a.published(task, p)
}

func (a *App) publishedDependency(id string) (model.Task, error) {
	dependency, err := store.Get[model.Task](a.Store, "task", id)
	if err != nil {
		return model.Task{}, err
	}
	if dependency == nil {
		return model.Task{}, model.BlockedDependencyBlocked
	}
	if dependency.Status != model.StatusPublished {
		return model.Task{}, model.BlockedDependencyBlocked
	}
	return *dependency, nil
}

func ensureWorkspaceAt(ctx context.Context, cfg config.Config, ws, revision string) error {
	at, err := gitops.At(ctx, cfg, ws, revision)
	if err != nil {
		return err
	}
	if !at {
		return model.BlockedWorkspaceInvalid
	}
	return nil
}

func requireDefaultRevision(ctx context.Context, cfg config.Config, want string) error {
	def, err := gitops.RemoteRevision(ctx, cfg, cfg.DefaultBranch)
	if err != nil {
		return err
	}
	if def == nil || *def != want {
		return model.BlockedStaleBase
	}
	return nil
}

func (a *App) retryPreflight(ctx context.Context, task *model.Task) error {
	c := task.ExecutionConfig()
	if task.Lifecycle.DiscardedAt != nil || task.Lifecycle.ArchivedAt != nil {
		return model.BlockedWorkspaceInvalid
	}
	if err := requireDefaultRevision(ctx, c, task.DefaultRevision); err != nil {
		return err
	}
	source, err := gitops.RemoteRevision(ctx, c, task.Proposal.Target)
	if err != nil {
		return err
	}
	authorized := source != nil && *source == task.SourceRevision
	for _, id := range task.Proposal.Dependencies {
		dependency, err := a.publishedDependency(id)
		if err != nil {
			return err
		}
		if task.ExecutionSession == nil && sourcePtrEqual(source, dependency.OutputCommit) {
			authorized = true
		}
	}
	if !authorized {
		return model.BlockedStaleBase
	}
	if task.ExecutionSession != nil && !workspace.Initialized(*task) {
		return model.BlockedWorkspaceInvalid
	}
	if task.ExecutionSession == nil && task.Workspace != "" {
		return a.checkWorkspace(ctx, task)
	}
	return nil
}

func (a *App) checkWorkspace(ctx context.Context, task *model.Task) error {
	ws := a.taskWorkspace(task.ID)
	if !config.SamePath(task.Workspace, ws) || task.ComparisonBase == "" {
		return model.BlockedWorkspaceInvalid
	}
	if _, err := workspace.GitDir(ws); err != nil {
		return model.BlockedWorkspaceInvalid
	}
	return ensureWorkspaceAt(ctx, task.ExecutionConfig(), ws, task.SourceRevision)
}

func (a *App) initializeTask(ctx context.Context, task *model.Task) error {
	cfg := task.ExecutionConfig()
	if err := gitops.Fetch(ctx, cfg); err != nil {
		return err
	}
	remote, err := gitops.RemoteRevision(ctx, cfg, task.Proposal.Target)
	if err != nil {
		return err
	}
	if remote == nil {
		return model.BlockedStaleBase
	}
	current := *remote
	dependencyOutputs := []string{}
	for _, identity := range task.Proposal.Dependencies {
		dependency, err := a.publishedDependency(identity)
		if err != nil {
			return err
		}
		if dependency.Branch != task.Proposal.Target {
			return model.BlockedDependencyBlocked
		}
		if dependency.OutputCommit == nil {
			return errors.New("Dependency output revision is missing")
		}
		ancestor, err := gitops.IsAncestor(ctx, cfg, cfg.Repository, *dependency.OutputCommit, current)
		if err != nil {
			return err
		}
		if !ancestor {
			return model.BlockedDependencyBlocked
		}
		dependencyOutputs = append(dependencyOutputs, *dependency.OutputCommit)
	}
	if current != task.SourceRevision {
		if !slices.Contains(dependencyOutputs, current) {
			return model.BlockedStaleBase
		}
		task.SourceRevision = current
		if err := a.saveTask(task); err != nil {
			return err
		}
	}
	if err := requireDefaultRevision(ctx, cfg, task.DefaultRevision); err != nil {
		return err
	}
	if task.PRNumber != nil {
		p, err := gitops.PR(ctx, cfg, *task.PRNumber)
		if err != nil {
			return err
		}
		if !p.OwnedOpen() || p.Base != cfg.DefaultBranch {
			return model.BlockedStaleBase
		}
	}
	if _, err := uuid.Parse(task.ID); err != nil {
		return fmt.Errorf("Invalid task workspace identity: %w", err)
	}
	if err := a.admit(ctx, task.CycleID, task, "executor", task.Route); err != nil {
		return err
	}
	if task.Workspace == "" {
		ws := a.taskWorkspace(task.ID)
		task.Workspace = ws
		if err := a.saveTask(task); err != nil {
			return err
		}
		if err := gitops.CloneAt(ctx, cfg, ws, task.SourceRevision); err != nil {
			return err
		}
		if task.PRNumber != nil {
			task.ComparisonBase, err = gitops.WorkGit(ctx, cfg, ws, []string{"merge-base", task.DefaultRevision, task.SourceRevision})
			if err != nil {
				return err
			}
		} else {
			task.ComparisonBase = task.SourceRevision
		}
		return a.saveTask(task)
	}
	return a.checkWorkspace(ctx, task)
}

func (a *App) runExecutor(ctx context.Context, task *model.Task, client *runner.Runners, admissionReserved bool) error {
	cfg := task.ExecutionConfig()
	for _, s := range task.Sessions {
		if s.Role == "executor" && s.Status == model.SessionCompleted {
			return nil
		}
	}
	_, err := a.invoke(ctx, client, invocation{
		cycleID: task.CycleID, task: task, role: "executor", route: task.Route, workspace: task.Workspace,
		resume: task.ExecutionSession, keep: func(session string) { task.ExecutionSession = &session },
		prompt: executorPrompt(task, cfg), reserved: admissionReserved,
	})
	return err
}

func executorPrompt(task *model.Task, cfg config.Config) string {
	return fmt.Sprintf(
		"Implement this accepted task end to end in this workspace. Source revision: %s. Full comparison base: %s. Existing PR: %s. Preserve existing accumulated branch behavior; inspect its full diff. Do not push, publish, merge or deploy. Required repository verification commands: %s. Objective and constraints:\n%s\nProblem: %s\nBenefit: %s\nScope: %s\nEvidence: %s\nReturn a concise summary of actual changes, verification and material risks or migration notes.",
		task.SourceRevision,
		task.ComparisonBase,
		quoteOption(task.PRURL),
		quoteList(cfg.VerificationCommands),
		task.Proposal.Prompt,
		task.Proposal.Problem,
		task.Proposal.Benefit,
		task.Proposal.Scope,
		quoteList(task.Proposal.Evidence)) + maintenancePolicy(task.Config)
}

func (a *App) repair(ctx context.Context, task *model.Task, client *runner.Runners, review model.Review, verificationErrors []string) error {
	cfg := task.ExecutionConfig()
	if err := a.transition(task, model.StatusRepairing); err != nil {
		return err
	}
	prompt, err := repairPrompt(task, cfg, review, verificationErrors)
	if err != nil {
		return err
	}
	_, err = a.invoke(ctx, client, invocation{
		cycleID: task.CycleID, task: task, role: "repair", route: cfg.RepairRoute, workspace: task.Workspace,
		resume: task.RepairSession, keep: func(session string) { task.RepairSession = &session },
		prompt: prompt,
		completed: func() {
			*task.RepairRounds++
			if task.RepairProgress == nil {
				task.RepairProgress = &model.RepairProgress{}
			}
			task.RepairProgress.Revision = task.Reviews[len(task.Reviews)-1].Revision
			task.RepairProgress.AwaitingReview = true
		},
	})
	return err
}

func repairPrompt(task *model.Task, cfg config.Config, review model.Review, verificationErrors []string) (string, error) {
	findings := review.Findings
	if findings == nil {
		findings = []model.Finding{}
	}
	findingsJSON, err := wirejson.Marshal(findings)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"Repair actionable findings and verification failures for this task. Preserve useful capabilities and meaningful tests. Do not push, publish, merge or deploy. If a finding is unsupported, explain the technical evidence in your final summary; the next fresh reviewer must independently assess it. Rerun relevant verification %s. Full comparison base: %s. Task: %s. Findings: %s. Verification failures: %s",
		quoteList(cfg.VerificationCommands),
		task.ComparisonBase,
		task.Proposal.Prompt,
		string(findingsJSON),
		quoteList(verificationErrors)) + maintenanceRepairPolicy(task), nil
}

func (a *App) published(task *model.Task, p model.PullRequest) error {
	task.PRNumber = &p.Number
	task.PRURL = &p.URL
	task.Error = nil
	task.BlockedReason = nil
	task.Status = model.StatusPublished
	task.UpdatedAt = model.Now()
	merge, err := a.initialMergeState(task)
	if err != nil {
		return err
	}
	if err := a.Store.CompletePublication(*task, p, merge); err != nil {
		return err
	}
	a.runtimeMu.Lock()
	a.runtime.lastMergeCheck = time.Time{}
	a.runtimeMu.Unlock()
	a.notify()
	return nil
}

func (a *App) saveTask(task *model.Task) error {
	task.UpdatedAt = model.Now()
	return a.Store.Put("task", task.ID, *task)
}

func (a *App) transition(task *model.Task, status model.Status) error {
	task.Status = status
	if err := a.saveTask(task); err != nil {
		return err
	}
	return a.Store.Event(task.ID, "status", statusLabel(status))
}

func statusLabel(status model.Status) string {
	name := status.String()
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func (a *App) taskWorkspace(taskID string) string {
	return filepath.Join(a.dataDir, "tasks", taskID, "workspace")
}

func (a *App) setTaskError(task *model.Task, err error) error {
	recordTaskError(task, err)
	return a.transition(task, model.StatusBlocked)
}

func recordTaskError(task *model.Task, err error) {
	task.BlockedReason = new(model.BlockedReasonFromError(err))
	task.Error = new(redact.Error(err))
}

// Caller holds the gate until the exited worker releases its runtime claim.
// Keep new work blocked when a publication checkpoint cannot be settled yet.
func (a *App) settleExitedTask(task *model.Task, cause error) {
	publishing := task.Status == model.StatusPublishing && task.OutputCommit != nil
	model.FailRunning(task.Sessions, redact.Error(cause))
	if err := a.setTaskError(task, cause); err != nil && publishing {
		a.setRecoveryError(err)
	}
}

func sourcePtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// quoteList and quoteOption render prompt operands as quoted literals, so a command or path reads as one unit.
func quoteList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Quote(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func quoteOption(value *string) string {
	if value == nil {
		return "none"
	}
	return strconv.Quote(*value)
}
