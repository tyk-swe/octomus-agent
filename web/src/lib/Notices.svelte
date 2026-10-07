<script lang="ts">
  import { relative } from './format';
  import Icon from './Icon.svelte';
  import { sandboxVerdict } from './sandbox';
  import type { Snapshot } from './types';

  let {
    data,
    error = $bindable(),
    connectionError,
    onnavigate
  }: {
    data: Snapshot;
    error: string;
    connectionError: string;
    onnavigate: (id: string) => void;
  } = $props();
  const OPERATING_MODE_LABELS: Record<string, string> = {
    run_once: 'Run once',
    continuous: 'Continuous operation',
    paused: 'New work paused'
  };
  function operatingStatus(snapshot: Snapshot): string {
    const mode = OPERATING_MODE_LABELS[snapshot.control.mode] ?? 'New work paused';
    const phase = snapshot.control.batch?.phase === 'merging' ? ' · settling merges' : '';
    const publishing =
      snapshot.control.paused && snapshot.active_tasks > 0 ? ' · active workflows may publish' : '';
    return `${mode}${phase} · ${snapshot.active_tasks} active tasks${publishing}`;
  }
  const mergePending = (snapshot: Snapshot): number =>
    (snapshot.auto_merge.counts.waiting ?? 0) +
    (snapshot.auto_merge.counts.merging ?? 0) +
    (snapshot.auto_merge.counts.uncertain ?? 0);
</script>

{#if error}<div class="notice error" role="alert">
    <Icon name="alert" size={18} /><span>{error}</span><button
      class="icon-button"
      aria-label="Dismiss error"
      onclick={() => (error = '')}><Icon name="close" size={16} /></button
    >
  </div>{/if}
{#if connectionError}<div class="notice error" role="alert">
    <Icon name="alert" size={18} /><span
      >Connection interrupted. Displaying the last received state. {connectionError}</span
    >
  </div>{/if}
{#if data.sandbox.mode === 'off' || !data.sandbox.healthy}<div
    class="notice error sandbox-banner"
    role="status"
    aria-label="Sandbox status"
  >
    <Icon name="shield" size={18} /><span
      ><strong>{sandboxVerdict(data.sandbox).label}.</strong>
      {sandboxVerdict(data.sandbox).detail}</span
    >
  </div>{/if}
{#if data.recovery_error}<div class="notice error" role="alert" aria-label="Recovery status">
    <Icon name="alert" /><span
      ><strong>Recovery is retrying.</strong> New work waits while saved state is recovered.
      {data.recovery_error}</span
    >
  </div>{/if}
{#if data.control.error}<div class="notice error">
    <Icon name="alert" /><span>{data.control.error}</span><button
      class="text-button"
      onclick={() => onnavigate('settings')}>Inspect configuration</button
    >
  </div>{/if}
{#if data.active_cycle_mode === 'audit' && !data.recovery_error}
  <div class="notice" role="status">
    <Icon name="proposals" />Audit in progress. Execution stays paused; recommendations will not be
    queued.
  </div>
{/if}
{#if data.planning_capacity.status !== 'ready'}
  <div class="notice" role="status" aria-live="polite">
    <Icon name="alert" /><span
      >A complete planning pass requires {data.planning_capacity.required} daily admissions; {data
        .planning_capacity.remaining} remain today.
      {data.planning_capacity.status === 'limit_too_low'
        ? 'The configured daily limit cannot fund a complete planning pass; increase it in Configuration.'
        : 'The daily allowance resets at midnight UTC.'}
      {data.control.mode === 'continuous' && !data.control.paused
        ? 'Continuous operation keeps waiting and plans again when the allowance returns.'
        : 'Audit and Run once are refused until planning can be funded.'}</span
    >
  </div>
{/if}
{#if data.pr_capacity.status !== 'ready'}
  <div class="notice">
    <Icon name="alert" /><span
      ><span role="status" aria-live="polite"
        >{#if data.pr_capacity.status === 'full'}Open-PR capacity is full: {data.pr_capacity
            .owned_open} owned open PRs of {data.pr_capacity.limit}
          allowed{data.pr_capacity.reserved > 0
            ? `, plus ${data.pr_capacity.reserved} reserved deliveries`
            : ''}. New-PR work waits for an observed closure or merge; maintenance on eligible owned
          PRs continues.
        {:else if data.pr_capacity.status === 'refreshing'}{data.pr_capacity.reason ??
            'Refreshing the open-PR inventory'}. New-PR work waits until the refresh completes.
        {:else}Open-PR capacity is unavailable: {data.pr_capacity.reason ??
            'no complete inventory observed'}. New-PR work waits; unknown capacity is never treated
          as zero.{/if}</span
      >
      {#if data.pr_capacity.observed_at}Observed {relative(
          data.pr_capacity.observed_at
        )}.{/if}</span
    >
  </div>
{/if}
{#if data.delivery_mode === 'maintenance' && mergePending(data) > 0}
  <div class="notice" role="status" aria-label="Automatic merges" aria-live="polite">
    <Icon name="prs" size={18} /><span
      >{mergePending(data)} pull {mergePending(data) === 1 ? 'request waits' : 'requests wait'} on GitHub
      checks or protections for an automatic squash merge{data.auto_merge.active
        ? ' — checking now'
        : ''}.</span
    >
  </div>
{/if}
<div class="notice" role="status" aria-label="Operating mode" aria-live="polite">
  <span>{operatingStatus(data)}</span>
</div>
