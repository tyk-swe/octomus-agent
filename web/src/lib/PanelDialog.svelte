<!--
  The modal shell of the task and run evidence panels. It opens as a modal on mount, and
  every close request goes through the page's panel state rather than closing it here.
-->
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
    /** Id of the panel's heading, rendered by the panel itself. */
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
    // Every close request, wherever focus is, closes through the page's panel state.
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
