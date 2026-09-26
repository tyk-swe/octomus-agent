// execution.go owns the task lifecycle once the scheduler admits a task: a
// deadline-supervised executor run, a fresh full-diff review per round,
// persistent repair sessions, bounded verification evidence, an output
// checkpoint, and publication through the guarded git layer. Every transition
// is durable before remote or runner work resumes so a restart never loses why
// a task ended where it did.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// superviseTask runs one task to a terminal durable state and records why it
// ended there: a deadline, a cancellation and a failure are all distinguishable
// afterwards, because a task that simply stopped being mentioned would be
// indistinguishable from one still running. This is the production TaskRunner.
func (a *App) superviseTask(ctx context.Context, task model.Task) error {
	return a.superviseExecution(ctx, task, a.execute)
}

func (a *App) superviseExecution(ctx context.Context, task model.Task, execute func(context.Context, *model.Task) error) error {
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	limit := time.Duration(task.ExecutionConfig().TaskTimeoutSeconds) * time.Second
	// The callback owns mutable task state and durable writes. runJoined waits
	// for it to return, so terminal evidence is recorded, and runTask releases
	// runtime and shutdown ownership, only after its last write.
	result, executeErr := runJoined(ctx, workCancel, limit, "Task worker panicked", func() error {
		return execute(workCtx, &task)
	})
	// execute succeeds only after publication is durably recorded, so a
	// deadline that expired during that final bookkeeping is not an outcome.
	if executeErr == nil {
		return nil
	}
	if task.Status == model.StatusPublished {
		// Delivery was recorded and only bookkeeping after it failed: keep the
		// published record. runTask still blocks the task if the published
		// status itself never became durable.
		_ = a.Store.Event(task.ID, "error", executeErr.Error())
		return executeErr
	}
	timedOut := result.Expired && !result.AlreadyCancelled
	taskErr := executeErr
	if result.Expired {
		taskErr = errors.New("Task time limit exceeded")
	}
	message := taskErr.Error()
	// Read the shutdown scope before the cancel marker. Shutdown cancels it
	// under the gate, which an operator cancel holds until its marker is
	// durable, so a cancel that preceded the shutdown is always seen.
	shuttingDown := a.ctx.Err() != nil
	operatorCancelled, _ := a.Store.MarkerSet("cancel", task.ID)
	if shuttingDown && !operatorCancelled && !timedOut && task.Status.Active() && workspace.Initialized(task) {
		// A service shutdown is not a task outcome. Leave the initialized
		// record active with its sessions running, exactly as after a crash:
		// restart recovery interrupts the sessions and requeues the task
		// within its retry budget. Work stopped before initialization is
		// blocked below and stays retryable; recovery could only report it
		// as an invalid workspace.
		if err := a.saveTask(&task); err != nil {
			_ = a.Store.Event(task.ID, "worker_error", store.ErrorMessage(err))
		}
		return a.Store.Event(task.ID, "interrupted", message)
	}
	status := model.StatusBlocked
	if workCtx.Err() != nil && !timedOut && task.OutputCommit == nil && (operatorCancelled || a.ctx.Err() == nil) {
		status = model.StatusCancelled
	}
	switch {
	case status == model.StatusCancelled:
		// Only an operator cancel stops a worker outside shutdown and its
		// deadline. Whatever the interrupted step returned, or a deadline that
		// fired after the cancel, is not why the task ended; that cause stays
		// in the error event below.
		task.BlockedReason = nil
		task.Error = stringPointer("Cancelled by the operator")
	case result.Expired:
		task.BlockedReason = blockedReasonPtr(model.BlockedReasonTimeout)
		task.Error = stringPointer(store.Redact(message))
	default:
		reason := model.BlockedReasonFromError(executeErr)
		task.BlockedReason = &reason
		task.Error = stringPointer(store.Redact(message))
	}
	model.FailRunning(task.Sessions, *task.Error)
	if err := a.transition(&task, status); err != nil {
		_ = a.Store.Event(task.ID, "worker_error", store.ErrorMessage(err))
	}
	return a.Store.Event(task.ID, "error", message)
}

