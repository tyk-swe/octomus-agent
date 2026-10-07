<script module lang="ts">
  import type { CycleSummary, ProposalDetail, ProposalRow } from './types';
  /** A proposal row with the full detail this view has requested for it. */
  export type ProposalEntry = ProposalRow & {
    detail?: ProposalDetail;
    detailRequested?: boolean;
    detailLoading?: number;
    detailError?: string;
  };
</script>

<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import { api } from './api';
  import { CycleHistory, type CycleHistoryState } from './cycleHistory';
  import Badge from './Badge.svelte';
  import { DECISIONS, cycleLabel, decisionTone } from './evidence';
  import FilterTabs from './FilterTabs.svelte';
  import Icon from './Icon.svelte';
  import ProposalCard from './ProposalCard.svelte';
  import RecoveryNotice from './RecoveryNotice.svelte';
  import SearchBox from './SearchBox.svelte';

  // The cycle history and requested proposal details outlive a visit: the page keeps this
  // component mounted and renders it while `active`.
  let {
    active,
    snapshots,
    rows,
    counts,
    loaded,
    search = $bindable(),
    filter = $bindable(),
    cycle = $bindable(),
    busy = $bindable(),
    pendingAction = $bindable(),
    error = $bindable(),
    onrefresh,
    oninspect,
    onintent,
    navigationGeneration
  }: {
    active: boolean;
    snapshots: CycleSummary[];
    rows: ProposalRow[];
    counts: Record<string, number>;
    loaded: boolean;
    search: string;
    filter: string;
    cycle: string;
    busy: boolean;
    pendingAction: string;
    error: string;
    onrefresh: () => Promise<void>;
    oninspect: (cycle: string, proposal: string) => void;
    /** An operator interaction, which drops a pending focus redirect; background loads must not. */
    onintent: () => void;
    /** The page's interaction count, read when the retry button takes focus and before redirecting it. */
    navigationGeneration: () => number;
  } = $props();
  const PROPOSAL_FILTERS = ['all', ...DECISIONS];
  let entries = $state<ProposalEntry[]>([]);
  let historyState = $state<CycleHistoryState>({
    rows: [],
    cursor: null,
    loading: false,
    error: ''
  });
  const history = new CycleHistory((state) => (historyState = state));
  let cycleRows = $derived(historyState.rows);
  let selectedCycle = $derived(cycle === 'all' ? undefined : cycleRows.find((c) => c.id === cycle));
  let cycleCursor = $derived(historyState.cursor);
  let cyclesLoading = $derived(historyState.loading);
  let refreshMessage = $state('');
  let refreshError = $derived(historyState.error);
  let retryButton = $state<HTMLButtonElement>();
  let cyclePicker = $state<HTMLSelectElement>();
  let retryNavigation = -1;
  let disposed = false;
  let tabCounts = $derived({
    all: Object.values(counts).reduce((n, v) => n + v, 0),
    ...counts
  } as Record<string, number | undefined>);
  onDestroy(() => {
    disposed = true;
    history.cancel();
  });
  $effect.pre(() => {
    if (
      !refreshMessage &&
      !refreshError &&
      !cyclesLoading &&
      active &&
      retryNavigation === navigationGeneration() &&
      retryButton &&
      document.activeElement === retryButton &&
      cyclePicker?.isConnected
    )
      cyclePicker.focus();
  });
  // The page loads the rows; a card's requested detail survives a refresh at the same content revision.
  $effect(() => {
    const next = rows;
    untrack(() => {
      entries = next.map((summary) => {
        const previous = entries.find(
          (p) => p.cycle_id === summary.cycle_id && p.id === summary.id
        );
        if (!previous) return summary;
        const detailChanged = previous.content_revision !== summary.content_revision;
        Object.assign(previous, summary);
        if (detailChanged) {
          previous.detail = undefined;
          previous.detailError = undefined;
          if (previous.detailRequested) void loadProposal(previous);
        }
        return previous;
      });
    });
  });
  $effect(() => {
    if (!active) {
      history.cancel();
      return;
    }
    const selected = cycle;
    untrack(() => {
      history.cancel();
      void loadCycles(selected);
    });
    return () => history.cancel();
  });
  $effect(() => {
    if (!active) return;
    void snapshots;
    untrack(() => void loadCycles(cycle));
  });
  async function loadCycles(selected = cycle) {
    if (!active || disposed) return;
    const completedAction = refreshMessage;
    const loaded = await history.refresh(selected);
    if (loaded && !disposed && completedAction && completedAction === refreshMessage)
      refreshMessage = '';
  }
  function loadOlderCycles() {
    onintent();
    void history.older();
  }
  async function loadProposal(p: ProposalEntry) {
    p.detailRequested = true;
    const revision = p.content_revision;
    if (p.detail || p.detailLoading === revision) return;
    p.detailLoading = revision;
    try {
      const detail = await api<ProposalDetail>(
        `/proposals/${encodeURIComponent(p.cycle_id)}/${encodeURIComponent(p.id)}`
      );
      if (!disposed && p.content_revision === revision && detail.content_revision === revision) {
        p.detail = detail;
        p.detailError = undefined;
      }
    } catch (e) {
      if (!disposed && p.content_revision === revision) p.detailError = (e as Error).message;
    } finally {
      if (p.detailLoading === revision) p.detailLoading = undefined;
    }
  }
  async function cycleAction(value: 'archive' | 'discard') {
    if (busy || refreshMessage) return;
    let applied = false;
    let ownsBusy = true;
    busy = true;
    pendingAction = value;
    error = '';
    try {
      await api(`/cycles/${encodeURIComponent(cycle)}/${value}`, 'POST');
      if (disposed) return;
      applied = true;
      refreshMessage = value === 'archive' ? 'Cycle archived.' : 'Cycle workspaces discarded.';
      // History recovery guards duplicate cycle actions independently. The
      // accepted mutation must leave running work pausable during its reads.
      busy = false;
      pendingAction = '';
      ownsBusy = false;
      history.cancel();
      void loadCycles();
      await onrefresh();
    } catch (e) {
      if (!disposed) {
        if (!applied) error = `Cycle action failed. ${(e as Error).message}`;
      }
    } finally {
      if (ownsBusy && !disposed) {
        busy = false;
        pendingAction = '';
      }
    }
  }
  function retryCycleHistory() {
    if (cyclesLoading) return;
    void loadCycles();
    void onrefresh();
  }
