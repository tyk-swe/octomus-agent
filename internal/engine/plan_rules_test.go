package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestReviewExternalPRContextDoesNotExposeCutToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, secret, fragment string
	}{
		{"GitHub token", "ghp_abcdefghijklmnopqrstuvwxyz1234567890", "ghp_abcdef"},
		{"API key", "sk-abcdefghijklmnopqrstuvwxyz1234567890", "sk-abcdef"},
		{"URL password", "https://owner:PrivatePassword42@repo.invalid/path", "owner:Private"},
		{"Bearer", "Bearer abcdefghijklmnopqrstuvwxyz", "Bearer abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := model.PullRequest{Number: 43,
				Title: strings.Repeat("界", maxPRTitleChars-11) + " " + tc.secret + " trailing text",
				Body:  strings.Repeat("界", maxPRBodyChars-11) + " " + tc.secret + " trailing text"}
			inventory := model.OpenPRInventory{ObservedAt: model.Now(), PRs: []model.PullRequest{pr}}
			context, coverage, err := externalContext(inventory)
			must0(t, err)
			if len(context) != 1 {
				t.Fatalf("lost PR context: %+v", coverage)
			}
			for _, text := range []string{context[0].Title, context[0].Body} {
				if strings.Contains(text, tc.fragment) || !strings.Contains(text, "[redacted]") {
					t.Fatalf("secret was cut before redaction: %q", text)
				}
			}
			if utf8.RuneCountInString(context[0].Title) > maxPRTitleChars || utf8.RuneCountInString(context[0].Body) > maxPRBodyChars || !context[0].TitleTruncated || !context[0].BodyTruncated {
				t.Fatalf("context lost its character bounds or cut flags: %+v", context[0])
			}
			if inventory.PRs[0].Title != pr.Title || inventory.PRs[0].Body != pr.Body {
				t.Fatal("the outward context changed its raw inventory")
			}
		})
	}
}

func TestExternalPRContextBoundsSerializedText(t *testing.T) {
	t.Parallel()
	inventory := model.OpenPRInventory{ObservedAt: model.Now()}
	for n := 110; n > 0; n-- {
		inventory.PRs = append(inventory.PRs, model.PullRequest{Number: uint64(n),
			Title: strings.Repeat("界", 250), Body: strings.Repeat("界", 2200)})
	}
	context, coverage, err := externalContext(inventory)
	must0(t, err)
	encoded, err := json.Marshal(context)
	must0(t, err)
	if len(encoded) > maxPRContextBytes || len(context) >= maxExternalPRs || len(context) == 0 {
		t.Fatalf("serialized context bound was not enforced: %d bytes, %d entries", len(encoded), len(context))
	}
	if coverage.IncludedExternal != uint64(len(context)) || coverage.OmittedExternal+coverage.IncludedExternal != 110 || !coverage.Complete {
		t.Fatalf("incorrect inventory coverage: %+v", coverage)
	}
	for i, pr := range context {
		if pr.Number != uint64(i+1) || !pr.TitleTruncated || !pr.BodyTruncated {
			t.Fatalf("unstable order or missing cut flags at %d: %+v", i, pr)
		}
	}
}
