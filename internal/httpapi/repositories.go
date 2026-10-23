package httpapi

import (
	"net/http"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// RepositorySummary is one served repository in the fleet listing.
type RepositorySummary struct {
	GitHubRepo string `json:"github_repo"`
	Primary    bool   `json:"primary"`
}

// RepositoriesView lists every repository this instance serves (in
// registration order, primary first) plus the instance-wide session ceiling
// (0 = no ceiling beyond per-repository caps).
type RepositoriesView struct {
	Repositories              []RepositorySummary `json:"repositories"`
	InstanceMaxSessionsPerDay uint64              `json:"instance_max_sessions_per_day"`
}

func (a *api) repositories() (RepositoriesView, error) {
	registered, err := a.fleet.Registered()
	if err != nil {
		return RepositoriesView{}, err
	}
	settings, err := a.fleet.InstanceSettings()
	if err != nil {
		return RepositoriesView{}, err
	}
	view := RepositoriesView{Repositories: []RepositorySummary{}, InstanceMaxSessionsPerDay: settings.MaxSessionsPerDay}
	for i, entry := range registered {
		view.Repositories = append(view.Repositories, RepositorySummary{GitHubRepo: entry.GitHubRepo, Primary: i == 0})
	}
	return view, nil
}

func (a *api) repositoriesView(_ http.ResponseWriter, _ *http.Request) (int, any, error) {
	view, err := a.repositories()
	return http.StatusOK, view, err
}

type repositoryCreateBody struct {
	GitHubRepo string `json:"github_repo"`
}

func (v *repositoryCreateBody) UnmarshalJSON(data []byte) error {
	return wirejson.DecodeStrict(data, v)
}

// repositoryCreate registers a repository with this instance and starts
// serving it paused; the operator then saves its configuration through the
// usual settings flow with ?repository= selecting it.
func (a *api) repositoryCreate(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var body repositoryCreateBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	if !config.ValidGitHubRepo(body.GitHubRepo) {
		return 0, nil, &bodyError{http.StatusBadRequest, "GitHub repository must be OWNER/REPOSITORY"}
	}
	registered, err := a.fleet.Registered()
	if err != nil {
		return 0, nil, err
	}
	status := http.StatusCreated
	for _, entry := range registered {
		if config.EqualASCII(entry.GitHubRepo, body.GitHubRepo) {
			status = http.StatusOK
		}
	}
	dataDir := "repos/" + store.RepositorySlug(body.GitHubRepo)
	if len(registered) == 0 {
		dataDir = "."
	}
	if _, err := a.fleet.AddRepository(store.RepositoryEntry{GitHubRepo: body.GitHubRepo, DataDir: dataDir}); err != nil {
		return 0, nil, err
	}
	view, err := a.repositories()
	return status, view, err
}

type instanceUpdateBody struct {
	MaxSessionsPerDay uint64 `json:"max_sessions_per_day"`
}

func (v *instanceUpdateBody) UnmarshalJSON(data []byte) error { return wirejson.DecodeStrict(data, v) }

// instanceUpdate saves the instance-wide daily session ceiling (0 clears it).
func (a *api) instanceUpdate(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var body instanceUpdateBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	if err := a.fleet.SetInstanceSettings(config.InstanceSettings{MaxSessionsPerDay: body.MaxSessionsPerDay}); err != nil {
		return 0, nil, err
	}
	view, err := a.repositories()
	return http.StatusOK, view, err
}
