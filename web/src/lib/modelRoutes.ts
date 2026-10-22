import type { Backend, Config, DoctorResponse, ModelCatalog, Route } from './types';

export const BACKENDS = ['codex', 'opencode'] as const;
export const backendLabel = (backend: Backend) => (backend === 'codex' ? 'Codex' : 'OpenCode');

type Catalogs = Partial<Record<Backend, ModelCatalog>>;
type RouteCheck = {
  state: 'incomplete' | 'unchecked' | 'invalid' | 'valid';
  problem: string;
  field?: 'provider' | 'model' | 'effort' | 'variant';
};

export function catalogForRoute(route: Route, config: Config, catalogs: Catalogs) {
  const backend = route.backend ?? 'codex';
  const catalog = catalogs[backend];
  return catalog?.binary === config[`${backend}_binary`] ? catalog : undefined;
}

export function checkRoute(route: Route | undefined, catalog?: ModelCatalog): RouteCheck {
  const backend = route?.backend ?? 'codex';
  if (backend === 'opencode' && !route?.provider?.trim())
    return { state: 'incomplete', problem: 'Select an OpenCode provider.', field: 'provider' };
  if (!route?.model.trim())
    return { state: 'incomplete', problem: 'Enter a model ID.', field: 'model' };
  if (backend === 'codex' && !route.effort.trim())
    return { state: 'incomplete', problem: 'Select a reasoning effort.', field: 'effort' };
  if (!catalog?.loaded || catalog.error) return { state: 'unchecked', problem: '' };
  const model = catalog.models.find(
    (entry) =>
      entry.backend === backend &&
      entry.model === route.model &&
      (entry.provider ?? null) === (route.provider ?? null)
  );
  if (!model)
    return {
      state: 'invalid',
      problem: 'This model is not in the loaded catalog.',
      field: 'model'
    };
  if (!model.available)
    return {
      state: 'invalid',
      problem: model.unavailable_reason || 'This model is unavailable.',
      field: 'model'
    };
  if (backend === 'codex' && !model.efforts.includes(route.effort))
    return {
      state: 'invalid',
      problem: 'The selected effort is not supported by this model.',
      field: 'effort'
    };
  if (backend === 'opencode' && route.variant && !model.variants.includes(route.variant))
    return {
      state: 'invalid',
      problem: 'The selected variant is not supported by this model.',
      field: 'variant'
    };
  return { state: 'valid', problem: '' };
}

export const routeComplete = (route: Route | undefined) => checkRoute(route).state !== 'incomplete';

export function failedCatalog(
  previous: ModelCatalog | undefined,
  binary: string,
  error: string
): ModelCatalog {
  return previous?.loaded && previous.binary === binary
    ? { ...previous, error }
    : { binary, models: [], loaded: false, error };
}

export function catalogsFromDoctor(catalogs: Catalogs, result: DoctorResponse): Catalogs {
  const next = { ...catalogs };
  for (const { backend } of result.backends) {
    next[backend] = {
      binary: result.checked_config[`${backend}_binary`],
      models: result.models.filter((model) => model.backend === backend),
      loaded: true
    };
  }
  return next;
}

export function routeLabel(route: Route): string {
  return route.backend === 'opencode'
    ? `OpenCode · ${route.provider ?? ''}/${route.model} · ${route.variant ?? 'Provider default'}`
    : `Codex · ${route.model} · ${route.effort}`;
}
