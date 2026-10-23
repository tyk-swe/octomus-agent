<script lang="ts">
  import Badge from './Badge.svelte';
  import DecisionCounts from './DecisionCounts.svelte';
  import { modeLabel, planningVerdict, taskOutcomeCounts } from './evidence';
  import { bytesLabel, gb, shortHash } from './format';
  import Icon from './Icon.svelte';
  import RelativeTime from './RelativeTime.svelte';
  import { sandboxVerdict } from './sandbox';
  import TaskList from './TaskList.svelte';
  import { ACTIVE_STATUSES, ERROR_EVENT_KINDS } from './types';
  import type { CycleSummary, Snapshot } from './types';

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
    onrunonce,
    onselftest
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
    onselftest: () => void;
  } = $props();
  const runnerStorageStatus: Record<string, string> = {
    unconfigured: 'not configured',
    unavailable: 'path unavailable',
    error: 'measurement error'
  };
  const mergePending = $derived(
    (data.auto_merge.counts.waiting ?? 0) +
      (data.auto_merge.counts.merging ?? 0) +
      (data.auto_merge.counts.uncertain ?? 0)
  );
  const mergeSettled = $derived(
    (data.auto_merge.counts.merged ?? 0) + (data.auto_merge.counts.closed ?? 0)
  );
  const showMerges = $derived(
    data.delivery_mode === 'maintenance' ||
      mergePending > 0 ||
      mergeSettled > 0 ||
      (data.auto_merge.counts.manual ?? 0) > 0
  );
  const sandbox = $derived(data.sandbox);
  const verdict = $derived(sandboxVerdict(sandbox));
  const broker = $derived(sandbox.broker);
  const hosts = (kind: string) => sandbox.egress?.[kind] ?? [];
</script>