// runJoined runs fn under limit through process.WithDeadline, then waits for
// fn to return even past WithDeadline's bounded cleanup grace: fn may still be
// writing durable state or remote publication, and its caller must not record
// an outcome or release ownership beside it. It returns WithDeadline's result
// together with fn's own result, which WithDeadline drops when the deadline
// fires first. fn runs on WithDeadline's goroutine, beyond every caller's
// recovery boundary, so a panic comes back as an error prefixed by panicked.
func runJoined(ctx context.Context, cancel context.CancelFunc, limit time.Duration, panicked string, fn func() error) (process.Deadline[error], error) {
	done := make(chan struct{})
	// Written before done closes and read only after the join.
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

// execute drives the admitted task through executor → snapshot → review →
// verification/repair rounds → output checkpoint → publication.
func (a *App) execute(ctx context.Context, task *model.Task) error {
	cfg := task.ExecutionConfig()
	task.Error = nil
	task.BlockedReason = nil
	if err := a.saveTask(task); err != nil {
		return err
	}
	if task.OutputCommit != nil {
		return a.publishReviewed(ctx, task)
	}
	if err := a.retryPreflight(ctx, task); err != nil {
		return err
	}
	client := a.runners(ctx, cfg, task.ID)
	defer func() { _ = client.Close() }()
	if err := client.ValidateRoutes(cfg, a.DataDir, false); err != nil {
		return fmt.Errorf("%w: %w", model.BlockedReasonRunnerUnavailable, err)
	}
	// Initialization reserves the first executor admission, including on retries.
	admissionReserved := task.ExecutionSession == nil
	if admissionReserved {
		if err := a.initializeTask(ctx, task); err != nil {
			return err
		}
	}
	ws := task.Workspace
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		return model.BlockedReasonWorkspaceInvalid
	}
	if task.ComparisonBase == "" {
		return fmt.Errorf("Comparison base was not persisted; cancel this task and rediscover: %w", model.BlockedReasonWorkspaceInvalid)
	}
	if err := a.runExecutor(ctx, task, client, admissionReserved); err != nil {
		return err
	}
	previous := ""
	noProgress := uint64(0)
	for {
		revision, err := gitops.Snapshot(ctx, cfg, ws, task.Proposal.Title)
		if err != nil {
			return err
		}
		if revision == task.SourceRevision {
			return fmt.Errorf("No changes were committed on top of the source revision: %w", model.BlockedReasonVerificationFailed)
		}
		names, err := gitops.Git(ctx, cfg, ws, []string{"diff", "--name-only", task.SourceRevision, revision})
		if err != nil {
			return err
		}
		if names == "" {
			return fmt.Errorf("The change set is empty against the source revision: %w", model.BlockedReasonVerificationFailed)
		}
		if task.AttemptReviews() >= cfg.MaxRepairRounds+1 {
			return model.BlockedReasonRetryLimit
		}
		review, err := a.reviewRevision(ctx, task, client, revision)
		if err != nil {
			return err
		}
		verificationErrors := []string{}
		if review.Clean() {
			verificationErrors, err = a.verifyRevision(ctx, task, revision)
			if err != nil {
				return err
			}
			if len(verificationErrors) == 0 {
				// Main movement changes the integration context; never silently publish an obsolete review.
				// Publication refuses a moved default branch for every task, so check it before the
				// checkpoint for existing-PR work too. A new-PR task's source is the default revision.
				def, err := gitops.RemoteRevision(ctx, cfg, cfg.DefaultBranch)
				if err != nil {
					return err
				}
				if def == nil || *def != task.DefaultRevision {
					return model.BlockedReasonStaleBase
				}
				task.OutputCommit = &revision
				return a.publishReviewed(ctx, task)
			}
		}
		if task.AttemptReviews() > cfg.MaxRepairRounds {
			return fmt.Errorf("Repair budget exhausted (max_repair_rounds %d): %w", cfg.MaxRepairRounds, model.BlockedReasonVerificationFailed)
		}
		if previous == revision {
			noProgress++
		} else {
			noProgress = 0
			previous = revision
		}
		if noProgress >= cfg.MaxNoProgressRounds {
			return fmt.Errorf("Repairs made no progress on the reviewed revision (max_no_progress_rounds %d): %w", cfg.MaxNoProgressRounds, model.BlockedReasonVerificationFailed)
		}
		if err := a.repair(ctx, task, client, review, verificationErrors); err != nil {
			return err
		}
	}
}

