<script lang="ts">
  import Icon from './Icon.svelte';
  let {
    value,
    label,
    oncopy
  }: {
    value: string;
    label: string;
    oncopy: (value: string, label: string) => void;
  } = $props();
</script>

<span class="copy-text"
  ><code>{value}</code><button
    type="button"
    class="icon-button"
    aria-label={`Copy ${label.toLowerCase()}`}
    onclick={() => oncopy(value, label)}><Icon name="copy" size={13} /></button
  ></span
>
