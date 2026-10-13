// The gitlink refusal reports a work-tree path; these tests stage a real nested
// repository so the name comes from Git's own diagnostic output, not a helper.

package git_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/git"
)

// reportedGitlinkCut finds the quoted path in a gitlink refusal.
var reportedGitlinkCut = regexp.MustCompile(`at ("(?:[^"\\]|\\.)*")`)

// refusedGitlink stages a nested repository at path in a fresh clone and returns the
// refusal a snapshot produces for it.
func refusedGitlink(t *testing.T, path string) error {
	t.Helper()
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	base, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := git.CloneAt(ctx, c, workspace, base); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(workspace, filepath.FromSlash(path))
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit(t, nested, "init", "-b", "main")
	realGit(t, nested, "config", "user.name", "Fixture")
	realGit(t, nested, "config", "user.email", "fixture@example.com")
	writeFile(t, filepath.Join(nested, "nested.txt"), "nested\n")
	realGit(t, nested, "add", ".")
	realGit(t, nested, "commit", "-m", "Nested repository")
	_, err = git.Snapshot(ctx, c, workspace, "Deliver the feature")
	return err
}

// refusedPath returns the path a gitlink refusal reported.
func refusedPath(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("a staged nested repository must be refused")
	}
	match := reportedGitlinkCut.FindStringSubmatch(err.Error())
	if match == nil {
		t.Fatalf("refusal does not report the path: %q", err)
	}
	path, unquoteErr := strconv.Unquote(match[1])
	if unquoteErr != nil {
		t.Fatalf("reported path is not a quoted literal: %q", match[1])
	}
	return path
}

// assertNoSecretFragment fails if any window of the configured secret survived in text.
func assertNoSecretFragment(t *testing.T, text, secret string) {
	t.Helper()
	const window = 8
	if len(secret) < window {
		t.Fatalf("fixture secret %q is too short to test a partial leak", secret)
	}
	for i := 0; i+window <= len(secret); i++ {
		if strings.Contains(text, secret[i:i+window]) {
			t.Fatalf("reported path %q retains %q from the configured secret", text, secret[i:i+window])
		}
	}
}

// A configured secret starting before the diagnostic bound must be recognized whole, even though the reported path is
// far longer than that bound.
func TestRefusedGitlinkRedactsSecretSpanningDiagnosticCut(t *testing.T) {
	t.Parallel()
	path := strings.Repeat("a", 185) + "/" + envSecretValue
	reported := refusedPath(t, refusedGitlink(t, path))
	if !strings.Contains(reported, "[redacted]") {
		t.Fatalf("reported path = %q; want the configured environment secret redacted", reported)
	}
	assertNoSecretFragment(t, reported, envSecretValue)
}

// A token that only a whole-token pattern recognizes must not survive as a fragment.
func TestRefusedGitlinkRedactsTokenSpanningDiagnosticCut(t *testing.T) {
	t.Parallel()
	token := "ghp_" + strings.Repeat("A1b2C3d4E5", 4)
	path := strings.Repeat("a", 188) + "/" + token
	reported := refusedPath(t, refusedGitlink(t, path))
	if strings.Contains(reported, "ghp_") {
		t.Fatalf("reported path = %q; want the token redacted", reported)
	}
	if !strings.Contains(reported, "[redacted]") {
		t.Fatalf("reported path = %q; want a redaction marker", reported)
	}
}

// A multibyte name beyond the bound must be cut between characters, never inside one.
func TestRefusedGitlinkBoundsMultibytePathAtCharacterBoundary(t *testing.T) {
	t.Parallel()
	path := strings.Join([]string{strings.Repeat("€", 84), strings.Repeat("€", 84), strings.Repeat("€", 82)}, "/")
	reported := refusedPath(t, refusedGitlink(t, path))
	if !utf8.ValidString(reported) {
		t.Fatalf("reported path %q is not valid UTF-8", reported)
	}
	if runes := []rune(reported); len(runes) > 201 {
		t.Fatalf("reported path is %d characters; want at most 201", len(runes))
	}
	if strings.HasPrefix(path, reported) || !strings.HasPrefix(path, strings.TrimSuffix(reported, "…")) {
		t.Fatalf("reported path %q is not the bounded start of the staged path", reported)
	}
}

// Control characters stay escaped, so a name cannot forge extra diagnostic lines.
func TestRefusedGitlinkEscapesControlCharacters(t *testing.T) {
	t.Parallel()
	path := "nested\nrepo\tmodule"
	err := refusedGitlink(t, path)
	reported := refusedPath(t, err)
	if reported != path {
		t.Fatalf("reported path = %q; want the staged path with its control characters preserved", reported)
	}
	if strings.ContainsAny(err.Error(), "\n\t\r\x1b") {
		t.Fatalf("refusal contains a raw control character: %q", err)
	}
}

// An ordinary long path is bounded and visibly marked as cut.
func TestRefusedGitlinkBoundsOrdinaryLongPath(t *testing.T) {
	t.Parallel()
	path := strings.Join([]string{strings.Repeat("a", 200), strings.Repeat("b", 200), strings.Repeat("c", 100)}, "/")
	err := refusedGitlink(t, path)
	reported := refusedPath(t, err)
	if runes := []rune(reported); len(runes) > 201 {
		t.Fatalf("reported path is %d characters; want at most 201", len(runes))
	}
	if !strings.HasPrefix(reported, strings.Repeat("a", 200)) {
		t.Fatalf("reported path = %q; want the start of the staged path", reported)
	}
	if strings.HasPrefix(path, reported) {
		t.Fatalf("reported path = %q; want a visible cut marker", reported)
	}
	if len(err.Error()) > 512 {
		t.Fatalf("refusal is %d bytes; want a bounded diagnostic", len(err.Error()))
	}
}

// Short harmless paths, and short paths holding a secret, keep their current shape.
func TestRefusedGitlinkReportsShortPathsUnchanged(t *testing.T) {
	t.Parallel()
	if reported := refusedPath(t, refusedGitlink(t, "vendor/module")); reported != "vendor/module" {
		t.Fatalf("reported path = %q; want the staged path unchanged", reported)
	}
	if reported := refusedPath(t, refusedGitlink(t, "vendor/"+envSecretValue)); reported != "vendor/[redacted]" {
		t.Fatalf("reported path = %q; want the short secret redacted in place", reported)
	}
}
