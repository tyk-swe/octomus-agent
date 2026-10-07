package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

type roleOutcome struct {
	answer string
	err    error
}

const interruptedMsg = "Discovery interrupted; incomplete proposals were not dispatched"

func (a *App) planCycle(ctx context.Context, cfg config.Config, cycle model.Cycle) {
	err := a.plan(ctx, cfg, &cycle)
	shuttingDown := err != nil && a.ctx.Err() != nil
	var terminalErr error
	if err != nil {
		cycle.Status = model.CycleFailed
		cycle.Error = new(redact.Error(err))
		if shuttingDown {
			cycle.Status = model.CycleInterrupted
			cycle.Error = new(interruptedMsg)
		}
		cycle.CompletedAt = new(model.Now())
		terminalErr = a.saveCycle(&cycle)
	}

	a.gate.Lock()
	// A refused terminal checkpoint leaves recovery work behind. Establish its
	// admission barrier before releasing worker ownership, without waiting for tick.
	if terminalErr != nil {
		a.setRecoveryError(terminalErr)
	}
	a.runtimeMu.Lock()
	if a.runtime.cycle != nil && a.runtime.cycle.id == cycle.ID {
		a.runtime.cycle = nil
	}
	a.runtimeMu.Unlock()
	control, loadErr := a.Control()
	if loadErr != nil {
		// The saved control may still require run-once settlement. Keep admission
		// blocked until recovery can inspect it, even when the cycle write succeeded.
		a.setRecoveryError(loadErr)
	} else if !shuttingDown {
		var message string
		if err != nil {
			message = redact.Error(err)
			control.Error = &message
		} else {
			control.Error = nil
		}
		if cycle.Mode == model.CycleModeExecution {
			delay := idleDelay(cfg.CycleIntervalSeconds, control.IdleStreak)
			control.NextCycleAt = time.Now().Unix() + int64(delay)
		}
		failedRunOnce := err != nil && cycle.Mode == model.CycleModeExecution && control.Mode == model.OperatingModeRunOnce
		if failedRunOnce {
			if pauseErr := a.pauseLocked(&control, &message); pauseErr != nil {
				a.setRecoveryError(pauseErr)
			}
		} else {
			_ = a.Store.SaveControl(control)
		}
		if err != nil {
			_ = a.Store.Event(cycle.ID, "planning_error", message)
		}
	}
	a.gate.Unlock()
	a.notify()
}

func (a *App) plan(ctx context.Context, cfg config.Config, cycle *model.Cycle) error {
	inventory, err := a.captureGrounding(ctx, cfg, cycle)
	if err != nil {
		return err
	}
	memory, err := a.planningMemory(ctx, cfg, *cycle.Grounding)
	if err != nil {
		return err
	}
	if cycle.Mode == model.CycleModeExecution {
		if err := a.seedRediscoveries(cycle, memory.requests); err != nil {
			return err
		}
	}
	reservations, err := a.Store.PRReservations(cfg.GitHubRepo)
	if err != nil {
		return err
	}
	capacity, _ := capacityOf(cfg, inventory, reservations)
	decisions, err := memory.promptEntries()
	if err != nil {
		return err
	}
	contextBytes, err := wirejson.Marshal(map[string]any{"grounding": cycle.Grounding, "decision_memory": decisions, "pr_capacity": capacity})
	if err != nil {
		return err
	}
	ground, err := a.summarizeGrounding(ctx, cfg, cycle, string(contextBytes))
	if err != nil {
		return err
	}
	if err := a.discover(ctx, cfg, cycle, ground, string(contextBytes)); err != nil {
		return err
	}
	if err := a.reviewProposals(ctx, cfg, cycle, ground, string(contextBytes)); err != nil {
		return err
	}
	proposals, err := a.consolidate(ctx, cfg, cycle, ground, string(contextBytes))
	if err != nil {
		return err
	}
	for i := range proposals {
		proposals[i].ProblemKey = proposals[i].ProblemIdentity()
	}
	history, err := a.Store.DuplicateTasks(cfg.GitHubRepo, proposals)
	if err != nil {
		return err
	}
	if err := validateProposals(cfg, proposals, *cycle.Grounding, history); err != nil {
		return err
	}
	if err := validateDecisionMemory(proposals, memory); err != nil {
		return err
	}
	if cycle.Mode == model.CycleModeExecution {
		if err := checkRediscoveries(memory.requests, proposals); err != nil {
			return err
		}
	}
	cycle.Proposals = proposals
	cycle.DecisionMemory, err = a.recordDecisions(ctx, cfg, *cycle)
	if err != nil {
		return err
	}
	if cycle.Mode == model.CycleModeAudit {
		cycle.Status = plannedStatus(proposals)
		cycle.CompletedAt = new(model.Now())
		return a.commitPlan(*cycle, nil)
	}
	return a.commitTasks(cfg, cycle)
}

