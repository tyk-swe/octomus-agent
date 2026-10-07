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
	".gitmodules", ".gitattributes",
}

func ManualMergePath(name string, excluded []string) bool {
	components := strings.Split(name, "/")
	for _, component := range components {
		if slices.Contains(manualMergeNames, strings.ToLower(component)) {
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
