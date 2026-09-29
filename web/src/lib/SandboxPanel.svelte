<script lang="ts">
  import Badge from './Badge.svelte';
  import Icon from './Icon.svelte';
  import { relative } from './api';
  import { bytesLabel, sandboxVerdict, shortImage } from './sandbox';
  import type { SandboxPosture } from './types';

  let {
    sandbox,
    busy,
    onselftest
  }: { sandbox: SandboxPosture; busy: boolean; onselftest: () => void } = $props();
  const verdict = $derived(sandboxVerdict(sandbox));
  const broker = $derived(sandbox.broker);
  const hosts = (kind: string) => sandbox.egress?.[kind] ?? [];
</script>

<section class="panel sandbox-panel" aria-labelledby="sandbox-heading">
  <div class="section-heading">
    <div class="row-title">
      <span class="section-icon"><Icon name="shield" /></span>
      <div>
        <h2 id="sandbox-heading">Sandbox</h2>
        <p>{verdict.detail}</p>
      </div>
    </div>
    <div class="row-title">
      <Badge label={verdict.label} tone={verdict.tone} />
      {#if sandbox.mode === 'docker'}<button
          class="button secondary small"
          disabled={busy || !sandbox.healthy}
          onclick={onselftest}><Icon name="shield" size={15} />Run self-test</button
        >{/if}
    </div>
  </div>
  {#if sandbox.mode === 'docker' && broker}
    <dl class="sandbox-facts">
      <div>
        <dt>Isolation</dt>
        <dd>
          One container per agent turn and per verification command, on {broker.runtime ||
            'the default runtime'}: non-root, no capabilities, read-only image, no route out except
          the egress gateway.
        </dd>
      </div>
      <div>
        <dt>Limits per sandbox</dt>
        <dd>
          {broker.limits.nano_cpus / 1e9} CPU · {bytesLabel(broker.limits.memory_bytes)} memory, no swap
          · {broker.limits.pids} processes · up to {broker.limits.max_sandboxes} at once ({broker.live}
          running)
        </dd>
      </div>
      <div>
        <dt>Image</dt>
        <dd><code>{broker.image}</code> · <code>{shortImage(broker.image_id)}</code></dd>
      </div>
      <div>
        <dt>Model hosts</dt>
        <dd>
          {hosts('model').join(', ') || 'None: agent sessions cannot reach a model provider.'}
        </dd>
      </div>
      <div>
        <dt>Build hosts</dt>
        <dd>{hosts('build').join(', ') || 'None: sandboxes reach no package registry.'}</dd>
      </div>
      {#if sandbox.pinned_repository}<div>
          <dt>Repository</dt>
          <dd>{sandbox.pinned_repository}, fixed by the deployment</dd>
        </div>{/if}
    </dl>
  {/if}
  {#if sandbox.mode === 'docker' && sandbox.self_test}
    {@const test = sandbox.self_test}
    <div class="sandbox-proof">
      <span class="eyebrow"
        >LAST PROVEN FROM INSIDE A SANDBOX · {relative(test.at).toUpperCase()}</span
      >
      {#if test.error}<div class="notice error">
          <Icon name="alert" size={18} /><span>{test.error}</span>
        </div>{:else}<ul aria-label="Containment checks">
          {#each test.checks as check (check.id)}<li class:failed={!check.passed}>
              <Icon name={check.passed ? 'check' : 'alert'} size={16} />
              <span>{check.label}</span>
              {#if !check.passed}<small>{check.detail}</small>{/if}
            </li>{/each}
        </ul>{/if}
    </div>
  {/if}
</section>
