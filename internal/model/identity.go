package model

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

func DecisionMemoryFingerprint(revision string, paths []string, treeOutput string) (string, error) {
	if len(paths) > 40 {
		return "", fmt.Errorf("Decision has too many relevant paths")
	}
	for _, path := range paths {
		if path == "" || strings.HasPrefix(path, "/") {
			return "", fmt.Errorf("Decision paths must be repository-relative files")
		}
		for i, part := range strings.Split(path, "/") {
			if part == ".." || (part == "." && i == 0) {
				return "", fmt.Errorf("Decision paths must be repository-relative files")
			}
		}
	}
	if len(paths) == 0 {
		return revision, nil
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(treeOutput)))), nil
}
