package httpapi

import (
	"net/http"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

type routeSpec struct {
	method  string
	pattern string
	handler func(*api) handlerFunc
}

// apiRoutes is the complete authenticated HTTP contract. Keep this table ordered
// to make method discovery and route inventory review straightforward.
var apiRoutes = []routeSpec{
	{http.MethodGet, "/api/state", func(a *api) handlerFunc { return a.stateView }},
	{http.MethodGet, "/api/repositories", func(a *api) handlerFunc { return a.repositoriesView }},
	{http.MethodPost, "/api/repositories", func(a *api) handlerFunc { return a.repositoryCreate }},
	{http.MethodPut, "/api/instance", func(a *api) handlerFunc { return a.instanceUpdate }},
	{http.MethodGet, "/api/tasks", func(a *api) handlerFunc {
		return a.history(func(app *engine.App, q store.HistoryQuery) (store.Page[store.TaskSummary], error) {
			return app.TaskPage(q)
		})
	}},
	{http.MethodGet, "/api/cycles", func(a *api) handlerFunc {
		return a.history(func(app *engine.App, q store.HistoryQuery) (store.Page[store.CycleSummary], error) {
			return app.CyclePage(q)
		})
	}},
	{http.MethodGet, "/api/cycles/{id}", func(a *api) handlerFunc { return a.cycleDetail }},
	{http.MethodGet, "/api/cycles/{id}/evidence", func(a *api) handlerFunc { return a.cycleEvidence }},
	{http.MethodPost, "/api/cycles/{id}/{action}", func(a *api) handlerFunc { return a.cycleAction }},
	{http.MethodGet, "/api/proposals", func(a *api) handlerFunc {
		return a.history(func(app *engine.App, q store.HistoryQuery) (store.Page[store.ProposalSummary], error) {
			return app.ProposalPage(q)
		})
	}},
	{http.MethodGet, "/api/proposals/{cycle}/{id}", func(a *api) handlerFunc { return a.proposalDetail }},
	{http.MethodGet, "/api/requests", func(a *api) handlerFunc { return a.requestsView }},
	{http.MethodPost, "/api/requests", func(a *api) handlerFunc { return a.requestCreate }},
	{http.MethodPost, "/api/requests/{id}/{action}", func(a *api) handlerFunc { return a.requestAction }},
	{http.MethodGet, "/api/prs", func(a *api) handlerFunc {
		return a.history(func(app *engine.App, q store.HistoryQuery) (store.Page[store.PRSummary], error) { return app.PRPage(q) })
	}},
	{http.MethodGet, "/api/insights", func(a *api) handlerFunc { return a.insights }},
	{http.MethodGet, "/api/tasks/{id}", func(a *api) handlerFunc { return a.taskDetail }},
	{http.MethodPost, "/api/tasks/{id}/{action}", func(a *api) handlerFunc { return a.taskAction }},
	{http.MethodGet, "/api/config", func(a *api) handlerFunc { return a.getConfig }},
	{http.MethodPut, "/api/config", func(a *api) handlerFunc { return a.saveConfig }},
	{http.MethodPost, "/api/baseline-checks", func(a *api) handlerFunc { return a.baselineStart }},
	{http.MethodGet, "/api/baseline-checks/latest", func(a *api) handlerFunc { return a.baselineLatest }},
	{http.MethodGet, "/api/baseline-checks/{id}", func(a *api) handlerFunc { return a.baselineDetail }},
	{http.MethodPost, "/api/baseline-checks/{id}/cancel", func(a *api) handlerFunc { return a.baselineCancel }},
	{http.MethodPost, "/api/control/{action}", func(a *api) handlerFunc { return a.controlAction }},
	{http.MethodPost, "/api/doctor", func(a *api) handlerFunc { return a.doctor }},
	{http.MethodPost, "/api/sandbox/self-test", func(a *api) handlerFunc { return a.sandboxSelfTest }},
	{http.MethodPost, "/api/model-catalog", func(a *api) handlerFunc { return a.modelCatalog }},
	{http.MethodGet, "/api/events", func(a *api) handlerFunc { return a.events }},
}
