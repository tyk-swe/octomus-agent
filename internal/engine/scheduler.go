package engine

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// IdleDelay applies bounded exponential backoff after repeated empty plans.
func IdleDelay(base uint64, streak uint32) uint64 {
	ceiling := uint64(86400)
	if base > ceiling {
		ceiling = base
	}
	if streak <= 1 {
		return base
	}
	shift := streak - 1
	if shift > 16 {
		shift = 16
	}
	if base > math.MaxUint64>>shift {
		return ceiling
	}
	delay := base << shift
	if delay > ceiling {
		return ceiling
	}
	return delay
}

// Tick performs one short authoritative scheduling pass. Remote preflights,
// planning, PR refreshes, and task work are launched after their durable
// eligibility checks and execute outside gate.
func (a *App) Tick() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	if a.ctx.Err() != nil {
		return nil
	}
	control, err := a.Control()
	if err != nil {
		return err
	}
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	a.maybeStartHousekeeping(cfg)
	// Reconciliation can write a preserved PR branch. Reserve publication while
	// leaving the gate available to pause and other operator controls.
	a.runtimeMu.Lock()
	reconciling := a.runtime.reconcilingPublication
	baseline := a.runtime.baseline != nil
	a.runtimeMu.Unlock()
	if reconciling {
		return nil
	}
	// A running baseline check owns the whole service: no cycle, task dispatch
	// or batch completion may proceed until it finishes.
	if baseline {
		return nil
	}
	if control.Mode == model.OperatingModePaused {
		return nil
	}
	if err := cfg.Validate(true); err != nil {
		return err
	}
	a.runtimeMu.Lock()
	planning := a.runtime.cycle != nil || a.runtime.preflight
	a.runtimeMu.Unlock()
	if planning {
		return nil
	}

	var runID *string
	if control.Mode == model.OperatingModeRunOnce {
		if control.Batch == nil {
			return errors.New("Run once is missing its durable batch")
		}
		id := control.Batch.ID
		runID = &id
		pending, unresolved, err := a.Store.BatchCounts(id)
		if err != nil {
			return err
		}
		if pending == 0 && (control.Batch.Phase == model.BatchPhaseExecuting || unresolved > 0) {
			return a.finishRunOnce(control, unresolved)
		}
	}

	tasks, err := a.Store.SchedulingTasks(runID)
	if err != nil {
		return err
	}
	visibleCycles := map[string]struct{}{}
	for _, task := range tasks {
		visibleCycles[task.CycleID] = struct{}{}
	}
	a.runtimeMu.Lock()
	for cycleID := range a.runtime.checkedCycles {
		if _, visible := visibleCycles[cycleID]; !visible {
			delete(a.runtime.checkedCycles, cycleID)
		}
	}
	a.runtimeMu.Unlock()
	if err := a.validateQueuedCycles(tasks); err != nil {
		return err
	}
	// Validation may have durably blocked queued records; dispatch only the
	// canonical post-validation view.
	tasks, err = a.Store.SchedulingTasks(runID)
	if err != nil {
		return err
	}
	started, waiting, err := a.dispatch(cfg, control, tasks)
	if err != nil {
		return err
	}
	if started || waiting {
		return nil
	}

	if control.Mode == model.OperatingModeRunOnce {
		pending, unresolved, err := a.Store.BatchCounts(control.Batch.ID)
		if err != nil {
			return err
		}
		if pending == 0 && unresolved > 0 {
			return a.finishRunOnce(control, unresolved)
		}
		if control.Batch.Phase != model.BatchPhaseDraining || pending != 0 {
			return nil
		}
		return a.maybePlan(cfg, control)
	}
	if control.Mode == model.OperatingModeContinuous && time.Now().Unix() >= control.NextCycleAt {
		return a.maybePlan(cfg, control)
	}
	return nil
}

func (a *App) finishRunOnce(control model.Control, unresolved uint64) error {
	control.SetMode(model.OperatingModePaused)
	message := "Run once completed; new work paused"
	if unresolved > 0 {
		message = "Run once finished with unresolved work"
	}
	if err := a.Store.Event("system", "run_complete", message); err != nil {
		return err
	}
	return a.Store.SaveControl(control)
}

