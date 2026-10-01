package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	prObservationLifetime = observationLifetime
	prAdmissionLifetime   = time.Minute
	prRefreshRetryDelay   = time.Minute
)

const prCapacityFullReason = "The configured owned open-PR limit is reached; new-PR work waits for an observed closure or merge"

var errPrInventorySuperseded = errors.New("Pull request inventory became stale before persistence")

var errPrPolicyChanged = errors.New("Pull request policy changed during refresh")

type freshPrObservation struct {
	identity          store.PrIdentity
	inventory         model.OpenPrInventory
	fetchedAt         time.Time
	admissionConsumed bool
}

type prRefreshJob struct {
	cancel context.CancelFunc
}

func (a *App) invalidatePrObservation() {
	a.runtimeMu.Lock()
	if a.runtime.prRefresh != nil {
		a.runtime.prRefresh.cancel()
		a.runtime.prRefresh = nil
	}
	a.runtime.prObservation = nil
	a.runtime.prRefreshError = ""
	a.runtime.lastPrAttempt = time.Time{}
	a.runtime.prAdmissionRefused = false
	a.runtimeMu.Unlock()
}

func (a *App) takePrAdmissionInventory(cfg config.Config, now time.Time) (*model.OpenPrInventory, string) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	observation := a.runtime.prObservation
	if observation == nil {
		if a.runtime.prRefreshError != "" {
			return nil, a.runtime.prRefreshError
		}
		return nil, "A current-process pull request observation is required"
	}
	if !observation.identity.Matches(cfg) {
		return nil, "The pull request observation belongs to different repository policy"
	}
	if observation.admissionConsumed {
		return nil, "The pull request observation has already authorized an admission batch"
	}
	observedAt, err := time.Parse(time.RFC3339Nano, observation.inventory.ObservedAt)
	if err != nil || now.Sub(observedAt) > prAdmissionLifetime {
		return nil, "The pull request observation is stale"
	}
	observation.admissionConsumed = true
	copy := observation.inventory.Clone()
	return &copy, ""
}

func (a *App) PrCapacity() (model.PrCapacity, error) {
	cfg, err := a.Config()
	if err != nil {
		return model.PrCapacity{}, err
	}
	return a.prCapacity(cfg)
}

func (a *App) prCapacity(cfg config.Config) (model.PrCapacity, error) {
	reservations, err := a.Store.PrReservations(cfg.GitHubRepo)
	if err != nil {
		return model.PrCapacity{}, err
	}
	stored, err := a.Store.OpenPrInventory()
	if err != nil {
		return model.PrCapacity{}, err
	}
	if stored != nil && !config.EqualASCII(stored.Repository, cfg.GitHubRepo) {
		stored = nil
	}
	var ownedOpen *uint64
	reserved := uint64(len(reservations))
	remaining := uint64(0)
	var observedAt *string
	if stored != nil {
		owned, unrepresented, available := store.PrUnion(*stored, reservations, cfg.MaxOpenPRs)
		ownedOpen, reserved, remaining = &owned, unrepresented, available
		observed := stored.ObservedAt
		observedAt = &observed
	}

	a.runtimeMu.Lock()
	refreshing := a.runtime.prRefresh != nil
	lastError := a.runtime.prRefreshError
	observation := a.runtime.prObservation
	fresh := lastError == "" && observation != nil && observation.identity.Matches(cfg) && time.Since(observation.fetchedAt) <= prObservationLifetime && stored != nil
	a.runtimeMu.Unlock()

	status := "unavailable"
	var reason string
	switch {
	case refreshing:
		status = "refreshing"
		reason = "Refreshing the open-PR inventory"
		if lastError != "" {
			reason += " after a failure: " + lastError
		}
	case !fresh:
		switch {
		case lastError != "":
			reason = lastError
		case observation == nil || stored == nil:
			reason = "No complete open-PR inventory has been observed"
		case !observation.identity.Matches(cfg):
			reason = "Configuration changed since the last complete open-PR inventory"
		default:
			reason = "The last complete open-PR inventory is stale"
		}
	case remaining == 0:
		status = "full"
		reason = prCapacityFullReason
	default:
		status = "ready"
	}
	capacity := model.PrCapacity{Limit: cfg.MaxOpenPRs, OwnedOpen: ownedOpen, Reserved: reserved, ObservedAt: observedAt, Status: status}
	if fresh {
		capacity.Remaining = &remaining
	}
	if reason != "" {
		capacity.Reason = &reason
	}
	return capacity, nil
}

