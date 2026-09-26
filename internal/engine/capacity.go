package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	prObservationLifetime = observationLifetime
	prAdmissionLifetime   = time.Minute
	prRefreshRetryDelay   = time.Minute
)

// prCapacityFullReason explains a full owned-PR capacity wherever it is reported.
const prCapacityFullReason = "The configured owned open-PR limit is reached; new-PR work waits for an observed closure or merge"

// errPrInventorySuperseded reports a refresh whose inventory was not saved
// because a refresh whose fetch started later already saved a newer complete
// one. The older result is obsolete, not a failure.
var errPrInventorySuperseded = errors.New("Pull request inventory became stale before persistence")

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
	a.runtimeMu.Unlock()
}

// takePrAdmissionInventory consumes the observation for one dispatch batch.
// Its dashboard evidence remains cached independently of admission authority.
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

// PrCapacity reports only from a fresh observation made by this process.
// Persisted inventory is evidence, never dispatch authority after restart.
func (a *App) PrCapacity() (model.PrCapacity, error) {
	cfg, err := a.Config()
	if err != nil {
		return model.PrCapacity{}, err
	}
	reservations, err := a.Store.PrReservations(cfg.GitHubRepo)
	if err != nil {
		return model.PrCapacity{}, err
	}
	stored, err := a.Store.OpenPrInventory()
	if err != nil {
		return model.PrCapacity{}, err
	}
	if stored != nil && !sameRepository(stored.Repository, cfg.GitHubRepo) {
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

// prCapacityFrom reports the capacity that one complete inventory shows with
// the current reservations. It is observed context for planning prompts,
// never dispatch authority: only PrCapacity's fresh current-process
// observation authorizes admission.
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
	// A refresh in flight settles every waiting task; skip the capacity read.
	a.runtimeMu.Lock()
	inFlight := a.runtime.prRefresh != nil
	a.runtimeMu.Unlock()
	if inFlight {
		return
	}
	capacity, err := a.PrCapacity()
	available := err == nil && capacity.Remaining != nil && *capacity.Remaining > 0
	a.runtimeMu.Lock()
	if a.runtime.prRefresh != nil || !available && time.Since(a.runtime.lastPrAttempt) < prRefreshRetryDelay {
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

// RefreshPRs performs a complete remote observation without holding gate and
// revalidates configuration and mode before the result can authorize work.
func (a *App) RefreshPRs(ctx context.Context) error {
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	return a.refreshPRs(ctx, cfg)
}

func (a *App) refreshPRs(ctx context.Context, snapshot config.Config) (result error) {
	startedAt := time.Now()
	defer func() {
		// A refresh whose own context ended (invalidation by pause, config
		// save or a failed run, or shutdown) is obsolete, not failed: remote
		// captures report that as process.ErrCancelled, which does not wrap
		// context.Canceled, and invalidation has already reset this state. A
		// superseded refresh is obsolete too: the newer one that persisted
		// first already settled this state.
		if result == nil || ctx.Err() != nil || errors.Is(result, context.Canceled) || errors.Is(result, errPrInventorySuperseded) {
			return
		}
		a.runtimeMu.Lock()
		observation := a.runtime.prObservation
		if observation == nil || (observation.identity.Matches(snapshot) && !observation.fetchedAt.After(startedAt)) {
			a.runtime.prObservation = nil
			a.runtime.prRefreshError = store.ErrorMessage(result)
		}
		a.runtimeMu.Unlock()
	}()
	inventory, err := gitops.OpenPrInventory(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("Open pull request inventory failed: %w", err)
	}
	details, err := gitops.OwnedPrDetails(ctx, snapshot, inventory)
	if err != nil {
		return fmt.Errorf("Owned pull request refresh failed: %w", err)
	}
	detailByNumber := map[uint64]model.PullRequest{}
	for _, detail := range details {
		detailByNumber[detail.Number] = detail
	}
	for i, observed := range inventory.PRs {
		if detail, ok := detailByNumber[observed.Number]; ok {
			inventory.PRs[i] = detail
		}
	}
	released, err := a.releasableReservations(ctx, snapshot, inventory)
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
		return errors.New("Pull request policy changed during refresh")
	}
	control, err := a.Control()
	if err != nil {
		return err
	}
	persisted, err := a.Store.PersistPrInventory(inventory, released)
	if err != nil {
		return err
	}
	if !persisted {
		// The live PR identity was confirmed above, so a refused persist can
		// only mean a newer saved inventory.
		return errPrInventorySuperseded
	}
	for _, detail := range details {
		if err := a.Store.RecordPrObservation(live.GitHubRepo, detail, false); err != nil {
			return err
		}
	}
	// A persisted complete inventory supersedes any earlier refresh failure,
	// even while paused; only an unpaused service gains dispatch authority.
	a.runtimeMu.Lock()
	if control.Mode != model.OperatingModePaused {
		a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(live), inventory: inventory.Clone(), fetchedAt: time.Now()}
	}
	a.runtime.prRefreshError = ""
	a.runtimeMu.Unlock()
	return nil
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
			sameRepository(detail.HeadRepository, cfg.GitHubRepo) &&
			sameRepository(detail.BaseRepository, cfg.GitHubRepo) && detail.Branch == reservation.Branch
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

func sameRepository(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
