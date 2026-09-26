import { createHash } from 'node:crypto';
import { test, expect, type Locator, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import type {
  BaselineView,
  Config,
  Model,
  PrObservation,
  SettingsView,
  Snapshot,
  Task,
  TaskRow,
  TransformedField
} from '../src/lib/types';
import { login, openNavigation, token, trackWrites } from './synthetic';

// A poll can still be inside a route handler when a test ends; closing the page then
// disposes its response. That teardown error says nothing about the test's result.
test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

// The fixture revision mirrors the service contract: a content hash of the
// canonical configuration. Key order and display transforms never change it;
// any saved value change does.
function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  if (value && typeof value === 'object')
    return `{${Object.keys(value as Record<string, unknown>)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonical((value as Record<string, unknown>)[key])}`)
      .join(',')}}`;
  return JSON.stringify(value);
}
const revisionOf = (config: Config | null) =>
  createHash('sha256').update(canonical(config)).digest('hex');
// The service may serialize the same configuration with its keys in any order; this copy
// reverses or sorts every object's keys, nested routes included.
function reorderKeys<T>(value: T, order: 'reverse' | 'sort'): T {
  return JSON.parse(
    JSON.stringify(value, (_key, member) =>
      member && typeof member === 'object' && !Array.isArray(member)
        ? Object.fromEntries(
            order === 'sort'
              ? Object.entries(member).sort(([a], [b]) => a.localeCompare(b))
              : Object.entries(member).reverse()
          )
        : member
    )
  );
}
const navigatorFor = (page: Page, isMobile: boolean) => (name: string) =>
  openNavigation(page, name, isMobile);

// All writes terminate in browser fixtures; neither runner nor GitHub is contacted.
async function configurationFixture(
  page: Page,
  options: {
    unconfigured?: boolean;
    snapshot?: (snapshot: Snapshot) => void;
    /** Display-only field values plus their transform metadata, as the server reports them. */
    transformed?: { overrides: Partial<Config>; fields: TransformedField[] };
    /** Canonical values the fixture reports as already saved. */
    saved?: Partial<Config>;
  } = {}
) {
  const state = {
    saved: null as Config | null,
    /** Display-only replacements for transformed fields; canonical values stay in `saved`. */
    overrides: {} as Record<string, unknown>,
    transformed: [] as TransformedField[],
    reads: 0,
    writes: [] as { expected_revision: string; config: Record<string, unknown> }[],
    catalogs: [] as { backend: string; binary: string }[],
    checks: [] as string[],
    failLoad: false,
    failSave: false,
    failCheck: false,
    loadGate: null as ReturnType<typeof deferred> | null,
    saveGate: null as ReturnType<typeof deferred> | null,
    checkGate: null as ReturnType<typeof deferred> | null,
    baselineView: {
      check: null,
      eligible: true,
      reason: null,
      config_matches: null,
      config_revision: null,
      revision_status: 'unknown',
      default_observation: null,
      caveat: 'Synthetic baseline caveat'
    } as BaselineView,
    baselines: [] as unknown[],
    revision() {
      return revisionOf(this.saved);
    }
  };
  const viewOf = (saved: Config): SettingsView => ({
    config: { ...structuredClone(saved), ...structuredClone(state.overrides) } as Config,
    revision: revisionOf(saved),
    transformed_fields: structuredClone(state.transformed)
  });
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.control.paused = true;
    snapshot.control.mode = 'paused';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = false;
    snapshot.active_cycle_mode = null;
    options.snapshot?.(snapshot);
    await route.fulfill({ json: snapshot });
  });
  await page.route('**/api/config', async (route) => {
    if (route.request().method() === 'PUT') {
      const body = route.request().postDataJSON() as {
        expected_revision: string;
        config: Record<string, unknown>;
      };
      state.writes.push(body);
      await state.saveGate?.promise;
      // `failSave` stands for the service's other conflicts (no longer paused, tasks to
      // resolve first), which a reload does not fix; only a stale revision asks for one.
      if (state.failSave) {
        await route.fulfill({ status: 409, json: { error: 'Synthetic save conflict' } });
        return;
      }
      if (body.expected_revision !== state.revision()) {
        await route.fulfill({
          status: 409,
          json: { error: 'Synthetic save conflict; reload settings and check the current values.' }
        });
        return;
      }
      // Each supplied top-level field replaces its canonical value; omitted
      // fields keep theirs, including any hidden display values.
      for (const [key, value] of Object.entries(body.config)) {
        (state.saved as unknown as Record<string, unknown>)[key] = value;
        delete state.overrides[key];
      }
      state.transformed = state.transformed.filter((entry) => !(entry.field in body.config));
      await route.fulfill({ json: viewOf(state.saved!) });
      return;
    }
    state.reads++;
    if (!state.saved) {
      const response = await route.fetch();
      const initial = ((await response.json()) as SettingsView).config;
      const model = options.unconfigured
        ? { backend: 'codex' as const, model: '', effort: '' }
        : { backend: 'codex' as const, model: 'gpt-6-astra', effort: 'medium' };
      state.saved = {
        ...initial,
        repository: options.unconfigured ? '' : '/fixture/repository',
        github_repo: options.unconfigured ? '' : 'fixture/project',
        default_branch: 'fixture-main',
        codex_binary: '/fixture/codex',
        opencode_binary: '/fixture/opencode',
        verification_commands: options.unconfigured ? [] : ['fixture saved test'],
        roles: Object.fromEntries(Object.keys(initial.roles).map((key) => [key, { ...model }])),
        tiers: Object.fromEntries(Object.keys(initial.tiers).map((key) => [key, { ...model }])),
        repair_route: { ...model },
        ...options.saved
      };
      if (options.transformed) {
        state.overrides = { ...options.transformed.overrides };
        state.transformed = [...options.transformed.fields];
      }
    }
    // Snapshot before a delayed refresh so edits can race a real stale response.
    const view = viewOf(structuredClone(state.saved!));
    await state.loadGate?.promise;
    await route.fulfill(
      state.failLoad
        ? { status: 503, json: { error: 'Synthetic configuration unavailable' } }
        : { json: view }
    );
  });
  await page.route('**/api/model-catalog', async (route) => {
    state.catalogs.push(route.request().postDataJSON());
    await route.fulfill({
      json: [
        {
          backend: 'codex',
          provider: null,
          provider_name: null,
          model: 'gpt-6-astra',
          display_name: 'Astra',
          efforts: ['medium', 'high'],
          variants: [],
          available: true,
          unavailable_reason: null
        }
      ] satisfies Model[]
    });
  });
  await page.route('**/api/baseline-checks/latest', async (route) => {
    await route.fulfill({ json: state.baselineView });
  });
  await page.route('**/api/baseline-checks/*/cancel', async (route) => {
    state.baselines.push({ cancel: route.request().url() });
    if (state.baselineView.check) {
      state.baselineView = {
        ...state.baselineView,
        check: {
          ...state.baselineView.check,
          status: 'cancelled',
          completed_at: new Date().toISOString()
        }
      };
    }
    await route.fulfill({ json: { ok: true } });
  });
  await page.route('**/api/baseline-checks', async (route) => {
    const body = route.request().postDataJSON() as { expected_revision: string };
    state.baselines.push(body);
    if (body.expected_revision !== state.revision()) {
      await route.fulfill({ status: 409, json: { error: 'Synthetic baseline conflict' } });
      return;
    }
    // The admitted check snapshots the canonical configuration and its revision.
    state.baselineView = {
      ...state.baselineView,
      config_revision: state.revision(),
      check: {
        id: 'synthetic-check',
        status: 'running',
        config: structuredClone(state.saved!),
        config_fingerprint: state.revision(),
        revision: null,
        started_at: new Date().toISOString(),
        completed_at: null,
        commands: [],
        error: null,
        workspace_removed: false,
        cleanup_error: null
      }
    };
    await route.fulfill({ status: 202, json: state.baselineView.check });
  });
  await page.route('**/api/doctor?*', async (route) => {
    state.checks.push(new URL(route.request().url()).searchParams.get('mode')!);
    // The diagnostic is attributed to the canonical revision it ran against.
    const checked_config = structuredClone(state.saved);
    const checked_revision = state.revision();
    await state.checkGate?.promise;
    await route.fulfill(
      state.failCheck
        ? {
            status: 400,
            json: {
              error: 'Synthetic connection check failed',
              checked_config,
              checked_revision
            }
          }
        : {
            json: {
              message: 'Synthetic saved configuration checked',
              checked_config,
              checked_revision
            }
          }
    );
  });
  return state;
}

test('baseline refresh preserves server staleness and rejects obsolete responses', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  const navigate = navigatorFor(page, !!isMobile);
  await login(page);
  await navigate('Configuration');
  await expect(page.locator('#check-baseline')).toBeEnabled();
  state.baselineView = {
    ...state.baselineView,
    config_matches: false,
    check: {
      id: 'current-baseline',
      status: 'passed',
      config: structuredClone(state.saved!),
      config_fingerprint: 'synthetic',
      revision: 'a'.repeat(40),
      started_at: new Date().toISOString(),
      completed_at: new Date().toISOString(),
      commands: [],
      error: null,
      workspace_removed: true,
      cleanup_error: null
    }
  };
  await expect(
    page.getByText('Configuration changed since this check', { exact: true })
  ).toBeVisible({ timeout: 10000 });
  await expect(page.getByText('Matches the saved configuration', { exact: true })).toHaveCount(0);
  const gate = deferred();
  let requests = 0;
  await page.route('**/api/baseline-checks/latest', async (route) => {
    const number = ++requests;
    const response = structuredClone(state.baselineView);
    if (number === 1) {
      response.check!.error = 'Obsolete baseline response';
      await gate.promise;
    }
    await route.fulfill({ json: response }).catch(() => {});
  });
  await expect.poll(() => requests, { timeout: 10000 }).toBe(1);
  await page.waitForTimeout(4500);
  expect(requests).toBe(1);
  await navigate('Task queue');
  gate.resolve();
  await navigate('Configuration');
  await expect.poll(() => requests).toBeGreaterThan(1);
  await expect(
    page.getByText('Configuration changed since this check', { exact: true })
  ).toBeVisible();
  await expect(page.getByText('Obsolete baseline response')).toHaveCount(0);
  expect(state.baselines).toHaveLength(0);
});

test('opening Configuration reads the baseline status once, and a new saved revision reads it again at once', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  // Each read is held open until released, so a second read can only come from something
  // that starts one alongside it: polling waits for the read in flight.
  let gate = deferred();
  let reads = 0;
  await page.route('**/api/baseline-checks/latest', async (route) => {
    reads++;
    await gate.promise;
    await route.fulfill({ json: state.baselineView }).catch(() => {});
  });
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect.poll(() => reads).toBe(1);
  await page.waitForTimeout(1000);
  expect(reads).toBe(1);
  // Later reads wait on a fresh gate, set before this one opens, so the next poll's read
  // is held however soon it starts; saving a change must not wait for it.
  const first = gate;
  gate = deferred();
  first.resolve();
  await expect(page.locator('#check-baseline')).toBeEnabled();
  await expect.poll(() => reads, { timeout: 10000 }).toBe(2);
  await page.getByLabel('Default branch', { exact: true }).fill('baseline-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect.poll(() => reads).toBe(3);
  gate.resolve();
  await expect(page.locator('#check-baseline')).toBeEnabled();
  expect(state.baselines).toHaveLength(0);
});

test('the baseline confirmation takes focus, and Back returns it to the button that opened it', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const open = page.locator('#check-baseline');
  await expect(open).toBeEnabled();
  await open.focus();
  await page.keyboard.press('Enter');
  // The confirmation replaces the focused button; focus moves into it, never to the page.
  const dialog = page.getByRole('alertdialog', { name: 'Confirm baseline check' });
  const run = dialog.getByRole('button', { name: 'Run baseline check' });
  await expect(run).toBeFocused();
  await expect(dialog).toHaveAccessibleDescription(
    /^Run the saved verification commands on a disposable clone/
  );
  const accessibility = await new AxeBuilder({ page })
    .include('[role="alertdialog"]')
    .withTags(['wcag2a', 'wcag2aa'])
    .analyze();
  expect(
    accessibility.violations.map((v) => ({ rule: v.id, elements: v.nodes.map((n) => n.target) }))
  ).toEqual([]);
  await page.keyboard.press('Tab');
  await expect(dialog.getByRole('button', { name: 'Back' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(dialog).toHaveCount(0);
  await expect(open).toBeFocused();

  // Closing because the operator started an edit leaves focus on the field being edited.
  await open.click();
  await expect(run).toBeFocused();
  const branch = page.getByLabel('Default branch', { exact: true });
  await branch.fill('edited-main');
  await expect(dialog).toHaveCount(0);
  await expect(open).toBeDisabled();
  await expect(branch).toBeFocused();
  await page.keyboard.type('-next');
  await expect(branch).toHaveValue('edited-main-next');
  expect(state.baselines).toHaveLength(0);
  expect(state.writes).toHaveLength(0);
});

test('the baseline panel never denies a recorded check while its status loads or is unavailable', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  const navigate = navigatorFor(page, !!isMobile);
  await login(page);
  await navigate('Configuration');
  const panel = page.getByRole('region', { name: 'Clean baseline' });
  const never = panel.getByText('No baseline check has been run.', { exact: true });
  await expect(never).toBeVisible();
  state.baselineView = {
    ...state.baselineView,
    check: {
      id: 'failed-baseline',
      status: 'failed',
      config: structuredClone(state.saved!),
      config_fingerprint: state.revision(),
      revision: 'a'.repeat(40),
      started_at: new Date().toISOString(),
      completed_at: new Date().toISOString(),
      commands: [],
      error: 'Synthetic baseline failure',
      workspace_removed: true,
      cleanup_error: null
    }
  };
  await navigate('Overview');
  const gate = deferred();
  let fail = true;
  await page.route('**/api/baseline-checks/latest', async (route) => {
    await gate.promise;
    await route
      .fulfill(
        fail
          ? { status: 503, json: { error: 'Synthetic baseline status outage' } }
          : { json: state.baselineView }
      )
      .catch(() => {});
  });
  await navigate('Configuration');
  await expect(panel.getByText('Loading baseline status…', { exact: true })).toBeVisible();
  await expect(never).toHaveCount(0);
  gate.resolve();
  await expect(panel.getByRole('alert')).toHaveText('Synthetic baseline status outage');
  await expect(panel.getByText('Baseline status unavailable.', { exact: true })).toBeVisible();
  await expect(never).toHaveCount(0);
  fail = false;
  await expect(panel.getByText('Synthetic baseline failure', { exact: true })).toBeVisible({
    timeout: 10000
  });
  await expect(panel.getByText('Baseline status unavailable.', { exact: true })).toHaveCount(0);
  await expect(never).toHaveCount(0);

  // An unsaved edit is the reason the check is unavailable, and the panel says so.
  const unsaved = panel.getByText('Save or discard edits before checking the baseline.');
  await expect(unsaved).toHaveCount(0);
  await page.getByLabel('Default branch', { exact: true }).fill('unsaved-main');
  await expect(unsaved).toBeVisible();
  await expect(page.locator('#check-baseline')).toBeDisabled();
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(unsaved).toHaveCount(0);
  await expect(page.locator('#check-baseline')).toBeEnabled();
  expect(state.baselines).toHaveLength(0);
});

test('notification health and PR limits remain read-only observations', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page, {
    snapshot: (snapshot) => {
      snapshot.notifications = {
        state: 'enabled',
        configured: true,
        pending: 2,
        failed: 1,
        last_delivered_at: null,
        last_error: 'http_status',
        last_http_status: 503
      };
      snapshot.pr_capacity = {
        limit: 5,
        owned_open: 4,
        reserved: 1,
        remaining: 0,
        observed_at: new Date().toISOString(),
        status: 'full',
        reason: 'Capacity full'
      };
    }
  });
  await login(page);
  await expect(page.getByText(/Open-PR capacity is full/)).toBeVisible();
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(page.getByRole('heading', { name: 'Attention notifications' })).toBeVisible();
  await expect(page.getByText('http_status (HTTP 503)', { exact: true })).toBeVisible();
  await expect(page.getByLabel(/^Open PR capacity/)).toHaveValue('5');
  expect(state.writes).toHaveLength(0);
  expect(state.baselines).toHaveLength(0);
});

test('a refreshing PR inventory names its earlier failure, and runner storage names each status', async ({
  page
}) => {
  const writes = trackWrites(page);
  await page.route('**/api/state', async (route) => {
    const snapshot: Snapshot = await (await route.fetch()).json();
    // The service retries a failed inventory refresh and says why in the reason.
    snapshot.pr_capacity = {
      limit: 5,
      owned_open: null,
      reserved: 0,
      remaining: null,
      observed_at: null,
      status: 'refreshing',
      reason: 'Refreshing the open-PR inventory after a failure: synthetic auth error'
    };
    snapshot.storage = {
      measured_at: new Date().toISOString(),
      application_bytes: 2_000_000_000,
      task_bytes: 1_000_000_000,
      planning_bytes: 500_000_000,
      runner_transcripts: {
        bytes: null,
        status: 'unavailable',
        message: 'Runner storage reported separately',
        runners: {
          codex: { bytes: null, status: 'unconfigured' },
          opencode: { bytes: null, status: 'error' }
        }
      }
    };
    await route.fulfill({ json: snapshot });
  });
  await login(page);
  await expect(
    page.getByText(
      'Refreshing the open-PR inventory after a failure: synthetic auth error. New-PR work waits until the refresh completes.'
    )
  ).toBeVisible();
  await page.locator('.operating-details > summary').click();
  const details = page.locator('.operating-summary');
  await expect(details.getByText('codex storage: not configured', { exact: true })).toBeVisible();
  await expect(
    details.getByText('opencode storage: measurement error', { exact: true })
  ).toBeVisible();
  await expect(details.getByText(/storage: Unavailable/)).toHaveCount(0);
  expect(writes).toEqual([]);
});

test('routes list in pipeline and size order whatever the saved key order, and categories keep their names', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  const navigate = navigatorFor(page, !!isMobile);
  await login(page);
  await navigate('Configuration');
  const order = [
    'Orchestrator',
    'Discovery agents',
    'Proposal reviewers',
    'Code reviewer',
    'XS execution',
    'S execution',
    'M execution',
    'L execution',
    'XL execution',
    'Repair'
  ];
  const headings = page.locator('.model-route h3');
  // The service sorts map keys (code_reviewer first; tiers L, M, S, XL, XS).
  await expect(headings).toHaveText(order);
  state.saved = reorderKeys(state.saved, 'reverse');
  await navigate('Overview');
  const refresh = page.waitForResponse('**/api/config');
  await navigate('Configuration');
  await refresh;
  await expect(headings).toHaveText(order);
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
  await expect(page.getByText('UX & DX', { exact: true })).toBeVisible();
  expect(state.writes).toEqual([]);
});

test('a blank runner executable is flagged on its own field before any save is sent', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const save = page.getByRole('button', { name: 'Save configuration' });
  const invalid = (field: Locator) =>
    field.evaluate((input) => (input as HTMLInputElement).matches(':invalid'));
  // The service validates both executables on every save, whichever runner is used.
  for (const name of ['OpenCode executable', 'Codex executable']) {
    const field = page.getByLabel(name, { exact: true });
    const saved = await field.inputValue();
    expect(await invalid(field)).toBe(false);
    await field.fill('');
    await save.click();
    expect(await invalid(field)).toBe(true);
    await expect(field).toBeFocused();
    expect(state.writes).toEqual([]);
    await field.fill(saved);
  }
  await page.getByLabel('OpenCode executable', { exact: true }).fill('/fixture/opencode-next');
  await save.click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes.map((write) => write.config)).toEqual([
    { opencode_binary: '/fixture/opencode-next' }
  ]);
});

test('a revision conflict offers to discard the draft and reload the saved configuration in place', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const branch = page.getByLabel('Default branch', { exact: true });
  await expect(branch).toHaveValue('fixture-main');
  const reads = state.reads;
  const alert = page.getByRole('alert').and(page.locator('.settings-feedback'));
  const reload = page.getByRole('button', { name: 'Discard edits and reload' });

  // Another tab saves first, so this tab's save pins a superseded revision.
  state.saved!.default_branch = 'external-main';
  await branch.fill('stale-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  await expect(branch).toHaveValue('stale-main');
  expect(state.reads).toBe(reads);
  // Plain Discard keeps the stale revision and offers nothing more.
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(alert).toHaveCount(0);
  await expect(branch).toHaveValue('fixture-main');
  await branch.fill('stale-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  await expect(reload).toBeVisible();
  expect(state.writes).toHaveLength(2);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  // Typing while the reload's read is in flight keeps that draft; nothing claims a reload.
  const saved = page.getByRole('status').and(page.locator('.settings-feedback'));
  state.loadGate = deferred();
  await reload.click();
  await expect.poll(() => state.reads).toBe(reads + 1);
  await branch.fill('typed-during-reload');
  state.loadGate.resolve();
  state.loadGate = null;
  await expect(page.getByRole('button', { name: 'Save configuration' })).toBeEnabled();
  await expect(branch).toHaveValue('typed-during-reload');
  await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible();
  await expect(saved).toHaveCount(0);
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  expect(state.writes).toHaveLength(3);

  // The reload is explicit, drops the draft and reads the saved configuration once.
  await reload.click();
  await expect(branch).toHaveValue('external-main');
  await expect(saved).toHaveText('Edits discarded. Saved configuration reloaded.');
  await expect(alert).toHaveCount(0);
  await expect(reload).toHaveCount(0);
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
  expect(state.reads).toBe(reads + 2);
  expect(state.writes).toHaveLength(3);

  // The next save pins the reloaded revision and is accepted.
  await branch.fill('current-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes.at(-1)!.config).toEqual({ default_branch: 'current-main' });
  expect(state.saved!.default_branch).toBe('current-main');

  // A conflict a reload cannot resolve, such as a service that is no longer paused, keeps
  // the draft and offers no reload.
  state.failSave = true;
  await branch.fill('paused-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toHaveText('Synthetic save conflict');
  await expect(reload).toHaveCount(0);
  await expect(branch).toHaveValue('paused-main');
  expect(state.writes).toHaveLength(5);
  expect(state.reads).toBe(reads + 2);
  state.failSave = false;

  // Other failures are not offered a reload.
  await page.route('**/api/model-catalog', (route) =>
    route.fulfill({ status: 503, json: { error: 'Synthetic catalog outage' } })
  );
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await expect(alert).toContainText('Synthetic catalog outage');
  await expect(reload).toHaveCount(0);
});

test('configuration keeps drafts and catalogs across views, discards locally, and refreshes clean values', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  const navigate = navigatorFor(page, !!isMobile);
  await login(page);
  await navigate('Configuration');
  const branch = page.getByLabel('Default branch', { exact: true });
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });
  await expect(branch).toHaveValue('fixture-main');
  await page.getByLabel('Codex executable', { exact: true }).fill('/draft/codex');
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await branch.fill('draft-main');
  const draftCommands = '  fixture draft test  \n\nfixture draft build\n';
  await commands.fill(draftCommands);
  await page.getByLabel('Repair reasoning effort', { exact: true }).selectOption('high');
  await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible();
  // Each disabled check names the reason it is unavailable.
  for (const name of ['Check connection', 'Check audit connection']) {
    const check = page.getByRole('button', { name, exact: true });
    await expect(check).toBeDisabled();
    await expect(check).toHaveAccessibleDescription(/Save or discard edits before checking\./);
  }
  await navigate('Task queue');
  await expect(branch).toBeHidden();
  await navigate('Configuration');
  await expect(branch).toHaveValue('draft-main');
  await expect(commands).toHaveValue(draftCommands);
  await expect(page.getByLabel('Repair reasoning effort', { exact: true })).toHaveValue('high');
  await expect(
    page.getByLabel('Repair reasoning effort', { exact: true }).locator('option[value="high"]')
  ).toHaveCount(1);
  expect(state.reads).toBe(1);
  expect(state.catalogs).toEqual([{ backend: 'codex', binary: '/draft/codex' }]);
  expect(state.checks).toEqual([]);
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(branch).toHaveValue('fixture-main');
  await expect(commands).toHaveValue('fixture saved test');
  expect(state.writes).toEqual([]);
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Check audit connection' }).click();
  await expect.poll(() => state.checks).toEqual(['audit']);
  await navigate('Overview');
  state.saved!.default_branch = 'externally-saved-main';
  await navigate('Configuration');
  await expect(branch).toHaveValue('externally-saved-main');
  expect(state.reads).toBe(2);
  // A clean revisit must not overwrite typing started during that request.
  await navigate('Overview');
  state.loadGate = deferred();
  await navigate('Configuration');
  await expect.poll(() => state.reads).toBe(3);
  await branch.fill('edited-during-refresh');
  state.loadGate.resolve();
  await expect(page.getByRole('button', { name: 'Save configuration' })).toBeEnabled();
  await expect(branch).toHaveValue('edited-during-refresh');
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(branch).toHaveValue('externally-saved-main');
});

test('typing a model ID keeps the chosen effort or variant unless a catalog entry lacks it', async ({
  page,
  isMobile
}) => {
  await configurationFixture(page);
  await page.route('**/api/model-catalog', async (route) => {
    const { backend } = route.request().postDataJSON() as { backend: 'codex' | 'opencode' };
    const entry = (over: Partial<Model>): Model => ({
      backend,
      provider: null,
      provider_name: null,
      model: '',
      display_name: '',
      efforts: [],
      variants: [],
      available: true,
      unavailable_reason: null,
      ...over
    });
    await route.fulfill({
      json:
        backend === 'codex'
          ? [
              entry({ model: 'gpt-6', display_name: 'Base', efforts: ['medium'] }),
              entry({ model: 'gpt-6-astra', display_name: 'Astra', efforts: ['medium', 'high'] }),
              entry({ model: 'gpt-6-lite', display_name: 'Lite', efforts: ['medium'] })
            ]
          : [
              entry({
                provider: 'fixture',
                provider_name: 'Fixture',
                model: 'fixture-model',
                display_name: 'Fixture model',
                variants: ['low', 'high']
              })
            ]
    });
  });
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  // Keyboard editing passes through model IDs that no catalog lists.
  const retype = async (field: Locator, last: string) => {
    await field.click();
    await field.press('End');
    await field.press('Backspace');
    await field.pressSequentially(last);
  };
  const model = page.getByLabel('Repair model', { exact: true });
  const effort = page.getByLabel('Repair reasoning effort', { exact: true });
  await expect(model).toHaveValue('gpt-6-astra');
  // Without a catalog nothing proves the saved effort unsupported, and it stays selectable.
  await retype(model, 'a');
  await expect(model).toHaveValue('gpt-6-astra');
  await expect(effort).toHaveValue('medium');
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await effort.selectOption('high');
  await retype(model, 'a');
  await expect(model).toHaveValue('gpt-6-astra');
  await expect(effort).toHaveValue('high');
  // A catalog entry for the new model that lacks the effort still clears it.
  await model.fill('gpt-6-lite');
  await expect(effort).toHaveValue('');
  // Typing through 'gpt-6', which lacks 'high' but begins longer IDs, keeps the choice...
  await model.fill('gpt-6-astra');
  await effort.selectOption('high');
  await model.fill('');
  await model.pressSequentially('gpt-6-astra');
  await expect(effort).toHaveValue('high');
  // ...until 'gpt-6' is what the operator commits.
  await model.fill('gpt-6');
  await expect(effort).toHaveValue('high');
  await model.press('Tab');
  await expect(effort).toHaveValue('');

  const xs = (field: string) => page.getByLabel(`XS execution ${field}`, { exact: true });
  await xs('runner').selectOption('opencode');
  await page.getByRole('button', { name: 'Load OpenCode models' }).click();
  await xs('provider').selectOption('fixture');
  await xs('model').fill('fixture-model');
  await xs('variant').selectOption('high');
  await retype(xs('model'), 'l');
  await expect(xs('model')).toHaveValue('fixture-model');
  await expect(xs('variant')).toHaveValue('high');
});

for (const check of [
  {
    mode: 'execution',
    button: 'Check connection',
    key: 'codex_binary',
    field: 'Codex executable'
  },
  {
    mode: 'audit',
    button: 'Check audit connection',
    key: 'repository',
    field: 'Repository path'
  }
] as const) {
  test(`${check.mode} check feedback clears when refreshed saved configuration changes`, async ({
    page,
    isMobile
  }) => {
    const state = await configurationFixture(page);
    const navigate = navigatorFor(page, !!isMobile);
    await login(page);
    await navigate('Configuration');
    const field = page.getByLabel(check.field);
    const button = page.getByRole('button', { name: check.button, exact: true });
    const badge = page.locator('[data-step="preflight"] .badge');
    await expect(button).toBeEnabled();

    for (const failed of [false, true]) {
      state.failCheck = failed;
      const feedback = page
        .getByRole(failed ? 'alert' : 'status')
        .and(page.locator('.settings-feedback'));
      const text = failed
        ? 'Synthetic connection check failed'
        : 'Synthetic saved configuration checked';
      await button.click();
      await expect(feedback).toHaveText(text);
      await expect(badge).toHaveText(
        new RegExp(`^${failed ? 'Failed' : 'Passed'} · ${check.mode} · `)
      );
      const result = await badge.innerText();

      // Reordering object keys, including nested routes, keeps the diagnostic result.
      state.saved = reorderKeys(state.saved, 'reverse');
      await navigate('Overview');
      const refresh = page.waitForResponse('**/api/config');
      await navigate('Configuration');
      await refresh;
      await expect(button).toBeEnabled();
      await expect(feedback).toHaveText(text);
      await expect(badge).toHaveText(result);

      // Simulate settings saved by another tab, then accept them on a clean revisit.
      await navigate('Overview');
      const updated = `/external/${check.key}-${failed ? 'after-failure' : 'after-success'}`;
      state.saved![check.key] = updated;
      await navigate('Configuration');
      await expect(field).toHaveValue(updated);
      await expect(feedback).toHaveCount(0);
      await expect(badge).toHaveText('Not checked');
      await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
    }
    expect(state.checks).toEqual([check.mode, check.mode]);
    expect(state.writes).toEqual([]);
  });
}

test('failed configuration loads retry, failed saves retain exact drafts, and successful saves set the discard baseline', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  state.failLoad = true;
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(page.getByRole('alert')).toContainText('Could not load configuration');
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeDisabled();
  state.failLoad = false;
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });
  await expect(commands).toHaveValue('fixture saved test');
  const draft = '  fixture new test  \n\nfixture new build\n';
  await commands.fill(draft);
  state.failSave = true;
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('alert')).toContainText('Synthetic save conflict');
  await expect(commands).toHaveValue(draft);
  await openNavigation(page, 'Overview', !!isMobile);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(commands).toHaveValue(draft);
  state.failSave = false;
  state.saveGate = deferred();
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('button', { name: 'Saving configuration…' })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Discard changes' })).toBeDisabled();
  // Even a scripted form submission while pending cannot create a duplicate write.
  await page.locator('form').evaluate((form) => (form as HTMLFormElement).requestSubmit());
  state.saveGate.resolve();
  await expect(page.getByRole('status').and(page.locator('.settings-feedback'))).toHaveText(
    'Configuration saved.'
  );
  expect(state.writes).toHaveLength(2);
  await expect(commands).toHaveValue('fixture new test\nfixture new build');
  await page.getByRole('button', { name: 'Check connection', exact: true }).click();
  await expect.poll(() => state.checks).toEqual(['execution']);
  await commands.fill('another draft');
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(commands).toHaveValue('fixture new test\nfixture new build');
});

test('a successful save clears an earlier failed configuration refresh', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  const navigate = navigatorFor(page, !!isMobile);
  await login(page);
  await navigate('Configuration');
  const branch = page.getByLabel('Default branch', { exact: true });
  await expect(branch).toHaveValue('fixture-main');
  state.failLoad = true;
  await navigate('Overview');
  await navigate('Configuration');
  const stale = page.getByText(/Could not refresh saved configuration/);
  await expect(stale).toBeVisible();
  await expect(branch).toHaveValue('fixture-main');
  state.failLoad = false;
  await branch.fill('saved-after-failure');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  // The form now shows the service's fresh canonical view, not the last loaded values.
  await expect(stale).toHaveCount(0);
  await expect(branch).toHaveValue('saved-after-failure');
  expect(state.writes.map((write) => write.config)).toEqual([
    { default_branch: 'saved-after-failure' }
  ]);
});

test('saved PR maintenance thresholds of 0 are valid and never block saving other settings', async ({
  page,
  isMobile
}) => {
  // The service accepts 0 for both thresholds: every owned open PR then counts.
  const state = await configurationFixture(page, {
    saved: { large_pr_lines: 0, long_lived_pr_days: 0 }
  });
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  for (const label of ['Large PR threshold', 'Long-lived PR (days)']) {
    const field = page.getByRole('spinbutton', { name: label });
    await expect(field).toHaveValue('0');
    expect(await field.evaluate((input: HTMLInputElement) => input.validity.valid)).toBe(true);
  }
  await page.getByLabel('Default branch', { exact: true }).fill('threshold-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes).toHaveLength(1);
  expect(state.writes[0].config).toEqual({ default_branch: 'threshold-main' });
});

test('an emptied operating limit stays empty and blocks saving until it is filled', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const retries = page.getByRole('spinbutton', { name: /^Operator retries/ });
  const agents = page.getByRole('spinbutton', { name: /^Discovery agents/ });
  const invalid = (field: Locator) => field.evaluate((input) => input.matches(':invalid'));
  await expect(retries).toHaveValue('2');
  const savedAgents = await agents.inputValue();
  // The service accepts 0 retries, so an emptied field must never read as 0.
  await retries.fill('');
  await expect(retries).toHaveValue('');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  expect(await invalid(retries)).toBe(true);
  await retries.pressSequentially('3');
  await expect(retries).toHaveValue('3');
  expect(await invalid(retries)).toBe(false);
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  // Only the completed value was ever sent, as a number.
  expect(state.writes.map((write) => write.config)).toEqual([{ max_retries: 3 }]);
  await agents.fill('');
  await expect(agents).toHaveValue('');
  expect(await invalid(agents)).toBe(true);
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(agents).toHaveValue(savedAgents);
  await expect(retries).toHaveValue('3');
  expect(state.writes).toHaveLength(1);
});

test('operating limits refuse values above the service maxima before saving', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const validity = (field: Locator) =>
    field.evaluate((input: HTMLInputElement) => ({
      valid: input.validity.valid,
      overflow: input.validity.rangeOverflow
    }));
  // Upper bounds internal/config enforces on save.
  const maxima: [RegExp, number][] = [
    [/^Cycle interval/, 604800],
    [/^Maintenance cadence/, 10000],
    [/^Session timeout/, 604800],
    [/^Task timeout/, 604800],
    [/^Command timeout/, 604800],
    [/^Daily session budget/, 1000000],
    [/^Workspace budget/, 1e15],
    [/^Workspace retention/, 36500]
  ];
  for (const [name, max] of maxima) {
    const field = page.getByRole('spinbutton', { name });
    const saved = await field.inputValue();
    await field.fill(String(max + 1));
    expect(await validity(field)).toEqual({ valid: false, overflow: true });
    await field.fill(String(max));
    expect(await validity(field)).toEqual({ valid: true, overflow: false });
    await field.fill(saved);
  }
  const interval = page.getByRole('spinbutton', { name: /^Cycle interval/ });
  await interval.fill('1209600');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  expect(await validity(interval)).toEqual({ valid: false, overflow: true });
  await interval.fill('604800');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes.map((write) => write.config)).toEqual([{ cycle_interval_seconds: 604800 }]);
});

test('display-transformed fields stay canonical: previews lock, unrelated saves omit them, replacement is explicit', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page, {
    transformed: {
      // The canonical command keeps its secret; only the preview is served.
      overrides: { verification_commands: ['echo [redacted] > /dev/null'] },
      fields: [
        {
          field: 'verification_commands',
          kinds: ['redacted'],
          paths: [['verification_commands', 0]]
        }
      ]
    }
  });
  const navigate = navigatorFor(page, !!isMobile);
  await login(page);
  await navigate('Configuration');
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });

  // The served preview is marked and locked; the hidden value is never editable.
  await expect(commands).toHaveValue('echo [redacted] > /dev/null');
  // Read the seeded revision only after the load landed: before the first GET
  // the fixture's saved config is still null.
  const loaded = state.revision();
  await expect(commands).toHaveJSProperty('readOnly', true);
  await expect(page.locator('#preview-verification_commands')).toContainText('hidden value');

  // Saving an unrelated field sends only that field under the loaded revision;
  // the hidden preview is never written back.
  await page.getByLabel('Default branch', { exact: true }).fill('preview-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes).toHaveLength(1);
  expect(state.writes[0].expected_revision).toBe(loaded);
  expect(state.writes[0].config).toEqual({ default_branch: 'preview-main' });
  expect(state.saved!.verification_commands).toEqual(['fixture saved test']);
  await expect(commands).toHaveValue('echo [redacted] > /dev/null');

  // A write pinning a superseded revision conflicts instead of overwriting.
  state.saved!.default_branch = 'external-main';
  await page.getByLabel('Default branch', { exact: true }).fill('stale-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('alert')).toContainText('Synthetic save conflict');
  await page.getByRole('button', { name: 'Discard changes' }).click();
  // Reloading accepts the externally saved values and their new revision.
  await navigate('Overview');
  await navigate('Configuration');
  await expect(page.getByLabel('Default branch', { exact: true })).toHaveValue('external-main');
  await expect(commands).toHaveValue('echo [redacted] > /dev/null');

  // Replacing a hidden collection clears it for full re-entry; only then is it sent.
  await page.locator('#replace-verification_commands').click();
  await expect(commands).toHaveValue('');
  await expect(commands).toHaveJSProperty('readOnly', false);
  await commands.fill('echo replacement test');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes.at(-1)!.config).toEqual({
    verification_commands: ['echo replacement test']
  });
  expect(state.saved!.verification_commands).toEqual(['echo replacement test']);
  await navigate('Overview');
  await navigate('Configuration');
  await expect(commands).toHaveValue('echo replacement test');
  await expect(page.locator('#preview-verification_commands')).toHaveCount(0);
});

test('locked previews look as non-editable as disabled fields until they are replaced', async ({
  page,
  isMobile
}) => {
  await configurationFixture(page, {
    transformed: {
      overrides: {
        repository: '/srv/[redacted]',
        repair_route: { backend: 'codex', model: 'gpt-6-astra-shortened…', effort: 'medium' }
      },
      fields: [
        { field: 'repository', kinds: ['redacted'], paths: [['repository']] },
        { field: 'repair_route', kinds: ['shortened'], paths: [['repair_route', 'model']] }
      ]
    }
  });
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const well = 'rgb(250, 251, 247)';
  const white = 'rgb(255, 255, 255)';
  const repository = page.locator('#repository-path');
  const branch = page.getByLabel('Default branch', { exact: true });
  await expect(repository).toHaveJSProperty('readOnly', true);
  await expect(repository).toHaveCSS('background-color', well);
  await expect(branch).toHaveCSS('background-color', white);
  // A locked route disables its selects; they share the disabled model field's fill and
  // keep their chevron.
  const runner = page.getByLabel('Repair runner', { exact: true });
  await expect(runner).toBeDisabled();
  await expect(runner).toHaveCSS('background-color', well);
  await expect(runner).toHaveCSS('background-image', /^url\(/);
  await expect(page.getByLabel('Repair model', { exact: true })).toHaveCSS(
    'background-color',
    well
  );
  if (!isMobile) {
    // Hovering an editable field highlights its border; a read-only preview's stays put.
    await branch.hover();
    await expect(branch).toHaveCSS('border-top-color', 'rgb(201, 212, 184)');
    await repository.hover();
    // The border colour transitions, so wait until the field just left has settled: a
    // highlight on the read-only field would have finished by then too.
    await expect(branch).toHaveCSS('border-top-color', 'rgb(223, 231, 213)');
    await expect(repository).toHaveCSS('border-top-color', 'rgb(223, 231, 213)');
  }
  const accessibility = await new AxeBuilder({ page })
    .include('#main-content')
    .withTags(['wcag2a', 'wcag2aa'])
    .analyze();
  expect(
    accessibility.violations.map((v) => ({ rule: v.id, elements: v.nodes.map((n) => n.target) }))
  ).toEqual([]);

  await page.locator('#replace-repository').click();
  await expect(repository).toHaveJSProperty('readOnly', false);
  await expect(repository).toHaveCSS('background-color', white);
  await page.locator('#replace-repair_route').click();
  await expect(runner).toBeEnabled();
  await expect(runner).toHaveCSS('background-color', white);
});

test('a previewed runner executable is never sent for a model catalog until it is replaced', async ({
  page,
  isMobile
}) => {
  // The saved path is served redacted; posting the preview would run a path that
  // does not exist, so its catalog waits until the operator enters a real one.
  const state = await configurationFixture(page, {
    transformed: {
      overrides: { codex_binary: '/opt/ta[redacted]/bin/codex' },
      fields: [{ field: 'codex_binary', kinds: ['redacted'], paths: [['codex_binary']] }]
    }
  });
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const executable = page.getByRole('textbox', { name: /^Codex executable/ });
  const load = page.getByRole('button', { name: 'Load Codex models' });
  await expect(executable).toHaveValue('/opt/ta[redacted]/bin/codex');
  await expect(load).toBeDisabled();
  await expect(load).toHaveAccessibleDescription(
    'Replace the Codex executable preview to load its catalog.'
  );
  // The other runner's saved path is not a preview, so its catalog stays available.
  await expect(page.getByRole('button', { name: 'Load OpenCode models' })).toBeEnabled();
  await load.click({ force: true });
  expect(state.catalogs).toEqual([]);

  await page.locator('#replace-codex_binary').click();
  await expect(executable).toHaveValue('');
  await expect(page.locator('#catalog-preview-codex')).toHaveCount(0);
  await executable.fill('/draft/codex');
  await load.click();
  await expect(page.getByText('1 Codex models available.')).toBeVisible();
  expect(state.catalogs).toEqual([{ backend: 'codex', binary: '/draft/codex' }]);
});

test('setup checklist distinguishes entered, saved, checked, stale and failed states without starting work', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page, {
    unconfigured: true,
    snapshot: (snapshot) => {
      snapshot.configured = false;
      snapshot.audit_configured = false;
      snapshot.cycles = [];
      snapshot.counts = {};
    }
  });
  const writes = trackWrites(page);
  const navigate = navigatorFor(page, !!isMobile);
  const step = (id: string) => page.locator(`[data-step="${id}"]`);
  const badge = (id: string) => step(id).locator('.badge');
  await login(page);
  await expect(page.getByRole('region', { name: 'Choose your first run' })).toContainText(
    'Your first move: an audit.'
  );
  await expect(page.getByRole('button', { name: 'Set up your project' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeDisabled();
  expect(writes).toEqual([]);
  await navigate('Configuration');
  await expect(page.getByRole('heading', { name: 'Setup checklist' })).toBeVisible();
  await expect(page.getByLabel('Setup status meanings')).toContainText('Saved');
  await expect(badge('repository')).toHaveText('Incomplete');
  await expect(badge('routes')).toHaveText('Incomplete');
  await expect(badge('verification')).toHaveText('None');
  await expect(badge('preflight')).toHaveText('Not checked');
  await expect(badge('baseline')).toHaveText('Optional · not run');
  await expect(badge('choose')).toHaveText('Not run');
  await expect(step('choose')).toContainText(
    'Audit: saved configuration incomplete. Run once: saved configuration incomplete.'
  );
  await expect(page.getByText('0 of 6 steps saved, checked or run')).toBeVisible();

  // Entered: typed in this tab only.
  await page.getByLabel('Repository path').fill('/fixture/entered');
  await page.getByLabel('GitHub repository').fill('fixture/entered');
  await expect(badge('repository')).toHaveText('Entered, not saved');
  await expect(badge('preflight')).toHaveText('Unsaved edits');
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Load Codex models' }).click();

  // Partially configured: only the three audit routes, saved.
  for (const role of ['Orchestrator', 'Discovery agents', 'Proposal reviewers']) {
    await page.getByLabel(`${role} model`, { exact: true }).fill('gpt-6-astra');
    await page.getByLabel(`${role} reasoning effort`, { exact: true }).selectOption('medium');
  }
  await expect(badge('routes')).toHaveText('Entered, not saved');
  await expect(step('routes')).toContainText(
    '3 of 10 execution routes selected (3 of 3 audit routes). 3 match a loaded catalog'
  );
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect(badge('repository')).toHaveText('Saved');
  await expect(badge('routes')).toHaveText('Saved, audit routes only');
  await expect(step('routes')).toContainText('Run once also needs the code reviewer');
  await expect(badge('verification')).toHaveText('None');
  await expect(badge('preflight')).toHaveText('Not checked');
  expect(state.writes).toHaveLength(1);

  // Route edits change the draft only; nothing is saved until Save.
  for (const role of [
    'Code reviewer',
    'XS execution',
    'S execution',
    'M execution',
    'L execution',
    'XL execution',
    'Repair'
  ]) {
    await page.getByLabel(`${role} model`, { exact: true }).fill('gpt-6-astra');
    await page.getByLabel(`${role} reasoning effort`, { exact: true }).selectOption('medium');
  }
  await expect(badge('routes')).toHaveText('Entered, not saved');
  await expect(step('routes')).toContainText(
    '10 of 10 execution routes selected (3 of 3 audit routes). 10 match a loaded catalog'
  );
  expect(state.writes).toHaveLength(1);
  await page.getByRole('textbox', { name: /^Verification commands/ }).fill('fixture entered test');
  await expect(badge('verification')).toHaveText('Entered, not saved');
  expect(state.writes).toHaveLength(1);

  // Saved: written to the service, still unchecked.
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect(badge('repository')).toHaveText('Saved');
  await expect(badge('routes')).toHaveText('Saved');
  await expect(badge('verification')).toHaveText('Saved');
  await expect(badge('preflight')).toHaveText('Not checked');
  await expect(step('preflight')).toContainText('does not prove repository push permission');
  await expect(page.getByText('3 of 6 steps saved, checked or run')).toBeVisible();
  expect(state.writes).toHaveLength(2);

  // Checked: the link only focuses the existing control; the operator activates it.
  await step('preflight').getByRole('button', { name: 'Open the execution check' }).click();
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeFocused();
  expect(state.checks).toEqual([]);
  await page.keyboard.press('Enter');
  await expect(badge('preflight')).toHaveText(/^Passed · execution · /);
  await expect.poll(() => state.checks).toEqual(['execution']);
  await expect(page.getByText('4 of 6 steps saved, checked or run')).toBeVisible();

  // The API sorts keys; revisiting after a saved change must preserve this exact check.
  const checked = structuredClone(state.saved!);
  const result = await badge('preflight').innerText();
  state.saved = reorderKeys(state.saved, 'sort');
  expect(state.saved).toEqual(checked);
  expect(JSON.stringify(state.saved)).not.toBe(JSON.stringify(checked));
  await navigate('Overview');
  const refresh = page.waitForResponse('**/api/config');
  await navigate('Configuration');
  await refresh;
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeEnabled();
  await expect(badge('preflight')).toHaveText(result);
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
  await expect(page.getByText('4 of 6 steps saved, checked or run')).toBeVisible();
  expect(state.checks).toEqual(['execution']);

  // Stale while dirty, restored by discard, invalidated by a saved change.
  await page.getByLabel('Default branch', { exact: true }).fill('edited-main');
  await expect(badge('preflight')).toHaveText('Unsaved edits');
  await expect(step('preflight')).toContainText(
    'covered the previously saved values, not these edits'
  );
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(badge('preflight')).toHaveText(/^Passed · execution · /);
  await page.getByLabel('Default branch', { exact: true }).fill('resaved-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect(badge('preflight')).toHaveText('Not checked');

  // A failed check is reported as a failure, never as readiness.
  state.failCheck = true;
  await step('preflight').getByRole('button', { name: 'Open the audit check' }).click();
  await expect(page.getByRole('button', { name: 'Check audit connection' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(badge('preflight')).toHaveText(/^Failed · audit · /);
  await expect(step('preflight')).toContainText('Synthetic connection check failed');
  await expect(page.getByRole('alert')).toHaveText('Synthetic connection check failed');
  expect(state.checks).toEqual(['execution', 'audit']);

  await expect(badge('baseline')).toHaveText('Optional · not run');
  expect(state.baselines).toHaveLength(0);
  await step('baseline').getByRole('button', { name: 'Open the baseline check' }).click();
  await expect(page.locator('#check-baseline')).toBeFocused();
  expect(state.baselines).toHaveLength(0);
  await page.locator('#check-baseline').click();
  await expect(page.getByRole('alertdialog')).toContainText(
    'Run the saved verification commands on a disposable clone of the remote default branch?'
  );
  await page.getByRole('button', { name: 'Back' }).click();
  expect(state.baselines).toHaveLength(0);
  await page.locator('#check-baseline').click();
  await page.getByRole('button', { name: 'Run baseline check' }).click();
  await expect.poll(() => state.baselines.length).toBe(1);
  // Admission pins the canonical configuration revision, never an echoed object.
  expect(state.baselines[0]).toEqual({ expected_revision: state.revision() });
  await expect(page.getByRole('button', { name: 'Cancel baseline check' })).toBeVisible();
  await page.getByRole('button', { name: 'Cancel baseline check' }).click();
  await expect.poll(() => state.baselines.length).toBe(2);
  expect(state.baselines[1]).toEqual({ cancel: expect.stringContaining('/cancel') });
  expect(writes.map((write) => write.path)).toEqual([
    '/api/model-catalog',
    '/api/config',
    '/api/config',
    '/api/doctor',
    '/api/config',
    '/api/doctor',
    '/api/baseline-checks',
    '/api/baseline-checks/synthetic-check/cancel'
  ]);
});

for (const mode of ['execution', 'audit'] as const) {
  test(`setup checklist attributes ${mode} checks to the server snapshot across external saves`, async ({
    page,
    isMobile
  }) => {
    const state = await configurationFixture(page);
    const navigate = navigatorFor(page, !!isMobile);
    await login(page);
    await navigate('Configuration');
    const branch = page.getByLabel('Default branch', { exact: true });
    const badge = page.locator('[data-step="preflight"] .badge');
    const check = page.getByRole('button', {
      name: mode === 'execution' ? 'Check connection' : 'Check audit connection',
      exact: true
    });
    await expect(branch).toHaveValue('fixture-main');
    await check.click();
    await expect(badge).toHaveText(new RegExp(`^Passed · ${mode} · `));

    const original = structuredClone(state.saved!);
    // Simulate another dashboard tab saving before this stale form starts a check.
    state.saved!.default_branch = 'external-main';
    for (const fail of [false, true]) {
      state.failCheck = fail;
      state.checkGate = deferred();
      const count = state.checks.length;
      await check.click();
      await expect.poll(() => state.checks.length).toBe(count + 1);
      // Another save during the check must not change which snapshot it covered.
      state.saved = structuredClone(original);
      state.checkGate.resolve();
      await expect(check).toBeEnabled();
      await expect(badge).toHaveText('Not checked');
      await expect(page.locator('[data-step="preflight"]')).toContainText(
        'The server checked a different saved configuration'
      );
      await expect(branch).toHaveValue('fixture-main');
      state.saved!.default_branch = 'external-main';
    }

    state.failCheck = false;
    state.checkGate = null;
    await navigate('Overview');
    await navigate('Configuration');
    await expect(branch).toHaveValue('external-main');
    // Serialization order is not a configuration change, including nested routes.
    state.saved = reorderKeys(state.saved, 'reverse');
    await check.click();
    await expect(badge).toHaveText(new RegExp(`^Passed · ${mode} · `));

    // A transport failure has no checked snapshot and must clear the previous result.
    await page.route('**/api/doctor?*', (route) => route.abort('failed'));
    await check.click();
    await expect(check).toBeEnabled();
    await expect(badge).toHaveText('Not checked');
    expect(state.writes).toHaveLength(0);
  });
}

test('setup checklist links focus existing controls, hands off to the Overview and reports active work without starting anything', async ({
  page,
  isMobile
}) => {
  let restriction: 'idle' | 'task' | 'audit' | 'continuous' = 'idle';
  await configurationFixture(page, {
    snapshot: (snapshot) => {
      snapshot.configured = true;
      snapshot.audit_configured = true;
      snapshot.counts = { ...snapshot.counts, queued: 2 };
      snapshot.cycles = [
        {
          id: 'synthetic-audit',
          number: 4,
          mode: 'audit',
          status: 'completed',
          started_at: new Date().toISOString(),
          completed_at: new Date().toISOString(),
          error: null,
          session_count: 13,
          decisions: { accepted: 1 },
          lifecycle: {}
        }
      ];
      if (restriction === 'task') snapshot.active_tasks = 1;
      if (restriction === 'audit') {
        snapshot.cycle_active = true;
        snapshot.active_cycle_mode = 'audit';
      }
      if (restriction === 'continuous') {
        snapshot.control.paused = false;
        snapshot.control.mode = 'continuous';
      }
    }
  });
  const writes = trackWrites(page);
  const navigate = navigatorFor(page, !!isMobile);
  const step = (id: string) => page.locator(`[data-step="${id}"]`);
  const badge = (id: string) => step(id).locator('.badge');
  await login(page);
  await navigate('Configuration');
  await expect(badge('choose')).toHaveText('Audit cycle 4 · completed');
  await expect(step('choose')).toContainText(
    'Audit: available. Run once: available, and 2 queued tasks would be drained first.'
  );
  await expect(step('choose')).toContainText('no later cycle executes its recommendations');
  await expect(badge('preflight')).toHaveText('Not checked');

  // Links move focus to the existing controls without editing them.
  await step('repository').getByRole('button', { name: 'Edit repository details' }).click();
  await expect(page.getByLabel('Repository path')).toBeFocused();
  await step('verification').getByRole('button', { name: 'Edit verification commands' }).click();
  await expect(page.getByRole('textbox', { name: /^Verification commands/ })).toBeFocused();
  await step('routes').getByRole('button', { name: 'Edit routes' }).click();
  await expect(page.getByLabel('Orchestrator runner', { exact: true })).toBeFocused();
  await step('routes').getByRole('button', { name: 'Load a runner catalog' }).click();
  await expect(page.getByRole('button', { name: 'Load Codex models' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
  await expect(badge('routes')).toHaveText('Saved');
  await expect(step('routes')).toContainText('10 match a loaded catalog');

  // Choosing hands off to the Overview control; the operator still has to click it.
  await step('choose').getByRole('button', { name: 'Run once on the Overview' }).click();
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Run once', exact: true })).toBeFocused();
  await navigate('Configuration');
  await step('choose').getByRole('button', { name: 'Audit on the Overview' }).click();
  await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeFocused();
  await navigate('Configuration');

  // Hiding the checklist is tab-local and starts nothing.
  await page.getByRole('button', { name: 'Hide checklist' }).click();
  await expect(page.locator('[data-step]')).toHaveCount(0);
  await page.getByRole('button', { name: 'Show checklist' }).click();
  await expect(page.locator('[data-step]')).toHaveCount(6);

  // Active work, audits and continuous operation are reported, not hidden.
  restriction = 'task';
  await expect(step('choose')).toContainText(
    'Unavailable now: 1 active task may still finish and publish.',
    { timeout: 10000 }
  );
  restriction = 'audit';
  await expect(step('choose')).toContainText('Unavailable now: An audit is in progress.', {
    timeout: 10000
  });
  restriction = 'continuous';
  await expect(step('choose')).toContainText(
    'Continuous operation is running; Pause stops new work first.',
    { timeout: 10000 }
  );
  expect(writes.map((write) => write.path)).toEqual(['/api/model-catalog']);
});

for (const boundary of ['disconnect', 'expiry', 'reload']) {
  test(`configuration drafts and catalogs clear on ${boundary}`, async ({ page, isMobile }) => {
    await configurationFixture(page);
    await login(page);
    await openNavigation(page, 'Configuration', !!isMobile);
    await page.getByRole('button', { name: 'Check connection', exact: true }).click();
    await expect(page.locator('[data-step="preflight"] .badge')).toHaveText(
      /^Passed · execution · /
    );
    await page.getByLabel('Default branch', { exact: true }).fill('private-unsaved-main');
    await page.getByRole('button', { name: 'Load Codex models' }).click();
    await expect(
      page.getByLabel('Repair reasoning effort', { exact: true }).locator('option[value="high"]')
    ).toHaveCount(1);
    if (boundary === 'disconnect') {
      if (isMobile) await page.getByRole('button', { name: 'Toggle navigation' }).click();
      await page.getByRole('button', { name: /Disconnect/ }).click();
    } else if (boundary === 'expiry') {
      await page.route('**/api/state', (route) =>
        route.fulfill({ status: 401, json: { error: 'Synthetic expired session' } })
      );
      await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible(
        { timeout: 10000 }
      );
      await page.unroute('**/api/state');
    } else await page.reload();
    await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible();
    // Reconnect in the same document for disconnect/expiry to exercise unmounting.
    await page.getByLabel('Operator access token').fill(token);
    await page.getByRole('button', { name: 'Open dashboard' }).click();
    await openNavigation(page, 'Configuration', !!isMobile);
    await expect(page.getByLabel('Default branch', { exact: true })).toHaveValue('fixture-main');
    await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
    await expect(
      page.getByLabel('Repair reasoning effort', { exact: true }).locator('option[value="high"]')
    ).toHaveCount(0);
    // The earlier check result belongs to the old session and is not carried over.
    await expect(page.locator('[data-step="preflight"] .badge')).toHaveText('Not checked');
  });
}

test('short desktop sidebars keep Disconnect reachable by mouse', async ({
  page,
  isMobile
}, testInfo) => {
  test.skip(!!isMobile, 'Exercises the desktop sidebar at 1280×680.');
  await page.setViewportSize({ width: 1280, height: 680 });
  await login(page);
  await page.locator('aside').hover();
  await page.mouse.wheel(0, 800);
  const disconnect = page.getByRole('button', { name: /Disconnect/ });
  await expect(disconnect).toBeInViewport({ ratio: 1 });
  await page.screenshot({ path: testInfo.outputPath('sidebar-1280x680.png') });
  await disconnect.click();
  await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible();
});

for (const reducedMotion of ['reduce', 'no-preference'] as const) {
  test(`mobile navigation disclosure supports skip, selected states, Escape and destination focus (${reducedMotion})`, async ({
    page
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.emulateMedia({ reducedMotion });
    await login(page);
    const toggle = page.getByRole('button', { name: 'Toggle navigation' });
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByRole('navigation')).toBeHidden();
    await page.getByRole('link', { name: 'Skip to main content' }).focus();
    await page.keyboard.press('Enter');
    await expect(page.locator('main')).toBeFocused();
    await toggle.focus();
    await page.keyboard.press('Tab');
    expect(await page.evaluate(() => !!document.activeElement?.closest('aside'))).toBe(false);
    await toggle.focus();
    await page.keyboard.press('Enter');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const overview = page
      .getByRole('navigation')
      .getByRole('button', { name: 'Overview', exact: true });
    await expect(overview).toHaveAttribute('aria-current', 'page');
    await expect(overview).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(toggle).toBeFocused();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await page.keyboard.press('Enter');
    const queue = page
      .getByRole('navigation')
      .getByRole('button', { name: 'Task queue', exact: true });
    await page.keyboard.press('Tab');
    await expect(queue).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(page.locator('main')).toBeFocused();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await page
      .getByRole('group', { name: 'Task filters' })
      .getByRole('button', { name: 'queued', exact: true })
      .click();
    await expect(
      page
        .getByRole('group', { name: 'Task filters' })
        .getByRole('button', { name: 'queued', exact: true })
    ).toHaveAttribute('aria-pressed', 'true');
    await toggle.click();
    await expect(queue).toHaveAttribute('aria-current', 'page');
    await page.keyboard.press('Escape');
    await page.setViewportSize({ width: 1280, height: 800 });
    await expect(page.getByRole('navigation')).toBeVisible();
  });
}

for (const list of [
  {
    view: 'Task queue',
    endpoint: 'tasks',
    noun: 'tasks',
    rows: '.task-row',
    empty: 'The next good idea starts here.',
    filter: 'queued'
  },
  {
    view: 'Proposals',
    endpoint: 'proposals',
    noun: 'proposals',
    rows: '.proposal-card',
    empty: 'Better ideas start with questions.',
    filter: 'rejected'
  },
  {
    view: 'Pull requests',
    endpoint: 'prs',
    noun: 'pull requests',
    rows: '.pr-row',
    empty: 'Room for your next improvement.',
    filter: 'merged'
  }
]) {
  test(`${list.view} distinguishes loading, retry, retained results and filtered emptiness`, async ({
    page,
    isMobile
  }) => {
    let mode: 'failure' | 'rows' | 'empty' = 'failure';
    let gate: ReturnType<typeof deferred> | null = deferred();
    await page.route(`**/api/${list.endpoint}?*`, async (route) => {
      await gate?.promise;
      if (mode === 'failure')
        await route.fulfill({ status: 503, json: { error: 'Synthetic list failure' } });
      else {
        const response = await route.fetch();
        const result = await response.json();
        if (mode === 'empty') result.items = [];
        await route.fulfill({ json: result });
      }
    });
    await login(page);
    await openNavigation(page, list.view, !!isMobile);
    await expect(page.getByText(`Loading ${list.noun}…`, { exact: true })).toBeVisible();
    await expect(page.getByRole('heading', { name: list.empty, exact: true })).toHaveCount(0);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText(`Could not load ${list.noun}`);
    mode = 'rows';
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.locator(list.rows).first()).toBeVisible();
    const count = await page.locator(list.rows).count();
    const rowPositions = () =>
      page.locator(list.rows).evaluateAll((rows) =>
        rows.map((row) => {
          const { x, y, width, height } = row.getBoundingClientRect();
          return { x: x + scrollX, y: y + scrollY, width, height };
        })
      );
    const positions = await rowPositions();
    gate = deferred();
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toBeVisible({
      timeout: 10000
    });
    await expect(page.locator(list.rows)).toHaveCount(count);
    expect(await rowPositions()).toEqual(positions);
    mode = 'failure';
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText('Showing the last received results');
    await expect(page.locator(list.rows)).toHaveCount(count);
    expect(await rowPositions()).toEqual(positions);
    mode = 'rows';
    gate = deferred();
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toBeVisible();
    expect(await rowPositions()).toEqual(positions);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toHaveCount(0);
    expect(await rowPositions()).toEqual(positions);
    mode = 'empty';
    await expect(page.getByRole('heading', { name: list.empty, exact: true })).toBeVisible({
      timeout: 10000
    });
    await page.getByRole('button', { name: list.filter, exact: true }).click();
    await expect(
      page.getByRole('heading', { name: `No matching ${list.noun}`, exact: true })
    ).toBeVisible();
    await expect(page.getByRole('button', { name: list.filter, exact: true })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    if (list.endpoint === 'proposals') {
      await page.getByRole('button', { name: 'all', exact: true }).click();
      await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
      await expect(
        page.getByRole('heading', { name: 'No matching proposals', exact: true })
      ).toBeVisible();
    }
  });
}

for (const rejection of [
  {
    title: "a plain-text 422 rejection is shown in the service's words",
    status: 422,
    contentType: 'text/plain; charset=utf-8',
    body: '  Failed to deserialize the query into the target type\n',
    shown: 'Could not load tasks. Failed to deserialize the query into the target type'
  },
  {
    title: 'an HTML 502 page is reported by its status only',
    status: 502,
    contentType: 'text/html',
    body: '<html><body>Synthetic proxy page</body></html>',
    shown: 'Could not load tasks. Service returned 502'
  }
]) {
  test(rejection.title, async ({ page, isMobile }) => {
    await page.route('**/api/tasks?*', (route) =>
      route.fulfill({
        status: rejection.status,
        contentType: rejection.contentType,
        body: rejection.body
      })
    );
    await login(page);
    await openNavigation(page, 'Task queue', !!isMobile);
    const alert = page.getByRole('alert');
    await expect(alert.locator('span')).toHaveText(rejection.shown);
    await expect(alert).not.toContainText('Synthetic proxy page');
  });
}

test('empty PR outcomes keep delivery history stationary during refresh and retry', async ({
  page,
  isMobile
}) => {
  let gate: ReturnType<typeof deferred> | null = null;
  let fail = false;
  await page.route('**/api/prs?*', async (route) => {
    await gate?.promise;
    if (fail) await route.fulfill({ status: 503, json: { error: 'Synthetic list failure' } });
    else {
      const response = await route.fetch();
      const result = await response.json();
      result.items = [];
      await route.fulfill({ json: result });
    }
  });
  await login(page);
  await openNavigation(page, 'Pull requests', !!isMobile);
  const empty = page.getByRole('heading', { name: 'Room for your next improvement.', exact: true });
  await expect(empty).toBeVisible();
  const target = page.locator('.published-panel .task-row').first();
  await target.scrollIntoViewIfNeeded();
  const position = (await target.boundingBox())!;
  const documentPosition = () =>
    target.evaluate((row) => {
      const { x, y, width, height } = row.getBoundingClientRect();
      return { x: x + scrollX, y: y + scrollY, width, height };
    });
  const savedPosition = await documentPosition();
  gate = deferred();
  try {
    await expect(page.getByText('Refreshing pull requests…', { exact: true })).toBeVisible({
      timeout: 10000
    });
    expect(await target.boundingBox()).toEqual(position);
    await expect(empty).toBeVisible();
    fail = true;
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText('Showing the last received results');
    await expect(empty).toBeVisible();
    expect(await documentPosition()).toEqual(savedPosition);
    fail = false;
    gate = deferred();
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.getByText('Refreshing pull requests…', { exact: true })).toBeVisible();
    expect(await documentPosition()).toEqual(savedPosition);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByText('Refreshing pull requests…', { exact: true })).toHaveCount(0);
    await expect(empty).toBeVisible();
    expect(await documentPosition()).toEqual(savedPosition);
  } finally {
    gate?.resolve();
  }
});

test('the attention link opens the attention queue, and the list search answers to its keys and clear button', async ({
  page
}) => {
  const queries: { status: string | null; q: string | null }[] = [];
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname === '/api/tasks')
      queries.push({ status: url.searchParams.get('status'), q: url.searchParams.get('q') });
  });
  await login(page);
  const blocked = 'Handle interrupted verification commands';
  const attention = page.getByRole('region', { name: /^Needs attention/ });
  await expect(attention.getByRole('button', { name: new RegExp(blocked) })).toBeVisible();
  // The link opens the queue first; the queue's reset must not undo the attention filter.
  await attention.getByRole('button', { name: 'View all unresolved work' }).click();
  await expect(page.getByRole('heading', { name: 'From idea to improvement.' })).toBeVisible();
  const filters = page.getByRole('group', { name: 'Task filters' });
  await expect(filters.getByRole('button', { name: 'attention', exact: true })).toHaveAttribute(
    'aria-pressed',
    'true'
  );
  const rows = page.locator('.task-row');
  const setup = rows.filter({ hasText: 'Complete the repository setup flow' });
  await expect(rows.filter({ hasText: blocked })).toHaveCount(1);
  await expect(setup).toHaveCount(0);
  await expect.poll(() => queries.at(-1)).toEqual({ status: 'attention', q: '' });

  await filters.getByRole('button', { name: 'all', exact: true }).click();
  await expect(setup).toHaveCount(1);
  const search = page.getByLabel('Search work');
  // "/" moves focus into the search from outside a field without typing the slash.
  await page.keyboard.press('/');
  await expect(search).toBeFocused();
  await expect(search).toHaveValue('');
  await page.keyboard.type('documentation');
  await expect(setup).toHaveCount(0);
  await expect(rows.filter({ hasText: 'Explain the local development workflow' })).toHaveCount(1);
  await expect.poll(() => queries.at(-1)).toEqual({ status: 'all', q: 'documentation' });

  // Escape is spent clearing a query; with nothing left to clear it reaches the page.
  await page.evaluate(() => {
    const counter = window as unknown as { escapes: number };
    counter.escapes = 0;
    window.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') counter.escapes++;
    });
  });
  const escapes = () => page.evaluate(() => (window as unknown as { escapes: number }).escapes);
  await page.keyboard.press('Escape');
  await expect(search).toHaveValue('');
  await expect(setup).toHaveCount(1);
  await expect.poll(() => queries.at(-1)).toEqual({ status: 'all', q: '' });
  expect(await escapes()).toBe(0);
  await page.keyboard.press('Escape');
  expect(await escapes()).toBe(1);

  await search.fill('documentation');
  await expect(setup).toHaveCount(0);
  const clear = page.getByRole('button', { name: 'Clear search', exact: true });
  await clear.click();
  await expect(search).toHaveValue('');
  await expect(clear).toHaveCount(0);
  await expect(setup).toHaveCount(1);
  await expect.poll(() => queries.at(-1)).toEqual({ status: 'all', q: '' });
});

test('polling keeps the second task under the same mouse position', async ({ page, isMobile }) => {
  let gate: ReturnType<typeof deferred> | null = null;
  await page.route('**/api/tasks?*', async (route) => {
    await gate?.promise;
    await route.continue();
  });
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  const target = page.locator('.task-row').nth(1);
  await expect(target).toBeVisible();
  await target.scrollIntoViewIfNeeded();
  await expect(target).toBeInViewport({ ratio: 1 });
  const title = await target.locator('strong').innerText();
  const position = (await target.boundingBox())!;
  gate = deferred();
  try {
    await expect(page.getByText('Refreshing tasks…', { exact: true })).toBeVisible({
      timeout: 10000
    });
    expect(await target.boundingBox()).toEqual(position);
    // Use the original coordinates: a locator click would follow a shifted row.
    await page.mouse.click(position.x + position.width / 2, position.y + 12);
    await expect(
      page.getByRole('dialog').getByRole('heading', { name: title, exact: true })
    ).toBeVisible();
  } finally {
    gate.resolve();
    gate = null;
  }
});

test('polling that adds a task keeps keyboard focus on the same task', async ({
  page,
  isMobile
}) => {
  let prepend = false;
  await page.route('**/api/tasks?*', async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as { items: TaskRow[] };
    if (prepend) body.items.unshift({ ...body.items[0], id: 'task-new', title: 'Brand new task' });
    await route.fulfill({ response, json: body });
  });
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  const rows = page.locator('.task-row');
  const target = rows.nth(1);
  await expect(target).toBeVisible();
  const title = await target.locator('strong').innerText();
  await target.focus();
  prepend = true;
  await expect(rows.first()).toContainText('Brand new task', { timeout: 10000 });
  expect(
    await page.evaluate(() => document.activeElement?.querySelector('strong')?.textContent)
  ).toBe(title);
  await page.keyboard.press('Enter');
  await expect(
    page.getByRole('dialog').getByRole('heading', { name: title, exact: true })
  ).toBeVisible();
});

test('polling that adds a pull request keeps keyboard focus on the same link', async ({
  page,
  isMobile
}) => {
  let prepend = false;
  await page.route('**/api/prs?*', async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as { items: PrObservation[] };
    const [first] = body.items;
    if (prepend)
      body.items.unshift({
        ...first,
        pr: {
          ...first.pr,
          number: 99,
          title: 'Brand new pull request',
          url: first.pr.url.replace(/\d+$/, '99')
        }
      });
    await route.fulfill({ response, json: body });
  });
  await login(page);
  await openNavigation(page, 'Pull requests', !!isMobile);
  const rows = page.locator('.pr-row');
  const target = rows.nth(1);
  await expect(target).toBeVisible();
  const href = await target.getAttribute('href');
  await target.focus();
  prepend = true;
  await expect(rows.first()).toContainText('Brand new pull request', { timeout: 10000 });
  expect(await page.evaluate(() => document.activeElement?.getAttribute('href'))).toBe(href);
});

test('a polled change to task actions keeps keyboard focus on the same action', async ({
  page,
  isMobile
}) => {
  let withoutCancel = false;
  await page.route('**/api/tasks/task-blocked', async (route) => {
    const response = await route.fetch();
    const task = (await response.json()) as Task;
    if (withoutCancel) task.allowed_actions = task.allowed_actions.filter((a) => a !== 'cancel');
    await route.fulfill({ response, json: task });
  });
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  await page.getByRole('button', { name: /Handle interrupted verification commands/ }).click();
  const dialog = page.getByRole('dialog');
  const cancel = dialog.getByRole('button', { name: 'Cancel task' });
  await expect(cancel).toBeVisible();
  // Archive follows Cancel, so removing Cancel shifts every later action.
  const archive = dialog.getByRole('button', { name: 'Archive task' });
  await archive.focus();
  withoutCancel = true;
  await expect(cancel).toHaveCount(0, { timeout: 10000 });
  await expect(archive).toBeFocused();
});

test('a record identity stays one path segment in every request', async ({ page, isMobile }) => {
  // Service identities are UUIDs, but no path may rely on that: an identity holding a
  // reserved character must still address its own record, never another endpoint.
  const taskId = 'synthetic/task?one#two';
  const cycleId = 'synthetic/cycle?one#two';
  const requests: string[] = [];
  page.on('request', (request) => {
    const { pathname } = new URL(request.url());
    if (/^\/api\/(tasks|cycles)\//.test(pathname)) requests.push(`${request.method()} ${pathname}`);
  });
  await page.route('**/api/tasks?*', async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as { items: TaskRow[] };
    body.items = body.items.map((row) =>
      row.id === 'task-blocked' ? { ...row, id: taskId } : row
    );
    await route.fulfill({ response, json: body });
  });
  const taskPath = `/api/tasks/${encodeURIComponent(taskId)}`;
  await page.route(
    (url) => url.pathname === taskPath,
    async (route) => {
      const url = new URL('/api/tasks/task-blocked', route.request().url()).toString();
      const response = await route.fetch({ url });
      const task = (await response.json()) as Task;
      await route.fulfill({ response, json: { ...task, id: taskId } });
    }
  );
  await page.route(
    (url) => url.pathname === `${taskPath}/archive`,
    (route) => route.fulfill({ json: { ok: true } })
  );
  await page.route('**/api/cycles?*', async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.items = body.items.map((cycle: { id: string }) =>
      cycle.id === 'cycle-1' ? { ...cycle, id: cycleId } : cycle
    );
    await route.fulfill({ response, json: body });
  });
  const cyclePath = `/api/cycles/${encodeURIComponent(cycleId)}`;
  await page.route(
    (url) => url.pathname === `${cyclePath}/archive`,
    (route) => route.fulfill({ json: { ok: true } })
  );
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  await page.getByRole('button', { name: /Handle interrupted verification commands/ }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('button', { name: 'Archive task' }).click();
  await expect.poll(() => requests).toContain(`POST ${taskPath}/archive`);
  expect(requests).toContain(`GET ${taskPath}`);
  await dialog.getByRole('button', { name: 'Close task details' }).click();
  await openNavigation(page, 'Proposals', !!isMobile);
  await page.getByLabel('Cycle', { exact: true }).selectOption(cycleId);
  await page.getByRole('button', { name: 'Archive cycle', exact: true }).click();
  await expect.poll(() => requests).toContain(`POST ${cyclePath}/archive`);
  // No request ever split an identity into a second path segment.
  expect(requests.filter((request) => request.includes('/synthetic/'))).toEqual([]);
});

test('an action taken while a poll is in flight shows the state after the action', async ({
  page
}) => {
  await page.clock.install();
  let paused = true;
  let hold: ReturnType<typeof deferred> | null = null;
  let held = 0;
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.configured = true;
    snapshot.control.paused = paused;
    snapshot.control.mode = paused ? 'paused' : 'continuous';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = false;
    snapshot.active_cycle_mode = null;
    snapshot.baseline_active = false;
    const gate = hold;
    if (gate) {
      held++;
      await gate.promise;
    }
    await route.fulfill({ json: snapshot });
  });
  await page.route('**/api/control/resume', async (route) => {
    paused = false;
    await route.fulfill({ json: { paused: false } });
  });
  await login(page);
  const start = page.getByRole('button', { name: 'Start continuous', exact: true });
  await expect(start).toBeEnabled();
  // From here on, polls run only when the test advances the clock.
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
  // One timer poll reads the paused snapshot and is held before it lands. A tick is
  // skipped while an earlier poll is still landing, so advance until one is held.
  const gate = (hold = deferred());
  await expect
    .poll(async () => {
      if (!held) await page.clock.runFor(4000);
      return held;
    })
    .toBe(1);
  hold = null;
  const resumed = page.waitForResponse('**/api/control/resume');
  await start.click();
  await resumed;
  // The action has finished while the stale poll is still outstanding.
  await expect(start).toBeEnabled();
  gate.resolve();
  // No clock advance: the post-action snapshot must not wait for the next poll.
  await expect(page.getByRole('button', { name: 'Pause', exact: true })).toBeVisible();
});

test('header and empty discovery actions share eligibility and prevent duplicate pending controls', async ({
  page
}) => {
  let restriction = 'continuous';
  const writes: string[] = [];
  const gate = deferred();
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.configured = true;
    snapshot.audit_configured = true;
    snapshot.status = restriction;
    snapshot.control.paused = restriction !== 'continuous';
    snapshot.active_tasks = restriction === 'task' ? 1 : 0;
    snapshot.cycle_active = ['execution', 'audit'].includes(restriction);
    snapshot.active_cycle_mode =
      restriction === 'audit' ? 'audit' : restriction === 'execution' ? 'execution' : null;
    snapshot.tasks = [];
    snapshot.attention_tasks = [];
    snapshot.counts = {};
    await route.fulfill({ json: snapshot });
  });
  await page.route('**/api/control/*', async (route) => {
    writes.push(new URL(route.request().url()).pathname);
    await gate.promise;
    restriction = 'continuous';
    await route.fulfill({ json: { paused: false } });
  });
  await login(page);
  for (const value of ['continuous', 'task', 'execution', 'audit', 'idle']) {
    restriction = value;
    const run = page.getByRole('button', { name: 'Run once', exact: true });
    if (value === 'idle') {
      await expect(run).toBeEnabled({ timeout: 10000 });
      await expect(
        page.getByRole('button', { name: 'Discover opportunities', exact: true })
      ).toBeEnabled();
      await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeEnabled();
    } else {
      await expect(page.locator('.status-value')).toHaveText(value, { timeout: 10000 });
      await expect(run).toBeDisabled();
      const discover = page.getByRole('button', { name: 'Discover opportunities', exact: true });
      if (value === 'task') await expect(discover).toHaveCount(0);
      else await expect(discover).toBeDisabled();
      await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeDisabled();
    }
  }
  expect(writes).toEqual([]);
  await page.getByRole('button', { name: 'Discover opportunities', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Starting run…', exact: true })).toHaveCount(2);
  for (const button of await page.getByRole('button', { name: 'Starting run…', exact: true }).all())
    await expect(button).toBeDisabled();
  await page
    .getByRole('button', { name: 'Starting run…', exact: true })
    .first()
    .dispatchEvent('click');
  gate.resolve();
  await expect(page.getByRole('button', { name: 'Run once', exact: true })).toBeDisabled();
  expect(writes).toEqual(['/api/control/cycle']);
});

test('the header names the operating mode, and the continuous toggle names its pending action', async ({
  page
}) => {
  let mode: Snapshot['control']['mode'] = 'paused';
  let activeTasks = 0;
  let gate = deferred();
  const writes: string[] = [];
  await page.route('**/api/state', async (route) => {
    const snapshot: Snapshot = await (await route.fetch()).json();
    snapshot.configured = true;
    snapshot.control.mode = mode;
    snapshot.control.paused = mode !== 'continuous';
    snapshot.active_tasks = activeTasks;
    snapshot.cycle_active = false;
    snapshot.active_cycle_mode = null;
    snapshot.baseline_active = false;
    await route.fulfill({ json: snapshot });
  });
  await page.route('**/api/control/*', async (route) => {
    const action = new URL(route.request().url()).pathname.split('/').at(-1)!;
    writes.push(action);
    await gate.promise;
    mode = action === 'resume' ? 'continuous' : 'paused';
    await route.fulfill({ json: { paused: mode === 'paused' } });
  });
  await login(page);
  const status = page.getByRole('status', { name: 'Operating mode' });
  await expect(status).toHaveText('New work paused · 0 active tasks');
  // Paused work that is still running may publish, and the status says so.
  activeTasks = 2;
  await expect(status).toHaveText(
    'New work paused · 2 active tasks · active workflows may publish',
    { timeout: 10000 }
  );
  mode = 'run_once';
  await expect(status).toHaveText('Run once · 2 active tasks · active workflows may publish', {
    timeout: 10000
  });
  activeTasks = 0;
  await expect(status).toHaveText('Run once · 0 active tasks', { timeout: 10000 });

  await page.getByRole('button', { name: 'Start continuous', exact: true }).click();
  await expect(
    page.getByRole('button', { name: 'Starting continuous…', exact: true })
  ).toBeDisabled();
  gate.resolve();
  await expect(status).toHaveText('Continuous operation · 0 active tasks');
  gate = deferred();
  await page.getByRole('button', { name: 'Pause', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Pausing…', exact: true })).toBeDisabled();
  gate.resolve();
  await expect(status).toHaveText('New work paused · 0 active tasks');
  await expect(page.getByRole('button', { name: 'Start continuous', exact: true })).toBeEnabled();
  expect(writes).toEqual(['resume', 'pause']);
});
