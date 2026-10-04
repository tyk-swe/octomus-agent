package dashboard_test

// The dashboard's TypeScript types, vocabularies and numeric limits mirror the Go records and validation.

import (
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/evidence"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

var (
	objectBlock = regexp.MustCompile(`(?m)^export type (\w+) = \{\n((?:  .*\n)*?)\};$`)
	blockKey    = regexp.MustCompile(`(?m)^  ([a-z_]+)\??:`)
	objectLine  = regexp.MustCompile(`(?m)^export type (\w+) = \{ (.*) \};$`)
	lineKey     = regexp.MustCompile(`(?:^|; )([a-z_]+)\??:`)
	stringUnion = regexp.MustCompile(`(?m)^export type (\w+) =((?:\s*\|?\s*'[a-z_]+')+);$`)
	quoted      = regexp.MustCompile(`'([^']*)'`)
	limitEntry  = regexp.MustCompile(`(?s)\{\s*key:\s*'([a-z_]+)'.*?min:\s*([\d_]+)(?:,\s*max:\s*([\d_]+))?\s*\}`)
)

func dashboardSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("src/lib/" + name)
	if err != nil {
		t.Fatalf("dashboard source: %v", err)
	}
	return string(data)
}

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

// quotedWords returns the quoted strings of the array that follows `marker` in source.
func quotedWords(t *testing.T, source, marker string) []string {
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

func TestTypesMirrorGoJSON(t *testing.T) {
	declared := objectTypes(dashboardSource(t, "types.ts"))
	for name, record := range map[string]any{
		"Config":                   config.Config{},
		"Route":                    config.Route{},
		"Proposal":                 model.Proposal{},
		"Session":                  model.Session{},
		"ReviewRound":              model.ReviewRound{},
		"RepairProgress":           model.RepairProgress{},
		"WorkspaceLifecycle":       model.WorkspaceLifecycle{},
		"PR":                       model.PullRequest{},
		"PrObservation":            model.PRObservation{},
		"ExternalPrContext":        model.ExternalPRContext{},
		"PrCoverage":               model.PRCoverage{},
		"Cycle":                    model.Cycle{},
		"PrCapacity":               model.PRCapacity{},
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
		"PrReference":              evidence.PRReference{},
		"SandboxPosture":           engine.SandboxPosture{},
		"SandboxRecord":            model.SandboxRecord{},
		"SandboxEgress":            model.SandboxEgress{},
		"SandboxSelfTest":          engine.SandboxSelfTest{},
		"ProbeCheck":               sandbox.ProbeCheck{},
		"BrokerInfo":               wire.BrokerInfo{},
		"BrokerLimits":             wire.BrokerLimits{},
		"BrokerNetworks":           wire.BrokerNetworks{},
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

func TestVocabulariesMirrorGo(t *testing.T) {
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
	if got := quotedWords(t, types, "export const ACTIVE_STATUSES"); !slices.Equal(got, active) {
		t.Errorf("types.ts ACTIVE_STATUSES = %q; want model.ActiveStatuses() %q", got, active)
	}
	decisions := quotedWords(t, dashboardSource(t, "evidence.ts"), "export const DECISIONS")
	if !slices.Equal(slices.Sorted(slices.Values(decisions)), slices.Sorted(slices.Values(model.Decisions()))) {
		t.Errorf("evidence.ts DECISIONS = %q; want the words of model.Decisions() %q", decisions, model.Decisions())
	}
	settings := dashboardSource(t, "Settings.svelte")
	for _, list := range []struct {
		name string
		want []string
	}{
		{"categories", config.Categories()},
		{"ROLES", config.Roles()},
		{"TIERS", config.Tiers()},
	} {
		if got := quotedWords(t, settings, "const "+list.name); !slices.Equal(got, list.want) {
			t.Errorf("Settings.svelte %s = %q; want %q", list.name, got, list.want)
		}
	}
}

type dashboardLimit struct {
	key      string
	min, max uint64
	bounded  bool
}

func dashboardLimits(t *testing.T) []dashboardLimit {
	t.Helper()
	_, body, found := strings.Cut(dashboardSource(t, "limits.ts"), "export const LIMITS")
	if found {
		body, _, found = strings.Cut(body, "\n];")
	}
	if !found {
		t.Fatal("limits.ts: no `export const LIMITS … ];` array")
	}
	matches := limitEntry.FindAllStringSubmatch(body, -1)
	if entries := strings.Count(body, "key:"); len(matches) != entries || entries == 0 {
		t.Fatalf("limits.ts: parsed %d of %d LIMITS entries; keep each as { key, label, help, min, max? }", len(matches), entries)
	}
	number := func(text string) uint64 {
		value, err := strconv.ParseUint(strings.ReplaceAll(text, "_", ""), 10, 64)
		if err != nil {
			t.Fatalf("limits.ts: %v", err)
		}
		return value
	}
	limits := make([]dashboardLimit, 0, len(matches))
	for _, match := range matches {
		limit := dashboardLimit{key: match[1], min: number(match[2]), bounded: match[3] != ""}
		if limit.bounded {
			limit.max = number(match[3])
		}
		limits = append(limits, limit)
	}
	return limits
}

// Every numeric configuration field has a dashboard limit, and the service accepts exactly that range.
func TestLimitsMatchValidation(t *testing.T) {
	fields := map[string]int{}
	configType := reflect.TypeFor[config.Config]()
	for i := range configType.NumField() {
		if field := configType.Field(i); field.Type.Kind() == reflect.Uint64 {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			fields[name] = i
		}
	}
	seen := map[string]bool{}
	for _, limit := range dashboardLimits(t) {
		index, ok := fields[limit.key]
		if !ok {
			t.Errorf("LIMITS key %q is not a numeric Config field", limit.key)
			continue
		}
		if seen[limit.key] {
			t.Errorf("LIMITS repeats %q", limit.key)
		}
		seen[limit.key] = true
		accepts := func(value uint64) bool {
			cfg := config.Default()
			reflect.ValueOf(&cfg).Elem().Field(index).SetUint(value)
			switch limit.key {
			case "session_timeout_seconds":
				cfg.TaskTimeoutSeconds = max(cfg.TaskTimeoutSeconds, value)
			case "task_timeout_seconds":
				cfg.SessionTimeoutSeconds = min(cfg.SessionTimeoutSeconds, value)
			}
			return cfg.Validate(false) == nil
		}
		if !accepts(limit.min) {
			t.Errorf("%s: the service rejects the dashboard minimum %d", limit.key, limit.min)
		}
		if limit.min > 0 && accepts(limit.min-1) {
			t.Errorf("%s: the service accepts %d, below the dashboard minimum %d", limit.key, limit.min-1, limit.min)
		}
		switch {
		case !limit.bounded:
			if !accepts(math.MaxUint64) {
				t.Errorf("%s: the service has a maximum the dashboard does not declare", limit.key)
			}
		case !accepts(limit.max):
			t.Errorf("%s: the service rejects the dashboard maximum %d", limit.key, limit.max)
		case limit.max < math.MaxUint64 && accepts(limit.max+1):
			t.Errorf("%s: the service accepts %d, above the dashboard maximum %d", limit.key, limit.max+1, limit.max)
		}
	}
	for name := range fields {
		if !seen[name] {
			t.Errorf("numeric Config field %q has no dashboard LIMITS entry", name)
		}
	}
}