{#if !data.configured}<section class="onboarding">
    <h2>Finish setting up your project</h2>
    <button class="button primary" onclick={() => onnavigate('settings')}
      >Set up your project<Icon name="arrow" size={17} /></button
    >
  </section>{/if}
{#if data.cycles.length === 0}
  <section class="first-run-guide" aria-label="Choose your first run">
    <div class="first-run-heading">
      <h2>Your first move: an audit.</h2>
    </div>
    <dl class="run-options">
      <div>
        <dt>Run an audit <span>Recommended first</span></dt>
        <dd>Discover and review proposals. No queue, no PRs.</dd>
      </div>
      <div>
        <dt>Run once</dt>
        <dd>Plan one cycle, finish accepted tasks, then pause.</dd>
      </div>
      <div>
        <dt>Start continuous</dt>
        <dd>Keep scheduling work within your limits until paused.</dd>
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
      >{data.recovery_error
        ? 'Waiting for saved state to recover'
        : data.control.paused
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
    <strong>{data.active_tasks}</strong><small>{data.counts.queued ?? 0} waiting in the queue</small
    >
  </article>
  <article class="stat">
    <div class="stat-label">Delivered tasks<Icon name="prs" size={17} /></div>
    <strong>{data.counts.published ?? 0}</strong><small>{data.merged_prs} PRs observed merged</small
    >
  </article>
  <article class="stat">
    <div class="stat-label">Needs attention<Icon name="alert" size={17} /></div>
    <strong class:warning-number={attentionCount > 0}>{attentionCount}</strong><small
      >{attentionCount ? 'Work preserved for inspection' : 'No blocked or failed tasks'}</small
    >
  </article>
  {#if showMerges}<article class="stat" id="auto-merge-stat">
      <div class="stat-label">Merge outcomes<Icon name="prs" size={17} /></div>
      <strong>{data.auto_merge.counts.merged ?? 0}</strong><small
        >{mergePending} waiting on checks or protections{data.auto_merge.active
          ? ' · checking now'
          : ''}</small
      >
    </article>{/if}
</div>
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
          {#if latestCycle}Started <RelativeTime
              value={latestCycle.started_at}
            />{#if latestCycle.completed_at}{' · planning finished '}<RelativeTime
                value={latestCycle.completed_at}
              />{/if}{:else}A thoughtful path from opportunity to pull request.{/if}
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
        <Badge label="Ready when you are" tone="queued" />
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
        <DecisionCounts decisions={cycle.decisions} />
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
          <small
            >Recent window only, not cycle totals. Published = pull request delivered; {data.delivery_mode ===
            'maintenance'
              ? 'eligible maintenance PRs may merge automatically.'
              : 'merging stays with you.'}</small
          >
        {:else}
          <p class="muted">
            {cycle.mode === 'audit'
              ? 'Audits record recommendations and queue no tasks.'
              : 'No tasks from this run appear in the recent window. Older tasks may exist.'}
          </p>
          <small>Recent window only, not cycle totals.</small>
        {/if}
      </div>
    </div>
    {#if cycle.error}<div class="notice error">
        <Icon name="alert" size={18} /><span>{cycle.error}</span>
      </div>{/if}
  {/if}
</section>
<div class="overview-columns">
  <section class="panel">
    <div class="section-heading">
      <div>
        <h2>
          Work in motion <span class="count">{data.active_tasks + (data.counts.queued ?? 0)}</span>
        </h2>
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
        <Icon name="queue" size={30} />
        <h3>No active or queued tasks</h3>
        <p>
          {data.configured
            ? 'Run a cycle to discover improvements.'
            : 'Configure your repository first.'}
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
      <h2>Recent activity</h2>
    </div>
    <div class="activity-list">
      {#each data.events.slice(0, 6) as event (event.id)}<div class="activity-item">
          <span
            class={'activity-point ' +
              (ERROR_EVENT_KINDS.includes(event.kind) ? 'error-point' : '')}
          ></span>
          <div>
            <p>{event.message}</p>
            <small
              >{ERROR_EVENT_KINDS.includes(event.kind) ? 'Error · ' : ''}<RelativeTime
                value={event.at}
              /></small
            >
          </div>
        </div>{:else}<p class="muted">No activity yet.</p>{/each}
    </div>
    <div class="budget">
      <dl class="budget-facts">
        <div>
          <dt>Sessions today</dt>
          <dd>
            {data.sessions_today} / {data.session_limit}
            <progress
              value={data.sessions_today}
              max={data.session_limit}
              aria-label="Daily session budget used"
            ></progress>
          </dd>
        </div>
        <div>
          <dt>Planning pass</dt>
          <dd>
            needs {data.planning_capacity.required} · {data.planning_capacity.remaining} remain
          </dd>
        </div>
        <div>
          <dt>Open PRs</dt>
          <dd>
            {data.pr_capacity.owned_open ?? '?'} / {data.pr_capacity.limit}{data.pr_capacity
              .reserved > 0
              ? ` + ${data.pr_capacity.reserved} reserved`
              : ''}{#if data.pr_capacity.observed_at}{' · observed '}<RelativeTime
                value={data.pr_capacity.observed_at}
              />{/if}
          </dd>
        </div>
      </dl>
      <small>Resets at midnight UTC.</small>
    </div>
  </section>
</div>
<section class="panel sandbox-panel" aria-labelledby="sandbox-heading">
  <div class="section-heading">
    <div class="row-title">
      <span class="section-icon"><Icon name="shield" /></span>
      <div>
        <h2 id="sandbox-heading">Sandbox</h2>
        <p>{verdict.detail}</p>
      </div>
    </div>
    <div class="row-title">
      <Badge label={verdict.label} tone={verdict.tone} />
      {#if sandbox.mode === 'docker'}<button
          class="button secondary small"
          disabled={busy || !sandbox.healthy}
          onclick={onselftest}><Icon name="shield" size={15} />Run self-test</button
        >{/if}
    </div>
  </div>
  {#if sandbox.mode === 'docker' && broker}
    <dl class="sandbox-facts">
      <div>
        <dt>Isolation</dt>
        <dd>
          One container per agent turn and per verification command, on {broker.runtime ||
            'the default runtime'}: non-root, no capabilities, read-only image, no route out except
          the egress gateway.
        </dd>
      </div>
      <div>
        <dt>Limits per sandbox</dt>
        <dd>
          {broker.limits.nano_cpus / 1e9} CPU · {bytesLabel(broker.limits.memory_bytes)} memory, no swap
          · {broker.limits.pids} processes · up to {broker.limits.max_sandboxes} at once ({broker.live}
          running) · {broker.limits.max_seconds.toLocaleString()} seconds maximum lifetime
        </dd>
      </div>
      <div>
        <dt>Image</dt>
        <dd><code>{broker.image}</code> · <code>{shortHash(broker.image_id)}</code></dd>
      </div>
      <div>
        <dt>Model hosts</dt>
        <dd>
          {hosts('model').join(', ') || 'None: agent sessions cannot reach a model provider.'}
        </dd>
      </div>
      <div>
        <dt>Build hosts</dt>
        <dd>{hosts('build').join(', ') || 'None: sandboxes reach no package registry.'}</dd>
      </div>
      {#if sandbox.pinned_repository}<div>
          <dt>Repository</dt>
          <dd>{sandbox.pinned_repository}, fixed by the deployment</dd>
        </div>{/if}
    </dl>
  {/if}
  {#if sandbox.mode === 'docker' && sandbox.self_test}
    {@const test = sandbox.self_test}
    <div class="sandbox-proof">
      <span class="eyebrow">LAST SELF-TEST · <RelativeTime value={test.at} /></span>
      {#if test.error !== null}<div class="notice error">
          <Icon name="alert" size={18} /><span
            >{test.error || 'The containment self-test did not complete.'}</span
          >
        </div>{:else}<ul aria-label="Containment checks">
          {#each test.checks as check (check.id)}<li class:failed={!check.passed}>
              <Icon name={check.passed ? 'check' : 'alert'} size={16} />
              <span>{check.label}</span>
              {#if !check.passed}<small>{check.detail}</small>{/if}
            </li>{/each}
        </ul>{/if}
    </div>
  {/if}
</section>
<section class="panel operating-panel">
  <details class="operating-details">
    <summary>
      <span class="section-icon"><Icon name="activity" /></span>
      <h2>Operating limits and storage</h2>
      <span class="operating-gist"
        >{#if data.storage}Application storage {gb(data.storage.application_bytes)} GB of {gb(
            data.storage_limit
          )} GB admission limit{' · measured '}<RelativeTime
            value={data.storage.measured_at}
          />{:else}Storage measurement pending.{/if}</span
      >
      <Icon name="chevron" size={15} />
    </summary>
    <div class="operating-summary muted">
      {#if data.storage}<p>
          Application storage: {gb(data.storage.application_bytes)} GB / {gb(data.storage_limit)} GB admission
          limit. Measured <RelativeTime value={data.storage.measured_at} />.
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
