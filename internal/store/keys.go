package store

import (
	"encoding/json"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

// Key names one saved record and the Go type it decodes to.
type Key[T any] struct{ kind, id string }

// Kind is one record kind. schema.sql's triggers and indexes match these literals.
type Kind[T any] struct{ name string }

func (k Kind[T]) Key(id string) Key[T] { return Key[T]{k.name, id} }

var (
	Tasks     = Kind[model.Task]{"task"}
	Cycles    = Kind[model.Cycle]{"cycle"}
	Baselines = Kind[model.BaselineCheck]{"baseline"}
	PRs       = Kind[model.PRObservation]{"pr"}
	Decisions = Kind[model.DecisionRecord]{"decision"}
	Requests  = Kind[model.OperatorRequest]{"request"}
)

// Setting names one record of the settings kind; the engine declares its own.
func Setting[T any](id string) Key[T] { return Key[T]{"settings", id} }

var (
	ConfigKey  = Setting[config.Config]("config")
	ControlKey = Setting[model.Control]("control")
)

var (
	prInventoryKey    = Setting[model.OpenPRInventory]("pr_inventory")
	latestBaselineKey = Setting[string]("baseline_latest")
)

// Root-only settings: the repository registry and the instance-wide policy.
// They are never repository-scoped.
var (
	repositoriesKey = Setting[repositoryRegistry]("repositories")
	instanceKey     = Setting[config.InstanceSettings]("instance")
)

// scopedSetting names a settings record for this handle's repository. The root
// handle and the primary repository keep the legacy identity, so existing
// single-repository state needs no record rewrite; added repositories get
// "<id>:<owner/name>".
func (s *Store) scopedSetting(id string) string {
	if s.repo == "" || config.EqualASCII(s.repo, s.primary) {
		return id
	}
	return id + ":" + s.repo
}

// ScopedSetting gives engine-owned settings the same repository identity as
// the built-in settings, preserving the primary repository's legacy keys.
func ScopedSetting[T any](s *Store, id string) Key[T] {
	return Setting[T](s.scopedSetting(id))
}

func (s *Store) ConfigKey() Key[config.Config] {
	return Setting[config.Config](s.scopedSetting("config"))
}
func (s *Store) ControlKey() Key[model.Control] {
	return Setting[model.Control](s.scopedSetting("control"))
}
func (s *Store) prInventoryKey() Key[model.OpenPRInventory] {
	return Setting[model.OpenPRInventory](s.scopedSetting("pr_inventory"))
}
func (s *Store) latestBaselineKey() Key[string] {
	return Setting[string](s.scopedSetting("baseline_latest"))
}

var (
	cancelMarkers         = Kind[*string]{"cancel"}
	baselineCancelMarkers = Kind[*string]{"baseline_cancel"}
)

// Raw keeps the record's kind and id but reads or writes its JSON unchanged, for diagnostics and tests.
func (k Key[T]) Raw() Key[json.RawMessage] { return Key[json.RawMessage]{k.kind, k.id} }
