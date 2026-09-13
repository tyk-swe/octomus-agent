<!--
  Inspect run: read-only navigation over the evidence one planning cycle actually saved.

  This is not an event timeline. Every step names a saved record, and missing, stale,
  duplicate or unattributable evidence is rendered as such instead of being smoothed
  into a pass. The panel only ever issues GET requests.
-->
<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, relative, safeUrl } from './api';
  import { routeLabel } from './routes';
  import Icon from './Icon.svelte';
  import EvidenceText from './EvidenceText.svelte';
  import EvidenceFact from './EvidenceFact.svelte';
  import type { ProposalEvidence, RunEvidenceV1, TaskEvidence } from './types';
  import {
    checksVerdict,
    commandBadge,
    decisionTone,
    outcomeVerdict,
    prVerdict,
    reviewVerdict,
    reviewerAgreement,
    reviewerLabel,
    reviewerSlot,
    revisionMatchLabel,
    shortCommit,
    verdictBadge,
    type Tone
  } from './evidence';
  let {
    cycleId,
    proposalId = null,
    onclose,
    onopentask
  }: {
    cycleId: string;
    proposalId?: string | null;
    onclose: () => void;
    onopentask: (id: string) => void;
  } = $props();
  let dialog: HTMLDialogElement;
  let run = $state<RunEvidenceV1 | null>(null),
    error = $state(''),
    stale = $state(false),
    loading = $state(true),
    focus = $state<string | null>(null),
    taskFocus = $state<{ proposal: string; task: string } | null>(null),
    copyStatus = $state('');
  let generation = 0;
  let request: AbortController | null = null;
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  let proposals = $derived<ProposalEvidence[]>(run?.proposals ?? []);
  let focused = $derived<ProposalEvidence | null>(proposals.find((p) => p.id === focus) ?? null);
  let missingFocus = $derived(!!run && !!focus && !focused);
  let linked = $derived<TaskEvidence[]>(focused?.linked_tasks ?? []);
  // Multiple matches are preserved and never resolved for the operator: one must be
  // chosen explicitly before its review, check and delivery evidence is shown.
  let selectedTask = $derived<TaskEvidence | null>(
    taskFocus && taskFocus.proposal === focused?.id
      ? (linked.find((t) => t.id === taskFocus?.task) ?? null)
      : linked.length === 1
        ? linked[0]
        : null
  );
  let audit = $derived(run?.cycle.mode === 'audit');
  async function load(cycle: string) {
    const current = ++generation;
    request?.abort();
    const controller = new AbortController();
    request = controller;
    try {
      const next = await api<RunEvidenceV1>(
        `/cycles/${encodeURIComponent(cycle)}/evidence`,
        'GET',
        undefined,
        controller.signal
      );
      // A late response for a superseded selection must never replace newer evidence.
      if (current !== generation || controller.signal.aborted || cycle !== cycleId) return;
      run = next;
      error = '';
      stale = false;
      // A removed selection stays explicit; another proposal is never silently substituted.
      if (focus === null) focus = proposalId ?? next.proposals[0]?.id ?? null;
    } catch (e) {
      if (current !== generation || controller.signal.aborted || cycle !== cycleId) return;
      error = (e as Error).message;
      // A rejected session must not keep displaying the previous session's records.
      if (e instanceof ApiError && e.status === 401) run = null;
      stale = run !== null;
    } finally {
      if (current === generation) {
        loading = false;
        request = null;
      }
    }
  }
  $effect(() => {
    const cycle = cycleId;
    // Selecting another run discards the previous run rather than mixing two cycles.
    run = null;
    error = '';
    stale = false;
    loading = true;
    focus = proposalId;
    taskFocus = null;
    void load(cycle);
    const timer = setInterval(() => {
      // Slow responses must finish before polling can start another request.
      if (!request) void load(cycle);
    }, 10000);
    return () => {
      clearInterval(timer);
      generation++;
      request?.abort();
    };
  });
  onMount(() => {
    dialog.showModal();
    return () => {
      clearTimeout(copyTimer);
      generation++;
      request?.abort();
    };
  });
  /** Downloads exactly the allowlisted payload the service returned. */
  function download() {
    if (!run) return;
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(run, null, 2)], { type: 'application/json' })
    );
    const link = document.createElement('a');
    link.href = url;
    link.download = `octomus-run-evidence-${run.cycle.id}.json`;
    link.click();
    // Released after the browser has taken the blob, never before the click is handled.
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }
  async function copy(value: string, label: string) {
    clearTimeout(copyTimer);
    try {
      await navigator.clipboard.writeText(value);
      copyStatus = `${label} copied.`;
    } catch {
      copyStatus = `${label} could not be copied in this browser context.`;
    }
    copyTimer = setTimeout(() => (copyStatus = ''), 4000);
  }
