<script lang="ts">
  import { relative, safeUrl } from './api';
  import Icon from './Icon.svelte';
  import type { PrObservation } from './types';

  /** One observed pull request, linking out to GitHub, with its ownership and head movement. */
  let { observed }: { observed: PrObservation } = $props();
  const pr = $derived(observed.pr);
</script>

<a class="pr-row" href={safeUrl(pr.url)} target="_blank" rel="noreferrer"
  ><span class={'pr-icon ' + pr.state}><Icon name="prs" /></span>
  <div>
    <h3>{pr.title}<span class="pr-number">#{pr.number}</span></h3>
    <p>
      <code>{pr.branch}</code><span>→</span><code>{pr.base}</code><span class="pr-observed"
        >· {pr.owned ? 'owned by Octomus' : 'not owned by Octomus'}</span
      >{#if observed.observed_at}<span class="pr-observed"
          >· observed {relative(observed.observed_at)}</span
        >{/if}
    </p>
  </div>
  <span class={'badge ' + (pr.owned ? 'published' : 'queued')}
    >{pr.state}{observed.external_head_movement ? ' · external head change' : ''}</span
  ><Icon name="external" size={16} /></a
>