</script>

{#if active}
  <p class="muted">
    Audits record recommendations without queuing work. A later execution cycle plans afresh.
  </p>
  {#if refreshMessage || refreshError}<RecoveryNotice
      message={refreshMessage}
      error={refreshError}
      loading={cyclesLoading}
      label="cycle history"
      onretry={retryCycleHistory}
      onfocus={() => (retryNavigation = navigationGeneration())}
      bind:button={retryButton}
    />{/if}
  <div class="actions">
    {#if cyclesLoading && !cycleRows.length && !refreshMessage && !refreshError}
      <span class="muted" role="status">Loading cycle history…</span>
    {/if}
    {#if cycleCursor !== null}<button
        class="button"
        disabled={cyclesLoading}
        onclick={loadOlderCycles}>Load older cycles</button
      >{/if}
    {#if selectedCycle && selectedCycle.status !== 'running' && !selectedCycle.lifecycle.discarded_at}
      {#if !selectedCycle.lifecycle.archived_at}<button
          class="button"
          disabled={busy || !!refreshMessage}
          onclick={() => cycleAction('archive')}
          >{pendingAction === 'archive' ? 'Archiving cycle…' : 'Archive cycle'}</button
        >{:else if !selectedCycle.lifecycle.discarded_at}<button
          class="button danger"
          disabled={busy || !!refreshMessage}
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
      <select
        id="proposal-cycle"
        bind:this={cyclePicker}
        bind:value={cycle}
        onfocus={onintent}
        aria-busy={cyclesLoading}
      >
        <option value="all">All cycles</option>
        {#each cycleRows as row}<option value={row.id}
            >{cycleLabel(row)} · {row.status}{row.lifecycle.discarded_at
              ? ' · workspaces discarded'
              : row.lifecycle.archived_at
                ? ' · archived'
                : ''}</option
          >{/each}
      </select>
    </div>
    <div class="decision-counts" role="group" aria-label="Decision counts">
      {#each DECISIONS as decision}
        <Badge label={`${decision}: ${counts[decision] ?? 0}`} tone={decisionTone(decision)} />
      {/each}
    </div>
  </div>
  <section class="panel">
    <div class="list-toolbar" onfocusin={onintent}>
      <FilterTabs
        labels={PROPOSAL_FILTERS}
        current={filter}
        aria="Proposal filters"
        onselect={(state) => {
          onintent();
          filter = state;
        }}
        counts={tabCounts}
      />
      <SearchBox bind:value={search} />
    </div>
    <div class="proposal-list">
      {#each entries as p (JSON.stringify([p.cycle_id, p.id]))}<ProposalCard
          proposal={p}
          onexpand={() => {
            onintent();
            return loadProposal(p);
          }}
          oninspect={() => oninspect(p.cycle_id, p.id)}
          oncollapse={onintent}
        />{/each}
      {#if !entries.length && loaded}<div class="empty">
          <Icon name="proposals" size={34} />
          <h3>
            {search || filter !== 'all' || cycle !== 'all'
              ? 'No matching proposals'
              : 'Better ideas start with questions.'}
          </h3>
          <p>
            {search || filter !== 'all' || cycle !== 'all'
              ? 'Try another cycle, filter or search term.'
              : 'Discovery explores your project. Two adversarial reviewers challenge each proposal before the orchestrator decides.'}
          </p>
        </div>{/if}
    </div>
  </section>
{/if}
