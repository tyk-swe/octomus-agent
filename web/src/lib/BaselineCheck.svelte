<script lang="ts">
  import { onDestroy, tick, untrack } from 'svelte';
  import { api } from './api';
  import Badge from './Badge.svelte';
  import { baselineStatusLabel, type Tone } from './evidence';
  import { relative } from './format';
  import Icon from './Icon.svelte';
  import RecoveryNotice from './RecoveryNotice.svelte';
  import SandboxRun from './SandboxRun.svelte';
  import Sha from './Sha.svelte';
  import type { BaselineCheck, BaselineView } from './types';
  let {
    active,
    editable,
    controlStatePending = false,
    savedRevision,
    dirty,
    onchanged
  }: {
    active: boolean;
    editable: boolean;
    controlStatePending?: boolean;
    savedRevision: string;
    dirty: boolean;
    onchanged: () => void;
  } = $props();
  let view = $state<BaselineView | null>(null),
    error = $state(''),
    actionError = $state(''),
    actionRecovery = $state(''),
    cancellingId = $state(''),
    acknowledgedCheck = $state<BaselineCheck | null>(null),
    loading = $state(false),
    pending = $state(''),
    confirming = $state(false);
  let recoveryButton = $state<HTMLButtonElement>();
  let disposed = false;
  let generation = 0;
  let lastSaved = untrack(() => savedRevision);
  let request: AbortController | null = null;
  const check = $derived(view?.check ?? acknowledgedCheck);
  const running = $derived(check?.status === 'running');
  const canStart = $derived(
    active &&
      !!savedRevision &&
      editable &&
      !controlStatePending &&
      !dirty &&
      !pending &&
      !actionRecovery &&
      !running &&
      view?.eligible === true
  );
  const configMatches = $derived(
    view?.config_matches === false ||
      (check && savedRevision && check.config_fingerprint !== savedRevision)
      ? false
      : (view?.config_matches ?? null)
  );
  const statusTone = (status: BaselineCheck['status']): Tone =>
    status === 'passed' ? 'clean' : status === 'running' ? 'running' : 'failed';
  function stopRead() {
    generation += 1;
    request?.abort();
    request = null;
    loading = false;
  }
  async function load(force = false) {
    if (disposed || !active || pending || (request && !force)) return;
    request?.abort();
    const controller = new AbortController();
    const gen = ++generation;
    request = controller;
    loading = true;
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
        if (recoveryButton && document.activeElement === recoveryButton)
          document.getElementById('baseline-heading')?.focus();
        // Cancellation is acknowledged before the worker has necessarily stopped.
        const awaitingCancellation =
          cancellingId !== '' && next.check?.id === cancellingId && next.check.status === 'running';
        // Keep the last known running resource when navigation clears the detail view.
        acknowledgedCheck = next.check?.status === 'running' ? next.check : null;
        if (!awaitingCancellation) {
          actionRecovery = '';
          cancellingId = '';
        }
      }
    } catch (e) {
      if (gen === generation && !controller.signal.aborted) error = (e as Error).message;
    } finally {
      if (gen === generation) {
        request = null;
        loading = false;
      }
    }
  }
  $effect(() => {
    stopRead();
    confirming = false;
    if (!active) {
      view = null;
      error = '';
      return;
    }
    untrack(() => void load());
    const timer = setInterval(() => void load(), 4000);
    return () => {
      stopRead();
      clearInterval(timer);
    };
  });
  $effect(() => {
    if (savedRevision === lastSaved) return;
    lastSaved = savedRevision;
    confirming = false;
    untrack(() => {
      if (active) void load(true);
    });
  });
  $effect(() => {
    if (confirming && !canStart) confirming = false;
  });
  onDestroy(() => {
    disposed = true;
    stopRead();
  });
  async function openConfirm() {
    if (disposed || !canStart) return;
    confirming = true;
    await tick();
    document.getElementById('run-baseline-check')?.focus();
  }
  async function closeConfirm() {
    confirming = false;
    await tick();
    document.getElementById('check-baseline')?.focus();
  }
  async function start() {
    if (disposed || !canStart) return;
    confirming = false;
    pending = 'start';
    actionError = '';
    stopRead();
    let accepted = false;
    try {
      const next = await api<BaselineCheck>('/baseline-checks', 'POST', {
        expected_revision: savedRevision
      });
      if (disposed) return;
      // Keep the returned resource available, including its cancel action, if the read fails.
      acknowledgedCheck = next;
      view = null;
      error = '';
      actionRecovery = 'Baseline check started.';
      accepted = true;
    } catch (e) {
      if (!disposed) actionError = (e as Error).message;
    } finally {
      pending = '';
    }
    if (accepted && !disposed) {
      // Global controls must reflect the accepted write even when this panel is hidden.
      onchanged();
      if (active) await load(true);
    }
  }
  async function cancel() {
    const current = check;
    const id = current?.id;
    if (disposed || !active || !id || !running || pending || cancellingId === id) return;
    pending = 'cancel';
    actionError = '';
    stopRead();
    let accepted = false;
    try {
      await api(`/baseline-checks/${encodeURIComponent(id)}/cancel`, 'POST');
      if (disposed) return;
      acknowledgedCheck = current;
      cancellingId = id;
      error = '';
      actionRecovery = 'Baseline cancellation requested.';
      accepted = true;
    } catch (e) {
      if (!disposed) actionError = (e as Error).message;
    } finally {
      pending = '';
    }
    if (accepted && !disposed) {
      // Global controls must reflect the accepted write even when this panel is hidden.
      onchanged();
      if (active) await load(true);
    }
  }
