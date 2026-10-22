<script lang="ts">
  import { onDestroy } from 'svelte';
  import { client } from './api';
  import { Resource } from './resource';
  import Badge from './Badge.svelte';
  import { decisionTone } from './evidence';
  import Icon from './Icon.svelte';
  import RelativeTime from './RelativeTime.svelte';
  import type { OperatorRequest, RequestsView, Snapshot } from './types';

  let {
    active,
    data,
    oninspectrun,
    onopentask,
    onnavigate
  }: {
    active: boolean;
    data: Snapshot;
    oninspectrun: (cycle: string, proposal: string | null) => void;
    onopentask: (id: string) => void;
    onnavigate: (id: string) => void;
  } = $props();

  let view = $state<RequestsView | null>(null),
    loadError = $state(''),
    loading = $state(false),
    actionError = $state(''),
    message = $state(''),
    pendingAction = $state('');
  let title = $state(''),
    description = $state(''),
    target = $state('');

  const resource = new Resource(
    (_key: string, signal) => client.requests(signal),
    (state) => {
      view = state.value;
      loadError = state.error;
      loading = state.loading;
    }
  );

  $effect(() => {
    // A successful state poll can reflect new PR inventory without changing request counts.
    void data;
    if (!active) return;
    void resource.refresh('requests');
  });
  onDestroy(() => resource.dispose());

  $effect(() => {
    const branch = view?.default_branch ?? '';
    if (branch && target !== branch && !view?.targets.some((option) => option.branch === target)) {
      target = branch;
    }
  });

  const planning = $derived(data.cycle_active || data.active_cycle_mode !== null);
  const atLimit = $derived(
    view !== null && view.repository !== '' && view.pending.length >= view.pending_limit
  );
  const configured = $derived(view !== null && view.repository !== '');
  const canSubmit = $derived(
    configured &&
      !atLimit &&
      pendingAction === '' &&
      title.trim() !== '' &&
      description.trim() !== '' &&
      target !== ''
  );

  async function submit() {
    pendingAction = 'create';
    actionError = '';
    message = '';
    try {
      await client.createRequest({
        title: title.trim(),
        description: description.trim(),
        target
      });
      title = '';
      description = '';
      target = view?.default_branch ?? '';
      message = 'Request saved. The next planning cycle will assess it.';
      await resource.refresh('requests', true);
    } catch (error) {
      actionError = (error as Error).message;
    } finally {
      pendingAction = '';
    }
  }

  async function withdraw(request: OperatorRequest) {
    pendingAction = `withdraw-${request.id}`;
    actionError = '';
    message = '';
    try {
      await client.requestAction(request.id, 'withdraw');
      await resource.refresh('requests', true);
    } catch (error) {
      actionError = (error as Error).message;
    } finally {
      pendingAction = '';
    }
  }
</script>

