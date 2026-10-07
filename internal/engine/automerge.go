package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	autoMergeInterval  = 30 * time.Second
	mergeMutationBound = 15 * time.Second
	mergePassLimit     = 8
	mergePageSize      = 8
)

var unfinishedBranchWork = func(s *store.Store, repository, branch, taskID string) (bool, error) {
	return s.UnfinishedBranchWork(repository, branch, taskID)
}

func (a *App) branchHasUnfinishedWork(repository, branch, exceptID string) (bool, error) {
	a.runtimeMu.Lock()
	running := false
	for id, job := range a.runtime.tasks {
		if id != exceptID && job.branch == branch {
			running = true
			break
		}
	}
	a.runtimeMu.Unlock()
	if running {
		return true, nil
	}
	return unfinishedBranchWork(a.Store, repository, branch, exceptID)
}

var settleMergeRecord = func(s *store.Store, repository string, number uint64, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) (bool, error) {
	return s.SettleMerge(repository, number, expected, status, reason, source, commit, revoke)
}

func (a *App) initialMergeState(task *model.Task) (*model.AutoMergeState, error) {
	if task.Config.DeliveryMode != config.DeliveryModeMaintenance {
		return nil, nil
	}
	live, err := a.Config()
	if err != nil {
		return nil, err
	}
	revision, err := live.Fingerprint()
	if err != nil {
		return nil, err
	}
	state := &model.AutoMergeState{
		TaskID: task.ID, Head: *task.OutputCommit, ComparisonBase: task.ComparisonBase,
		HeadBranch: task.Branch, BaseBranch: task.Config.DefaultBranch,
		PolicyRevision: revision, ObservedAt: model.Now(),
	}
	if task.MaintenanceFootprint != nil {
		footprint := task.MaintenanceFootprint.Clone()
		state.Footprint = &footprint
	}
	if task.MaintenanceMergeAuthorized(live) {
		state.Authorized = true
		state.Status = model.AutoMergeWaiting
		state.Reason = "Waiting for GitHub checks and protections"
	} else {
		state.Status = model.AutoMergeManual
		state.Reason = manualMergeReason(*task, live)
	}
	return state, nil
}

func (a *App) checkMerges(cfg config.Config, control model.Control) {
	a.runtimeMu.Lock()
	if a.runtime.mergeWorker != nil || a.ctx.Err() != nil {
		a.runtimeMu.Unlock()
		return
	}
	if time.Since(a.runtime.lastMergeCheck) < autoMergeInterval {
		a.runtimeMu.Unlock()
		return
	}
	cursor := a.runtime.mergeCursor
	a.runtimeMu.Unlock()
	candidates, err := a.Store.MergeCandidates(cursor, mergePageSize)
	if err == nil && len(candidates) == 0 && cursor != 0 {
		candidates, err = a.Store.MergeCandidates(0, mergePageSize)
	}
	if err != nil {
		a.setRecoveryError(err)
		return
	}
	a.runtimeMu.Lock()
	if len(candidates) == 0 {
		if cursor != 0 {
			a.runtime.mergeCursor = 0
		}
		a.runtimeMu.Unlock()
		return
	}
	a.runtime.lastMergeCheck = time.Now()
	ctx, cancel := context.WithCancel(a.ctx)
	a.runtime.mergeWorker = &mergeJob{cancel: cancel}
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		a.mergePass(ctx, control)
		a.runtimeMu.Lock()
		a.runtime.mergeWorker = nil
		a.runtimeMu.Unlock()
		a.notify()
	}()
}

func (a *App) mergePass(ctx context.Context, control model.Control) {
	cfg, err := a.Config()
	if err != nil {
		a.setRecoveryError(err)
		return
	}
	modeOff := cfg.DeliveryMode != config.DeliveryModeMaintenance
	paused := control.Paused || control.Mode == model.OperatingModePaused
	continuous := !modeOff && !paused && control.Mode == model.OperatingModeContinuous
	var batch *string
	if !modeOff && !paused && control.Mode == model.OperatingModeRunOnce && control.Batch != nil {
		batch = new(control.Batch.ID)
	}
	seen := map[int64]struct{}{}
	processed := 0
	for processed < mergePassLimit {
		if ctx.Err() != nil {
			return
		}
		a.runtimeMu.Lock()
		cursor := a.runtime.mergeCursor
		a.runtimeMu.Unlock()
		candidates, err := a.Store.MergeCandidates(cursor, mergePageSize)
		if err != nil {
			a.setRecoveryError(err)
			return
		}
		if len(candidates) == 0 {
			if cursor == 0 {
				return
			}
			a.runtimeMu.Lock()
			a.runtime.mergeCursor = 0
			a.runtimeMu.Unlock()
			continue
		}
		for _, candidate := range candidates {
			if _, dup := seen[candidate.Seq]; dup {
				a.runtimeMu.Lock()
				a.runtime.mergeCursor = 0
				a.runtimeMu.Unlock()
				return
			}
			seen[candidate.Seq] = struct{}{}
			a.runtimeMu.Lock()
			a.runtime.mergeCursor = candidate.Seq
			a.runtimeMu.Unlock()
			if ctx.Err() != nil {
				return
			}
			a.mergeCandidate(ctx, cfg, modeOff, paused, continuous, batch, candidate.Observation)
			processed++
			if processed >= mergePassLimit {
				break
			}
		}
	}
}

