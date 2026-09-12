<script lang="ts">
  import type { Config, ModelCatalog } from './types';
  import { applyAstraRehearsal, astraCatalog } from './astraRehearsal';

  let {
    config = $bindable(),
    catalog,
    editable,
    busy
  }: { config: Config; catalog?: ModelCatalog; editable: boolean; busy: boolean } = $props();
  let choice = $state<{ catalog?: ModelCatalog; binary: string; effort: string }>();
  let confirming = $state(false);
  let applied = $state(false);
  const availability = $derived(astraCatalog(config.codex_binary, catalog));
  const effort = $derived(
    choice?.catalog === catalog && choice?.binary === config.codex_binary
      ? choice.effort
      : !choice && availability.efforts.includes('medium')
        ? 'medium'
        : ''
  );
  const problem = $derived(
    !editable
      ? 'Pause the service and wait for active work to finish to apply this preset.'
      : busy
        ? 'Wait for the current configuration request to finish.'
        : availability.problem ||
          (!availability.efforts.includes(effort)
            ? 'Select a supported Astra effort to continue.'
            : '')
  );

  function apply() {
    if (!confirming || problem) return;
    const next = applyAstraRehearsal(config, catalog, effort);
    if (!next) return;
    config = next;
    confirming = false;
    applied = true;
  }
</script>

<div class="preset" role="group" aria-label="Astra rehearsal preset">
  <h3>Astra rehearsal</h3>
  <p>
    Apply to the unsaved draft, then Save, Check connection, and explicitly choose Run once. This
    preset does not set operating mode.
  </p>
  <label>
    Astra rehearsal effort
    <select
      value={effort}
      disabled={!!availability.problem || !editable || busy}
      onchange={(event) => {
        choice = { catalog, binary: config.codex_binary, effort: event.currentTarget.value };
        confirming = false;
        applied = false;
      }}
    >
      <option value="">Select supported effort</option>
      {#each availability.efforts as value}<option {value}>{value}</option>{/each}
    </select>
  </label>
  {#if problem}<p id="astra-preset-blocker">{problem}</p>{/if}
  <button
    type="button"
    class="button small"
    disabled={!!problem}
    aria-describedby={problem ? 'astra-preset-blocker' : undefined}
    onclick={() => {
      if (problem) return;
      confirming = true;
      applied = false;
    }}>Apply Astra rehearsal preset</button
  >
  {#if confirming}
    <div role="group" aria-label="Confirm Astra rehearsal preset">
      <p>Confirm these changes to the unsaved draft:</p>
      <ul>
        <li>
          Replace Orchestrator, Discovery agents, Proposal reviewers, Code reviewer, XS, S, M, L, XL
          execution, and Repair routes with Codex / gpt-6-astra / {effort || '(select effort)'}.
          Remove OpenCode provider and variant fields from those routes.
        </li>
        <li>Discovery agents: {config.discovery_agents} → 9</li>
        <li>Concurrent tasks: {config.execution_concurrency} → 1</li>
        <li>Tasks per cycle: {config.max_tasks_per_cycle} → 1</li>
        <li>Cycle interval (seconds): {config.cycle_interval_seconds} → 21600 (6 hours)</li>
      </ul>
      <p>Every other setting and unsaved verification command is preserved.</p>
      <div class="actions">
        <button type="button" class="button small" disabled={!!problem} onclick={apply}
          >Confirm preset</button
        >
        <button type="button" class="button small" onclick={() => (confirming = false)}
          >Cancel</button
        >
      </div>
    </div>
  {/if}
  {#if applied}<p role="status">
      Astra rehearsal preset applied to the unsaved draft. Save, Check connection, then explicitly
      choose Run once. Operating mode is unchanged.
    </p>{/if}
</div>

<style>
  .preset {
    margin: 20px 24px;
    font-size: 13px;
  }
  h3 {
    font-size: 13px;
  }
  label {
    display: grid;
    gap: 6px;
    max-width: 280px;
    margin-bottom: 12px;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
  }
  button,
  select {
    scroll-margin-block: 100px;
  }
</style>
