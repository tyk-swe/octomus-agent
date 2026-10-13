package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

var settleMergeRecord = func(s *store.Store, repository string, number uint64, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) (bool, error) {
	return s.SettleMerge(repository, number, expected, status, reason, source, commit, revoke)
}

func mergeBarrierKey(repository string, number uint64) string {
	return strings.ToLower(repository) + ":" + strconv.FormatUint(number, 10)
}

// mergePrecheck is a settlement that failed while only recording a precheck
// decision: a waiting status, an authority revocation, or a terminal
// observation. No merge request exists for it, so recovery retries the exact
// intended write, or releases the barrier once the saved record already
// reflects the decision or has moved on to a newer one.
type mergePrecheck struct {
	message    string
	repository string
	number     uint64
	expected   model.AutoMergeState
	status     model.AutoMergeStatus
	reason     string
	source     string
	commit     *string
	revoke     bool
}

func (a *App) setMergeRecoveryError(key string, err error) {
	message := strings.Clone(redact.Error(err))
	a.runtimeMu.Lock()
	a.runtime.mergeRecoveryErrors[key] = message
	a.runtimeMu.Unlock()
}

func (a *App) clearMergeRecoveryError(key string) {
	a.runtimeMu.Lock()
	delete(a.runtime.mergeRecoveryErrors, key)
	a.runtimeMu.Unlock()
}

func (a *App) setMergePrecheckError(key string, pre mergePrecheck) {
	a.runtimeMu.Lock()
	a.runtime.mergePrecheckErrors[key] = pre
	a.runtimeMu.Unlock()
}

// clearMergeBarrier drops every failure recorded for one delivery after a
// settlement succeeds.
func (a *App) clearMergeBarrier(key string) {
	a.clearMergeRecoveryError(key)
	a.runtimeMu.Lock()
	delete(a.runtime.mergePrecheckErrors, key)
	a.runtimeMu.Unlock()
}

// clearMergePrecheck releases one precheck barrier only while it is still the
// recorded failure. A concurrent pass may have replaced it with a newer one.
func (a *App) clearMergePrecheck(key string, pre mergePrecheck) bool {
	a.runtimeMu.Lock()
	current, present := a.runtime.mergePrecheckErrors[key]
	if !present || current != pre {
		a.runtimeMu.Unlock()
		return false
	}
	delete(a.runtime.mergePrecheckErrors, key)
	a.runtimeMu.Unlock()
	return true
}

// recordMergeSettlementFailure keeps a failed settlement recoverable. A failure
// that resolves an in-flight or unconfirmed mutation stays a service-wide
// barrier until a remote read reconciles it. Every other failure only recorded
// a precheck decision, so it is stored for the recovery pass to retry or drop.
func (a *App) recordMergeSettlementFailure(key string, observation model.PRObservation, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool, err error) {
	if expected.Status == model.AutoMergeMerging || expected.Status == model.AutoMergeUncertain ||
		status == model.AutoMergeUncertain {
		a.setMergeRecoveryError(key, err)
		return
	}
	a.setMergePrecheckError(key, mergePrecheck{
		message:    strings.Clone(redact.Error(err)),
		repository: observation.Repository,
		number:     observation.PR.Number,
		expected:   expected,
		status:     status,
		reason:     reason,
		source:     source,
		commit:     commit,
		revoke:     revoke,
	})
}

// recoverMergePrechecks retries or releases the precheck settlements that
// failed before any merge request could be recorded. It runs independently of
// the candidate query, so a delivery that became ready, was archived or
// revoked, or resolved through observation cannot leave a stale barrier behind.
// It reports whether it released at least one barrier.
func (a *App) recoverMergePrechecks(ctx context.Context) bool {
	a.runtimeMu.Lock()
	pending := make([]mergePrecheck, 0, len(a.runtime.mergePrecheckErrors))
	for _, pre := range a.runtime.mergePrecheckErrors {
		pending = append(pending, pre)
	}
	a.runtimeMu.Unlock()
	released := false
	for _, pre := range pending {
		if ctx.Err() != nil {
			break
		}
		if a.recoverMergePrecheck(pre) {
			released = true
		}
	}
	if released {
		// A released precheck may have made a candidate ready; do not wait out
		// the pacing interval before the next candidate pass can merge it.
		a.runtimeMu.Lock()
		a.runtime.lastMergeCheck = time.Time{}
		a.runtimeMu.Unlock()
		a.notify()
	}
	return released
}

