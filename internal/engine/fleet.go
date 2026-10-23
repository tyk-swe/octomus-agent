package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// FleetOptions carries the process-wide wiring shared by every repository's
// App. Deployments pin trusted checkouts per repository, keyed by the
// lowercase GitHub identity. A nonempty pin map fixes the repositories this
// deployment serves; BaseDeployment applies only when no repositories are pinned.
type FleetOptions struct {
	Backend           sandbox.Backend
	Connector         runner.Connector
	BaseDeployment    Deployment
	Deployments       map[string]Deployment
	PrimaryRepository string
}

// Fleet runs one App per registered repository over one shared store. Apps
// keep fully independent control, budgets, workspaces and runtime state; the
// store enforces per-repository caps under the instance-wide ceiling inside
// its admission transactions. With no repositories registered the fleet is
// exactly the original single unscoped App.
type Fleet struct {
	root    *store.Store
	dataDir string
	options FleetOptions
	ctx     context.Context
	cancel  context.CancelFunc

	mu      sync.Mutex
	apps    []*App
	entries []store.RepositoryEntry
}

func NewFleet(root *store.Store, dataDir string, options FleetOptions) (*Fleet, error) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Fleet{root: root, dataDir: filepath.Clean(dataDir), options: options, ctx: ctx, cancel: cancel}
	entries, err := root.Repositories()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, entry := range entries {
		if _, err := f.deploymentFor(entry.GitHubRepo); err != nil {
			cancel()
			return nil, err
		}
	}
	// Deployment-pinned repositories are served even before their first
	// saved configuration registers them (the first Docker boot).
	pins := make([]string, 0, len(options.Deployments))
	for key := range options.Deployments {
		pins = append(pins, key)
	}
	slices.Sort(pins)
	if primary := strings.ToLower(options.PrimaryRepository); primary != "" {
		if i := slices.Index(pins, primary); i > 0 {
			copy(pins[1:i+1], pins[:i])
			pins[0] = primary
		}
	}
	for _, key := range pins {
		deployment := options.Deployments[key]
		if deployment.GitHubRepo == "" || f.entryFor(entries, deployment.GitHubRepo) != nil {
			continue
		}
		dataDir := "repos/" + store.RepositorySlug(deployment.GitHubRepo)
		if len(entries) == 0 {
			dataDir = "."
		}
		entry := store.RepositoryEntry{GitHubRepo: deployment.GitHubRepo, DataDir: dataDir}
		if err := root.RegisterRepository(entry); err != nil {
			cancel()
			return nil, err
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		app := f.newApp(root, f.dataDir, f.options.BaseDeployment)
		f.apps = []*App{app}
		return f, nil
	}
	for _, entry := range entries {
		if _, err := f.add(entry); err != nil {
			cancel()
			return nil, err
		}
	}
	return f, nil
}

func (f *Fleet) entryFor(entries []store.RepositoryEntry, repo string) *store.RepositoryEntry {
	for i := range entries {
		if config.EqualASCII(entries[i].GitHubRepo, repo) {
			return &entries[i]
		}
	}
	return nil
}

func (f *Fleet) newApp(scoped *store.Store, dir string, deployment Deployment) *App {
	options := []Option{WithSandbox(f.options.Backend), WithDeployment(deployment)}
	if f.options.Connector != nil {
		options = append(options, WithRunnerConnector(f.options.Connector))
	}
	return newWithContext(f.ctx, scoped, dir, options...)
}

func (f *Fleet) deploymentFor(repo string) (Deployment, error) {
	if deployment, ok := f.options.Deployments[strings.ToLower(repo)]; ok {
		return deployment, nil
	}
	if len(f.options.Deployments) > 0 {
		return Deployment{}, fmt.Errorf("Repository %s is not pinned by this deployment (OCTOMUS_GITHUB_REPO and OCTOMUS_GITHUB_REPOS); restore its pin before restarting", repo)
	}
	return f.options.BaseDeployment, nil
}

// add builds (but does not recover) the App for one registry entry.
func (f *Fleet) add(entry store.RepositoryEntry) (*App, error) {
	app, err := f.repositoryApp(entry)
	if err != nil {
		return nil, err
	}
	f.publish(entry, app)
	return app, nil
}

