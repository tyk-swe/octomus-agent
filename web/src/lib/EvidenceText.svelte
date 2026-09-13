<script lang="ts">
  let { label, value }: { label: string; value: string } = $props();
</script>

{#if !value.trim()}
  <p class="muted">{label}: none recorded.</p>
{:else if value.length > 320}
  <div class="expandable">
    <p class="preview">{value.slice(0, 220)}…</p>
    <details>
      <summary>Show the full {label.toLowerCase()} ({value.length} characters)</summary>
      <p>{value}</p>
    </details>
  </div>
{:else}
  <p><strong>{label}.</strong> {value}</p>
{/if}

<style>
  p {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
