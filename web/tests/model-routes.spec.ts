import { expect, test } from '@playwright/test';
import { client } from '../src/lib/api';
import {
  catalogForRoute,
  catalogsFromDoctor,
  checkRoute,
  failedCatalog,
  routeComplete
} from '../src/lib/modelRoutes';
import { routesStep } from '../src/lib/setup';
import type { Config, DoctorResponse, Model, ModelCatalog, Route } from '../src/lib/types';

const route: Route = { backend: 'codex', model: 'future/model', effort: 'high' };
const model: Model = {
  backend: 'codex',
  provider: null,
  provider_name: null,
  model: route.model,
  display_name: 'Future model',
  efforts: ['high'],
  variants: [],
  available: true,
  unavailable_reason: null
};
const catalog: ModelCatalog = { binary: '/fixture/codex', models: [model], loaded: true };
// These helpers use only the route collections and executable paths.
const config = {
  codex_binary: catalog.binary,
  opencode_binary: '/fixture/opencode',
  roles: Object.fromEntries(
    ['orchestrator', 'discovery', 'proposal_reviewer', 'code_reviewer'].map((role) => [role, route])
  ),
  tiers: Object.fromEntries(['XS', 'S', 'M', 'L', 'XL'].map((tier) => [tier, route])),
  repair_route: route
} as Config;

test('route completeness reports the missing control even without a catalog', () => {
  for (const [value, field] of [
    [undefined, 'model'],
    [{ ...route, model: ' ' }, 'model'],
    [{ ...route, effort: '' }, 'effort'],
    [{ backend: 'opencode', provider: '', model: 'one', effort: '' }, 'provider']
  ] as [Route | undefined, string][]) {
    expect(routeComplete(value)).toBe(false);
    expect(checkRoute(value)).toMatchObject({ state: 'incomplete', field });
  }
  expect(checkRoute(route)).toEqual({ state: 'unchecked', problem: '' });
  expect(routeComplete(route)).toBe(true);
});

test('catalog validation preserves unknown and unsupported selections', () => {
  const original = structuredClone(route);
  expect(checkRoute(route, catalog).state).toBe('valid');
  expect(checkRoute({ ...route, model: 'not-listed' }, catalog)).toMatchObject({
    state: 'invalid',
    field: 'model'
  });
  expect(checkRoute({ ...route, effort: 'max' }, catalog)).toMatchObject({
    state: 'invalid',
    field: 'effort'
  });
  const unavailable = { ...model, available: false, unavailable_reason: 'Account not connected' };
  expect(checkRoute(route, { ...catalog, models: [unavailable] })).toMatchObject({
    problem: 'Account not connected',
    state: 'invalid'
  });
  const otherBackend: Model = { ...model, backend: 'opencode' };
  expect(checkRoute(route, { ...catalog, models: [otherBackend] }).state).toBe('invalid');
  expect(route).toEqual(original);
});

test('OpenCode validation uses provider identity and native variants', () => {
  const open: Route = { backend: 'opencode', provider: 'one', model: 'shared/id', effort: '' };
  const choices: ModelCatalog = {
    binary: '/fixture/opencode',
    loaded: true,
    models: [
      {
        ...model,
        backend: 'opencode',
        provider: 'one',
        model: open.model,
        efforts: [],
        variants: ['careful']
      }
    ]
  };
  expect(checkRoute(open, choices).state).toBe('valid');
  expect(checkRoute({ ...open, variant: 'careful' }, choices).state).toBe('valid');
  expect(checkRoute({ ...open, variant: 'other' }, choices)).toMatchObject({
    state: 'invalid',
    field: 'variant'
  });
  expect(checkRoute({ ...open, provider: 'two' }, choices).state).toBe('invalid');
});

test('failed refreshes retain same-executable choices but not validation', () => {
  const stale = failedCatalog(catalog, catalog.binary, 'Refresh failed');
  expect(stale.models).toEqual(catalog.models);
  expect(stale).toMatchObject({ loaded: true, error: 'Refresh failed' });
  expect(checkRoute(route, stale)).toEqual({ state: 'unchecked', problem: '' });
  expect(catalog.error).toBeUndefined();
  const other = failedCatalog(catalog, '/other/codex', 'Refresh failed');
  expect(other).toEqual({
    binary: '/other/codex',
    models: [],
    loaded: false,
    error: 'Refresh failed'
  });
  expect(catalogForRoute(route, config, { codex: other })).toBeUndefined();
  expect(routesStep(config, config, { codex: stale }).detail).toContain('0 match a loaded catalog');
  expect(routesStep(config, config, { codex: other }).detail).toContain('0 match a loaded catalog');
  expect(routesStep(config, config, { codex: catalog }).detail).toContain(
    '10 match a loaded catalog'
  );
});

test('doctor catalogs use checked executable paths and update only inspected backends', () => {
  const opencode: ModelCatalog = { binary: config.opencode_binary, models: [], loaded: true };
  const previous = { codex: failedCatalog(catalog, catalog.binary, 'Old error'), opencode };
  const result: DoctorResponse = {
    ok: true,
    mode: 'audit',
    models: [model],
    backends: [
      { backend: 'codex', version: 'synthetic', protocol_version: 'synthetic', warning: null }
    ],
    warnings: [],
    message: 'Checked',
    sandbox: { mode: 'off' },
    checked_config: { ...config, codex_binary: '/checked/codex' },
    checked_revision: 'checked-revision'
  };
  const next = catalogsFromDoctor(previous, result);
  expect(next.codex).toEqual({ binary: '/checked/codex', models: [model], loaded: true });
  expect(next.opencode).toBe(opencode);
  expect(previous.codex.error).toBe('Old error');
  expect(catalogForRoute(route, config, next)).toBeUndefined();
  expect(catalogForRoute(route, result.checked_config, next)?.models).toEqual([model]);
});

test('diagnostics forward abort signals and reject late responses', async () => {
  const originalFetch = globalThis.fetch;
  try {
    for (const start of [
      (signal: AbortSignal) => client.catalog('codex', catalog.binary, signal),
      (signal: AbortSignal) => client.doctor('audit', signal)
    ]) {
      const observed: { signal?: AbortSignal } = {};
      let finish!: (value: string) => void;
      const body = new Promise<string>((resolve) => {
        finish = resolve;
      });
      globalThis.fetch = async (_url, options) => {
        observed.signal = options?.signal ?? undefined;
        const response = new Response('[]');
        response.text = () => body;
        return response;
      };
      const abort = new AbortController();
      const request = start(abort.signal);
      abort.abort();
      expect(observed.signal?.aborted).toBe(true);
      finish('[]');
      await expect(request).rejects.toThrow();
    }
  } finally {
    globalThis.fetch = originalFetch;
  }
});
