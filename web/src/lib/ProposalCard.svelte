<script lang="ts">
  import Badge from './Badge.svelte';
  import { cycleLabel, decisionTone } from './evidence';
  import Icon from './Icon.svelte';
  import type { ProposalRow } from './types';

  /**
   * One proposal from the paged history. Opening its details asks the page to fetch the
   * full proposal; fetched detail is shown in place of the truncated polling summary.
   */
  let {
    proposal,
    onexpand,
    oninspect
  }: { proposal: ProposalRow; onexpand: () => void; oninspect: () => void } = $props();
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
    }}
  >
    <summary>Scope, evidence & execution prompt</summary>
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
