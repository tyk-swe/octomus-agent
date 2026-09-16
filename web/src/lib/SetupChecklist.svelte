<script lang="ts">
  import type { Backend, Config, ModelCatalog } from './types';
  import {
    baselineStep,
    chooseStep,
    preflightStep,
    repositoryStep,
    routesStep,
    verificationStep,
    type Preflight,
    type SetupStatus,
    type SetupStep
  } from './setup';
  import Icon from './Icon.svelte';

  let {
    draft,
    saved,
    baseline,
    commands,
    dirty,
    catalogs,
    preflight,
    status,
    onfocus,
    onchoose
  }: {
    draft: Config;
    saved: Config | null;
    baseline: string;
    commands: string;
    dirty: boolean;
    catalogs: Partial<Record<Backend, ModelCatalog>>;
    preflight: Preflight | null;
    status: SetupStatus | null;
    /** Moves focus to an existing control; it never changes or saves a value. */
    onfocus: (target: string) => void;
    /** Hands off to the Overview controls; it never starts work. */
    onchoose: (action: 'audit' | 'cycle') => void;
  } = $props();
  // Tab-local only: collapsing the checklist is not persisted and starts nothing.
  let open = $state(true);
  const draftCommands = $derived(
    commands
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean)
  );
  type Link = { label: string; target?: string; choose?: 'audit' | 'cycle' };
  const steps = $derived<{ id: string; title: string; step: SetupStep; links: Link[] }[]>([
    {
      id: 'repository',
      title: 'Repository details',
      step: repositoryStep(draft, saved),
      links: [{ label: 'Edit repository details', target: 'repository-path' }]
    },
    {
      id: 'routes',
      title: 'Model routes',
      step: routesStep(draft, saved, catalogs),
      links: [
        { label: 'Load a runner catalog', target: 'load-codex-models' },
        { label: 'Edit routes', target: 'route-orchestrator' }
      ]
    },
    {
      id: 'verification',
      title: 'Verification policy',
      step: verificationStep(draftCommands, saved?.verification_commands ?? []),
      links: [{ label: 'Edit verification commands', target: 'verification-commands' }]
    },
    {
      id: 'preflight',
      title: 'Connection check',
      step: preflightStep(preflight, dirty, baseline),
      links: [
        { label: 'Open the execution check', target: 'check-connection' },
        { label: 'Open the audit check', target: 'check-audit-connection' }
      ]
    },
    {
      id: 'baseline',
      title: 'Clean baseline (optional)',
      step: baselineStep(status),
      links: [{ label: 'Open the baseline check', target: 'check-baseline' }]
    },
    {
      id: 'choose',
      title: 'Choose Audit or Run once',
      step: chooseStep(status),
      links: [
        { label: 'Audit on the Overview', choose: 'audit' },
        { label: 'Run once on the Overview', choose: 'cycle' }
      ]
    }
  ]);
  const progress = $derived(
    steps.filter((entry) => ['saved', 'checked', 'ran'].includes(entry.step.tone)).length
  );
</script>

<section class="panel setup" aria-labelledby="setup-heading">
  <div class="section-heading">
    <div>
      <h2 id="setup-heading">Setup checklist</h2>
      <p>
        {progress} of {steps.length} steps saved, checked or run. Entered means typed here; saved means
        sent to the service; checked means the saved configuration passed an explicit connection check;
        ran means a cycle actually executed.
      </p>
    </div>
    <button
      type="button"
      class="button small"
      aria-expanded={open}
      aria-controls="setup-steps"
      onclick={() => (open = !open)}>{open ? 'Hide checklist' : 'Show checklist'}</button
    >
  </div>
  {#if open}
    <ol id="setup-steps" class="steps">
      {#each steps as entry, index (entry.id)}
        <li class="step" data-step={entry.id} data-tone={entry.step.tone}>
          <span class="step-index" aria-hidden="true">{index + 1}</span>
          <div class="step-body">
            <div class="step-title">
              <h3>{entry.title}</h3>
              <span class={'badge tone-' + entry.step.tone}>{entry.step.label}</span>
            </div>
            <p>{entry.step.detail}</p>
            <div class="step-links">
              {#each entry.links as link (link.label)}
                <button
                  type="button"
                  class="text-button"
                  onclick={() => (link.choose ? onchoose(link.choose) : onfocus(link.target!))}
                  >{link.label}<Icon name="arrow" size={14} /></button
                >
              {/each}
            </div>
          </div>
        </li>
      {/each}
    </ol>
    <p class="setup-footnote">
      Nothing here starts work. Populated fields, catalog matches and a passed check do not prove
      repository push permission or model inference; only a run's recorded evidence does.
    </p>
  {/if}
</section>

<style>
  .setup {
    margin-bottom: 20px;
  }
  .steps {
    list-style: none;
    margin: 0;
    padding: 0 24px;
    display: grid;
    gap: 14px;
  }
  .step {
    display: flex;
    gap: 14px;
    padding-top: 14px;
    border-top: 1px solid var(--line);
  }
  .step-index {
    flex-shrink: 0;
    width: 26px;
    height: 26px;
    border-radius: 50%;
    border: 1px solid var(--line);
    background: #f5f6f2;
    color: #5c694f;
    font-size: 12px;
    font-weight: 650;
    display: grid;
    place-items: center;
  }
  .step[data-tone='saved'] .step-index,
  .step[data-tone='checked'] .step-index,
  .step[data-tone='ran'] .step-index {
    background: #eef5e9;
    border-color: #e0ecd6;
    color: #476e48;
  }
  .step-body {
    min-width: 0;
    flex: 1;
  }
  .step-title {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 12px;
    align-items: center;
  }
  h3 {
    font-size: 14px;
    margin: 0;
  }
  .step-body > p {
    margin: 6px 0 0;
    font-size: 12px;
    color: var(--muted);
    overflow-wrap: anywhere;
  }
  .step-links {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 18px;
    margin-top: 6px;
  }
  .badge.tone-draft {
    background: #fbf3e6;
    color: #835d29;
    border-color: #f0e4ce;
  }
  .badge.tone-saved,
  .badge.tone-checked,
  .badge.tone-ran {
    color: #476e48;
    background: #eef5e9;
    border-color: #e0ecd6;
  }
  .badge.tone-failed {
    background: #fbefeb;
    color: #915441;
    border-color: #eeddd5;
  }
  .setup-footnote {
    margin: 16px 24px 20px;
    font-size: 12px;
    color: var(--muted);
  }
</style>
