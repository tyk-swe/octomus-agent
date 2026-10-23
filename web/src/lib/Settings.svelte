<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import { Resource } from './resource';
  import { client, ApiError } from './api';
  import { clockTime, plural } from './format';
  import type {
    Backend,
    Config,
    CycleMode,
    DoctorResponse,
    ModelCatalog,
    RepositoriesView,
    Route,
    SettingsView,
    TransformedField
  } from './types';
  import RelativeTime from './RelativeTime.svelte';
  import RouteEditor from './RouteEditor.svelte';
  import {
    BACKENDS,
    backendLabel,
    catalogForRoute,
    catalogsFromDoctor,
    checkRoute,
    failedCatalog
  } from './modelRoutes';
  import SetupChecklist from './SetupChecklist.svelte';
  import BaselineCheck from './BaselineCheck.svelte';
  import { type Preflight, type SetupStatus } from './setup';
  import {
    configurationDraft,
    configurationPatch,
    discardConfigurationDraft,
    draftCommands as commandsForDraft,
    draftExcluded as excludedForDraft,
    draftDirty,
    replaceDraftField,
    type ConfigurationDraft
  } from './configurationDraft';
  import Icon from './Icon.svelte';
  import { LIMITS, limitHelp } from './limits';
  let {
    active,
    editable,
    status,
    onsaved,
    onbaselinechanged,
    onchoose
  }: {
    active: boolean;
    editable: boolean;
    status: SetupStatus | null;
    onsaved: (repository?: string) => void;
    onbaselinechanged: () => void;
    onchoose: (action: 'audit' | 'cycle') => void;
  } = $props();
  let draft = $state<ConfigurationDraft | null>(null);
  const config = $derived(draft?.values ?? null);
  const revision = $derived(draft?.revision ?? '');
  const savedConfig = $derived(draft?.saved ?? null);
  const transformed = $derived(draft?.transformed ?? []);
  const replaced = $derived(draft?.replaced ?? {});
  let loading = $state(false),
    loadError = $state(''),
    error = $state(''),
    conflict = $state(false),
    message = $state(''),
    pending = $state(''),
    catalogs = $state<Partial<Record<Backend, ModelCatalog>>>({}),
    preflight = $state<Preflight | null>(null),
    connectionDetails = $state<DoctorResponse | null>(null);
  const busy = $derived(pending !== '');
  const dirty = $derived(draft !== null && draftDirty(draft));
  const draftCommands = $derived(draft ? commandsForDraft(draft) : []);
  const draftExcluded = $derived(draft ? excludedForDraft(draft) : []);
  let fleet = $state<RepositoriesView | null>(null),
    newRepository = $state(''),
    instanceCeiling = $state(0),
    fleetError = $state(''),
    fleetBusy = $state(false);
  let disposed = false;
  let diagnostic: AbortController | null = null;
  let received: SettingsView | null = null;
  const settingsResource = new Resource(
    (_key: string, signal) => client.settings(signal),
    (state) => {
      received = state.value;
      loading = state.loading;
      loadError = state.error;
    }
  );
  onDestroy(() => {
    disposed = true;
    diagnostic?.abort();
    settingsResource.dispose();
  });
  const transformedByField = $derived(new Map(transformed.map((entry) => [entry.field, entry])));
  const locked = (field: string) => transformedByField.has(field) && !replaced[field];
  const pinnedRepository = $derived(status?.sandbox.pinned_repository ?? null);
  const sandboxed = $derived(status?.sandbox.mode === 'docker');
  const previewKind = (field: string) =>
    transformedByField.get(field)?.kinds.includes('redacted') ? 'hidden' : 'shortened';
  $effect(() => {
    if (active) untrack(() => void load());
  });
  $effect(() => {
    if (dirty) message = '';
  });
  const categories = [
    'features',
    'correctness',
    'performance',
    'ux-dx',
    'refactoring',
    'simplification',
    'tests',
    'dependencies',
    'documentation'
  ];
  const categoryLabel = (category: string) => (category === 'ux-dx' ? 'UX & DX' : category);
  const names: Record<string, string> = {
    orchestrator: 'Orchestrator',
    discovery: 'Discovery agents',
    proposal_reviewer: 'Proposal reviewers',
    code_reviewer: 'Code reviewer'
  };
  const ROLES = ['orchestrator', 'discovery', 'proposal_reviewer', 'code_reviewer'];
  const TIERS = ['XS', 'S', 'M', 'L', 'XL'];
  const ordered = (keys: string[], known: string[]) => [
    ...known.filter((key) => keys.includes(key)),
    ...keys.filter((key) => !known.includes(key))
  ];
  const routeIssues = $derived.by(() => {
    if (!config) return [];
    const routes = [
      ...ordered(Object.keys(config.roles), ROLES).map((role) => ({
        name: names[role] ?? role,
        route: config.roles[role],
        field: 'roles',
        anchor: 'route-' + role
      })),
      ...ordered(Object.keys(config.tiers), TIERS).map((tier) => ({
        name: tier + ' execution',
        route: config.tiers[tier],
        field: 'tiers',
        anchor: 'route-tier-' + tier
      })),
      { name: 'Repair', route: config.repair_route, field: 'repair_route', anchor: 'route-repair' }
    ];
    return routes
      .filter((entry) => !locked(entry.field))
      .flatMap((entry) => {
        const { problem } = checkRoute(entry.route, routeCatalog(entry.route));
        return problem ? [{ ...entry, problem }] : [];
      });
  });
  const connectionWarnings = $derived(
    connectionDetails
      ? [
          ...new Set([
            ...connectionDetails.warnings,
            ...connectionDetails.backends.flatMap((backend) =>
              backend.warning ? [backend.warning] : []
            )
          ])
        ]
      : []
  );
  async function load() {
    if (loading || busy || dirty) return;
    void loadFleet();
    if (await settingsResource.refresh('settings')) {
      // Edits typed during a reload still belong to this draft.
      if (!disposed && !dirty && received) acceptSaved(received);
    }
  }
  async function loadFleet() {
    try {
      const view = await client.repositories();
      if (disposed) return;
      fleet = view;
      instanceCeiling = view.instance_max_sessions_per_day;
      fleetError = '';
    } catch (e) {
      if (!disposed) fleetError = (e as Error).message;
    }
  }
  async function addRepository() {
    const repo = newRepository.trim();
    if (!repo || fleetBusy) return;
    fleetBusy = true;
    fleetError = '';
    try {
      fleet = await client.addRepository(repo);
      newRepository = '';
      onsaved();
    } catch (e) {
      if (!disposed) fleetError = (e as Error).message;
    } finally {
      if (!disposed) fleetBusy = false;
    }
  }
  async function saveInstanceCeiling() {
    if (fleetBusy) return;
    fleetBusy = true;
    fleetError = '';
    try {
      fleet = await client.saveInstance(Math.max(0, Math.floor(instanceCeiling) || 0));
    } catch (e) {
      if (!disposed) fleetError = (e as Error).message;
    } finally {
      if (!disposed) fleetBusy = false;
    }
  }

  function acceptSaved(view: SettingsView) {
    if (!draft || view.revision !== revision) {
      error = '';
      conflict = false;
      message = '';
      preflight = null;
      connectionDetails = null;
    }
    draft = configurationDraft(view);
    loadError = '';
  }
  function unlockField(field: string) {
    if (draft) replaceDraftField(draft, field);
  }
  function discard() {
    if (!dirty || busy) return;
    if (draft) draft = discardConfigurationDraft(draft);
    error = '';
    conflict = false;
    message = 'Changes discarded. Saved configuration restored.';
  }
  async function reload() {
    if (busy || loading) return;
    if (draft) draft = discardConfigurationDraft(draft);
    error = '';
    conflict = false;
    message = '';
    await load();
    if (!loadError && !dirty) message = 'Edits discarded. Saved configuration reloaded.';
  }
  async function save() {
    if (!config || !editable || status?.control_state_pending || busy || loading || !dirty) return;
    pending = 'save';
    error = '';
    conflict = false;
    message = '';
    try {
      const patch = configurationPatch(draft!);
      const view = await client.saveSettings(revision, patch);
      if (disposed) return;
      acceptSaved(view);
      message = 'Configuration saved.';
      onsaved(view.config.github_repo);
    } catch (e) {
      if (disposed) return;
      error = (e as Error).message;
      conflict = e instanceof ApiError && e.code === 'config_revision_conflict';
    } finally {
      if (!disposed) pending = '';
    }
  }
  function routeCatalog(route: Route) {
    return config ? catalogForRoute(route, config, catalogs) : undefined;
  }
  function cancelDiagnostic() {
    if (!diagnostic || diagnostic.signal.aborted) return;
    diagnostic.abort();
    message = pending.startsWith('catalog-')
      ? 'Catalog loading cancelled.'
      : 'Connection check cancelled.';
  }
  async function catalog(backend: Backend) {
    if (!config || !editable || busy || loading || locked(`${backend}_binary`)) return;
    const binary = config[`${backend}_binary`];
    const request = new AbortController();
    diagnostic = request;
    pending = `catalog-${backend}`;
    error = '';
    conflict = false;
    message = '';
    try {
      const models = await client.catalog(backend, binary, request.signal);
      if (disposed || request.signal.aborted || diagnostic !== request) return;
      catalogs[backend] = { binary, models, loaded: true };
      const available = models.filter((model) => model.available).length;
      message = `${plural(available, `${backendLabel(backend)} model`)} available. Routes are never silently substituted.`;
    } catch (e) {
      if (disposed || request.signal.aborted || diagnostic !== request) return;
      error = (e as Error).message;
      catalogs[backend] = failedCatalog(catalogs[backend], binary, error);
    } finally {
      if (!disposed && diagnostic === request) {
        diagnostic = null;
        pending = '';
      }
    }
  }
  async function doctor(mode: CycleMode) {
    if (!config || dirty || busy || loading) return;
    const request = new AbortController();
    diagnostic = request;
    pending = mode;
    error = '';
    conflict = false;
    message = '';
    const at = clockTime();
    try {
      const result = await client.doctor(mode, request.signal);
      if (disposed || request.signal.aborted || diagnostic !== request) return;
      catalogs = catalogsFromDoctor(catalogs, result);
      connectionDetails = result;
      message = result.message;
      preflight = {
        mode,
        ok: true,
        detail: result.message,
        checkedRevision: result.checked_revision,
        at
      };
    } catch (e) {
      if (disposed || request.signal.aborted || diagnostic !== request) return;
      error = (e as Error).message;
      preflight =
        e instanceof ApiError && e.checkedRevision
          ? { mode, ok: false, detail: error, checkedRevision: e.checkedRevision, at }
          : null;
    } finally {
      if (!disposed && diagnostic === request) {
        diagnostic = null;
        pending = '';
      }
    }
  }
  function focusControl(target: string) {
    const element = document.getElementById(target);
    if (!element) return;
    const control = element.matches('input, select, textarea, button')
      ? element
      : (element.querySelector<HTMLElement>('[aria-invalid="true"]') ??
        element.querySelector<HTMLElement>('input, select, textarea, button'));
    (control ?? element).scrollIntoView({ block: 'center' });
    control?.focus({ preventScroll: true });
  }
