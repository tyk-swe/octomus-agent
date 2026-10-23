package httpapi

// Fleet endpoints: registration, per-repository request scoping and the
// instance ceiling over the authenticated API.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestRepositoriesEndpointsAndScoping(t *testing.T) {
	fleet, _, state := testFleet(t, nil)
	router := Router(fleet, token, "", "test")

	created := request(t, router, http.MethodPost, "/api/repositories", `{"github_repo":"acme/one"}`, true)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	second := request(t, router, http.MethodPost, "/api/repositories", `{"github_repo":"acme/two"}`, true)
	if second.Code != http.StatusCreated {
		t.Fatalf("second create status = %d: %s", second.Code, second.Body.String())
	}
	var view RepositoriesView
	if err := json.Unmarshal(second.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Repositories) != 2 || view.Repositories[0].GitHubRepo != "acme/one" || !view.Repositories[0].Primary || view.Repositories[1].GitHubRepo != "acme/two" {
		t.Fatalf("fleet listing differs: %+v", view)
	}
	// Re-registering is an idempotent success, not a duplicate.
	again := request(t, router, http.MethodPost, "/api/repositories", `{"github_repo":"ACME/One"}`, true)
	if again.Code != http.StatusOK {
		t.Fatalf("re-create status = %d: %s", again.Code, again.Body.String())
	}
	invalid := request(t, router, http.MethodPost, "/api/repositories", `{"github_repo":"not-a-repo"}`, true)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid repository status = %d", invalid.Code)
	}

	// State is scoped: each repository reports its own identity, and an
	// unknown selection is not-found rather than another repository's state.
	for _, repo := range []string{"acme/one", "acme/two"} {
		cfg := config.Default()
		cfg.GitHubRepo = repo
		scoped := state.ForRepository(repo)
		if err := store.Put(scoped, scoped.ConfigKey(), cfg); err != nil {
			t.Fatal(err)
		}
		if err := scoped.Event("system", model.EventConfiguration, "Configuration for "+repo); err != nil {
			t.Fatal(err)
		}
	}
	for _, repo := range []string{"acme/one", "acme/two"} {
		state := request(t, router, http.MethodGet, "/api/state?repository="+repo, "", true)
		if state.Code != http.StatusOK {
			t.Fatalf("%s state status = %d: %s", repo, state.Code, state.Body.String())
		}
		body := decode(t, state)
		if body["repository"] != repo {
			t.Fatalf("%s state names %v", repo, body["repository"])
		}
		for _, event := range body["events"].([]any) {
			if event.(map[string]any)["message"] != "Configuration for "+repo {
				t.Fatalf("%s state contains another repository's event: %v", repo, event)
			}
		}
		response := request(t, router, http.MethodGet, "/api/events?repository="+repo, "", true)
		var events []model.Event
		if response.Code != http.StatusOK {
			t.Fatalf("%s events status = %d: %s", repo, response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Message != "Configuration for "+repo {
			t.Fatalf("%s events contain another repository's state: %+v", repo, events)
		}
	}
	unknown := request(t, router, http.MethodGet, "/api/state?repository=acme/missing", "", true)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown repository status = %d: %s", unknown.Code, unknown.Body.String())
	}

	ceiling := request(t, router, http.MethodPut, "/api/instance", `{"max_sessions_per_day":42}`, true)
	if ceiling.Code != http.StatusOK {
		t.Fatalf("instance status = %d: %s", ceiling.Code, ceiling.Body.String())
	}
	listed := request(t, router, http.MethodGet, "/api/repositories", "", true)
	var after RepositoriesView
	if err := json.Unmarshal(listed.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.InstanceMaxSessionsPerDay != 42 {
		t.Fatalf("instance ceiling = %d, want 42", after.InstanceMaxSessionsPerDay)
	}
}