func (a *App) mergeCandidate(ctx context.Context, cfg config.Config, modeOff, paused, continuous bool, batch *string, observation model.PRObservation) {
	merge := observation.AutoMerge
	if merge == nil || (!merge.Status.Pending() && merge.Status != model.AutoMergeManual) {
		return
	}
	task, err := store.Get[model.Task](a.Store, "task", merge.TaskID)
	if err != nil {
		a.setRecoveryError(err)
		return
	}
	inflight := merge.Status == model.AutoMergeMerging || merge.Status == model.AutoMergeUncertain
	if task == nil {
		if inflight {
			readCfg := cfg.Clone()
			readCfg.GitHubRepo = observation.Repository
			a.reconcileMerge(ctx, readCfg, observation)
			return
		}
		a.settleMerge(observation, *merge, model.AutoMergeManual, "The authorizing task record is missing", "", nil, true)
		return
	}
	branch := task.Branch
	a.runtimeMu.Lock()
	if _, busy := a.runtime.merges[branch]; busy {
		a.runtimeMu.Unlock()
		return
	}
	a.runtime.merges[branch] = struct{}{}
	a.runtimeMu.Unlock()
	defer func() {
		a.runtimeMu.Lock()
		delete(a.runtime.merges, branch)
		a.runtimeMu.Unlock()
	}()
	if inflight {
		a.reconcileMerge(ctx, task.Config, observation)
		return
	}
	if batch != nil && (task.RunID == nil || *task.RunID != *batch) {
		return
	}
	if modeOff {
		if merge.Status == model.AutoMergeWaiting || merge.Status == model.AutoMergeManual && merge.Authorized {
			a.settleMerge(observation, *merge, model.AutoMergeManual, "The live delivery mode no longer authorizes automatic merge", "", nil, true)
		}
		return
	}
	if paused {
		return
	}
	if merge.Status == model.AutoMergeManual && !(continuous && merge.Authorized) {
		return
	}
	if reason, waiting, terminal, revoke := a.mergeBlocked(ctx, cfg, *task, *merge); terminal {
		a.reconcileMerge(ctx, task.Config, observation)
	} else if waiting {
		a.settleMerge(observation, *merge, model.AutoMergeWaiting, reason, "", nil, false)
	} else if reason != "" {
		a.settleMerge(observation, *merge, model.AutoMergeManual, reason, "", nil, revoke)
	} else {
		a.mergeAttempt(ctx, *task, observation, continuous)
	}
}

