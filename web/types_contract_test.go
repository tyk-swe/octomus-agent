package dashboard_test

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/evidence"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// src/lib/types.ts restates the service's JSON records by hand. These tests
// hold each restated object type to its Go record's JSON field names, and each
// restated vocabulary to the Go values, so a renamed or added field or status
// fails here instead of rendering as undefined. Intersections and Pick/Omit
// types built from these (TaskRow, Task, CycleSummary, ProposalRow) and nested
// inline objects are not compared.

var (
	// A multi-line object type; its top-level keys sit at two-space indentation.
	objectBlock = regexp.MustCompile(`(?m)^export type (\w+) = \{\n((?:  .*\n)*?)\};$`)
	blockKey    = regexp.MustCompile(`(?m)^  ([a-z_]+)\??:`)
	// A one-line object type such as `export type X = { a: string; b?: number };`.
	objectLine = regexp.MustCompile(`(?m)^export type (\w+) = \{ (.*) \};$`)
	lineKey    = regexp.MustCompile(`(?:^|; )([a-z_]+)\??:`)
	// A union of string literals, on one line or one member per line.
	stringUnion = regexp.MustCompile(`(?m)^export type (\w+) =((?:\s*\|?\s*'[a-z_]+')+);$`)
	quoted      = regexp.MustCompile(`'([a-z_]+)'`)
)

func dashboardSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("src/lib/" + name)
	if err != nil {
		t.Fatalf("dashboard source: %v", err)
	}
	return string(data)
}

// objectTypes returns the sorted top-level keys of every object type in source.
func objectTypes(source string) map[string][]string {
	types := map[string][]string{}
	for _, match := range objectBlock.FindAllStringSubmatch(source, -1) {
		for _, key := range blockKey.FindAllStringSubmatch(match[2], -1) {
			types[match[1]] = append(types[match[1]], key[1])
		}
	}
	for _, match := range objectLine.FindAllStringSubmatch(source, -1) {
		for _, key := range lineKey.FindAllStringSubmatch(match[2], -1) {
			types[match[1]] = append(types[match[1]], key[1])
		}
	}
	for _, keys := range types {
		slices.Sort(keys)
	}
	return types
}

// jsonKeys returns the sorted JSON field names value encodes with.
func jsonKeys(t *testing.T, value any) []string {
	t.Helper()
	record := reflect.TypeOf(value)
	var keys []string
	for i := range record.NumField() {
		field := record.Field(i)
		if field.Anonymous {
			t.Fatalf("%s embeds %s; compare its flattened fields explicitly", record, field.Type)
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		keys = append(keys, name)
	}
	slices.Sort(keys)
	return keys
}

// quotedList returns the quoted words of the array literal marker is assigned.
func quotedList(t *testing.T, source, marker string) []string {
	t.Helper()
	_, rest, found := strings.Cut(source, marker)
	if found {
		_, rest, found = strings.Cut(rest, "= [")
	}
	list, _, closed := strings.Cut(rest, "]")
	if !found || !closed {
		t.Fatalf("no `%s … = [...]` array", marker)
	}
	var words []string
	for _, match := range quoted.FindAllStringSubmatch(list, -1) {
		words = append(words, match[1])
	}
	return words
}

// enumNames lists a Go enum's wire names in value order.
func enumNames[T interface {
	~uint8
	String() string
}]() []string {
	var names []string
	for value := T(0); value.String() != "" && value < 255; value++ {
		names = append(names, value.String())
	}
	return names
}

func TestDashboardTypesMirrorGoJSON(t *testing.T) {
	declared := objectTypes(dashboardSource(t, "types.ts"))
	for name, record := range map[string]any{
		"Config":                   config.Config{},
		"Route":                    config.Route{},
		"Proposal":                 model.Proposal{},
		"Session":                  model.Session{},
		"ReviewRound":              model.ReviewRound{},
		"WorkspaceLifecycle":       model.WorkspaceLifecycle{},
		"PR":                       model.PullRequest{},
		"PrObservation":            model.PrObservation{},
		"ExternalPrContext":        model.ExternalPrContext{},
		"PrCoverage":               model.PrCoverage{},
		"Cycle":                    model.Cycle{},
		"PrCapacity":               model.PrCapacity{},
		"PlanningCapacity":         model.PlanningCapacity{},
		"BaselineCommand":          model.BaselineCommand{},
		"BaselineCheck":            model.BaselineCheck{},
		"DefaultBranchObservation": model.DefaultBranchObservation{},
		"Event":                    model.Event{},
		"NotificationHealth":       store.NotificationHealth{},
		"Model":                    runner.Model{},
		"RunEvidenceV1":            evidence.RunEvidenceV1{},
		"CycleEvidence":            evidence.CycleEvidence{},
		"PlanningOutcome":          evidence.PlanningOutcome{},
		"ProposalEvidence":         evidence.ProposalEvidence{},
		"ReviewerVerdict":          evidence.ReviewerVerdict{},
		"TaskEvidence":             evidence.TaskEvidence{},
		"EvidenceRevisions":        evidence.Revisions{},
		"SessionRoute":             evidence.SessionRoute{},
		"ReviewEvidence":           evidence.ReviewEvidence{},
		"ReviewRoundEvidence":      evidence.ReviewRoundEvidence{},
		"FindingEvidence":          evidence.FindingEvidence{},
		"CommandEvidence":          evidence.CommandEvidence{},
		"CommandResult":            evidence.CommandResult{},
		"PrReference":              evidence.PrReference{},
	} {
		keys, found := declared[name]
		if !found {
			t.Errorf("types.ts declares no object type %s", name)
			continue
		}
		if want := jsonKeys(t, record); !slices.Equal(keys, want) {
			t.Errorf("types.ts %s keys = %q; want the %T JSON fields %q", name, keys, record, want)
		}
	}
}

func TestDashboardVocabulariesMirrorGo(t *testing.T) {
	types := dashboardSource(t, "types.ts")
	unions := map[string][]string{}
	for _, match := range stringUnion.FindAllStringSubmatch(types, -1) {
		for _, word := range quoted.FindAllStringSubmatch(match[2], -1) {
			unions[match[1]] = append(unions[match[1]], word[1])
		}
	}
	for name, want := range map[string][]string{
		"CycleMode":      enumNames[model.CycleMode](),
		"OperatingMode":  enumNames[model.OperatingMode](),
		"TaskStatus":     enumNames[model.Status](),
		"BaselineStatus": enumNames[model.BaselineStatus](),
	} {
		got, found := unions[name]
		if !found {
			t.Errorf("types.ts declares no string union %s", name)
			continue
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
			t.Errorf("types.ts %s = %q; want %q", name, got, want)
		}
	}

	var active []string
	for _, status := range model.ActiveStatuses() {
		active = append(active, status.String())
	}
	if got := quotedList(t, types, "export const ACTIVE_STATUSES"); !slices.Equal(got, active) {
		t.Errorf("types.ts ACTIVE_STATUSES = %q; want model.ActiveStatuses() %q", got, active)
	}
	// DECISIONS is in display order, so only its membership must match.
	decisions := quotedList(t, dashboardSource(t, "evidence.ts"), "export const DECISIONS")
	if !slices.Equal(slices.Sorted(slices.Values(decisions)), slices.Sorted(slices.Values(model.Decisions()))) {
		t.Errorf("evidence.ts DECISIONS = %q; want the words of model.Decisions() %q", decisions, model.Decisions())
	}
}
