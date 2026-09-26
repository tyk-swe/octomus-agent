<script lang="ts">
  import { untrack } from 'svelte';
  import { api, ApiError, clockTime, relative } from './api';
  import type {
    Backend,
    Config,
    Model,
    ModelCatalog,
    Route,
    SettingsView,
    TransformedField
  } from './types';
  import RouteEditor from './RouteEditor.svelte';
  import { BACKENDS, backendLabel } from './routes';
  import SetupChecklist from './SetupChecklist.svelte';
  import BaselineCheck from './BaselineCheck.svelte';
  import { parseCommands, type Preflight, type SetupStatus } from './setup';
  import Icon from './Icon.svelte';
  import { LIMITS } from './limits';
  let {
    active,
    editable,
    status,
    onsaved,
    onchoose
  }: {
    active: boolean;
    editable: boolean;
    status: SetupStatus | null;
    onsaved: () => void;
    /** Hands the operator to the Overview controls without starting anything. */
    onchoose: (action: 'audit' | 'cycle') => void;
  } = $props();
  let config = $state<Config | null>(null),
    /** Canonical revision the displayed values came from; writes pin it and checks use it. */
    revision = $state(''),
    /** Serialized display baseline for the draft/dirty comparison. */
    baseline = $state(''),
    baselineCommands = $state(''),
    /** Every field the server transformed for display; those values are previews only. */
    transformed = $state<TransformedField[]>([]),
    /** Transformed fields the operator deliberately chose to replace in full. */
    replaced = $state<Record<string, boolean>>({}),
    loading = $state(false),
    loadError = $state(''),
    error = $state(''),
    /** The last save was refused because another save changed the revision first. */
    conflict = $state(false),
    message = $state(''),
    pending = $state(''),
    catalogs = $state<Partial<Record<Backend, ModelCatalog>>>({}),
    commands = $state(''),
    /** Result of the last explicit connection check, keyed to the exact saved revision. */
    preflight = $state<Preflight | null>(null);
  const busy = $derived(pending !== '');
  const dirty = $derived(
    config !== null && (JSON.stringify(config) !== baseline || commands !== baselineCommands)
  );
  const savedConfig = $derived<Config | null>(baseline ? JSON.parse(baseline) : null);
  const transformedByField = $derived(new Map(transformed.map((entry) => [entry.field, entry])));
  // A transformed collection is a read-only preview until deliberately replaced:
  // its hidden members must never be merged back by position.
  const locked = (field: string) => transformedByField.has(field) && !replaced[field];
  const previewKind = (field: string) =>
    transformedByField.get(field)?.kinds.includes('redacted') ? 'hidden' : 'shortened';
  // Revisit saved values only on navigation, never in response to a draft edit.
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
  // Routes render in pipeline and size order, mirroring config.Roles() and config.Tiers();
  // the service's JSON sorts map keys. Any unexpected key follows in received order.
  const ROLES = ['orchestrator', 'discovery', 'proposal_reviewer', 'code_reviewer'];
  const TIERS = ['XS', 'S', 'M', 'L', 'XL'];
  const ordered = (keys: string[], known: string[]) => [
    ...known.filter((key) => keys.includes(key)),
    ...keys.filter((key) => !known.includes(key))
  ];
  async function load() {
    if (loading || busy || dirty) return;
    loading = true;
    loadError = '';
    try {
      const view = await api<SettingsView>('/config');
      // The operator may have started typing while this refresh was in flight.
      if (!dirty) acceptSaved(view);
    } catch (e) {
      loadError = (e as Error).message;
    } finally {
      loading = false;
    }
  }
  function acceptSaved(view: SettingsView) {
    if (!baseline || view.revision !== revision) {
      error = '';
      conflict = false;
      message = '';
      // A connection check only ever covers the exact saved revision it ran against.
      preflight = null;
    }
    config = view.config;
    baseline = JSON.stringify(view.config);
    revision = view.revision;
    transformed = view.transformed_fields;
    commands = view.config.verification_commands.join('\n');
    baselineCommands = commands;
    replaced = {};
    // Every caller passes a fresh server view, so an earlier load failure is resolved.
    loadError = '';
  }
  // clearPath drops one display-transformed value so only deliberately supplied
  // text is ever sent back; hidden originals are never combined into a replacement.
  // Segments arrive structured: strings are object keys, numbers array indices.
  function clearPath(segments: (string | number)[]) {
    if (!config) return;
    let node: unknown = config;
    for (const segment of segments.slice(0, -1)) {
      node = (node as Record<string, unknown>)?.[segment as string];
      if (node == null) return;
    }
    const leaf = segments.at(-1);
    if (leaf === undefined || node == null) return;
    // Clearing keeps positions stable: an emptied string marks exactly where the
    // hidden value was, and every other member keeps its index.
    if (Array.isArray(node)) node[leaf as number] = '';
    else if (segments[0] === 'runner_storage_paths')
      delete (node as Record<string, unknown>)[leaf as string];
    else (node as Record<string, unknown>)[leaf as string] = '';
  }
  function unlockField(field: string) {
    for (const path of transformedByField.get(field)?.paths ?? []) clearPath(path);
    if (field === 'verification_commands') commands = '';
    replaced[field] = true;
  }
  function discard() {
    if (!dirty || busy) return;
    config = JSON.parse(baseline);
    commands = baselineCommands;
    replaced = {};
    error = '';
    conflict = false;
    message = 'Changes discarded. Saved configuration restored.';
  }
  /**
   * The service answers a stale save with 409 and asks to reload settings. This is the
   * explicit way to follow that in place: drop the draft, then load the saved configuration.
   */
  async function reload() {
    if (busy || loading) return;
    if (config) {
      config = JSON.parse(baseline);
      commands = baselineCommands;
      replaced = {};
    }
    error = '';
    conflict = false;
    message = '';
    await load();
    // An edit typed while the read was in flight keeps its draft, so nothing was reloaded.
    if (!loadError && !dirty) message = 'Edits discarded. Saved configuration reloaded.';
  }
  async function save() {
    if (!config || !editable || busy || loading || !dirty) return;
    pending = 'save';
    error = '';
    conflict = false;
    message = '';
    try {
      const draft: Config = { ...config, verification_commands: parseCommands(commands) };
      // Send only the top-level fields the operator changed; each supplied field
      // replaces its canonical value completely and omitted fields keep theirs.
      const patch: Record<string, unknown> = {};
      for (const key of Object.keys(draft) as (keyof Config)[]) {
        if (JSON.stringify(draft[key]) !== JSON.stringify(savedConfig?.[key]))
          patch[key] = draft[key];
      }
      const view = await api<SettingsView>('/config', 'PUT', {
        expected_revision: revision,
        config: patch
      });
      acceptSaved(view);
      message = 'Configuration saved.';
      onsaved();
    } catch (e) {
      error = (e as Error).message;
      // The service also answers 409 when it is no longer paused or when tasks must be
      // resolved first; a reload fixes neither. Only the stale-revision conflict asks for one.
      conflict = e instanceof ApiError && e.status === 409 && /\breload\b/i.test(error);
    } finally {
      pending = '';
    }
  }
  function routeCatalog(route: Route) {
    const backend = route.backend ?? 'codex';
    const entry = catalogs[backend];
    return entry?.binary === config?.[`${backend}_binary`] ? entry : undefined;
  }
  async function catalog(backend: Backend) {
    if (!config || !editable || busy || loading) return;
    const binary = config[`${backend}_binary`];
    pending = `catalog-${backend}`;
    error = '';
    conflict = false;
    message = '';
    try {
      const models = await api<Model[]>('/model-catalog', 'POST', { backend, binary });
      catalogs[backend] = { binary, models, loaded: true };
      message = `${models.filter((model) => model.available).length} ${backendLabel(backend)} models available. Routes are never silently substituted.`;
    } catch (e) {
      error = (e as Error).message;
      catalogs[backend] = { binary, models: [], loaded: false, error };
    } finally {
      pending = '';
    }
  }
  async function doctor(mode: 'execution' | 'audit') {
    if (!config || dirty || busy || loading) return;
    pending = mode;
    error = '';
    conflict = false;
    message = '';
    const at = clockTime();
    try {
      const result = await api<{ message: string; checked_revision: string }>(
        `/doctor?mode=${mode}`,
        'POST'
      );
      message = result.message;
      preflight = {
        mode,
        ok: true,
        detail: result.message,
        baseline: result.checked_revision,
        at
      };
    } catch (e) {
      error = (e as Error).message;
      preflight =
        e instanceof ApiError && e.checkedRevision
          ? { mode, ok: false, detail: error, baseline: e.checkedRevision, at }
          : null;
    } finally {
      pending = '';
    }
  }
  /** Checklist links move focus to the existing control; they never edit, save or start work. */
  function focusControl(target: string) {
    const element = document.getElementById(target);
    if (!element) return;
    const control = element.matches('input, select, textarea, button')
      ? element
      : element.querySelector<HTMLElement>('input, select, textarea, button');
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
  </div>
</div>
{#if !editable}<div class="notice">
    <Icon name="clock" /> Pause the service and wait for active work to finish to edit configuration.
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
{#if config}
  <SetupChecklist
    draft={config}
    saved={savedConfig}
    {revision}
    {commands}
    {dirty}
    {catalogs}
    {preflight}
    {status}
    onfocus={focusControl}
    {onchoose}
  />
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
          <div>
            <h2>Repository</h2>
            <p>Your project and its delivery destination.</p>
          </div>
          <Icon name="branch" />
        </div>
        <div class="form-grid">
          <label
            >Repository path<input
              id="repository-path"
              bind:value={config.repository}
              readonly={locked('repository')}
              placeholder="/srv/projects/your-project"
            />{@render previewNote('repository', 'value', false)}<small
              >Absolute path to the checkout on this host.</small
            ></label
          >
          <label
            >GitHub repository<input
              bind:value={config.github_repo}
              readonly={locked('github_repo')}
              placeholder="owner/repository"
            />{@render previewNote('github_repo', 'value', false)}<small
              >Must match the checkout’s origin remote.</small
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
              bind:value={commands}
              rows="3"
              readonly={locked('verification_commands')}
              placeholder={'npm test\nnpm run build'}
            ></textarea>{@render previewNote('verification_commands', 'command list', true)}<small
              >One shell command per line, run inside each task workspace. All must pass before
              publication.</small
            ></label
          >
        </div>
      </section>
      <section class="panel settings-section">
        <div class="section-heading">
          <div>
            <h2>Models & reasoning</h2>
            <p>Explicit routes for every role. No automatic substitutions.</p>
          </div>
        </div>
        <div class="form-grid">
          <label
            >Codex executable<input
              bind:value={config.codex_binary}
              readonly={locked('codex_binary')}
              required
            />{@render previewNote('codex_binary', 'value', false)}</label
          >
          <label
            >OpenCode executable<input
              aria-label="OpenCode executable"
              aria-describedby="opencode-executable-help"
              bind:value={config.opencode_binary}
              readonly={locked('opencode_binary')}
              required
            />{@render previewNote('opencode_binary', 'value', false)}<small
              id="opencode-executable-help"
              >Uses the service user's configured providers and login.</small
            ></label
          >
        </div>
        <div class="catalog-actions">
          {#each BACKENDS as backend}
            {@const label = backendLabel(backend)}
            <button
              id={`load-${backend}-models`}
              type="button"
              class="button small"
              onclick={() => catalog(backend)}
              disabled={loading}
              >{pending === `catalog-${backend}`
                ? `Loading ${label} models…`
                : `Load ${label} models`}</button
            >
          {/each}
        </div>
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
            bind:route={config.tiers[tier]}
            catalog={routeCatalog(config.tiers[tier])}
            disabled={locked('tiers')}
          />
        {/each}
        {@render previewNote('repair_route', 'repair route', true)}
        <RouteEditor
          name="Repair"
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
          <div>
            <h2>Improvement coverage</h2>
            <p>Discover work with a concrete benefit to your project.</p>
          </div>
          <Icon name="proposals" />
        </div>
        {@render previewNote('categories', 'category set', true)}
        <div class="category-options">
          {#each categories as category}<label class="checkbox"
              ><input
                type="checkbox"
                value={category}
                disabled={locked('categories')}
                bind:group={config.categories}
              /><span>{categoryLabel(category)}</span></label
            >{/each}
        </div>
      </section>
      <section class="panel settings-section">
        <div class="section-heading">
          <div>
            <h2>Scheduling & operating limits</h2>
            <p>Bound the work. Preserve anything that needs attention.</p>
          </div>
          <Icon name="settings" />
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
                  const path = event.currentTarget.value.trim();
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
              /><small>{limit.help}</small></label
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
      </div>
      <div class="actions">
        <button class="button" type="button" onclick={discard} disabled={!dirty || busy}
          >Discard changes</button
        >
        <button
          class="button primary"
          type="submit"
          disabled={!editable || busy || loading || !dirty}
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
  <BaselineCheck {active} {editable} savedRevision={revision} {dirty} onchanged={onsaved} />
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
      <dl class="baseline-facts">
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
              {relative(health.last_delivered_at)}
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
    color: #835d29;
  }
  /* Notes for whole collections sit at section level, outside the form grid. */
  .settings-section > .preview-note {
    margin: 0 24px 14px;
  }
  .replace-preview {
    background: none;
    border: none;
    padding: 0;
    color: var(--green);
    font-size: 12px;
    font-weight: 600;
    cursor: pointer;
    text-decoration: underline;
  }
  .baseline-facts {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 12px;
    margin: 0 24px 20px;
  }
  .baseline-facts dt {
    color: var(--muted);
    font-size: 12px;
  }
  .baseline-facts dd {
    margin: 4px 0 0;
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
  /* A narrow save bar moves the reload control below the message instead of squeezing it. */
  .settings-feedback > span {
    flex: 1 1 16em;
  }
  .settings-feedback > .button {
    flex: 0 0 auto;
  }
</style>
