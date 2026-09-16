<script lang="ts">
  import { api, relative } from './api';
  import { configIdentity } from './setup';
  import type { BaselineCheck, BaselineView, Config } from './types';
  import Sha from './Sha.svelte';
  import Icon from './Icon.svelte';
  let {
    active,
    editable,
    saved,
    dirty,
    onchanged
  }: {
    active: boolean;
    editable: boolean;
    saved: Config | null;
    dirty: boolean;
    onchanged: () => void;
  } = $props();
  let view = $state<BaselineView | null>(null),
    error = $state(''),
    pending = $state(''),
    confirming = $state(false);
  let generation = 0,
    lastSaved = '';
  let request: AbortController | null = null;
  const check = $derived(view?.check ?? null);
  const running = $derived(check?.status === 'running');
  const savedKey = $derived(saved ? configIdentity(saved) : '');
  const configMatches = $derived(
    view?.config_matches === false || (check && saved && configIdentity(check.config) !== savedKey)
      ? false
      : (view?.config_matches ?? null)
  );
  const statusLabel: Record<BaselineCheck['status'], string> = {
    running: 'Running',
    passed: 'Passed',
    failed: 'Failed',
    cancelled: 'Cancelled',
    timed_out: 'Timed out',
    interrupted: 'Interrupted'
  };
  async function load(force = false) {
    if (request && !force) return;
    request?.abort();
    const controller = new AbortController();
    const gen = ++generation;
    request = controller;
    try {
      const next = await api<BaselineView>(
        '/baseline-checks/latest',
        'GET',
        undefined,
        controller.signal
      );
      if (gen === generation && !controller.signal.aborted) {
        view = next;
        error = '';
      }
    } catch (e) {
      if (gen === generation && !controller.signal.aborted) error = (e as Error).message;
    } finally {
      if (gen === generation) request = null;
    }
  }
  $effect(() => {
    generation += 1;
    confirming = false;
    if (!active) {
      view = null;
      error = '';
      return;
    }
    void load();
    const timer = setInterval(() => void load(), 4000);
    return () => {
      generation += 1;
      request?.abort();
      request = null;
      clearInterval(timer);
    };
  });
  $effect(() => {
    if (savedKey === lastSaved) return;
    lastSaved = savedKey;
    confirming = false;
    generation += 1;
    if (active) void load(true);
  });
  $effect(() => {
    if (confirming && (!saved || !editable || dirty || pending !== '' || view?.eligible !== true))
      confirming = false;
  });
  async function start() {
    if (!saved || !editable || dirty || pending || view?.eligible !== true) return;
    confirming = false;
    pending = 'start';
    error = '';
    try {
      await api<BaselineCheck>('/baseline-checks', 'POST', { expected_config: saved });
      await load(true);
      onchanged();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      pending = '';
    }
  }
  async function cancel() {
    const id = check?.id;
    if (!id || pending) return;
    pending = 'cancel';
    error = '';
    try {
      await api(`/baseline-checks/${id}/cancel`, 'POST');
      await load(true);
      onchanged();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      pending = '';
    }
  }
</script>

