package config

import (
	"slices"
	"strings"
)

var manualMergeNames = []string{
	".github", ".circleci", ".buildkite", ".gitlab-ci.yml", ".travis.yml",
	"jenkinsfile", "azure-pipelines.yml", "makefile",
	"agents.md", "security.md", "codeowners", ".devin", ".agents", ".claude", ".cursor",
	"deploy", "deployment", "deployments", "migrations", "migration", "schema.sql",
	"dockerfile", "containerfile", ".dockerignore", "compose.yml", "compose.yaml",
	"docker-compose.yml", "docker-compose.yaml",
	".gitmodules", ".gitattributes",
}

func ManualMergePath(name string, excluded []string) bool {
	components := strings.Split(name, "/")
	for _, component := range components {
		lower := strings.ToLower(component)
		if slices.Contains(manualMergeNames, lower) ||
			strings.HasPrefix(lower, "dockerfile.") || strings.HasSuffix(lower, ".dockerfile") ||
			strings.HasPrefix(lower, "containerfile.") || strings.HasSuffix(lower, ".containerfile") ||
			((strings.HasPrefix(lower, "compose.") || strings.HasPrefix(lower, "docker-compose.")) &&
				(strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml"))) {
			return true
		}
	}
	for _, entry := range excluded {
		entry = strings.TrimSuffix(entry, "/")
		if !strings.Contains(entry, "/") {
			if slices.Contains(components, entry) {
				return true
			}
		} else if name == entry || strings.HasPrefix(name, entry+"/") {
			return true
		}
	}
	return false
}
