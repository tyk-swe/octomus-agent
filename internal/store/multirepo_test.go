// Multi-repository scoping: settings, budgets and the instance ceiling stay per repository.

package store_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func saveRepoConfig(t *testing.T, s *store.Store, repo string, edit func(*config.Config)) config.Config {
	t.Helper()
	scoped := s.ForRepository(repo)
	c := config.Default()
	c.GitHubRepo = repo
	edit(&c)
	must(t, store.Put(scoped, scoped.ConfigKey(), c))
	return c
}

func TestRepositoryScopedSettingsIsolation(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	one := saveRepoConfig(t, s, "acme/one", func(c *config.Config) { c.MaxSessionsPerDay = 11 })
	two := saveRepoConfig(t, s, "acme/two", func(c *config.Config) { c.MaxSessionsPerDay = 22 })

	gotOne, err := store.Get(s.ForRepository("acme/one"), s.ForRepository("acme/one").ConfigKey())
	must(t, err)
	if gotOne == nil || gotOne.MaxSessionsPerDay != one.MaxSessionsPerDay || gotOne.GitHubRepo != "acme/one" {
		t.Fatalf("repository one configuration differs: %+v", gotOne)
	}
	gotTwo, err := store.Get(s.ForRepository("acme/two"), s.ForRepository("acme/two").ConfigKey())
	must(t, err)
	if gotTwo == nil || gotTwo.MaxSessionsPerDay != two.MaxSessionsPerDay || gotTwo.GitHubRepo != "acme/two" {
		t.Fatalf("repository two configuration differs: %+v", gotTwo)
	}
	legacy, err := store.Get(s, store.ConfigKey)
	must(t, err)
	if legacy != nil {
		t.Fatalf("scoped configurations leaked into the unscoped record: %+v", legacy)
	}

	control := model.DefaultControl()
	control.CycleNumber = 7
	must(t, s.ForRepository("acme/one").SaveControl(control))
	other, err := store.Get(s.ForRepository("acme/two"), s.ForRepository("acme/two").ControlKey())
	must(t, err)
	if other != nil {
		t.Fatalf("control leaked across repositories: %+v", other)
	}
}

func TestRepositoryRegistry(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: "acme/one", DataDir: "."}))
	must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: "acme/two", DataDir: "repos/acme-two"}))
	// Re-registering the same repository (any ASCII case) is a no-op, not a duplicate.
	must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: "ACME/One", DataDir: "elsewhere"}))
	entries, err := s.Repositories()
	must(t, err)
	if len(entries) != 2 {
		t.Fatalf("registry has %d entries: %+v", len(entries), entries)
	}
	if entries[0].GitHubRepo != "acme/one" || entries[0].DataDir != "." {
		t.Fatalf("first registry entry differs: %+v", entries[0])
	}
	if entries[1].GitHubRepo != "acme/two" || entries[1].DataDir != "repos/acme-two" {
		t.Fatalf("second registry entry differs: %+v", entries[1])
	}
}

func TestRepositoryDirectoryIdentitiesDoNotCollide(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	repos := []string{"acme-tools/service", "acme/tools-service", "acme/one-two", "acme/one_two"}
	seen := map[string]bool{}
	for _, repo := range repos {
		slug := store.RepositorySlug(repo)
		if seen[slug] || slug != filepath.Base(slug) {
			t.Fatalf("unsafe or duplicate repository directory: %q", slug)
		}
		seen[slug] = true
		must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: repo, DataDir: "repos/" + slug}))
	}
	if store.RepositorySlug("ACME/ONE_TWO") != store.RepositorySlug("acme/one_two") {
		t.Fatal("directory identity depends on GitHub identity casing")
	}
}

func TestRepositoryScopedEvents(t *testing.T) {
	t.Parallel()
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgraded-v0.1.0"}[migrated], func(t *testing.T) {
			path := statePath(t)
			if migrated {
				path = golden(t)
			}
			s := open(t, path)
			entries, err := s.Repositories()
			must(t, err)
			primary := "acme/one"
			if len(entries) != 0 {
				primary = entries[0].GitHubRepo
			}
			must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: primary, DataDir: "."}))
			must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: "acme/two", DataDir: "repos/two"}))
			for _, repo := range []string{primary, "acme/two"} {
				scoped := s.ForRepository(repo)
				must(t, scoped.Event("system", model.EventConfiguration, "Configuration for "+repo))
				task := task()
				task.Config.GitHubRepo = repo
				must(t, store.Put(s, store.Tasks.Key(task.ID), task))
				// Root writes of record activity must infer ownership too.
				must(t, s.Event(task.ID, model.EventWorkerError, "Failure for "+repo))
				other := "acme/two"
				if repo == other {
					other = primary
				}
				foreign, err := s.ForRepository(other).Events(&task.ID)
				must(t, err)
				if len(foreign) != 0 {
					t.Fatalf("%s can read %s's activity by entity: %+v", other, repo, foreign)
				}
			}
			for _, repo := range []string{primary, "acme/two"} {
				scoped := s.ForRepository(repo)
				events, err := scoped.Events(nil)
				must(t, err)
				dashboard, err := scoped.Dashboard()
				must(t, err)
				for _, view := range [][]model.Event{events, dashboard.Events} {
					seen := map[string]bool{}
					for _, event := range view {
						seen[event.Message] = true
					}
					if !seen["Configuration for "+repo] || !seen["Failure for "+repo] {
						t.Fatalf("%s lost its own events", repo)
					}
					for _, other := range []string{primary, "acme/two"} {
						if other != repo && (seen["Configuration for "+other] || seen["Failure for "+other]) {
							t.Fatalf("%s shows %s's activity", repo, other)
						}
					}
				}
			}
		})
	}
}

