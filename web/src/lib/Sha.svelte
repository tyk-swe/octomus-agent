<!--
  A commit identity: the short form is what people scan, the full SHA stays available to
  assistive technology (and on hover), and an optional copy action carries the full value.
-->
<script lang="ts">
  import Icon from './Icon.svelte';
  import { shortCommit } from './evidence';
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
    ><code title={value}>{shortCommit(value)}</code><span class="visually-hidden"
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