func (a *App) seedRediscoveries(cycle *model.Cycle, requests []rediscoveryRequest) error {
	for _, request := range requests {
		id := request.ID
		if id == "" {
			return errors.New("Missing rediscovery identity")
		}
		old, err := store.Get[model.Task](a.Store, "task", id)
		if err != nil || old == nil {
			if err == nil {
				err = fmt.Errorf("Missing rediscovery task %s", id)
			}
			return err
		}
		proposal := old.Proposal.Clone()
		proposal.ID = "rediscover-" + old.ID
		proposal.Dependencies = []string{}
		proposal.Reconsiders = []string{old.ID}
		proposal.Decision = model.DecisionCandidate
		proposal.Reason = "Operator requested fresh assessment against current context"
		cycle.Proposals = append(cycle.Proposals, proposal)
	}
	return nil
}

func (a *App) commitPlan(cycle model.Cycle, tasks []model.Task) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	if err := a.ctx.Err(); err != nil {
		return err
	}
	return a.Store.CommitPlan(a.ctx, cycle, tasks)
}

func (a *App) captureGrounding(ctx context.Context, cfg config.Config, cycle *model.Cycle) (model.OpenPRInventory, error) {
	if err := a.preflight(ctx, cfg, cycle.Mode == model.CycleModeAudit); err != nil {
		return model.OpenPRInventory{}, err
	}
	revision, observedAt, err := a.defaultBranchSHA(ctx, cfg)
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	if err := a.observeDefaultBranch(cfg, revision, observedAt); err != nil {
		return model.OpenPRInventory{}, err
	}
	observed, err := a.observeOpenPRs(ctx, cfg)
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	if err := gitops.Fetch(ctx, cfg); err != nil {
		return model.OpenPRInventory{}, err
	}
	external, coverage, err := externalContext(observed.inventory)
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	forks := []uint64{}
	for _, pr := range external {
		if !config.EqualASCII(pr.HeadRepository, cfg.GitHubRepo) {
			forks = append(forks, pr.Number)
		}
	}
	missing, err := gitops.FetchForkHeads(ctx, cfg, forks)
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	if len(missing) > 0 {
		_ = a.Store.Event(cycle.ID, "grounding", fmt.Sprintf("Fork PR heads unavailable locally: %v", missing))
	}
	limit := 100
	history, err := a.Store.HistoryPage("task", store.HistoryQuery{Limit: &limit})
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	targets := []string{}
	now := time.Now()
	for _, pr := range observed.owned {
		if pr.OwnedOpen() && (pr.ChangedLines >= cfg.LargePRLines || prAgeReached(pr.CreatedAt, cfg.LongLivedPRDays, now)) {
			targets = append(targets, pr.Branch)
		}
	}
	sort.Strings(targets)
	grounding := model.Grounding{
		Revision:           revision,
		PRs:                observed.owned,
		ExternalPRs:        external,
		PRCoverage:         coverage,
		History:            history.Items,
		MaintenanceDue:     cfg.DeliveryMode == config.DeliveryModeMaintenance || cycle.Number%cfg.MaintenanceEveryCycles == 0,
		MaintenanceTargets: targets,
	}

	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	liveFingerprint, err := live.Fingerprint()
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	snapshotFingerprint, err := cfg.Fingerprint()
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	if liveFingerprint != snapshotFingerprint {
		return model.OpenPRInventory{}, errors.New("Configuration changed during planning grounding")
	}
	if _, err := a.savePRsLocked(cfg, observed); err != nil {
		return model.OpenPRInventory{}, err
	}
	cycle.Grounding = &grounding
	if err := a.saveCycle(cycle); err != nil {
		return model.OpenPRInventory{}, err
	}
	return observed.inventory, nil
}

