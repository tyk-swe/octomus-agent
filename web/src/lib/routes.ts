import type { Backend, Route } from './types';

/** Every runner, in the order the dashboard lists them. */
export const BACKENDS = ['codex', 'opencode'] as const;
/** A runner's display name, spelled as config.Backend.Display spells it. */
export const backendLabel = (backend: Backend) => (backend === 'codex' ? 'Codex' : 'OpenCode');

export function routeLabel(route: Route): string {
  return route.backend === 'opencode'
    ? `OpenCode · ${route.provider ?? ''}/${route.model} · ${route.variant ?? 'Provider default'}`
    : `Codex · ${route.model} · ${route.effort}`;
}
