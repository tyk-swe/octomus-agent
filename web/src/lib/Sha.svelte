<script lang="ts">
  import Icon from './Icon.svelte';
  import { shortHash } from './format';
  let {
    value,
    label,
    oncopy
  }: {
    value: string | null;
    label: string;
    oncopy?: (value: string, label: string) => void;
  } = $props();
</script>

{#if value}
  <span class="sha"
    ><code title={value}>{shortHash(value)}</code><span class="visually-hidden"
      >, full {label.toLowerCase()} {value}</span
    >{#if oncopy}<button
        type="button"
        class="icon-button"
        aria-label={`Copy full ${label.toLowerCase()}`}
        onclick={() => oncopy?.(value, label)}><Icon name="copy" size={13} /></button
      >{/if}</span
  >
{:else}
  <span class="sha-none">None recorded</span>
{/if}
