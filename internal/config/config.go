// Package config owns configuration loading, validation, and immutable snapshots.
package config

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// Return fresh slices so callers cannot mutate the service's vocabulary.
func Categories() []string {
	return []string{"features", "correctness", "performance", "ux-dx", "refactoring", "simplification", "tests", "dependencies", "documentation"}
}
func Roles() []string {
	return []string{"orchestrator", "discovery", "proposal_reviewer", "code_reviewer"}
}
func Tiers() []string { return []string{"XS", "S", "M", "L", "XL"} }
func NewRoute(model, effort string) Route {
	return Route{Backend: BackendCodex, Model: model, Effort: effort}
}
func DefaultRepairRoute() Route { return NewRoute("", "medium") }
func Default() Config {
	roles := make(map[string]Route, 4)
	for _, role := range Roles() {
		roles[role] = NewRoute("", "")
	}
	return Config{
		DefaultBranch: "main", BranchPrefix: "octomus/", CodexBinary: "codex", OpencodeBinary: "opencode",
		Roles: roles, Tiers: map[string]Route{"XS": NewRoute("", "xhigh"), "S": NewRoute("", "max"), "M": NewRoute("", "low"), "L": NewRoute("", "medium"), "XL": NewRoute("", "high")},
		RepairRoute: DefaultRepairRoute(), Categories: Categories(), VerificationCommands: []string{},
		DiscoveryAgents: 9, ExecutionConcurrency: 2, CycleIntervalSeconds: 1800, MaintenanceEveryCycles: 3,
		LargePRLines: 1000, LongLivedPRDays: 7, MaxTasksPerCycle: 5, MaxRepairRounds: 4, MaxNoProgressRounds: 2, MaxRetries: 2,
		SessionTimeoutSeconds: 1800, TaskTimeoutSeconds: 14400, CommandTimeoutSeconds: 600,
		MaxSessionsPerDay: 150, MaxOpenPRs: 5, MaxWorkspaceBytes: 20_000_000_000, RunnerStoragePaths: map[string]string{},
		RetainCompletedDays: 14, RetainEvents: 10000,
	}
}
func (b Backend) Slug() string { return b.String() }
func (b Backend) Display() string {
	if b == BackendCodex {
		return "Codex"
	}
	return "OpenCode"
}
func (r Route) String() string {
	if r.Backend == BackendCodex {
		return fmt.Sprintf("Codex · %s / %s", r.Model, r.Effort)
	}
	provider, variant := "", "provider default"
	if r.Provider != nil {
		provider = *r.Provider
	}
	if r.Variant != nil {
		variant = *r.Variant
	}
	return fmt.Sprintf("OpenCode · %s/%s / %s", provider, r.Model, variant)
}
func (r Route) RequireBackend(backend Backend) error {
	if r.Backend != backend {
		return fmt.Errorf("Wrong runner for %s", r)
	}
	return nil
}
func (r Route) Validate(ready bool) error {
	valid := func(s string, max int) bool {
		return len(s) <= max && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
	}
	max := 100
	if r.Backend == BackendOpencode {
		max = 512
	}
	if !valid(r.Model, max) || !valid(r.Effort, 20) || (r.Provider != nil && !valid(*r.Provider, 100)) || (r.Variant != nil && (*r.Variant == "" || !valid(*r.Variant, 100))) {
		return fmt.Errorf("Invalid model route")
	}
	switch r.Backend {
	case BackendCodex:
		if r.Provider != nil || r.Variant != nil {
			return fmt.Errorf("Codex routes use reasoning effort, not an OpenCode provider or variant")
		}
		if ready && (r.Model == "" || r.Effort == "") {
			return fmt.Errorf("Set the Codex model and effort for every required route")
		}
	case BackendOpencode:
		if r.Effort != "" {
			return fmt.Errorf("OpenCode routes use a variant, not Codex reasoning effort")
		}
		if ready && (r.Model == "" || r.Provider == nil || *r.Provider == "") {
			return fmt.Errorf("Set the OpenCode provider and model for every required route")
		}
	default:
		return fmt.Errorf("Invalid backend")
	}
	return nil
}
func (c Config) Binary(backend Backend) string {
	if backend == BackendCodex {
		return c.CodexBinary
	}
	return c.OpencodeBinary
}

