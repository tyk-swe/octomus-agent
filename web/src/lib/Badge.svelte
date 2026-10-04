<script lang="ts">
  import type { Tone } from './evidence';
  import type { TaskStatus } from './types';
  // A tone, a task or session status, or a decision: each has a `.badge.<tone>` style; '' is the plain badge.
  let { label, tone = '' }: { label: string; tone?: Tone | TaskStatus | string } = $props();
</script>

<span class={'badge ' + tone}>{label}</span>
