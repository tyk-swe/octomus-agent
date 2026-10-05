package engine

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestContextFingerprintIncludesPRStatusNotObservationTime(t *testing.T) {
	t.Parallel()
	pr := model.PullRequest{Number: 42, Head: "head", Base: "main", State: "open", ReviewDecision: "review_required", CheckStatus: "pending", Mergeability: "unknown"}
	before := contextFingerprint("revision", []model.PullRequest{pr})
	for name, change := range map[string]func(*model.PullRequest){
		"review":       func(p *model.PullRequest) { p.ReviewDecision = "approved" },
		"checks":       func(p *model.PullRequest) { p.CheckStatus = "success" },
		"mergeability": func(p *model.PullRequest) { p.Mergeability = "mergeable" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pr.Clone()
			change(&changed)
			if contextFingerprint("revision", []model.PullRequest{changed}) == before {
				t.Fatal("changed PR status did not change the fingerprint")
			}
		})
	}
	pr.StatusObservedAt, pr.StatusSource = model.Now(), "https://github.com/fixture/project/pull/42"
	if contextFingerprint("revision", []model.PullRequest{pr}) != before {
		t.Fatal("unchanged status observation changed the fingerprint")
	}
	other := model.PullRequest{Number: 43, Head: "other", Base: "main", State: "open"}
	if contextFingerprint("revision", []model.PullRequest{pr, other}) != contextFingerprint("revision", []model.PullRequest{other, pr}) {
		t.Fatal("PR ordering changed the fingerprint")
	}
}
