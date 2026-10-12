// The HTTP layer's read-only queries over saved state. Bounded pages stay owned by the store; views that mix live
// policy are assembled here beside their response types.

package engine

import (
	"github.com/tyk-swe/octomus-agent/internal/export"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func (a *App) TaskDetail(id string) (*TaskDetail, error) {
	task, err := store.Get(a.store, store.Tasks.Key(id))
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, NotFound("Task not found")
	}
	live, err := a.Config()
	if err != nil {
		return nil, err
	}
	return &TaskDetail{
		Task: *task, AllowedActions: task.AllowedActions(),
		EffectiveAttemptPolicy: model.PolicyOf(task.ExecutionConfig()),
		OperatingPolicy:        OperatingPolicy{MaxSessionsPerDay: live.MaxSessionsPerDay, MaxWorkspaceBytes: live.MaxWorkspaceBytes},
	}, nil
}

func (a *App) CycleDetail(id string) (*model.Cycle, error) {
	cycle, err := store.Get(a.store, store.Cycles.Key(id))
	if err != nil {
		return nil, err
	}
	if cycle == nil {
		return nil, NotFound("Cycle not found")
	}
	return cycle, nil
}

func (a *App) RunEvidence(cycleID string) (map[string]any, error) {
	value, err := export.RunEvidence(a.store, cycleID)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, NotFound("Cycle not found")
	}
	return value, nil
}

func (a *App) ProposalDetail(cycle, id string) (*store.ProposalDetail, error) {
	detail, err := a.store.ProposalDetail(cycle, id)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return nil, NotFound("Proposal not found")
	}
	return detail, nil
}

func (a *App) TaskPage(q store.HistoryQuery) (store.Page[store.TaskSummary], error) {
	return a.store.TaskPage(q)
}

func (a *App) CyclePage(q store.HistoryQuery) (store.Page[store.CycleSummary], error) {
	return a.store.CyclePage(q)
}

func (a *App) PRPage(q store.HistoryQuery) (store.Page[store.PRSummary], error) {
	return a.store.PRPage(q)
}

func (a *App) ProposalPage(q store.HistoryQuery) (store.Page[store.ProposalSummary], error) {
	return a.store.ProposalPage(q)
}

func (a *App) Events(entity *string) ([]model.Event, error) {
	return a.store.Events(entity)
}
