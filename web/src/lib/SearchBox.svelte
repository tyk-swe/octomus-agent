<script lang="ts">
  import Icon from './Icon.svelte';

  let { value = $bindable('') }: { value: string } = $props();
</script>

<label class="search-box"
  ><Icon name="search" size={17} /><input
    bind:value
    placeholder="Search…"
    aria-label="Search work"
    aria-keyshortcuts="/"
    onkeydown={(event) => {
      if (event.key === 'Escape' && value) {
        value = '';
        event.stopPropagation();
      }
    }}
  />{#if value}<button class="icon-button" aria-label="Clear search" onclick={() => (value = '')}
      ><Icon name="close" size={14} /></button
    >{:else}<kbd class="search-hint" aria-hidden="true">/</kbd>{/if}</label
>
