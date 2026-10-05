package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	observeInterval     = 5 * time.Minute
	observationLifetime = 2 * observeInterval
	prAdmissionLifetime = time.Minute
	prRefreshRetryDelay = time.Minute
)

func (a *App) defaultBranchSHA(ctx context.Context, cfg config.Config) (string, string, error) {
	observedAt := model.Now()
	revision, err := gitops.RemoteRevision(ctx, cfg, cfg.DefaultBranch)
	if err != nil {
		return "", "", err
	}
	if revision == nil || *revision == "" {
		if err := a.observeDefaultBranch(cfg, "", observedAt); err != nil {
			return "", "", err
		}
		return "", "", errors.New("Default branch missing on remote")
	}
	return *revision, observedAt, nil
}

func (a *App) observeDefaultBranch(cfg config.Config, revision, observedAt string) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return err
	}
	if !live.SameRemoteIdentity(cfg) {
		return errors.New("Configuration identity changed during remote observation")
	}
	return a.observeLocked(cfg, revision, observedAt)
}

func (a *App) observeLocked(cfg config.Config, revision, observedAt string) error {
	observed, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		return err
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if existing := a.runtime.defaultObservation; existing != nil {
		sameTarget := existing.Describes(cfg)
		newer := true
		if at, parseErr := time.Parse(time.RFC3339Nano, existing.ObservedAt); parseErr == nil {
			newer = !at.Before(observed)
		}
		if sameTarget && newer {
			return nil
		}
	}
	a.runtime.defaultObservation = &model.DefaultBranchObservation{
		Repository: cfg.GitHubRepo, DefaultBranch: cfg.DefaultBranch, Revision: revision, ObservedAt: observedAt,
	}
	// An empty revision records a successful read of a missing branch. Keep its
	// timestamp so an older in-flight read cannot restore the previous revision.
	return nil
}

func (a *App) observeRemote(ctx context.Context, cfg config.Config) error {
	if err := gitops.ValidateRemote(ctx, cfg); err != nil {
		return err
	}
	if err := a.refreshPRs(ctx, cfg); errors.Is(err, errPRPolicyChanged) {
		return nil
	} else if err != nil && !errors.Is(err, errStaleInventory) {
		return err
	}
	inventory, err := a.Store.OpenPRInventory()
	if err != nil || inventory == nil {
		if err == nil {
			err = errors.New("PR refresh did not persist an inventory")
		}
		return err
	}
	open := map[uint64]struct{}{}
	for _, pr := range inventory.PRs {
		open[pr.Number] = struct{}{}
	}
	before := (*int64)(nil)
	status := "open"
	limit := 100
	closed := []model.PullRequest{}
	for {
		page, err := a.Store.HistoryPage("pr", store.HistoryQuery{Before: before, Status: &status, Limit: &limit})
		if err != nil {
			return err
		}
		for _, raw := range page.Items {
			if ctx.Err() != nil {
				return nil
			}
			var summary struct {
				Repository string `json:"repository"`
				PR         struct {
					Number uint64 `json:"number"`
				} `json:"pr"`
			}
			if json.Unmarshal(raw, &summary) != nil || !config.EqualASCII(summary.Repository, cfg.GitHubRepo) {
				continue
			}
			if _, present := open[summary.PR.Number]; !present {
				pr, err := gitops.PR(ctx, cfg, summary.PR.Number)
				if err != nil {
					return err
				}
				closed = append(closed, pr)
			}
		}
		before = page.NextCursor
		if before == nil {
			break
		}
	}
	observedAt := model.Now()
	revision, err := gitops.RemoteRevision(ctx, cfg, cfg.DefaultBranch)
	if err != nil {
		return err
	}
	revisionValue := ""
	if revision != nil {
		revisionValue = *revision
	}
	fingerprint := contextFingerprint(revisionValue, inventory.PRs)
	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return err
	}
	if !live.SameRemoteIdentity(cfg) {
		return nil
	}
	if err := a.observeLocked(cfg, revisionValue, observedAt); err != nil {
		return err
	}
	for _, pr := range closed {
		if err := a.Store.RecordPRObservation(cfg.GitHubRepo, pr, false); err != nil {
			return err
		}
	}
	control, err := a.Control()
	if err != nil {
		return err
	}
	applyContextFingerprint(&control, fingerprint, time.Now(), cfg.CycleIntervalSeconds)
	return a.Store.SaveControl(control)
}

func applyContextFingerprint(control *model.Control, fingerprint string, now time.Time, interval uint64) {
	if control.ContextFingerprint != "" && control.ContextFingerprint != fingerprint {
		if control.IdleStreak > 1 {
			ordinary := now.Unix() + int64(interval)
			if control.NextCycleAt > ordinary {
				control.NextCycleAt = ordinary
			}
		}
		control.IdleStreak = 0
	}
	control.ContextFingerprint = fingerprint
}

func contextFingerprint(revision string, prs []model.PullRequest) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		parts = append(parts, fmt.Sprintf("%d:%s:%s:%s:%s:%s:%s", pr.Number, pr.Head, pr.Base, pr.State,
			pr.ReviewDecision, pr.CheckStatus, pr.Mergeability))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(revision+"\n"+strings.Join(parts, "\n"))))
}

