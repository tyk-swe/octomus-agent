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

var (
	cancelMarkers         = Kind[*string]{"cancel"}
	baselineCancelMarkers = Kind[*string]{"baseline_cancel"}
)

// Raw keeps the record's kind and id but reads or writes its JSON unchanged, for diagnostics and tests.
func (k Key[T]) Raw() Key[json.RawMessage] { return Key[json.RawMessage]{k.kind, k.id} }
