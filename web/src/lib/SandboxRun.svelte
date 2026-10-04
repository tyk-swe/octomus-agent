<script lang="ts">
  import Badge from './Badge.svelte';
  import { plural, shortHash } from './format';
  import type { SandboxRecord } from './types';

  let { record }: { record: SandboxRecord | null } = $props();
  const hosts = (counts: Record<string, number>) =>
    Object.entries(counts).sort(([a, x], [b, y]) => y - x || a.localeCompare(b));
</script>

{#if record}
  {@const denied = hosts(record.egress.denied)}
  {@const failed = hosts(record.egress.failed)}
  {@const allowed = hosts(record.egress.allowed)}
  <div class="sandbox-run" aria-label="Sandbox record">
    <p class="command-meta">
      Sandboxed · {plural(record.runs, 'container')} · image
      <code>{shortHash(record.image_id)}</code>{record.runtime
        ? ` · ${record.runtime}`
        : ''}{#if record.oom}
        · <strong class="sandbox-oom">memory limit killed a process</strong
        >{/if}{#if record.incomplete}
        · <strong
          class="sandbox-oom"
          title="The broker could not read all of this record: egress may be missing, and a memory-limit kill would not show."
          >record incomplete</strong
        >{/if}
    </p>
    {#if denied.length}<p class="sandbox-hosts">
        <span>Blocked egress</span>
        {#each denied as [host, count] (host)}<Badge
            label={`${host} ×${count}`}
            tone="failed"
          />{/each}
      </p>{/if}
    {#if failed.length}<p class="sandbox-hosts">
        <span>Unreachable egress</span>
        {#each failed as [host, count] (host)}<Badge
            label={`${host} ×${count}`}
            tone="blocked"
          />{/each}
      </p>{/if}
    {#if allowed.length}<p class="sandbox-hosts">
        <span>Reached</span>
        {#each allowed as [host, count] (host)}<Badge label={`${host} ×${count}`} />{/each}
      </p>{/if}
  </div>
{/if}
