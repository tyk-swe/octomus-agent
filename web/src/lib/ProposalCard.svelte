<script lang="ts">
  import Badge from './Badge.svelte';
  import { cycleLabel, decisionTone } from './evidence';
  import Icon from './Icon.svelte';
  import type { ProposalEntry } from './Proposals.svelte';

  let {
    proposal,
    onexpand,
    oninspect,
    oncollapse
  }: {
    proposal: ProposalEntry;
    onexpand: () => Promise<void>;
    oninspect: () => void;
    oncollapse?: () => void;
  } = $props();
  let summary = $state<HTMLElement>();
  let retryButton = $state<HTMLButtonElement>();

  $effect.pre(() => {
    if (
      proposal.detailError === undefined &&
      retryButton &&
      document.activeElement === retryButton &&
      summary?.isConnected
    )
      summary.focus();
  });

  function retry() {
    if (proposal.detailLoading !== undefined) return;
    void onexpand();
  }
</script>

<article class="proposal-card">
  <div class="row-between">
    <div class="proposal-meta">
      <Badge label={proposal.decision} tone={decisionTone(proposal.decision)} /><span
        >{cycleLabel({ mode: proposal.mode, number: proposal.cycle })}</span
      ><span class="tier">{proposal.tier}</span>
    </div>
    <span class="category">{proposal.category}</span>
  </div>
  <h2>{proposal.title}</h2>
  <p>{proposal.detail?.problem ?? proposal.problem}</p>
  <div class="decision-reason">
    <Icon name="shield" size={17} />
    <p>{proposal.detail?.reason ?? proposal.reason}</p>
  </div>
  <details
    ontoggle={(event) => {
      if (event.currentTarget.open) onexpand();
      else oncollapse?.();
    }}
  >
    <summary bind:this={summary}>Scope, evidence & execution prompt</summary>
    {#if proposal.detailLoading !== undefined}<p class="muted" role="status">
        Loading full proposal details…
      </p>{/if}
    {#if proposal.detailError !== undefined}<div class="notice error" role="alert">
        <span
          >Could not load full proposal details. Showing the summary. {proposal.detailError}</span
        >
      </div>
      <button
        bind:this={retryButton}
        class="button small"
        aria-disabled={proposal.detailLoading !== undefined}
        onclick={retry}
        >{proposal.detailLoading !== undefined ? 'Retrying details…' : 'Retry details'}</button
      >{/if}
    <p>{proposal.detail?.benefit ?? proposal.benefit}</p>
    <p>{proposal.detail?.scope ?? proposal.scope}</p>
    {#each proposal.detail?.evidence ?? proposal.evidence as item}<p class="evidence">
        {item}
      </p>{/each}
    <pre class="prompt">{proposal.detail?.prompt ?? proposal.prompt}</pre>
    <small
      >Dependencies: {(proposal.detail?.dependencies ?? proposal.dependencies).join(', ') ||
        'None'}</small
    >
  </details>
  <div class="proposal-target">
    <Icon name="branch" size={14} /><code>{proposal.target}</code>
    <button class="text-button" onclick={() => oninspect()}
      >Inspect decision evidence<Icon name="arrow" size={15} /></button
    >
  </div>
</article>
