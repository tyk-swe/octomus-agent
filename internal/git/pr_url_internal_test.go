package git

import (
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// TestParseCreatedPRURLAcceptsOnlyThisRepositorysPullURL: the URL `gh pr
// create` prints is the only evidence of where a new pull request landed, so
// only an https github.com pull URL for the configured repository yields a
// number. Every other shape is a RemoteConflict naming why, because the request
// may exist somewhere this task does not know about.
func TestParseCreatedPRURLAcceptsOnlyThisRepositorysPullURL(t *testing.T) {
	const repo = "fixture/project"
	accepted := []struct {
		url  string
		want uint64
	}{
		{"https://github.com/fixture/project/pull/7", 7},
		{"  https://github.com/fixture/project/pull/7\n", 7},
		{"https://github.com/Fixture/Project/pull/7", 7},
		{"https://github.com:443/fixture/project/pull/7", 7},
	}
	for _, tc := range accepted {
		number, err := parseCreatedPRURL(tc.url, repo)
		if err != nil || number != tc.want {
			t.Errorf("parseCreatedPRURL(%q) = %d, %v; want %d", tc.url, number, err, tc.want)
		}
	}
	rejected := []struct {
		url  string
		want string
	}{
		{"http://github.com/fixture/project/pull/7", "Invalid PR creation URL"},
		{"https://example.com/fixture/project/pull/7", "Invalid PR creation URL"},
		{"https://github.com/fixture/project/pull/7?x=1", "Invalid PR creation URL"},
		{"https://github.com/fixture/project/pull/7#c", "Invalid PR creation URL"},
		{"https://github.com/other/project/pull/7", "Created PR belongs to a different repository"},
		{"https://github.com/fixture/project/issues/7", "Created PR belongs to a different repository"},
		{"https://github.com/fixture/project/pull/7/files", "Created PR belongs to a different repository"},
		{"https://github.com/fixture/project/pull/abc", "Missing created PR number"},
		{"https://github.com/fixture/project/pull/-1", "Missing created PR number"},
		{"https://github.com/fixture/project/pull/", "Missing created PR number"},
		{"not a url", "PR creation returned no unambiguous URL; reconcile before retrying"},
		{"", "PR creation returned no unambiguous URL; reconcile before retrying"},
	}
	for _, tc := range rejected {
		number, err := parseCreatedPRURL(tc.url, repo)
		if err == nil {
			t.Errorf("parseCreatedPRURL(%q) = %d; want a refusal", tc.url, number)
			continue
		}
		if !strings.HasPrefix(err.Error(), tc.want+": ") {
			t.Errorf("parseCreatedPRURL(%q) = %q; want %q first", tc.url, err, tc.want)
		}
		if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonRemoteConflict {
			t.Errorf("parseCreatedPRURL(%q) reason = %v; want remote_conflict", tc.url, reason)
		}
	}
}
