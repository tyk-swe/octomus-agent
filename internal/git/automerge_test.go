package git

import (
	"encoding/json"
	"strings"
	"testing"
)

func mergeStatusJSON(mutate func(pr map[string]any, repo map[string]any)) string {
	pr := map[string]any{
		"number":              7,
		"url":                 "https://github.com/fixture/project/pull/7",
		"state":               "OPEN",
		"isDraft":             false,
		"headRefName":         "octomus/work",
		"headRefOid":          strings.Repeat("a", 40),
		"headRepository":      map[string]any{"nameWithOwner": "fixture/project"},
		"baseRefName":         "main",
		"baseRefOid":          strings.Repeat("b", 40),
		"repository":          map[string]any{"nameWithOwner": "fixture/project"},
		"isMergeQueueEnabled": false,
		"reviewDecision":      "APPROVED",
		"mergeable":           "MERGEABLE",
		"mergeStateStatus":    "CLEAN",
		"additions":           3,
		"deletions":           1,
		"changedFiles":        2,
		"mergedAt":            nil,
		"mergeCommit":         nil,
		"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
			"oid": strings.Repeat("a", 40),
			"statusCheckRollup": map[string]any{
				"state":    "SUCCESS",
				"contexts": map[string]any{"totalCount": 2},
			},
		}}}},
	}
	repo := map[string]any{
		"nameWithOwner":      "fixture/project",
		"squashMergeAllowed": true,
		"pullRequest":        pr,
	}
	mutate(pr, repo)
	return `{"data":{"repository":` + marshal(repo) + `}}`
}

func marshal(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestParseMergeStatusAcceptsTheCanonicalResponse(t *testing.T) {
	status, err := parseMergeStatus(mergeStatusJSON(func(pr, repo map[string]any) {}), "fixture/project", 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.Number != 7 || status.State != "open" || status.Head != strings.Repeat("a", 40) ||
		status.BaseBranch != "main" || status.Repository != "fixture/project" || status.Draft ||
		status.MergeQueue || !status.SquashAllowed || status.Mergeable != "MERGEABLE" ||
		status.MergeState != "CLEAN" || status.ReviewDecision == nil || *status.ReviewDecision != "APPROVED" ||
		status.CheckState == nil || *status.CheckState != "SUCCESS" || status.CheckContexts != 2 ||
		status.Additions == nil || *status.Additions != 3 || status.Deletions == nil || *status.Deletions != 1 ||
		status.ChangedFiles == nil || *status.ChangedFiles != 2 {
		t.Fatalf("parsed merge status = %+v", status)
	}
}

func TestParseMergeStatusRefusesIncompleteOrForeignAnswers(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, test := range []struct {
		name   string
		mutate func(pr, repo map[string]any)
		body   string
	}{
		{"malformed", nil, "not json"},
		{"graphql-errors", nil, `{"errors":[{"message":"x"}],"data":{"repository":{}}}`},
		{"no-pull-request", func(pr, repo map[string]any) { repo["pullRequest"] = nil }, ""},
		{"missing-repository", func(pr, repo map[string]any) { repo["nameWithOwner"] = nil }, ""},
		{"foreign-repository", func(pr, repo map[string]any) { repo["nameWithOwner"] = "other/project" }, ""},
		{"missing-squash", func(pr, repo map[string]any) { repo["squashMergeAllowed"] = nil }, ""},
		{"wrong-number", func(pr, repo map[string]any) { pr["number"] = 9 }, ""},
		{"missing-url", func(pr, repo map[string]any) { pr["url"] = "" }, ""},
		{"missing-head", func(pr, repo map[string]any) { pr["headRefOid"] = "" }, ""},
		{"missing-head-repository", func(pr, repo map[string]any) { pr["headRepository"] = nil }, ""},
		{"fork-head", func(pr, repo map[string]any) {
			pr["headRepository"] = map[string]any{"nameWithOwner": "contributor/project"}
		}, ""},
		{"missing-base", func(pr, repo map[string]any) { pr["baseRefOid"] = "" }, ""},
		{"missing-draft", func(pr, repo map[string]any) { pr["isDraft"] = nil }, ""},
		{"missing-queue", func(pr, repo map[string]any) { pr["isMergeQueueEnabled"] = nil }, ""},
		{"unknown-state", func(pr, repo map[string]any) { pr["state"] = "EXPLODED" }, ""},
		{"unknown-mergeable", func(pr, repo map[string]any) { pr["mergeable"] = "MAYBE" }, ""},
		{"unknown-merge-state", func(pr, repo map[string]any) { pr["mergeStateStatus"] = "WEIRD" }, ""},
		{"missing-review-decision", func(pr, repo map[string]any) { delete(pr, "reviewDecision") }, ""},
		{"unknown-review-decision", func(pr, repo map[string]any) { pr["reviewDecision"] = "LOOKS_GOOD" }, ""},
		{"head-check-mismatch", func(pr, repo map[string]any) {
			pr["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
				"oid": strings.Repeat("c", 40), "statusCheckRollup": nil}}}}
		}, ""},
		{"rollup-missing-contexts", func(pr, repo map[string]any) {
			pr["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
				"oid": head, "statusCheckRollup": map[string]any{"state": "SUCCESS"}}}}}
		}, ""},
		{"unknown-check-state", func(pr, repo map[string]any) {
			pr["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
				"oid": head, "statusCheckRollup": map[string]any{"state": "GLOWING", "contexts": map[string]any{"totalCount": 1}}}}}}
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := test.body
			if test.mutate != nil {
				body = mergeStatusJSON(test.mutate)
			}
			if _, err := parseMergeStatus(body, "fixture/project", 7); err == nil {
				t.Fatalf("accepted %s", test.name)
			}
		})
	}
}

func TestParseMergeStatusExplicitNullReviewAndOutcomes(t *testing.T) {
	status, err := parseMergeStatus(mergeStatusJSON(func(pr, repo map[string]any) {
		pr["reviewDecision"] = nil
		delete(pr, "additions")
	}), "fixture/project", 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.ReviewDecision != nil || status.Additions != nil || status.Deletions == nil {
		t.Fatalf("null review decision or absent stats = %+v", status)
	}
	merged, err := parseMergeStatus(mergeStatusJSON(func(pr, repo map[string]any) {
		pr["state"] = "MERGED"
		pr["mergedAt"] = "2026-10-01T00:00:00Z"
		pr["mergeCommit"] = map[string]any{"oid": strings.Repeat("d", 40)}
	}), "fixture/project", 7)
	if err != nil {
		t.Fatal(err)
	}
	if merged.State != "merged" || merged.MergedAt == nil || merged.MergeCommit == nil ||
		*merged.MergeCommit != strings.Repeat("d", 40) {
		t.Fatalf("merged outcome = %+v", merged)
	}
	if _, err := parseMergeStatus(mergeStatusJSON(func(pr, repo map[string]any) {
		pr["mergeCommit"] = map[string]any{"oid": ""}
	}), "fixture/project", 7); err == nil {
		t.Fatal("accepted an empty merge commit")
	}
}
