<script lang="ts">
  import type { Tone } from './evidence';
  let { label, tone }: { label: string; tone: Tone } = $props();
</script>

<span class={'badge ' + tone}>{label}</span>
