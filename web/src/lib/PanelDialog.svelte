<script lang="ts">
  import { onMount, type Snippet } from 'svelte';
  import Icon from './Icon.svelte';
  let {
    eyebrow,
    closeLabel,
    labelledby,
    class: className = '',
    onclose,
    children
  }: {
    eyebrow: string;
    closeLabel: string;
    labelledby: string;
    class?: string;
    onclose: () => void;
    children: Snippet;
  } = $props();
  let dialog: HTMLDialogElement;
  onMount(() => {
    dialog.showModal();
  });
</script>

<dialog
  bind:this={dialog}
  class={['task-dialog', className]}
  aria-labelledby={labelledby}
  oncancel={(e) => {
    e.preventDefault();
    onclose();
  }}
>
  <div class="dialog-top">
    <span class="eyebrow">{eyebrow}</span><button
      class="icon-button"
      aria-label={closeLabel}
      onclick={onclose}><Icon name="close" /></button
    >
  </div>
  {@render children()}
</dialog>