func prAgeReached(createdAt string, threshold uint64, now time.Time) bool {
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil || created.After(now) {
		return false
	}
	elapsedSeconds := now.Unix() - created.Unix()
	if now.Nanosecond() < created.Nanosecond() {
		elapsedSeconds--
	}
	return uint64(elapsedSeconds/int64((24*time.Hour)/time.Second)) >= threshold
}

func (a *App) summarizeGrounding(ctx context.Context, cfg config.Config, cycle *model.Cycle, recorded string) (string, error) {
	outcome := a.role(ctx, cfg, cycle.ID, cycle.Grounding.Revision, "grounding", "orchestrator", groundingPrompt+maintenancePolicy(cfg)+recorded, schemas.GroundingSchema())
	if err := a.attachOutcomes(cycle, []roleOutcome{outcome}); err != nil {
		return "", err
	}
	var document model.GroundingDocument
	if err := json.Unmarshal([]byte(outcome.answer), &document); err != nil {
		return "", err
	}
	if strings.TrimSpace(document.Context) == "" {
		return "", errors.New("Grounding returned an empty context")
	}
	return document.Context, nil
}

func (a *App) discover(ctx context.Context, cfg config.Config, cycle *model.Cycle, ground, recorded string) error {
	scopes := discoveryScopes
	if cfg.DeliveryMode == config.DeliveryModeMaintenance {
		scopes = maintenanceDiscoveryScopes
	}
	if cfg.DiscoveryAgents > uint64(len(scopes)) {
		return fmt.Errorf("Discovery supports at most %d agents", len(scopes))
	}
	cycleID, revision := cycle.ID, cycle.Grounding.Revision
	perAgent := discoveryProposalLimit(len(cycle.Proposals), cfg.DiscoveryAgents)
	reconsiders := discoveryReconsidersAudit
	if cycle.Mode == model.CycleModeExecution {
		reconsiders = discoveryReconsidersExecution
	}
	outcomes := runRoles(int(cfg.DiscoveryAgents), func(i int) roleOutcome {
		return a.role(ctx, cfg, cycleID, revision, fmt.Sprintf("discovery-%d", i), "discovery", discoveryPrompt(cfg, cycle, i, perAgent, reconsiders, ground, recorded), schemas.ProposalSchema())
	})
	if err := a.attachOutcomes(cycle, outcomes); err != nil {
		return err
	}
	for _, outcome := range outcomes {
		var document model.ProposalDocument
		if err := json.Unmarshal([]byte(outcome.answer), &document); err != nil {
			return err
		}
		cycle.Proposals = append(cycle.Proposals, document.Proposals...)
	}
	if len(cycle.Proposals) > maxPlanningProposals {
		return fmt.Errorf("Discovery returned too many proposals: %d candidates (limit %d)", len(cycle.Proposals), maxPlanningProposals)
	}
	identities := make(map[string]struct{}, len(cycle.Proposals))
	for _, proposal := range cycle.Proposals {
		if strings.TrimSpace(proposal.ID) == "" {
			return errors.New("Discovery returned an empty proposal identity")
		}
		if _, duplicate := identities[proposal.ID]; duplicate {
			return fmt.Errorf("Discovery returned a duplicate proposal identity %q", proposal.ID)
		}
		identities[proposal.ID] = struct{}{}
	}
	return a.saveCycle(cycle)
}

