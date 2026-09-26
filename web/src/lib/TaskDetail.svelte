<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, gb, relative, safeUrl } from './api';
  import { routeLabel } from './routes';
  import { createCopyFeedback } from './copyFeedback.svelte';
  import type { Task, Event, RunEvidenceV1, TaskEvidence } from './types';
  import Icon from './Icon.svelte';
  import PanelDialog from './PanelDialog.svelte';
  import Sha from './Sha.svelte';
  import Badge from './Badge.svelte';
  import EvidenceText from './EvidenceText.svelte';
  import EvidenceFact from './EvidenceFact.svelte';
  import FindingCard from './FindingCard.svelte';
  import ReviewChangeSet from './ReviewChangeSet.svelte';
  import {
    UNKNOWN_VERDICT,
    checksVerdict,
    findTaskEvidence,
    outcomeVerdict,
    prVerdict,
    reviewRoundBadge,
    reviewVerdict,
    roundRevisionLabel
  } from './evidence';
  let {
    id,
    onclose,
    onaction,
    onselect
  }: { id: string; onclose: () => void; onaction: () => void; onselect: (id: string) => void } =
    $props();
  let task = $state<Task | null>(null),
    error = $state(''),
    tab = $state('Overview'),
    busy = $state(false),
    events = $state<Event[]>([]);
  let evidence = $state<TaskEvidence | null>(null),
    evidenceError = $state(''),
    evidenceStale = $state(false),
    evidenceLoading = $state(false),
    taskStale = $state(false);
  let loading = false;
  let generation = 0;
  let request: AbortController | null = null;
  let evidenceKey = '';
  let evidenceGeneration = 0;
  let evidenceRequest: AbortController | null = null;
  const feedback = createCopyFeedback();
  const TABS = ['Overview', 'Sessions', 'Reviews', 'Verification', 'Activity'];
  /** Button labels for the actions the service allows, as model.Task.AllowedActions names them. */
  const ACTION_LABELS: Record<string, string> = {
    retry: 'Retry task',
    cancel: 'Cancel task',
    supersede: 'Supersede and rediscover',
    reconcile: 'Reconcile publication',
    archive: 'Archive task',
    discard: 'Discard workspace'
  };
  /** Actions that give up the task or its workspace. */
  const DESTRUCTIVE_ACTIONS = new Set(['discard', 'cancel']);
  /**
   * Tabs follow the ARIA tabs pattern: only the selected tab is in the Tab order, and the
   * arrow, Home and End keys select and focus another tab.
   */
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
  /**
   * Recorded evidence is fetched per (cycle, task, task revision) and skipped while that
   * key is unchanged. Awaiting it keeps slow evidence reads from being restarted on
   * every task poll, and preserves a coherent evidence snapshot during refresh.
   */
  async function loadEvidence(cycleId: string, key: string, force = false) {
    if (evidenceKey === key && !force) return;
    evidenceLoading = true;
    evidenceStale = evidence !== null;
    evidenceKey = key;
    const current = ++evidenceGeneration;
    evidenceRequest?.abort();
    const controller = new AbortController();
    evidenceRequest = controller;
    const task = id;
    try {
      const run = await api<RunEvidenceV1>(
        `/cycles/${encodeURIComponent(cycleId)}/evidence`,
        'GET',
        undefined,
        controller.signal
      );
      // A late response must not describe a task this panel no longer shows.
      if (current !== evidenceGeneration || controller.signal.aborted || task !== id) return;
      evidence = findTaskEvidence(run, task);
      evidenceError = evidence ? '' : 'This task has no recorded evidence in its planning cycle.';
      evidenceStale = false;
    } catch (e) {
      if (current !== evidenceGeneration || controller.signal.aborted || task !== id) return;
      evidenceError = (e as Error).message;
      // A rejected session must not keep displaying the previous session's records.
      if (e instanceof ApiError && e.status === 401) evidence = null;
      evidenceStale = evidence !== null;
      // Retain failures for this identity/revision, including cycles removed by retention.
      // Retry explicitly or fetch again when the saved task changes.
    } finally {
      if (current === evidenceGeneration) evidenceLoading = false;
    }
  }
  async function load(force = false) {
    if (loading && !force) return;
    request?.abort();
    const current = ++generation;
    const controller = new AbortController();
    request = controller;
    loading = true;
    try {
      const [nextTask, nextEvents] = await Promise.all([
        api<Task>(`/tasks/${id}`, 'GET', undefined, controller.signal),
        api<Event[]>(
          `/events?entity=${encodeURIComponent(id)}`,
          'GET',
          undefined,
          controller.signal
        )
      ]);
      if (current === generation && !controller.signal.aborted) {
        task = nextTask;
        events = nextEvents;
        error = '';
        taskStale = false;
        await loadEvidence(
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
      clearInterval(timer);
      feedback.dispose();
      generation++;
      request?.abort();
      evidenceGeneration++;
      evidenceRequest?.abort();
    };
  });
  // Recorded result, taken from the server's normalized statuses. When evidence is
  // absent these stay explicitly unknown instead of falling back to optimistic booleans.
  let outcome = $derived(evidence ? outcomeVerdict(evidence) : null);
  let review = $derived(reviewVerdict(evidence));
  let checks = $derived(checksVerdict(evidence));
  let delivery = $derived(prVerdict(evidence));
  let outputSha = $derived(evidence?.revisions.output ?? null);
  async function action(value: string) {
    busy = true;
    error = '';
    try {
      await api(`/tasks/${id}/${value}`, 'POST');
      await load(true);
      onaction();
    } catch (e) {
      error = (e as Error).message;
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
  {#if error}<div class="notice error" role="alert">
      <Icon name="alert" size={18} /><span
        >{taskStale ? 'Retained task details · stale. ' : ''}{error}</span
      >
    </div>{/if}
  {#if task}
    <div class="task-title">
      <div class="badge-row"><span class={'badge ' + task.status}>{task.status}</span></div>
      <h2 id="task-title">{task.proposal.title}</h2>
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
          {#if evidenceStale || taskStale}<span class="badge blocked">Retained · stale</span>{/if}
        </div>
        {#if evidenceError}<p class="muted">
            {evidenceStale
              ? `Showing the last received evidence, which may now be out of date. ${evidenceError}`
              : `Recorded evidence is unavailable, so review and check standing stay unknown rather than assumed. ${evidenceError}`}
          </p>
          <button
            class="button small"
            disabled={evidenceLoading}
            onclick={() => task && loadEvidence(task.cycle_id, evidenceKey, true)}
            >{evidenceLoading ? 'Retrying evidence…' : 'Retry evidence'}</button
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
                />{:else}<span class="badge cancelled">Unknown</span>{/if}<small
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
      {#if task.rediscovery_requested}<p role="status">
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
        {#each task.proposal.evidence as evidence}<p class="evidence">
            <Icon name="code" size={16} />{evidence}
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
              <span class={'badge ' + session.status}>{session.status}</span>
            </div>
            <p>Requested route: {routeLabel(session.route)}</p>
            <code>{session.id}</code><small>{relative(session.started_at)}</small
            >{#if session.id === task.repair_session}<div class="inline-note">
                This repair context is reused across rounds.
              </div>{/if}{#if session.summary}<pre class="prompt">{session.summary}</pre>{/if}
          </article>{:else}<div class="empty">
            <Icon name="code" size={32} />
            <h3>No sessions yet</h3>
            <p>A separate Codex session starts when this task runs.</p>
          </div>{/each}
      {:else if tab === 'Reviews'}
        <p class="muted">
          A round is clean only when it completed, recorded a summary, and found nothing. Whether it
          ran at the recorded output commit is a separate fact.
        </p>
        {#each task.reviews as round, index}
          {@const badge = reviewRoundBadge(round)}
          {@const marker = roundRevisionLabel(round.revision, task.output_commit)}
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
              {@const marker = roundRevisionLabel(verification.revision, task.output_commit)}
              <li class="command-row">
                <code class="command">{verification.command}</code>
                <span class="review-badges"
                  ><span class={'badge ' + (verification.success ? 'clean' : 'failed')}
                    >{verification.success ? 'Passed' : 'Failed'}</span
                  ><Badge label={marker.label} tone={marker.tone} /></span
                >
                <p class="command-meta">
                  Ran at <Sha
                    value={verification.revision}
                    label="Verified revision"
                    oncopy={feedback.copy}
                  /> · {relative(verification.created_at)}
                </p>
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
      {:else}<div class="activity-list">
          {#each events as event}<div class="activity-item">
              <span class={'activity-point ' + (event.kind === 'error' ? 'error-point' : '')}
              ></span>
              <div>
                <p>{event.message}</p>
                <small>{event.kind.replaceAll('_', ' ')} · {relative(event.at)}</small>
              </div>
            </div>{:else}<p class="muted">No activity recorded yet.</p>{/each}
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
            disabled={busy}
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