func (a *App) mergeBlocked(ctx context.Context, cfg config.Config, task model.Task, merge model.AutoMergeState) (reason string, waiting, terminal, revoke bool) {
	number := uint64(0)
	if task.PRNumber != nil {
		number = *task.PRNumber
	}
	switch {
	case task.Lifecycle.ArchivedAt != nil:
		return "The authorizing task was archived", false, false, true
	case !strings.EqualFold(task.Config.GitHubRepo, cfg.GitHubRepo):
		return "The repository configuration changed; the recorded authority no longer applies", false, false, true
	case merge.HeadBranch == "" || merge.BaseBranch == "" ||
		merge.HeadBranch != task.Branch || merge.BaseBranch != task.Config.DefaultBranch:
		return "The recorded merge intent predates immutable branch and base binding", false, false, true
	case task.Status != model.StatusPublished || task.OutputCommit == nil || *task.OutputCommit != merge.Head ||
		task.ComparisonBase != merge.ComparisonBase:
		return "The recorded delivery evidence no longer matches the authorized head", false, false, true
	case !task.MaintenanceMergeAuthorized(cfg):
		return manualMergeReason(task, cfg), false, false, true
	}
	if unfinished, err := a.branchHasUnfinishedWork(cfg.GitHubRepo, task.Branch, task.ID); err != nil {
		return "Waiting for the same-branch work check: " + redact.Error(err), true, false, false
	} else if unfinished {
		return "Waiting for same-branch work to finish", true, false, false
	}
	pr, err := gitops.PR(ctx, cfg, number)
	if err != nil {
		return "Waiting for a readable pull request: " + redact.Error(err), true, false, false
	}
	if pr.State == "merged" || pr.State == "closed" {
		return "", false, true, false
	}
	if !pr.OwnedOpen() || pr.Branch != merge.HeadBranch || pr.Base != merge.BaseBranch || pr.Head != merge.Head {
		return "The pull request changed; the recorded authority no longer applies", false, false, true
	}
	marker, err := gitops.TaskMarker(ctx, cfg, task.ID, pr)
	if err != nil {
		return "Waiting for a readable task marker: " + redact.Error(err), true, false, false
	}
	if !marker {
		return "The pull request does not carry this task's delivery marker", false, false, true
	}
	status, err := gitops.MaintenanceMergeStatus(ctx, cfg, number)
	if err != nil {
		return "Waiting for merge status: " + redact.Error(err), true, false, false
	}
	if status.State == "merged" || status.State == "closed" {
		return "", false, true, false
	}
	if status.Head != merge.Head || status.HeadBranch != merge.HeadBranch || status.BaseBranch != merge.BaseBranch {
		return "The pull request head, branch or base changed", false, false, true
	}
	if reason, waiting := mergeReadiness(task, cfg, status); reason != "" {
		return reason, waiting, false, false
	}
	base, err := mergeBaseRevision(ctx, cfg, status, merge)
	if err != nil {
		return "Waiting for the trusted merge base: " + redact.Error(err), true, false, false
	}
	if base != merge.ComparisonBase {
		return "The base moved since review; a fresh reviewed delivery is required", false, false, true
	}
	return "", false, false, false
}

func mergeReadiness(task model.Task, cfg config.Config, status gitops.MergeStatus) (reason string, waiting bool) {
	if status.Draft {
		return "The pull request is a draft", false
	}
	if status.MergeQueue {
		return "The repository requires a merge queue", false
	}
	if !status.SquashAllowed {
		return "The repository does not allow squash merging", false
	}
	if status.ReviewDecision != nil {
		switch *status.ReviewDecision {
		case "APPROVED":
		case "REVIEW_REQUIRED":
			return "Waiting for a required pull request review", true
		default:
			return "The pull request review requested changes", false
		}
	}
	if status.CheckState == nil {
		return "Waiting for check evidence on the reviewed head", true
	}
	switch *status.CheckState {
	case "SUCCESS":
		if status.CheckContexts == 0 {
			return "Waiting for check evidence on the reviewed head", true
		}
	case "EXPECTED", "PENDING":
		return "Waiting for checks to complete on the reviewed head", true
	default:
		return "Checks failed on the reviewed head", false
	}
	if status.Mergeable == "CONFLICTING" {
		return "The pull request has conflicts", false
	}
	if status.Mergeable != "MERGEABLE" {
		return "Waiting for GitHub to report mergeability", true
	}
	switch status.MergeState {
	case "CLEAN":
	case "UNSTABLE":
		return "Waiting for checks to complete on the reviewed head", true
	default:
		return "GitHub protections block merging: " + strings.ToLower(status.MergeState), false
	}
	if status.StatsMalformed {
		return "GitHub reported malformed footprint statistics", false
	}
	if status.Additions == nil || status.Deletions == nil || status.ChangedFiles == nil {
		return "GitHub did not report the pull request footprint", false
	}
	if !gitops.RemoteFootprintWithin(*status.Additions, *status.Deletions, *status.ChangedFiles, task.Config, cfg) {
		return "The full pull request footprint exceeds the automatic merge limits", false
	}
	return "", false
}

func mergeBaseRevision(ctx context.Context, cfg config.Config, status gitops.MergeStatus, merge model.AutoMergeState) (string, error) {
	if err := gitops.Fetch(ctx, cfg); err != nil {
		return "", err
	}
	return gitops.MergeBase(ctx, cfg, status.BaseOID, merge.Head)
}