// recoverMergePrecheck resolves one failed precheck settlement. It is bound to
// the recorded repository, PR, delivery, reviewed head, comparison base and
// attempt: a record that changed any of them is a stale barrier and is dropped,
// never re-applied over the newer delivery.
func (a *App) recoverMergePrecheck(pre mergePrecheck) bool {
	key := mergeBarrierKey(pre.repository, pre.number)
	observation, err := a.store.MergeObservation(pre.repository, pre.number)
	if err != nil {
		return false
	}
	if observation == nil || observation.AutoMerge == nil {
		return a.clearMergePrecheck(key, pre)
	}
	merge := observation.AutoMerge
	if !mergeBarrierBound(pre.expected, *merge) {
		return a.clearMergePrecheck(key, pre)
	}
	// Another decision moved the delivery past the state this precheck started
	// from. Retrying would overwrite newer evidence, so release the barrier.
	if !wirejson.Equal(*merge, pre.expected) {
		return a.clearMergePrecheck(key, pre)
	}
	// A matching status alone does not prove the reason, timestamp or merge
	// provenance was saved. Retry the complete settlement before releasing it.
	applied, err := settleMergeRecord(a.store, pre.repository, pre.number, pre.expected, pre.status, pre.reason, pre.source, pre.commit, pre.revoke)
	if err != nil {
		return false
	}
	if applied && pre.status != model.AutoMergeWaiting {
		_ = a.store.Event(pre.expected.TaskID, model.EventAutomerge, fmt.Sprintf("Automatic merge: %s — %s", pre.status, pre.reason))
	}
	return a.clearMergePrecheck(key, pre)
}

func mergeBarrierBound(expected, merge model.AutoMergeState) bool {
	if expected.TaskID != merge.TaskID || expected.Head != merge.Head ||
		expected.ComparisonBase != merge.ComparisonBase ||
		expected.HeadBranch != merge.HeadBranch || expected.BaseBranch != merge.BaseBranch ||
		expected.PolicyRevision != merge.PolicyRevision {
		return false
	}
	if (expected.AttemptID == nil) != (merge.AttemptID == nil) {
		return false
	}
	return expected.AttemptID == nil || *expected.AttemptID == *merge.AttemptID
}

func (a *App) mergeConfirmed(task *model.Task, observation model.PRObservation, expected model.AutoMergeState, commit string) {
	if ok, err := settleMergeRecord(a.store, observation.Repository, observation.PR.Number, expected, model.AutoMergeMerged, "Squash merged by Octomus", store.MergeResultConfirmed, &commit, false); err != nil || !ok {
		if err == nil {
			err = errors.New("The confirmed merge result could not be recorded")
		}
		a.setMergeRecoveryError(mergeBarrierKey(observation.Repository, observation.PR.Number), err)
		return
	}
	a.clearMergeBarrier(mergeBarrierKey(observation.Repository, observation.PR.Number))
	_ = a.store.Event(task.ID, model.EventAutomerge, fmt.Sprintf("Squash merged pull request %d at %s", observation.PR.Number, commit))
	live, err := a.Config()
	if err != nil {
		return
	}
	a.startPRRefresh(live)
	if revision, revErr := gitops.RemoteRevision(a.ctx, live, live.DefaultBranch); revErr == nil && revision != nil {
		_ = a.observeDefaultBranch(live, *revision, model.Now())
	}
	a.notify()
}

func (a *App) settleMerge(observation model.PRObservation, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) {
	key := mergeBarrierKey(observation.Repository, observation.PR.Number)
	applied, err := settleMergeRecord(a.store, observation.Repository, observation.PR.Number, expected, status, reason, source, commit, revoke)
	if err != nil {
		a.recordMergeSettlementFailure(key, observation, expected, status, reason, source, commit, revoke, err)
		return
	}
	if applied {
		a.clearMergeBarrier(key)
	}
	if applied && status != model.AutoMergeWaiting {
		_ = a.store.Event(expected.TaskID, model.EventAutomerge, fmt.Sprintf("Automatic merge: %s — %s", status, reason))
		a.notify()
	}
}
