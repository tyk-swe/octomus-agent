package engine

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const prFullReason = "The configured owned open-PR limit is reached; new-PR work waits for an observed closure or merge"

var errStaleInventory = errors.New("Pull request inventory became stale before persistence")

var errPRPolicyChanged = errors.New("Pull request policy changed during refresh")

type freshPRs struct {
	identity          store.PRIdentity
	inventory         model.OpenPRInventory
	fetchedAt         time.Time
	admissionConsumed bool
}

func (a *App) invalidatePRs() {
	a.runtimeMu.Lock()
	if a.runtime.prRefresh != nil {
		a.runtime.prRefresh.cancel()
		a.runtime.prRefresh = nil
	}
	a.runtime.prObservation = nil
	a.runtime.prRefreshError = ""
	a.runtime.lastPRAttempt = time.Time{}
	a.runtime.prAdmissionRefused = false
	a.runtimeMu.Unlock()
}

func (a *App) claimInventory(cfg config.Config, now time.Time) (*model.OpenPRInventory, string) {
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

// capacityOf is the open-PR capacity an inventory and the durable reservations leave under the configured limit, with
// the remaining slots separately for the caller that decides whether the inventory is fresh enough to report them.
func capacityOf(cfg config.Config, inventory model.OpenPRInventory, reservations []store.PRReservation) (model.PRCapacity, uint64) {
	owned, unrepresented, remaining := store.PRUnion(inventory, reservations, cfg.MaxOpenPRs)
	capacity := model.PRCapacity{Limit: cfg.MaxOpenPRs, OwnedOpen: &owned, Reserved: unrepresented, Remaining: &remaining, ObservedAt: &inventory.ObservedAt, Status: "ready"}
	if remaining == 0 {
		capacity.Status = "full"
		capacity.Reason = new(prFullReason)
	}
	return capacity, remaining
}

// prCapacity is the capacity the dashboard shows: the saved inventory's union with reservations, reported as ready
// only while this process holds a fresh observation under the same policy.
func (a *App) prCapacity(cfg config.Config) (model.PRCapacity, error) {
	reservations, err := a.Store.PRReservations(cfg.GitHubRepo)
	if err != nil {
		return model.PRCapacity{}, err
	}
	stored, err := a.Store.OpenPRInventory()
	if err != nil {
		return model.PRCapacity{}, err
	}
	if stored != nil && !config.EqualASCII(stored.Repository, cfg.GitHubRepo) {
		stored = nil
	}
	capacity := model.PRCapacity{Limit: cfg.MaxOpenPRs, Reserved: uint64(len(reservations))}
	remaining := uint64(0)
	if stored != nil {
		capacity, remaining = capacityOf(cfg, *stored, reservations)
	}

	a.runtimeMu.Lock()
	refreshing := a.runtime.prRefresh != nil
	lastError := a.runtime.prRefreshError
	observation := a.runtime.prObservation
	fresh := lastError == "" && observation != nil && observation.identity.Matches(cfg) && time.Since(observation.fetchedAt) <= observationLifetime && stored != nil
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
		reason = prFullReason
	default:
		status = "ready"
	}
	capacity.Status, capacity.Reason = status, nil
	if !fresh {
		capacity.Remaining = nil
	}
	if reason != "" {
		capacity.Reason = &reason
	}
	return capacity, nil
}

func (a *App) startPRRefresh(cfg config.Config) {
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
	if a.ctx.Err() != nil || a.runtime.prRefresh != nil || paced && time.Since(a.runtime.lastPRAttempt) < prRefreshRetryDelay {
		a.runtimeMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	job := &prRefreshJob{cancel: cancel}
	a.runtime.prRefresh = job
	a.runtime.lastPRAttempt = time.Now()
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