func (f *Fleet) repositoryApp(entry store.RepositoryEntry) (*App, error) {
	deployment, err := f.deploymentFor(entry.GitHubRepo)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(f.dataDir, filepath.FromSlash(entry.DataDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	app := f.newApp(f.root.ForRepository(entry.GitHubRepo), dir, deployment)
	if deployment.Repository != "" {
		owned, err := filepath.Rel(f.dataDir, deployment.Repository)
		inside, insideErr := filepath.Rel(dir, deployment.Repository)
		if err == nil && filepath.IsLocal(owned) && insideErr == nil && !filepath.IsLocal(inside) {
			app.checkoutStorage = deployment.Repository
		}
	}
	return app, nil
}

func (f *Fleet) publish(entry store.RepositoryEntry, app *App) {
	f.entries = append(f.entries, entry)
	f.apps = append(f.apps, app)
	// The root-level (primary) App excludes nested repositories' roots from
	// its storage measurement; refresh its list as repositories appear.
	for i, existing := range f.entries {
		if filepath.Clean(existing.DataDir) == "." {
			var nested []string
			for _, other := range f.entries {
				if filepath.Clean(other.DataDir) != "." {
					nested = append(nested, filepath.FromSlash(other.DataDir))
				}
			}
			for j, other := range f.apps {
				if i == j || other.deployment.Repository == "" {
					continue
				}
				if rel, err := filepath.Rel(f.dataDir, other.deployment.Repository); err == nil && rel != "." && filepath.IsLocal(rel) {
					nested = append(nested, rel)
				}
			}
			f.apps[i].nestedMu.Lock()
			f.apps[i].nested = nested
			f.apps[i].nestedMu.Unlock()
			break
		}
	}
}

// AddRepository registers a repository and starts serving it: its App is
// recovered immediately and joins the rotation. The returned App is paused
// by its default control until the operator configures and resumes it.
func (f *Fleet) AddRepository(entry store.RepositoryEntry) (*App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx.Err() != nil {
		return nil, fmt.Errorf("The service is shutting down")
	}
	var retired *App
	legacyLocked := false
	if len(f.entries) == 0 && len(f.apps) == 1 {
		retired = f.apps[0]
		// Registry materialization and a legacy SaveConfig must agree on
		// which repository owns the unsuffixed settings key.
		retired.gate.Lock()
		legacyLocked = true
		defer func() {
			if legacyLocked {
				retired.gate.Unlock()
			}
		}()
	}
	if len(f.options.Deployments) > 0 {
		if _, pinned := f.options.Deployments[strings.ToLower(entry.GitHubRepo)]; !pinned {
			return nil, fmt.Errorf("Repositories are fixed by this deployment (OCTOMUS_GITHUB_REPO and OCTOMUS_GITHUB_REPOS); change them there and restart")
		}
	}
	if err := f.root.RegisterRepository(entry); err != nil {
		return nil, err
	}
	registered, err := f.root.Repositories()
	if err != nil {
		return nil, err
	}
	if existing := f.forLocked(entry.GitHubRepo); existing != nil && len(f.entries) > 0 {
		return existing, nil
	}
	// A fleet that started unscoped retires that App once repositories are
	// registered; scoped Apps re-read the same saved policy (legacy
	// identities for the primary). The retired App shuts down first so its
	// workers are joined before the replacements recover their state.
	var added []*App
	var addedEntries []store.RepositoryEntry
	discard := func() {
		for _, app := range added {
			app.Shutdown()
		}
	}
	// Hold the fleet lock until every replacement has recovered. Neither the
	// scheduler nor API readers may see an App whose previous workers still run.
	for _, next := range registered {
		if f.entryFor(f.entries, next.GitHubRepo) != nil {
			continue
		}
		app, err := f.repositoryApp(next)
		if err != nil {
			discard()
			return nil, err
		}
		added = append(added, app)
		addedEntries = append(addedEntries, next)
	}
	// Finish filesystem initialization while the original primary is still
	// live. Recovery itself must wait until its old workers have joined.
	if retired != nil {
		retired.cancel()
		retired.gate.Unlock()
		legacyLocked = false
		retired.Shutdown()
	}
	for _, app := range added {
		if err := app.Recover(); err != nil {
			if retired != nil {
				// The original App cannot resume after shutdown. Stop the
				// fleet instead of leaving an apparently live idle service.
				f.cancel()
			}
			discard()
			return nil, fmt.Errorf("Repository %s registered but its recovery failed; restart the service: %w", app.Repository(), err)
		}
	}
	if retired != nil {
		f.apps = nil
	}
	for i, app := range added {
		f.publish(addedEntries[i], app)
	}
	return f.forLocked(entry.GitHubRepo), nil
}

// Apps returns every served repository's App in registration order.
func (f *Fleet) Apps() []*App {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*App{}, f.apps...)
}

// Entries returns the registry entries being served, in registration order.
func (f *Fleet) Entries() []store.RepositoryEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.RepositoryEntry{}, f.entries...)
}

