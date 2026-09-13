import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
const token = 'browser-test-operator-token-32-characters';

const codexModels = ['gpt-6-astra', 'gpt-5.6-luna'].map((model) => ({
  backend: 'codex',
  provider: null,
  provider_name: null,
  model,
  display_name: model,
  efforts: ['low', 'medium', 'high', 'xhigh', 'max'],
  variants: [],
  available: true,
  unavailable_reason: null
}));
const opencodeModels = [
  { provider: 'fixture', model: 'fixture-model', variants: ['low', 'high'] },
  { provider: 'fixture', model: 'plain-model', variants: [] },
  { provider: 'alternate', model: 'fixture-model', variants: ['deep'] }
].map((model) => ({
  backend: 'opencode',
  provider_name: model.provider,
  display_name: model.model,
  efforts: [],
  available: true,
  unavailable_reason: null,
  ...model
}));

test.beforeEach(async ({ page }) => {
  await page.route('**/api/model-catalog', async (route) => {
    const { backend } = route.request().postDataJSON();
    await route.fulfill({ json: backend === 'codex' ? codexModels : opencodeModels });
  });
});

test('private dashboard, navigation, task evidence, configuration, and mobile layout', async ({
  page
}, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible();
  await page.getByLabel('Operator access token').fill('incorrect');
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('alert')).toContainText('operator access token');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Run once' })).toBeDisabled();
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(0);
  const accessibility = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze();
  expect(
    accessibility.violations.map((v) => ({ rule: v.id, elements: v.nodes.map((n) => n.target) }))
  ).toEqual([]);
  await page.screenshot({
    path: `test-results/${testInfo.project.name}-overview.png`,
    fullPage: true
  });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  async function navigate(name: string) {
    if (testInfo.project.name === 'mobile')
      await page.getByRole('button', { name: 'Toggle navigation' }).click();
    await page.getByRole('navigation').getByRole('button', { name, exact: true }).click();
  }
  await navigate('Task queue');
  await page.getByLabel('Search work').fill('documentation');
  await expect(
    page.getByRole('button', { name: /Explain the local development workflow/ })
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: /Complete the repository setup flow/ })
  ).toHaveCount(0);
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await expect(page.getByRole('dialog')).toBeVisible();
  await page.getByRole('tab', { name: /Reviews/ }).click();
  await expect(
    page.getByText('The full change set meets the objective without actionable findings.')
  ).toBeVisible();
  await page.getByRole('tab', { name: 'Verification' }).click();
  await expect(page.getByText('Passed', { exact: true })).toBeVisible();
  await page.getByText('Command output', { exact: true }).click();
  await expect(page.getByText('All tests passed.')).toBeVisible();
  await page.getByRole('button', { name: 'Close task details' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await navigate('Proposals');
  await expect(page.getByRole('heading', { name: 'Worth doing. Before doing.' })).toBeVisible();
  await page.getByRole('button', { name: 'rejected', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'No matching proposals' })).toBeVisible();
  await navigate('Pull requests');
  await expect(
    page.getByRole('link', { name: /Explain the local development workflow/ })
  ).toHaveAttribute('href', 'https://github.com/fixture/project/pull/12');
  await navigate('Configuration');
  await page.getByLabel('Orchestrator runner').selectOption('codex');
  await page.getByLabel('Repair runner').selectOption('codex');
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await page.getByLabel('Orchestrator model', { exact: true }).fill('gpt-6-astra');
  await page.getByLabel('Orchestrator reasoning effort', { exact: true }).selectOption('medium');
  await page.getByLabel('Repair model', { exact: true }).fill('gpt-5.6-luna');
  await page.getByLabel('Repair reasoning effort', { exact: true }).selectOption('high');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('status')).toHaveText('Configuration saved.');
  await page.waitForTimeout(4500); // Ensure state polling never overwrites an operator's draft.
  await expect(page.getByLabel('Orchestrator model', { exact: true })).toHaveValue('gpt-6-astra');
  await expect(page.getByLabel('Repair model', { exact: true })).toHaveValue('gpt-5.6-luna');
  await navigate('Overview');
  await navigate('Configuration');
  await expect(page.getByLabel('Repair reasoning effort', { exact: true })).toHaveValue('high');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await navigate('Overview');
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  expect(errors).toEqual([]);
});