</script>

{#snippet tone(label: string, value: Tone)}<span class={'badge ' + value}>{label}</span>{/snippet}
{#snippet commit(label: string, value: string | null)}<div class="evidence-fact">
    <dt>{label}</dt>
    <dd>
      <code>{shortCommit(value)}</code>{#if value}<button
          class="icon-button"
          aria-label={'Copy ' + label.toLowerCase()}
          onclick={() => copy(value, label)}><Icon name="code" size={14} /></button
        >{/if}
    </dd>
  </div>{/snippet}
{#snippet gapList(title: string, gaps: string[])}{#if gaps.length}<details class="evidence-gaps">
      <summary>{title} ({gaps.length})</summary>
      <ul>
        {#each gaps as gap}<li>{gap}</li>{/each}
      </ul>
    </details>{/if}{/snippet}

<dialog
  bind:this={dialog}
  class="task-dialog evidence-dialog"
  aria-labelledby="run-evidence-title"
  onkeydown={(e) => {
    if (e.key === 'Escape') onclose();
  }}
>
  <div class="dialog-top">
    <span class="eyebrow">RECORDED RUN EVIDENCE</span><button
      class="icon-button"
      aria-label="Close run evidence"
      onclick={onclose}><Icon name="close" /></button
    >
  </div>
  {#if run}
    <div class="task-title">
      <span class={'badge ' + run.cycle.status}>{run.cycle.status}</span>
      {#if stale}<span class="badge blocked">Retained · stale</span>{/if}
      <h2 id="run-evidence-title">
        {audit ? 'Audit' : 'Execution'} cycle #{String(run.cycle.number).padStart(3, '0')}
      </h2>
      <p>
        <code>{run.cycle.repository || 'Repository not recorded'}</code>
        <span>started {relative(run.cycle.started_at)}</span>
      </p>
    </div>
    <div class="detail-content" aria-label="Recorded run evidence">
      {#if error}<div class="notice error" role="alert">
          <Icon name="alert" size={18} /><span
            >{stale
              ? `Showing the last received evidence, which may now be out of date. ${error}`
              : error}</span
          >
        </div>{/if}
      <div class="notice">
        <Icon name="shield" size={18} /><span>{run.review_requirement}</span>
      </div>
      {#if audit}<div class="notice">
          <Icon name="proposals" size={18} /><span
            >Audit run. Audit acceptance is a recommendation: audit cycles never create an execution
            queue, so an accepted audit proposal has no linked task by design.</span
          >
        </div>{/if}
      <dl class="detail-grid">
        <div>
          <dt>Planning status</dt>
          <dd>
            {run.cycle.planning.status}{run.cycle.planning.planning_finished
              ? ' · planning finished'
              : ' · planning unfinished'}
          </dd>
        </div>
        <div>
          <dt>Proposals recorded</dt>
          <dd>{run.cycle.planning.proposal_count}</dd>
        </div>
        <div>
          <dt>Reviewer batches saved</dt>
          <dd>{run.cycle.planning.reviewer_batches_saved} of 2</dd>
        </div>
        <div>
          <dt>Creates an execution queue</dt>
          <dd>{run.cycle.planning.creates_execution_queue ? 'Yes' : 'No'}</dd>
        </div>
        {@render commit('Grounding revision', run.cycle.grounding_revision)}
        <div>
          <dt>Decisions</dt>
          <dd>
            {Object.entries(run.cycle.planning.decisions)
              .map(([decision, count]) => `${decision}: ${count}`)
              .join(' · ') || 'None recorded'}
          </dd>
        </div>
      </dl>
      <p class="muted">
        Planning completion is not task completion: a completed cycle records decisions, not
        delivered work.
      </p>
      {@render gapList('Recorded gaps for this run', run.gaps)}
      {#if proposals.length}
        <div class="evidence-picker">
          <label for="evidence-proposal">Proposal</label>
          <select id="evidence-proposal" bind:value={focus}>
            {#if missingFocus}<option value={focus} disabled
                >Selected proposal missing: {focus}</option
              >{/if}
            {#each proposals as p (p.id)}<option value={p.id}>{p.final_decision} · {p.title}</option
              >{/each}
          </select>
        </div>
      {/if}
      {#if missingFocus}<div class="notice" role="status">
          <Icon name="alert" size={18} /><span
            >The selected proposal ({focus}) is not recorded in this run. Choose another proposal to
            inspect its evidence.</span
          >
        </div>{/if}
      {#if focused}
        {#key focused.id}
          {@const verdicts = focused.reviewer_verdicts}
          {@const agreement = reviewerAgreement(verdicts)}
          <p class="muted">
            Navigation over saved records, in recorded order. This is not a replayed event timeline,
            and no timing is inferred.
          </p>
          <ol class="evidence-sequence">
            <li class="evidence-step">
              <div class="row-between">
                <h3>Proposal evidence</h3>
                <span class="tier">{focused.tier}</span>
              </div>
              <p class="evidence-identity">
                <code>{focused.id}</code><span>{focused.category}</span>
              </p>
              <h4>{focused.title}</h4>
              <EvidenceText label="Problem" value={focused.problem} />
              <EvidenceText label="Benefit" value={focused.benefit} />
              <EvidenceText label="Scope" value={focused.scope} />
              {#each focused.evidence as item}<EvidenceText
                  label="Grounding evidence"
                  value={item}
                />{:else}<p class="muted">No grounding evidence is recorded for this proposal.</p>
              {/each}
              <div class="proposal-target">
                <Icon name="branch" size={14} /><code>{focused.target}</code>
              </div>
            </li>
            {#each verdicts as verdict (verdict.reviewer)}
              {@const badge = verdictBadge(verdict)}
              <li class="evidence-step">
                <div class="row-between">
                  <h3>{reviewerSlot(verdict.reviewer)} — {reviewerLabel(verdict.reviewer)}</h3>
                  {@render tone(badge.label, badge.tone)}
                </div>
                {#if verdict.state === 'recorded'}
                  <p><strong>Recorded verdict.</strong> {verdict.decision}</p>
                  <EvidenceText label="Reviewer reason" value={verdict.reason ?? ''} />
                {:else}
                  <p class="muted">
                    {#if verdict.state === 'duplicate'}
                      Several verdicts are recorded in this reviewer's slot. No single assessment or
                      reason was selected.
                    {:else}
                      No usable verdict is recorded in this reviewer's slot. It was not filled from
                      another reviewer's batch.
                    {/if}
                  </p>
                  {#if verdict.decision}<p>
                      <strong>Duplicated verdict.</strong>
                      {verdict.decision}
                    </p>{/if}
                {/if}
                {#if verdict.note}<div class="inline-note">{verdict.note}</div>{/if}
              </li>
            {/each}
            <li class="evidence-step">
              <div class="row-between">
                <h3>Final decision</h3>
                <span class={'badge ' + decisionTone(focused.final_decision)}
                  >{focused.final_decision}</span
                >
              </div>
              <div class="row-between">
                <span class="muted">Reviewer agreement</span>
                {@render tone(agreement.label, agreement.tone)}
              </div>
              <p class="muted">{agreement.detail}</p>
              <EvidenceText label="Final rationale" value={focused.final_reason} />
              {#if focused.final_decision === 'deferred' || verdicts.some((v) => v.decision === 'deferred')}
                <div class="inline-note">Deferred is not rejected.</div>
              {/if}
              {@render gapList('Recorded gaps for this proposal', focused.gaps)}
            </li>
            <li class="evidence-step">
              <div class="row-between">
                <h3>Associated task</h3>
                {#if linked.length !== 1}{@render tone(
                    linked.length === 0 ? 'No linked task' : `${linked.length} matches`,
                    linked.length === 0 ? 'cancelled' : 'blocked'
                  )}{/if}
              </div>
              {#if taskFocus?.proposal === focused.id && !selectedTask}
                <p role="status">
                  The selected task ({taskFocus.task}) is no longer recorded among this proposal's
                  matches. Choose a recorded task to continue.
                </p>
              {/if}
              {#if linked.length === 0}
                <p>
                  {#if audit}
                    Audit-only outcome: this run recorded a recommendation and no execution queue,
                    so no task is linked by design.
                  {:else if focused.final_decision === 'accepted'}
                    The proposal was accepted but no task is linked in this cycle. Acceptance is not
                    execution.
                  {:else}
                    No task is linked, which matches a {focused.final_decision} decision.
                  {/if}
                </p>
              {:else}
                {#if linked.length > 1}
                  <p>
                    {linked.length} tasks match this proposal on (cycle, proposal). Every match is preserved
                    and none is selected for you. Choose one to inspect its recorded evidence.
                  </p>
                {/if}
                <div class="task-list">
                  {#each linked as task (task.id)}
                    {@const outcome = outcomeVerdict(task)}
                    <button
                      class="task-row"
                      aria-pressed={selectedTask?.id === task.id}
                      onclick={() => (taskFocus = { proposal: focused?.id ?? '', task: task.id })}
                    >
                      <span class={'task-type-icon ' + task.status}
                        ><Icon name="code" size={18} /></span
                      >
                      <span class="task-row-body"
                        ><strong>{task.id}</strong><span
                          ><code>{task.branch}</code><span class="dot-separator">·</span><span
                            >{task.attempts} retries</span
                          ></span
                        ></span
                      >
                      {@render tone(outcome.label, outcome.tone)}
                    </button>
                  {/each}
                </div>
              {/if}
              {#if selectedTask}
                {@const task = selectedTask}
                <dl class="detail-grid">
                  <EvidenceFact label="Recorded outcome" verdict={outcomeVerdict(task)} />
                  <div>
                    <dt>Branch</dt>
                    <dd><code>{task.branch}</code></dd>
                  </div>
                  {@render commit('Source revision', task.revisions.source)}
                  {@render commit('Comparison base', task.revisions.comparison_base)}
                  {@render commit('Output commit', task.revisions.output)}
                  <div>
                    <dt>Last updated</dt>
                    <dd>{relative(task.updated_at)}</dd>
                  </div>
                </dl>
                <details>
                  <summary>
                    Recorded sessions ({task.sessions.length}) — requested routes, not verified
                    runtime identity
                  </summary>
                  {#each task.sessions as session}<article class="history-card">
                      <div class="row-between">
                        <h4>{session.role}</h4>
                        <span class={'badge ' + session.status}>{session.status}</span>
                      </div>
                      <p>Requested route: {routeLabel(session.requested_route)}</p>
                      <code>{session.id}</code><small>{relative(session.started_at)}</small>
                    </article>{:else}<p class="muted">No sessions are recorded for this task.</p>
                  {/each}
                  <p class="muted">
                    Saved routes are the routes that were requested. Runtime model identity is not
                    independently reported here.
                  </p>
                </details>
                {@render gapList('Recorded gaps for this task', task.gaps)}
                <div class="actions">
                  <button class="button small" onclick={() => onopentask(task.id)}
                    >Open task details<Icon name="arrow" size={15} /></button
                  >
                </div>
                <p class="muted">
                  Supersession relationships are not included in run evidence. Task details show
                  recorded replacement tasks and rediscovery status.
                </p>
              {:else if linked.length > 1}
                <p class="muted">
                  Select one of the {linked.length} matching tasks to see its review, check and delivery
                  evidence.
                </p>
              {/if}
            </li>
            <li class="evidence-step">
              <h3>Review and check evidence</h3>
              {#if !selectedTask}
                <p class="muted">
                  {linked.length === 0
                    ? 'No task is linked, so no review or check evidence exists for this proposal.'
                    : 'No task is selected, so no review or check evidence is shown.'}
                </p>
              {:else}
                {@const task = selectedTask}
                {@const review = task.latest_review.latest}
                <dl class="detail-grid">
                  <EvidenceFact label="Review at the output commit" verdict={reviewVerdict(task)} />
                  <EvidenceFact label="Configured check results" verdict={checksVerdict(task)} />
                </dl>
                {#if review}
                  {@const marker = revisionMatchLabel(review.matches_output_revision)}
                  <article class="history-card">
                    <div class="row-between">
                      <h4>Latest recorded review round</h4>
                      {@render tone(marker.label, marker.tone)}
                    </div>
                    <p>
                      {review.completed ? 'Completed' : 'Never completed'} · {review.summary_present
                        ? 'summary recorded'
                        : 'no summary recorded'} · {review.findings.length} findings. Round {task
                        .latest_review.rounds_recorded} of {task.latest_review.rounds_recorded}.
                    </p>
                    <small
                      >Full change set · {review.comparison_base.slice(0, 12)} → {review.revision.slice(
                        0,
                        12
                      )} · {relative(review.created_at)}</small
                    >
                    <p class="muted">
                      The review summary text is not part of this export; only whether one was
                      recorded, and the structured findings.
                    </p>
                    {#each review.findings as finding}<div class="finding">
                        <span class="tier">{finding.priority}</span>
                        <h4>{finding.title}</h4>
                        <code>{finding.file}</code>
                        <EvidenceText label="Finding detail" value={finding.detail} />
                      </div>{/each}
                  </article>
                {:else}
                  <p class="muted">No review round is recorded for this task.</p>
                {/if}
                {#if task.required_commands.state === 'not_configured'}
                  <p>
                    The task's saved execution configuration requires no verification commands, so
                    no check evidence exists. This is not a pass.
                  </p>
                {:else}
                  {#each task.required_commands.commands as command}
                    {@const badge = commandBadge(command.state)}
                    <article class="history-card">
                      <div class="row-between">
                        <code>{command.command}</code>
                        {@render tone(badge.label, badge.tone)}
                      </div>
                      <small
                        >{command.results_recorded} recorded result{command.results_recorded === 1
                          ? ''
                          : 's'}{command.latest_revision
                          ? ` · latest at ${command.latest_revision.slice(0, 12)}`
                          : ''}{command.latest_created_at
                          ? ` · ${relative(command.latest_created_at)}`
                          : ''}</small
                      >
                    </article>
                  {/each}
                  <p class="muted">
                    The latest recorded result decides: a newer failure invalidates an older pass,
                    and raw command output is not exported.
                  </p>
                {/if}
              {/if}
            </li>
            <li class="evidence-step">
              <h3>Recorded pull request</h3>
              {#if !selectedTask}
                <p class="muted">
                  {linked.length === 0
                    ? 'No task is linked, so no delivery is recorded for this proposal.'
                    : 'No task is selected, so no delivery reference is shown.'}
                </p>
              {:else}
                {@const pr = selectedTask.pull_request}
                <dl class="detail-grid">
                  <EvidenceFact label="Recorded pull request" verdict={prVerdict(selectedTask)} />
                </dl>
                {#if pr && pr.url}
                  <a class="button" href={safeUrl(pr.url)} target="_blank" rel="noreferrer"
                    >Open recorded PR{pr.number === null ? '' : ` #${pr.number}`}<Icon
                      name="external"
                      size={16}
                    /></a
                  >
                {/if}
                <p class="muted">
                  A recorded pull request describes delivery, not merge. This is the reference saved
                  at publication time, not a fresh observation of the GitHub head.
                </p>
              {/if}
            </li>
          </ol>
        {/key}
      {:else if !loading && !missingFocus}
        <div class="empty">
          <Icon name="proposals" size={32} />
          <h3>No proposals recorded</h3>
          <p>This run saved no proposal evidence.</p>
        </div>
      {/if}
      <details class="evidence-gaps">
        <summary>Limitations recorded in this evidence ({run.limitations.length})</summary>
        <ul>
          {#each run.limitations as limitation}<li>{limitation}</li>{/each}
        </ul>
      </details>
      <small>Schema version {run.schema_version} · assembled {relative(run.generated_at)}</small>
    </div>
    <div class="dialog-footer">
      <span class="muted" aria-live="polite"
        >{copyStatus || 'Private operator export of saved records.'}</span
      >
      <div class="actions">
        <button class="button" onclick={() => copy(run?.cycle.id ?? '', 'Cycle identity')}
          >Copy cycle ID</button
        ><button class="button primary" onclick={download}
          >Download evidence JSON (review before sharing)<Icon name="arrow" size={16} /></button
        >
      </div>
    </div>
  {:else if loading}
    <div class="empty">
      <span class="spinner"></span>
      <p>Loading recorded evidence…</p>
    </div>
  {:else}
    <div class="empty">
      <Icon name="alert" size={32} />
      <h3>No evidence available</h3>
      <p>{error || 'This run has no saved evidence to show.'}</p>
    </div>
  {/if}
</dialog>
