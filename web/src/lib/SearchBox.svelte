<script lang="ts">
  import Icon from './Icon.svelte';

  /**
   * The list search, bound to the page's query. The page focuses it on "/"; Escape
   * clears a query here before the key reaches anything else.
   */
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
