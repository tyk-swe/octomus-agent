<script lang="ts">
  import { relative } from './api';
  import { taskIcon } from './evidence';
  import Icon from './Icon.svelte';
  import type { TaskRow } from './types';

  /** Task rows that each open the task's details; an empty list reads as all caught up. */
  let { tasks, onselect }: { tasks: TaskRow[]; onselect: (id: string) => void } = $props();
</script>

<div class="task-list">
  {#each tasks as task (task.id)}<button class="task-row" onclick={() => onselect(task.id)}
      ><span class={'task-type-icon ' + task.status}
        ><Icon name={taskIcon(task.status)} size={18} /></span
      ><span class="task-row-body"
        ><strong>{task.title}</strong><span
          ><span class="tier">{task.tier}</span><span>{task.category}</span><span
            class="dot-separator">·</span
          ><code>{task.target}</code><span class="dot-separator">·</span><span
            >updated {relative(task.updated_at)}</span
          ></span
        ></span
      ><span class={'badge ' + task.status}>{task.status}</span><Icon
        name="chevron"
        size={16}
      /></button
    >{:else}<div class="empty">
      <Icon name="check" size={30} />
      <h3>All caught up.</h3>
      <p>No tasks are waiting right now.</p>
    </div>{/each}
</div>
