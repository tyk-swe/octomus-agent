import { createHash } from 'node:crypto';
import { expect, type Page } from '@playwright/test';
import type {
  BaselineView,
  Config,
  Model,
  SettingsView,
  Snapshot,
  TransformedField
} from '../src/lib/types';
import { login, openNavigation, test, trackWrites, unsandboxed } from './synthetic';

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

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

async function configurationFixture(
  page: Page,
  options: {
    unconfigured?: boolean;
    snapshot?: (snapshot: Snapshot) => void;
    transformed?: { overrides: Partial<Config>; fields: TransformedField[] };
    saved?: Partial<Config>;
  } = {}
) {
  const state = {
    saved: null as Config | null,
    overrides: {} as Record<string, unknown>,
    transformed: [] as TransformedField[],
    reads: 0,
    stateReads: 0,
    failStateRead: false,
    stateGates: [] as ReturnType<typeof deferred>[],
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
    const gate = state.stateGates.shift();
    const fail = state.failStateRead;
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.control.paused = true;
    snapshot.control.mode = 'paused';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = false;
    snapshot.active_cycle_mode = null;
    options.snapshot?.(snapshot);
    state.stateReads++;
    await gate?.promise;
    await route.fulfill(
      fail
        ? { status: 503, json: { error: 'Synthetic dashboard read outage' } }
        : { json: snapshot }
    );
  });
  await page.route('**/api/config', async (route) => {
    if (route.request().method() === 'PUT') {
      const body = route.request().postDataJSON() as {
        expected_revision: string;
        config: Record<string, unknown>;
      };
      state.writes.push(body);
      await state.saveGate?.promise;
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

  state.saved!.default_branch = 'external-main';
  await branch.fill('stale-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  await expect(branch).toHaveValue('stale-main');
  expect(state.reads).toBe(reads);
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(alert).toHaveCount(0);
  await expect(branch).toHaveValue('fixture-main');
  await branch.fill('stale-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  await expect(reload).toBeVisible();
  expect(state.writes).toHaveLength(2);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

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

  await reload.click();
  await expect(branch).toHaveValue('external-main');
  await expect(saved).toHaveText('Edits discarded. Saved configuration reloaded.');
  await expect(alert).toHaveCount(0);
  await expect(reload).toHaveCount(0);
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
  expect(state.reads).toBe(reads + 2);
  expect(state.writes).toHaveLength(3);

  await branch.fill('current-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes.at(-1)!.config).toEqual({ default_branch: 'current-main' });
  expect(state.saved!.default_branch).toBe('current-main');

  state.failSave = true;
  await branch.fill('paused-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toHaveText('Synthetic save conflict');
  await expect(reload).toHaveCount(0);
  await expect(branch).toHaveValue('paused-main');
  expect(state.writes).toHaveLength(5);
  expect(state.reads).toBe(reads + 2);
  state.failSave = false;

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
  const state = await configurationFixture(page, { snapshot: unsandboxed });
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
  await expect(badge('sandbox')).toHaveText(/^Proven · /);
  await expect(step('sandbox')).toContainText('containment checks passed from inside a sandbox');
  await expect(page.getByText('1 of 7 steps saved, checked or run')).toBeVisible();

  await page.getByLabel('Repository path').fill('/fixture/entered');
  await page.getByLabel('GitHub repository').fill('fixture/entered');
  await expect(badge('repository')).toHaveText('Entered, not saved');
  await expect(badge('preflight')).toHaveText('Unsaved edits');
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Load Codex models' }).click();

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

  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect(badge('repository')).toHaveText('Saved');
  await expect(badge('routes')).toHaveText('Saved');
  await expect(badge('verification')).toHaveText('Saved');
  await expect(badge('preflight')).toHaveText('Not checked');
  await expect(step('preflight')).toContainText('does not prove repository push permission');
  await expect(page.getByText('4 of 7 steps saved, checked or run')).toBeVisible();
  expect(state.writes).toHaveLength(2);

  await step('preflight').getByRole('button', { name: 'Open the execution check' }).click();
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeFocused();
  expect(state.checks).toEqual([]);
  await page.keyboard.press('Enter');
  await expect(badge('preflight')).toHaveText(/^Passed · execution · /);
  await expect.poll(() => state.checks).toEqual(['execution']);
  await expect(page.getByText('5 of 7 steps saved, checked or run')).toBeVisible();

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
  await expect(page.getByText('5 of 7 steps saved, checked or run')).toBeVisible();
  expect(state.checks).toEqual(['execution']);

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
