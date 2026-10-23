package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// RepositoryEntry is one repository this instance serves. DataDir is the
// repository's workspace root relative to the instance data directory;
// "." (or empty, normalized to ".") is the data directory itself, which is
// where the original single repository lives, so existing task workspaces
// keep their paths when more repositories are added.
type RepositoryEntry struct {
	GitHubRepo string `json:"github_repo"`
	DataDir    string `json:"data_dir"`
}

func (v *RepositoryEntry) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v RepositoryEntry) MarshalJSON() ([]byte, error) {
	type plain RepositoryEntry
	return wirejson.Record(plain(v))
}

type repositoryRegistry struct {
	Repositories []RepositoryEntry `json:"repositories"`
}

func (v *repositoryRegistry) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v repositoryRegistry) MarshalJSON() ([]byte, error) {
	type plain repositoryRegistry
	return wirejson.Record(plain(v))
}

// RepositorySlug maps a GitHub repository ("owner/name") to a single safe
// directory name for its workspace root under the data directory.
func RepositorySlug(repo string) string {
	return url.PathEscape(strings.ToLower(repo))
}

func normalizeEntry(e RepositoryEntry) (RepositoryEntry, error) {
	if e.GitHubRepo == "" {
		return RepositoryEntry{}, errors.New("Repository identity must not be empty")
	}
	if e.DataDir == "" {
		e.DataDir = "."
	}
	clean := filepath.Clean(e.DataDir)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return RepositoryEntry{}, fmt.Errorf("Repository data directory %q must stay inside the instance data directory", e.DataDir)
	}
	e.DataDir = clean
	return e, nil
}

// Repositories lists the registered repositories in registration order. A
// database with no registry record (fresh, or never multi-repository)
// reports the single legacy repository from its saved configuration, if any,
// rooted at the data directory itself.
func (s *Store) Repositories() ([]RepositoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	registry, err := recordAt(s.conn, repositoriesKey)
	if err != nil {
		return nil, err
	}
	if registry != nil {
		return append([]RepositoryEntry{}, registry.Repositories...), nil
	}
	cfg, err := recordAt(s.conn, ConfigKey)
	if err != nil {
		return nil, err
	}
	if cfg == nil || cfg.GitHubRepo == "" {
		return []RepositoryEntry{}, nil
	}
	return []RepositoryEntry{{GitHubRepo: cfg.GitHubRepo, DataDir: "."}}, nil
}

// RegisterRepository adds a repository to the registry. Registering an
// already-known repository (compared ASCII-case-insensitively, like GitHub
// identities elsewhere) is a no-op that keeps the original entry.
func (s *Store) RegisterRepository(entry RepositoryEntry) error {
	entry, err := normalizeEntry(entry)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.transaction(true, func(c *sql.Conn) error {
		registry, err := recordAt(c, repositoriesKey)
		if err != nil {
			return err
		}
		var next repositoryRegistry
		if registry != nil {
			next = *registry
		} else if cfg, err := recordAt(c, ConfigKey); err != nil {
			return err
		} else if cfg != nil && cfg.GitHubRepo != "" {
			// Materialize the implicit legacy repository first so it keeps
			// the data-directory root and first position.
			next.Repositories = []RepositoryEntry{{GitHubRepo: cfg.GitHubRepo, DataDir: "."}}
		}
		for _, existing := range next.Repositories {
			if config.EqualASCII(existing.GitHubRepo, entry.GitHubRepo) {
				return nil
			}
			if existing.DataDir == entry.DataDir {
				return fmt.Errorf("Repository data directory %q already belongs to %s", entry.DataDir, existing.GitHubRepo)
			}
		}
		next.Repositories = append(next.Repositories, entry)
		return txPut(c, repositoriesKey, next)
	})
	if err == nil && s.primary == "" {
		s.loadPrimary()
	}
	return err
}

// InstanceSettings returns the saved instance-wide policy (zero when unset).
func (s *Store) InstanceSettings() (config.InstanceSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return storedInstanceSettings(s.conn)
}

func storedInstanceSettings(c *sql.Conn) (config.InstanceSettings, error) {
	stored, err := recordAt(c, instanceKey)
	if err != nil || stored == nil {
		return config.InstanceSettings{}, err
	}
	return *stored, nil
}

// SetInstanceSettings saves the instance-wide policy.
func (s *Store) SetInstanceSettings(settings config.InstanceSettings) error {
	return Put(s, instanceKey, settings)
}

// RecordRepository reports which repository owns one record, for routing
// ID-addressed reads and actions to that repository's engine. found is
// false when no such record exists (or it carries no repository).
func (s *Store) RecordRepository(kind, id string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var query string
	switch kind {
	case "task", "cycle", "pr":
		query = "SELECT repository FROM record_meta WHERE kind=?1 AND id=?2"
	case "baseline":
		query = "SELECT COALESCE(json_extract(data,'$.config.github_repo'),'') FROM records WHERE kind='baseline' AND id=?2"
	case "request":
		query = "SELECT COALESCE(json_extract(data,'$.repository'),'') FROM records WHERE kind='request' AND id=?2"
	default:
		return "", false, fmt.Errorf("Unknown record kind %q", kind)
	}
	var repo string
	err := s.conn.QueryRowContext(background, query, kind, id).Scan(&repo)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return repo, repo != "", nil
}