func (a *App) maybePlan(cfg config.Config, control model.Control) error {
	a.runtimeMu.Lock()
	if !a.runtime.idle() {
		a.runtimeMu.Unlock()
		return nil
	}
	a.runtimeMu.Unlock()
	capacity, err := a.Store.PlanningCapacity()
	if err != nil {
		return err
	}
	if !capacity.Available() {
		return a.handlePlanningCapacity(control, capacity)
	}
	a.runtimeMu.Lock()
	if !a.runtime.idle() {
		a.runtimeMu.Unlock()
		return nil
	}
	a.runtime.preflight = true
	a.runtime.preflightMode = model.CycleModeExecution
	a.runtimeMu.Unlock()
	snapshot := cfg.Clone()
	expected := cloneControl(control)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		err := a.doctor(a.ctx, snapshot, false)
		a.gate.Lock()
		defer a.gate.Unlock()
		if err == nil {
			_, err = a.beginCycle(snapshot, expected, model.CycleModeExecution)
		}
		if err != nil {
			a.endPreflight()
			live, loadErr := a.Control()
			var capacityErr *planningCapacityError
			if loadErr == nil && controlsEqual(live, expected) && errors.As(err, &capacityErr) {
				_ = a.handlePlanningCapacity(live, capacityErr.capacity)
			} else if loadErr == nil && controlsEqual(live, expected) && live.Mode == model.OperatingModeRunOnce {
				message := store.ErrorMessage(err)
				live.SetMode(model.OperatingModePaused)
				live.Error = &message
				_ = a.Store.SaveControl(live)
				_ = a.Store.Event("system", "planning_error", message)
			} else if loadErr == nil && controlsEqual(live, expected) && live.Mode == model.OperatingModeContinuous {
				message := store.ErrorMessage(err)
				live.Error = &message
				live.NextCycleAt = time.Now().Unix() + int64(cfg.CycleIntervalSeconds)
				_ = a.Store.SaveControl(live)
				_ = a.Store.Event("system", "planning_error", message)
			}
		}
		a.notify()
	}()
	return nil
}

func (a *App) handlePlanningCapacity(control model.Control, capacity model.PlanningCapacity) error {
	if control.Mode == model.OperatingModeContinuous {
		control.Error = nil
		control.NextCycleAt = capacity.NextResetAt
		return a.Store.SaveControl(control)
	}
	message := capacity.Message()
	control.SetMode(model.OperatingModePaused)
	control.Error = &message
	if err := a.Store.SaveControl(control); err != nil {
		return err
	}
	a.invalidatePrObservation()
	return a.Store.Event("system", "planning_capacity", message)
}

func (a *App) validateQueuedCycles(tasks []model.Task) error {
	cycleIDs := map[string]struct{}{}
	for _, task := range tasks {
		if task.Status == model.StatusQueued {
			cycleIDs[task.CycleID] = struct{}{}
		}
	}
	for cycleID := range cycleIDs {
		a.runtimeMu.Lock()
		_, checked := a.runtime.checkedCycles[cycleID]
		a.runtimeMu.Unlock()
		if checked {
			continue
		}
		cycleTasks, err := a.Store.TasksForCycle(cycleID)
		if err != nil {
			return err
		}
		if err := ValidateTaskPlan(cycleTasks); err != nil {
			for i := range cycleTasks {
				if cycleTasks[i].Status == model.StatusQueued {
					if blockErr := a.setTaskError(&cycleTasks[i], invalidPlan(err.Error())); blockErr != nil {
						return blockErr
					}
				}
			}
		}
		a.runtimeMu.Lock()
		a.runtime.checkedCycles[cycleID] = struct{}{}
		a.runtimeMu.Unlock()
	}
	return nil
}

