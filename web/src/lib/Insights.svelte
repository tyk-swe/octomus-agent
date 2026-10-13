<script lang="ts">
  import { onDestroy } from 'svelte';
  import { client } from './api';
  import { Resource } from './resource';
  import FilterTabs from './FilterTabs.svelte';
  import Icon from './Icon.svelte';
  import type { Insights } from './types';

  let { active = false }: { active?: boolean } = $props();

  const windows = ['7 days', '30 days', '90 days'];
  let range = $state('30 days');
  const windowDays = (label: string) => Number(label.split(' ')[0]);

  let insights = $state<Insights | null>(null);
  let loading = $state(true);
  let loadError = $state('');
  const resource = new Resource<Insights, number>(
    (days, signal) => client.insights(days, signal),
    (state) => {
      insights = state.value;
      loadError = state.error;
      loading = state.loading;
    }
  );

  $effect(() => {
    if (!active) return;
    void resource.refresh(windowDays(range));
  });
  onDestroy(() => resource.dispose());

  const sum = (values: number[]) => values.reduce((a, b) => a + b, 0);
  let showAll = $state(false);
  let totalAdmissions = $derived(sum((insights?.daily ?? []).map((d) => d.admissions)));
  let decided = $derived(
    sum((insights?.categories ?? []).map((c) => c.accepted + c.rejected + c.deferred))
  );
  let acceptedTotal = $derived(sum((insights?.categories ?? []).map((c) => c.accepted)));
  let tasksCreated = $derived(insights ? sum(Object.values(insights.tasks)) : 0);
  let delivered = $derived(insights ? sum(Object.values(insights.prs)) : 0);
  let visibleCategories = $derived(
    (insights?.categories ?? []).filter(
      (c) => showAll || c.tasks > 0 || c.accepted + c.rejected + c.deferred > 0
    )
  );
  let maxDaily = $derived(Math.max(1, ...(insights?.daily ?? []).map((d) => d.admissions)));
  const categoryLabel = (category: string) => (category === 'ux-dx' ? 'UX & DX' : category);
  const rate = (accepted: number, all: number) =>
    all === 0 ? '—' : `${((accepted / all) * 100).toFixed(0)}%`;
  const per = (total: number, count: number) => (count === 0 ? '—' : (total / count).toFixed(1));
</script>