// publishReviewed is the shared publication tail once a task's output is
// recorded: transition, publish, record.
func (a *App) publishReviewed(ctx context.Context, task *model.Task) error {
	if err := a.transition(task, model.StatusPublishing); err != nil {
		return err
	}
	p, err := gitops.Publish(ctx, *task)
	if err != nil {
		return err
	}
	return a.published(task, p)
}

// publishedDependency returns a dependency that is recorded published;
// anything else blocks the dependent.
func (a *App) publishedDependency(id string) (model.Task, error) {
	dependency, err := store.Get[model.Task](a.Store, "task", id)
	if err != nil {
		return model.Task{}, err
	}
	if dependency == nil {
		return model.Task{}, model.BlockedReasonDependencyBlocked
	}
	if dependency.Status != model.StatusPublished {
		return model.Task{}, model.BlockedReasonDependencyBlocked
	}
	return *dependency, nil
}

// ensureWorkspaceAt requires the recorded workspace to still sit cleanly at
// `revision`; anything else means recorded evidence does not describe the
// current tree.
func ensureWorkspaceAt(ctx context.Context, cfg config.Config, ws, revision string) error {
	at, err := gitops.At(ctx, cfg, ws, revision)
	if err != nil {
		return err
	}
	if !at {
		return model.BlockedReasonWorkspaceInvalid
	}
	return nil
}

// retryPreflight revalidates a task's remote and workspace prerequisites under
// its live attempt policy before a retry or reconciliation resumes work.
func (a *App) retryPreflight(ctx context.Context, task *model.Task) error {
	c := task.ExecutionConfig()
	if task.Lifecycle.DiscardedAt != nil || task.Lifecycle.ArchivedAt != nil {
		return model.BlockedReasonWorkspaceInvalid
	}
	def, err := gitops.RemoteRevision(ctx, c, c.DefaultBranch)
	if err != nil {
		return err
	}
	if def == nil || *def != task.DefaultRevision {
		return model.BlockedReasonStaleBase
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
		return model.BlockedReasonStaleBase
	}
	// A task that started work must still hold its initialized workspace; one
	// whose initialization was recorded but never started a session may resume
	// only in a fully initialized clone at its source revision.
	if task.ExecutionSession != nil && !workspace.Initialized(*task) {
		return model.BlockedReasonWorkspaceInvalid
	}
	if task.ExecutionSession == nil && task.Workspace != "" {
		return a.validateRecordedWorkspace(ctx, task)
	}
	return nil
}

// validateRecordedWorkspace accepts a recorded workspace only when it is the
// task's own managed clone, initialization recorded its comparison base, and
// the clone sits cleanly at the task's source revision. Partial clones and
// edits made before a session was recorded are preserved for operator
// inspection, never reused.
func (a *App) validateRecordedWorkspace(ctx context.Context, task *model.Task) error {
	ws := a.taskWorkspace(task.ID)
	if !samePath(task.Workspace, ws) || task.ComparisonBase == "" {
		return model.BlockedReasonWorkspaceInvalid
	}
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		return model.BlockedReasonWorkspaceInvalid
	}
	return ensureWorkspaceAt(ctx, task.ExecutionConfig(), ws, task.SourceRevision)
}

