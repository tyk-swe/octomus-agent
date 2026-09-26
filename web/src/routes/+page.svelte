<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { api, clockTime, setToken, onUnauthorized, relative } from '$lib/api';
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
  import FilterTabs from '$lib/FilterTabs.svelte';
  import Icon, { type IconName } from '$lib/Icon.svelte';
  import LoginScreen from '$lib/LoginScreen.svelte';
  import Overview from '$lib/Overview.svelte';
  import PrRow from '$lib/PrRow.svelte';
  import ProposalCard from '$lib/ProposalCard.svelte';
  import SearchBox from '$lib/SearchBox.svelte';
  import Settings from '$lib/Settings.svelte';
  import type { SetupStatus } from '$lib/setup';
  import TaskDetail from '$lib/TaskDetail.svelte';
  import TaskList from '$lib/TaskList.svelte';
  import RunEvidence from '$lib/RunEvidence.svelte';
  import { DECISIONS, cycleLabel, decisionTone } from '$lib/evidence';
  /** The running build's version; the sidebar shows its major.minor part. */
  const version = __APP_VERSION__;
  const shortVersion = version.split('.').slice(0, 2).join('.');
  let connected = $state(false),
    accessToken = $state(''),
    data = $state<Snapshot | null>(null),
    error = $state(''),
    connectionError = $state(''),
    busy = $state(false),
    pendingAction = $state(''),
    view = $state('overview'),
    settingsVisited = $state(false),
    search = $state(''),
    filter = $state('all'),
    proposalFilter = $state('all'),
    proposalCycle = $state('all'),
    selected = $state<string | null>(null),
    runPanel = $state<{ cycle: string; proposal: string | null } | null>(null),
    mobileOpen = $state(false),
    lastUpdated = $state('');
  /** The control that opened the first panel; keyboard focus returns there on close. */
  let panelOpener: HTMLElement | null = null;
  function rememberOpener() {
    if (selected || runPanel) return;
    panelOpener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  }
  /** Only one panel is ever open: run evidence hands deep inspection to TaskDetail. */
  function inspectRun(cycle: string, proposal: string | null) {
    rememberOpener();
    selected = null;
    runPanel = { cycle, proposal };
  }
  function inspectTask(id: string) {
    rememberOpener();
    runPanel = null;
    selected = id;
  }
  async function closePanels() {
    if (!selected && !runPanel) return;
    selected = null;
    runPanel = null;
    const opener = panelOpener;
    panelOpener = null;
    await tick();
    if (opener?.isConnected) opener.focus();
    else document.getElementById('main-content')?.focus();
  }
  function inspectLatestRun() {
    if (latestCycle) inspectRun(latestCycle.id, null);
  }
  /** The overview's attention link: the queue, filtered to blocked and failed work. */
  async function viewAttention() {
    await navigate('queue');
    filter = 'attention';
  }
  /** Each view with its page heading; the paged history views also name what they list. */
  const navigation: {
    id: string;
    label: string;
    icon: IconName;
    heading: string;
    lede: string;
    noun?: string;
  }[] = [
    {
      id: 'overview',
      label: 'Overview',
      icon: 'overview',
      heading: 'The bigger picture.',
      lede: 'A clear view of what’s happening, and what’s coming next.'
    },
    {
      id: 'queue',
      label: 'Task queue',
      icon: 'queue',
      heading: 'From idea to improvement.',
      lede: 'Every task has a purpose, a workspace, and a path to a reviewed PR.',
      noun: 'tasks'
    },
    {
      id: 'proposals',
      label: 'Proposals',
      icon: 'proposals',
      heading: 'Worth doing. Before doing.',
      lede: 'Grounded opportunities, challenged from two independent perspectives.',
      noun: 'proposals'
    },
    {
      id: 'prs',
      label: 'Pull requests',
      icon: 'prs',
      heading: 'Progress, ready for review.',
      lede: 'New improvements and continued work on your existing branches.',
      noun: 'pull requests'
    },
    {
      id: 'settings',
      label: 'Configuration',
      icon: 'settings',
      heading: 'Make it work your way.',
      lede: 'Your repository, your priorities, your operating limits.'
    }
  ];
  const current = $derived(navigation.find((item) => item.id === view));
  /** The operating mode as the header status names it; any other mode reads as paused. */
  const OPERATING_MODE_LABELS: Record<string, string> = {
    run_once: 'Run once',
    continuous: 'Continuous operation',
    paused: 'New work paused'
  };
  /** The header status: operating mode, active tasks, and whether paused work may still publish. */
  function operatingStatus(snapshot: Snapshot): string {
    const mode = OPERATING_MODE_LABELS[snapshot.control.mode] ?? 'New work paused';
    const publishing =
      snapshot.control.paused && snapshot.active_tasks > 0 ? ' · active workflows may publish' : '';
    return `${mode} · ${snapshot.active_tasks} active tasks${publishing}`;
  }
  /** The continuous-operation toggle while its request is pending. */
  const TOGGLE_PENDING_LABELS: Record<string, string> = {
    resume: 'Starting continuous…',
    pause: 'Pausing…'
  };
  /** The status filters each paged history view offers, in tab order. */
  const QUEUE_FILTERS = [
    'all',
    'active',
    'queued',
    'published',
    'attention',
    'blocked',
    'cancelled'
  ];
  const PROPOSAL_FILTERS = ['all', ...DECISIONS];
  const PR_FILTERS = ['all', 'open', 'merged', 'closed'];
  let filtered = $state<TaskRow[]>([]);
  let proposals = $state<ProposalRow[]>([]);
  let prRows = $state<PrObservation[]>([]);
  /** A PR record's identity: the repository compared case-insensitively, plus the number. */
  const prKey = (observed: PrObservation) =>
    `${observed.repository.toLowerCase()}#${observed.pr.number}`;
  let cycleRows = $state<CycleSummary[]>([]);
  /**
   * The picked cycle's loaded summary. Its lifecycle decides which workspace action is
   * still open: archiving again would restart the retention clock, and a discarded
   * cycle has nothing left to discard.
   */
  let selectedCycle = $derived(
    proposalCycle === 'all' ? undefined : cycleRows.find((c) => c.id === proposalCycle)
  );
  let cycleCursor = $state<number | null>(null);
  let cyclesLoading = $state(false);
  let decisionCounts = $state<Record<string, number>>({});
  let listBefore = $state<number | null>(null);
  let listNext = $state<number | null>(null);
  let previousPages = $state<(number | null)[]>([]);
  let listRefresh = $state(0);
  let listLoading = $state(false);
  let listLoaded = $state(false);
  let listError = $state('');
  // Non-reactive bookkeeping, never rendered: request chains and generations, the list
  // effect's last scope and page, and the refresh guard. The list effect and the request
  // handlers read and write these without subscribing to them, so they are not $state.
  let cycleRequest = Promise.resolve();
  let listGeneration = 0;
  let listRequest: AbortController | null = null;
  let lastScope = '';
  let lastPage = '';
  let sessionGeneration = 0;
  /** A refresh asked for while one is in flight runs once more after it, never alongside it. */
  let refreshing = false;
  let refreshQueued = false;
  let published = $derived(data?.tasks.filter((t) => t.status === 'published') ?? []);
  let attentionCount = $derived((data?.counts.blocked ?? 0) + (data?.counts.failed ?? 0));
  /** Status totals behind the queue filter tabs; 'active' and 'attention' are status groups. */
  let queueTabCounts = $derived.by(() => {
    const counts = data?.counts ?? {};
    const sum = (keys: readonly string[]) => keys.reduce((n, k) => n + (counts[k] ?? 0), 0);
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
  async function loadOlderCycles() {
    const currentSession = sessionGeneration;
    cyclesLoading = true;
    // Like every operator action, a new attempt replaces the previous attempt's failure.
    error = '';
    try {
      await loadCycles(true);
    } catch (e) {
      if (currentSession === sessionGeneration)
        error = `Could not load older cycles. ${(e as Error).message}`;
    } finally {
      if (currentSession === sessionGeneration) cyclesLoading = false;
    }
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
  async function cycleAction(value: 'archive' | 'discard') {
    if (busy) return;
    const currentSession = sessionGeneration;
    busy = true;
    pendingAction = value;
    error = '';
    try {
      await api(`/cycles/${encodeURIComponent(proposalCycle)}/${value}`, 'POST');
      await loadCycles();
      await refresh();
    } catch (e) {
      // A 401 has already ended the session and explained why on the login screen.
      if (currentSession === sessionGeneration) error = (e as Error).message;
    } finally {
      busy = false;
      pendingAction = '';
    }
  }
  async function refresh() {
    if (!connected) return;
    if (refreshing) {
      // The in-flight snapshot may predate an action that just finished.
      refreshQueued = true;
      return;
    }
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
      if (currentSession === sessionGeneration) {
        refreshing = false;
        if (refreshQueued) {
          refreshQueued = false;
          void refresh();
        }
      }
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
    // Timer polls skip while one is in flight; only explicit refreshes queue a follow-up.
    const timer = setInterval(() => {
      if (!refreshing) void refresh();
    }, 4000);
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
    const currentSession = sessionGeneration;
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
      // A 401 has already ended the session and explained why on the login screen.
      if (currentSession === sessionGeneration) error = (e as Error).message;
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
    runPanel = null;
    panelOpener = null;
    listGeneration++;
    listRequest?.abort();
    refreshing = false;
    refreshQueued = false;
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
    cyclesLoading = false;
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
          ><Icon name="logout" size={17} /><span>Disconnect</span><span class="version"
            >v{shortVersion}</span
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
          ><span>Workspace</span><Icon name="chevron" size={13} /><strong>{current?.label}</strong>
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
            <h1>{current?.heading}</h1>
            <p>{current?.lede}</p>
          </div>
          {#if view !== 'settings'}<div class="actions">
              <button
                class="button"
                disabled={busy || !canControl[data.control.paused ? 'resume' : 'pause']}
                onclick={() => control(data?.control.paused ? 'resume' : 'pause')}
                ><Icon
                  name={data.control.paused ? 'play' : 'pause'}
                  size={16}
                />{TOGGLE_PENDING_LABELS[pendingAction] ??
                  (data.control.paused ? 'Start continuous' : 'Pause')}</button
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
          <!-- Only the capacity message is live: the relative observation time changes every
               minute and would otherwise re-announce the whole notice. -->
          <div class="notice">
            <Icon name="alert" /><span
              ><span role="status" aria-live="polite"
                >{#if data.pr_capacity.status === 'full'}Open-PR capacity is full: {data.pr_capacity
                    .owned_open} owned open PRs of {data.pr_capacity.limit}
                  allowed{data.pr_capacity.reserved > 0
                    ? `, plus ${data.pr_capacity.reserved} reserved deliveries`
                    : ''}. New-PR work waits for an observed closure or merge; maintenance on
                  eligible owned PRs continues.
                {:else if data.pr_capacity.status === 'refreshing'}{data.pr_capacity.reason ??
                    'Refreshing the open-PR inventory'}. New-PR work waits until the refresh
                  completes.
                {:else}Open-PR capacity is unavailable: {data.pr_capacity.reason ??
                    'no complete inventory observed'}. New-PR work waits; unknown capacity is never
                  treated as zero.{/if}</span
              >
              {#if data.pr_capacity.observed_at}Observed {relative(
                  data.pr_capacity.observed_at
                )}.{/if}</span
            >
          </div>
        {/if}
        <div class="notice" role="status" aria-label="Operating mode" aria-live="polite">
          <span>{operatingStatus(data)}</span>
        </div>
        {#if view === 'overview'}
          <Overview
            {data}
            {latestCycle}
            {attentionCount}
            canRunOnce={canControl.cycle}
            {busy}
            {pendingAction}
            onnavigate={navigate}
            oninspectrun={inspectLatestRun}
            onopentask={inspectTask}
            onviewattention={viewAttention}
            onrunonce={() => control('cycle')}
          />
        {:else if view === 'queue'}
          <section class="panel">
            <div class="list-toolbar">
              <FilterTabs
                labels={QUEUE_FILTERS}
                current={filter}
                aria="Task filters"
                onselect={(state) => (filter = state)}
                counts={queueTabCounts}
              />
              <SearchBox bind:value={search} />
            </div>
            {#if filtered.length}<TaskList
                tasks={filtered}
                onselect={inspectTask}
              />{:else if listLoaded}<div class="empty">
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
            {#if cycleCursor !== null}<button
                class="button"
                disabled={cyclesLoading}
                onclick={loadOlderCycles}>Load older cycles</button
              >{/if}
            {#if selectedCycle && selectedCycle.status !== 'running'}
              {#if !selectedCycle.lifecycle.archived_at}<button
                  class="button"
                  disabled={busy}
                  onclick={() => cycleAction('archive')}
                  >{pendingAction === 'archive' ? 'Archiving cycle…' : 'Archive cycle'}</button
                >{:else if !selectedCycle.lifecycle.discarded_at}<button
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
                    >{cycleLabel(cycle)} · {cycle.status}{cycle.lifecycle.discarded_at
                      ? ' · workspaces discarded'
                      : cycle.lifecycle.archived_at
                        ? ' · archived'
                        : ''}</option
                  >{/each}
              </select>
            </div>
            <div class="decision-counts" role="group" aria-label="Decision counts">
              {#each DECISIONS as decision}
                <Badge
                  label={`${decision}: ${decisionCounts[decision] ?? 0}`}
                  tone={decisionTone(decision)}
                />
              {/each}
            </div>
          </div>
          <section class="panel">
            <div class="list-toolbar">
              <FilterTabs
                labels={PROPOSAL_FILTERS}
                current={proposalFilter}
                aria="Proposal filters"
                onselect={(state) => (proposalFilter = state)}
                counts={proposalTabCounts}
              />
              <SearchBox bind:value={search} />
            </div>
            <div class="proposal-list">
              {#each proposals as p (JSON.stringify([p.cycle_id, p.id]))}<ProposalCard
                  proposal={p}
                  onexpand={() => loadProposal(p)}
                  oninspect={() => inspectRun(p.cycle_id, p.id)}
                />{/each}
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
            <FilterTabs
              labels={PR_FILTERS}
              current={filter}
              aria="PR filters"
              onselect={(state) => (filter = state)}
            />
            <SearchBox bind:value={search} />
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
            {#each prRows as observed (prKey(observed))}<PrRow {observed} />{/each}
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
              <TaskList tasks={published} onselect={inspectTask} />
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
        {#if current?.noun}
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
          {@render listFeedback(current.noun, filtered.length + proposals.length + prRows.length)}
        {/if}
        <footer class="content-footer">
          <span><span class="footer-dot"></span> Thoughtful progress. No artificial churn.</span
          ><span>Updated {lastUpdated || 'just now'} · v{version}</span>
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
  {#if runPanel}<RunEvidence
      cycleId={runPanel.cycle}
      proposalId={runPanel.proposal}
      onopentask={inspectTask}
      onclose={closePanels}
    />{/if}
{/if}
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