func (a *App) dispatch(cfg config.Config, control model.Control, tasks []model.Task) (bool, bool, error) {
	if a.taskRunner == nil {
		for _, task := range tasks {
			if task.Status == model.StatusQueued {
				return false, true, nil
			}
		}
		return false, len(tasks) > 0, nil
	}
	activeByBranch := map[string]struct{}{}
	activeTasks := map[string]struct{}{}
	activeCount := uint64(0)
	for _, task := range tasks {
		if task.Status.Active() {
			activeTasks[task.ID] = struct{}{}
			activeCount++
			activeByBranch[task.Branch] = struct{}{}
		}
	}
	a.runtimeMu.Lock()
	for id, task := range a.runtime.tasks {
		if _, active := activeTasks[id]; !active {
			activeCount++
		}
		activeByBranch[task.branch] = struct{}{}
	}
	a.runtimeMu.Unlock()
	available := uint64(0)
	if cfg.ExecutionConcurrency > activeCount {
		available = cfg.ExecutionConcurrency - activeCount
	}
	started := false
	waiting := false
	var inventory *model.OpenPrInventory
	inventoryChecked := false
	for i := range tasks {
		task := &tasks[i]
		if task.Status != model.StatusQueued {
			continue
		}
		if available == 0 {
			waiting = true
			continue
		}
		ready, blocked, err := a.dependenciesReady(*task, control)
		if err != nil {
			return started, waiting, err
		}
		if blocked != nil {
			if err := a.setTaskError(task, blocked); err != nil {
				return started, waiting, err
			}
			continue
		}
		if !ready {
			waiting = true
			continue
		}
		if _, busy := activeByBranch[task.Branch]; busy {
			waiting = true
			continue
		}
		reservation, err := a.Store.HasPrReservation(task.ID)
		if err != nil {
			return started, waiting, err
		}
		admitted := false
		if task.Proposal.Target == task.Config.DefaultBranch && !reservation {
			if !inventoryChecked {
				inventory, _ = a.takePrAdmissionInventory(cfg, time.Now())
				inventoryChecked = true
			}
			if inventory == nil {
				a.startPrRefresh(cfg)
				waiting = true
				continue
			}
			admitted, err = a.Store.AdmitNewPrTask(task, *inventory)
			if err != nil {
				return started, waiting, err
			}
			if !admitted {
				a.startPrRefresh(cfg)
				waiting = true
				continue
			}
		} else {
			task.Status = model.StatusExecuting
			task.UpdatedAt = model.Now()
			if err := a.Store.Put("task", task.ID, *task); err != nil {
				return started, waiting, err
			}
			if err := a.Store.Event(task.ID, "status", "Executing"); err != nil {
				return started, waiting, err
			}
		}
		activeByBranch[task.Branch] = struct{}{}
		available--
		started = true
		a.runTask(*task)
	}
	return started, waiting, nil
}

func (a *App) dependenciesReady(task model.Task, control model.Control) (bool, error, error) {
	for _, id := range task.Proposal.Dependencies {
		dependency, err := store.Get[model.Task](a.Store, "task", id)
		if err != nil {
			return false, nil, err
		}
		if dependency == nil {
			return false, fmt.Errorf("Dependency %s is missing: %w", id, model.BlockedReasonDependencyBlocked), nil
		}
		if dependency.Status == model.StatusPublished {
			continue
		}
		if control.Mode == model.OperatingModeRunOnce && (dependency.RunID == nil || task.RunID == nil || *dependency.RunID != *task.RunID) {
			return false, fmt.Errorf("Dependency %s is outside this run-once batch: %w", id, model.BlockedReasonDependencyBlocked), nil
		}
		if dependency.Status == model.StatusBlocked || dependency.Status == model.StatusFailed || dependency.Status == model.StatusCancelled {
			return false, fmt.Errorf("Dependency %s is unresolved: %w", id, model.BlockedReasonDependencyBlocked), nil
		}
		return false, nil, nil
	}
	return true, nil, nil
}

// ValidateTaskPlan enforces dependency and same-branch writer ordering again at
// dispatch, so edited or legacy records cannot bypass planning validation.
func ValidateTaskPlan(tasks []model.Task) error {
	byID := map[string]model.Task{}
	for _, task := range tasks {
		if _, duplicate := byID[task.ID]; duplicate {
			return fmt.Errorf("Duplicate task identity %s", task.ID)
		}
		byID[task.ID] = task
	}
	edges := map[string][]string{}
	for _, task := range tasks {
		for _, dependencyID := range task.Proposal.Dependencies {
			dependency, ok := byID[dependencyID]
			if !ok {
				return fmt.Errorf("Task %s has unknown dependency %s", task.ID, dependencyID)
			}
			if task.Proposal.Target == task.Config.DefaultBranch {
				return fmt.Errorf("Default-branch tasks cannot depend on another task")
			}
			if dependency.Proposal.Target != task.Proposal.Target {
				return fmt.Errorf("Dependent tasks must write the same existing pull request")
			}
			edges[task.ID] = append(edges[task.ID], dependencyID)
		}
	}
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return errors.New("Task dependency cycle")
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependency := range edges[id] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	groups := map[string][]string{}
	for _, task := range tasks {
		if task.Proposal.Target != task.Config.DefaultBranch {
			groups[task.Proposal.Target] = append(groups[task.Proposal.Target], task.ID)
		}
	}
	var reaches func(string, string, map[string]bool) bool
	reaches = func(from, target string, seen map[string]bool) bool {
		if from == target {
			return true
		}
		if seen[from] {
			return false
		}
		seen[from] = true
		for _, dependency := range edges[from] {
			if reaches(dependency, target, seen) {
				return true
			}
		}
		return false
	}
	for target, ids := range groups {
		sort.Strings(ids)
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				if !reaches(ids[i], ids[j], map[string]bool{}) && !reaches(ids[j], ids[i], map[string]bool{}) {
					return fmt.Errorf("Writers to %s do not form a total dependency order", target)
				}
			}
		}
	}
	return nil
}
