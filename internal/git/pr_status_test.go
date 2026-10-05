package git

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func statusResponse(pr model.PullRequest, review any, checks, mergeability string) map[string]any {
	var rollup any
	if checks != "" {
		rollup = map[string]any{"state": checks}
	}
	return map[string]any{"number": pr.Number, "url": pr.URL, "headRefOid": pr.Head,
		"reviewDecision": review, "mergeable": mergeability,
		"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
			"oid": pr.Head, "statusCheckRollup": rollup,
		}}}},
	}
}

func statusJSON(t *testing.T, pr any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestOwnedPRStatus(t *testing.T) {
	t.Parallel()
	for _, review := range []struct {
		value any
		want  string
	}{{nil, "none"}, {"APPROVED", "approved"}, {"CHANGES_REQUESTED", "changes_requested"}, {"REVIEW_REQUIRED", "review_required"}} {
		for _, checks := range []struct{ value, want string }{{"", "none"}, {"ERROR", "failure"}, {"FAILURE", "failure"}, {"EXPECTED", "pending"}, {"PENDING", "pending"}, {"SUCCESS", "success"}} {
			for _, mergeability := range []string{"CONFLICTING", "MERGEABLE", "UNKNOWN"} {
				t.Run(fmt.Sprintf("%s/%s/%s", review.want, checks.value, mergeability), func(t *testing.T) {
					pr := model.PullRequest{Number: 42, Head: strings.Repeat("a", 40), URL: "https://github.com/fixture/project/pull/42", Owned: true}
					if err := applyOwnedPRStatus(statusJSON(t, statusResponse(pr, review.value, checks.value, mergeability)), "fixture/project", &pr); err != nil {
						t.Fatal(err)
					}
					if pr.ReviewDecision != review.want || pr.CheckStatus != checks.want || pr.Mergeability != strings.ToLower(mergeability) || pr.StatusSource != pr.URL || pr.StatusObservedAt == "" {
						t.Fatalf("status = %+v", pr)
					}
				})
			}
		}
	}
}

func TestOwnedPRStatusRejectsPartialOrRacingReads(t *testing.T) {
	t.Parallel()
	before := model.PullRequest{Number: 42, Head: strings.Repeat("a", 40), URL: "https://github.com/fixture/project/pull/42", Owned: true}
	for _, field := range []string{"number", "url", "headRefOid", "commits", "reviewDecision", "mergeable", "checks", "commit_oid"} {
		t.Run(field, func(t *testing.T) {
			pr := before.Clone()
			status := statusResponse(pr, "APPROVED", "SUCCESS", "MERGEABLE")
			switch field {
			case "number":
				status[field] = 43
			case "commits":
				status[field] = map[string]any{"nodes": []any{}}
			case "checks", "commit_oid":
				commit := status["commits"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["commit"].(map[string]any)
				if field == "checks" {
					commit["statusCheckRollup"] = map[string]any{"state": "UNRECOGNIZED"}
				} else {
					commit["oid"] = strings.Repeat("b", 40)
				}
			default:
				status[field] = strings.Repeat("unrecognized", 1000)
			}
			if err := applyOwnedPRStatus(statusJSON(t, status), "fixture/project", &pr); err == nil {
				t.Fatal("partial, unknown or racing status was accepted")
			}
			if !reflect.DeepEqual(pr, before) {
				t.Fatal("failed status read changed the PR")
			}
		})
	}
	for _, out := range []string{"not json", statusJSON(t, nil), `{"data":{"repository":null}}`, `{"errors":[{"message":"ghp_DoNotExposeThisError00000001"}],"data":null}`} {
		pr := before.Clone()
		if err := applyOwnedPRStatus(out, "fixture/project", &pr); err == nil || strings.Contains(err.Error(), "DoNotExpose") {
			t.Fatalf("invalid response = %v", err)
		}
		if !reflect.DeepEqual(pr, before) {
			t.Fatal("failed status read changed the PR")
		}
	}
}