func prCapacityFrom(cfg config.Config, inventory model.OpenPrInventory, reservations []store.PrReservation) model.PrCapacity {
	owned, unrepresented, remaining := store.PrUnion(inventory, reservations, cfg.MaxOpenPRs)
	observedAt := inventory.ObservedAt
	capacity := model.PrCapacity{Limit: cfg.MaxOpenPRs, OwnedOpen: &owned, Reserved: unrepresented, Remaining: &remaining, ObservedAt: &observedAt, Status: "ready"}
	if remaining == 0 {
		reason := prCapacityFullReason
		capacity.Status = "full"
		capacity.Reason = &reason
	}
	return capacity
}

func (a *App) startPrRefresh(cfg config.Config) {
	a.runtimeMu.Lock()
	inFlight := a.runtime.prRefresh != nil
	a.runtimeMu.Unlock()
	if a.ctx.Err() != nil || inFlight {
		return
	}
	capacity, err := a.prCapacity(cfg)
	available := err == nil && capacity.Remaining != nil && *capacity.Remaining > 0
	a.runtimeMu.Lock()
	paced := !available || a.runtime.prAdmissionRefused
	if a.ctx.Err() != nil || a.runtime.prRefresh != nil || paced && time.Since(a.runtime.lastPrAttempt) < prRefreshRetryDelay {
		a.runtimeMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	job := &prRefreshJob{cancel: cancel}
	a.runtime.prRefresh = job
	a.runtime.lastPrAttempt = time.Now()
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		_ = a.refreshPRs(ctx, cfg)
		a.runtimeMu.Lock()
		if a.runtime.prRefresh == job {
			a.runtime.prRefresh = nil
		}
		a.runtimeMu.Unlock()
		a.notify()
	}()
}

func (a *App) refreshPRs(ctx context.Context, snapshot config.Config) (result error) {
	startedAt := time.Now()
	defer func() {
		if result == nil || ctx.Err() != nil || errors.Is(result, context.Canceled) || errors.Is(result, errPrPolicyChanged) || errors.Is(result, errPrInventorySuperseded) {
			return
		}
		a.gate.Lock()
		defer a.gate.Unlock()
		if live, err := a.Config(); err == nil && !store.PrIdentityOf(snapshot).Matches(live) {
			result = errPrPolicyChanged
			return
		}
		a.runtimeMu.Lock()
		observation := a.runtime.prObservation
		if observation == nil || (observation.identity.Matches(snapshot) && !observation.fetchedAt.After(startedAt)) {
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
	if !store.PrIdentityOf(snapshot).Matches(live) {
		return errPrPolicyChanged
	}
	persisted, err := a.commitPrObservationLocked(live, observed)
	if err != nil {
		return err
	}
	if !persisted {
		return errPrInventorySuperseded
	}
	return nil
}

type prSnapshot struct {
	inventory model.OpenPrInventory
	owned     []model.PullRequest
	released  []string
}

func (a *App) observeOpenPRs(ctx context.Context, cfg config.Config) (prSnapshot, error) {
	inventory, err := gitops.OpenPrInventory(ctx, cfg)
	if err != nil {
		return prSnapshot{}, fmt.Errorf("Open pull request inventory failed: %w", err)
	}
	owned, err := gitops.OwnedPrDetails(ctx, cfg, inventory)
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

func overlayOwnedDetails(inventory *model.OpenPrInventory, details []model.PullRequest) {
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

func (a *App) commitPrObservationLocked(observed config.Config, snapshot prSnapshot) (bool, error) {
	control, err := a.Control()
	if err != nil {
		return false, err
	}
	persisted, err := a.Store.PersistPrInventory(snapshot.inventory, snapshot.released)
	if err != nil || !persisted {
		return false, err
	}
	for _, pr := range snapshot.owned {
		if err := a.Store.RecordPrObservation(observed.GitHubRepo, pr, false); err != nil {
			return true, err
		}
	}
	a.runtimeMu.Lock()
	if control.Mode != model.OperatingModePaused {
		a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(observed), inventory: snapshot.inventory.Clone(), fetchedAt: time.Now()}
	}
	a.runtime.prRefreshError = ""
	a.runtimeMu.Unlock()
	return true, nil
}

func (a *App) releasableReservations(ctx context.Context, cfg config.Config, inventory model.OpenPrInventory) ([]string, error) {
	reservations, err := a.Store.PrReservations(cfg.GitHubRepo)
	if err != nil {
		return nil, err
	}
	represented := map[string]struct{}{}
	for _, pr := range inventory.PRs {
		if pr.OwnedOpen() {
			represented[pr.Branch] = struct{}{}
		}
	}
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