func (a *App) reconcileMerge(ctx context.Context, cfg config.Config, observation model.PRObservation) {
	merge := observation.AutoMerge
	number := observation.PR.Number
	status, err := gitops.MaintenanceMergeStatus(ctx, cfg, number)
	if err != nil {
		a.settleMerge(observation, *merge, merge.Status, "Merge result is unconfirmed; remote status is unreadable: "+redact.Error(err), "", nil, false)
		return
	}
	bound := status.Head == merge.Head && status.HeadBranch == merge.HeadBranch &&
		status.BaseBranch == merge.BaseBranch && strings.EqualFold(status.Repository, observation.Repository)
	switch {
	case status.State == "merged" && bound:
		a.settleMerge(observation, *merge, model.AutoMergeMerged, "The pull request is merged on the remote", store.MergeResultObserved, status.MergeCommit, false)
	case status.State == "merged":
		a.settleMerge(observation, *merge, model.AutoMergeManual, "The pull request merged after its head, branch or base changed; the outcome does not apply to the recorded reviewed head", "", nil, true)
	case status.State == "closed" && bound:
		a.settleMerge(observation, *merge, model.AutoMergeClosed, "The pull request was closed without merging", store.MergeResultObserved, nil, false)
	case status.State == "closed":
		a.settleMerge(observation, *merge, model.AutoMergeManual, "The pull request closed after its head, branch or base changed; the outcome does not apply to the recorded reviewed head", "", nil, true)
	case bound && status.State == "open":
		a.settleMerge(observation, *merge, model.AutoMergeWaiting, "The merge attempt was interrupted; fresh checks are required", "", nil, false)
	default:
		a.settleMerge(observation, *merge, model.AutoMergeManual, "The pull request head changed during the merge attempt; authority is revoked", "", nil, true)
	}
}

func (a *App) mergeAttempt(ctx context.Context, task model.Task, observation model.PRObservation, allowManual bool) {
	merge := observation.AutoMerge
	number := observation.PR.Number
	attemptID := model.ID()
	a.gate.Lock()
	locked := true
	unlock := func() {
		if locked {
			a.gate.Unlock()
			locked = false
		}
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return
	}
	control, err := a.Control()
	if err != nil {
		return
	}
	live, err := a.Config()
	if err != nil {
		return
	}
	if a.recoveryConflict() != nil {
		return
	}
	current, err := store.Get[model.Task](a.Store, "task", merge.TaskID)
	if err != nil || current == nil {
		return
	}
	if control.Paused ||
		(control.Mode != model.OperatingModeContinuous && control.Mode != model.OperatingModeRunOnce) ||
		live.DeliveryMode != config.DeliveryModeMaintenance || !current.MaintenanceMergeAuthorized(live) {
		return
	}
	if control.Mode == model.OperatingModeRunOnce &&
		(control.Batch == nil || current.RunID == nil || *current.RunID != control.Batch.ID) {
		return
	}
	if current.Status != model.StatusPublished || current.Lifecycle.ArchivedAt != nil ||
		current.OutputCommit == nil || *current.OutputCommit != merge.Head ||
		current.ComparisonBase != merge.ComparisonBase || current.PRNumber == nil ||
		*current.PRNumber != number || merge.HeadBranch == "" || merge.BaseBranch == "" ||
		merge.HeadBranch != current.Branch || merge.BaseBranch != current.Config.DefaultBranch ||
		current.Branch != observation.PR.Branch || observation.PR.Base != merge.BaseBranch ||
		!strings.EqualFold(current.Config.GitHubRepo, observation.Repository) {
		return
	}
	cancelled, err := a.Store.Marked("cancel", merge.TaskID)
	if err != nil || cancelled {
		return
	}
	if unfinished, err := a.branchHasUnfinishedWork(live.GitHubRepo, current.Branch, current.ID); err != nil || unfinished {
		return
	}
	intent := merge.Clone()
	intent.Status = model.AutoMergeMerging
	intent.AttemptID = new(attemptID)
	intent.AttemptedAt = new(model.Now())
	intent.Reason = "Squash merge in flight"
	intent.ObservedAt = model.Now()
	claimed, err := a.Store.ClaimMerge(observation.Repository, number, intent, allowManual)
	if err != nil || !claimed {
		return
	}
	expected := merge.Clone()
	expected.Status = model.AutoMergeMerging
	expected.AttemptID = new(attemptID)
	mutCtx, cancel := context.WithTimeout(ctx, mergeMutationBound)
	bounded := live
	bounded.CommandTimeoutSeconds = min(bounded.CommandTimeoutSeconds, uint64(mergeMutationBound/time.Second))
	commit, mutateErr := gitops.SquashMerge(mutCtx, bounded, number, merge.Head)
	cancel()
	unlock()
	if mutateErr == nil {
		a.mergeConfirmed(current, observation, expected, commit)
		return
	}
	if refusal := mergeRefusal(mutateErr); refusal != "" {
		a.settleMerge(observation, expected, model.AutoMergeManual, refusal, "", nil, true)
		return
	}
	a.settleMerge(observation, expected, model.AutoMergeUncertain, "The merge request outcome is unconfirmed: "+redact.Error(mutateErr), "", nil, false)
}

