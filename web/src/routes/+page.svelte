<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { api, get, setToken, onUnauthorized } from '$lib/api';
  import type { Snapshot, TaskRow, Page, ProposalRow, PRObservation } from '$lib/types';
  import Badge from '$lib/Badge.svelte';
  import FilterTabs from '$lib/FilterTabs.svelte';
  import { clockTime, relative, safeUrl } from '$lib/format';
  import Icon from '$lib/Icon.svelte';
  import LoginScreen from '$lib/LoginScreen.svelte';
  import Notices from '$lib/Notices.svelte';
  import Overview from '$lib/Overview.svelte';
  import Proposals from '$lib/Proposals.svelte';
  import SearchBox from '$lib/SearchBox.svelte';
  import Settings from '$lib/Settings.svelte';
  import { planningBlocker, type SetupStatus } from '$lib/setup';
  import TaskDetail from '$lib/TaskDetail.svelte';
  import TaskList from '$lib/TaskList.svelte';
  import RunEvidence from '$lib/RunEvidence.svelte';
  import { NAVIGATION, PR_FILTERS, QUEUE_FILTERS, TOGGLE_PENDING_LABELS } from '$lib/navigation';
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
  let proposalsView = $state<ReturnType<typeof Proposals>>();
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
  let refreshing = false;
  let refreshQueued = false;
  let refreshRequest = Promise.resolve();
  let controlGeneration = 0;
  let controlStatePending = $state(false);
  let published = $derived(data?.tasks.filter((t) => t.status === 'published') ?? []);
  let attentionCount = $derived((data?.counts.blocked ?? 0) + (data?.counts.failed ?? 0));
  let latestCycle = $derived(data?.cycles[0]);
  type ControlAction = 'resume' | 'pause' | 'cycle' | 'audit';
  const planningBlocked = $derived(!!planningBlocker(data?.planning_capacity));
  const canControl = $derived({
    resume:
      !!data?.configured &&
      !data.recovery_error &&
      !controlStatePending &&
      data.active_cycle_mode !== 'audit' &&
      !data.baseline_active,
    pause: !!data?.configured && (!!data.recovery_error || data.active_cycle_mode !== 'audit'),
    cycle:
      !!data?.configured &&
      !data.recovery_error &&
      !controlStatePending &&
      !planningBlocked &&
      data.control.paused &&
      !data.cycle_active &&
      !data.active_tasks &&
      !data.baseline_active,
    audit:
      !!data?.audit_configured &&
      !data.recovery_error &&
      !controlStatePending &&
      !planningBlocked &&
      data.control.paused &&
      !data.cycle_active &&
      !data.active_tasks &&
      !data.baseline_active
  });
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
          latest: data.cycles[0] ?? null,
          planning_capacity: data.planning_capacity,
          control_state_pending: controlStatePending,
          recovery_error: data.recovery_error,
          sandbox: data.sandbox
        }
      : null
  );
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
      const page = await get<Page<TaskRow | ProposalRow | PRObservation>>(
        `/${endpoint}?${params}`,
        controller.signal
      );
      if (current !== listGeneration || controller.signal.aborted) return;
      if (view === 'queue') {
        filtered = page.items as TaskRow[];
        queueTabCounts = page.counts;
      }
      if (view === 'proposals') {
        proposals = page.items as ProposalRow[];
        proposalCounts = page.counts;
      }
      if (view === 'prs') prRows = page.items as PRObservation[];
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
    if (!connected) return Promise.resolve();
    if (refreshing) {
      refreshQueued = true;
      return refreshRequest;
    }
    const currentSession = sessionGeneration;
    refreshing = true;
    refreshRequest = (async () => {
      try {
        do {
          refreshQueued = false;
          const currentControl = controlGeneration;
          try {
            const snapshot = await api<Snapshot>('/state');
            if (currentSession !== sessionGeneration) return;
            // A poll started before a successful control cannot describe its result.
            if (currentControl !== controlGeneration) continue;
            data = snapshot;
            controlStatePending = false;
            if (!listLoading) listRefresh++;
            if (view === 'proposals') await proposalsView?.loadCycles();
            connectionError = '';
            lastUpdated = clockTime();
          } catch (e) {
            if (
              connected &&
              currentSession === sessionGeneration &&
              currentControl === controlGeneration
            )
              connectionError = (e as Error).message;
          }
        } while (currentSession === sessionGeneration && refreshQueued);
      } finally {
        if (currentSession === sessionGeneration) refreshing = false;
      }
    })();
    return refreshRequest;
  }
  function baselineChanged() {
    controlGeneration++;
    controlStatePending = true;
    // Accepted start/cancel owns baseline activity until a fresh state read
    // confirms the worker has finished, even when its panel is hidden.
    if (data) data.baseline_active = true;
    void refresh();
  }
  function configSaved() {
    // Readiness belongs to the newly saved configuration; an earlier poll
    // cannot confirm which work can start with it.
    controlGeneration++;
    controlStatePending = true;
    void refresh();
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
    navigationGeneration++;
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
        await proposalsView?.loadCycles();
      } catch (e) {
        if (currentSession === sessionGeneration) connectionError = (e as Error).message;
      }
    }
  }
  async function onWindowKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape' && mobileOpen) {
      event.preventDefault();
      noteNavigationIntent();
      mobileOpen = false;
      await tick();
      document.getElementById('navigation-toggle')?.focus();
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
      await api('/sandbox/self-test', 'POST');
      await refresh();
    } catch (e) {
      if (currentSession === sessionGeneration) error = (e as Error).message;
    } finally {
      busy = false;
      pendingAction = '';
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
      const result = await api<Snapshot['control']>(`/control/${action}`, 'POST');
      if (currentSession !== sessionGeneration) return;
      controlGeneration++;
      controlStatePending = true;
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
  function disconnect() {
    sessionGeneration++;
    navigationGeneration++;
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
    refreshRequest = Promise.resolve();
    controlStatePending = false;
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
        {:else if view === 'prs'}
          <div class="notice">
            <Icon name="shield" size={18} /><span
              >Octomus publishes reviewed pull requests. Merge decisions stay with you.</span
            >
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
                        >· observed {relative(observed.observed_at)}</span
                      >{/if}
                  </p>
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
        <Proposals
          active={view === 'proposals'}
          rows={proposals}
          counts={proposalCounts}
          loaded={listLoaded}
          bind:search
          bind:filter={proposalFilter}
          bind:cycle={proposalCycle}
          bind:busy
          bind:pendingAction
          bind:error
          bind:this={proposalsView}
          onrefresh={refresh}
          oninspect={inspectRun}
          onintent={noteNavigationIntent}
          navigationGeneration={() => navigationGeneration}
        />
        {#if settingsVisited}<div hidden={view !== 'settings'}>
            <Settings
              active={view === 'settings'}
              editable={data.control.paused &&
                !data.active_tasks &&
                !data.cycle_active &&
                !data.baseline_active}
              status={setupStatus}
              onsaved={configSaved}
              onbaselinechanged={baselineChanged}
              onchoose={chooseOnOverview}
            />
          </div>{/if}
        {#if current?.noun}
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
