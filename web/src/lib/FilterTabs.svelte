<script lang="ts">
  /** Pressed-state buttons choosing one list filter; non-zero totals show beside a state. */
  let {
    labels,
    current,
    aria,
    onselect,
    counts = {}
  }: {
    labels: string[];
    current: string;
    aria: string;
    onselect: (state: string) => void;
    counts?: Record<string, number | undefined>;
  } = $props();
</script>

<div class="filter-tabs" role="group" aria-label={aria}>
  {#each labels as state}<button
      class:active={current === state}
      aria-pressed={current === state}
      onclick={() => onselect(state)}
      >{state}{#if counts[state]}<b class="tab-count" aria-hidden="true">{counts[state]}</b
        >{/if}</button
    >{/each}
</div>