{#if active}
  <div class="insights-toolbar">
    <FilterTabs
      labels={windows}
      current={range}
      aria="Time window"
      onselect={(value) => (range = value)}
    />
    <button class="button small" onclick={() => void resource.refresh(windowDays(range), true)}
      >Refresh</button
    >
  </div>
  {#if loading && insights === null}
    <p class="muted">Loading insights…</p>
  {:else if insights === null && loadError !== ''}
    <div class="notice error" role="alert"><span>Could not load insights. {loadError}</span></div>
  {:else if insights !== null}
    {#if loadError !== ''}
      <div class="notice error" role="alert">
        <span>Could not refresh insights. Showing the last loaded results. {loadError}</span>
      </div>
    {/if}
    <div class="stats-grid">
      <article class="stat">
        <div class="stat-label">Admissions<Icon name="chart" size={17} /></div>
        <strong>{totalAdmissions}</strong><small
          >avg {(totalAdmissions / insights.days).toFixed(1)}/day</small
        >
      </article>
      <article class="stat">
        <div class="stat-label">Execution cycles<Icon name="prs" size={17} /></div>
        <strong>{insights.cycles.execution}</strong><small
          >{insights.cycles.audit} audits · {insights.cycles.failed} failed</small
        >
      </article>
      <article class="stat">
        <div class="stat-label">Accepted proposals<Icon name="check" size={17} /></div>
        <strong>{acceptedTotal}</strong><small
          >{decided === 0
            ? 'No decided proposals'
            : `${rate(acceptedTotal, decided)} of decided proposals`}</small
        >
      </article>
      <article class="stat">
        <div class="stat-label">Tasks published<Icon name="code" size={17} /></div>
        <strong>{insights.tasks.published}</strong><small>of {tasksCreated} created</small>
      </article>
      <article class="stat">
        <div class="stat-label">PRs merged<Icon name="prs" size={17} /></div>
        <strong>{insights.prs.merged}</strong><small>of {delivered} delivered</small>
      </article>
    </div>
    <section class="panel" aria-labelledby="insights-daily-heading">
      <div class="section-heading">
        <h2 id="insights-daily-heading">Admissions per day</h2>
      </div>
      <svg
        class="daily-chart"
        viewBox="0 0 320 112"
        preserveAspectRatio="none"
        role="img"
        aria-label="Admissions per day, stacked as planning, task and unattributed"
      >
        {#each insights.daily as day, i (day.day)}
          {@const slot = 320 / insights.daily.length}
          {@const scale = 100 / maxDaily}
          {@const planning = day.planning_admissions * scale}
          {@const task = day.task_admissions * scale}
          {@const other = day.unattributed_admissions * scale}
          <g>
            <title
              >{day.day}: {day.admissions} admissions ({day.planning_admissions} planning, {day.task_admissions}
              task{day.unattributed_admissions
                ? `, ${day.unattributed_admissions} unattributed`
                : ''})</title
            >
            <rect
              class="bar planning"
              x={i * slot + slot * 0.15}
              y={104 - planning}
              width={slot * 0.7}
              height={planning}
            />
            <rect
              class="bar task"
              x={i * slot + slot * 0.15}
              y={104 - planning - task}
              width={slot * 0.7}
              height={task}
            />
            <rect
              class="bar other"
              x={i * slot + slot * 0.15}
              y={104 - planning - task - other}
              width={slot * 0.7}
              height={other}
            />
          </g>
        {/each}
        <line class="axis" x1="0" y1="104" x2="320" y2="104" />
      </svg>
      <p class="legend">
        <span><i class="swatch planning"></i>Planning</span>
        <span><i class="swatch task"></i>Task</span>
        <span><i class="swatch other"></i>Unattributed</span>
      </p>
      <details>
        <summary>Daily data</summary>
        <div class="insight-table">
          <table>
            <thead>
              <tr
                ><th>Day</th><th>Admissions</th><th>Planning</th><th>Task</th><th>Unattributed</th
                ><th>Cycles started</th><th>Tasks created</th></tr
              >
            </thead>
            <tbody>
              {#each insights.daily as day (day.day)}
                <tr
                  ><td>{day.day}</td><td>{day.admissions}</td><td>{day.planning_admissions}</td><td
                    >{day.task_admissions}</td
                  ><td>{day.unattributed_admissions}</td><td>{day.cycles_started}</td><td
                    >{day.tasks_created}</td
                  ></tr
                >
              {/each}
            </tbody>
          </table>
        </div>
      </details>
    </section>
    <section class="panel" aria-labelledby="insights-categories-heading">
      <div class="section-heading">
        <h2 id="insights-categories-heading">Categories</h2>
        <label class="check"
          ><input type="checkbox" bind:checked={showAll} /><span>Show all categories</span></label
        >
      </div>
      <div class="insight-table">
        <table>
          <thead>
            <tr
              ><th>Category</th><th>Accepted</th><th>Rejected</th><th>Deferred</th><th
                >Acceptance</th
              ><th>Tasks</th><th>Published</th><th>In progress</th><th>Needs attention</th><th
                >Cancelled</th
              ></tr
            >
          </thead>
          <tbody>
            {#each visibleCategories as category (category.category)}
              {@const rowDecided = category.accepted + category.rejected + category.deferred}
              <tr
                ><td>{categoryLabel(category.category)}</td><td>{category.accepted}</td><td
                  >{category.rejected}</td
                ><td>{category.deferred}</td><td>{rate(category.accepted, rowDecided)}</td><td
                  >{category.tasks}</td
                ><td>{category.published}</td><td>{category.in_progress}</td><td
                  >{category.unresolved}</td
                ><td>{category.cancelled}</td></tr
              >
            {/each}
          </tbody>
        </table>
      </div>
    </section>
    <section class="panel" aria-labelledby="insights-tiers-heading">
      <div class="section-heading"><h2 id="insights-tiers-heading">Tiers</h2></div>
      <div class="insight-table">
        <table>
          <thead>
            <tr
              ><th>Tier</th><th>Tasks</th><th>Published</th><th>Admissions</th><th
                >Admissions per task</th
              ></tr
            >
          </thead>
          <tbody>
            {#each insights.tiers as tier (tier.tier)}
              <tr
                ><td>{tier.tier}</td><td>{tier.tasks}</td><td>{tier.published}</td><td
                  >{tier.admissions}</td
                ><td>{per(tier.admissions, tier.tasks)}</td></tr
              >
            {/each}
          </tbody>
        </table>
      </div>
    </section>
    <p class="muted">{insights.measurement}</p>
  {/if}
{/if}

<style>
  .insights-toolbar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    margin-bottom: 16px;
    flex-wrap: wrap;
  }
  .daily-chart {
    display: block;
    width: 100%;
    height: 130px;
  }
  .bar.planning {
    fill: #235a49;
  }
  .bar.task {
    fill: #3d6780;
  }
  .bar.other {
    fill: #ff9365;
  }
  .axis {
    stroke: var(--line);
    stroke-width: 1;
  }
  .legend {
    display: flex;
    gap: 16px;
    font-size: 12px;
    color: var(--ink-2);
    margin: 4px 0 10px;
  }
  .legend span {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .swatch {
    display: inline-block;
    width: 10px;
    height: 10px;
    border-radius: 2px;
  }
  .swatch.planning {
    background: #235a49;
  }
  .swatch.task {
    background: #3d6780;
  }
  .swatch.other {
    background: #ff9365;
  }
  .insight-table {
    overflow-x: auto;
  }
  .insight-table table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12.5px;
  }
  .insight-table th {
    text-align: left;
    font-weight: 600;
    color: var(--ink-2);
    padding: 6px 10px 6px 0;
    border-bottom: 1px solid var(--line);
    white-space: nowrap;
  }
  .insight-table td {
    padding: 6px 10px 6px 0;
    border-bottom: 1px solid var(--line-soft);
    white-space: nowrap;
  }
</style>