<section class="panel settings-section" aria-labelledby="baseline-heading">
  <div class="section-heading">
    <div>
      <h2 id="baseline-heading">Clean baseline</h2>
      <p>
        Optionally verify the saved commands on a disposable clone of the remote default branch. A
        pass reflects the moment the check ran; it is not publication evidence and does not prove
        later host or remote health.
      </p>
    </div>
    <Icon name="shield" />
  </div>
  {#if error}<div class="notice error" role="alert">{error}</div>{/if}
  {#if check}
    <dl class="baseline-facts">
      <div>
        <dt>Status</dt>
        <dd>
          <span
            class={'badge ' +
              (check.status === 'passed'
                ? 'accepted'
                : check.status === 'running'
                  ? 'candidate'
                  : 'rejected')}>{statusLabel[check.status]}</span
          >
        </dd>
      </div>
      <div>
        <dt>Started</dt>
        <dd>{relative(check.started_at)} · {check.started_at}</dd>
      </div>
      {#if check.completed_at}<div>
          <dt>Finished</dt>
          <dd>{relative(check.completed_at)}</dd>
        </div>{/if}
      <div>
        <dt>Revision</dt>
        <dd><Sha value={check.revision} label="checked revision" /></dd>
      </div>
      <div>
        <dt>Saved configuration</dt>
        <dd>
          {configMatches === null
            ? 'Unknown'
            : configMatches
              ? 'Matches the saved configuration'
              : 'Configuration changed since this check'}
        </dd>
      </div>
      <div>
        <dt>Remote revision</dt>
        <dd>
          {view?.revision_status === 'matches_last_observation'
            ? 'Matches the latest observed remote revision'
            : view?.revision_status === 'stale'
              ? 'The observed remote default branch has moved since this check'
              : 'No recent remote observation to compare'}
        </dd>
      </div>
      {#if view?.default_observation}<div>
          <dt>Last observed</dt>
          <dd>
            {relative(view.default_observation.observed_at)} · <Sha
              value={view.default_observation.revision}
              label="observed revision"
            />
          </dd>
        </div>{/if}
    </dl>
    {#if check.error}<div class="notice error">{check.error}</div>{/if}
    {#if check.cleanup_error}<div class="notice error">
        Workspace cleanup failed: {check.cleanup_error}
      </div>{/if}
    {#if check.commands.length}
      <ul class="baseline-commands">
        {#each check.commands as result (result.created_at + result.command)}
          <li>
            <span class={'badge ' + (result.success ? 'accepted' : 'rejected')}
              >{result.success ? 'Passed' : 'Failed'}</span
            >
            <code>{result.command}</code>
            {#if result.output}<details>
                <summary>Output{result.output_truncated ? ' (truncated)' : ''}</summary>
                <pre>{result.output}</pre>
              </details>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  {:else}
    <p class="muted">No baseline check has been run.</p>
  {/if}
  {#if view && !view.eligible && !running && view.reason}<p class="muted">{view.reason}</p>{/if}
  {#if confirming}
    <div class="notice" role="alertdialog" aria-label="Confirm baseline check">
      <p>
        Run the saved verification commands on a disposable clone of the remote default branch?
        Commands run with the service user's permissions and may have external effects. No model
        calls or tasks will be created.
      </p>
      <div class="actions">
        <button class="button primary" onclick={start} disabled={pending !== ''}
          >{pending === 'start' ? 'Starting…' : 'Run baseline check'}</button
        >
        <button class="button" onclick={() => (confirming = false)} disabled={pending !== ''}
          >Back</button
        >
      </div>
    </div>
  {:else}
    <div class="actions baseline-actions">
      <button
        id="check-baseline"
        class="button"
        onclick={() => (confirming = true)}
        disabled={!saved || !editable || dirty || pending !== '' || view?.eligible !== true}
        ><Icon name="shield" size={16} />Check clean baseline</button
      >
      {#if running}<button class="button" onclick={cancel} disabled={pending !== ''}
          >{pending === 'cancel' ? 'Cancelling…' : 'Cancel baseline check'}</button
        >{/if}
    </div>
  {/if}
</section>

<style>
  .baseline-facts {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: 10px 18px;
    margin: 0 24px;
  }
  .baseline-facts dt {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
  }
  .baseline-facts dd {
    margin: 2px 0 0;
    font-size: 13px;
  }
  .baseline-commands {
    list-style: none;
    margin: 12px 24px 0;
    padding: 0;
    display: grid;
    gap: 10px;
  }
  .baseline-commands code {
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  .baseline-commands pre {
    max-height: 240px;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-size: 12px;
    background: #f5f6f2;
    padding: 10px;
    border-radius: 6px;
  }
  .baseline-actions {
    margin: 14px 24px 20px;
  }
</style>
