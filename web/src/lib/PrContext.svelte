<script lang="ts">
  import { relative, safeUrl } from './api';
  import type { Cycle } from './types';
  let { grounding }: { grounding: Cycle['grounding'] } = $props();
  let external = $derived(grounding?.external_prs ?? []);
  let coverage = $derived(grounding?.pr_coverage);
</script>

{#if grounding}
  <section class="evidence-section" aria-labelledby="external-prs-heading">
    <div class="row-between">
      <h3 id="external-prs-heading">External pull requests observed</h3>
    </div>
    <p class="muted">
      Read-only context for overlap review — never execution or maintenance targets.
      {#if coverage?.complete}
        {coverage.total_external} external of {coverage.total_open} open ·
        {coverage.included_external} included{coverage.omitted_external > 0
          ? ` · ${coverage.omitted_external} omitted by context limits`
          : ''}{#if coverage.observed_at}
          · observed {relative(coverage.observed_at)}{/if}
      {:else}
        Coverage was not recorded for this run.
      {/if}
    </p>
    {#if external.length > 0}
      <ul>
        {#each external as pr (pr.number)}
          <li>
            <a href={safeUrl(pr.url)} target="_blank" rel="noreferrer"
              >#{pr.number} {pr.title}{pr.title_truncated ? '…' : ''}</a
            >
            <span class="muted">
              <code>{pr.head_repository || 'deleted repository'}:{pr.branch}</code>
              <code>{pr.head.slice(0, 7)}</code> → {pr.base}{pr.body_truncated
                ? ' · body truncated in context'
                : ''}
            </span>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}
