<script lang="ts">
  import Badge from './Badge.svelte';
  import { gb, relative } from './api';
  import {
    decisionCounts as decisionEntries,
    modeLabel,
    planningVerdict,
    taskOutcomeCounts
  } from './evidence';
  import Icon, { type IconName } from './Icon.svelte';
  import TaskList from './TaskList.svelte';
  import { ACTIVE_STATUSES } from './types';
  import type { CycleSummary, Snapshot } from './types';

  /**
   * The Overview view: onboarding, the latest run, attention, work in motion, activity and
   * operating limits. It only presents the polled snapshot; every action is the page's.
   */
  let {
    data,
    latestCycle,
    attentionCount,
    canRunOnce,
    busy,
    pendingAction,
    onnavigate,
    oninspectrun,
    onopentask,
    onviewattention,
    onrunonce
  }: {
    data: Snapshot;
    latestCycle: CycleSummary | undefined;
    attentionCount: number;
    canRunOnce: boolean;
    busy: boolean;
    pendingAction: string;
    onnavigate: (id: string) => void;
    oninspectrun: () => void;
    onopentask: (id: string) => void;
    onviewattention: () => void;
    onrunonce: () => void;
  } = $props();
  /** The overview's picture of one cycle, from discovery to a delivered PR. */
  const pipeline: { name: string; icon: IconName; detail: string }[] = [
    { name: 'Ground & discover', icon: 'proposals', detail: 'Understand what matters' },
    { name: 'Challenge & refine', icon: 'shield', detail: 'Keep the worthwhile work' },
    { name: 'Build & verify', icon: 'code', detail: 'Make the complete change' },
    { name: 'Review & deliver', icon: 'prs', detail: 'Fresh eyes before every PR' }
  ];
  /** How each runner storage status without a measurement reads; unknown words stay verbatim. */
  const runnerStorageStatus: Record<string, string> = {
    unconfigured: 'not configured',
    unavailable: 'path unavailable',
    error: 'measurement error'
  };
</script>