// Primary is the first registered repository's App, or the unscoped App when
// no repository is registered yet.
func (f *Fleet) Primary() *App {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apps[0]
}

// For returns the App serving repo (ASCII-case-insensitive); empty selects
// the primary. Unknown repositories return nil.
func (f *Fleet) For(repo string) *App {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forLocked(repo)
}

func (f *Fleet) forLocked(repo string) *App {
	if repo == "" {
		return f.apps[0]
	}
	for i, entry := range f.entries {
		if config.EqualASCII(entry.GitHubRepo, repo) {
			return f.apps[i]
		}
	}
	if len(f.entries) == 0 {
		// The first saved configuration can name the legacy App before
		// registration materializes it in the fleet.
		if cfg, err := f.apps[0].Config(); err == nil && config.EqualASCII(cfg.GitHubRepo, repo) {
			return f.apps[0]
		}
	}
	return nil
}

// Repository names the GitHub repository an App serves ("" when unscoped).
func (a *App) Repository() string { return a.store.Repository() }

func (f *Fleet) Context() context.Context { return f.ctx }

// Recover runs startup recovery for every App in registration order.
func (f *Fleet) Recover() error {
	for _, app := range f.Apps() {
		if err := app.Recover(); err != nil {
			if repo := app.Repository(); repo != "" {
				return fmt.Errorf("Repository %s: %w", repo, err)
			}
			return err
		}
	}
	return nil
}

// Run ticks every App on the shared scheduler cadence, rotating the starting
// repository each pass so no repository's cycle start, dispatch or
// housekeeping systematically precedes another's. Task workers still run
// concurrently inside each App.
func (f *Fleet) Run(ctx context.Context) error {
	stop := context.AfterFunc(ctx, f.cancel)
	defer stop()
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()
	start := 0
	for {
		if ctx.Err() != nil {
			f.Shutdown()
			return nil
		}
		apps := f.Apps()
		for i := range apps {
			app := apps[(start+i)%len(apps)]
			if err := app.tick(); err != nil && ctx.Err() == nil {
				app.fail(err)
			}
		}
		if len(apps) > 0 {
			start = (start + 1) % len(apps)
		}
		select {
		case <-ctx.Done():
			f.Shutdown()
			return nil
		case <-f.ctx.Done():
			f.Shutdown()
			return nil
		case <-ticker.C:
		}
	}
}

// Shutdown cancels and joins every App. It is safe to call more than once.
func (f *Fleet) Shutdown() {
	f.cancel()
	for _, app := range f.Apps() {
		app.Shutdown()
	}
}

// ForRecord resolves the App owning one record by the repository saved on
// it. Missing or unattributed records resolve to the primary App, which
// answers exactly as the single-repository service did (usually not-found).
func (f *Fleet) ForRecord(kind, id string) (*App, error) {
	repo, found, err := f.root.RecordRepository(kind, id)
	if err != nil {
		return nil, err
	}
	if found {
		if app := f.For(repo); app != nil {
			return app, nil
		}
	}
	return f.Primary(), nil
}

// InstanceSettings returns the saved instance-wide policy.
func (f *Fleet) InstanceSettings() (config.InstanceSettings, error) {
	return f.root.InstanceSettings()
}

// SetInstanceSettings saves the instance-wide policy.
func (f *Fleet) SetInstanceSettings(settings config.InstanceSettings) error {
	return f.root.SetInstanceSettings(settings)
}

// Registered returns the durable repository registry, including the implicit
// legacy repository a pre-multi-repository database carries in its saved
// configuration.
func (f *Fleet) Registered() ([]store.RepositoryEntry, error) {
	return f.root.Repositories()
}
