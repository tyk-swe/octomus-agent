<script lang="ts">
  import type { Verdict } from './evidence';
  let { label, verdict }: { label: string; verdict: Verdict } = $props();
</script>

<div class="evidence-fact">
  <dt>{label}</dt>
  <dd>
    <span class={'badge ' + verdict.tone}>{verdict.label}</span><small>{verdict.detail}</small>
  </dd>
</div>
