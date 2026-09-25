import { createHash } from 'node:crypto';
import { test, expect, type Page } from '@playwright/test';
import type {
  BaselineView,
  Config,
  Model,
  SettingsView,
  Snapshot,
  TransformedField
} from '../src/lib/types';
import { login, openNavigation, token, trackWrites } from './synthetic';

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

// All writes terminate in browser fixtures; neither runner nor GitHub is contacted.
async function configurationFixture(
  page: Page,
  options: {
    unconfigured?: boolean;
    snapshot?: (snapshot: Snapshot) => void;
    /** Display-only field values plus their transform metadata, as the server reports them. */
    transformed?: { overrides: Partial<Config>; fields: TransformedField[] };
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
      if (state.failSave || body.expected_revision !== state.revision()) {
        await route.fulfill({ status: 409, json: { error: 'Synthetic save conflict' } });
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
        repair_route: { ...model }
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
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
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

test('configuration keeps drafts and catalogs across views, discards locally, and refreshes clean values', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
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
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Check audit connection' })).toBeDisabled();
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
    const navigate = (name: string) => openNavigation(page, name, !!isMobile);
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
      state.saved = JSON.parse(
        JSON.stringify(state.saved, (_key, value) =>
          value && typeof value === 'object' && !Array.isArray(value)
            ? Object.fromEntries(Object.entries(value).reverse())
            : value
        )
      );
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
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
  await login(page);
  await navigate('Configuration');
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });
  const loaded = state.revision();

  // The served preview is marked and locked; the hidden value is never editable.
  await expect(commands).toHaveValue('echo [redacted] > /dev/null');
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
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
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
  state.saved = JSON.parse(
    JSON.stringify(state.saved, (_key, value) =>
      value && typeof value === 'object' && !Array.isArray(value)
        ? Object.fromEntries(Object.entries(value).sort(([a], [b]) => a.localeCompare(b)))
        : value
    )
  );
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
    const navigate = (name: string) => openNavigation(page, name, !!isMobile);
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
    state.saved = JSON.parse(
      JSON.stringify(state.saved, (_key, value) =>
        value && typeof value === 'object' && !Array.isArray(value)
          ? Object.fromEntries(Object.entries(value).reverse())
          : value
      )
    );
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
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
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
