<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { api, clockTime, gb, setToken, onUnauthorized, relative, safeUrl } from '$lib/api';
  import { ACTIVE_STATUSES } from '$lib/types';
  import type {
    Snapshot,
    TaskRow,
    Page,
    ProposalRow,
    PrObservation,
    CycleSummary,
    ProposalDetail
  } from '$lib/types';
  import Badge from '$lib/Badge.svelte';
  import Icon from '$lib/Icon.svelte';
  import LoginScreen from '$lib/LoginScreen.svelte';
  import Settings from '$lib/Settings.svelte';
  import type { SetupStatus } from '$lib/setup';
  import TaskDetail from '$lib/TaskDetail.svelte';
  import RunEvidence from '$lib/RunEvidence.svelte';
  import {
    DECISIONS,
    cycleLabel,
    decisionCounts as decisionEntries,
    decisionTone,
    modeLabel,
    planningVerdict,
    taskOutcomeCounts
  } from '$lib/evidence';
  let connected = $state(false),
    accessToken = $state(''),
    data = $state<Snapshot | null>(null),
    error = $state(''),
    connectionError = $state(''),
    refreshing = false,
    busy = $state(false),
    pendingAction = $state(''),
    view = $state('overview'),
    settingsVisited = $state(false),
    search = $state(''),
    filter = $state('all'),
    proposalFilter = $state('all'),
    proposalCycle = $state('all'),
    selected = $state<string | null>(null),
    evidence = $state<{ cycle: string; proposal: string | null } | null>(null),
    mobileOpen = $state(false),
    lastUpdated = $state('');
  /** The control that opened the first panel; keyboard focus returns there on close. */
  let panelOpener: HTMLElement | null = null;
  function rememberOpener() {
    if (selected || evidence) return;
    panelOpener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  }
  /** Only one panel is ever open: run evidence hands deep inspection to TaskDetail. */
  function inspectRun(cycle: string, proposal: string | null) {
    rememberOpener();
    selected = null;
    evidence = { cycle, proposal };
  }
  function inspectTask(id: string) {
    rememberOpener();
    evidence = null;
    selected = id;
  }
  async function closePanels() {
    if (!selected && !evidence) return;
    selected = null;
    evidence = null;
    const opener = panelOpener;
    panelOpener = null;
    await tick();
    if (opener?.isConnected) opener.focus();
    else document.getElementById('main-content')?.focus();
  }
  function inspectLatestRun() {
    if (latestCycle) inspectRun(latestCycle.id, null);
  }
  const navigation = [
    { id: 'overview', label: 'Overview', icon: 'overview' },
    { id: 'queue', label: 'Task queue', icon: 'queue' },
    { id: 'proposals', label: 'Proposals', icon: 'proposals' },
    { id: 'prs', label: 'Pull requests', icon: 'prs' },
    { id: 'settings', label: 'Configuration', icon: 'settings' }
  ];
  let filtered = $state<TaskRow[]>([]);
  let proposals = $state<ProposalRow[]>([]);
  let prRows = $state<PrObservation[]>([]);
  /** A PR record's identity: the repository compared case-insensitively, plus the number. */
  const prKey = (observed: PrObservation) =>
    `${observed.repository.toLowerCase()}#${observed.pr.number}`;
  let cycleRows = $state<CycleSummary[]>([]);
  let cycleCursor = $state<number | null>(null);
  let cycleRequest = Promise.resolve();
  let decisionCounts = $state<Record<string, number>>({});
  let listBefore = $state<number | null>(null);
  let listNext = $state<number | null>(null);
  let previousPages = $state<(number | null)[]>([]);
  let listRefresh = $state(0);
  let listLoading = $state(false);
  let listLoaded = $state(false);
  let listError = $state('');
  let listGeneration = 0;
  let listRequest: AbortController | null = null;
  let lastScope = '';
  let lastPage = '';
  let sessionGeneration = 0;
  let published = $derived(data?.tasks.filter((t) => t.status === 'published') ?? []);
  let attentionCount = $derived((data?.counts.blocked ?? 0) + (data?.counts.failed ?? 0));
  /** Status totals behind the queue filter tabs; 'active' and 'attention' are status groups. */
  let queueTabCounts = $derived.by(() => {
    const counts = data?.counts ?? {};
    const sum = (keys: string[]) => keys.reduce((n, k) => n + (counts[k] ?? 0), 0);
    return {
      all: sum(Object.keys(counts)),
      active: sum(ACTIVE_STATUSES),
      queued: counts.queued,
      published: counts.published,
      attention: attentionCount,
      blocked: counts.blocked,
      cancelled: counts.cancelled
    } as Record<string, number | undefined>;
  });
  /** Decision totals behind the proposal filter tabs, scoped to the selected cycle. */
  let proposalTabCounts = $derived({
    all: Object.values(decisionCounts).reduce((n, v) => n + v, 0),
    ...decisionCounts
  } as Record<string, number | undefined>);
  let latestCycle = $derived(data?.cycles[0]);
  type ControlAction = 'resume' | 'pause' | 'cycle' | 'audit';
  const canControl = $derived({
    resume: !!data?.configured && data.active_cycle_mode !== 'audit' && !data.baseline_active,
    pause: !!data?.configured && data.active_cycle_mode !== 'audit',
    cycle:
      !!data?.configured &&
      data.control.paused &&
      !data.cycle_active &&
      !data.active_tasks &&
      !data.baseline_active,
    audit:
      !!data?.audit_configured &&
      data.control.paused &&
      !data.cycle_active &&
      !data.active_tasks &&
      !data.baseline_active
  });
  /** Polled snapshot facts the setup checklist reads; nothing new is stored or fetched. */
  const setupStatus = $derived<SetupStatus | null>(
    data
      ? {
          configured: data.configured,
          audit_configured: data.audit_configured,
          paused: data.control.paused,
          mode: data.control.mode,
          active_tasks: data.active_tasks,
          cycle_active: data.cycle_active,
          baseline_active: data.baseline_active,
          baseline: data.baseline,
          notifications: data.notifications,
          active_cycle_mode: data.active_cycle_mode,
          queued: data.counts.queued ?? 0,
          latest: data.cycles[0] ?? null
        }
      : null
  );
  /** The checklist hands off to the existing Overview controls; the operator still has to click. */
  async function chooseOnOverview(action: 'audit' | 'cycle') {
    await navigate('overview');
    document.getElementById(action === 'audit' ? 'run-audit-control' : 'run-once-control')?.focus();
  }
  $effect(() => {
    const scope = `${connected}:${view}:${search}:${filter}:${proposalFilter}:${proposalCycle}`;
    const before = listBefore;
    const refreshNumber = listRefresh;
    const changed = scope !== lastScope;
    if (changed) {
      lastScope = scope;
      listBefore = null;
      previousPages = [];
    }
    if (!connected || !['queue', 'proposals', 'prs'].includes(view)) return;
    const cursor = changed ? null : before;
    const page = `${scope}:${cursor}`;
    if (page !== lastPage) {
      lastPage = page;
      filtered = [];
      proposals = [];
      prRows = [];
      decisionCounts = {};
      listNext = null;
      listLoaded = false;
      listError = '';
    }
    listLoading = true;
    const timer = setTimeout(() => loadList(cursor), 100);
    void refreshNumber;
    return () => {
      clearTimeout(timer);
      listRequest?.abort();
      listGeneration++;
    };
  });
  async function loadList(before: number | null) {
    const current = ++listGeneration;
    listRequest?.abort();
    const controller = new AbortController();
    listRequest = controller;
    listLoading = true;
    try {
      const params = new URLSearchParams({ limit: '50', q: search });
      if (before !== null) params.set('before', String(before));
      let endpoint = 'tasks';
      if (view === 'queue') params.set('status', filter);
      if (view === 'proposals') {
        endpoint = 'proposals';
        params.set('status', proposalFilter);
        params.set('cycle', proposalCycle);
      }
      if (view === 'prs') {
        endpoint = 'prs';
        params.set('status', filter);
      }
      const page = await api<Page<TaskRow | ProposalRow | PrObservation>>(
        `/${endpoint}?${params}`,
        'GET',
        undefined,
        controller.signal
      );
      if (current !== listGeneration || controller.signal.aborted) return;
      if (view === 'queue') filtered = page.items as TaskRow[];
      if (view === 'proposals') {
        proposals = (page.items as ProposalRow[]).map((summary) => {
          const previous = proposals.find(
            (p) => p.cycle_id === summary.cycle_id && p.id === summary.id
          );
          if (!previous) return summary;
          // Keep fetched detail separate from the truncated polling summary.
          const detailChanged = previous.content_revision !== summary.content_revision;
          Object.assign(previous, summary);
          if (detailChanged) {
            previous.detail = undefined;
            if (previous.detailRequested) void loadProposal(previous);
          }
          return previous;
        });
        decisionCounts = page.counts;
      }
      if (view === 'prs') prRows = page.items as PrObservation[];
      listNext = page.next_cursor;
      listLoaded = true;
      listError = '';
    } catch (e) {
      if (current === listGeneration && !controller.signal.aborted)
        listError = (e as Error).message;
    } finally {
      if (current === listGeneration) listLoading = false;
    }
  }
  function loadCycles(more = false) {
    const currentSession = sessionGeneration;
    const request = cycleRequest.then(async () => {
      if (!connected || currentSession !== sessionGeneration) return;
      if (more && cycleCursor === null) return;
      let before = more ? cycleCursor : null;
      const oldest = more ? undefined : cycleRows.at(-1)?.id;
      const rows: CycleSummary[] = [];
      do {
        const params = new URLSearchParams({ limit: '100' });
        if (before !== null) params.set('before', String(before));
        const page = await api<Page<CycleSummary>>(`/cycles?${params}`);
        rows.push(...page.items);
        before = page.next_cursor;
      } while (!more && before !== null && oldest && !rows.some((c) => c.id === oldest));
      cycleRows = more ? [...cycleRows, ...rows] : rows;
      cycleCursor = before;
    });
    cycleRequest = request.catch(() => {});
    return request;
  }
  async function loadProposal(p: ProposalRow) {
    const currentSession = sessionGeneration;
    p.detailRequested = true;
    const revision = p.content_revision;
    if (p.detail || p.detailLoading === revision) return;
    p.detailLoading = revision;
    try {
      const detail = await api<ProposalDetail>(
        `/proposals/${encodeURIComponent(p.cycle_id)}/${encodeURIComponent(p.id)}`
      );
      // A response for an older summary must not overwrite newer evidence.
      if (
        currentSession === sessionGeneration &&
        p.content_revision === revision &&
        detail.content_revision === revision
      ) {
        p.detail = detail;
      }
    } catch (e) {
      if (currentSession === sessionGeneration && p.content_revision === revision)
        error = (e as Error).message;
    } finally {
      if (p.detailLoading === revision) p.detailLoading = undefined;
    }
  }
  async function cycleAction(value: string) {
    if (busy) return;
    busy = true;
    pendingAction = value;
    error = '';
    try {
      await api(`/cycles/${proposalCycle}/${value}`, 'POST');
      await loadCycles();
      await refresh();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
      pendingAction = '';
    }
  }
  async function refresh() {
    if (!connected || refreshing) return;
    const currentSession = sessionGeneration;
    refreshing = true;
    try {
      data = await api<Snapshot>('/state');
      if (!listLoading) listRefresh++;
      if (view === 'proposals') await loadCycles();
      connectionError = '';
      lastUpdated = clockTime();
    } catch (e) {
      if (connected && currentSession === sessionGeneration) connectionError = (e as Error).message;
    } finally {
      if (currentSession === sessionGeneration) refreshing = false;
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
      lastUpdated = clockTime();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  onMount(() => {
    const unsubscribe = onUnauthorized(() => {
      if (!connected) return;
      disconnect();
      error = 'Session expired. Connect again to inspect private records.';
    });
    const timer = setInterval(refresh, 4000);
    return () => {
      clearInterval(timer);
      unsubscribe();
      disconnect();
    };
  });
  async function navigate(id: string) {
    const currentSession = sessionGeneration;
    view = id;
    if (id === 'settings') settingsVisited = true;
    search = '';
    filter = 'all';
    mobileOpen = false;
    await tick();
    document.getElementById('main-content')?.focus();
    window.scrollTo(0, 0);
    if (id === 'proposals') {
      try {
        await loadCycles();
      } catch (e) {
        if (currentSession === sessionGeneration) error = (e as Error).message;
      }
    }
  }
  async function onWindowKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape' && mobileOpen) {
      event.preventDefault();
      mobileOpen = false;
      await tick();
      document.getElementById('navigation-toggle')?.focus();
      return;
    }
    // "/" jumps to the list search on the views that have one.
    if (event.key !== '/' || event.defaultPrevented || mobileOpen) return;
    const target = event.target as HTMLElement | null;
    if (target?.closest('input, textarea, select, [contenteditable="true"], dialog')) return;
    const box = document.querySelector<HTMLInputElement>('.search-box input');
    if (!box) return;
    event.preventDefault();
    box.focus();
  }
  async function toggleNavigation() {
    mobileOpen = !mobileOpen;
    if (mobileOpen) {
      await tick();
      document
        .querySelector<HTMLButtonElement>('#workspace-navigation [aria-current="page"]')
        ?.focus();
    }
  }
  async function control(action: ControlAction) {
    if (busy || !canControl[action]) return;
    busy = true;
    pendingAction = action;
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
      pendingAction = '';
    }
  }
  function disconnect() {
    sessionGeneration++;
    connected = false;
    settingsVisited = false;
    data = null;
    setToken('');
    selected = null;
    evidence = null;
    panelOpener = null;
    listGeneration++;
    listRequest?.abort();
    refreshing = false;
    listLoading = false;
    listLoaded = false;
    listError = '';
    lastPage = '';
    busy = false;
    pendingAction = '';
    filtered = [];
    proposals = [];
    prRows = [];
    cycleRows = [];
    cycleCursor = null;
    cycleRequest = Promise.resolve();
    decisionCounts = {};
    listBefore = null;
    listNext = null;
    previousPages = [];
    proposalCycle = 'all';
    proposalFilter = 'all';
    search = '';
    filter = 'all';
    view = 'overview';
    mobileOpen = false;
    lastUpdated = '';
    error = '';
    connectionError = '';
  }