func TestRepositoryEventRetention(t *testing.T) {
	t.Parallel()
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgraded-v0.1.0"}[migrated], func(t *testing.T) {
			path := statePath(t)
			if migrated {
				path = golden(t)
			}
			s := open(t, path)
			entries, err := s.Repositories()
			must(t, err)
			primary := "acme/one"
			if len(entries) != 0 {
				primary = entries[0].GitHubRepo
			}
			must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: primary, DataDir: "."}))
			must(t, s.RegisterRepository(store.RepositoryEntry{GitHubRepo: "acme/two", DataDir: "repos/two"}))
			saveRepoConfig(t, s, primary, func(c *config.Config) { c.RetainEvents = 2 })
			saveRepoConfig(t, s, "acme/two", func(c *config.Config) { c.RetainEvents = 4 })
			one, two := s.ForRepository(primary), s.ForRepository("acme/two")
			for range 4 {
				must(t, two.Event("system", model.EventConfiguration, "Secondary activity"))
			}
			for range 7 {
				must(t, one.Event("system", model.EventConfiguration, "Primary activity"))
			}
			assertCount := func(scoped *store.Store, want int) {
				t.Helper()
				events, err := scoped.Events(nil)
				must(t, err)
				if len(events) != want {
					t.Fatalf("%s retained %d events, want %d", scoped.Repository(), len(events), want)
				}
			}
			assertCount(one, 2)
			assertCount(two, 4)
			// Unscoped writes must be attributed before the retention trigger
			// chooses a repository's policy and rows to prune.
			task := task()
			task.Config.GitHubRepo = "acme/two"
			must(t, store.Put(s, store.Tasks.Key(task.ID), task))
			for range 3 {
				must(t, s.Event(task.ID, model.EventWorkerError, "Secondary task activity"))
			}
			assertCount(one, 2)
			assertCount(two, 4)
			must(t, one.PruneEvents(1))
			assertCount(one, 1)
			assertCount(two, 4)
			must(t, two.PruneEvents(2))
			assertCount(one, 1)
			assertCount(two, 2)
		})
	}
}

func TestRepositoryScopedTaskLists(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	for _, repo := range []string{"acme/one", "acme/two"} {
		task := task()
		task.Config.GitHubRepo = repo
		task.Proposal.Target = "octomus/work"
		must(t, store.Put(s, store.Tasks.Key(task.ID), task))
	}
	one := s.ForRepository("acme/one")
	scheduling, err := one.SchedulingTasks(nil)
	must(t, err)
	if len(scheduling) != 1 || scheduling[0].Config.GitHubRepo != "acme/one" {
		t.Fatalf("scheduling leaked across repositories: %+v", scheduling)
	}
	page, err := s.ForRepository("acme/two").TaskPage(store.HistoryQuery{})
	must(t, err)
	if len(page.Items) != 1 || page.Counts["all"] != 1 {
		t.Fatalf("task page leaked across repositories: items %d counts %+v", len(page.Items), page.Counts)
	}
	dashboard, err := one.Dashboard()
	must(t, err)
	if len(dashboard.Tasks) != 1 {
		t.Fatalf("dashboard leaked across repositories: %d tasks", len(dashboard.Tasks))
	}
	unresolved, err := one.HasUnresolvedTasks()
	must(t, err)
	if !unresolved {
		t.Fatal("queued task not unresolved in its own repository")
	}
}

func TestPerRepositoryBudgetAndInstanceCeiling(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	saveRepoConfig(t, s, "acme/one", func(c *config.Config) { c.MaxSessionsPerDay = 1 })
	saveRepoConfig(t, s, "acme/two", func(c *config.Config) { c.MaxSessionsPerDay = 5 })
	must(t, s.SetInstanceSettings(config.InstanceSettings{MaxSessionsPerDay: 2}))

	one := s.ForRepository("acme/one")
	two := s.ForRepository("acme/two")
	route := config.NewRoute("fixture", "low")
	must(t, one.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", route)))
	// Repository one's own cap is now exhausted.
	if err := one.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", route)); !errors.Is(err, model.BlockedBudgetExhausted) {
		t.Fatalf("repository one exceeded its own cap: %v", err)
	}
	// Repository two still has its own budget; one reservation reaches the instance ceiling of two.
	must(t, two.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", route)))
	if err := two.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", route)); !errors.Is(err, model.BlockedBudgetExhausted) {
		t.Fatalf("instance ceiling not enforced across repositories: %v", err)
	}

	capOne, err := one.PlanningCapacity(time.Now())
	must(t, err)
	if capOne.Used != 1 || capOne.Limit != 1 {
		t.Fatalf("repository one capacity differs: %+v", capOne)
	}
	capTwo, err := two.PlanningCapacity(time.Now())
	must(t, err)
	if capTwo.Used != 1 || capTwo.Limit != 5 {
		t.Fatalf("repository two capacity differs: %+v", capTwo)
	}
	if capTwo.Remaining != 0 {
		t.Fatalf("planning capacity ignores the instance ceiling: %+v", capTwo)
	}
}
