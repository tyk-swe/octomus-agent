<script lang="ts">
  import { exactTime, relative } from './format';
  let { value }: { value: string } = $props();
</script>

<time datetime={value} title={exactTime(value) || undefined}>{relative(value)}</time>
