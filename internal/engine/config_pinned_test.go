package engine

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestSaveConfigRejectsChangedPinnedIdentity(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"repository", "github_repo"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			state := testStore(t)
			deployment := Deployment{Repository: t.TempDir(), GitHubRepo: "fixture/project"}
			app := New(state, t.TempDir(), WithDeployment(deployment))
			cfg := deployment.pin(config.Default())
			if err := state.Put("settings", "config", cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := app.Settings()
			if err != nil {
				t.Fatal(err)
			}
			value := deployment.Repository + "/other"
			if field == "github_repo" {
				value = "fixture/other"
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.SaveConfig(loaded.Revision, map[string]json.RawMessage{
				field:         raw,
				"max_retries": json.RawMessage(`3`),
			}); err == nil {
				t.Fatal("save accepted a different deployment identity")
			}
			stored, err := store.Get[config.Config](state, "settings", "config")
			if err != nil {
				t.Fatal(err)
			}
			if !wirejson.Equal(stored, cfg) {
				t.Fatal("rejected save changed persisted configuration")
			}
		})
	}
}