// initializeTask prepares a task's workspace and reserves its first executor
// admission before the clone; the executor invocation then starts the fresh
// session under that reservation.
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
		return model.BlockedReasonStaleBase
	}
	current := *remote
	// Declared dependencies are validated against the selected head on every
	// initialization, not only when the head moved: a branch reset back to the
	// recorded source would otherwise skip the check entirely while the
	// scheduler still regards the dependency as delivered.
	dependencyOutputs := []string{}
	for _, identity := range task.Proposal.Dependencies {
		dependency, err := a.publishedDependency(identity)
		if err != nil {
			return err
		}
		if dependency.Branch != task.Proposal.Target {
			return model.BlockedReasonDependencyBlocked
		}
		if dependency.OutputCommit == nil {
			return errors.New("Dependency output revision is missing")
		}
		ancestor, err := gitops.IsAncestor(ctx, cfg, cfg.Repository, *dependency.OutputCommit, current)
		if err != nil {
			return err
		}
		if !ancestor {
			return model.BlockedReasonDependencyBlocked
		}
		dependencyOutputs = append(dependencyOutputs, *dependency.OutputCommit)
	}
	if current != task.SourceRevision {
		// Only the recorded source may advance, and only onto a dependency's
		// recorded output; any other remote movement remains a stale base.
		found := false
		for _, output := range dependencyOutputs {
			if output == current {
				found = true
			}
		}
		if !found {
			return model.BlockedReasonStaleBase
		}
		task.SourceRevision = current
		if err := a.saveTask(task); err != nil {
			return err
		}
	}
	def, err := gitops.RemoteRevision(ctx, cfg, cfg.DefaultBranch)
	if err != nil {
		return err
	}
	if def == nil || *def != task.DefaultRevision {
		return model.BlockedReasonStaleBase
	}
	if task.PRNumber != nil {
		p, err := gitops.PR(ctx, cfg, *task.PRNumber)
		if err != nil {
			return err
		}
		if !p.OwnedOpen() || p.Base != cfg.DefaultBranch {
			return model.BlockedReasonStaleBase
		}
	}
	if _, err := uuid.Parse(task.ID); err != nil {
		return fmt.Errorf("Invalid task workspace identity: %w", err)
	}
	if err := a.admit(task.CycleID, task, "executor", task.Route); err != nil {
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
			// The default revision was verified against the remote above and is
			// in the clone's object store; re-reading the remote here could name
			// a commit pushed after the fetch that the clone does not have.
			task.ComparisonBase, err = gitops.Git(ctx, cfg, ws, []string{"merge-base", task.DefaultRevision, task.SourceRevision})
			if err != nil {
				return err
			}
		} else {
			task.ComparisonBase = task.SourceRevision
		}
		return a.saveTask(task)
	}
	// A failed runner start can be retried in a fully initialized clone, at
	// the source revision after any dependency advance above.
	return a.validateRecordedWorkspace(ctx, task)
}

func (a *App) runExecutor(ctx context.Context, task *model.Task, client *runner.Runners, admissionReserved bool) error {
	cfg := task.ExecutionConfig()
	for _, s := range task.Sessions {
		if s.Role == "executor" && s.Status == model.SessionCompleted {
			return nil
		}
	}
	_, _, err := a.invoke(ctx, client, invocation{
		cycleID: task.CycleID, task: task, role: "executor", route: task.Route, workspace: task.Workspace,
		resume: task.ExecutionSession, keep: func(session string) { task.ExecutionSession = &session },
		prompt: executorPrompt(task, cfg), reserved: admissionReserved,
	})
	return err
}

// executorPrompt is the executor's task prompt. Its "Implement this accepted
// task" prefix is matched by the e2e runner fixtures (tests/fixtures), and it
// carries required policy: the full comparison base and no publication by
// the worker.
func executorPrompt(task *model.Task, cfg config.Config) string {
	return fmt.Sprintf(
		"Implement this accepted task end to end in this workspace. Source revision: %s. Full comparison base: %s. Existing PR: %s. Preserve existing accumulated branch behavior; inspect its full diff. Do not push, publish, merge or deploy. Required repository verification commands: %s. Objective and constraints:\n%s\nProblem: %s\nBenefit: %s\nScope: %s\nEvidence: %s\nReturn a concise summary of actual changes, verification and material risks or migration notes.",
		task.SourceRevision,
		task.ComparisonBase,
		debugOption(task.PRURL),
		debugList(cfg.VerificationCommands),
		task.Proposal.Prompt,
		task.Proposal.Problem,
		task.Proposal.Benefit,
		task.Proposal.Scope,
		debugList(task.Proposal.Evidence))
}