</script>

<div class="settings-actions">
  <p class="muted" id="connection-check-help">
    Connection checks validate saved configuration. Save or discard edits before checking. Model
    catalogs use the executable paths entered below.
  </p>
  <div class="actions">
    <button
      id="check-connection"
      class="button"
      aria-describedby="connection-check-help"
      onclick={() => doctor('execution')}
      disabled={!config || busy || loading || dirty}
      ><Icon name="shield" size={16} />{pending === 'execution'
        ? 'Checking connection…'
        : 'Check connection'}</button
    >
    <button
      id="check-audit-connection"
      class="button"
      aria-describedby="connection-check-help"
      onclick={() => doctor('audit')}
      disabled={!config || busy || loading || dirty}
      >{pending === 'audit' ? 'Checking audit connection…' : 'Check audit connection'}</button
    >
    {#if pending === 'audit' || pending === 'execution' || pending.startsWith('catalog-')}
      <button type="button" class="button" onclick={cancelDiagnostic}>Cancel diagnostic</button>
    {/if}
  </div>
</div>
{#if connectionDetails}
  <section class="panel connection-details" aria-labelledby="connection-details-heading">
    <div class="section-heading">
      <div>
        <h2 id="connection-details-heading">Last successful connection check</h2>
        <p>
          {connectionDetails.mode === 'audit' ? 'Audit' : 'Execution'} check of saved configuration
          <code>{connectionDetails.checked_revision}</code>.
          {#if dirty || connectionDetails.checked_revision !== revision}
            These details do not cover the current draft.
          {/if}
          No model inference was performed.
        </p>
      </div>
    </div>
    <dl class="facts">
      {#each connectionDetails.backends as backend (backend.backend)}
        <div>
          <dt>{backendLabel(backend.backend)}</dt>
          <dd>{backend.version} · Protocol {backend.protocol_version}</dd>
        </div>
      {/each}
    </dl>
    {#if connectionWarnings.length}
      <ul class="connection-warnings" aria-label="Connection warnings">
        {#each connectionWarnings as warning}<li>{warning}</li>{/each}
      </ul>
    {/if}
  </section>
{/if}
{#if !editable}<div class="notice">
    <Icon name="clock" />
    {status?.auto_merge_active
      ? 'An automatic merge check is still running. Wait for it to finish before editing configuration.'
      : 'Pause the service and wait for active work to finish to edit configuration.'}
  </div>{/if}
{#if loadError}<div class="notice error" role="alert">
    <span
      >{config
        ? 'Could not refresh saved configuration. Displaying the last loaded values.'
        : 'Could not load configuration.'}
      {loadError}</span
    >
    <button class="button" onclick={load} disabled={loading || busy || dirty}>Retry</button>
  </div>{/if}
{#if config && draft}
  <SetupChecklist
    draft={config}
    saved={savedConfig}
    {revision}
    commands={draftCommands}
    {dirty}
    {catalogs}
    {preflight}
    {status}
    onfocus={focusControl}
    {onchoose}
  />
  <section class="panel settings-section">
    <div class="section-heading">
      <h2>Repositories & instance</h2>
    </div>
    <div class="form-grid">
      {#if fleet}<p class="full">
          This instance serves {fleet.repositories.length}
          {fleet.repositories.length === 1 ? 'repository' : 'repositories'}: {fleet.repositories
            .map((entry) => entry.github_repo)
            .join(', ')}. Each has its own settings, pause state and daily cap; switch in the
          sidebar.
        </p>{/if}{#if fleetError}<div class="notice error full" role="alert">
          {fleetError}
        </div>{/if}
      <label
        >Add a repository<input
          bind:value={newRepository}
          placeholder="owner/repository"
          disabled={fleetBusy}
        /><small>Starts paused with default settings; select it, then save its own.</small></label
      >
      <div>
        <button
          class="button"
          type="button"
          onclick={addRepository}
          disabled={fleetBusy || !newRepository.trim()}>Add repository</button
        >
      </div>
      <label
        >Instance-wide sessions per day<input
          type="number"
          min="0"
          step="1"
          bind:value={instanceCeiling}
          disabled={fleetBusy}
        /><small
          >Ceiling across every repository, on top of each repository's own cap. 0 means no instance
          ceiling.</small
        ></label
      >
      <div>
        <button class="button" type="button" onclick={saveInstanceCeiling} disabled={fleetBusy}
          >Save instance ceiling</button
        >
      </div>
    </div>
  </section>
  <form
    onsubmit={(e) => {
      e.preventDefault();
      save();
    }}
  >
    {#snippet previewNote(field: string, noun: string, collection: boolean)}
      {@const entry = transformedByField.get(field)}
      {#if entry}
        <small class="preview-note" id={'preview-' + field}>
          {#if !replaced[field]}
            {collection
              ? `Saved ${noun} ${previewKind(field) === 'hidden' ? 'contain a hidden value' : 'are shortened'} in this preview and stay unchanged on save. `
              : `The saved ${noun} is ${previewKind(field)} in this preview and stays unchanged on save. `}<button
              type="button"
              class="replace-preview"
              id={'replace-' + field}
              onclick={() => unlockField(field)}
              >Replace the {collection ? 'complete ' : 'saved '}{noun}</button
            >
          {:else}
            Replacing the saved {noun}; {collection
              ? 'hidden values must be re-entered in full'
              : 'the hidden value must be re-entered'} — previews are never sent back.
          {/if}
        </small>
      {/if}
    {/snippet}
    <fieldset disabled={!editable || busy}>
      <section class="panel settings-section">
        <div class="section-heading">
          <h2>Repository</h2>
        </div>
        <div class="form-grid">
          <label
            >Repository path<input
              id="repository-path"
              bind:value={config.repository}
              readonly={locked('repository') || pinnedRepository !== null}
              placeholder="/srv/projects/your-project"
            />{@render previewNote('repository', 'value', false)}<small
              >{pinnedRepository
                ? 'The trusted checkout in the data volume, fixed by the deployment.'
                : 'Absolute path to the checkout on this host.'}</small
            ></label
          >
          <label
            >GitHub repository<input
              bind:value={config.github_repo}
              readonly={locked('github_repo') || pinnedRepository !== null}
              placeholder="owner/repository"
            />{@render previewNote('github_repo', 'value', false)}<small
              >{pinnedRepository
                ? 'Set by OCTOMUS_GITHUB_REPO in the deployment; the operator token cannot change it.'
                : 'Must match the checkout’s origin remote.'}</small
            ></label
          >
          <label
            >Default branch<input
              bind:value={config.default_branch}
              readonly={locked('default_branch')}
              required
            />{@render previewNote('default_branch', 'value', false)}</label
          >
          <label
            >Owned branch prefix<input
              bind:value={config.branch_prefix}
              readonly={locked('branch_prefix')}
              required
            />{@render previewNote('branch_prefix', 'value', false)}</label
          >
          <label class="full"
            >Verification commands<textarea
              id="verification-commands"
              bind:value={draft.commands}
              rows="3"
              readonly={locked('verification_commands')}
              placeholder={'npm test\nnpm run build'}></textarea>{@render previewNote(
              'verification_commands',
              'command list',
              true
            )}<small
              >One shell command per line, each run in a fresh sandbox inside the task workspace.
              All must pass before publication.</small
            ></label
          >
        </div>
      </section>
      <section class="panel settings-section">
        <div class="section-heading">
          <h2>Models & reasoning</h2>
        </div>
        <div class="form-grid">
          <label
            >Codex executable<input
              bind:value={config.codex_binary}
              readonly={locked('codex_binary') || sandboxed}
              required
            />{@render previewNote('codex_binary', 'value', false)}{#if sandboxed}<small
                >Sandboxes run the Codex in the sandbox image.</small
              >{/if}</label
          >
          <label
            >OpenCode executable<input
              aria-label="OpenCode executable"
              aria-describedby="opencode-executable-help"
              bind:value={config.opencode_binary}
              readonly={locked('opencode_binary') || sandboxed}
              required
            />{@render previewNote('opencode_binary', 'value', false)}<small
              id="opencode-executable-help"
              >{sandboxed
                ? 'Sandboxes run the OpenCode in the sandbox image, with the providers and login in the runner volume.'
                : "Uses the service user's configured providers and login."}</small
            ></label
          >
        </div>
        <div class="catalog-actions">
          {#each BACKENDS as backend}
            {@const label = backendLabel(backend)}
            {@const preview = locked(`${backend}_binary`)}
            <button
              id={`load-${backend}-models`}
              type="button"
              class="button small"
              onclick={() => catalog(backend)}
              disabled={loading || preview}
              aria-describedby={preview ? `catalog-preview-${backend}` : undefined}
              >{pending === `catalog-${backend}`
                ? `Loading ${label} models…`
                : `Load ${label} models`}</button
            >
          {/each}
        </div>
        {#each BACKENDS as backend}
          {#if catalogs[backend]?.binary === config[`${backend}_binary`] && catalogs[backend]?.error}
            <p class="catalog-note" role="status">
              {backendLabel(backend)}: {catalogs[backend]?.error}
              {catalogs[backend]?.loaded
                ? 'Showing stale, unchecked choices.'
                : 'Catalog unavailable.'}
              Use Load {backendLabel(backend)} models to retry. Saved routes are unchanged.
            </p>
          {/if}
          {#if locked(`${backend}_binary`)}<p
              class="muted catalog-note"
              id={`catalog-preview-${backend}`}
            >
              Replace the {backendLabel(backend)} executable preview to load its catalog.
            </p>{/if}
        {/each}
        {#if routeIssues.length}
          <section class="route-issues" aria-labelledby="route-issues-heading">
            <h3 id="route-issues-heading">Model routes needing attention</h3>
            <p>Incomplete drafts can still be saved. No route is silently substituted.</p>
            <ul>
              {#each routeIssues as issue (issue.anchor)}
                <li>
                  <button
                    type="button"
                    class="text-button"
                    onclick={() => focusControl(issue.anchor)}>{issue.name}</button
                  >: {issue.problem}
                </li>
              {/each}
            </ul>
          </section>
        {/if}
        {@render previewNote('roles', 'role routes', true)}
        {#each ordered(Object.keys(config.roles), ROLES) as role}
          <RouteEditor
            name={names[role] ?? role}
            anchor={'route-' + role}
            bind:route={config.roles[role]}
            catalog={routeCatalog(config.roles[role])}
            disabled={locked('roles')}
          />
        {/each}
        {@render previewNote('tiers', 'tier routes', true)}
        {#each ordered(Object.keys(config.tiers), TIERS) as tier}
          <RouteEditor
            name={tier + ' execution'}
            anchor={'route-tier-' + tier}
            bind:route={config.tiers[tier]}
            catalog={routeCatalog(config.tiers[tier])}
            disabled={locked('tiers')}
          />
        {/each}
        {@render previewNote('repair_route', 'repair route', true)}
        <RouteEditor
          name="Repair"
          anchor="route-repair"
          bind:route={config.repair_route}
          catalog={routeCatalog(config.repair_route)}
          disabled={locked('repair_route')}
        />
        <div class="inline-note">
          <Icon name="shield" size={16} /> Each task keeps its saved repair route and reuses one repair
          thread across rounds.
        </div>
      </section>
      <section class="panel settings-section">
        <div class="section-heading">
          <h2>Improvement coverage</h2>
        </div>
        {@render previewNote('categories', 'category set', true)}
        <div class="category-options">
          {#each categories as category}<label class="checkbox"
              ><input
                type="checkbox"
                value={category}
                disabled={locked('categories') ||
                  (config.delivery_mode === 'maintenance' && category === 'features')}
                bind:group={config.categories}
              /><span>{categoryLabel(category)}</span></label
            >{/each}
        </div>
        {#if config.delivery_mode === 'maintenance'}<p class="muted">
            Features stay in the saved category selection but are never eligible while maintenance
            delivery is active.
          </p>{/if}
        <div class="form-grid">
          <label class="full"
            >Planning guidance<textarea
              id="planning-guidance"
              bind:value={config.planning_guidance}
              rows="4"
              readonly={locked('planning_guidance')}
              placeholder="Prioritise test coverage in internal/store. Avoid changes under web/."
            ></textarea>{@render previewNote('planning_guidance', 'guidance', false)}<small
              >Optional. Shown to planning agents and reviewers as your preference for what to
              prioritise or avoid; it never bypasses review, verification or limits. At most 4,096
              bytes.</small
            ></label
          >
        </div>
      </section>
      <section class="panel settings-section">
        <div class="section-heading">
          <h2>Delivery</h2>
        </div>
        <div class="form-grid">
          <label class="full"
            >Delivery mode<select
              id="delivery-mode"
              aria-label="Delivery mode"
              bind:value={config.delivery_mode}
              required
            >
              <option value="standard">Standard — publish pull requests for manual merge</option>
              <option value="maintenance"
                >Maintenance — reviewed small maintenance PRs can merge automatically</option
              >
            </select><small
              >Standard keeps every pull request manual. Maintenance restricts work to non-feature
              upkeep.</small
            ></label
          >
          {#if config.delivery_mode === 'maintenance'}
            <div class="notice delivery-warning" role="status">
              <Icon name="alert" /> Maintenance mode is opt-in: after a clean scope-assessed review and
              passing verification, the orchestrator can squash-merge a small pull request itself when
              GitHub checks, reviews and protections allow it. Sensitive or oversized changes stay manual.
            </div>
            <label class="full"
              >Manual-merge paths<textarea
                id="auto-merge-excluded-paths"
                bind:value={draft.excluded}
                rows="3"
                readonly={locked('auto_merge_excluded_paths')}
                placeholder={'deploy/\nsecrets.txt'}></textarea>{@render previewNote(
                'auto_merge_excluded_paths',
                'path list',
                true
              )}<small
                >One repository-relative name or subtree prefix per line; a matching change keeps
                the pull request manual. CI rules, security policy, deployment and migration paths
                are always manual.</small
              ></label
            >
          {/if}
        </div>
      </section>
      <section class="panel settings-section">
        <div class="section-heading">
          <h2>Scheduling & operating limits</h2>
        </div>
        {@render previewNote('runner_storage_paths', 'storage paths', true)}
        <div class="form-grid">
          {#each BACKENDS as backend}
            <label
              >{backendLabel(backend)} storage measurement path (optional)
              <input
                value={config.runner_storage_paths[backend] ?? ''}
                placeholder="Absolute path to runner storage"
                readonly={locked('runner_storage_paths')}
                oninput={(event) => {
                  const path = event.currentTarget.value;
                  if (path) config!.runner_storage_paths[backend] = path;
                  else delete config!.runner_storage_paths[backend];
                }}
              />
              <small
                >Directory sizes only, measured every 15 minutes. Octomus never deletes this
                storage.</small
              >
            </label>
          {/each}
          {#each LIMITS as limit}<label
              >{limit.label}<input
                type="number"
                min={limit.min}
                max={limit.max}
                step="1"
                bind:value={config[limit.key]}
                required
              /><small>{limitHelp(limit)}</small></label
            >{/each}
        </div>
      </section>
    </fieldset>
    <div class="save-bar">
      <div class="save-summary">
        <strong aria-live="polite">{dirty ? 'Unsaved changes' : 'Saved configuration'}</strong>
        <p class="muted">
          Changes apply to new work. Drafts stay in this tab until disconnect or reload.
        </p>
        {#if status?.control_state_pending}<p class="muted" role="status">
            Waiting for current service activity before saving configuration.
          </p>{/if}
      </div>
      <div class="actions">
        <button class="button" type="button" onclick={discard} disabled={!dirty || busy}
          >Discard changes</button
        >
        <button
          class="button primary"
          type="submit"
          disabled={!editable || status?.control_state_pending || busy || loading || !dirty}
          >{pending === 'save' ? 'Saving configuration…' : 'Save configuration'}<Icon
            name="check"
            size={16}
          /></button
        >
      </div>
      {#if error}<div class="notice error settings-feedback" role="alert">
          <span>{error}</span>{#if conflict}<button
              type="button"
              class="button small"
              onclick={reload}
              disabled={busy || loading}>Discard edits and reload</button
            >{/if}
        </div>{/if}
      {#if message}<div class="notice success settings-feedback" role="status">{message}</div>{/if}
    </div>
  </form>
  <BaselineCheck
    {active}
    {editable}
    controlStatePending={status?.control_state_pending ?? false}
    savedRevision={revision}
    {dirty}
    onchanged={onbaselinechanged}
  />
  <section class="panel settings-section" aria-labelledby="notifications-heading">
    <div class="section-heading">
      <div>
        <h2 id="notifications-heading">Attention notifications</h2>
        <p>
          Read-only delivery health for the webhook configured by the
          <code>OCTOMUS_NOTIFICATION_WEBHOOK_URL</code> service environment variable. Set it on the service
          host to receive attention notices.
        </p>
      </div>
      <Icon name="alert" />
    </div>
    {#if status?.notifications}
      {@const health = status.notifications}
      <dl class="facts notification-facts">
        <div>
          <dt>State</dt>
          <dd>
            {health.state === 'enabled'
              ? 'Enabled'
              : health.state === 'invalid'
                ? 'Configured but invalid'
                : 'Disabled'}
          </dd>
        </div>
        <div>
          <dt>Destination</dt>
          <dd>{health.configured ? 'Configured' : 'Not set'}</dd>
        </div>
        <div>
          <dt>Pending</dt>
          <dd>{health.pending}</dd>
        </div>
        <div>
          <dt>Failed</dt>
          <dd>{health.failed}</dd>
        </div>
        {#if health.last_delivered_at}<div>
            <dt>Last delivered</dt>
            <dd>
              <RelativeTime value={health.last_delivered_at} />
            </dd>
          </div>{/if}
        {#if health.last_error}<div>
            <dt>Last error</dt>
            <dd>
              {health.last_error}{health.last_http_status !== null
                ? ` (HTTP ${health.last_http_status})`
                : ''}
            </dd>
          </div>{/if}
      </dl>
    {:else}
      <p class="muted">Notification status is unavailable.</p>
    {/if}
  </section>
{:else if loading}<div class="empty" role="status">
    <span class="spinner"></span>
    <p>Loading configuration…</p>
  </div>{/if}

<style>
  .preview-note {
    display: block;
    font-weight: 400;
    font-size: 12px;
    line-height: 1.7;
    color: var(--warn-fg);
  }
  .settings-section > .preview-note {
    margin: 0 24px 14px;
  }
  .replace-preview {
    background: none;
    border: none;
    padding: 0;
    color: var(--accent);
    font-size: 12px;
    font-weight: 600;
    cursor: pointer;
    text-decoration: underline;
  }
  .notification-facts {
    margin-bottom: 20px;
  }
  .connection-details {
    margin-bottom: 20px;
    padding-bottom: 20px;
  }
  .connection-details code,
  .connection-details dd {
    overflow-wrap: anywhere;
  }
  .connection-warnings,
  .route-issues {
    margin: 0 24px 20px;
    font-size: 12px;
    color: var(--warn-fg);
  }
  .route-issues li {
    margin: 6px 0;
    overflow-wrap: anywhere;
  }
  .catalog-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
    margin: 20px 24px;
  }
  .catalog-actions button {
    scroll-margin-block: 100px;
  }
  .catalog-note {
    margin: -8px 24px 16px;
    font-size: 12px;
  }
  .settings-feedback > span {
    flex: 1 1 16em;
  }
  .settings-feedback > .button {
    flex: 0 0 auto;
  }
</style>