</script>

<svelte:window onkeydown={onWindowKeydown} />
<svelte:head
  ><title>Octomus Agent · Your project, moving forward</title><meta
    name="description"
    content="The private control room for continuous, reviewed project improvement."
  /></svelte:head
>
{#if !connected}
  <LoginScreen bind:accessToken {error} {busy} onsubmit={login} />
{:else if data}
  <a class="skip-link" href="#main-content">Skip to main content</a>
  <div class="app-shell">
    <aside id="workspace-navigation" class:mobile-open={mobileOpen} class="sidebar">
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
            aria-current={view === item.id ? 'page' : undefined}
            class:active={view === item.id}
            onclick={() => navigate(item.id)}
            ><Icon name={item.icon} size={19} /><span>{item.label}</span
            >{#if item.id === 'queue' && (data.counts.queued ?? 0) > 0}<b
                >{data.counts.queued ?? 0}</b
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
            id="navigation-toggle"
            aria-label="Toggle navigation"
            aria-controls="workspace-navigation"
            aria-expanded={mobileOpen}
            onclick={toggleNavigation}><Icon name="menu" /></button
          ><span>Workspace</span><Icon name="chevron" size={13} /><strong
            >{navigation.find((n) => n.id === view)?.label}</strong
          >
        </div>
        <div class="topbar-right">
          <span class="live-indicator" class:offline={!!connectionError}
            ><span></span>{connectionError ? 'Reconnecting' : 'Connected'}</span
          >{#if lastUpdated}<span class="topbar-updated">Updated {lastUpdated}</span>{/if}<span
            class="topbar-divider"
          ></span><span class="operator-avatar" title="Private operator">OP</span>
        </div>
      </header>
      <main class="content" id="main-content" tabindex="-1">
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
                disabled={busy || !canControl[data.control.paused ? 'resume' : 'pause']}
                onclick={() => control(data?.control.paused ? 'resume' : 'pause')}
                ><Icon name={data.control.paused ? 'play' : 'pause'} size={16} />{pendingAction ===
                'resume'
                  ? 'Starting continuous…'
                  : pendingAction === 'pause'
                    ? 'Pausing…'
                    : data.control.paused
                      ? 'Start continuous'
                      : 'Pause'}</button
              ><button
                id="run-once-control"
                class="button primary"
                disabled={busy || !canControl.cycle}
                onclick={() => control('cycle')}
                ><Icon name="refresh" size={16} />{pendingAction === 'cycle'
                  ? 'Starting run…'
                  : 'Run once'}</button
              >
              <button
                id="run-audit-control"
                class="button"
                disabled={busy || !canControl.audit}
                onclick={() => control('audit')}
                ><Icon name="proposals" size={16} />{pendingAction === 'audit'
                  ? 'Starting audit…'
                  : 'Run an audit'}</button
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
        {#if data.planning_capacity.status !== 'ready'}
          <div class="notice" role="status" aria-live="polite">
            <Icon name="alert" /><span
              >A complete planning pass requires {data.planning_capacity.required} daily admissions; {data
                .planning_capacity.remaining} remain today.
              {data.planning_capacity.status === 'limit_too_low'
                ? 'The configured daily limit cannot fund a complete planning pass; increase it in Configuration.'
                : 'The daily allowance resets at midnight UTC.'}
              {data.control.mode === 'continuous' && !data.control.paused
                ? 'Continuous operation keeps waiting and plans again when the allowance returns.'
                : 'Audit and Run once are refused until planning can be funded.'}</span
            >
          </div>
        {/if}
        {#if data.pr_capacity.status !== 'ready'}
          <div class="notice" role="status" aria-live="polite">
            <Icon name="alert" /><span
              >{#if data.pr_capacity.status === 'full'}Open-PR capacity is full: {data.pr_capacity
                  .owned_open} owned open PRs of {data.pr_capacity.limit}
                allowed{data.pr_capacity.reserved > 0
                  ? `, plus ${data.pr_capacity.reserved} reserved deliveries`
                  : ''}. New-PR work waits for an observed closure or merge; maintenance on eligible
                owned PRs continues.
              {:else if data.pr_capacity.status === 'refreshing'}Refreshing the open-PR inventory
                before admitting new-PR work…
              {:else}Open-PR capacity is unavailable: {data.pr_capacity.reason ??
                  'no complete inventory observed'}. New-PR work waits; unknown capacity is never
                treated as zero.{/if}
              {#if data.pr_capacity.observed_at}Observed {relative(
                  data.pr_capacity.observed_at
                )}.{/if}</span
            >
          </div>
        {/if}
        <div class="notice" aria-label="Operating mode" aria-live="polite">
          <span
            >{data.control.mode === 'run_once'
              ? 'Run once'
              : data.control.mode === 'continuous'
                ? 'Continuous operation'
                : 'New work paused'} · {data.active_tasks} active tasks{data.control.paused &&
            data.active_tasks > 0
              ? ' · active workflows may publish'
              : ''}</span
          >
        </div>
        {#if view === 'overview'}
          {#if !data.configured}<section class="onboarding">
              <div>
                <span class="eyebrow">LET’S SET THINGS IN MOTION</span>
                <h2>A home for your next improvement.</h2>
                <p>
                  Connect your repository, choose model routes, and set the checks every change must
                  pass. Save, check the connection, then try an audit. Setup never starts work on
                  its own.
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
          {#if data.cycles.length === 0}
            <section class="first-run-guide" aria-label="Choose your first run">
              <div class="first-run-heading">
                <span class="eyebrow">START WITH A LOOK AROUND</span>
                <h2>Your first move: an audit.</h2>
                <p>
                  Read the recommendations before choosing an execution run. Each new run plans
                  afresh.
                </p>
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
              ><small
                >{attentionCount
                  ? 'Work preserved for inspection'
                  : 'No blocked or failed tasks'}</small
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
                  <button class="button primary small" onclick={inspectLatestRun}
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
              {@const runTasks = taskOutcomeCounts(
                data.tasks.filter((t) => t.cycle_id === cycle.id)
              )}
              <div class="run-outcome">
                <div>
                  <span class="eyebrow">PROPOSAL DECISIONS</span>
                  <ul class="outcome-counts" aria-label="Proposal decisions">
                    {#each decisionEntries(cycle.decisions) as entry (entry.decision)}<li>
                        <strong>{entry.count}</strong><Badge
                          label={entry.decision}
                          tone={entry.tone}
                        />
                      </li>{:else}<li class="muted">No decisions recorded</li>{/each}
                  </ul>
                  <small>{planning.detail}</small>
                </div>
                <div>
                  <span class="eyebrow">RECENT TASKS FROM THIS RUN</span>
                  {#if runTasks.length}
                    <ul class="outcome-counts" aria-label="Recent tasks from this run">
                      {#each runTasks as entry (entry.label)}<li>
                          <strong>{entry.count}</strong><Badge
                            label={entry.label}
                            tone={entry.tone}
                          />
                        </li>{/each}
                    </ul>
                    <small
                      >Published means a pull request was delivered. Merging stays with you.</small
                    >
                  {:else}
                    <p class="muted">
                      {cycle.mode === 'audit'
                        ? 'Audits record recommendations and queue no tasks.'
                        : 'No tasks from this run appear in the recent window. Older tasks may exist.'}
                    </p>
                  {/if}
                  <small
                    >Recent window only, not cycle totals. Inspect run for complete retained run
                    evidence.</small
                  >
                </div>
              </div>
              {#if cycle.error}<div class="notice error">
                  <Icon name="alert" size={18} /><span>{cycle.error}</span>
                </div>{/if}
            {/if}
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
          </section>
          {#if attentionCount}<section
              class="panel attention-panel"
              aria-labelledby="attention-heading"
            >
              <div class="section-heading">
                <div>
                  <h2 id="attention-heading">
                    Needs attention <span class="count">{attentionCount}</span>
                  </h2>
                  <p>Blocked or failed tasks keep their workspace and evidence for inspection.</p>
                </div>
                <button
                  class="text-button"
                  onclick={async () => {
                    await navigate('queue');
                    filter = 'attention';
                  }}>View all unresolved work<Icon name="arrow" size={15} /></button
                >
              </div>
              {@render taskList(data.attention_tasks)}
            </section>{/if}
          <div class="overview-columns">
            <section class="panel">
              <div class="section-heading">
                <div>
                  <h2>
                    Work in motion <span class="count"
                      >{data.active_tasks + (data.counts.queued ?? 0)}</span
                    >
                  </h2>
                  <p>Good changes, one focused task at a time.</p>
                </div>
                <button class="text-button" onclick={() => navigate('queue')}
                  >View queue<Icon name="arrow" size={15} /></button
                >
              </div>
              {#if data.active_tasks > 0 || (data.counts.queued ?? 0) > 0}{@render taskList(
                  data.tasks
                    .filter((t) => ACTIVE_STATUSES.includes(t.status) || t.status === 'queued')
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
                    disabled={data.configured && (busy || !canControl.cycle)}
                    onclick={() => (data?.configured ? control('cycle') : navigate('settings'))}
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
                    <span class={'activity-point ' + (event.kind === 'error' ? 'error-point' : '')}
                    ></span>
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
                  {data.planning_capacity.remaining} remain · open-PR capacity {data.pr_capacity
                    .owned_open ?? '?'}/{data.pr_capacity.limit}{data.pr_capacity.reserved > 0
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
                    Application storage: {gb(data.storage.application_bytes)} GB / {gb(
                      data.storage_limit
                    )} GB admission limit. Measured {relative(data.storage.measured_at)}.
                  </p>
                  <p>
                    Task workspaces: {gb(data.storage.task_bytes)} GB · Planning clones: {gb(
                      data.storage.planning_bytes
                    )} GB.
                  </p>
                  <p>
                    {data.storage.runner_transcripts.message} · {data.storage.runner_transcripts
                      .status}.
                  </p>
                  {#each Object.entries(data.storage.runner_transcripts.runners ?? {}) as [backend, usage]}<p
                    >
                      {backend} storage: {usage.bytes === null
                        ? 'Unavailable'
                        : `${gb(usage.bytes)} GB`}
                    </p>{/each}{:else}<p>
                    Storage measurement pending. This limit controls admission, not disk growth
                    during active work.
                  </p>{/if}
                <p>
                  Session budget today: {data.sessions_today} of {data.session_limit} admissions. Admissions
                  reserve budget before work starts; they are not completed turns or billed usage.
                </p>
              </div>
            </details>
          </section>
        {:else if view === 'queue'}
          <section class="panel">
            <div class="list-toolbar">
              {@render filterTabs(
                ['all', 'active', 'queued', 'published', 'attention', 'blocked', 'cancelled'],
                filter,
                'Task filters',
                (state) => (filter = state),
                queueTabCounts
              )}
              {@render searchBox()}
            </div>
            {#if filtered.length}{@render taskList(filtered)}{:else if listLoaded}<div
                class="empty"
              >
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
          <div class="actions">
            {#if cycleCursor !== null}<button class="button" onclick={() => loadCycles(true)}
                >Load older cycles</button
              >{/if}
            {#if proposalCycle !== 'all' && cycleRows.find((c) => c.id === proposalCycle)?.status !== 'running'}
              <button class="button" disabled={busy} onclick={() => cycleAction('archive')}
                >{pendingAction === 'archive' ? 'Archiving cycle…' : 'Archive cycle'}</button
              >
              {#if cycleRows.find((c) => c.id === proposalCycle)?.lifecycle.archived_at}<button
                  class="button danger"
                  disabled={busy}
                  onclick={() => cycleAction('discard')}
                  >{pendingAction === 'discard'
                    ? 'Discarding workspaces…'
                    : 'Discard cycle workspaces'}</button
                >{/if}
            {/if}
          </div>
          <div class="proposal-controls">
            <div class="cycle-picker">
              <label for="proposal-cycle">Cycle</label>
              <select id="proposal-cycle" bind:value={proposalCycle}>
                <option value="all">All cycles</option>
                {#each cycleRows as cycle}<option value={cycle.id}
                    >{cycleLabel(cycle)} · {cycle.status}</option
                  >{/each}
              </select>
            </div>
            <div class="decision-counts" role="group" aria-label="Decision counts">
              {#each DECISIONS as decision}
                <span class={'badge ' + decisionTone(decision)}
                  >{decision}: {decisionCounts[decision] ?? 0}</span
                >
              {/each}
            </div>
          </div>
          <section class="panel">
            <div class="list-toolbar">
              {@render filterTabs(
                ['all', ...DECISIONS],
                proposalFilter,
                'Proposal filters',
                (state) => (proposalFilter = state),
                proposalTabCounts
              )}
              {@render searchBox()}
            </div>
            <div class="proposal-list">
              {#each proposals as p (JSON.stringify([p.cycle_id, p.id]))}<article
                  class="proposal-card"
                >
                  <div class="row-between">
                    <div class="proposal-meta">
                      <span class={'badge ' + p.decision}>{p.decision}</span><span
                        >{cycleLabel({ mode: p.mode, number: p.cycle })}</span
                      ><span class="tier">{p.tier}</span>
                    </div>
                    <span class="category">{p.category}</span>
                  </div>
                  <h2>{p.title}</h2>
                  <p>{p.detail?.problem ?? p.problem}</p>
                  <div class="decision-reason">
                    <Icon name="shield" size={17} />
                    <p>{p.detail?.reason ?? p.reason}</p>
                  </div>
                  <details
                    ontoggle={(event) => {
                      if (event.currentTarget.open) loadProposal(p);
                    }}
                  >
                    <summary>Scope, evidence & execution prompt</summary>
                    <p>{p.detail?.benefit ?? p.benefit}</p>
                    <p>{p.detail?.scope ?? p.scope}</p>
                    {#each p.detail?.evidence ?? p.evidence as evidence}<p class="evidence">
                        {evidence}
                      </p>{/each}
                    <pre class="prompt">{p.detail?.prompt ?? p.prompt}</pre>
                    <small
                      >Dependencies: {(p.detail?.dependencies ?? p.dependencies).join(', ') ||
                        'None'}</small
                    >
                  </details>
                  <div class="proposal-target">
                    <Icon name="branch" size={14} /><code>{p.target}</code>
                    <button class="text-button" onclick={() => inspectRun(p.cycle_id, p.id)}
                      >Inspect decision evidence<Icon name="arrow" size={15} /></button
                    >
                  </div>
                </article>{/each}
              {#if !proposals.length && listLoaded}<div class="empty">
                  <Icon name="proposals" size={34} />
                  <h3>
                    {search || proposalFilter !== 'all' || proposalCycle !== 'all'
                      ? 'No matching proposals'
                      : 'Better ideas start with questions.'}
                  </h3>
                  <p>
                    {search || proposalFilter !== 'all' || proposalCycle !== 'all'
                      ? 'Try another cycle, filter or search term.'
                      : 'Discovery explores your project. Two adversarial reviewers challenge each proposal before the orchestrator decides.'}
                  </p>
                </div>{/if}
            </div>
          </section>
        {:else if view === 'prs'}
          <div class="notice">
            <Icon name="shield" size={18} /><span
              >Octomus publishes reviewed pull requests. Merge decisions stay with you.</span
            >
          </div>
          <div class="list-toolbar">
            {@render filterTabs(
              ['all', 'open', 'merged', 'closed'],
              filter,
              'PR filters',
              (state) => (filter = state)
            )}
            {@render searchBox()}
          </div>
          <section class="panel">
            <div class="section-heading">
              <div>
                <h2>PR outcomes</h2>
                <p>
                  Observed every five minutes. Delivery and maintainer acceptance are recorded
                  separately.
                </p>
              </div>
              <span class="count">{prRows.length}</span>
            </div>
            {#each prRows as observed (prKey(observed))}{@const pr = observed.pr}<a
                class="pr-row"
                href={safeUrl(pr.url)}
                target="_blank"
                rel="noreferrer"
                ><span class={'pr-icon ' + pr.state}><Icon name="prs" /></span>
                <div>
                  <h3>{pr.title}<span class="pr-number">#{pr.number}</span></h3>
                  <p>
                    <code>{pr.branch}</code><span>→</span><code>{pr.base}</code
                    >{#if observed.observed_at}<span class="pr-observed"
                        >· observed {relative(observed.observed_at)}</span
                      >{/if}
                  </p>
                </div>
                <span class={'badge ' + (pr.owned ? 'published' : 'queued')}
                  >{pr.state}{observed.external_head_movement
                    ? ' · external head change'
                    : ''}</span
                ><Icon name="external" size={16} /></a
              >{/each}
            {#if !prRows.length && listLoaded}<div class="empty">
                <Icon name="prs" size={34} />
                <h3>
                  {search || filter !== 'all'
                    ? 'No matching pull requests'
                    : 'Room for your next improvement.'}
                </h3>
                <p>
                  {search || filter !== 'all'
                    ? 'Try another filter or search term.'
                    : 'Open Octomus branches appear here after discovery grounds the repository.'}
                </p>
              </div>{/if}
          </section>
          {#if published.length}<section class="panel published-panel">
              <div class="section-heading">
                <h2>Delivery history</h2>
                <span class="count">{published.length}</span>
              </div>
              {@render taskList(published)}
            </section>{/if}
        {/if}
        {#if settingsVisited}<div hidden={view !== 'settings'}>
            <Settings
              active={view === 'settings'}
              editable={data.control.paused &&
                !data.active_tasks &&
                !data.cycle_active &&
                !data.baseline_active}
              status={setupStatus}
              onsaved={refresh}
              onchoose={chooseOnOverview}
            />
          </div>{/if}
        {#if ['queue', 'proposals', 'prs'].includes(view)}
          <div class="actions" aria-label="History pagination">
            <button
              class="button"
              disabled={listLoading || previousPages.length === 0}
              onclick={() => {
                listBefore = previousPages.at(-1) ?? null;
                previousPages = previousPages.slice(0, -1);
              }}>Previous page</button
            >
            <button
              class="button"
              disabled={listLoading || !!listError || listNext === null}
              onclick={() => {
                previousPages = [...previousPages, listBefore];
                listBefore = listNext;
              }}>Next page</button
            >
          </div>
          <!-- Background feedback follows all results, including PR delivery history. -->
          {@render listFeedback(
            view === 'queue' ? 'tasks' : view === 'proposals' ? 'proposals' : 'pull requests',
            filtered.length + proposals.length + prRows.length
          )}
        {/if}
        <footer class="content-footer">
          <span><span class="footer-dot"></span> Thoughtful progress. No artificial churn.</span
          ><span>Updated {lastUpdated || 'just now'} · v0.1.0</span>
        </footer>
      </main>
    </div>
  </div>
  {#if selected}{#key selected}<TaskDetail
        id={selected}
        onselect={(id) => (selected = id)}
        onclose={closePanels}
        onaction={refresh}
      />{/key}{/if}
  {#if evidence}<RunEvidence
      cycleId={evidence.cycle}
      proposalId={evidence.proposal}
      onopentask={inspectTask}
      onclose={closePanels}
    />{/if}
{/if}
{#snippet filterTabs(
  labels: string[],
  current: string,
  aria: string,
  onselect: (state: string) => void,
  counts: Record<string, number | undefined> = {}
)}<div class="filter-tabs" role="group" aria-label={aria}>
    {#each labels as state}<button
        class:active={current === state}
        aria-pressed={current === state}
        onclick={() => onselect(state)}
        >{state}{#if counts[state]}<b class="tab-count" aria-hidden="true">{counts[state]}</b
          >{/if}</button
      >{/each}
  </div>{/snippet}
{#snippet listFeedback(noun: string, count: number)}
  {#if listError}<div class="notice error list-feedback" role="alert">
      <span
        >{listLoaded
          ? `Could not refresh ${noun}. Showing the last received results.`
          : `Could not load ${noun}.`}
        {listError}</span
      >
      <button class="button" disabled={listLoading} onclick={() => listRefresh++}>Retry</button>
    </div>{/if}
  <!-- Keep the status line's space between polls, including at the bottom of a page. -->
  {#if listLoading || listLoaded}<div
      class:empty={!listLoaded && !count}
      class="list-feedback"
      aria-live="polite"
    >
      {#if !listLoaded && !count}<span class="spinner"></span>{/if}
      <p>
        {listLoading
          ? listLoaded
            ? `Refreshing ${noun}…`
            : `Loading ${noun}…`
          : `Results on this page: ${count}`}
      </p>
    </div>{/if}
{/snippet}
{#snippet searchBox()}<label class="search-box"
    ><Icon name="search" size={17} /><input
      bind:value={search}
      placeholder="Search…"
      aria-label="Search work"
      aria-keyshortcuts="/"
      onkeydown={(event) => {
        if (event.key === 'Escape' && search) {
          search = '';
          event.stopPropagation();
        }
      }}
    />{#if search}<button
        class="icon-button"
        aria-label="Clear search"
        onclick={() => (search = '')}><Icon name="close" size={14} /></button
      >{:else}<kbd class="search-hint" aria-hidden="true">/</kbd>{/if}</label
  >{/snippet}
{#snippet taskList(tasks: TaskRow[])}<div class="task-list">
    {#each tasks as task (task.id)}<button class="task-row" onclick={() => inspectTask(task.id)}
        ><span class={'task-type-icon ' + task.status}
          ><Icon
            name={task.status === 'published'
              ? 'check'
              : task.status === 'blocked' || task.status === 'failed'
                ? 'alert'
                : task.status === 'queued'
                  ? 'clock'
                  : ACTIVE_STATUSES.includes(task.status)
                    ? 'activity'
                    : 'code'}
            size={18}
          /></span
        ><span class="task-row-body"
          ><strong>{task.title}</strong><span
            ><span class="tier">{task.tier}</span><span>{task.category}</span><span
              class="dot-separator">·</span
            ><code>{task.target}</code><span class="dot-separator">·</span><span
              >updated {relative(task.updated_at)}</span
            ></span
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