func (a *App) reviewRevision(ctx context.Context, task *model.Task, client *runner.Runners, revision string) (model.Review, error) {
	cfg := task.ExecutionConfig()
	ws := task.Workspace
	if err := a.transition(task, model.StatusReviewing); err != nil {
		return model.Review{}, err
	}
	route, ok := cfg.Roles["code_reviewer"]
	if !ok {
		return model.Review{}, errors.New("code_reviewer route is missing")
	}
	var review model.Review
	judge := func(thread, answer string) (string, error) {
		if err := json.Unmarshal([]byte(answer), &review); err != nil {
			return "", fmt.Errorf("%w: Unparseable review is not clean: %s", model.BlockedReasonInvalidReview, store.Redact(err.Error()))
		}
		if !review.Valid() {
			return "", model.BlockedReasonInvalidReview
		}
		if err := ensureWorkspaceAt(ctx, cfg, ws, revision); err != nil {
			return "", err
		}
		task.Reviews = append(task.Reviews, model.ReviewRound{SessionID: thread, Revision: revision, ComparisonBase: task.ComparisonBase, Result: review, CreatedAt: model.Now()})
		return review.Summary, nil
	}
	if _, _, err := a.invoke(ctx, client, invocation{
		cycleID: task.CycleID, task: task, role: "reviewer", route: route, workspace: ws,
		prompt: reviewPrompt(task, revision), schema: schemas.ReviewSchema(), judge: judge,
	}); err != nil {
		return model.Review{}, err
	}
	return review, nil
}

// reviewPrompt is a fresh reviewer's prompt for revision. Its "Perform a
// fresh code review" prefix is matched by the e2e runner fixtures, and it
// requires the full diff from the comparison base, never only the last commit.
func reviewPrompt(task *model.Task, revision string) string {
	return fmt.Sprintf(
		"Perform a fresh code review equivalent to /review of the COMPLETE change set: git diff %s HEAD. Recorded HEAD: %s. Include all accumulated PR changes and all repairs; do not only review the last commit. Task: %s. Scope: %s. Existing PR: %s. Inspect code and evidence, do not modify files. Report actionable correctness, regression, design or missing verification findings with file, priority and technical rationale. Do not invent findings. Set completed=true only after completing the review. A clean review must have an explanatory summary and zero findings.",
		task.ComparisonBase, revision, task.Proposal.Prompt, task.Proposal.Scope, debugOption(task.PRURL))
}

// verifyRevision runs every configured verification command against exactly
// `revision`. Worktree and HEAD are checked before the first command and after
// each one, so a command that changes tracked state, or leaves the check
// itself unable to run, is recorded as failed evidence and stops the run
// instead of lending its success to the reviewed revision.
func (a *App) verifyRevision(ctx context.Context, task *model.Task, revision string) ([]string, error) {
	cfg := task.ExecutionConfig()
	ws := task.Workspace
	verificationErrors := []string{}
	if err := a.transition(task, model.StatusVerifying); err != nil {
		return nil, err
	}
	if err := ensureWorkspaceAt(ctx, cfg, ws, revision); err != nil {
		return nil, err
	}
	for _, command := range cfg.VerificationCommands {
		outcome := runCheckCommand(ctx, cfg, ws, command, revision)
		if ctx.Err() != nil {
			return nil, errors.New("Operation cancelled")
		}
		failed := outcome.failed()
		output := outcome.outputText()
		intact, intactErr := outcome.intactResult()
		switch {
		case intactErr != nil:
			// The state check itself failed, for example because the command
			// removed the repository: still evidence against this command.
			output += "\n" + intactErr.Error()
		case !intact:
			output += "\nWorkspace or HEAD changed during this verification command"
		}
		task.Verification = append(task.Verification, model.Verification{
			Command: command, Success: intactErr == nil && intact && !failed, Output: store.Redact(output), Revision: revision, CreatedAt: model.Now(),
		})
		if err := a.saveTask(task); err != nil {
			return nil, err
		}
		if intactErr != nil {
			return nil, fmt.Errorf("Workspace state check failed during verification: %w", intactErr)
		}
		if !intact {
			return nil, model.BlockedReasonWorkspaceInvalid
		}
		if failed {
			verificationErrors = append(verificationErrors, command+": "+output)
		}
	}
	return verificationErrors, nil
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
	// The repair thread persists across rounds: the first repair starts it and
	// every later round resumes it.
	_, _, err = a.invoke(ctx, client, invocation{
		cycleID: task.CycleID, task: task, role: "repair", route: cfg.RepairRoute, workspace: task.Workspace,
		resume: task.RepairSession, keep: func(session string) { task.RepairSession = &session },
		prompt: prompt,
	})
	return err
}

