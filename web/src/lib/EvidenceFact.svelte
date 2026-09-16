<script lang="ts">
  import Badge from './Badge.svelte';
  import type { Verdict } from './evidence';
  let { label, verdict }: { label: string; verdict: Verdict } = $props();
</script>

<div class="evidence-fact">
  <dt>{label}</dt>
  <dd>
    <Badge label={verdict.label} tone={verdict.tone} /><small>{verdict.detail}</small>
  </dd>
</div>
