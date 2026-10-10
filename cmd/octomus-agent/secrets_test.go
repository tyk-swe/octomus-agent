package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func TestRestrictedMergerSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merger")
	if err := os.WriteFile(path, []byte("restricted-merger-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"OCTOMUS_MERGE_TOKEN_FILE": path}
	env := func(k string) (string, bool) { v, ok := values[k]; return v, ok }
	loaded, err := loadSecretFiles(env, func(k, v string) error { values[k] = v; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := loaded(config.MergeTokenEnv); !ok || value != "restricted-merger-secret" {
		t.Fatal("merger file was not loaded")
	}
	if _, err := loadSecretFiles(env, func(string, string) error { t.Fatal("conflicting secrets were applied"); return nil }); err == nil {
		t.Fatal("both merger environment and file were accepted")
	}
}