func (a *App) refreshPRs(ctx context.Context, snapshot config.Config) (result error) {
	startedAt := time.Now()
	defer func() {
		if result == nil || ctx.Err() != nil || errors.Is(result, context.Canceled) || errors.Is(result, errPRPolicyChanged) || errors.Is(result, errStaleInventory) {
			return
		}
		a.gate.Lock()
		defer a.gate.Unlock()
		if live, err := a.Config(); err == nil && !config.SamePRPolicy(snapshot, live) {
			result = errPRPolicyChanged
			return
		}
		a.runtimeMu.Lock()
		observation := a.runtime.prObservation
		if observation == nil || (config.SamePRPolicy(observation.policy, snapshot) && !observation.fetchedAt.After(startedAt)) {
			a.runtime.prObservation = nil
			a.runtime.prRefreshError = redact.Error(result)
		}
		a.runtimeMu.Unlock()
	}()
	observed, err := a.observeOpenPRs(ctx, snapshot)
	if err != nil {
		return err
	}

	a.gate.Lock()
	defer a.gate.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	live, err := a.Config()
	if err != nil {
		return err
	}
	if !config.SamePRPolicy(snapshot, live) {
		return errPRPolicyChanged
	}
	persisted, err := a.savePRsLocked(live, observed)
	if err != nil {
		return err
	}
	if !persisted {
		return errStaleInventory
	}
	return nil
}

type prSnapshot struct {
	inventory model.OpenPRInventory
	owned     []model.PullRequest
	released  []string
}

func (a *App) observeOpenPRs(ctx context.Context, cfg config.Config) (prSnapshot, error) {
	inventory, err := gitops.OpenPRs(ctx, cfg)
	if err != nil {
		return prSnapshot{}, fmt.Errorf("Open pull request inventory failed: %w", err)
	}
	owned, err := gitops.OwnedPRs(ctx, cfg, inventory)
	if err != nil {
		return prSnapshot{}, fmt.Errorf("Owned pull request refresh failed: %w", err)
	}
	overlayOwnedDetails(&inventory, owned)
	released, err := a.releasableReservations(ctx, cfg, inventory)
	if err != nil {
		return prSnapshot{}, err
	}
	return prSnapshot{inventory: inventory, owned: owned, released: released}, nil
}

func overlayOwnedDetails(inventory *model.OpenPRInventory, details []model.PullRequest) {
	byNumber := make(map[uint64]model.PullRequest, len(details))
	for _, detail := range details {
		byNumber[detail.Number] = detail
	}
	for i, observed := range inventory.PRs {
		if detail, ok := byNumber[observed.Number]; ok {
			inventory.PRs[i] = detail
		}
	}
}

func (a *App) savePRsLocked(observed config.Config, snapshot prSnapshot) (bool, error) {
	control, err := a.Control()
	if err != nil {
		return false, err
	}
	persisted, err := a.Store.PersistPRInventory(snapshot.inventory, snapshot.released)
	if err != nil || !persisted {
		return false, err
	}
	for _, pr := range snapshot.owned {
		if err := a.Store.RecordPRObservation(observed.GitHubRepo, pr, false); err != nil {
			return true, err
		}
	}
	a.runtimeMu.Lock()
	if control.Mode != model.OperatingModePaused {
		a.runtime.prObservation = &freshPRs{policy: observed, inventory: snapshot.inventory.Clone(), fetchedAt: time.Now()}
	}
	a.runtime.prRefreshError = ""
	a.runtimeMu.Unlock()
	return true, nil
}

func (a *App) releasableReservations(ctx context.Context, cfg config.Config, inventory model.OpenPRInventory) ([]string, error) {
	reservations, err := a.Store.PRReservations(cfg.GitHubRepo)
	if err != nil {
		return nil, err
	}
	represented := inventory.OwnedBranches()
	released := []string{}
	for _, reservation := range reservations {
		if _, open := represented[reservation.Branch]; open {
			continue
		}
		task, err := store.Get[model.Task](a.Store, "task", reservation.TaskID)
		if err != nil {
			return nil, err
		}
		if task == nil || task.OutputCommit == nil {
			continue
		}
		published := task.Status == model.StatusPublished && task.PRNumber != nil
		cancelled := task.Status == model.StatusCancelled
		if !published && !cancelled {
			continue
		}
		var detail *model.PullRequest
		if task.PRNumber != nil {
			pr, remoteErr := gitops.PR(ctx, cfg, *task.PRNumber)
			if remoteErr != nil {
				continue
			}
			detail = &pr
		} else {
			detail, err = gitops.PublicationPR(ctx, cfg, reservation.Branch)
			if err != nil {
				continue
			}
		}
		if detail == nil {
			released = append(released, reservation.TaskID)
			continue
		}
		settled := (detail.State == "closed" || detail.State == "merged") &&
			config.EqualASCII(detail.HeadRepository, cfg.GitHubRepo) &&
			config.EqualASCII(detail.BaseRepository, cfg.GitHubRepo) && detail.Branch == reservation.Branch
		if !settled {
			continue
		}
		if detail.Head == *task.OutputCommit {
			released = append(released, reservation.TaskID)
			continue
		}
		marker, markerErr := gitops.TaskMarker(ctx, cfg, task.ID, *detail)
		if markerErr == nil && marker {
			released = append(released, reservation.TaskID)
		}
	}
	return released, nil
}