test('one-shot audit progress, decisions and paused controls', async ({ page }, testInfo) => {
  let running = false;
  let finished = false;
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const state = await response.json();
    state.configured = false;
    state.audit_configured = true;
    state.active_tasks = 0;
    state.control.paused = true;
    state.status = running ? 'auditing' : 'paused';
    state.cycle_active = running;
    state.active_cycle_mode = running ? 'audit' : null;
    if (running || finished) {
      const cycle = structuredClone(state.cycles[0]);
      cycle.id = 'audit-fixture';
      cycle.number = 2;
      cycle.mode = 'audit';
      cycle.status = running ? 'running' : 'completed';
      cycle.decisions = { accepted: 1, rejected: 1, deferred: 1 };
      state.cycles.unshift(cycle);
    }
    await route.fulfill({ response, json: state });
  });
  await page.route('**/api/cycles?*', async (route) => {
    const response = await route.fetch();
    const result = await response.json();
    if (running || finished)
      result.items.unshift({
        ...result.items[0],
        id: 'audit-fixture',
        number: 2,
        mode: 'audit',
        status: running ? 'running' : 'completed'
      });
    await route.fulfill({ response, json: result });
  });
  await page.route('**/api/proposals?*', async (route) => {
    const response = await route.fetch();
    const result = await response.json();
    if (running || finished) {
      const seed = result.items[0] ?? {
        target: 'main',
        tier: 'M',
        category: 'features',
        problem: 'Concrete evidence',
        scope: 'Small scope',
        benefit: 'Useful',
        evidence: [],
        dependencies: [],
        prompt: ''
      };
      const status = new URL(route.request().url()).searchParams.get('status');
      result.items = finished
        ? ['accepted', 'rejected', 'deferred']
            .filter((d) => status === 'all' || d === status)
            .map((decision, index) => ({
              ...seed,
              id: `audit-${index}`,
              cycle_id: 'audit-fixture',
              cycle: 2,
              mode: 'audit',
              title: `Audit ${decision} recommendation`,
              decision,
              reason: `${decision}: both adversaries considered the concrete evidence.`
            }))
        : [];
      result.counts = { accepted: 1, rejected: 1, deferred: 1 };
    }
    await route.fulfill({ response, json: result });
  });
  await page.route('**/api/control/audit', async (route) => {
    expect(route.request().method()).toBe('POST');
    running = true;
    await route.fulfill({ json: { paused: true } });
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('button', { name: 'Run once' })).toBeDisabled();
  await page.getByRole('button', { name: 'Run an audit' }).click();
  await expect(page.getByRole('heading', { name: 'Worth doing. Before doing.' })).toBeVisible();
  await expect(page.getByRole('status')).toContainText('Audit in progress');
  await expect(page.getByRole('button', { name: 'Start continuous', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Run an audit' })).toBeDisabled();
  running = false;
  finished = true;
  await expect(page.getByRole('heading', { name: 'Audit rejected recommendation' })).toBeVisible({
    timeout: 10000
  });
  await page.getByLabel('Cycle', { exact: true }).selectOption('audit-fixture');
  await expect(page.getByLabel('Decision counts')).toContainText('rejected: 1');
  await page.getByRole('button', { name: 'rejected', exact: true }).click();
  await expect(
    page.getByText('rejected: both adversaries considered the concrete evidence.')
  ).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Audit accepted recommendation' })).toHaveCount(0);
  const accessibility = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze();
  expect(accessibility.violations).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({
    path: `test-results/${testInfo.project.name}-audit-fixture.png`,
    fullPage: true
  });
});

test('model routing across all roles, provider variants, draft catalogs and unavailable selections', async ({
  page
}, testInfo) => {
  let catalogState: 'normal' | 'removed' | 'error' = 'normal';
  const drafts: { backend: string; binary: string }[] = [];
  await page.route('**/api/model-catalog', async (route) => {
    drafts.push(route.request().postDataJSON());
    if (catalogState === 'error') {
      await route.fulfill({ status: 400, json: { error: 'OpenCode catalog is unavailable' } });
    } else {
      await route.fulfill({ json: catalogState === 'normal' ? opencodeModels : [] });
    }
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  async function navigate(name: string) {
    if (testInfo.project.name === 'mobile')
      await page.getByRole('button', { name: 'Toggle navigation' }).click();
    await page.getByRole('navigation').getByRole('button', { name, exact: true }).click();
  }
  await navigate('Configuration');
  await page.getByLabel('OpenCode executable', { exact: true }).fill('/draft/opencode');
  await page.getByRole('button', { name: 'Load OpenCode models' }).click();
  expect(drafts).toEqual([{ backend: 'opencode', binary: '/draft/opencode' }]);
  const names = [
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
  for (const name of names) {
    await page.getByLabel(name + ' runner', { exact: true }).selectOption('opencode');
    await page.getByLabel(name + ' provider', { exact: true }).selectOption('fixture');
    await page.getByLabel(name + ' model', { exact: true }).fill('fixture-model');
    await page.getByLabel(name + ' variant', { exact: true }).selectOption('high');
    await expect(page.getByLabel(name + ' reasoning effort', { exact: true })).toHaveCount(0);
  }
  await page.getByLabel('XS execution model', { exact: true }).fill('plain-model');
  await expect(page.getByLabel('XS execution variant', { exact: true })).toHaveValue('');
  await expect(
    page.getByLabel('XS execution variant', { exact: true }).locator('option')
  ).toHaveCount(1);
  await page.getByLabel('Repair provider', { exact: true }).selectOption('alternate');
  await expect(page.getByLabel('Repair model', { exact: true })).toHaveValue('');
  await page.getByLabel('Repair model', { exact: true }).fill('fixture-model');
  await page.getByLabel('Repair variant', { exact: true }).selectOption('deep');
  await expect(
    page.getByLabel('Repair variant', { exact: true }).locator('option[value="high"]')
  ).toHaveCount(0);

  catalogState = 'removed';
  await page.getByRole('button', { name: 'Load OpenCode models' }).click();
  await expect(page.getByRole('group', { name: 'Repair route', exact: true })).toContainText(
    'not in the loaded catalog'
  );
  await expect(page.getByLabel('Repair variant', { exact: true })).toHaveValue('deep');
  catalogState = 'error';
  await page.getByRole('button', { name: 'Load OpenCode models' }).click();
  await expect(page.getByRole('alert')).toContainText('catalog is unavailable');
  await page.waitForTimeout(4500);
  await expect(page.getByLabel('Repair model', { exact: true })).toHaveValue('fixture-model');
  await expect(page.getByLabel('Repair variant', { exact: true })).toHaveValue('deep');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('status')).toHaveText('Configuration saved.');
  await navigate('Overview');
  await navigate('Configuration');
  for (const name of names)
    await expect(page.getByLabel(name + ' runner', { exact: true })).toHaveValue('opencode');
  await expect(page.getByLabel('Repair provider', { exact: true })).toHaveValue('alternate');
  await expect(page.getByLabel('Repair variant', { exact: true })).toHaveValue('deep');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const accessibility = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze();
  expect(accessibility.violations).toEqual([]);
  await page.screenshot({
    path: `test-results/${testInfo.project.name}-model-routes.png`,
    fullPage: true
  });
});

// Synthetic catalog/configuration fixtures only; these tests never call live models.
const rehearsalRoutes = [
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

async function rehearsalFixture(page: import('@playwright/test').Page, mobile: boolean) {
  const writes: { path: string; body: unknown }[] = [];
  let initial: import('../src/lib/types').Config;
  await page.route('**/api/config', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fulfill({ json: { ok: true } });
      return;
    }
    const response = await route.fetch();
    const config = await response.json();
    const oldRoute = {
      backend: 'opencode',
      provider: 'fixture',
      model: 'fixture-model',
      variant: 'high',
      effort: ''
    };
    initial = {
      ...config,
      repository: '/fixture/repository',
      github_repo: 'fixture/rehearsal',
      default_branch: 'fixture-main',
      branch_prefix: 'tyk/fixture/',
      codex_binary: '/fixture/codex',
      opencode_binary: '/fixture/opencode',
      roles: Object.fromEntries(Object.keys(config.roles).map((key) => [key, { ...oldRoute }])),
      tiers: Object.fromEntries(Object.keys(config.tiers).map((key) => [key, { ...oldRoute }])),
      repair_route: { ...oldRoute },
      categories: ['correctness', 'documentation'],
      verification_commands: ['fixture saved command'],
      discovery_agents: 8,
      execution_concurrency: 3,
      max_tasks_per_cycle: 7,
      cycle_interval_seconds: 900,
      maintenance_every_cycles: 4,
      large_pr_lines: 456,
      long_lived_pr_days: 11,
      max_repair_rounds: 6,
      max_no_progress_rounds: 3,
      max_retries: 4,
      session_timeout_seconds: 789,
      task_timeout_seconds: 9876,
      command_timeout_seconds: 321,
      max_sessions_per_day: 73,
      max_workspace_bytes: 123456789,
      runner_storage_paths: {
        codex: '/fixture/storage/codex',
        opencode: '/fixture/storage/opencode'
      },
      retain_completed_days: 23,
      retain_events: 2345
    };
    await route.fulfill({ json: initial });
  });
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname;
    if (path.startsWith('/api/') && request.method() !== 'GET' && path !== '/api/model-catalog')
      writes.push({ path, body: request.postDataJSON() });
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  if (mobile) await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Configuration', exact: true })
    .click();
  await expect(page.getByLabel('Codex executable', { exact: true })).toHaveValue('/fixture/codex');
  return { writes, initial: initial! };
}

async function draftInputs(page: import('@playwright/test').Page) {
  return page.locator('form input, form select, form textarea').evaluateAll((inputs) =>
    inputs
      .filter((input) => !input.closest('[aria-label="Astra rehearsal preset"]'))
      .map((input) => {
        const field = input as HTMLInputElement;
        return { value: field.value, checked: field.checked };
      })
  );
}

test('Astra rehearsal fixture: confirmation, cancellation, all routes and unrelated draft preservation', async ({
  page
}, testInfo) => {
  const { writes, initial } = await rehearsalFixture(page, testInfo.project.name === 'mobile');
  const commands = '  fixture unsaved test  \n\nfixture unsaved build\n';
  await page.getByRole('textbox', { name: /^Verification commands/ }).fill(commands);
  await page.getByLabel('Default branch', { exact: true }).fill('unsaved-main');
  await page.getByLabel('Repair rounds').fill('9');
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await expect(page.getByLabel('Astra rehearsal effort')).toHaveValue('medium');
  await page.getByLabel('Astra rehearsal effort').selectOption('high');
  const before = await draftInputs(page);
  const apply = page.getByRole('button', { name: 'Apply Astra rehearsal preset', exact: true });
  await apply.click();
  const confirmation = page.getByRole('group', {
    name: 'Confirm Astra rehearsal preset',
    exact: true
  });
  await expect(confirmation).toContainText('Codex / gpt-6-astra / high');
  await expect(confirmation).toContainText('21600 (6 hours)');
  expect(await draftInputs(page)).toEqual(before);
  expect(writes).toEqual([]);
  await confirmation.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(confirmation).toHaveCount(0);
  expect(await draftInputs(page)).toEqual(before);
  expect(writes).toEqual([]);
  await apply.click();
  await confirmation.getByRole('button', { name: 'Confirm preset', exact: true }).click();
  for (const name of rehearsalRoutes) {
    await expect(page.getByLabel(name + ' runner', { exact: true })).toHaveValue('codex');
    await expect(page.getByLabel(name + ' model', { exact: true })).toHaveValue('gpt-6-astra');
    await expect(page.getByLabel(name + ' reasoning effort', { exact: true })).toHaveValue('high');
    await expect(page.getByLabel(name + ' provider', { exact: true })).toHaveCount(0);
    await expect(page.getByLabel(name + ' variant', { exact: true })).toHaveCount(0);
  }
  await expect(page.getByRole('textbox', { name: /^Verification commands/ })).toHaveValue(commands);
  await expect(page.getByText(/Astra rehearsal preset applied to the unsaved draft/)).toBeVisible();
  expect(writes).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  expect(
    (await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze()).violations
  ).toEqual([]);
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect.poll(() => writes.length).toBe(1);
  const newRoute = { backend: 'codex', model: 'gpt-6-astra', effort: 'high' };
  expect(writes).toEqual([
    {
      path: '/api/config',
      body: {
        ...initial,
        default_branch: 'unsaved-main',
        max_repair_rounds: 9,
        verification_commands: ['fixture unsaved test', 'fixture unsaved build'],
        roles: Object.fromEntries(Object.keys(initial.roles).map((key) => [key, newRoute])),
        tiers: Object.fromEntries(Object.keys(initial.tiers).map((key) => [key, newRoute])),
        repair_route: newRoute,
        discovery_agents: 9,
        execution_concurrency: 1,
        max_tasks_per_cycle: 1,
        cycle_interval_seconds: 21600
      }
    }
  ]);
});

test('Astra rehearsal fixture: missing, stale, failed, incompatible catalogs and unsupported effort', async ({
  page
}, testInfo) => {
  const { writes } = await rehearsalFixture(page, testInfo.project.name === 'mobile');
  const preset = page.getByRole('group', { name: 'Astra rehearsal preset', exact: true });
  const apply = preset.getByRole('button', { name: 'Apply Astra rehearsal preset', exact: true });
  const load = page.getByRole('button', { name: 'Load Codex models' });
  await expect(apply).toBeDisabled();
  await expect(preset).toContainText('Load Codex models');
  await page.getByLabel('Orchestrator runner', { exact: true }).selectOption('codex');
  await page.getByLabel('Orchestrator model', { exact: true }).fill('gpt-6-astra');
  await expect(apply).toBeDisabled();
  const before = await draftInputs(page);
  await load.click();
  await expect(apply).toBeEnabled();
  await apply.click();
  await page.getByLabel('Codex executable', { exact: true }).fill('/fixture/changed-codex');
  await expect(apply).toBeDisabled();
  await expect(preset).toContainText('stale');
  await expect(preset.getByRole('button', { name: 'Confirm preset', exact: true })).toBeDisabled();
  await preset.getByRole('button', { name: 'Cancel', exact: true }).click();
  await page.getByLabel('Codex executable', { exact: true }).fill('/fixture/codex');
  let models: unknown = [];
  let failed = false;
  await page.route('**/api/model-catalog', async (route) => {
    await route.fulfill(
      failed ? { status: 400, json: { error: 'Fixture catalog failure' } } : { json: models }
    );
  });
  for (const scenario of [
    { models: [], problem: 'no exact gpt-6-astra entry' },
    { models: [{ ...codexModels[0], model: 'gpt-6-astra-preview' }], problem: 'no exact' },
    { models: [{ ...codexModels[0], backend: 'opencode' }], problem: 'no exact' },
    { models: [{ ...codexModels[0], provider: 'fixture' }], problem: 'no exact' },
    {
      models: [
        { ...codexModels[0], available: false, unavailable_reason: 'Fixture access denied' }
      ],
      problem: 'Fixture access denied'
    },
    {
      models: [{ ...codexModels[0], efforts: [] }],
      problem: 'no compatible supported effort list'
    },
    {
      models: [{ ...codexModels[0], efforts: [''] }],
      problem: 'no compatible supported effort list'
    }
  ]) {
    models = scenario.models;
    await load.click();
    await expect(apply).toBeDisabled();
    await expect(preset).toContainText(scenario.problem);
    expect(await draftInputs(page)).toEqual(before);
  }
  failed = true;
  await load.click();
  await expect(preset).toContainText('failed to load');
  await expect(apply).toBeDisabled();
  expect(await draftInputs(page)).toEqual(before);
  failed = false;
  models = [{ ...codexModels[0], efforts: ['high', 'low'] }];
  await load.click();
  const effort = page.getByLabel('Astra rehearsal effort');
  await expect(effort).toHaveValue('');
  await expect(effort.locator('option')).toHaveText(['Select supported effort', 'high', 'low']);
  await expect(apply).toBeDisabled();
  // A forged unsupported selection must still fail the apply guard.
  await effort.evaluate((element) => {
    const select = element as HTMLSelectElement;
    select.add(new Option('unsupported', 'unsupported'));
    select.value = 'unsupported';
    select.dispatchEvent(new Event('change', { bubbles: true }));
  });
  await expect(apply).toBeDisabled();
  await expect(preset).toContainText('Select a supported Astra effort');
  expect(await draftInputs(page)).toEqual(before);
  await effort.selectOption('low');
  await expect(apply).toBeEnabled();
  await apply.click();
  // Reloading invalidates a previously selected effort and pending confirmation.
  models = [{ ...codexModels[0], efforts: ['high'] }];
  await load.click();
  await expect(effort).toHaveValue('');
  await expect(preset.getByRole('button', { name: 'Confirm preset', exact: true })).toBeDisabled();
  expect(await draftInputs(page)).toEqual(before);
  await effort.selectOption('high');
  await apply.click();
  await preset.getByRole('button', { name: 'Confirm preset', exact: true }).click();
  for (const name of rehearsalRoutes)
    await expect(page.getByLabel(name + ' reasoning effort', { exact: true })).toHaveValue('high');
  expect(writes).toEqual([]);
});

test('Astra rehearsal fixture: active work and continuous operation prevent draft replacement', async ({
  page
}, testInfo) => {
  test.setTimeout(60000);
  let restriction: 'none' | 'tasks' | 'cycle' | 'continuous' = 'none';
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const state = await response.json();
    state.control.paused = restriction !== 'continuous';
    state.active_tasks = restriction === 'tasks' ? 1 : 0;
    state.cycle_active = restriction === 'cycle';
    await route.fulfill({ json: state });
  });
  const { writes } = await rehearsalFixture(page, testInfo.project.name === 'mobile');
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  const preset = page.getByRole('group', { name: 'Astra rehearsal preset', exact: true });
  const apply = preset.getByRole('button', { name: 'Apply Astra rehearsal preset', exact: true });
  await apply.click();
  const before = await draftInputs(page);
  for (const mode of ['tasks', 'cycle', 'continuous'] as const) {
    restriction = mode;
    await expect(apply).toBeDisabled({ timeout: 10000 });
    await expect(
      preset.getByRole('button', { name: 'Confirm preset', exact: true })
    ).toBeDisabled();
    await expect(preset).toContainText('Pause the service and wait for active work');
    expect(await draftInputs(page)).toEqual(before);
    restriction = 'none';
    await expect(apply).toBeEnabled({ timeout: 10000 });
  }
  await preset.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect(await draftInputs(page)).toEqual(before);
  expect(writes).toEqual([]);
});

test('task detail polling does not overlap or apply a response after close', async ({
  page
}, testInfo) => {
  let reads = 0;
  let release: (() => void) | undefined;
  await page.route('**/api/tasks/task-reviewed', async (route) => {
    reads++;
    if (reads === 1)
      await new Promise<void>((resolve) => {
        release = resolve;
      });
    try {
      const response = await route.fetch();
      await route.fulfill({ response });
    } catch {
      /* closing aborts the outstanding request */
    }
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Task queue', exact: true })
    .click();
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await expect.poll(() => reads).toBe(1);
  await page.waitForTimeout(4500);
  expect(reads).toBe(1);
  await page.getByRole('button', { name: 'Close task details' }).click();
  release?.();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await expect(
    page
      .getByRole('dialog')
      .getByRole('heading', { name: 'Explain the local development workflow' })
  ).toBeVisible();
  expect(reads).toBe(2);
});

test('expanded proposal evidence survives summary polling by cycle and proposal identity', async ({
  page
}, testInfo) => {
  let reads = 0;
  let detailReads = 0;
  let otherCycle = false;
  let revision = 1;
  let instructions = 'Execution prompt';
  let evidencePrefix = 'Evidence';
  let problem = 'Full problem evidence. '.repeat(120) + 'Problem ending';
  let reason = 'Full decision rationale. '.repeat(120) + 'Reason ending';
  await page.route('**/api/proposals?*', async (route) => {
    const response = await route.fetch();
    const result = await response.json();
    reads++;
    result.items = [
      {
        ...result.items[0],
        id: 'shared-proposal-id',
        cycle_id: otherCycle ? 'other-cycle' : 'first-cycle',
        title: otherCycle ? 'Another cycle proposal' : 'Expanded proposal',
        content_revision: revision,
        problem: problem.slice(0, 2000),
        reason: reason.slice(0, 2000),
        prompt: '',
        evidence: []
      }
    ];
    await route.fulfill({ response, json: result });
  });
  await page.route('**/api/proposals/*/shared-proposal-id', async (route) => {
    detailReads++;
    const cycle = route.request().url().includes('/first-cycle/') ? 'first' : 'other';
    await route.fulfill({
      json: {
        content_revision: revision,
        problem,
        reason,
        prompt: `${instructions} for ${cycle} cycle`,
        evidence: [`${evidencePrefix} for ${cycle} cycle`]
      }
    });
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Proposals', exact: true })
    .click();
  await page.getByText('Scope, evidence & execution prompt', { exact: true }).click();
  await expect(page.getByText('Execution prompt for first cycle', { exact: true })).toBeVisible();
  await expect(page.getByText(problem, { exact: true })).toBeVisible();
  await expect(page.getByText(reason, { exact: true })).toBeVisible();
  const initialReads = reads;
  await expect.poll(() => reads).toBeGreaterThan(initialReads);
  await expect(page.getByText(problem, { exact: true })).toBeVisible();
  await expect(page.getByText(reason, { exact: true })).toBeVisible();
  await expect(page.getByText('Execution prompt for first cycle', { exact: true })).toBeVisible();
  await expect(page.getByText('Evidence for first cycle', { exact: true })).toBeVisible();
  await page.getByText('Scope, evidence & execution prompt', { exact: true }).click();
  await page.getByText('Scope, evidence & execution prompt', { exact: true }).click();
  await expect(page.getByText(problem, { exact: true })).toBeVisible();
  await expect(page.getByText(reason, { exact: true })).toBeVisible();
  expect(detailReads).toBe(1);
  problem += ' Updated problem beyond the summary prefix';
  reason += ' Updated reason beyond the summary prefix';
  revision++;
  await expect(page.getByText(problem, { exact: true })).toBeVisible({ timeout: 10000 });
  await expect(page.getByText(reason, { exact: true })).toBeVisible();
  expect(detailReads).toBe(2);
  instructions = 'Consolidated execution prompt';
  evidencePrefix = 'Fresh consolidated evidence';
  revision++;
  await expect(
    page.getByText('Consolidated execution prompt for first cycle', { exact: true })
  ).toBeVisible({ timeout: 10000 });
  await expect(
    page.getByText('Fresh consolidated evidence for first cycle', { exact: true })
  ).toBeVisible();
  await expect(page.getByText('Execution prompt for first cycle', { exact: true })).toHaveCount(0);
  await expect(page.getByText('Evidence for first cycle', { exact: true })).toHaveCount(0);
  expect(detailReads).toBe(3);
  otherCycle = true;
  revision = 1;
  instructions = 'Execution prompt';
  evidencePrefix = 'Evidence';
  await expect(page.getByRole('heading', { name: 'Another cycle proposal' })).toBeVisible({
    timeout: 10000
  });
  await expect(page.locator('.proposal-card details')).not.toHaveAttribute('open', '');
  await page.getByText('Scope, evidence & execution prompt', { exact: true }).click();
  await expect(page.getByText('Execution prompt for other cycle', { exact: true })).toBeVisible();
  await expect(page.getByText('Execution prompt for first cycle', { exact: true })).toHaveCount(0);
});

test('proposal revision changes replace pending detail and ignore late stale responses', async ({
  page
}, testInfo) => {
  let revision = 1;
  let reads = 0;
  let detailReads = 0;
  let release: (() => void) | undefined;
  await page.route('**/api/proposals?*', async (route) => {
    const response = await route.fetch();
    const result = await response.json();
    reads++;
    result.items = [
      {
        ...result.items[0],
        id: 'slow-proposal',
        cycle_id: 'slow-cycle',
        content_revision: revision,
        prompt: '',
        evidence: []
      }
    ];
    await route.fulfill({ response, json: result });
  });
  await page.route('**/api/proposals/slow-cycle/slow-proposal', async (route) => {
    detailReads++;
    const requestedRevision = revision;
    if (requestedRevision === 1)
      await new Promise<void>((resolve) => {
        release = resolve;
      });
    await route.fulfill({
      json: {
        content_revision: requestedRevision,
        prompt:
          requestedRevision === 1 ? 'Stale draft instructions' : 'Fresh consolidated instructions',
        evidence: [requestedRevision === 1 ? 'Stale draft evidence' : 'Fresh consolidated evidence']
      }
    });
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Proposals', exact: true })
    .click();
  const toggle = page.getByText('Scope, evidence & execution prompt', { exact: true });
  await toggle.click();
  await expect.poll(() => detailReads).toBe(1);
  await toggle.click();
  await toggle.click();
  const initialReads = reads;
  await expect.poll(() => reads).toBeGreaterThan(initialReads);
  expect(detailReads).toBe(1);
  revision = 2;
  await expect(page.getByText('Fresh consolidated instructions', { exact: true })).toBeVisible({
    timeout: 10000
  });
  expect(detailReads).toBe(2);
  const staleResponse = page.waitForResponse('**/api/proposals/slow-cycle/slow-proposal');
  release?.();
  await (await staleResponse).finished();
  const refreshedReads = reads;
  await expect.poll(() => reads).toBeGreaterThan(refreshedReads);
  await expect(page.getByText('Fresh consolidated instructions', { exact: true })).toBeVisible();
  await expect(page.getByText('Fresh consolidated evidence', { exact: true })).toBeVisible();
  await expect(page.getByText('Stale draft instructions', { exact: true })).toHaveCount(0);
  await expect(page.getByText('Stale draft evidence', { exact: true })).toHaveCount(0);
  expect(detailReads).toBe(2);
});

test('loaded older cycles and their actions survive background refresh', async ({
  page
}, testInfo) => {
  let newest = 102;
  let archived = false;
  await page.route('**/api/cycles?*', async (route) => {
    const before = Number(new URL(route.request().url()).searchParams.get('before') ?? newest + 1);
    const cycles = Array.from({ length: newest }, (_, i) => ({
      id: `history-${newest - i}`,
      number: newest - i,
      mode: 'execution',
      status: 'completed',
      started_at: '2026-09-10T00:00:00Z',
      completed_at: '2026-09-10T00:01:00Z',
      error: null,
      session_count: 0,
      decisions: {},
      lifecycle: newest - i === 1 && archived ? { archived_at: '2026-09-10T00:00:00Z' } : {}
    })).filter((cycle) => cycle.number < before);
    const items = cycles.slice(0, 100);
    await route.fulfill({
      json: {
        items,
        next_cursor: cycles.length > 100 ? items.at(-1)?.number : null,
        counts: {}
      }
    });
  });
  await page.route('**/api/cycles/history-1/archive', async (route) => {
    archived = true;
    await route.fulfill({ json: { ok: true } });
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Proposals', exact: true })
    .click();
  const picker = page.getByLabel('Cycle', { exact: true });
  await expect(picker.locator('option')).toHaveCount(101);
  await page.getByRole('button', { name: 'Load older cycles' }).click();
  await expect(picker.locator('option')).toHaveCount(103);
  await picker.selectOption('history-1');
  newest = 103;
  await expect(picker.locator('option[value="history-103"]')).toHaveCount(1, { timeout: 10000 });
  await expect(picker.locator('option')).toHaveCount(104);
  await expect(picker).toHaveValue('history-1');
  await expect(page.getByRole('button', { name: 'Load older cycles' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Archive cycle', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Discard cycle workspaces' })).toBeVisible();
  await expect(picker).toHaveValue('history-1');
});

test('slow history requests survive polling while filter changes replace them', async ({
  page
}, testInfo) => {
  let reads = 0;
  let release: (() => void) | undefined;
  await page.route('**/api/tasks?*', async (route) => {
    const response = await route.fetch();
    const query = new URL(route.request().url()).searchParams.get('q');
    reads++;
    if (!query) await new Promise((resolve) => setTimeout(resolve, 5000));
    if (query === 'documentation')
      await new Promise<void>((resolve) => {
        release = resolve;
      });
    await route.fulfill({ response });
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Task queue', exact: true })
    .click();
  await expect(
    page.getByRole('button', { name: /Explain the local development workflow/ })
  ).toBeVisible({ timeout: 10000 });
  expect(reads).toBe(1);
  await page.getByLabel('Search work').fill('documentation');
  await expect.poll(() => !!release).toBe(true);
  await page.getByLabel('Search work').fill('setup');
  await expect(
    page.getByRole('button', { name: /Complete the repository setup flow/ })
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: /Explain the local development workflow/ })
  ).toHaveCount(0);
  release?.();
  await page.waitForTimeout(200);
  await expect(
    page.getByRole('button', { name: /Complete the repository setup flow/ })
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: /Explain the local development workflow/ })
  ).toHaveCount(0);
});
