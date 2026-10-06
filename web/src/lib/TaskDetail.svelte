<script lang="ts">
  import { onMount } from 'svelte';
  import { api, get, ApiError, fetchEvidence } from './api';
  import Badge from './Badge.svelte';
  import { createCopyFeedback } from './copyFeedback.svelte';
  import {
    UNKNOWN_VERDICT,
    checksVerdict,
    findTaskEvidence,
    outcomeVerdict,
    prVerdict,
    reviewRoundBadge,
    reviewVerdict,
    revisionMatchLabel
  } from './evidence';
  import EvidenceFact from './EvidenceFact.svelte';
  import EvidenceText from './EvidenceText.svelte';
  import FindingCard from './FindingCard.svelte';
  import { gb, relative, safeUrl } from './format';
  import Icon from './Icon.svelte';
  import { routeLabel } from './modelRoutes';
  import PanelDialog from './PanelDialog.svelte';
  import RecoveryNotice from './RecoveryNotice.svelte';
  import ReviewChangeSet from './ReviewChangeSet.svelte';
  import SandboxRun from './SandboxRun.svelte';
  import Sha from './Sha.svelte';
  import type { Task, Event, TaskEvidence } from './types';
  let {
    id,
    onclose,
    onaction,
    onselect
  }: { id: string; onclose: () => void; onaction: () => void; onselect: (id: string) => void } =
    $props();
  let task = $state<Task | null>(null),
    error = $state(''),
    actionError = $state(''),
    actionRecovery = $state(''),
    tab = $state('Overview'),
    busy = $state(false),
    events = $state<Event[]>([]);
  let evidence = $state<TaskEvidence | null>(null),
    evidenceError = $state(''),
    evidenceStale = $state(false),
    evidenceLoading = $state(false),
    taskStale = $state(false);
  let loading = $state(false);
  let eventsLoading = $state(false),
    eventsError = $state('');
  let eventsRequest: AbortController | null = null;
  let recoveryButton = $state<HTMLButtonElement>();
  let disposed = false;
  let generation = 0;
  let request: AbortController | null = null;
  let evidenceKey = '';
  let evidenceGeneration = 0;
  let evidenceRequest: AbortController | null = null;
  let pendingEvidence: { cycleId: string; key: string } | null = null;
  const feedback = createCopyFeedback();
  const TABS = ['Overview', 'Sessions', 'Reviews', 'Verification', 'Activity'];
  const ACTION_LABELS: Record<string, string> = {
    retry: 'Retry task',
    cancel: 'Cancel task',
    supersede: 'Supersede and rediscover',
    reconcile: 'Reconcile publication',
    archive: 'Archive task',
    discard: 'Discard workspace'
  };
  const DESTRUCTIVE_ACTIONS = new Set(['discard', 'cancel']);
  const ACTION_RESULTS: Record<string, string> = {
    retry: 'Task retry requested.',
    cancel: 'Task cancellation requested.',
    supersede: 'Rediscovery requested.',
    reconcile: 'Publication reconciliation completed.',
    archive: 'Task archived.',
    discard: 'Workspace discarded.'
  };
  function moveTab(event: KeyboardEvent, from: string) {
    const index = TABS.indexOf(from);
    const next =
      event.key === 'ArrowRight'
        ? (index + 1) % TABS.length
        : event.key === 'ArrowLeft'
          ? (index - 1 + TABS.length) % TABS.length
          : event.key === 'Home'
            ? 0
            : event.key === 'End'
              ? TABS.length - 1
              : -1;
    if (next < 0) return;
    event.preventDefault();
    tab = TABS[next];
    document.getElementById('task-tab-' + tab)?.focus();
  }
  function loadPendingEvidence() {
    const pending = pendingEvidence;
    pendingEvidence = null;
    if (pending && !disposed) void loadEvidence(pending.cycleId, pending.key);
  }
  async function loadEvidence(cycleId: string, key: string, force = false) {
    if (disposed) return;
    if (evidenceKey === key && !force) return;
    if (evidenceLoading && !force) {
      // Let slow evidence make progress while coalescing changes to the latest task revision.
      pendingEvidence = { cycleId, key };
      evidenceStale = evidence !== null;
      return;
    }
    pendingEvidence = null;
    evidenceLoading = true;
    evidenceStale = evidence !== null;
    evidenceKey = key;
    const current = ++evidenceGeneration;
    evidenceRequest?.abort();
    const controller = new AbortController();
    evidenceRequest = controller;
    const task = id;
    try {
      const run = await fetchEvidence(cycleId, controller.signal);
      if (current !== evidenceGeneration || controller.signal.aborted || task !== id) return;
      evidence = findTaskEvidence(run, task);
      evidenceError = evidence ? '' : 'This task has no recorded evidence in its planning cycle.';
      evidenceStale = pendingEvidence !== null;
    } catch (e) {
      if (current !== evidenceGeneration || controller.signal.aborted || task !== id) return;
      evidenceError = (e as Error).message;
      if (e instanceof ApiError && e.status === 401) evidence = null;
      evidenceStale = evidence !== null;
    } finally {
      if (current === evidenceGeneration) {
        evidenceLoading = false;
        evidenceRequest = null;
        loadPendingEvidence();
      }
    }
  }
  async function loadEvents(force = false) {
    if (disposed || (eventsLoading && !force)) return;
    eventsRequest?.abort();
    const controller = new AbortController();
    eventsRequest = controller;
    eventsLoading = true;
    try {
      const nextEvents = await get<Event[]>(
        `/events?entity=${encodeURIComponent(id)}`,
        controller.signal
      );
      if (eventsRequest === controller && !controller.signal.aborted) {
        events = nextEvents;
        eventsError = '';
      }
    } catch (e) {
      if (eventsRequest === controller && !controller.signal.aborted)
        eventsError = (e as Error).message;
    } finally {
      if (eventsRequest === controller) {
        eventsLoading = false;
        eventsRequest = null;
      }
    }
  }
  async function load(force = false) {
    if (disposed || (loading && !force)) return;
    request?.abort();
    const current = ++generation;
    const controller = new AbortController();
    request = controller;
    loading = true;
    void loadEvents(force);
    try {
      const nextTask = await get<Task>(`/tasks/${encodeURIComponent(id)}`, controller.signal);
      if (current === generation && !controller.signal.aborted) {
        task = nextTask;
        error = '';
        taskStale = false;
        if (recoveryButton && document.activeElement === recoveryButton)
          document.getElementById('task-title')?.focus();
        actionRecovery = '';
        // Evidence has its own request identity and must not hold current task controls or polling.
        void loadEvidence(
          nextTask.cycle_id,
          `${nextTask.cycle_id}:${id}:${nextTask.updated_at}:${nextTask.status}`
        );
      }
    } catch (e) {
      if (current === generation && !controller.signal.aborted) {
        error = (e as Error).message;
        taskStale = task !== null;
      }
    } finally {
      if (current === generation) loading = false;
    }
  }
  onMount(() => {
    load();
    const timer = setInterval(() => load(), 4000);
    return () => {
      disposed = true;
      clearInterval(timer);
      feedback.dispose();
      generation++;
      request?.abort();
      eventsRequest?.abort();
      evidenceGeneration++;
      evidenceRequest?.abort();
    };
  });
  let outcome = $derived(evidence ? outcomeVerdict(evidence) : null);
  let review = $derived(reviewVerdict(evidence));
  let checks = $derived(checksVerdict(evidence));
  let delivery = $derived(prVerdict(evidence));
  let outputSha = $derived(evidence?.revisions.output ?? null);
  async function action(value: string) {
    if (disposed || busy || actionRecovery) return;
    busy = true;
    actionError = '';
    try {
      await api(`/tasks/${encodeURIComponent(id)}/${encodeURIComponent(value)}`, 'POST');
      if (disposed) return;
      // The mutation is confirmed, but its old allowed_actions no longer describe the task.
      actionRecovery = ACTION_RESULTS[value];
      error = '';
      taskStale = task !== null;
      await load(true);
      if (!disposed) onaction();
    } catch (e) {
      if (!disposed) actionError = (e as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

<PanelDialog
  eyebrow="TASK DETAILS"
  closeLabel="Close task details"
  labelledby="task-title"
  {onclose}
>
  {#if actionRecovery}<RecoveryNotice
      message={actionRecovery}
      {error}
      {loading}
      label="task details"
      onretry={() => load()}
      bind:button={recoveryButton}
    />{:else if error}<div class="notice error" role="alert">
      <Icon name="alert" size={18} /><span
        >{taskStale ? 'Retained task details · stale. ' : ''}{error}</span
      >
    </div>{/if}
  {#if actionError}<div class="notice error" role="alert">
      <Icon name="alert" size={18} /><span>Task action failed. {actionError}</span>
      <button
        class="icon-button"
        aria-label="Dismiss task action error"
        onclick={() => {
          actionError = '';
          document.getElementById('task-title')?.focus();
        }}><Icon name="close" size={16} /></button
      >
    </div>{/if}
  {#if task}
    <div class="task-title">
      <div class="badge-row"><Badge label={task.status} tone={task.status} /></div>
      <h2 id="task-title" tabindex="-1">{task.proposal.title}</h2>
      <p>
        <span class="tier">{task.proposal.tier}</span><span>{task.proposal.category}</span><span
          class="dot-separator">·</span
        ><span>Requested route: {routeLabel(task.route)}</span>
      </p>
    </div>
    <div class="tabs" role="tablist" aria-label="Task information">
      {#each TABS as name}<button
          role="tab"
          id={'task-tab-' + name}
          aria-selected={tab === name}
          aria-controls="task-tabpanel"
          tabindex={tab === name ? 0 : -1}
          class:active={tab === name}
          onclick={() => (tab = name)}
          onkeydown={(event) => moveTab(event, name)}
          >{name}{#if name === 'Reviews'}
            <span>{task.reviews.length}</span>{/if}</button
        >{/each}
    </div>
    <div
      class="detail-content"
      role="tabpanel"
      id="task-tabpanel"
      aria-labelledby={'task-tab-' + tab}
    >
      <section class="result-summary" aria-labelledby="result-heading">
        <div class="row-between">
          <h3 id="result-heading">Recorded result</h3>
          {#if evidenceStale || taskStale}<Badge label="Retained · stale" tone="blocked" />{/if}
        </div>
        {#if evidenceError}<p class="muted">
            {evidenceStale
              ? `Showing the last received evidence, which may now be out of date. ${evidenceError}`
              : `Recorded evidence is unavailable, so review and check standing stay unknown rather than assumed. ${evidenceError}`}
          </p>
          <button
            class="button small"
            disabled={evidenceLoading}
            onclick={() =>
              task &&
              loadEvidence(
                task.cycle_id,
                `${task.cycle_id}:${id}:${task.updated_at}:${task.status}`,
                true
              )}>{evidenceLoading ? 'Retrying evidence…' : 'Retry evidence'}</button
          >{/if}
        <dl class="detail-grid">
          <EvidenceFact label="Recorded outcome" verdict={outcome ?? UNKNOWN_VERDICT} />
          <div>
            <dt>Output commit</dt>
            <dd>
              {#if evidence}<Sha
                  value={outputSha}
                  label="Output commit"
                  oncopy={feedback.copy}
                />{:else}<Badge label="Unknown" tone="cancelled" />{/if}<small
                >{evidence
                  ? outputSha
                    ? 'The commit this task recorded as its output.'
                    : 'No output commit is recorded for this task yet.'
                  : 'Recorded outcome evidence has not loaded.'}</small
              >
            </dd>
          </div>
          <EvidenceFact label="Review at the output commit" verdict={review} />
          <EvidenceFact label="Configured check results" verdict={checks} />
          <div>
            <dt>Recorded PR</dt>
            <dd>
              <Badge
                label={delivery.label}
                tone={delivery.tone}
              />{#if evidence?.pull_request?.url}<a
                  href={safeUrl(evidence.pull_request.url)}
                  target="_blank"
                  rel="noreferrer">Open on GitHub<Icon name="external" size={13} /></a
                >{/if}<small>{delivery.detail}</small>
            </dd>
          </div>
        </dl>
      </section>
      {#if task.error}<div class="notice error">
          <Icon name="alert" size={18} /><span>{task.error}</span>
        </div>{/if}
      {#if task.blocked_reason}<p>
          <strong>Blocked reason:</strong>
          {task.blocked_reason.replaceAll('_', ' ')}
        </p>{/if}
      {#if task.rediscovery_requested && task.lifecycle?.archived_at}<p>
          Rediscovery withdrawn. The task was archived before an execution cycle reassessed this
          objective.
        </p>{:else if task.rediscovery_requested}<p role="status">
          Rediscovery pending. The next execution cycle will reassess this objective.
        </p>{/if}
      {#if task.rediscovery_result}<p>{task.rediscovery_result}</p>{/if}
      {#if task.superseded_by?.length}<p>
          Replacement tasks: {#each task.superseded_by as replacement}<button
              class="text-button"
              onclick={() => onselect(replacement)}>{replacement.slice(0, 8)}</button
            >{/each}
        </p>{/if}
      {#if task.lifecycle?.archived_at}<p>
          Archived · {task.lifecycle.discarded_at
            ? 'Workspace discarded'
            : 'Workspace retained until cleanup'}
        </p>{/if}
      {#if tab === 'Overview'}
        <h3>Proposal</h3>
        <p>{task.proposal.problem}</p>
        <p>{task.proposal.benefit}</p>
        <dl class="detail-grid">
          <div>
            <dt>Target</dt>
            <dd>{task.proposal.target}</dd>
          </div>
          <div>
            <dt>Branch</dt>
            <dd><code>{task.branch}</code></dd>
          </div>
          <div>
            <dt>Source revision</dt>
            <dd>
              <Sha value={task.source_revision} label="Source revision" oncopy={feedback.copy} />
            </dd>
          </div>
          <div>
            <dt>Comparison base</dt>
            <dd>
              {#if task.comparison_base}<Sha
                  value={task.comparison_base}
                  label="Comparison base"
                  oncopy={feedback.copy}
                />{:else}Assigned at execution{/if}
            </dd>
          </div>
          <div class="full">
            <dt>Workspace</dt>
            <dd><code>{task.workspace || 'Created when execution starts'}</code></dd>
          </div>
          <div>
            <dt>Dependencies</dt>
            <dd>{task.proposal.dependencies.join(', ') || 'None'}</dd>
          </div>
          <div>
            <dt>Operator retries</dt>
            <dd>{task.attempts}</dd>
          </div>
        </dl>
        <h3>Scope & evidence</h3>
        <p>{task.proposal.scope}</p>
        {#each task.proposal.evidence as item}<p class="evidence">
            <Icon name="code" size={16} />{item}
          </p>{/each}
        <details class="raw-detail">
          <summary>Execution prompt</summary>
          <!-- svelte-ignore a11y_no_noninteractive_tabindex (a scrollable region must be keyboard reachable) -->
          <pre class="prompt" role="region" aria-label="Execution prompt" tabindex="0">{task
              .proposal.prompt}</pre>
        </details>
        <details class="operating-limits">
          <summary>Effective operating limits</summary>
          <p>
            Daily admissions: {task.operating_policy.max_sessions_per_day} (original snapshot: {task
              .config.max_sessions_per_day}). Storage admission: {gb(
              task.operating_policy.max_workspace_bytes
            )} GB.
          </p>
          <p>
            This attempt allows {task.effective_attempt_policy.max_repair_rounds} repair rounds and {task
              .effective_attempt_policy.task_timeout_seconds} seconds. An explicit retry adopts current
            attempt limits.
          </p>
        </details>
      {:else if tab === 'Sessions'}
        {#each task.sessions as session}<article class="history-card">
            <div class="row-between">
              <h3>{session.role}</h3>
              <Badge label={session.status} tone={session.status} />
            </div>
            <p>Requested route: {routeLabel(session.route)}</p>
            <SandboxRun record={session.sandbox} />
            <code>{session.id}</code><small>{relative(session.started_at)}</small
            >{#if session.id === task.repair_session}<div class="inline-note">
                This repair context is reused across rounds.
              </div>{/if}{#if session.summary}<pre class="prompt">{session.summary}</pre>{/if}
          </article>{:else}<div class="empty">
            <Icon name="code" size={32} />
            <h3>No sessions yet</h3>
            <p>A separate runner session starts when this task runs.</p>
          </div>{/each}
      {:else if tab === 'Reviews'}
        <p class="muted">
          A round is clean only when it completed, recorded a summary, and found nothing. Whether it
          ran at the recorded output commit is a separate fact.
        </p>
        {#each task.reviews as round, index}
          {@const badge = reviewRoundBadge(round)}
          {@const marker = revisionMatchLabel(
            task.output_commit ? round.revision === task.output_commit : null
          )}
          <article class="history-card">
            <div class="row-between">
              <h3>Review {index + 1}</h3>
              <span class="review-badges"
                ><Badge label={badge.label} tone={badge.tone} /><Badge
                  label={marker.label}
                  tone={marker.tone}
                /></span
              >
            </div>
            {#if round.result.summary.trim()}<EvidenceText
                label="Review summary"
                value={round.result.summary}
              />{:else}<p>No review summary was recorded for this round.</p>{/if}
            <ReviewChangeSet
              comparisonBase={round.comparison_base}
              revision={round.revision}
              createdAt={round.created_at}
              oncopy={feedback.copy}
            />
            {#each round.result.findings as finding}<FindingCard {finding} />{/each}
          </article>{:else}<div class="empty">
            <Icon name="shield" size={32} />
            <h3>Review is ahead</h3>
            <p>Every round starts with a fresh reviewer and the full change set.</p>
          </div>{/each}
      {:else if tab === 'Verification'}
        {#if task.verification.length}
          <p class="muted">
            Every configured check must pass at the recorded output commit. A newer failure
            invalidates any older pass.
          </p>
          <ul class="command-list">
            {#each task.verification as verification}
              {@const marker = revisionMatchLabel(
                task.output_commit ? verification.revision === task.output_commit : null
              )}
              <li class="command-row">
                <code class="command">{verification.command}</code>
                <span class="review-badges"
                  ><Badge
                    label={verification.success ? 'Passed' : 'Failed'}
                    tone={verification.success ? 'clean' : 'failed'}
                  /><Badge label={marker.label} tone={marker.tone} /></span
                >
                <p class="command-meta">
                  Ran at <Sha
                    value={verification.revision}
                    label="Verified revision"
                    oncopy={feedback.copy}
                  /> · {relative(verification.created_at)}
                </p>
                <SandboxRun record={verification.sandbox} />
                <details class="command-output" open={!verification.success}>
                  <summary>Command output</summary>
                  <!-- svelte-ignore a11y_no_noninteractive_tabindex (a scrollable region must be keyboard reachable) -->
                  <pre
                    role="region"
                    aria-label={`Output of ${verification.command}`}
                    tabindex="0">{verification.output || 'Command completed without output.'}</pre>
                </details>
              </li>
            {/each}
          </ul>
        {:else}<div class="empty">
            <Icon name="check" size={32} />
            <h3>No verification results yet</h3>
            <p>All configured checks must pass on the reviewed revision.</p>
          </div>{/if}
      {:else}
        {#if eventsError}<p class="notice error" role="alert">
            {events.length ? 'Retained activity · stale. ' : ''}{eventsError}
          </p>{/if}
        <div class="activity-list">
          {#each events as event}<div class="activity-item">
              <span class={'activity-point ' + (event.kind === 'error' ? 'error-point' : '')}
              ></span>
              <div>
                <p>{event.message}</p>
                <small>{event.kind.replaceAll('_', ' ')} · {relative(event.at)}</small>
              </div>
            </div>{:else}{#if !eventsError}<p class="muted">
                {eventsLoading ? 'Loading activity…' : 'No activity recorded yet.'}
              </p>{/if}{/each}
        </div>{/if}
    </div>
    <div class="dialog-footer">
      <span class="muted" aria-live="polite"
        >{feedback.status || `Created ${relative(task.created_at)}`}</span
      >
      <div class="actions">
        {#if task.pr_url}<a
            class="button primary"
            href={safeUrl(task.pr_url)}
            target="_blank"
            rel="noreferrer">Open PR #{task.pr_number}<Icon name="external" size={16} /></a
          >{/if}
        {#each task.allowed_actions as value (value)}<button
            class={'button ' + (DESTRUCTIVE_ACTIONS.has(value) ? 'danger' : '')}
            disabled={busy || !!actionRecovery}
            onclick={() => action(value)}
          >
            {ACTION_LABELS[value]}
          </button>{/each}
      </div>
    </div>
  {:else if error}<div class="empty">
      <Icon name="alert" size={32} />
      <h2 id="task-title">Task unavailable</h2>
      <p>
        The selected task ({id}) could not be loaded. Its saved link does not establish that the
        task is still available.
      </p>
      <button class="button primary" onclick={() => load(true)}
        ><Icon name="refresh" size={16} />Try again</button
      >
    </div>
  {:else}<div class="empty">
      <span class="spinner"></span>
      <p id="task-title">Loading task…</p>
    </div>{/if}
</PanelDialog>