{#if !data.configured}<section class="onboarding">
    <div>
      <span class="eyebrow">LET’S SET THINGS IN MOTION</span>
      <h2>A home for your next improvement.</h2>
      <p>
        Connect your repository, choose model routes, and set the checks every change must pass.
        Save, check the connection, then try an audit. Setup never starts work on its own.
      </p>
      <button class="button primary" onclick={() => onnavigate('settings')}
        >Set up your project<Icon name="arrow" size={17} /></button
      >
    </div>
    <div class="onboarding-art" aria-hidden="true">
      <div class="orbit orbit-one"></div>
      <div class="orbit orbit-two"></div>
      <div class="art-node node-one"><Icon name="proposals" size={21} /></div>
      <div class="art-node node-two"><Icon name="prs" size={21} /></div>
      <div class="art-node node-three"><Icon name="check" size={20} /></div>
      <img src="/favicon.svg" alt="" width="82" height="82" />
    </div>
  </section>{/if}
{#if data.cycles.length === 0}
  <section class="first-run-guide" aria-label="Choose your first run">
    <div class="first-run-heading">
      <span class="eyebrow">START WITH A LOOK AROUND</span>
      <h2>Your first move: an audit.</h2>
      <p>Read the recommendations before choosing an execution run. Each new run plans afresh.</p>
    </div>
    <dl class="run-options">
      <div>
        <dt>Run an audit <span>Recommended first</span></dt>
        <dd>Discover and review proposals. No execution queue or PRs.</dd>
      </div>
      <div>
        <dt>Run once</dt>
        <dd>Drain queued work, plan one cycle, finish accepted tasks, then pause.</dd>
      </div>
      <div>
        <dt>Start continuous</dt>
        <dd>Keep scheduling work within your configured limits until paused.</dd>
      </div>
    </dl>
  </section>
{/if}
<div class="stats-grid">
  <article class="stat">
    <div class="stat-label">System status<Icon name="activity" size={17} /></div>
    <strong class="status-value"
      ><span class={'status-dot ' + data.status}></span>{data.status}</strong
    ><small
      >{data.control.paused
        ? 'New work is paused'
        : data.cycle_active
          ? 'Discovering the next opportunity'
          : data.active_tasks
            ? 'Making steady progress'
            : 'Waiting for the next cycle'}</small
    >
  </article>
  <article class="stat">
    <div class="stat-label">Active tasks<Icon name="code" size={17} /></div>
    <strong>{data.active_tasks.toString().padStart(2, '0')}</strong><small
      >{data.counts.queued ?? 0} waiting in the queue</small
    >
  </article>
  <article class="stat">
    <div class="stat-label">Delivered tasks<Icon name="prs" size={17} /></div>
    <strong>{(data.counts.published ?? 0).toString().padStart(2, '0')}</strong><small
      >{data.merged_prs} PRs merged by maintainers</small
    >
  </article>
  <article class="stat">
    <div class="stat-label">Needs attention<Icon name="alert" size={17} /></div>
    <strong class:warning-number={attentionCount > 0}
      >{attentionCount.toString().padStart(2, '0')}</strong
    ><small>{attentionCount ? 'Work preserved for inspection' : 'No blocked or failed tasks'}</small
    >
  </article>
</div>
<section class="panel cycle-panel" aria-labelledby="latest-run-heading">
  <div class="section-heading">
    <div class="row-title">
      <span class="section-icon"><Icon name="refresh" /></span>
      <div>
        <h2 id="latest-run-heading">
          {latestCycle
            ? `Latest run · ${modeLabel(latestCycle.mode)} cycle ${latestCycle.number}`
            : 'The improvement loop'}
        </h2>
        <p>
          {latestCycle
            ? `Started ${relative(latestCycle.started_at)}${latestCycle.completed_at ? ` · planning finished ${relative(latestCycle.completed_at)}` : ''}`
            : 'A thoughtful path from opportunity to pull request.'}
        </p>
      </div>
    </div>
    <div class="row-title">
      {#if latestCycle}
        {@const planning = planningVerdict(latestCycle)}
        <Badge label={planning.label} tone={planning.tone} />
        <button class="button primary small" onclick={oninspectrun}
          ><Icon name="search" size={15} />Inspect run</button
        >
      {:else}
        <span class="badge queued">Ready when you are</span>
      {/if}
    </div>
  </div>
  {#if latestCycle}
    {@const cycle = latestCycle}
    {@const planning = planningVerdict(cycle)}
    {@const runTasks = taskOutcomeCounts(data.tasks.filter((t) => t.cycle_id === cycle.id))}
    <div class="run-outcome">
      <div>
        <span class="eyebrow">PROPOSAL DECISIONS</span>
        <ul class="outcome-counts" aria-label="Proposal decisions">
          {#each decisionEntries(cycle.decisions) as entry (entry.decision)}<li>
              <strong>{entry.count}</strong><Badge label={entry.decision} tone={entry.tone} />
            </li>{:else}<li class="muted">No decisions recorded</li>{/each}
        </ul>
        <small>{planning.detail}</small>
      </div>
      <div>
        <span class="eyebrow">RECENT TASKS FROM THIS RUN</span>
        {#if runTasks.length}
          <ul class="outcome-counts" aria-label="Recent tasks from this run">
            {#each runTasks as entry (entry.label)}<li>
                <strong>{entry.count}</strong><Badge label={entry.label} tone={entry.tone} />
              </li>{/each}
          </ul>
          <small>Published means a pull request was delivered. Merging stays with you.</small>
        {:else}
          <p class="muted">
            {cycle.mode === 'audit'
              ? 'Audits record recommendations and queue no tasks.'
              : 'No tasks from this run appear in the recent window. Older tasks may exist.'}
          </p>
        {/if}
        <small
          >Recent window only, not cycle totals. Inspect run for complete retained run evidence.</small
        >
      </div>
    </div>
    {#if cycle.error}<div class="notice error">
        <Icon name="alert" size={18} /><span>{cycle.error}</span>
      </div>{/if}
  {/if}
  <div class="pipeline">
    {#each pipeline as step, i}<div class="pipeline-step">
        <div class:highlight={data.cycle_active && i === 0} class="pipeline-icon">
          <Icon name={step.icon} size={22} />
        </div>
        <div><strong>{step.name}</strong><small>{step.detail}</small></div>
        {#if i < 3}<span class="pipeline-connector"><Icon name="chevron" size={15} /></span>{/if}
      </div>{/each}
  </div>
</section>
{#if attentionCount}<section class="panel attention-panel" aria-labelledby="attention-heading">
    <div class="section-heading">
      <div>
        <h2 id="attention-heading">
          Needs attention <span class="count">{attentionCount}</span>
        </h2>
        <p>Blocked or failed tasks keep their workspace and evidence for inspection.</p>
      </div>
      <button class="text-button" onclick={onviewattention}
        >View all unresolved work<Icon name="arrow" size={15} /></button
      >
    </div>
    <TaskList tasks={data.attention_tasks} onselect={onopentask} />
  </section>{/if}
<div class="overview-columns">
  <section class="panel">
    <div class="section-heading">
      <div>
        <h2>
          Work in motion <span class="count">{data.active_tasks + (data.counts.queued ?? 0)}</span>
        </h2>
        <p>Good changes, one focused task at a time.</p>
      </div>
      <button class="text-button" onclick={() => onnavigate('queue')}
        >View queue<Icon name="arrow" size={15} /></button
      >
    </div>
    {#if data.active_tasks > 0 || (data.counts.queued ?? 0) > 0}<TaskList
        tasks={data.tasks
          .filter((t) => ACTIVE_STATUSES.includes(t.status) || t.status === 'queued')
          .slice(0, 5)}
        onselect={onopentask}
      />{:else}<div class="empty work-empty">
        <div class="empty-illustration">
          <Icon name="queue" size={30} /><span><Icon name="check" size={12} /></span>
        </div>
        <h3>A little quiet. A lot of potential.</h3>
        <p>
          {data.configured
            ? 'Run a cycle to discover grounded improvements. Only worthwhile work makes it to the queue.'
            : 'Once your project is configured, accepted improvements will appear here.'}
        </p>
        <button
          class="text-button"
          disabled={data.configured && (busy || !canRunOnce)}
          onclick={() => (data.configured ? onrunonce() : onnavigate('settings'))}
          >{pendingAction === 'cycle'
            ? 'Starting run…'
            : data.configured
              ? 'Discover opportunities'
              : 'Configure your repository'}<Icon name="arrow" size={16} /></button
        >
      </div>{/if}
  </section>
  <section class="panel activity-panel">
    <div class="section-heading">
      <div>
        <h2>Recent activity</h2>
        <p>The latest from your workspace.</p>
      </div>
      <Icon name="activity" size={18} />
    </div>
    <div class="activity-list">
      {#each data.events.slice(0, 6) as event}<div class="activity-item">
          <span class={'activity-point ' + (event.kind === 'error' ? 'error-point' : '')}></span>
          <div>
            <p>{event.message}</p>
            <small>{event.kind === 'error' ? 'Error · ' : ''}{relative(event.at)}</small>
          </div>
        </div>{:else}<div class="activity-item">
          <span class="activity-point"></span>
          <div>
            <p>Your control room is connected.</p>
            <small>Waiting for the first cycle</small>
          </div>
        </div>
        <div class="activity-idle">
          Progress will be recorded here as the system discovers, builds, and reviews.
        </div>{/each}
    </div>
    <div class="budget">
      <div>
        <span>Today’s session budget</span><strong
          >{data.sessions_today} <span>/ {data.session_limit}</span></strong
        >
      </div>
      <progress
        value={data.sessions_today}
        max={data.session_limit}
        aria-label="Daily session budget used"
      ></progress><small
        >Resets at midnight UTC · planning pass requires {data.planning_capacity.required} admissions,
        {data.planning_capacity.remaining} remain · open-PR capacity {data.pr_capacity.owned_open ??
          '?'}/{data.pr_capacity.limit}{data.pr_capacity.reserved > 0
          ? ` + ${data.pr_capacity.reserved} reserved`
          : ''}{data.pr_capacity.observed_at
          ? ` · observed ${relative(data.pr_capacity.observed_at)}`
          : ''}</small
      >
    </div>
  </section>
</div>
<section class="panel operating-panel">
  <details class="operating-details">
    <summary>
      <span class="section-icon"><Icon name="activity" /></span>
      <h2>Operating limits and storage</h2>
      <span class="operating-gist"
        >{data.storage
          ? `Application storage ${gb(data.storage.application_bytes)} GB of ${gb(data.storage_limit)} GB admission limit · measured ${relative(data.storage.measured_at)}`
          : 'Storage measurement pending.'}</span
      >
      <Icon name="chevron" size={15} />
    </summary>
    <div class="operating-summary muted">
      {#if data.storage}<p>
          Application storage: {gb(data.storage.application_bytes)} GB / {gb(data.storage_limit)} GB admission
          limit. Measured {relative(data.storage.measured_at)}.
        </p>
        <p>
          Task workspaces: {gb(data.storage.task_bytes)} GB · Planning clones: {gb(
            data.storage.planning_bytes
          )} GB.
        </p>
        <p>
          {data.storage.runner_transcripts.message} · {data.storage.runner_transcripts.status}.
        </p>
        {#each Object.entries(data.storage.runner_transcripts.runners ?? {}) as [backend, usage]}<p>
            {backend} storage: {usage.bytes === null
              ? (runnerStorageStatus[usage.status] ?? usage.status)
              : `${gb(usage.bytes)} GB`}
          </p>{/each}{:else}<p>
          Storage measurement pending. This limit controls admission, not disk growth during active
          work.
        </p>{/if}
      <p>
        Session budget today: {data.sessions_today} of {data.session_limit} admissions. Admissions reserve
        budget before work starts; they are not completed turns or billed usage.
      </p>
    </div>
  </details>
</section>
