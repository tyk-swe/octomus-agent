<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { api, ApiError, setToken, relative, safeUrl } from '$lib/api';
  import type { Snapshot, TaskRow } from '$lib/types';
  import Icon from '$lib/Icon.svelte';
  import Settings from '$lib/Settings.svelte';
  import TaskDetail from '$lib/TaskDetail.svelte';
  let connected = $state(false),
    accessToken = $state(''),
    data = $state<Snapshot | null>(null),
    error = $state(''),
    connectionError = $state(''),
    refreshing = false,
    busy = $state(false),
    view = $state('overview'),
    search = $state(''),
    filter = $state('all'),
    proposalFilter = $state('all'),
    proposalCycle = $state('all'),
    selected = $state<string | null>(null),
    mobileOpen = $state(false),
    lastUpdated = $state('');
  const navigation = [
    { id: 'overview', label: 'Overview', icon: 'overview' },
    { id: 'queue', label: 'Task queue', icon: 'queue' },
    { id: 'proposals', label: 'Proposals', icon: 'proposals' },
    { id: 'prs', label: 'Pull requests', icon: 'prs' },
    { id: 'settings', label: 'Configuration', icon: 'settings' }
  ];
  const activeStatuses = ['executing', 'reviewing', 'repairing', 'verifying', 'publishing'];
  let filtered = $derived(
    (data?.tasks ?? []).filter(
      (t) =>
        (filter === 'all' ||
          (filter === 'active' ? activeStatuses.includes(t.status) : t.status === filter)) &&
        `${t.title} ${t.branch} ${t.category}`.toLowerCase().includes(search.toLowerCase())
    )
  );
  let proposals = $derived(
    (data?.cycles ?? [])
      .filter((c) => proposalCycle === 'all' || c.id === proposalCycle)
      .flatMap((c) => c.proposals.map((p) => ({ ...p, cycle: c.number, mode: c.mode })))
      .filter(
        (p) =>
          (proposalFilter === 'all' || p.decision === proposalFilter) &&
          `${p.title} ${p.problem}`.toLowerCase().includes(search.toLowerCase())
      )
  );
  let published = $derived(data?.tasks.filter((t) => t.status === 'published') ?? []);
  let attention = $derived(
    data?.tasks.filter((t) => ['failed', 'blocked'].includes(t.status)) ?? []
  );
  let latestCycle = $derived(data?.cycles[0]);
  async function refresh() {
    if (!connected || refreshing) return;
    refreshing = true;
    try {
      data = await api<Snapshot>('/state');
      connectionError = '';
      lastUpdated = new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    } catch (e) {
      connectionError = (e as Error).message;
      if (e instanceof ApiError && e.status === 401) connected = false;
    } finally {
      refreshing = false;
    }
  }
  async function login() {
    busy = true;
    error = '';
    setToken(accessToken.trim());
    try {
      data = await api<Snapshot>('/state');
      connected = true;
      connectionError = '';
      accessToken = '';
      await tick();
      window.scrollTo(0, 0);
      lastUpdated = new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  onMount(() => {
    const timer = setInterval(refresh, 4000);
    return () => clearInterval(timer);
  });
  async function navigate(id: string) {
    view = id;
    search = '';
    mobileOpen = false;
    await tick();
    window.scrollTo(0, 0);
  }
  async function control(action: string) {
    busy = true;
    error = '';
    try {
      await api(`/control/${action}`, 'POST');
      await refresh();
      if (action === 'audit') {
        proposalCycle = 'all';
        proposalFilter = 'all';
        await navigate('proposals');
      }
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  function disconnect() {
    connected = false;
    data = null;
    setToken('');
    selected = null;
  }
</script>

<svelte:head
  ><title>Octomus Agent · Your project, moving forward</title><meta
    name="description"
    content="The private control room for continuous, reviewed project improvement."
  /></svelte:head
>
{#if !connected}
  <main class="login-page">
    <div class="login-brand">
      <img src="/favicon.svg" alt="" width="40" height="40" /><span
        >octomus<span class="brand-light">agent</span></span
      ><span class="version">MVP / 0.1</span>
    </div>
    <div class="login-layout">
      <section class="login-story">
        <span class="eyebrow">WHEN THE PROJECT BUILDS ITSELF</span>
        <h1>Good projects<br />keep getting<br /><em>better.</em></h1>
        <p>A little less coordination.<br />A lot more thoughtful progress.</p>
        <div class="login-pipeline">
          <span><Icon name="proposals" />Discover</span><i></i><span><Icon name="code" />Build</span
          ><i></i><span><Icon name="shield" />Review</span><i></i><span
            ><Icon name="prs" />Deliver</span
          >
        </div>
      </section>
      <section class="login-card">
        <span class="login-mark"><Icon name="key" size={24} /></span>
        <h2>Your project’s control room.</h2>
        <p>
          Connect to your Octomus service to follow the work, tune the system, and see what ships
          next.
        </p>
        <form
          onsubmit={(e) => {
            e.preventDefault();
            login();
          }}
        >
          <label for="access-token">Operator access token</label><input
            id="access-token"
            type="password"
            bind:value={accessToken}
            placeholder="Enter your access token"
            autocomplete="off"
            required
          />{#if error}<div class="notice error" role="alert">{error}</div>{/if}<button
            class="button primary"
            disabled={busy || !accessToken.trim()}
            >{busy ? 'Connecting…' : 'Open dashboard'}<Icon name="arrow" size={18} /></button
          >
        </form>
        <div class="login-security">
          <Icon name="shield" size={16} /><span
            >Private operator access. Your token stays in this tab’s memory.</span
          >
        </div>
      </section>
    </div>
    <footer class="login-footer">
      <span>Built for the long run.</span><span>Discover thoughtfully. Ship with confidence.</span>
    </footer>
  </main>
{:else if data}
  <div class="app-shell">
    <aside class:mobile-open={mobileOpen} class="sidebar">
      <a class="brand" href="#overview" onclick={() => navigate('overview')}
        ><img src="/favicon.svg" alt="" width="35" height="35" /><span
          >octomus<span class="brand-light">agent</span></span
        ></a
      >
      <div class="workspace-label">WORKSPACE</div>
      <div class="repository-switch">
        <div class="repo-avatar"><Icon name="code" size={19} /></div>
        <div>
          <strong>{data.repository.split('/').pop() || 'Your repository'}</strong><small
            >{data.repository.split('/')[0] || 'Not connected yet'}</small
          >
        </div>
        <span class="connection-dot"></span>
      </div>
      <nav aria-label="Main navigation">
        {#each navigation as item}<button
            aria-label={item.label}
            class:active={view === item.id}
            onclick={() => navigate(item.id)}
            ><Icon name={item.icon} size={19} /><span>{item.label}</span
            >{#if item.id === 'queue' && data.tasks.some((t) => t.status === 'queued')}<b
                >{data.tasks.filter((t) => t.status === 'queued').length}</b
              >{/if}</button
          >{/each}
      </nav>
      <div class="sidebar-bottom">
        <div class="autonomy-note">
          <span class="small-orbit"><Icon name="activity" size={18} /></span><strong
            >Always improving.</strong
          >
          <p>Useful changes.<br />A fresh review. Every time.</p>
        </div>
        <button class="disconnect" onclick={disconnect}
          ><Icon name="logout" size={17} /><span>Disconnect</span><span class="version">v0.1</span
          ></button
        >
      </div>
    </aside>
    <div class="main-shell">
      <header class="topbar">
        <div class="breadcrumbs">
          <button
            class="icon-button mobile-toggle"
            aria-label="Toggle navigation"
            onclick={() => (mobileOpen = !mobileOpen)}><Icon name="menu" /></button
          ><span>Workspace</span><Icon name="chevron" size={13} /><strong
            >{navigation.find((n) => n.id === view)?.label}</strong
          >
        </div>
        <div class="topbar-right">
          <span class="live-indicator" class:offline={!!connectionError}
            ><span></span>{connectionError ? 'Reconnecting' : 'Connected'}</span
          ><span class="topbar-divider"></span><span
            class="operator-avatar"
            title="Private operator">OP</span
          >
        </div>
      </header>
      <main class="content" id="main-content">
        <div class="page-heading">
          <div>
            <div class="eyebrow">YOUR PROJECT, MOVING FORWARD</div>
            <h1>
              {view === 'overview'
                ? 'The bigger picture.'
                : view === 'queue'
                  ? 'From idea to improvement.'
                  : view === 'proposals'
                    ? 'Worth doing. Before doing.'
                    : view === 'prs'
                      ? 'Progress, ready for review.'
                      : 'Make it work your way.'}
            </h1>
            <p>
              {view === 'overview'
                ? 'A clear view of what’s happening, and what’s coming next.'
                : view === 'queue'
                  ? 'Every task has a purpose, a workspace, and a path to a reviewed PR.'
                  : view === 'proposals'
                    ? 'Grounded opportunities, challenged from two independent perspectives.'
                    : view === 'prs'
                      ? 'New improvements and continued work on your existing branches.'
                      : 'Your repository, your priorities, your operating limits.'}
            </p>
          </div>
          {#if view !== 'settings'}<div class="actions">
              <button
                class="button"
                disabled={busy || !data.configured || data.active_cycle_mode === 'audit'}
                onclick={() => control(data?.control.paused ? 'resume' : 'pause')}
                ><Icon name={data.control.paused ? 'play' : 'pause'} size={16} />{data.control
                  .paused
                  ? 'Resume'
                  : 'Pause'}</button
              ><button
                class="button primary"
                disabled={busy || !data.configured || data.cycle_active}
                onclick={() => control('cycle')}
                ><Icon name="refresh" size={16} />Run a cycle</button
              >
              <button
                class="button"
                disabled={busy ||
                  !data.audit_configured ||
                  !data.control.paused ||
                  data.cycle_active ||
                  data.active_tasks > 0}
                onclick={() => control('audit')}
                ><Icon name="proposals" size={16} />Run an audit</button
              >
            </div>{/if}
        </div>
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
        {#if data.control.error}<div class="notice error">
            <Icon name="alert" /><span>{data.control.error}</span><button
              class="text-button"
              onclick={() => navigate('settings')}>Inspect configuration</button
            >
          </div>{/if}
        {#if data.active_cycle_mode === 'audit'}
          <div class="notice" role="status">
            <Icon name="proposals" />Audit in progress. Execution stays paused; recommendations will
            not be queued.
          </div>
        {/if}
        {#if view === 'overview'}
          {#if !data.configured}<section class="onboarding">
              <div>
                <span class="eyebrow">LET’S SET THINGS IN MOTION</span>
                <h2>A home for your next improvement.</h2>
                <p>
                  Connect a repository, choose your models, and define the checks that every change
                  needs to pass.
                </p>
                <button class="button primary" onclick={() => navigate('settings')}
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
                >{data.tasks.filter((t) => t.status === 'queued').length} waiting in the queue</small
              >
            </article>
            <article class="stat">
              <div class="stat-label">Published improvements<Icon name="prs" size={17} /></div>
              <strong>{published.length.toString().padStart(2, '0')}</strong><small
                >Delivered as reviewed pull requests</small
              >
            </article>
            <article class="stat">
              <div class="stat-label">Needs attention<Icon name="alert" size={17} /></div>
              <strong class:warning-number={attention.length > 0}
                >{attention.length.toString().padStart(2, '0')}</strong
              ><small
                >{attention.length
                  ? 'Work preserved for inspection'
                  : 'No blocked or failed tasks'}</small
              >
            </article>
          </div>
          <section class="panel cycle-panel">
            <div class="section-heading">
              <div class="row-title">
                <span class="section-icon"><Icon name="refresh" /></span>
                <div>
                  <h2>The improvement loop</h2>
                  <p>
                    {latestCycle
                      ? `Cycle ${String(latestCycle.number).padStart(3, '0')} · started ${relative(latestCycle.started_at)}`
                      : 'A thoughtful path from opportunity to pull request.'}
                  </p>
                </div>
              </div>
              <span class={'badge ' + (latestCycle?.status || 'queued')}
                >{latestCycle?.status || 'Ready when you are'}</span
              >
            </div>
            <div class="pipeline">
              {#each [{ name: 'Ground & discover', icon: 'proposals', detail: 'Understand what matters' }, { name: 'Challenge & refine', icon: 'shield', detail: 'Keep the worthwhile work' }, { name: 'Build & verify', icon: 'code', detail: 'Make the complete change' }, { name: 'Review & deliver', icon: 'prs', detail: 'Fresh eyes before every PR' }] as step, i}<div
                  class="pipeline-step"
                >
                  <div class:highlight={data?.cycle_active && i === 0} class="pipeline-icon">
                    <Icon name={step.icon} size={22} />
                  </div>
                  <div><strong>{step.name}</strong><small>{step.detail}</small></div>
                  {#if i < 3}<span class="pipeline-connector"
                      ><Icon name="chevron" size={15} /></span
                    >{/if}
                </div>{/each}
            </div>
            {#if latestCycle?.error}<div class="notice error">{latestCycle.error}</div>{/if}
          </section>
          <div class="overview-columns">
            <section class="panel">
              <div class="section-heading">
                <div>
                  <h2>
                    Work in motion <span class="count"
                      >{data.tasks.filter(
                        (t) => activeStatuses.includes(t.status) || t.status === 'queued'
                      ).length}</span
                    >
                  </h2>
                  <p>Good changes, one focused task at a time.</p>
                </div>
                <button class="text-button" onclick={() => navigate('queue')}
                  >View queue<Icon name="arrow" size={15} /></button
                >
              </div>
              {#if data.tasks.length}{@render taskList(
                  data.tasks
                    .filter((t) => t.status !== 'published' && t.status !== 'cancelled')
                    .slice(0, 5)
                )}{:else}<div class="empty work-empty">
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
                    onclick={() => (data?.configured ? control('cycle') : navigate('settings'))}
                    >{data.configured ? 'Discover opportunities' : 'Configure your repository'}<Icon
                      name="arrow"
                      size={16}
                    /></button
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
                    <span class={'activity-point ' + (event.kind === 'error' ? 'error-point' : '')}
                    ></span>
                    <div>
                      <p>{event.message}</p>
                      <small>{relative(event.at)}</small>
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
                ></progress><small>Resets at midnight UTC</small>
              </div>
            </section>
          </div>
        {:else if view === 'queue'}
          <section class="panel">
            <div class="list-toolbar">
              <div class="filter-tabs" aria-label="Task filters">
                {#each ['all', 'active', 'queued', 'published', 'blocked', 'cancelled'] as state}<button
                    class:active={filter === state}
                    onclick={() => (filter = state)}>{state}</button
                  >{/each}
              </div>
              {@render searchBox()}
            </div>
            {#if filtered.length}{@render taskList(filtered)}{:else}<div class="empty">
                <Icon name="queue" size={34} />
                <h3>
                  {search || filter !== 'all'
                    ? 'No matching tasks'
                    : 'The next good idea starts here.'}
                </h3>
                <p>
                  {search || filter !== 'all'
                    ? 'Try another filter or search term.'
                    : 'Accepted proposals become focused tasks with a clear outcome.'}
                </p>
              </div>{/if}
          </section>
        {:else if view === 'proposals'}
          <p class="muted">
            Audits record recommendations without queuing work. A later execution cycle plans
            afresh.
          </p>
          <div class="proposal-controls">
            <div class="cycle-picker">
              <label for="proposal-cycle">Cycle</label>
              <select id="proposal-cycle" bind:value={proposalCycle}>
                <option value="all">All recent cycles</option>
                {#each data.cycles as cycle}<option value={cycle.id}
                    >{cycle.mode === 'audit' ? 'Audit' : 'Execution'} #{cycle.number} · {cycle.status}</option
                  >{/each}
              </select>
            </div>
            <div class="decision-counts" role="group" aria-label="Decision counts">
              {#each ['accepted', 'rejected', 'deferred', 'candidate'] as decision}
                <span class={'badge ' + decision}
                  >{decision}: {data.cycles
                    .filter((c) => proposalCycle === 'all' || c.id === proposalCycle)
                    .flatMap((c) => c.proposals)
                    .filter((p) => p.decision === decision).length}</span
                >
              {/each}
            </div>
          </div>
          <section class="panel">
            <div class="list-toolbar">
              <div class="filter-tabs" aria-label="Proposal filters">
                {#each ['all', 'accepted', 'rejected', 'deferred', 'candidate'] as state}<button
                    class:active={proposalFilter === state}
                    onclick={() => (proposalFilter = state)}>{state}</button
                  >{/each}
              </div>
              {@render searchBox()}
            </div>
            <div class="proposal-list">
              {#each proposals as p}<article class="proposal-card">
                  <div class="row-between">
                    <div class="proposal-meta">
                      <span class={'badge ' + p.decision}>{p.decision}</span><span
                        >{p.mode === 'audit' ? 'Audit' : 'Execution'} #{p.cycle}</span
                      ><span class="tier">{p.tier}</span>
                    </div>
                    <span class="category">{p.category}</span>
                  </div>
                  <h2>{p.title}</h2>
                  <p>{p.problem}</p>
                  <div class="decision-reason">
                    <Icon name="shield" size={17} />
                    <p>{p.reason}</p>
                  </div>
                  <details>
                    <summary>Scope, evidence & execution prompt</summary>
                    <p>{p.benefit}</p>
                    <p>{p.scope}</p>
                    {#each p.evidence as evidence}<p class="evidence">{evidence}</p>{/each}
                    <pre class="prompt">{p.prompt}</pre>
                    <small>Dependencies: {p.dependencies.join(', ') || 'None'}</small>
                  </details>
                  <div class="proposal-target">
                    <Icon name="branch" size={14} /><code>{p.target}</code>
                  </div>
                </article>{:else}<div class="empty">
                  <Icon name="proposals" size={34} />
                  <h3>
                    {search || proposalFilter !== 'all'
                      ? 'No matching proposals'
                      : 'Better ideas start with questions.'}
                  </h3>
                  <p>
                    Discovery explores your project. Two adversarial reviewers challenge each
                    proposal before the orchestrator decides.
                  </p>
                </div>{/each}
            </div>
          </section>
        {:else if view === 'prs'}
          <div class="notice">
            <Icon name="shield" size={18} /><span
              >Octomus publishes reviewed pull requests. Merge decisions stay with you.</span
            >
          </div>
          <section class="panel">
            <div class="section-heading">
              <div>
                <h2>Open branch work</h2>
                <p>Refreshed when the repository is grounded.</p>
              </div>
              <span class="count">{data.prs.length}</span>
            </div>
            {#each data.prs as pr}<a
                class="pr-row"
                href={safeUrl(pr.url)}
                target="_blank"
                rel="noreferrer"
                ><span class="pr-icon"><Icon name="prs" /></span>
                <div>
                  <h3>{pr.title}<span class="pr-number">#{pr.number}</span></h3>
                  <p><code>{pr.branch}</code><span>→</span><code>{pr.base}</code></p>
                  {#if pr.owned && (pr.review_decision || pr.ci || pr.mergeable || pr.comments?.length)}
                    <p class="pr-feedback">
                      {#if pr.review_decision === 'changes_requested'}<span class="badge blocked"
                          >Changes requested</span
                        >{:else if pr.review_decision === 'approved'}<span class="badge published"
                          >Approved</span
                        >{/if}
                      {#if pr.ci === 'failure'}<span
                          class="badge failed"
                          title={(pr.failing_checks ?? []).join(', ')}>CI failing</span
                        >{:else if pr.ci === 'pending'}<span class="badge running">CI pending</span
                        >{:else if pr.ci === 'success'}<span class="badge clean">CI passing</span
                        >{/if}
                      {#if pr.mergeable === 'conflicts'}<span class="badge blocked">Conflicts</span
                        >{/if}
                      {#if pr.comments?.length}<span class="badge"
                          >{pr.comments.length} comment{pr.comments.length === 1 ? '' : 's'}</span
                        >{/if}
                    </p>
                  {/if}
                </div>
                <span class={'badge ' + (pr.owned ? 'published' : 'queued')}
                  >{pr.owned ? 'Octomus owned' : 'Context only'}</span
                ><Icon name="external" size={16} /></a
              >{:else}<div class="empty">
                <Icon name="prs" size={34} />
                <h3>Room for your next improvement.</h3>
                <p>Open Octomus branches appear here after discovery grounds the repository.</p>
              </div>{/each}
          </section>
          {#if published.length}<section class="panel published-panel">
              <div class="section-heading">
                <h2>Delivery history</h2>
                <span class="count">{published.length}</span>
              </div>
              {@render taskList(published)}
            </section>{/if}
        {:else}<Settings
            editable={data.control.paused && !data.active_tasks && !data.cycle_active}
            onsaved={refresh}
          />{/if}
        <footer class="content-footer">
          <span><span class="footer-dot"></span> Thoughtful progress. No artificial churn.</span
          ><span>Updated {lastUpdated || 'just now'} · v0.1.0</span>
        </footer>
      </main>
    </div>
  </div>
  {#if selected}{#key selected}<TaskDetail
        id={selected}
        onclose={() => (selected = null)}
        onaction={refresh}
      />{/key}{/if}
{/if}
{#snippet searchBox()}<label class="search-box"
    ><Icon name="search" size={17} /><input
      bind:value={search}
      placeholder="Search…"
      aria-label="Search work"
    />{#if search}<button
        class="icon-button"
        aria-label="Clear search"
        onclick={() => (search = '')}><Icon name="close" size={14} /></button
      >{/if}</label
  >{/snippet}
{#snippet taskList(tasks: TaskRow[])}<div class="task-list">
    {#each tasks as task}<button class="task-row" onclick={() => (selected = task.id)}
        ><span class={'task-type-icon ' + task.status}
          ><Icon
            name={task.status === 'published'
              ? 'check'
              : task.status === 'blocked'
                ? 'alert'
                : 'code'}
            size={18}
          /></span
        ><span class="task-row-body"
          ><strong>{task.title}</strong><span
            ><span class="tier">{task.tier}</span><span>{task.category}</span><span
              class="dot-separator">·</span
            ><code>{task.target}</code></span
          ></span
        ><span class={'badge ' + task.status}>{task.status}</span><Icon
          name="chevron"
          size={16}
        /></button
      >{:else}<div class="empty">
        <Icon name="check" size={30} />
        <h3>All caught up.</h3>
        <p>No tasks are waiting right now.</p>
      </div>{/each}
  </div>{/snippet}
