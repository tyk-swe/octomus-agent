<!--
  Inspect run: read-only navigation over the evidence one planning cycle actually saved.

  This is not an event timeline. Every section names a saved record, and missing, stale,
  duplicate or unattributable evidence is rendered as such instead of being smoothed
  into a pass. The panel only ever issues GET requests.
-->
<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, relative, safeUrl } from './api';
  import { routeLabel } from './routes';
  import { copyMessage, copyText } from './clipboard';
  import Icon from './Icon.svelte';
  import Sha from './Sha.svelte';
  import EvidenceText from './EvidenceText.svelte';
  import EvidenceFact from './EvidenceFact.svelte';
  import PrContext from './PrContext.svelte';
  import type { Cycle, ProposalEvidence, RunEvidenceV1, TaskEvidence } from './types';
  import {
    checksVerdict,
    commandBadge,
    commandExplanation,
    decisionCounts,
    decisionTone,
    outcomeVerdict,
    planningVerdict,
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
    cycleDetail = $state<Cycle | null>(null),
    contextError = $state(''),
    error = $state(''),
    stale = $state(false),
    loading = $state(true),
    focus = $state<string | null>(null),
    taskFocus = $state<{ proposal: string; task: string } | null>(null),
    copyStatus = $state(''),
    rawOpen = $state(false);
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
  let planning = $derived(run ? planningVerdict(run.cycle) : null);
  async function load(cycle: string) {
    const current = ++generation;
    request?.abort();
    const controller = new AbortController();
    request = controller;
    try {
      const [next, detail] = await Promise.all([
        api<RunEvidenceV1>(
          `/cycles/${encodeURIComponent(cycle)}/evidence`,
          'GET',
          undefined,
          controller.signal
        ),
        api<Cycle>(`/cycles/${encodeURIComponent(cycle)}`, 'GET', undefined, controller.signal)
          .then((c) => ({ cycle: c, error: '' }))
          .catch((e: unknown) => ({
            cycle: null,
            error: e instanceof Error ? e.message : String(e)
          }))
      ]);
      // A late response for a superseded selection must never replace newer evidence.
      if (current !== generation || controller.signal.aborted || cycle !== cycleId) return;
      run = next;
      cycleDetail = detail.cycle;
      contextError = detail.error;
      error = '';
      stale = false;
      // A removed selection stays explicit; another proposal is never silently substituted.
      if (focus === null) focus = proposalId ?? next.proposals[0]?.id ?? null;
    } catch (e) {
      if (current !== generation || controller.signal.aborted || cycle !== cycleId) return;
      error = (e as Error).message;
      // A rejected session must not keep displaying the previous session's records.
      if (e instanceof ApiError && e.status === 401) {
        run = null;
        cycleDetail = null;
      }
      stale = run !== null;
    } finally {
      if (current === generation) {
        loading = false;
        request = null;
      }
    }
  }
  function retry() {
    loading = true;
    void load(cycleId);
  }
  $effect(() => {
    const cycle = cycleId;
    // Selecting another run discards the previous run rather than mixing two cycles.
    run = null;
    cycleDetail = null;
    contextError = '';
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
    copyStatus = copyMessage(label, await copyText(value));
    copyTimer = setTimeout(() => (copyStatus = ''), 4000);
  }
</script>

{#snippet tone(label: string, value: Tone)}<span class={'badge ' + value}>{label}</span>{/snippet}
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
  {#if run && planning}
    <div class="task-title">
      <div class="badge-row">
        {@render tone(planning.label, planning.tone)}
        {#if stale}<span class="badge blocked">Retained · stale</span>{/if}
      </div>
      <h2 id="run-evidence-title">
        {audit ? 'Audit' : 'Execution'} cycle #{String(run.cycle.number).padStart(3, '0')}
      </h2>
      <p>
        <code>{run.cycle.repository || 'Repository not recorded'}</code>
        <span>started {relative(run.cycle.started_at)}</span>
        {#if run.cycle.completed_at}<span class="dot-separator">·</span><span
            >planning finished {relative(run.cycle.completed_at)}</span
          >{/if}
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
      <section class="evidence-section run-summary" aria-labelledby="run-outcome-heading">
        <div class="row-between">
          <h3 id="run-outcome-heading">Run outcome</h3>
          {@render tone(planning.label, planning.tone)}
        </div>
        <p>
          {run.cycle.planning.proposal_count} proposal{run.cycle.planning.proposal_count === 1
            ? ''
            : 's'} recorded. {planning.detail} Delivered work, if any, is shown per proposal below.
        </p>
        <ul class="outcome-counts" aria-label="Proposal decisions">
          {#each decisionCounts(run.cycle.planning.decisions) as entry (entry.decision)}<li>
              <strong>{entry.count}</strong>{@render tone(entry.decision, entry.tone)}
            </li>{:else}<li class="muted">No decisions recorded</li>{/each}
        </ul>
        <dl class="fact-row">
          <div>
            <dt>Grounding revision</dt>
            <dd>
              <Sha value={run.cycle.grounding_revision} label="Grounding revision" oncopy={copy} />
            </dd>
          </div>
          <div>
            <dt>Reviewer batches saved</dt>
            <dd>{run.cycle.planning.reviewer_batches_saved} of 2</dd>
          </div>
          <div>
            <dt>Execution queue</dt>
            <dd>
              {run.cycle.planning.creates_execution_queue
                ? 'Created from accepted proposals'
                : 'Not created by this run'}
            </dd>
          </div>
          {#if run.cycle.planning.error_recorded}<div>
              <dt>Planning error</dt>
              <dd>An error is recorded for this run.</dd>
            </div>{/if}
        </dl>
        {@render gapList('Recorded gaps for this run', run.gaps)}
      </section>
      {#if contextError}
        <section class="evidence-section">
          <p class="notice">
            <Icon name="alert" size={18} /> PR context unavailable: {contextError}. Retrying
            automatically.
          </p>
        </section>
      {:else}
        <PrContext grounding={cycleDetail?.grounding ?? null} />
      {/if}
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
        <p class="muted evidence-note">
          Saved records in recorded order. This is not a replayed event timeline, and no timing is
          inferred.
        </p>
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
          <section class="evidence-section" aria-labelledby="proposal-heading">
            <div class="row-between">
              <span class="eyebrow">PROPOSAL</span>
              <span class="evidence-meta"
                ><span class="tier">{focused.tier}</span><span>{focused.category}</span><code
                  >{focused.id}</code
                ></span
              >
            </div>
            <h3 id="proposal-heading" class="proposal-title">{focused.title}</h3>
            <EvidenceText label="Problem" value={focused.problem} />
            <details class="evidence-more">
              <summary
                >Benefit, scope and grounding evidence ({focused.evidence.length} reference{focused
                  .evidence.length === 1
                  ? ''
                  : 's'})</summary
              >
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
            </details>
          </section>
          <section class="evidence-section" aria-labelledby="reviewers-heading">
            <div class="row-between">
              <h3 id="reviewers-heading">Independent reviewer verdicts</h3>
              <span class="muted small-note"
                >Recorded per reviewer slot, never inferred from the final decision</span
              >
            </div>
            <div class="reviewer-grid">
              {#each verdicts as verdict (verdict.reviewer)}
                {@const badge = verdictBadge(verdict)}
                <article class="reviewer-card">
                  <h4>
                    <span class="reviewer-slot">{reviewerSlot(verdict.reviewer)}</span>
                    <span class="reviewer-role">{reviewerLabel(verdict.reviewer)}</span>
                  </h4>
                  <div>{@render tone(badge.label, badge.tone)}</div>
                  {#if verdict.state === 'recorded'}
                    <EvidenceText label="Reviewer reason" value={verdict.reason ?? ''} />
                  {:else}
                    <p class="muted">
                      {#if verdict.state === 'duplicate'}
                        Several verdicts are recorded in this reviewer's slot. No single assessment
                        or reason was selected.
                      {:else}
                        No usable verdict is recorded in this reviewer's slot. It was not filled
                        from another reviewer's batch.
                      {/if}
                    </p>
                    {#if verdict.decision}<p>
                        <strong>Duplicated verdict.</strong>
                        {verdict.decision}
                      </p>{/if}
                  {/if}
                  {#if verdict.note}<div class="inline-note">{verdict.note}</div>{/if}
                </article>
              {:else}
                <p class="muted">This run records no reviewer slots for this proposal.</p>
              {/each}
            </div>
          </section>
          <section class="evidence-section" aria-labelledby="decision-heading">
            <div class="row-between">
              <h3 id="decision-heading">Final decision</h3>
              <span class={'badge ' + decisionTone(focused.final_decision)}
                >{focused.final_decision}</span
              >
            </div>
            <dl class="fact-row">
              <div>
                <dt>Reviewer agreement</dt>
                <dd>{@render tone(agreement.label, agreement.tone)}</dd>
              </div>
            </dl>
            <p class="muted">{agreement.detail}</p>
            <EvidenceText label="Final rationale" value={focused.final_reason} />
            {#if focused.final_decision === 'deferred' || verdicts.some((v) => v.decision === 'deferred')}
              <div class="inline-note">Deferred is not rejected.</div>
            {/if}
            {@render gapList('Recorded gaps for this proposal', focused.gaps)}
          </section>
          <section class="evidence-section" aria-labelledby="task-heading">
            <div class="row-between">
              <h3 id="task-heading">Linked task</h3>
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
                  Audit-only outcome: this run recorded a recommendation and no execution queue, so
                  no task is linked by design.
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
                <div>
                  <dt>Source revision</dt>
                  <dd>
                    <Sha value={task.revisions.source} label="Source revision" oncopy={copy} />
                  </dd>
                </div>
                <div>
                  <dt>Comparison base</dt>
                  <dd>
                    <Sha
                      value={task.revisions.comparison_base}
                      label="Comparison base"
                      oncopy={copy}
                    />
                  </dd>
                </div>
                <div>
                  <dt>Output commit</dt>
                  <dd><Sha value={task.revisions.output} label="Output commit" oncopy={copy} /></dd>
                </div>
                <div>
                  <dt>Last updated</dt>
                  <dd>{relative(task.updated_at)}</dd>
                </div>
              </dl>
              <details class="evidence-more">
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
                <button class="button primary small" onclick={() => onopentask(task.id)}
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
          </section>
          <section class="evidence-section" aria-labelledby="review-heading">
            <h3 id="review-heading">Review at the output commit</h3>
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
                <EvidenceFact label="Latest review standing" verdict={reviewVerdict(task)} />
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
                  <dl class="fact-row">
                    <div>
                      <dt>Reviewed change set</dt>
                      <dd>
                        <Sha value={review.comparison_base} label="Comparison base" oncopy={copy} />
                        <span aria-hidden="true">→</span>
                        <Sha value={review.revision} label="Reviewed revision" oncopy={copy} />
                      </dd>
                    </div>
                    <div>
                      <dt>Recorded</dt>
                      <dd>{relative(review.created_at)}</dd>
                    </div>
                  </dl>
                  {#if review.matches_output_revision === false && task.revisions.output}
                    <p class="muted">
                      This round reviewed {shortCommit(review.revision)}, but the recorded output
                      commit is {shortCommit(task.revisions.output)}. A review of another revision
                      does not cover the output.
                    </p>
                  {/if}
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
            {/if}
          </section>
          <section class="evidence-section" aria-labelledby="checks-heading">
            <h3 id="checks-heading">Configured checks</h3>
            {#if !selectedTask}
              <p class="muted">
                {linked.length === 0
                  ? 'No task is linked, so no check results are recorded for this proposal.'
                  : 'No task is selected, so no check results are shown.'}
              </p>
            {:else}
              {@const task = selectedTask}
              <dl class="detail-grid">
                <EvidenceFact label="Configured check results" verdict={checksVerdict(task)} />
              </dl>
              {#if task.required_commands.state === 'not_configured'}
                <p>
                  The task's saved execution configuration requires no verification commands, so no
                  check evidence exists. This is not a pass.
                </p>
              {:else}
                <ul class="command-list">
                  {#each task.required_commands.commands as command}
                    {@const badge = commandBadge(command.state)}
                    <li class="command-row">
                      <code class="command">{command.command}</code>
                      {@render tone(badge.label, badge.tone)}
                      <p class="command-meta">
                        {commandExplanation(command, task.revisions.output)}
                        {command.results_recorded} recorded result{command.results_recorded === 1
                          ? ''
                          : 's'}{command.latest_created_at
                          ? ` · latest ${relative(command.latest_created_at)}`
                          : ''}.
                      </p>
                    </li>
                  {/each}
                </ul>
                <p class="muted">
                  The latest recorded result decides: a newer failure invalidates an older pass, and
                  raw command output is not exported.
                </p>
              {/if}
            {/if}
          </section>
          <section class="evidence-section" aria-labelledby="pr-heading">
            <h3 id="pr-heading">Recorded pull request</h3>
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
          </section>
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
      <details class="raw-detail" bind:open={rawOpen}>
        <summary>Raw evidence JSON</summary>
        <!-- svelte-ignore a11y_no_noninteractive_tabindex (a scrollable region must be keyboard reachable) -->
        {#if rawOpen}<pre role="region" aria-label="Raw evidence JSON" tabindex="0">{JSON.stringify(
              run,
              null,
              2
            )}</pre>{/if}
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
      <p id="run-evidence-title">Loading recorded evidence…</p>
    </div>
  {:else}
    <div class="empty">
      <Icon name="alert" size={32} />
      <h3 id="run-evidence-title">Recorded evidence could not be loaded</h3>
      <p>{error || 'This run has no saved evidence to show.'}</p>
      <p class="muted">
        Nothing is shown in place of the saved records. The panel retries automatically every ten
        seconds.
      </p>
      <button class="button primary" onclick={retry}
        ><Icon name="refresh" size={16} />Try again</button
      >
    </div>
  {/if}
</dialog>