</script>

<section class="panel settings-section" aria-labelledby="baseline-heading">
  <div class="section-heading">
    <div>
      <h2 id="baseline-heading" tabindex="-1">Clean baseline</h2>
      <p>
        Optionally verify the saved commands on a disposable clone of the remote default branch. A
        pass reflects the moment the check ran; it is not publication evidence and does not prove
        later host or remote health.
      </p>
    </div>
    <Icon name="shield" />
  </div>
  {#if actionRecovery}<RecoveryNotice
      message={actionRecovery}
      {error}
      loading={loading || pending !== ''}
      label="baseline status"
      waiting={cancellingId
        ? 'Waiting for the check to finish. Status refreshes automatically.'
        : undefined}
      icon={false}
      onretry={() => load()}
      bind:button={recoveryButton}
    />{:else if error}<div class="notice error" role="alert">{error}</div>{/if}
  {#if actionError}<div class="notice error" role="alert">
      <span>Baseline action failed. {actionError}</span>
      <button
        class="icon-button"
        aria-label="Dismiss baseline action error"
        onclick={() => {
          actionError = '';
          document.getElementById('baseline-heading')?.focus();
        }}><Icon name="close" size={16} /></button
      >
    </div>{/if}
  {#if check}
    <dl class="facts">
      <div>
        <dt>Status</dt>
        <dd>
          <Badge label={baselineStatusLabel(check.status)} tone={statusTone(check.status)} />
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
        <dt>Config revision</dt>
        <dd>
          <Sha value={check.config_fingerprint} label="checked configuration revision" />
        </dd>
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
            <Badge
              label={result.success ? 'Passed' : 'Failed'}
              tone={result.success ? 'accepted' : 'rejected'}
            />
            <code>{result.command}</code>
            <SandboxRun record={result.sandbox} />
            {#if result.output}<details>
                <summary>Output{result.output_truncated ? ' (truncated)' : ''}</summary>
                <pre>{result.output}</pre>
              </details>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  {:else if view}
    <p class="muted">No baseline check has been run.</p>
  {:else if error}
    <p class="muted">Baseline status unavailable.</p>
  {:else}
    <p class="muted">Loading baseline status…</p>
  {/if}
  {#if view && !view.eligible && !running && view.reason}<p class="muted">{view.reason}</p>{/if}
  {#if dirty && !running}<p class="muted">
      Save or discard edits before checking the baseline.
    </p>{/if}
  {#if controlStatePending && !running}<p class="muted" role="status">
      Waiting for current service activity before checking the baseline.
    </p>{/if}
  {#if confirming}
    <div
      class="notice"
      role="alertdialog"
      aria-label="Confirm baseline check"
      aria-describedby="baseline-confirm-text"
    >
      <p id="baseline-confirm-text">
        Run the saved verification commands on a disposable clone of the remote default branch?
        Commands run with the service user's permissions and may have external effects. No model
        calls or tasks will be created.
      </p>
      <div class="actions">
        <button
          id="run-baseline-check"
          class="button primary"
          onclick={start}
          disabled={pending !== ''}
          >{pending === 'start' ? 'Starting…' : 'Run baseline check'}</button
        >
        <button class="button" onclick={closeConfirm} disabled={pending !== ''}>Back</button>
      </div>
    </div>
  {:else}
    <div class="actions baseline-actions">
      <button id="check-baseline" class="button" onclick={openConfirm} disabled={!canStart}
        ><Icon name="shield" size={16} />Check clean baseline</button
      >
      {#if running}<button
          class="button"
          onclick={cancel}
          disabled={pending !== '' || cancellingId === check?.id}
          >{pending === 'cancel'
            ? 'Cancelling…'
            : cancellingId === check?.id
              ? 'Cancellation requested'
              : 'Cancel baseline check'}</button
        >{/if}
    </div>
  {/if}
</section>

<style>
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