func (a *App) reviewProposals(ctx context.Context, cfg config.Config, cycle *model.Cycle, ground, recorded string) error {
	candidates, err := wirejson.Marshal(cycle.Proposals)
	if err != nil {
		return err
	}
	slots := model.ReviewerSlots()
	prompts := make([]string, len(slots))
	for i, slot := range slots {
		focus, ok := reviewFocus[slot]
		if !ok {
			return fmt.Errorf("Missing review focus for %s", slot)
		}
		prompts[i] = fmt.Sprintf("%s%s Candidates: %s. Grounding: %s. Context: %s", focus, maintenancePolicy(cfg), candidates, ground, recorded)
	}
	cycleID, revision := cycle.ID, cycle.Grounding.Revision
	outcomes := runRoles(len(slots), func(i int) roleOutcome {
		return a.role(ctx, cfg, cycleID, revision, slots[i], "proposal_reviewer", prompts[i], schemas.AssessmentSchema())
	})
	if err := a.attachOutcomes(cycle, outcomes); err != nil {
		return err
	}
	for i, outcome := range outcomes {
		var document model.AssessmentDocument
		if err := json.Unmarshal([]byte(outcome.answer), &document); err != nil {
			return err
		}
		if err := checkAssessments(slots[i], cycle.Proposals, document.Assessments); err != nil {
			return err
		}
		cycle.Assessments = append(cycle.Assessments, document)
	}
	return a.saveCycle(cycle)
}

func (a *App) consolidate(ctx context.Context, cfg config.Config, cycle *model.Cycle, ground, recorded string) ([]model.Proposal, error) {
	candidates, err := wirejson.Marshal(cycle.Proposals)
	if err != nil {
		return nil, err
	}
	reviews, err := wirejson.Marshal(cycle.Assessments)
	if err != nil {
		return nil, err
	}
	tiers, err := wirejson.Marshal(cfg.Tiers)
	if err != nil {
		return nil, err
	}
	rediscovery := rediscoveryAudit
	if cycle.Mode == model.CycleModeExecution {
		rediscovery = rediscoveryExecution
	}
	outcome := a.role(ctx, cfg, cycle.ID, cycle.Grounding.Revision, "consolidation", "orchestrator", consolidationPrompt(cfg, rediscovery, tiers, candidates, reviews, ground, recorded), schemas.ProposalSchema())
	if err := a.attachOutcomes(cycle, []roleOutcome{outcome}); err != nil {
		return nil, err
	}
	var document model.ProposalDocument
	if err := json.Unmarshal([]byte(outcome.answer), &document); err != nil {
		return nil, err
	}
	if err := checkConsolidation(cycle.Proposals, document.Proposals); err != nil {
		return nil, err
	}
	return document.Proposals, nil
}

func (a *App) role(ctx context.Context, cfg config.Config, cycleID, revision, label, role, prompt string, schema schemas.Schema) (outcome roleOutcome) {
	defer func() {
		if panicked := recover(); panicked != nil {
			outcome.answer = ""
			outcome.err = fmt.Errorf("Planning role %s panicked: %v", label, panicked)
		}
	}()
	route, ok := cfg.Roles[role]
	if !ok {
		outcome.err = fmt.Errorf("Missing %s route", role)
		return outcome
	}
	roleRoot := filepath.Join(a.dataDir, "cycles", cycleID, label)
	roleWorkspace := filepath.Join(roleRoot, "workspace")
	outcome.answer, outcome.err = a.invoke(ctx, a.runners(ctx, cfg, cycleID), invocation{
		cycleID: cycleID, role: label, route: route, workspace: roleWorkspace,
		prompt: prompt, schema: schema, ownsClients: true,
		prepare: func() error {
			return a.withPlanningWorkspace(ctx, func() error {
				return gitops.CloneAt(ctx, cfg, roleWorkspace, revision)
			})
		},
		judge: func(_, answer string) (string, error) {
			var unchanged bool
			err := a.withPlanningWorkspace(ctx, func() error {
				var err error
				unchanged, err = gitops.At(ctx, cfg, roleWorkspace, revision)
				return err
			})
			if err != nil {
				return "", err
			}
			if !unchanged {
				return "", errors.New("Planning session modified its source snapshot")
			}
			return answer, nil
		},
	})
	if outcome.err == nil {
		if err := a.removePlanningWorkspace(roleRoot); err != nil {
			_ = a.Store.Event(cycleID, "cleanup_error", fmt.Sprintf("%s: %s", label, redact.Error(err)))
		}
	}
	return outcome
}