{#if active}
  <section class="panel" aria-labelledby="request-work-heading">
    <div class="section-heading">
      <h2 id="request-work-heading">Request work</h2>
    </div>
    {#if loading && view === null}
      <p class="muted">Loading requests…</p>
    {:else if view === null && loadError !== ''}
      <div class="notice error" role="alert"><span>Could not load requests. {loadError}</span></div>
    {:else if view !== null}
      {#if loadError !== ''}
        <div class="notice error" role="alert">
          <span>Could not refresh requests. Showing the last loaded requests. {loadError}</span>
        </div>
      {/if}
      {#if !configured}
        <div class="notice">
          <span
            >Save a GitHub repository before requesting work.
            <button class="text-button" onclick={() => onnavigate('settings')}>Open settings</button
            ></span
          >
        </div>
      {:else}
        <form
          onsubmit={(event) => {
            event.preventDefault();
            void submit();
          }}
        >
          <div class="form-grid">
            <label
              >Title<input
                id="request-title"
                bind:value={title}
                maxlength="200"
                required
                autocomplete="off"
              /></label
            >
            <label class="full"
              >What should change<textarea
                id="request-description"
                bind:value={description}
                rows="5"
                required></textarea></label
            >
            <label
              >Target<select id="request-target" bind:value={target}>
                <option value={view.default_branch}
                  >New pull request on {view.default_branch}</option
                >
                {#each view.targets as option (option.branch)}
                  <option value={option.branch}
                    >#{option.number} {option.title} ({option.branch})</option
                  >
                {/each}
              </select></label
            >
          </div>
          <div class="submit-row">
            <button class="button" type="submit" disabled={!canSubmit}
              >{pendingAction === 'create' ? 'Requesting…' : 'Request work'}</button
            >
          </div>
          <small
            >At most {view.seed_limit} requests are assessed per cycle, oldest first. A request still
            faces both adversarial reviewers and consolidation.</small
          >
          {#if atLimit}
            <p class="muted">
              {view.pending_limit} requests are already waiting; withdraw one or let a cycle assess them.
            </p>
          {/if}
        </form>
      {/if}
      {#if actionError !== ''}
        <div class="notice error" role="alert"><span>{actionError}</span></div>
      {/if}
      {#if message !== ''}
        <div class="notice" role="status"><span>{message}</span></div>
      {/if}
    {/if}
  </section>

  <section class="panel" aria-labelledby="pending-requests-heading">
    <div class="section-heading">
      <h2 id="pending-requests-heading">Waiting for planning</h2>
      {#if view !== null && view.pending.length > 0}<span>{view.pending.length}</span>{/if}
    </div>
    {#if view !== null && view.pending.length === 0}
      <div class="empty"><p>No requests are waiting. Ask for the work that matters.</p></div>
    {:else if view !== null}
      {#each view.pending as request (request.id)}
        <article class="proposal-card">
          <div class="row-between">
            <h2>{request.title}</h2>
            <code>{request.target}</code>
          </div>
          <p class="request-description">{request.description}</p>
          {#if request.assessment}
            <div class="request-assessment">
              <Badge
                label={request.assessment.decision}
                tone={decisionTone(request.assessment.decision)}
              />
              {#if request.assessment.mode === 'audit'}<span class="muted">audit preview</span>{/if}
              <p>{request.assessment.reason}</p>
              <button
                class="text-button"
                onclick={() =>
                  oninspectrun(request.assessment!.cycle_id, request.assessment!.proposal_id)}
                >Inspect run<Icon name="arrow" size={15} /></button
              >
            </div>
          {/if}
          <div class="request-meta">
            <span>Requested <RelativeTime value={request.created_at} /></span>
            <button
              class="button small"
              disabled={planning || pendingAction !== ''}
              onclick={() => void withdraw(request)}
              >{pendingAction === `withdraw-${request.id}` ? 'Withdrawing…' : 'Withdraw'}</button
            >
          </div>
          {#if planning}<p class="muted">Withdraw is available once planning finishes.</p>{/if}
        </article>
      {/each}
    {/if}
  </section>

  <section class="panel" aria-labelledby="recent-requests-heading">
    <div class="section-heading">
      <h2 id="recent-requests-heading">Recent decisions</h2>
    </div>
    {#if view !== null && view.recent.length === 0}
      <div class="empty"><p>No request decisions yet.</p></div>
    {:else if view !== null}
      {#each view.recent as request (request.id)}
        <article class="proposal-card">
          <div class="row-between">
            <div class="proposal-meta">
              <Badge label={request.status} tone={decisionTone(request.status)} />
              <h2>{request.title}</h2>
            </div>
            <code>{request.target}</code>
          </div>
          {#if request.assessment}
            <div class="request-assessment">
              <p>{request.assessment.reason}</p>
              <button
                class="text-button"
                onclick={() =>
                  oninspectrun(request.assessment!.cycle_id, request.assessment!.proposal_id)}
                >Inspect run<Icon name="arrow" size={15} /></button
              >
            </div>
          {/if}
          {#if request.task_id}
            <button class="text-button" onclick={() => onopentask(request.task_id!)}
              >Open task<Icon name="arrow" size={15} /></button
            >
          {/if}
        </article>
      {/each}
    {/if}
  </section>
{/if}

<style>
  .request-description {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .request-assessment {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
    font-size: 13px;
  }
  .request-assessment p {
    flex: 1;
    min-width: 240px;
    margin: 0;
  }
  .request-meta {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    font-size: 12px;
    color: var(--muted, #5c6950);
  }
  .submit-row {
    margin: 14px 0 8px;
  }
</style>
