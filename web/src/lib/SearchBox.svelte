<script lang="ts">
  import Icon from './Icon.svelte';

  let { value = $bindable('') }: { value: string } = $props();
  let input: HTMLInputElement;

  function clear() {
    value = '';
    input.focus();
  }
</script>

<label class="search-box"
  ><Icon name="search" size={17} /><input
    bind:value
    bind:this={input}
    placeholder="Search…"
    aria-label="Search work"
    aria-keyshortcuts="/"
    onkeydown={(event) => {
      if (event.key === 'Escape' && value) {
        value = '';
        event.stopPropagation();
      }
    }}
  />{#if value}<button class="icon-button" aria-label="Clear search" onclick={clear}
      ><Icon name="close" size={14} /></button
    >{:else}<kbd class="search-hint" aria-hidden="true">/</kbd>{/if}</label
>
