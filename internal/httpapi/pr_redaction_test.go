package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestPRContextPresentationDropsLegacyCutFragments(t *testing.T) {
	t.Parallel()
	app, state := testApp(t)
	router := Router(app, token, "", "test")
	cycle := model.Cycle{ID: model.ID(), Mode: model.CycleModeAudit, Status: model.CycleCompleted,
		StartedAt: model.Now(), CompletedAt: new(model.Now()), Grounding: &model.Grounding{Revision: "recorded"}}
	fragments := []string{"ghp_abcdef", "https://owner:partial-password", "unknown-old-credential-prefix"}
	for i, fragment := range fragments {
		cycle.Grounding.ExternalPRs = append(cycle.Grounding.ExternalPRs, model.ExternalPRContext{
			Number: uint64(i + 1), Title: "Known context " + fragment, Body: "Retained paragraph.\n" + fragment,
			TitleTruncated: true, BodyTruncated: true,
		})
	}
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get[json.RawMessage](state, "cycle", cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	response := call(t, router, "GET", "/api/cycles/"+cycle.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("cycle detail status %d: %s", response.Code, response.Body)
	}
	for _, fragment := range fragments {
		if strings.Contains(response.Body.String(), fragment) {
			t.Fatalf("legacy capture fragment was presented: %q", fragment)
		}
	}
	if !strings.Contains(response.Body.String(), "Known context") || !strings.Contains(response.Body.String(), "Retained paragraph.") || !strings.Contains(response.Body.String(), `"body_truncated":true`) {
		t.Fatalf("lost useful complete text or truncation evidence: %s", response.Body)
	}
	after, err := store.Get[json.RawMessage](state, "cycle", cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(*before) != string(*after) {
		t.Fatal("presentation changed raw historical cycle evidence")
	}
}

func TestPRHistoryRedactsCompleteTitlesBeforeBounds(t *testing.T) {
	t.Parallel()
	app, state := testApp(t)
	const credential = "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	pr := model.PRObservation{PR: model.PullRequest{Number: 42, State: "open",
		Title: strings.Repeat("x", 190) + credential + " tail"}}
	if err := state.Put("pr", "42", pr); err != nil {
		t.Fatal(err)
	}
	response := call(t, Router(app, token, "", "test"), "GET", "/api/prs", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "ghp_abcdef") || !strings.Contains(response.Body.String(), "[redacted]") {
		t.Fatalf("unsafe PR title presentation: %d %s", response.Code, response.Body)
	}
}