// repairPrompt is a repair round's prompt: the review's findings as JSON (an
// empty list when there are none) and the failed verification output. Its
// "Repair actionable findings" prefix is matched by the e2e runner fixtures,
// and it carries the no-publication policy.
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
		debugList(cfg.VerificationCommands),
		task.ComparisonBase,
		task.Proposal.Prompt,
		string(findingsJSON),
		debugList(verificationErrors)), nil
}

func (a *App) published(task *model.Task, p model.PullRequest) error {
	task.PRNumber = &p.Number
	task.PRURL = &p.URL
	task.Error = nil
	task.BlockedReason = nil
	if err := a.transition(task, model.StatusPublished); err != nil {
		return err
	}
	return a.Store.RecordPrObservation(task.Config.GitHubRepo, p, true)
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
	return a.Store.Event(task.ID, "status", statusEventName(status))
}

// statusEventName renders the saved status event name ("Reviewing", "Published", ...).
func statusEventName(status model.Status) string {
	name := status.String()
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func (a *App) taskWorkspace(taskID string) string {
	return filepath.Join(a.DataDir, "tasks", taskID, "workspace")
}

// checkOutcome is one verification command's captured result plus the
// workspace-integrity check that follows it. intactErr carries the check's own
// failure so each caller decides whether it is evidence or fatal.
type checkOutcome struct {
	captured  *process.ProcessOutput
	capture   error
	intact    bool
	intactErr error
}

// failed reports the command-failure condition process.Run reports as an
// error: a capture failure or a nonzero exit.
func (o checkOutcome) failed() bool {
	return o.capture != nil || !o.captured.Status.Success()
}

// outputText returns the text process.Run would have returned for this
// capture: bounded diagnostic output on success, the error chain on failure.
func (o checkOutcome) outputText() string {
	if o.capture != nil {
		return o.capture.Error()
	}
	text, err := process.DiagnosticText("bash", o.captured)
	if err != nil {
		return err.Error()
	}
	return text
}

func (o checkOutcome) intactResult() (bool, error) { return o.intact, o.intactErr }

// runCheckCommand runs one `bash -o pipefail -c` verification command in ws,
// then checks the workspace still sits at revision. The integrity read is
// skipped once ctx fires: it needs a live process and could only report the
// cancellation rather than the workspace state.
func runCheckCommand(ctx context.Context, cfg config.Config, ws, command, revision string) checkOutcome {
	captured, captureErr := process.ShellCheck(ctx, command, ws, cfg.CommandTimeoutSeconds)
	outcome := checkOutcome{captured: captured, capture: captureErr}
	if ctx.Err() != nil {
		outcome.intact = false
	} else {
		outcome.intact, outcome.intactErr = gitops.At(ctx, cfg, ws, revision)
	}
	return outcome
}

func blockedReasonPtr(reason model.BlockedReason) *model.BlockedReason { return &reason }

func sourcePtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// samePath compares workspace paths by component: separators and interior "."
// are normalized; ".." stays literal.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	return strings.Join(pathIdentityComponents(a), "/") == strings.Join(pathIdentityComponents(b), "/")
}

func pathIdentityComponents(path string) []string {
	parts := []string{}
	for i, part := range strings.Split(filepath.ToSlash(path), "/") {
		if i == 0 && part == "" {
			parts = append(parts, "/")
			continue
		}
		if part == "" || part == "." {
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

// debugOption, debugList and debugString render prompt values in Rust's Debug
// notation. The service was ported from Rust and its runner fixtures match
// the prompts it produced, so the notation is kept for byte-stable prompts.

// debugOption renders a saved optional value as `Some("…")` or `None`.
func debugOption(value *string) string {
	if value == nil {
		return "None"
	}
	return "Some(" + debugString(*value) + ")"
}

// debugList renders a saved string list as `["a", "b"]`.
func debugList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = debugString(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// debugString quotes a saved string with escapes and `\u{…}` for non-printable runes.
func debugString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0:
			b.WriteString(`\0`)
		default:
			if unicode.IsPrint(r) {
				b.WriteRune(r)
			} else {
				fmt.Fprintf(&b, `\u{%x}`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