func mergeRefusal(err error) string {
	text := err.Error()
	for _, marker := range []string{"HTTP 403", "HTTP 404", "HTTP 405", "HTTP 422"} {
		if strings.Contains(text, marker) {
			return "GitHub refused the merge request: " + redact.Error(err)
		}
	}
	return ""
}

func mergeBarrierKey(repository string, number uint64) string {
	return strings.ToLower(repository) + ":" + strconv.FormatUint(number, 10)
}

func (a *App) setMergeRecoveryError(key string, err error) {
	message := strings.Clone(redact.Error(err))
	a.runtimeMu.Lock()
	a.runtime.mergeRecoveryErrors[key] = message
	a.runtimeMu.Unlock()
}

func (a *App) clearMergeRecoveryError(key string) {
	a.runtimeMu.Lock()
	delete(a.runtime.mergeRecoveryErrors, key)
	a.runtimeMu.Unlock()
}

func (a *App) mergeConfirmed(task *model.Task, observation model.PRObservation, expected model.AutoMergeState, commit string) {
	if ok, err := settleMergeRecord(a.Store, observation.Repository, observation.PR.Number, expected, model.AutoMergeMerged, "Squash merged by Octomus", store.MergeResultConfirmed, &commit, false); err != nil || !ok {
		if err == nil {
			err = errors.New("The confirmed merge result could not be recorded")
		}
		a.setMergeRecoveryError(mergeBarrierKey(observation.Repository, observation.PR.Number), err)
		return
	}
	a.clearMergeRecoveryError(mergeBarrierKey(observation.Repository, observation.PR.Number))
	_ = a.Store.Event(task.ID, "automerge", fmt.Sprintf("Squash merged pull request %d at %s", observation.PR.Number, commit))
	live, err := a.Config()
	if err != nil {
		return
	}
	a.startPRRefresh(live)
	if revision, revErr := gitops.RemoteRevision(a.ctx, live, live.DefaultBranch); revErr == nil && revision != nil {
		_ = a.observeDefaultBranch(live, *revision, model.Now())
	}
	a.notify()
}

func (a *App) settleMerge(observation model.PRObservation, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) {
	key := mergeBarrierKey(observation.Repository, observation.PR.Number)
	applied, err := settleMergeRecord(a.Store, observation.Repository, observation.PR.Number, expected, status, reason, source, commit, revoke)
	if err != nil {
		a.setMergeRecoveryError(key, err)
		return
	}
	if applied {
		a.clearMergeRecoveryError(key)
	}
	if applied && status != model.AutoMergeWaiting {
		_ = a.Store.Event(expected.TaskID, "automerge", fmt.Sprintf("Automatic merge: %s — %s", status, reason))
		a.notify()
	}
}

func manualMergeReason(task model.Task, live config.Config) string {
	footprint := task.MaintenanceFootprint
	switch {
	case footprint == nil:
		return "No trusted footprint evidence is recorded"
	case !footprint.Complete:
		return "The trusted footprint measurement is incomplete"
	case len(footprint.ManualReasons) > 0:
		return footprint.ManualReasons[0]
	case footprint.ChangedLines == nil || footprint.ChangedFiles == nil:
		return "The trusted footprint counts are unknown"
	case len(task.Reviews) == 0 || task.Reviews[len(task.Reviews)-1].Maintenance == nil:
		return "No maintenance assessment is recorded"
	case !task.Reviews[len(task.Reviews)-1].Maintenance.Qualifies:
		return "The delivery was not assessed as maintenance"
	case task.Reviews[len(task.Reviews)-1].Maintenance.ManualMergeRequired:
		return "Sensitive changes require a manual merge"
	case !task.Reviews[len(task.Reviews)-1].TrustedDiffComplete:
		return "The complete trusted diff did not fit the review"
	case *footprint.ChangedLines > min(task.Config.AutoMergeMaxLines, live.AutoMergeMaxLines) ||
		*footprint.ChangedFiles > min(task.Config.AutoMergeMaxFiles, live.AutoMergeMaxFiles):
		return "The full pull request footprint exceeds the automatic merge limits"
	case slices.ContainsFunc(footprint.Paths, func(name string) bool { return config.ManualMergePath(name, live.AutoMergeExcludedPaths) }):
		return "A changed path requires manual merge under the saved or live exclusions"
	default:
		return "Automatic merge prerequisites are not recorded"
	}
}
