import type { Route } from './types';

export function routeLabel(route: Route): string {
  return route.backend === 'opencode'
    ? `OpenCode · ${route.provider ?? ''}/${route.model} · ${route.variant ?? 'Provider default'}`
    : `Codex · ${route.model} · ${route.effort}`;
}
