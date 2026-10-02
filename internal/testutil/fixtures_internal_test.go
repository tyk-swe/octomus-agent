package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindFixtureDirectoryValidatesTheCheckout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, module string
		fixtures     bool
		want         bool
	}{
		{"plain module", "module github.com/tyk-swe/octomus-agent\n", true, true},
		{"quoted module", "module \"github.com/tyk-swe/octomus-agent\" // checkout\n", true, true},
		{"other module", "module example.com/other\n", true, false},
		{"module prefix", "module github.com/tyk-swe/octomus-agent-other\n", true, false},
		{"comment only", "// module github.com/tyk-swe/octomus-agent\n", true, false},
		{"missing fixtures", "module github.com/tyk-swe/octomus-agent\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(tc.module), 0o644); err != nil {
				t.Fatal(err)
			}
			fixtures := filepath.Join(root, "tests", "fixtures")
			if tc.fixtures {
				if err := os.MkdirAll(fixtures, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			nested := filepath.Join(root, "internal", "nested")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.want {
				want = fixtures
			}
			if got := findFixtureDirectory(nested); got != want {
				t.Fatalf("fixture directory = %q; want %q", got, want)
			}
		})
	}
}

func TestFixtureDirectoryUsesAbsoluteSourceOrTrimmedInitialDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fixtures := filepath.Join(root, "tests", "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/tyk-swe/octomus-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	absoluteSource := filepath.Join(root, "internal", "testutil", "fixtures.go")
	if got := fixtureDirectoryFromSource(absoluteSource, t.TempDir()); got != fixtures {
		t.Fatalf("absolute source fixture directory = %q; want %q", got, fixtures)
	}
	trimmedSource := "github.com/tyk-swe/octomus-agent/internal/testutil/fixtures.go"
	if got := fixtureDirectoryFromSource(trimmedSource, filepath.Join(root, "internal", "testutil")); got != fixtures {
		t.Fatalf("trimmed source fixture directory = %q; want %q", got, fixtures)
	}
	if got := fixtureDirectoryFromSource(trimmedSource, ""); filepath.IsAbs(got) {
		t.Fatalf("missing checkout unexpectedly resolved to %q", got)
	}
}
