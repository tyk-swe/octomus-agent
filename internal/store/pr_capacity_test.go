package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func capacityPR(number uint64, branch string) model.PullRequest {
	return model.PullRequest{
		Number: number, Title: "Owned", Branch: branch, Head: strings.Repeat("b", 40), Base: "main",
		URL: fmt.Sprintf("https://example.invalid/pull/%d", number), State: "open", ChangedLines: 1,
		CreatedAt: model.Now(), Owned: true, HeadRepository: "fixture/project", BaseRepository: "fixture/project",
	}
}

func TestSharedBranchPullRequestsEachConsumeCapacity(t *testing.T) {
	s := open(t, statePath(t))
	saveConfig(t, s, func(c *config.Config) { c.GitHubRepo = "fixture/project"; c.MaxOpenPRs = 2 })
	secondBase := capacityPR(8, "octomus/shared")
	secondBase.Base = "release"
	inv := inventory(capacityPR(7, "octomus/shared"), secondBase)
	persisted, err := s.PersistPrInventory(inv, nil)
	must(t, err)
	if !persisted {
		t.Fatal("inventory was not persisted")
	}
	reservations, err := s.PrReservations("fixture/project")
	must(t, err)
	observed, _, remaining := store.PrUnion(inv, reservations, 2)
	if observed != 2 || remaining != 0 {
		t.Fatalf("shared-branch PRs collapsed into one slot: observed=%d remaining=%d", observed, remaining)
	}
	queued := task()
	queued.Branch = "octomus/next"
	must(t, s.Put("task", queued.ID, queued))
	admitted, err := s.AdmitNewPrTask(&queued, inv)
	must(t, err)
	if admitted || queued.Status != model.StatusQueued {
		t.Fatalf("admission beyond two shared-branch PRs succeeded: admitted=%t status=%s", admitted, queued.Status)
	}
}

func TestRespelledRepositoryPathKeepsThePrIdentity(t *testing.T) {
	s := open(t, statePath(t))
	queued := task()
	queued.Config.Repository = "/srv/repo"
	queued.Branch = queued.Config.BranchPrefix + "respelled"
	live := saveConfig(t, s, func(c *config.Config) {
		c.GitHubRepo = "fixture/project"
		c.MaxOpenPRs = 1
		c.Repository = "/srv//repo/"
	})
	identity := store.PrIdentityOf(queued.Config)
	if !identity.Matches(live) {
		t.Fatal("a respelled repository path changed the PR identity")
	}
	other := live.Clone()
	other.BranchPrefix = "other/"
	if identity.Matches(other) {
		t.Fatal("a different branch prefix kept the PR identity")
	}
	must(t, s.Put("settings", "pr_inventory", inventory()))
	must(t, s.Put("task", queued.ID, queued))
	admitted, err := s.AdmitNewPrTask(&queued, inventory())
	must(t, err)
	if !admitted || queued.Status != model.StatusExecuting {
		t.Fatalf("respelled repository path refused admission: admitted=%t status=%s", admitted, queued.Status)
	}
}

func TestPrObservationFallsBackToTheLatestPublishedOutput(t *testing.T) {
	s := open(t, statePath(t))
	saveConfig(t, s, func(c *config.Config) { c.GitHubRepo = "fixture/project"; c.MaxOpenPRs = 3 })
	delivered := task()
	delivered.Status = model.StatusPublished
	delivered.OutputCommit = str(strings.Repeat("f", 40))
	number := uint64(9)
	delivered.PRNumber = &number
	must(t, s.Put("task", delivered.ID, delivered))
	remote := capacityPR(9, "octomus/delivered")
	remote.Head = strings.Repeat("d", 40)
	must(t, s.RecordPrObservation("fixture/project", remote, false))
	_, observation, err := s.PrObservation("fixture/project", 9)
	must(t, err)
	if observation == nil || observation.DeliveredHead == nil || *observation.DeliveredHead != *delivered.OutputCommit {
		t.Fatalf("poll did not adopt the published output as the delivery baseline: %+v", observation)
	}
	if !observation.ExternalHeadMovement {
		t.Fatalf("remote head differing from the published output was not flagged: %+v", observation)
	}
}
