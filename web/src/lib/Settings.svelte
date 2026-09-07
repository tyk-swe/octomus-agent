<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from './api';
  import type { Config, Model } from './types';
  import Icon from './Icon.svelte';
  let { editable, onsaved }: { editable: boolean; onsaved: () => void } = $props();
  let config = $state<Config | null>(null),
    error = $state(''),
    message = $state(''),
    busy = $state(false),
    models = $state<Model[]>([]),
    commands = $state('');
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
      help: 'Maximum duration of a Codex turn',
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
    try {
      config = await api<Config>('/config');
      commands = config.verification_commands.join('\n');
    } catch (e) {
      error = (e as Error).message;
    }
  }
  onMount(load);
  async function save() {
    if (!config) return;
    busy = true;
    error = '';
    message = '';
    try {
      config.verification_commands = commands
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean);
      await api('/config', 'PUT', config);
      message = 'Configuration saved.';
      onsaved();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  async function catalog() {
    busy = true;
    error = '';
    try {
      models = await api<Model[]>('/models');
      message = `${models.length} models available. Routes are never silently substituted.`;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  async function doctor() {
    busy = true;
    error = '';
    message = '';
    try {
      const result = await api<{ message: string }>('/doctor', 'POST');
      message = result.message;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  function numberValue(key: keyof Config, value: string) {
    if (config) (config as unknown as Record<string, unknown>)[key] = Number(value);
  }
</script>

<div class="settings-actions">
  <p class="muted">Configure once. Keep the work moving.</p>
  <button class="button" onclick={doctor} disabled={busy}
    ><Icon name="shield" size={16} /> Check connection</button
  >
</div>
{#if !editable}<div class="notice">
    <Icon name="clock" /> Pause the service and wait for active work to finish to edit configuration.
  </div>{/if}
{#if error}<div class="notice error" role="alert">{error}</div>{/if}
{#if message}<div class="notice success" role="status">{message}</div>{/if}
{#if config}
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
          <button type="button" class="button small" onclick={catalog}>Load available models</button
          >
        </div>
        <datalist id="models"
          >{#each models as model}<option value={model.model}>{model.displayName}</option
            >{/each}</datalist
        >
        <div class="route-grid route-labels">
          <span>Agent role</span><span>Model</span><span>Reasoning effort</span>
        </div>
        {#each Object.entries(config.roles) as [role, route]}<div class="route-grid">
            <label for={'model-' + role}>{names[role]}</label><input
              id={'model-' + role}
              list="models"
              bind:value={route.model}
              placeholder="Select a model"
              aria-label={names[role] + ' model'}
            /><input
              list={'efforts-' + role}
              bind:value={route.effort}
              placeholder="Select effort"
              aria-label={names[role] + ' reasoning effort'}
            /><datalist id={'efforts-' + role}
              >{#each models.find((m) => m.model === route.model)?.supportedReasoningEfforts ?? [] as effort}<option
                  value={effort.reasoningEffort}
                ></option>{/each}</datalist
            >
          </div>{/each}
        <div class="divider"></div>
        {#each Object.entries(config.tiers) as [tier, route]}<div class="route-grid">
            <span><b class="tier">{tier}</b> Execution</span><input
              list="models"
              bind:value={route.model}
              aria-label={tier + ' model'}
            /><input bind:value={route.effort} aria-label={tier + ' effort'} />
          </div>{/each}
        <div class="inline-note">
          <Icon name="shield" size={16} /> Repair sessions always use gpt-6-astra · medium.
        </div>
        <label class="executable">Codex executable<input bind:value={config.codex_binary} /></label>
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
      <div class="save-bar">
        <span class="muted"
          >Changes apply to new work. Existing tasks retain their execution contract.</span
        ><button class="button primary" type="submit" disabled={!editable || busy}
          >{busy ? 'Working…' : 'Save configuration'}<Icon name="check" size={16} /></button
        >
      </div>
    </fieldset>
  </form>
{:else if !error}<div class="empty">
    <span class="spinner"></span>
    <p>Loading configuration…</p>
  </div>{/if}
