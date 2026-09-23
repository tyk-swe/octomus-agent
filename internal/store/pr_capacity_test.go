package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// capacityPR is an owned pull request in the fixture repository.
func capacityPR(number uint64, branch string) model.PullRequest {
	return model.PullRequest{
		Number: number, Title: "Owned", Branch: branch, Head: strings.Repeat("b", 40), Base: "main",
		URL: fmt.Sprintf("https://example.invalid/pull/%d", number), State: "open", ChangedLines: 1,
		CreatedAt: model.Now(), Owned: true, HeadRepository: "fixture/project", BaseRepository: "fixture/project",
	}
}

// Two open PRs from one head branch into different bases consume two slots.
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

// with no prior delivery record, a poll adopts the newest published output
// for that PR rather than treating the observed remote head as delivered.
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
