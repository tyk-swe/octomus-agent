package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func (a *App) superviseTask(ctx context.Context, task model.Task) error {
	return a.superviseExecution(ctx, task, a.execute)
}

func (a *App) superviseExecution(ctx context.Context, task model.Task, execute func(context.Context, *model.Task) error) error {
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	limit := time.Duration(task.ExecutionConfig().TaskTimeoutSeconds) * time.Second
	result, executeErr := runJoined(ctx, workCancel, limit, "Task worker panicked", func() error {
		return execute(workCtx, &task)
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
	operatorCancelled, _ := a.Store.MarkerSet("cancel", task.ID)
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
		task.Error = stringPointer("Cancelled by the operator")
	} else {
		recordTaskError(&task, taskErr)
		if result.Expired {
			task.BlockedReason = blockedReasonPtr(model.BlockedReasonTimeout)
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
	if err := a.validateRoutes(client, cfg, false); err != nil {
		return fmt.Errorf("%w: %w", model.BlockedReasonRunnerUnavailable, err)
	}
	admissionReserved := task.ExecutionSession == nil
	if admissionReserved {
		if err := a.initializeTask(ctx, task); err != nil {
			return err
		}
	}
	ws := task.Workspace
	if _, err := workspace.GitDir(ws); err != nil {
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
		names, err := gitops.WorkGit(ctx, cfg, ws, []string{"diff", "--name-only", task.SourceRevision, revision})
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
	if task.ExecutionSession != nil && !workspace.Initialized(*task) {
		return model.BlockedReasonWorkspaceInvalid
	}
	if task.ExecutionSession == nil && task.Workspace != "" {
		return a.validateRecordedWorkspace(ctx, task)
	}
	return nil
}

func (a *App) validateRecordedWorkspace(ctx context.Context, task *model.Task) error {
	ws := a.taskWorkspace(task.ID)
	if !config.SamePath(task.Workspace, ws) || task.ComparisonBase == "" {
		return model.BlockedReasonWorkspaceInvalid
	}
	if _, err := workspace.GitDir(ws); err != nil {
		return model.BlockedReasonWorkspaceInvalid
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
		return model.BlockedReasonStaleBase
	}
	current := *remote
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
		if !slices.Contains(dependencyOutputs, current) {
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
			task.ComparisonBase, err = gitops.WorkGit(ctx, cfg, ws, []string{"merge-base", task.DefaultRevision, task.SourceRevision})
			if err != nil {
				return err
			}
		} else {
			task.ComparisonBase = task.SourceRevision
		}
		return a.saveTask(task)
	}
	return a.validateRecordedWorkspace(ctx, task)
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
			return "", fmt.Errorf("%w: Unparseable review is not clean: %s", model.BlockedReasonInvalidReview, redact.Text(err.Error()))
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
	if _, err := a.invoke(ctx, client, invocation{
		cycleID: task.CycleID, task: task, role: "reviewer", route: route, workspace: ws,
		prompt: reviewPrompt(task, revision), schema: schemas.ReviewSchema(), judge: judge,
	}); err != nil {
		return model.Review{}, err
	}
	return review, nil
}

func reviewPrompt(task *model.Task, revision string) string {
	return fmt.Sprintf(
		"Perform a fresh code review equivalent to /review of the COMPLETE change set: git diff %s HEAD. Recorded HEAD: %s. Include all accumulated PR changes and all repairs; do not only review the last commit. Task: %s. Scope: %s. Existing PR: %s. Inspect code and evidence, do not modify files. Report actionable correctness, regression, design or missing verification findings with file, priority and technical rationale. Do not invent findings. Set completed=true only after completing the review. A clean review must have an explanatory summary and zero findings.",
		task.ComparisonBase, revision, task.Proposal.Prompt, task.Proposal.Scope, debugOption(task.PRURL))
}

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
	checkout, discard, err := a.verificationCheckout(ctx, task, revision)
	if err != nil {
		return nil, err
	}
	defer discard()
	for i, command := range cfg.VerificationCommands {
		outcome := runCheckCommand(ctx, a.sandbox, cfg, checkout, command, revision, i == 0)
		if ctx.Err() != nil {
			return nil, process.ErrCancelled
		}
		failed := outcome.failed()
		note := ""
		switch {
		case outcome.intactErr != nil:
			note = "\n" + boundedTail(redact.Secrets(outcome.intactErr.Error()), verificationNoteLimit)
		case !outcome.intact:
			note = "\nWorkspace or HEAD changed during this verification command"
		}
		output := outcome.evidenceText(verificationOutputLimit-len(note)) + note
		task.Verification = append(task.Verification, model.Verification{
			Command: command, Success: outcome.intactErr == nil && outcome.intact && !failed, Output: output, Revision: revision, CreatedAt: model.Now(),
			Sandbox: outcome.sandbox,
		})
		if err := a.saveTask(task); err != nil {
			return nil, err
		}
		if outcome.intactErr != nil {
			return nil, fmt.Errorf("Workspace state check failed during verification: %w", outcome.intactErr)
		}
		if !outcome.intact {
			return nil, model.BlockedReasonWorkspaceInvalid
		}
		if failed {
			verificationErrors = append(verificationErrors, command+": "+output)
		}
	}
	return verificationErrors, nil
}

// verificationDir holds each verification run's pristine checkout inside the task's root.
const verificationDir = "verify"

// verificationCheckout clones the reviewed revision fresh for one verification run and returns a function that removes
// it. Anything a session left in the task work tree beyond the reviewed commit cannot influence the result.
func (a *App) verificationCheckout(ctx context.Context, task *model.Task, revision string) (string, func(), error) {
	taskRoot := filepath.Dir(task.Workspace)
	root := filepath.Join(taskRoot, verificationDir)
	if err := workspace.RemoveOwnedDir(taskRoot, root); err != nil {
		return "", nil, fmt.Errorf("Removing a previous verification checkout: %w", err)
	}
	checkout := filepath.Join(root, "workspace")
	if err := gitops.CloneReviewed(ctx, task.ExecutionConfig(), task.Workspace, checkout, revision); err != nil {
		_ = workspace.RemoveOwnedDir(taskRoot, root)
		return "", nil, fmt.Errorf("Preparing the verification checkout: %w", err)
	}
	return checkout, func() {
		if err := workspace.RemoveOwnedDir(taskRoot, root); err != nil {
			_ = a.Store.Event(task.ID, "cleanup_error", "verification checkout: "+redact.Error(err))
		}
	}, nil
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

type checkOutcome struct {
	captured  *process.ProcessOutput
	capture   error
	intact    bool
	intactErr error
	sandbox   *model.SandboxRecord
}

func (o checkOutcome) failed() bool {
	return o.capture != nil || !o.captured.Status.Success()
}

const (
	verificationOutputLimit = 16 * 1024
	verificationNoteLimit   = 4096
	outputTruncatedMarker   = "[output truncated]"
)

func (o checkOutcome) evidenceText(limit int) string {
	if o.capture != nil {
		return boundedTail(redact.Secrets(o.capture.Error()), limit)
	}
	clean := func(stream process.Captured) string {
		text := strings.TrimSpace(stream.SafeText())
		if !stream.Truncated {
			return text
		}
		text += "\n" + outputTruncatedMarker
		if tail := strings.TrimSpace(stream.SafeTailText()); tail != "" {
			text += "\n" + tail
		}
		return text
	}
	status := ""
	if !o.captured.Status.Success() {
		status = "\n" + o.captured.Status.String()
	}
	stdout := clean(o.captured.Stdout)
	stderr := ""
	if text := clean(o.captured.Stderr); text != "" {
		const separator = "\n[stderr]\n"
		budget := max(limit/2, limit-len(separator)-len(stdout)-len(status))
		stderr = separator + boundedTail(text, budget)
	}
	return boundedTail(stdout, limit-len(stderr)-len(status)) + stderr + status
}

func boundedTail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const prefix = outputTruncatedMarker + "\n"
	start := len(text) - max(limit-len(prefix), 0)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return prefix + text[start:]
}

func runCheckCommand(ctx context.Context, box sandbox.Backend, cfg config.Config, ws, command, revision string, fresh bool) checkOutcome {
	captured, evidence, captureErr := sandbox.Verify(ctx, box, ws, command, cfg.CommandTimeoutSeconds, fresh)
	outcome := checkOutcome{captured: captured, capture: captureErr, sandbox: evidence}
	if ctx.Err() == nil {
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

func debugOption(value *string) string {
	if value == nil {
		return "None"
	}
	return "Some(" + debugString(*value) + ")"
}

func debugList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = debugString(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

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
