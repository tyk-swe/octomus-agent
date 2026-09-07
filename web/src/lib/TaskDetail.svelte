<script lang="ts">
  import { onMount } from 'svelte';
  import { api, relative, safeUrl } from './api';
  import type { Task, Event } from './types';
  import Icon from './Icon.svelte';
  let { id, onclose, onaction }: { id: string; onclose: () => void; onaction: () => void } =
    $props();
  let dialog: HTMLDialogElement;
  let task = $state<Task | null>(null),
    error = $state(''),
    tab = $state('Overview'),
    busy = $state(false),
    events = $state<Event[]>([]);
  async function load() {
    try {
      [task, events] = await Promise.all([
        api<Task>(`/tasks/${id}`),
        api<Event[]>(`/events?entity=${encodeURIComponent(id)}`)
      ]);
    } catch (e) {
      error = (e as Error).message;
    }
  }
  onMount(() => {
    dialog.showModal();
    load();
    const timer = setInterval(load, 4000);
    return () => clearInterval(timer);
  });
  async function action(value: string) {
    busy = true;
    error = '';
    try {
      await api(`/tasks/${id}/${value}`, 'POST');
      await load();
      onaction();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

<dialog
  bind:this={dialog}
  class="task-dialog"
  aria-labelledby="task-title"
  onkeydown={(e) => {
    if (e.key === 'Escape') onclose();
  }}
>
  <div class="dialog-top">
    <span class="eyebrow">TASK DETAILS</span><button
      class="icon-button"
      aria-label="Close task details"
      onclick={onclose}><Icon name="close" /></button
    >
  </div>
  {#if error}<div class="notice error" role="alert">{error}</div>{/if}
  {#if task}
    <div class="task-title">
      <span class={'badge ' + task.status}>{task.status}</span>
      <h2 id="task-title">{task.proposal.title}</h2>
      <p>
        <span class="tier">{task.proposal.tier}</span>
        {task.route.model} <span class="dot-separator">·</span>
        {task.route.effort}
      </p>
    </div>
    <div class="tabs" role="tablist" aria-label="Task information">
      {#each ['Overview', 'Sessions', 'Reviews', 'Verification', 'Activity'] as name}<button
          role="tab"
          aria-selected={tab === name}
          class:active={tab === name}
          onclick={() => (tab = name)}
          >{name}{#if name === 'Reviews'}
            <span>{task.reviews.length}</span>{/if}</button
        >{/each}
    </div>
    <div class="detail-content" role="tabpanel" aria-label={tab}>
      {#if task.error}<div class="notice error">
          <Icon name="alert" size={18} /><span>{task.error}</span>
        </div>{/if}
      {#if tab === 'Overview'}
        <h3>The opportunity</h3>
        <p>{task.proposal.problem}</p>
        <p>{task.proposal.benefit}</p>
        <dl class="detail-grid">
          <div>
            <dt>Target</dt>
            <dd>{task.proposal.target}</dd>
          </div>
          <div>
            <dt>Branch</dt>
            <dd>{task.branch}</dd>
          </div>
          <div>
            <dt>Source revision</dt>
            <dd><code>{task.source_revision}</code></dd>
          </div>
          <div>
            <dt>Comparison base</dt>
            <dd><code>{task.comparison_base || 'Assigned at execution'}</code></dd>
          </div>
          <div class="full">
            <dt>Workspace</dt>
            <dd><code>{task.workspace || 'Created when execution starts'}</code></dd>
          </div>
          <div>
            <dt>Dependencies</dt>
            <dd>{task.proposal.dependencies.join(', ') || 'None'}</dd>
          </div>
          <div>
            <dt>Operator retries</dt>
            <dd>{task.attempts}</dd>
          </div>
        </dl>
        <h3>Execution prompt</h3>
        <pre class="prompt">{task.proposal.prompt}</pre>
        <h3>Scope & evidence</h3>
        <p>{task.proposal.scope}</p>
        {#each task.proposal.evidence as evidence}<p class="evidence">
            <Icon name="code" size={16} />{evidence}
          </p>{/each}
      {:else if tab === 'Sessions'}
        {#each task.sessions as session}<article class="history-card">
            <div class="row-between">
              <h3>{session.role}</h3>
              <span class={'badge ' + session.status}>{session.status}</span>
            </div>
            <p>{session.route.model} · {session.route.effort}</p>
            <code>{session.id}</code><small>{relative(session.started_at)}</small
            >{#if session.id === task.repair_session}<div class="inline-note">
                This repair context is reused across rounds.
              </div>{/if}{#if session.summary}<pre class="prompt">{session.summary}</pre>{/if}
          </article>{:else}<div class="empty">
            <Icon name="code" size={32} />
            <h3>No sessions yet</h3>
            <p>A separate Codex session starts when this task runs.</p>
          </div>{/each}
      {:else if tab === 'Reviews'}
        {#each task.reviews as review, index}<article class="history-card">
            <div class="row-between">
              <h3>Review {index + 1}</h3>
              <span class={'badge ' + (review.result.findings.length ? 'blocked' : 'published')}
                >{review.result.findings.length
                  ? `${review.result.findings.length} findings`
                  : 'Clean'}</span
              >
            </div>
            <p>{review.result.summary}</p>
            <small
              >Full change set · {review.comparison_base.slice(0, 8)} → {review.revision.slice(
                0,
                8
              )}</small
            >{#each review.result.findings as finding}<div class="finding">
                <span class="tier">{finding.priority}</span>
                <h4>{finding.title}</h4>
                <code>{finding.file}</code>
                <p>{finding.detail}</p>
              </div>{/each}
          </article>{:else}<div class="empty">
            <Icon name="shield" size={32} />
            <h3>Review is ahead</h3>
            <p>Every round starts with a fresh reviewer and the full change set.</p>
          </div>{/each}
      {:else if tab === 'Verification'}
        {#each task.verification as verification}<article class="history-card">
            <div class="row-between">
              <code>{verification.command}</code><span
                class={'badge ' + (verification.success ? 'published' : 'failed')}
                >{verification.success ? 'Passed' : 'Failed'}</span
              >
            </div>
            <small
              >Revision {verification.revision.slice(0, 8)} · {relative(
                verification.created_at
              )}</small
            >
            <pre>{verification.output || 'Command completed without output.'}</pre>
          </article>{:else}<div class="empty">
            <Icon name="check" size={32} />
            <h3>No verification results yet</h3>
            <p>All configured checks must pass on the reviewed revision.</p>
          </div>{/each}
      {:else}<div class="activity-list">
          {#each events as event}<div class="activity-item">
              <span class="activity-point"></span>
              <div>
                <p>{event.message}</p>
                <small>{event.kind.replaceAll('_', ' ')} · {relative(event.at)}</small>
              </div>
            </div>{:else}<p class="muted">No activity recorded yet.</p>{/each}
        </div>{/if}
    </div>
    <div class="dialog-footer">
      <span class="muted">Created {relative(task.created_at)}</span>
      <div class="actions">
        {#if task.pr_url}<a
            class="button primary"
            href={safeUrl(task.pr_url)}
            target="_blank"
            rel="noreferrer">Open PR #{task.pr_number}<Icon name="external" size={16} /></a
          >{/if}{#if ['blocked', 'failed'].includes(task.status)}<button
            class="button primary"
            disabled={busy}
            onclick={() => action('retry')}><Icon name="refresh" size={16} />Retry task</button
          >{/if}{#if !['published', 'cancelled', 'publishing'].includes(task.status)}<button
            class="button danger"
            disabled={busy}
            onclick={() => action('cancel')}>Cancel task</button
          >{/if}
      </div>
    </div>
  {:else}<div class="empty">
      <span class="spinner"></span>
      <p>Loading task…</p>
    </div>{/if}
</dialog>
