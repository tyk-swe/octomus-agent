<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { goto } from '$app/navigation';
  import { client, setToken, setRepository, onUnauthorized } from '#lib/api.ts';
  import { StateSession } from '#lib/stateSession.ts';
  import type {
    ControlAction,
    CycleAction,
    Snapshot,
    TaskRow,
    Page,
    ProposalRow,
    PRObservation,
    RepositoriesView
  } from '#lib/types.ts';
  import Badge from '#lib/Badge.svelte';
  import FilterTabs from '#lib/FilterTabs.svelte';
  import { clockTime, safeUrl } from '#lib/format.ts';
  import Icon from '#lib/Icon.svelte';
  import LoginScreen from '#lib/LoginScreen.svelte';
  import Notices from '#lib/Notices.svelte';
  import Overview from '#lib/Overview.svelte';
  import Proposals from '#lib/Proposals.svelte';
  import Requests from '#lib/Requests.svelte';
  import RelativeTime from '#lib/RelativeTime.svelte';
  import SearchBox from '#lib/SearchBox.svelte';
  import { configurationEditable, controlEligibility, type SetupStatus } from '#lib/setup.ts';
  import TaskDetail from '#lib/TaskDetail.svelte';
  import TaskList from '#lib/TaskList.svelte';
  import RunEvidence from '#lib/RunEvidence.svelte';
  import {
    NAVIGATION,
    PR_FILTERS,
    QUEUE_FILTERS,
    TOGGLE_PENDING_LABELS,
    viewFromHash
  } from '#lib/navigation.ts';
  const version = __APP_VERSION__;
  // Settings and Insights load on demand so the startup chunk stays small. The
  // promises are memoized so a re-render never re-imports or remounts.
  const loadSettings = () => import('#lib/Settings.svelte');
  const loadInsights = () => import('#lib/Insights.svelte');
  let settingsModule: ReturnType<typeof loadSettings> | undefined;
  let insightsModule: ReturnType<typeof loadInsights> | undefined;
  const settingsView = () => (settingsModule ??= loadSettings());
  const insightsView = () => (insightsModule ??= loadInsights());
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
    lastUpdated = $state(''),
    repositories = $state<RepositoriesView['repositories']>([]),
    selectedRepository = $state('');
  let panelOpener: HTMLElement | null = null;
  function rememberOpener() {
    navigationGeneration++;
    if (selected || runPanel) return;
    panelOpener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  }
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
    navigationGeneration++;
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
  async function viewAttention() {
    await navigate('queue');
    filter = 'attention';
  }
  const navigation = NAVIGATION;
  const current = $derived(navigation.find((item) => item.id === view));
  let filtered = $state<TaskRow[]>([]);
  let proposals = $state<ProposalRow[]>([]);
  let proposalCounts = $state<Record<string, number>>({});
  let prRows = $state<PRObservation[]>([]);
  const prKey = (observed: PRObservation) =>
    `${observed.repository.toLowerCase()}#${observed.pr.number}`;
  let queueTabCounts = $state<Record<string, number>>({});
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
  let navigationGeneration = 0;
  // Operator interactions invalidate the redirect; background data loads must not.
  function noteNavigationIntent() {
    navigationGeneration++;
  }
  let controlStatePending = $state(false);
  let repositoriesSyncing = false;
  async function syncRepositories() {
    if (repositoriesSyncing) return;
    repositoriesSyncing = true;
    try {
      const fleet = await client.repositories();
      repositories = fleet.repositories;
    } catch {
      // The sidebar keeps the last known fleet; state reads surface failures.
    } finally {
      repositoriesSyncing = false;
    }
  }
  const stateSession = new StateSession(
    client.state,
    (snapshot) => {
      data = snapshot;
      connectionError = '';
      lastUpdated = clockTime();
      void syncRepositories();
      if (!listLoading) listRefresh++;
    },
    (failure) => {
      if (connected) connectionError = failure.message;
    },
    (pending) => {
      controlStatePending = pending;
    }
  );
  let published = $derived(data?.tasks.filter((t) => t.status === 'published') ?? []);
  let attentionCount = $derived((data?.counts.blocked ?? 0) + (data?.counts.failed ?? 0));
  const documentTitle = $derived(
    connected && data
      ? `${attentionCount ? `(${attentionCount}) ` : ''}${current?.label ?? 'Overview'} · Octomus Agent`
      : 'Octomus Agent'
  );
  const OPERATING_MODE_LABELS: Record<string, string> = {
    run_once: 'Run once',
    continuous: 'Continuous operation',
    paused: 'New work paused'
  };
  function operatingStatus(snapshot: Snapshot): string {
    const mode = OPERATING_MODE_LABELS[snapshot.control.mode] ?? 'New work paused';
    const phase = snapshot.control.batch?.phase === 'merging' ? ' · settling merges' : '';
    const publishing =
      snapshot.control.paused && snapshot.active_tasks > 0 ? ' · active workflows may publish' : '';
    return `${mode}${phase} · ${snapshot.active_tasks} active tasks${publishing}`;
  }
  function operatingTone(snapshot: Snapshot): string {
    if (snapshot.control.paused && snapshot.active_tasks > 0) return 'warn';
    if (snapshot.control.paused) return 'neutral';
    return 'good';
  }
  let latestCycle = $derived(data?.cycles[0]);
  const setupStatus = $derived<SetupStatus | null>(
    data
      ? {
          capabilities: data.capabilities,
          configured: data.configured,
          audit_configured: data.audit_configured,
          paused: data.control.paused,
          mode: data.control.mode,
          active_tasks: data.active_tasks,
          cycle_active: data.cycle_active,
          baseline_active: data.baseline_active,
          auto_merge_active: data.auto_merge.active,
          baseline: data.baseline,
          notifications: data.notifications,
          active_cycle_mode: data.active_cycle_mode,
          queued: data.counts.queued ?? 0,
          latest: data.cycles[0] ?? null,
          planning_capacity: data.planning_capacity,
          control_state_pending: controlStatePending,
          recovery_error: data.recovery_error,
          sandbox: data.sandbox
        }
      : null
  );
  const canControl = $derived(controlEligibility(setupStatus));
  async function cycleAction(cycle: string, action: CycleAction) {
    const currentSession = sessionGeneration;
    busy = true;
    pendingAction = action;
    try {
      await client.cycleAction(cycle, action);
    } finally {
      if (currentSession === sessionGeneration) {
        busy = false;
        pendingAction = '';
      }
    }
  }
  async function chooseOnOverview(action: 'audit' | 'cycle') {
    await navigate('overview');
    document.getElementById(action === 'audit' ? 'run-audit-control' : 'run-once-control')?.focus();
  }
  $effect(() => {
    const scope = `${connected}:${selectedRepository}:${view}:${search}:${filter}:${proposalFilter}:${proposalCycle}`;
    const before = listBefore;
    const refreshNumber = listRefresh;
    const changed = scope !== lastScope;
    if (changed) {
      navigationGeneration++;
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
      proposalCounts = {};
      queueTabCounts = {};
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
      let page;
      if (view === 'queue') {
        params.set('status', filter);
        const result = await client.tasks(params, controller.signal);
        if (current !== listGeneration || controller.signal.aborted) return;
        filtered = result.items;
        queueTabCounts = result.counts;
        page = result;
      } else if (view === 'proposals') {
        params.set('status', proposalFilter);
        params.set('cycle', proposalCycle);
        const result = await client.proposals(params, controller.signal);
        if (current !== listGeneration || controller.signal.aborted) return;
        proposals = result.items;
        proposalCounts = result.counts;
        page = result;
      } else {
        params.set('status', filter);
        const result = await client.prs(params, controller.signal);
        if (current !== listGeneration || controller.signal.aborted) return;
        prRows = result.items;
        page = result;
      }
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
  function refresh() {
    return connected ? stateSession.refresh() : Promise.resolve();
  }
  function baselineChanged() {
    stateSession.invalidate();
    // Accepted start/cancel owns baseline activity until a fresh state read
    // confirms the worker has finished, even when its panel is hidden.
    if (data) data.baseline_active = true;
    void refresh();
  }
  function rememberRepository(value: string) {
    try {
      localStorage.setItem('octomus-repository', value);
    } catch {
      // Browser storage is optional; the selected repository still works in memory.
    }
  }
  function configSaved(repository?: string) {
    if (repository !== undefined && repository.toLowerCase() !== selectedRepository.toLowerCase()) {
      selectedRepository = repository;
      // Identity changes are allowed only for the legacy primary. Address
      // that App without its obsolete explicit repository query.
      setRepository('');
      rememberRepository(repository);
      sessionGeneration++;
    }
    // Readiness belongs to the newly saved configuration; an earlier poll
    // cannot confirm which work can start with it.
    stateSession.invalidate();
    void refresh();
  }
  async function login() {
    busy = true;
    error = '';
    setToken(accessToken.trim());
    try {
      const fleet = await client.repositories();
      repositories = fleet.repositories;
      let remembered = '';
      try {
        remembered = localStorage.getItem('octomus-repository') ?? '';
      } catch {
        // Use the first repository when browser storage is unavailable.
      }
      selectedRepository = repositories.some((entry) => entry.github_repo === remembered)
        ? remembered
        : (repositories[0]?.github_repo ?? '');
      setRepository(selectedRepository);
      view = viewFromHash(location.hash) ?? 'overview';
      settingsVisited = view === 'settings';
      connected = true;
      connectionError = '';
      accessToken = '';
      await refresh();
      await tick();
      window.scrollTo(0, 0);
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
    const timer = setInterval(() => {
      if (!stateSession.active) void refresh();
    }, 4000);
    return () => {
      clearInterval(timer);
      unsubscribe();
      disconnect();
    };
  });
  async function navigate(id: string, record = true) {
    navigationGeneration++;
    view = id;
    if (record && viewFromHash(location.hash) !== id)
      await goto('#' + id, { shallow: true, state: {}, reset: false });
    if (id === 'settings') settingsVisited = true;
    search = '';
    filter = 'all';
    mobileOpen = false;
    await tick();
    document.getElementById('main-content')?.focus();
    window.scrollTo(0, 0);
  }
  function followLocation() {
    if (!connected) return;
    const id = viewFromHash(location.hash);
    if (!id || id === view) return;
    selected = null;
    runPanel = null;
    panelOpener = null;
    void navigate(id, false);
  }
  async function clearListFilters() {
    noteNavigationIntent();
    search = '';
    filter = 'all';
    await tick();
    document.querySelector<HTMLInputElement>('.search-box input')?.focus();
  }
  async function onWindowKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape' && mobileOpen) {
      event.preventDefault();
      await closeDrawer();
      return;
    }
    if (event.key !== '/' || event.defaultPrevented || mobileOpen) return;
    const target = event.target as HTMLElement | null;
    if (target?.closest('input, textarea, select, [contenteditable="true"], dialog')) return;
    const box = document.querySelector<HTMLInputElement>('.search-box input');
    if (!box) return;
    event.preventDefault();
    box.focus();
  }
  $effect(() => {
    document.documentElement.classList.toggle('nav-locked', mobileOpen);
    return () => document.documentElement.classList.remove('nav-locked');
  });
  async function closeDrawer() {
    noteNavigationIntent();
    mobileOpen = false;
    await tick();
    document.getElementById('navigation-toggle')?.focus();
  }
  async function toggleNavigation() {
    noteNavigationIntent();
    mobileOpen = !mobileOpen;
    if (mobileOpen) {
      await tick();
      document
        .querySelector<HTMLButtonElement>('#workspace-navigation [aria-current="page"]')
        ?.focus();
    }
  }
  async function sandboxSelfTest() {
    if (busy) return;
    const currentSession = sessionGeneration;
    busy = true;
    pendingAction = 'self-test';
    error = '';
    try {
      await client.selfTest();
      if (currentSession !== sessionGeneration) return;
      await refresh();
    } catch (e) {
      if (currentSession === sessionGeneration) error = (e as Error).message;
    } finally {
      if (currentSession === sessionGeneration) {
        busy = false;
        pendingAction = '';
      }
    }
  }
  async function control(action: ControlAction) {
    if (busy || !canControl[action]) return;
    const currentSession = sessionGeneration;
    const currentNavigation = navigationGeneration;
    let ownsBusy = true;
    busy = true;
    pendingAction = action;
    error = '';
    try {
      const result = await client.control(action);
      if (currentSession !== sessionGeneration) return;
      stateSession.invalidate();
      if (data) data.control = result;
      // Confirmed running operation must remain pausable while its state read waits.
      if (!result.paused) {
        busy = false;
        pendingAction = '';
        ownsBusy = false;
      }
      await refresh();
      if (currentSession !== sessionGeneration) return;
      if (action === 'audit' && currentNavigation === navigationGeneration) {
        proposalCycle = 'all';
        proposalFilter = 'all';
        await navigate('proposals');
      }
    } catch (e) {
      if (currentSession === sessionGeneration) error = (e as Error).message;
    } finally {
      if (ownsBusy && currentSession === sessionGeneration) {
        busy = false;
        pendingAction = '';
      }
    }
  }
  async function switchRepository(value: string) {
    if (value === selectedRepository || busy) return;
    selectedRepository = value;
    rememberRepository(value);
    setRepository(value);
    sessionGeneration++;
    navigationGeneration++;
    // Settings stay mounted behind a hidden div; remount so a draft for the
    // previous repository can never be saved into the newly selected one.
    const reopenSettings = view === 'settings';
    settingsVisited = false;
    if (reopenSettings) {
      await tick();
      settingsVisited = true;
    }
    selected = null;
    runPanel = null;
    panelOpener = null;
    listGeneration++;
    listRequest?.abort();
    stateSession.reset();
    data = null;
    filtered = [];
    proposals = [];
    prRows = [];
    proposalCounts = {};
    proposalCycle = 'all';
    queueTabCounts = {};
    listBefore = null;
    listNext = null;
    previousPages = [];
    listLoaded = false;
    listError = '';
    lastPage = '';
    error = '';
    connectionError = '';
    await refresh();
  }
  function disconnect() {
    sessionGeneration++;
    navigationGeneration++;
    connected = false;
    settingsVisited = false;
    data = null;
    repositories = [];
    selectedRepository = '';
    setRepository('');
    setToken('');
    selected = null;
    runPanel = null;
    panelOpener = null;
    listGeneration++;
    listRequest?.abort();
    stateSession.reset();
    listLoading = false;
    listLoaded = false;
    listError = '';
    lastPage = '';
    busy = false;
    pendingAction = '';
    filtered = [];
    proposals = [];
    prRows = [];
    proposalCounts = {};
    queueTabCounts = {};
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

<svelte:window
  onkeydown={onWindowKeydown}
  onhashchange={followLocation}
  onpopstate={followLocation}
/>
<svelte:head
  ><title>{documentTitle}</title><meta
    name="description"
    content="The private control room for continuous, reviewed project improvement."
  /></svelte:head
>
{#if !connected}
  <LoginScreen bind:accessToken {error} {busy} onsubmit={login} />
{:else if !data}
  <main class="repository-recovery">
    <h1>{connectionError ? 'Repository unavailable' : 'Loading repository'}</h1>
    <p>{connectionError || `Loading ${selectedRepository || 'your repository'}…`}</p>
    {#if repositories.length > 1}
      <label for="recovery-repository">Repository</label>
      <select
        id="recovery-repository"
        value={selectedRepository}
        onchange={(event) => switchRepository(event.currentTarget.value)}
      >
        {#each repositories as entry}
          <option value={entry.github_repo}>{entry.github_repo}</option>
        {/each}
      </select>
    {/if}
    <button onclick={() => refresh()}>Retry</button>
    <button onclick={disconnect}>Disconnect</button>
  </main>
{:else if data}
  <a class="skip-link" href="#main-content">Skip to main content</a>
  <div class="app-shell" class:nav-open={mobileOpen}>
    {#if mobileOpen}<div
        class="nav-scrim"
        role="presentation"
        aria-hidden="true"
        onclick={closeDrawer}
      ></div>{/if}
    <aside id="workspace-navigation" class:mobile-open={mobileOpen} class="sidebar">
      <a
        class="brand"
        href="#overview"
        onclick={(event) => {
          event.preventDefault();
          void navigate('overview');
        }}
        ><img src="/favicon.svg" alt="" width="35" height="35" /><span
          >octomus<span class="brand-light">agent</span></span
        ></a
      >
      <div class="repository-switch">
        <div class="repo-avatar"><Icon name="code" size={19} /></div>
        <div>
          <strong>{data.repository.split('/').pop() || 'Your repository'}</strong><small
            >{data.repository.split('/')[0] || 'Not connected yet'}</small
          >
          {#if repositories.length > 1}<select
              class="repository-select"
              aria-label="Repository"
              value={selectedRepository}
              disabled={busy}
              onchange={(event) => switchRepository(event.currentTarget.value)}
              >{#each repositories as entry}<option value={entry.github_repo}
                  >{entry.github_repo}</option
                >{/each}</select
            >{/if}
        </div>
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
              >{/if}{#if item.id === 'requests' && data.pending_requests > 0}<b
                >{data.pending_requests}</b
              >{/if}</button
          >{/each}
      </nav>
      <div class="sidebar-bottom">
        <button class="disconnect" onclick={disconnect}
          ><Icon name="logout" size={17} /><span>Disconnect</span><span class="version"
            >v{version}</span
          ></button
        >
      </div>
    </aside>
    <div class="main-shell">
      <header class="topbar">
        <div class="topbar-left">
          <button
            class="icon-button mobile-toggle"
            id="navigation-toggle"
            aria-label="Toggle navigation"
            aria-controls="workspace-navigation"
            aria-expanded={mobileOpen}
            onclick={toggleNavigation}><Icon name="menu" /></button
          ><img class="topbar-brand" src="/favicon.svg" alt="" width="26" height="26" />
          <div
            class="mode-pill"
            role="status"
            aria-label="Operating mode"
            aria-live="polite"
            data-tone={operatingTone(data)}
            title={operatingStatus(data)}
          >
            <span class="mode-dot"></span><span class="mode-text">{operatingStatus(data)}</span>
          </div>
        </div>
        <div class="topbar-right">
          <span class="live-indicator" class:offline={!!connectionError}
            ><span></span><span class="live-text"
              >{connectionError ? 'Reconnecting' : 'Connected'}</span
            ></span
          >{#if lastUpdated}<span class="topbar-updated">Updated {lastUpdated}</span>{/if}
        </div>
      </header>
      <main class="content" id="main-content" tabindex="-1" inert={mobileOpen}>
        <div class="page-heading">
          <h1>{current?.label}</h1>
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
        <Notices {data} bind:error {connectionError} onnavigate={navigate} />
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
            onselftest={sandboxSelfTest}
          />
        {:else if view === 'queue'}
          <section class="panel">
            <div class="list-toolbar" onfocusin={noteNavigationIntent}>
              <FilterTabs
                labels={QUEUE_FILTERS}
                current={filter}
                aria="Task filters"
                onselect={(state) => {
                  noteNavigationIntent();
                  filter = state;
                }}
                counts={queueTabCounts}
              />
              <SearchBox bind:value={search} />
            </div>
            {#if filtered.length}<TaskList
                tasks={filtered}
                onselect={inspectTask}
              />{:else if listLoaded}<div class="empty">
                <Icon name="queue" size={34} />
                <h3>{search || filter !== 'all' ? 'No matching tasks' : 'No tasks yet'}</h3>
                <p>
                  {search || filter !== 'all'
                    ? 'Try another filter or search term.'
                    : 'Accepted proposals become tasks.'}
                </p>
                {#if search || filter !== 'all'}<button class="button" onclick={clearListFilters}
                    >Clear search and filters</button
                  >{/if}
              </div>{/if}
          </section>
        {:else if view === 'prs'}
          {#if data.delivery_mode === 'maintenance'}<div class="notice">
              <Icon name="shield" size={18} /><span
                >Octomus publishes reviewed pull requests; small verified maintenance work can merge
                itself. Everything else stays manual.</span
              >
            </div>{/if}
          <section class="panel">
            <div class="section-heading">
              <h2>PR outcomes <span class="count">{prRows.length}</span></h2>
            </div>
            <div class="list-toolbar" onfocusin={noteNavigationIntent}>
              <FilterTabs
                labels={PR_FILTERS}
                current={filter}
                aria="PR filters"
                onselect={(state) => {
                  noteNavigationIntent();
                  filter = state;
                }}
              />
              <SearchBox bind:value={search} />
            </div>
            {#each prRows as observed (prKey(observed))}
              {@const pr = observed.pr}
              <a class="pr-row" href={safeUrl(pr.url)} target="_blank" rel="noreferrer"
                ><span class={'pr-icon ' + pr.state}><Icon name="prs" /></span>
                <div>
                  <h3>{pr.title}<span class="pr-number">#{pr.number}</span></h3>
                  <p>
                    <code>{pr.branch}</code><span>→</span><code>{pr.base}</code><span
                      class="pr-observed"
                      >· {pr.owned ? 'owned by Octomus' : 'not owned by Octomus'}</span
                    >{#if observed.observed_at}<span class="pr-observed"
                        >· observed <RelativeTime value={observed.observed_at} /></span
                      >{/if}
                  </p>
                  {#if observed.auto_merge}
                    {@const merge = observed.auto_merge}
                    <p class="pr-merge">
                      <span class="pr-observed"
                        >automatic merge: {merge.status}{merge.result_source === 'confirmed' &&
                        merge.status === 'merged'
                          ? ' — squash merged by Octomus'
                          : merge.result_source === 'observed' &&
                              (merge.status === 'merged' || merge.status === 'closed')
                            ? ` — ${merge.status} on GitHub (actor not confirmed)`
                            : ''}{merge.reason ? ` — ${merge.reason}` : ''}</span
                      >
                    </p>{/if}
                </div>
                <Badge
                  label={pr.state +
                    (observed.external_head_movement ? ' · external head change' : '')}
                  tone={pr.owned ? 'published' : 'queued'}
                /><Icon name="external" size={16} /></a
              >
            {/each}
            {#if !prRows.length && listLoaded}<div class="empty">
                <Icon name="prs" size={34} />
                <h3>
                  {search || filter !== 'all'
                    ? 'No matching pull requests'
                    : 'No pull requests yet'}
                </h3>
                <p>
                  {search || filter !== 'all'
                    ? 'Try another filter or search term.'
                    : 'Delivered tasks open pull requests here.'}
                </p>
                {#if search || filter !== 'all'}<button class="button" onclick={clearListFilters}
                    >Clear search and filters</button
                  >{/if}
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
        <Proposals
          active={view === 'proposals'}
          snapshots={data.cycles}
          rows={proposals}
          counts={proposalCounts}
          loaded={listLoaded}
          bind:search
          bind:filter={proposalFilter}
          bind:cycle={proposalCycle}
          {busy}
          {pendingAction}
          onaction={cycleAction}
          onrefresh={refresh}
          oninspect={inspectRun}
          onintent={noteNavigationIntent}
          navigationGeneration={() => navigationGeneration}
        />
        {#if view === 'requests'}
          <Requests
            active={view === 'requests'}
            {data}
            oninspectrun={inspectRun}
            onopentask={inspectTask}
            onnavigate={navigate}
          />
        {/if}
        {#if view === 'insights'}
          {#await insightsView() then { default: Insights }}
            <Insights active />
          {:catch}
            <div class="notice error" role="alert">
              <span>Could not load this view. Reload the page to get the current dashboard.</span>
            </div>
          {/await}
        {/if}
        {#if settingsVisited}<div hidden={view !== 'settings'}>
            {#await settingsView() then { default: Settings }}
              <Settings
                active={view === 'settings'}
                editable={configurationEditable(setupStatus)}
                status={setupStatus}
                onsaved={configSaved}
                onbaselinechanged={baselineChanged}
                onchoose={chooseOnOverview}
              />
            {:catch}
              <div class="notice error" role="alert">
                <span>Could not load this view. Reload the page to get the current dashboard.</span>
              </div>
            {/await}
          </div>{/if}
        {#if current?.noun}
          <div class="list-footer">
            {@render listFeedback(current.noun, filtered.length + proposals.length + prRows.length)}
            <div class="actions" aria-label="History pagination">
              <button
                class="button"
                disabled={listLoading || previousPages.length === 0}
                onclick={() => {
                  navigationGeneration++;
                  listBefore = previousPages.at(-1) ?? null;
                  previousPages = previousPages.slice(0, -1);
                }}>Previous page</button
              >
              <button
                class="button"
                disabled={listLoading || !!listError || listNext === null}
                onclick={() => {
                  navigationGeneration++;
                  previousPages = [...previousPages, listBefore];
                  listBefore = listNext;
                }}>Next page</button
              >
            </div>
          </div>
        {/if}
      </main>
    </div>
  </div>
  {#if selected}{#key selected}<TaskDetail
        id={selected}
        onselect={inspectTask}
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
      <button
        class="button"
        disabled={listLoading}
        onclick={() => {
          noteNavigationIntent();
          listRefresh++;
        }}>Retry</button
      >
    </div>{/if}
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
