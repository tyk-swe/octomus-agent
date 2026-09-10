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
  await expect(page.getByRole('button', { name: 'Run a cycle' })).toBeDisabled();
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
      cycle.proposals = finished
        ? ['accepted', 'rejected', 'deferred'].map((decision, index) => ({
            ...cycle.proposals[0],
            id: `audit-${index}`,
            title: `Audit ${decision} recommendation`,
            decision,
            reason: `${decision}: both adversaries considered the concrete evidence.`
          }))
        : [];
      state.cycles.unshift(cycle);
    }
    await route.fulfill({ response, json: state });
  });
  await page.route('**/api/control/audit', async (route) => {
    expect(route.request().method()).toBe('POST');
    running = true;
    await route.fulfill({ json: { paused: true } });
  });
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('button', { name: 'Run a cycle' })).toBeDisabled();
  await page.getByRole('button', { name: 'Run an audit' }).click();
  await expect(page.getByRole('heading', { name: 'Worth doing. Before doing.' })).toBeVisible();
  await expect(page.getByRole('status')).toContainText('Audit in progress');
  await expect(page.getByRole('button', { name: 'Resume', exact: true })).toBeDisabled();
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
