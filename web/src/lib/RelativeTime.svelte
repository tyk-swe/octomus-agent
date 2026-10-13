<script lang="ts">
  import { exactTime, relative } from './format';
  import { currentTime } from './currentTime';
  let { value }: { value: string } = $props();
</script>

<time datetime={value} title={exactTime(value) || undefined}>{relative(value, $currentTime)}</time>
