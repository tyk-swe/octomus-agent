import type { Backend, Route } from './types';

export const BACKENDS = ['codex', 'opencode'] as const;
export const backendLabel = (backend: Backend) => (backend === 'codex' ? 'Codex' : 'OpenCode');

export function routeLabel(route: Route): string {
  return route.backend === 'opencode'
    ? `OpenCode · ${route.provider ?? ''}/${route.model} · ${route.variant ?? 'Provider default'}`
    : `Codex · ${route.model} · ${route.effort}`;
}