// Independent role setup and status checks can overlap, but their transient git metadata must not race an admission
// scan of the same cycle. This only coordinates trusted host work; untrusted runners remain subject to Measure's
// bounded, fail-closed traversal. Check cancellation after waiting before starting any new filesystem work.
func (a *App) withPlanningWorkspace(ctx context.Context, work func() error) error {
	a.fsLock.RLock()
	defer a.fsLock.RUnlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Operation cancelled: %w", err)
	}
	return work()
}

func (a *App) removePlanningWorkspace(roleRoot string) error {
	a.fsLock.RLock()
	defer a.fsLock.RUnlock()
	// Completed, already-owned cleanup must finish even if shutdown cancelled the role while this lock was queued.
	return workspace.RemoveOwnedDir(filepath.Dir(roleRoot), roleRoot)
}

func runRoles(n int, run func(i int) roleOutcome) []roleOutcome {
	outcomes := make([]roleOutcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { outcomes[i] = run(i) })
	}
	wg.Wait()
	return outcomes
}

func (a *App) attachOutcomes(cycle *model.Cycle, outcomes []roleOutcome) error {
	var first error
	for _, outcome := range outcomes {
		if outcome.err != nil && first == nil {
			first = outcome.err
		}
	}
	if first != nil {
		return first
	}
	return a.saveCycle(cycle)
}

// saveCycle writes the cycle with the sessions the store holds, which running roles append as they finish.
func (a *App) saveCycle(cycle *model.Cycle) error {
	current, err := store.Get[model.Cycle](a.Store, "cycle", cycle.ID)
	if err != nil {
		return err
	}
	if current != nil {
		cycle.Sessions = current.Sessions
	}
	return a.Store.Put("cycle", cycle.ID, *cycle)
}

func (a *App) commitTasks(cfg config.Config, cycle *model.Cycle) error {
	if cycle.Grounding == nil {
		return errors.New("Planning cycle is missing its grounding")
	}
	ids := map[string]string{}
	for _, proposal := range cycle.Proposals {
		if proposal.Decision == model.DecisionAccepted {
			ids[proposal.ID] = model.ID()
		}
	}
	planned := []model.Task{}
	for _, original := range cycle.Proposals {
		if original.Decision != model.DecisionAccepted {
			continue
		}
		proposal := original.Clone()
		for i, dependency := range proposal.Dependencies {
			id, ok := ids[dependency]
			if !ok {
				return errors.New("Accepted dependency identity vanished")
			}
			proposal.Dependencies[i] = id
		}
		taskID := ids[original.ID]
		target, err := resolveTarget(cfg, cycle.Grounding.PRs, original.Target)
		if err != nil {
			return err
		}
		planned = append(planned, newPlannedTask(cfg, cycle, original, proposal, taskID, target))
	}
	cycle.Status = plannedStatus(cycle.Proposals)
	cycle.CompletedAt = new(model.Now())
	return a.commitPlan(*cycle, planned)
}

func newPlannedTask(cfg config.Config, cycle *model.Cycle, original, proposal model.Proposal, taskID string, target *model.PullRequest) model.Task {
	runID := cycle.RunID
	if runID != nil {
		runID = new(*runID)
	}
	source := cycle.Grounding.Revision
	branch := cfg.BranchPrefix + taskID
	var number *uint64
	var url *string
	if target != nil {
		source = target.Head
		branch = target.Branch
		n := target.Number
		u := target.URL
		number, url = &n, &u
	}
	now := model.Now()
	policy := model.PolicyOf(cfg)
	return model.Task{
		ID:              taskID,
		CycleID:         cycle.ID,
		Proposal:        proposal,
		Status:          model.StatusQueued,
		Route:           cfg.Tiers[original.Tier].Clone(),
		Config:          cfg.Clone(),
		SourceRevision:  source,
		ComparisonBase:  "",
		DefaultRevision: cycle.Grounding.Revision,
		Branch:          branch,
		Workspace:       "",
		Sessions:        []model.Session{},
		Reviews:         []model.ReviewRound{},
		Verification:    []model.Verification{},
		PRNumber:        number,
		PRURL:           url,
		CreatedAt:       now,
		UpdatedAt:       now,
		AttemptPolicy:   &policy,
		RunID:           runID,
		SupersededBy:    []string{},
		Supersedes:      append([]string(nil), original.Reconsiders...),
	}
}
