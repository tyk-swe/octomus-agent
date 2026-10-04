<script lang="ts">
  import Icon from './Icon.svelte';
  // An accepted action's result while its view refreshes, and the retry when that refresh fails.
  let {
    message,
    error,
    loading,
    label,
    waiting,
    icon = true,
    onretry,
    onfocus,
    button = $bindable()
  }: {
    message: string;
    error: string;
    loading: boolean;
    /** What is refreshing, as in 'task details': names the waiting, failed and retry texts. */
    label: string;
    waiting?: string;
    icon?: boolean;
    onretry: () => void;
    onfocus?: () => void;
    button?: HTMLButtonElement;
  } = $props();
  const failed = $derived(
    `${label[0].toUpperCase()}${label.slice(1)} could not be refreshed. ${error}`
  );
</script>

<div class="notice" class:error={!!error} role={error ? 'alert' : 'status'}>
  {#if icon}<Icon name={error ? 'alert' : 'refresh'} size={18} />{/if}<span
    >{message}
    {error ? failed : (waiting ?? `Refreshing ${label}…`)}</span
  >
  {#if error}<button
      bind:this={button}
      class="button small"
      aria-disabled={loading}
      {onfocus}
      onclick={onretry}>{loading ? `Retrying ${label}…` : `Retry ${label}`}</button
    >{/if}
</div>
