package config

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The dashboard's settings form (web/src/lib) restates parts of this package:
// the range of every numeric limit it lets the operator enter, the improvement
// categories it offers and the order it renders roles and tiers in. These tests
// read those TypeScript sources, so neither side can change without the other.

func dashboardSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", name))
	if err != nil {
		t.Fatalf("dashboard source: %v", err)
	}
	return string(data)
}

type dashboardLimit struct {
	key      string
	min, max uint64
	bounded  bool
}

// limitEntry matches one LIMITS object literal, which keeps key first and min,
// then the optional max, last.
var limitEntry = regexp.MustCompile(`(?s)\{\s*key:\s*'([a-z_]+)'.*?min:\s*([\d_]+)(?:,\s*max:\s*([\d_]+))?\s*\}`)

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

// TestDashboardLimitsMatchValidation holds every LIMITS entry to Validate: the
// service accepts the form's minimum and maximum and rejects the value just
// outside each, and a limit the form leaves unbounded is unbounded here too.
// Every numeric Config field has exactly one entry.
func TestDashboardLimitsMatchValidation(t *testing.T) {
	fields := map[string]int{}
	configType := reflect.TypeFor[Config]()
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
		// The task timeout must be at least the session timeout, so setting one
		// moves the other only as far as that rule requires.
		accepts := func(value uint64) bool {
			cfg := Default()
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

// TestDashboardVocabulariesMatchConfig requires the settings form to offer
// exactly Categories() and to render roles and tiers in Roles() and Tiers()
// order.
func TestDashboardVocabulariesMatchConfig(t *testing.T) {
	settings := dashboardSource(t, "Settings.svelte")
	quoted := regexp.MustCompile(`'([^']*)'`)
	for _, list := range []struct {
		name string
		want []string
	}{
		{"categories", Categories()},
		{"ROLES", Roles()},
		{"TIERS", Tiers()},
	} {
		array := regexp.MustCompile(`(?s)const ` + list.name + ` = \[(.*?)\];`).FindStringSubmatch(settings)
		if array == nil {
			t.Errorf("Settings.svelte: no `const %s = [...];`", list.name)
			continue
		}
		var got []string
		for _, match := range quoted.FindAllStringSubmatch(array[1], -1) {
			got = append(got, match[1])
		}
		if !slices.Equal(got, list.want) {
			t.Errorf("Settings.svelte %s = %q; want %q", list.name, got, list.want)
		}
	}
}
