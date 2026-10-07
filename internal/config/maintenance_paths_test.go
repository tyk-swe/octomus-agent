package config

import "testing"

func TestManualMergePaths(t *testing.T) {
	for _, test := range []struct {
		path     string
		excluded []string
		manual   bool
	}{
		{".github/workflows/verify.yml", nil, true},
		{"internal/store/migrations/update.sql", nil, true},
		{"db/MIGRATIONS/update.sql", nil, true},
		{"deploy/docker/env.example", nil, true},
		{"nested/SECURITY.md", nil, true},
		{"internal/database/schema.sql", nil, true},
		{"nested/AGENTS.md", nil, true},
		{"web/src/lib/Settings.svelte", nil, false},
		{"internal/store/queries.go", nil, false},
		{"docs/migrations-guide.md", nil, false},
		{"deployable.txt", nil, false},
		{"Dockerfile", nil, true},
		{"DOCKERFILE", nil, true},
		{"containers/Dockerfile.prod", nil, true},
		{"service/build.Dockerfile", nil, true},
		{"Containerfile", nil, true},
		{"Containerfile.prod", nil, true},
		{".dockerignore", nil, true},
		{"compose.yaml", nil, true},
		{"compose.yml", nil, true},
		{"compose.override.yaml", nil, true},
		{"docker-compose.yml", nil, true},
		{"docker-compose.yaml", nil, true},
		{"docker-compose.prod.yaml", nil, true},
		{"docs/dockerfile-guide.md", nil, false},
		{"compose-logic.go", nil, false},
		{"docker-compose-helper.ts", nil, false},
		{"internal/auth/rules.go", []string{"internal/auth/"}, true},
		{"internal/auth/rules.go", []string{"internal/aut/"}, false},
		{"service/private.policy", []string{"private.policy"}, true},
		{"private.policy.example", []string{"private.policy"}, false},
		{"internal/auth/rules.go", []string{"internal/auth/rules.go"}, true},
		{".github/workflows/verify.yml", []string{}, true},
	} {
		if got := ManualMergePath(test.path, test.excluded); got != test.manual {
			t.Errorf("ManualMergePath(%q,%v) = %t; want %t", test.path, test.excluded, got, test.manual)
		}
	}
}
