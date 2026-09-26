<script lang="ts">
  import type { Backend, ModelCatalog, Route } from './types';
  import { backendLabel } from './routes';

  let {
    name,
    route = $bindable(),
    catalog,
    anchor,
    disabled = false
  }: {
    name: string;
    route: Route;
    catalog?: ModelCatalog;
    anchor?: string;
    /** True while the saved route is a display-only preview pending replacement. */
    disabled?: boolean;
  } = $props();
  const id = $props.id();
  const backend = $derived(route.backend ?? 'codex');
  const models = $derived(
    (catalog?.models ?? []).filter((model) => model.provider === (route.provider ?? null))
  );
  const selected = $derived(models.find((model) => model.model === route.model));
  const providers = $derived([
    ...new Map(
      (catalog?.models ?? [])
        .filter((model) => model.provider)
        .map((model) => [model.provider!, model.provider_name ?? model.provider!])
    ).entries()
  ]);
  const efforts = $derived(selected?.efforts ?? []);
  const variants = $derived(selected?.variants ?? []);
  const problem = $derived(
    catalog?.loaded && route.model
      ? !selected
        ? 'This model is not in the loaded catalog.'
        : !selected.available
          ? selected.unavailable_reason
          : backend === 'codex' && route.effort && !efforts.includes(route.effort)
            ? 'The saved effort is not supported by this model.'
            : backend === 'opencode' && route.variant && !variants.includes(route.variant)
              ? 'The saved variant is not supported by this model.'
              : ''
      : ''
  );

  function changeBackend(value: Backend) {
    route = {
      backend: value,
      model: '',
      effort: '',
      ...(value === 'opencode' ? { provider: '' } : {})
    };
  }
  function changeModel(value: string) {
    if (value === route.model) return;
    route.model = value;
    // An ID that begins a longer catalog ID may be a step on the way to it, so its
    // choices are checked when the field is committed instead.
    if (!models.some((model) => model.model !== value && model.model.startsWith(value)))
      pruneChoices(value);
  }
  /**
   * Only a catalog entry for the model proves a choice unsupported; an unknown or
   * partially typed model keeps the current choice and `problem` flags it.
   */
  function pruneChoices(value: string) {
    const model = models.find((model) => model.model === value);
    if (!model) return;
    if (route.effort && !model.efforts.includes(route.effort)) route.effort = '';
    if (route.variant && !model.variants.includes(route.variant)) route.variant = undefined;
  }
</script>

<div class="model-route" id={anchor} role="group" aria-label={name + ' route'}>
  <h3>{name}</h3>
  <div class="route-fields" class:opencode={backend === 'opencode'}>
    <label
      >Runner
      <select
        aria-label={name + ' runner'}
        value={backend}
        {disabled}
        onchange={(event) => changeBackend(event.currentTarget.value as Backend)}
      >
        <option value="codex">Codex</option>
        <option value="opencode">OpenCode</option>
      </select>
    </label>
    {#if backend === 'opencode'}
      <label
        >Provider
        <select
          aria-label={name + ' provider'}
          {disabled}
          value={route.provider ?? ''}
          onchange={(event) => {
            route = {
              ...route,
              provider: event.currentTarget.value,
              model: '',
              effort: '',
              variant: undefined
            };
          }}
        >
          <option value="">Select provider</option>
          {#if route.provider && !providers.some(([id]) => id === route.provider)}<option
              value={route.provider}>{route.provider} (saved)</option
            >{/if}
          {#each providers as [id, label]}<option value={id}>{label}</option>{/each}
        </select>
      </label>
    {/if}
    <label
      >Model
      <input
        aria-label={name + ' model'}
        list={id + '-models'}
        {disabled}
        value={route.model}
        oninput={(event) => changeModel(event.currentTarget.value)}
        onchange={(event) => pruneChoices(event.currentTarget.value)}
        placeholder="Search or enter model ID"
        aria-describedby={problem ? id + '-problem' : undefined}
      />
      <datalist id={id + '-models'}>
        {#each models as model}<option value={model.model}
            >{model.display_name}{model.available ? '' : ' (unavailable)'}</option
          >{/each}
      </datalist>
    </label>
    {#if backend === 'codex'}
      <label
        >Reasoning effort
        <select aria-label={name + ' reasoning effort'} {disabled} bind:value={route.effort}>
          <option value="">Select effort</option>
          {#if route.effort && !efforts.includes(route.effort)}<option value={route.effort}
              >{route.effort} (current)</option
            >{/if}
          {#each efforts as effort}<option value={effort}>{effort}</option>{/each}
        </select>
      </label>
    {:else}
      <label
        >Variant
        <select
          aria-label={name + ' variant'}
          {disabled}
          value={route.variant ?? ''}
          onchange={(event) => {
            route.variant = event.currentTarget.value || undefined;
          }}
        >
          <option value="">Provider default</option>
          {#if route.variant && !variants.includes(route.variant)}<option value={route.variant}
              >{route.variant} (current)</option
            >{/if}
          {#each variants as variant}<option value={variant}>{variant}</option>{/each}
        </select>
      </label>
    {/if}
  </div>
  {#if problem}<p id={id + '-problem'} class="route-problem">
      {problem} The saved route will not be substituted.
    </p>{/if}
  {#if !catalog?.loaded}<p class="route-help">
      {catalog?.error
        ? 'Catalog unavailable. Saved values are preserved.'
        : `Load ${backendLabel(backend)} models to see available choices.`}
    </p>{/if}
</div>

<style>
  .model-route {
    margin: 0 24px;
    padding: 18px 0;
    border-top: 1px solid var(--line);
  }
  h3 {
    margin: 0 0 12px;
  }
  .route-fields {
    display: grid;
    grid-template-columns: minmax(120px, 1fr) minmax(150px, 2fr) minmax(120px, 1fr);
    gap: 12px;
  }
  .route-fields.opencode {
    grid-template-columns: minmax(110px, 1fr) minmax(120px, 1fr) minmax(150px, 2fr) minmax(
        120px,
        1fr
      );
  }
  label {
    display: grid;
    gap: 6px;
    font-size: 12px;
    min-width: 0;
  }
  input,
  select {
    min-width: 0;
  }
  .route-help,
  .route-problem {
    font-size: 12px;
    margin: 10px 0 0;
  }
  .route-help {
    color: var(--muted);
  }
  .route-problem {
    color: var(--bad-fg);
  }
  @media (max-width: 1100px) {
    .route-fields,
    .route-fields.opencode {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 600px) {
    .route-fields,
    .route-fields.opencode {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