type NamedRoute struct {
	Name  string
	Route Route
}

// RoutesFor returns cloned routes: roles in sorted key order (audits skip
// code_reviewer), then for execution the tiers in sorted key order and the
// repair route.
func (c Config) RoutesFor(audit bool) []NamedRoute {
	result := []NamedRoute{}
	for _, key := range slices.Sorted(maps.Keys(c.Roles)) {
		if audit && key == "code_reviewer" {
			continue
		}
		result = append(result, NamedRoute{key, c.Roles[key].Clone()})
	}
	if audit {
		return result
	}
	for _, key := range slices.Sorted(maps.Keys(c.Tiers)) {
		result = append(result, NamedRoute{key, c.Tiers[key].Clone()})
	}
	return append(result, NamedRoute{"repair", c.RepairRoute.Clone()})
}
func (c Config) PlanningAdmissionsRequired() uint64 { return c.DiscoveryAgents + 4 }
func EqualASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 32
		}
		if y >= 'A' && y <= 'Z' {
			y += 32
		}
		if x != y {
			return false
		}
	}
	return true
}

// Path identity ignores repeated separators and interior dots. It keeps '..'
// literal and retains a leading relative '.'.
func pathIdentity(path string) string {
	parts := []string{}
	if strings.HasPrefix(path, "/") {
		parts = append(parts, "/")
	}
	for i, p := range strings.Split(path, "/") {
		if p != "" && (p != "." || i == 0) {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}
func (c Config) SameRemoteIdentity(other Config) bool {
	return pathIdentity(c.Repository) == pathIdentity(other.Repository) && EqualASCII(c.GitHubRepo, other.GitHubRepo) && c.DefaultBranch == other.DefaultBranch
}
func (c Config) Validate(ready bool) error { return c.validateMode(ready, false) }
func (c Config) ValidateAudit() error      { return c.validateMode(true, true) }
func between(v, low, high uint64) bool     { return v >= low && v <= high }
func (c Config) validateMode(ready, audit bool) error {
	checks := []struct {
		ok      bool
		message string
	}{
		{between(c.DiscoveryAgents, 8, 10), "Discovery requires 8–10 agents"},
		{between(c.ExecutionConcurrency, 1, 8), "Execution concurrency must be 1–8"},
		{between(c.MaxTasksPerCycle, 1, 20), "Tasks per cycle must be 1–20"},
		{between(c.MaxRepairRounds, 1, 20), "Repair rounds must be 1–20"},
		{c.MaxNoProgressRounds > 0, "No-progress rounds must be at least 1"},
		{c.MaxRetries <= 10, "Retry limit must be at most 10"},
		{between(c.CycleIntervalSeconds, 30, 604800), "Cycle interval must be 30–604800 seconds"},
		{between(c.MaintenanceEveryCycles, 1, 10000), "Maintenance cadence must be 1–10000 cycles"},
		{between(c.SessionTimeoutSeconds, 10, 604800), "Session timeout must be 10–604800 seconds"},
		{between(c.CommandTimeoutSeconds, 1, 604800), "Command timeout must be 1–604800 seconds"},
		{between(c.TaskTimeoutSeconds, c.SessionTimeoutSeconds, 604800), "Task timeout must be at least the session timeout and at most 604800 seconds"},
		{between(c.MaxSessionsPerDay, 1, 1000000), "Daily session budget must be 1–1000000"},
		{between(c.MaxOpenPRs, 1, 1000), "Open PR capacity must be 1–1000"},
		{between(c.MaxWorkspaceBytes, 1000000, 1000000000000000), "Workspace budget must be 1000000–1000000000000000 bytes"},
		{between(c.RetainCompletedDays, 1, 36500), "Workspace retention must be 1–36500 days"},
		{between(c.RetainEvents, 100, 100000), "Retained activity events must be 100–100000"},
		{ValidBranch(c.DefaultBranch), "Default branch must be a valid branch name"},
		{ValidBranch(c.BranchPrefix+"task") && strings.HasSuffix(c.BranchPrefix, "/"), `Owned branch prefix must be a valid branch path ending in "/"`},
		{!strings.HasPrefix(c.DefaultBranch, c.BranchPrefix), "Owned branch prefix must exclude the default branch"},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("%s", check.message)
		}
	}
	if len(c.Categories) == 0 {
		return fmt.Errorf("Select valid improvement categories")
	}
	for _, v := range c.Categories {
		if !slices.Contains(Categories(), v) {
			return fmt.Errorf("Select valid improvement categories")
		}
	}
	exact := func(routes map[string]Route, keys []string) bool {
		if len(routes) != len(keys) {
			return false
		}
		for _, key := range keys {
			if _, ok := routes[key]; !ok {
				return false
			}
		}
		return true
	}
	if !exact(c.Roles, Roles()) {
		return fmt.Errorf("Configure exactly the four planning and review roles (%s)", strings.Join(Roles(), ", "))
	}
	if !exact(c.Tiers, Tiers()) {
		return fmt.Errorf("Configure exactly the five execution tiers (%s)", strings.Join(Tiers(), ", "))
	}
	for _, route := range c.RoutesFor(false) {
		if err := route.Route.Validate(false); err != nil {
			return err
		}
	}
	for backend, path := range c.RunnerStoragePaths {
		if (backend != "codex" && backend != "opencode") || !filepath.IsAbs(path) {
			return fmt.Errorf("Runner storage measurement requires an absolute path for Codex or OpenCode")
		}
	}
	for _, binary := range []string{c.CodexBinary, c.OpencodeBinary} {
		if err := ValidateBinary(binary); err != nil {
			return err
		}
	}
	for _, command := range c.VerificationCommands {
		if strings.TrimSpace(command) == "" || len(command) > 4096 {
			return fmt.Errorf("Each verification command must be non-empty and at most 4096 bytes")
		}
	}
	if ready {
		for _, route := range c.RoutesFor(audit) {
			if err := route.Route.Validate(true); err != nil {
				return fmt.Errorf("%s: %w", route.Name, err)
			}
		}
		if err := c.validateRepository(); err != nil {
			return err
		}
		if !audit && len(c.VerificationCommands) == 0 {
			return fmt.Errorf("Set at least one meaningful repository verification command")
		}
	}
	return nil
}
func (c Config) ValidateBaseline() error {
	if err := c.Validate(false); err != nil {
		return err
	}
	if err := c.validateRepository(); err != nil {
		return err
	}
	if len(c.VerificationCommands) == 0 {
		return fmt.Errorf("Set at least one meaningful repository verification command")
	}
	return nil
}
func (c Config) validateRepository() error {
	// Preserve symlink/.. for filesystem resolution instead of cleaning it lexically.
	if _, err := os.Stat(c.Repository + string(os.PathSeparator) + ".git"); !filepath.IsAbs(c.Repository) || err != nil {
		return fmt.Errorf("Repository must be an absolute path to a Git checkout")
	}
	parts := strings.Split(c.GitHubRepo, "/")
	valid := len(parts) == 2
	for _, part := range parts {
		if part == "" {
			valid = false
		}
		for _, b := range []byte(part) {
			if !asciiAlphanumeric(b) && !strings.ContainsRune("-_.", rune(b)) {
				valid = false
			}
		}
	}
	if !valid {
		return fmt.Errorf("GitHub repository must be owner/name")
	}
	return nil
}
func ValidateBinary(binary string) error {
	if strings.TrimSpace(binary) == "" || len(binary) > 4096 || strings.ContainsFunc(binary, unicode.IsControl) {
		return fmt.Errorf("Set a valid runner executable path")
	}
	return nil
}
func asciiAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
func ValidBranch(s string) bool {
	if s == "" || strings.ContainsRune("-/.", rune(s[0])) || strings.ContainsRune("/.", rune(s[len(s)-1])) || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//") {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	for _, b := range []byte(s) {
		if !asciiAlphanumeric(b) && !strings.ContainsRune("-_./", rune(b)) {
			return false
		}
	}
	return true
}
func (c Config) Fingerprint() (string, error) {
	data, err := wirejson.Marshal(c)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
