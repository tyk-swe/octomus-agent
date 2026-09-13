<script lang="ts">
  import { untrack } from 'svelte';
  import { api, ApiError } from './api';
  import type { Backend, Config, Model, ModelCatalog, Route } from './types';
  import RouteEditor from './RouteEditor.svelte';
  import AstraRehearsal from './AstraRehearsal.svelte';
  import SetupChecklist from './SetupChecklist.svelte';
  import { configIdentity, type Preflight, type SetupStatus } from './setup';
  import Icon from './Icon.svelte';
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
    baseline = $state(''),
    baselineCommands = $state(''),
    loading = $state(false),
    loadError = $state(''),
    error = $state(''),
    message = $state(''),
    pending = $state(''),
    presetResetKey = $state(0),
    catalogs = $state<Partial<Record<Backend, ModelCatalog>>>({}),
    commands = $state(''),
    /** Result of the last explicit connection check, keyed to the exact saved configuration. */
    preflight = $state<Preflight | null>(null);
  const busy = $derived(pending !== '');
  const dirty = $derived(
    config !== null && (JSON.stringify(config) !== baseline || commands !== baselineCommands)
  );
  const savedConfig = $derived<Config | null>(baseline ? JSON.parse(baseline) : null);
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
  const names: Record<string, string> = {
    orchestrator: 'Orchestrator',
    discovery: 'Discovery agents',
    proposal_reviewer: 'Proposal reviewers',
    code_reviewer: 'Code reviewer'
  };
  const limits: { key: keyof Config; label: string; help: string; min: number; max?: number }[] = [
    {
      key: 'discovery_agents',
      label: 'Discovery agents',
      help: 'Complementary agents per cycle · 8–10',
      min: 8,
      max: 10
    },
    {
      key: 'execution_concurrency',
      label: 'Concurrent tasks',
      help: 'Independent implementation workspaces · 1–8',
      min: 1,
      max: 8
    },
    {
      key: 'cycle_interval_seconds',
      label: 'Cycle interval (seconds)',
      help: 'Time to wait between completed cycles',
      min: 30
    },
    {
      key: 'max_tasks_per_cycle',
      label: 'Tasks per cycle',
      help: 'Maximum accepted improvements · 1–20',
      min: 1,
      max: 20
    },
    {
      key: 'maintenance_every_cycles',
      label: 'Maintenance cadence',
      help: 'Prioritize maintenance every N cycles',
      min: 1
    },
    {
      key: 'large_pr_lines',
      label: 'Large PR threshold',
      help: 'Changed lines that trigger maintenance focus',
      min: 1
    },
    {
      key: 'long_lived_pr_days',
      label: 'Long-lived PR (days)',
      help: 'PR age that triggers maintenance focus',
      min: 1
    },
    {
      key: 'max_repair_rounds',
      label: 'Repair rounds',
      help: 'Unresolved work is blocked at this limit',
      min: 1,
      max: 20
    },
    {
      key: 'max_no_progress_rounds',
      label: 'No-progress rounds',
      help: 'Stop repeated repairs without code changes',
      min: 1
    },
    {
      key: 'max_retries',
      label: 'Operator retries',
      help: 'Maximum retries for each blocked task',
      min: 0,
      max: 10
    },
    {
      key: 'session_timeout_seconds',
      label: 'Session timeout (seconds)',
      help: 'Maximum duration of an agent turn',
      min: 10
    },
    {
      key: 'task_timeout_seconds',
      label: 'Task timeout (seconds)',
      help: 'Total limit for execution, review and delivery',
      min: 10
    },
    {
      key: 'command_timeout_seconds',
      label: 'Command timeout (seconds)',
      help: 'Maximum time for Git and verification commands',
      min: 1
    },
    {
      key: 'max_sessions_per_day',
      label: 'Daily session budget',
      help: 'Hard admission limit, resets at UTC midnight',
      min: 1
    },
    {
      key: 'max_workspace_bytes',
      label: 'Workspace budget (bytes)',
      help: 'Block new sessions when storage reaches this limit',
      min: 1000000
    },
    {
      key: 'retain_completed_days',
      label: 'Workspace retention (days)',
      help: 'Published work only; unresolved work is preserved',
      min: 1
    },
    {
      key: 'retain_events',
      label: 'Retained activity events',
      help: 'Most recent events to keep · 100–100,000',
      min: 100,
      max: 100000
    }
  ];
  async function load() {
    if (loading || busy || dirty) return;
    loading = true;
    loadError = '';
    try {
      const saved = await api<Config>('/config');
      // The operator may have started typing while this refresh was in flight.
      if (!dirty) acceptSaved(saved);
    } catch (e) {
      loadError = (e as Error).message;
    } finally {
      loading = false;
    }
  }
  function acceptSaved(saved: Config) {
    const serialized = JSON.stringify(saved);
    if (!savedConfig || configIdentity(saved) !== configIdentity(savedConfig)) {
      error = '';
      message = '';
      // A connection check only ever covers the exact saved configuration it ran against.
      preflight = null;
    }
    config = saved;
    baseline = serialized;
    commands = saved.verification_commands.join('\n');
    baselineCommands = commands;
  }
  function discard() {
    if (!dirty || busy) return;
    config = JSON.parse(baseline);
    commands = baselineCommands;
    presetResetKey++;
    error = '';
    message = 'Changes discarded. Saved configuration restored.';
  }
  async function save() {
    if (!config || !editable || busy || loading || !dirty) return;
    pending = 'save';
    error = '';
    message = '';
    try {
      const saved = {
        ...config,
        verification_commands: commands
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean)
      };
      await api('/config', 'PUT', saved);
      acceptSaved(saved);
      message = 'Configuration saved.';
      onsaved();
    } catch (e) {
      error = (e as Error).message;
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
    message = '';
    try {
      const models = await api<Model[]>('/model-catalog', 'POST', { backend, binary });
      catalogs[backend] = { binary, models, loaded: true };
      message = `${models.filter((model) => model.available).length} ${backend === 'codex' ? 'Codex' : 'OpenCode'} models available. Routes are never silently substituted.`;
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
    message = '';
    const at = new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    try {
      const result = await api<{ message: string; checked_config: Config }>(
        `/doctor?mode=${mode}`,
        'POST'
      );
      message = result.message;
      preflight = {
        mode,
        ok: true,
        detail: result.message,
        baseline: configIdentity(result.checked_config),
        at
      };
    } catch (e) {
      error = (e as Error).message;
      preflight =
        e instanceof ApiError && e.checkedConfig
          ? { mode, ok: false, detail: error, baseline: configIdentity(e.checkedConfig), at }
          : null;
    } finally {
      pending = '';
    }
  }
  function numberValue(key: keyof Config, value: string) {
    if (config) (config as unknown as Record<string, unknown>)[key] = Number(value);
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
  <div class="actions" aria-describedby="connection-check-help">
    <button
      id="check-connection"
      class="button"
      onclick={() => doctor('execution')}
      disabled={!config || busy || loading || dirty}
      ><Icon name="shield" size={16} />{pending === 'execution'
        ? 'Checking connection…'
        : 'Check connection'}</button
    >
    <button
      id="check-audit-connection"
      class="button"
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
    {baseline}
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
              placeholder="/srv/projects/your-project"
            /><small>Absolute path to the checkout on this host.</small></label
          >
          <label
            >GitHub repository<input
              bind:value={config.github_repo}
              placeholder="owner/repository"
            /><small>Must match the checkout’s origin remote.</small></label
          >
          <label>Default branch<input bind:value={config.default_branch} required /></label>
          <label>Owned branch prefix<input bind:value={config.branch_prefix} required /></label>
          <label class="full"
            >Verification commands<textarea
              id="verification-commands"
              bind:value={commands}
              rows="3"
              placeholder={'npm test\nnpm run build'}
            ></textarea><small
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
          <label>Codex executable<input bind:value={config.codex_binary} /></label>
          <label
            >OpenCode executable<input
              aria-label="OpenCode executable"
              aria-describedby="opencode-executable-help"
              bind:value={config.opencode_binary}
            /><small id="opencode-executable-help"
              >Uses the service user's configured providers and login.</small
            ></label
          >
        </div>
        <div class="catalog-actions">
          <button
            id="load-codex-models"
            type="button"
            class="button small"
            onclick={() => catalog('codex')}
            disabled={loading}
            >{pending === 'catalog-codex' ? 'Loading Codex models…' : 'Load Codex models'}</button
          >
          <button
            id="load-opencode-models"
            type="button"
            class="button small"
            onclick={() => catalog('opencode')}
            disabled={loading}
            >{pending === 'catalog-opencode'
              ? 'Loading OpenCode models…'
              : 'Load OpenCode models'}</button
          >
        </div>
        {#key presetResetKey}
          <AstraRehearsal bind:config catalog={catalogs.codex} {editable} {busy} />
        {/key}
        {#each Object.keys(config.roles) as role}
          <RouteEditor
            name={names[role]}
            anchor={'route-' + role}
            bind:route={config.roles[role]}
            catalog={routeCatalog(config.roles[role])}
          />
        {/each}
        {#each Object.keys(config.tiers) as tier}
          <RouteEditor
            name={tier + ' execution'}
            bind:route={config.tiers[tier]}
            catalog={routeCatalog(config.tiers[tier])}
          />
        {/each}
        <RouteEditor
          name="Repair"
          bind:route={config.repair_route}
          catalog={routeCatalog(config.repair_route)}
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
        <div class="category-options">
          {#each categories as category}<label class="checkbox"
              ><input type="checkbox" value={category} bind:group={config.categories} /><span
                >{category.replace('-', ' & ')}</span
              ></label
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
        <div class="form-grid">
          {#each ['codex', 'opencode'] as backend}
            <label
              >{backend === 'codex' ? 'Codex' : 'OpenCode'} storage measurement path (optional)
              <input
                value={config.runner_storage_paths[backend] ?? ''}
                placeholder="Absolute path to runner storage"
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
          {#each limits as limit}<label
              >{limit.label}<input
                type="number"
                min={limit.min}
                max={limit.max}
                step="1"
                value={config[limit.key] as number}
                oninput={(e) => numberValue(limit.key, e.currentTarget.value)}
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
      {#if error}<div class="notice error settings-feedback" role="alert">{error}</div>{/if}
      {#if message}<div class="notice success settings-feedback" role="status">{message}</div>{/if}
    </div>
  </form>
{:else if loading}<div class="empty" role="status">
    <span class="spinner"></span>
    <p>Loading configuration…</p>
  </div>{/if}

<style>
  .catalog-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
    margin: 20px 24px;
  }
  .catalog-actions button {
    scroll-margin-block: 100px;
  }
</style>
