<script lang="ts">
  import Badge from './Badge.svelte';
  import { decisionCounts } from './evidence';
  let { decisions }: { decisions: Record<string, number> } = $props();
</script>

<ul class="outcome-counts" aria-label="Proposal decisions">
  {#each decisionCounts(decisions) as entry (entry.decision)}<li>
      <strong>{entry.count}</strong><Badge label={entry.decision} tone={entry.tone} />
    </li>{:else}<li class="muted">No decisions recorded</li>{/each}
</ul>
