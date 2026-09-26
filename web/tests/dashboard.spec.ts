import { readFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { login, openNavigation } from './synthetic';
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

test('the dashboard names the build version the service reports', async ({ page }) => {
  // The binary embeds the root VERSION file and the dashboard build injects the same file.
  const { version } = (await (await page.request.get('/healthz')).json()) as { version: string };
  expect(version).toBe(readFileSync(new URL('../../VERSION', import.meta.url), 'utf8').trim());
  await login(page);
  await expect(page.locator('.content-footer')).toContainText(`· v${version}`);
  await expect(page.locator('.disconnect .version')).toHaveText(
    `v${version.split('.').slice(0, 2).join('.')}`
  );
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
  // A head change is flagged only on a delivery whose head moved after Octomus delivered
  // it; an external request, never delivered by Octomus, has no head to compare. Older
  // deliveries remain visible.
  const moved = page.getByRole('link', { name: /Record the first delivered change/ });
  await expect(moved).toContainText('open · external head change');
  await expect(moved).toHaveAttribute('href', 'https://github.com/fixture/project/pull/7');
  for (const title of [/Explain the local development workflow/, /Adjust the retry backoff/])
    await expect(page.getByRole('link', { name: title })).not.toContainText('head change');
  await navigate('Overview');
  await page.getByRole('button', { name: 'Inspect run' }).click();
  // Read-only overlap context: the repository an external branch came from is
  // what tells a reviewer the change is not ours.
  await expect(
    page.getByRole('heading', { name: 'External pull requests observed' })
  ).toBeVisible();
  await expect(page.getByText('1 external of 2 open · 1 included')).toBeVisible();
  await expect(page.getByText('contributor/project:contributor/backoff')).toBeVisible();
  await page.getByRole('button', { name: 'Close run evidence' }).click();
  await navigate('Configuration');
  await page.getByLabel('Orchestrator runner').selectOption('codex');
  await page.getByLabel('Repair runner').selectOption('codex');
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await page.getByLabel('Orchestrator model', { exact: true }).fill('gpt-6-astra');
  await page.getByLabel('Orchestrator reasoning effort', { exact: true }).selectOption('medium');
  await page.getByLabel('Repair model', { exact: true }).fill('gpt-5.6-luna');
  await page.getByLabel('Repair reasoning effort', { exact: true }).selectOption('high');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('status').and(page.locator('.settings-feedback'))).toHaveText(
    'Configuration saved.'
  );
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

test('ownership and status are written out, and task tabs follow the arrow-key tabs pattern', async ({
  page,
  isMobile
}) => {
  const observedAt = new Date(Date.now() - 5 * 60_000).toISOString();
  await page.route('**/api/state', async (route) => {
    const state = await (await route.fetch()).json();
    state.pr_capacity = {
      limit: 2,
      owned_open: 2,
      reserved: 0,
      remaining: 0,
      observed_at: observedAt,
      status: 'full',
      reason: 'Capacity full'
    };
    await route.fulfill({ json: state });
  });
  await login(page);
  // The operating mode is a named status region, not an ignored label on a generic element.
  await expect(page.getByRole('status', { name: 'Operating mode' })).toContainText('active tasks');
  // The capacity message is live; its minute-by-minute observation time is not.
  const capacity = page.getByRole('status').filter({ hasText: 'Open-PR capacity is full' });
  await expect(capacity).toHaveCount(1);
  await expect(capacity).not.toContainText('Observed');
  await expect(
    page.locator('.notice').filter({ hasText: 'Open-PR capacity is full' })
  ).toContainText(/Observed \d+m ago\./);

  // Ownership is stated in words, not only by the badge colour.
  await openNavigation(page, 'Pull requests', isMobile);
  const external = page.getByRole('link', { name: /Adjust the retry backoff/ });
  await expect(external).toContainText('· not owned by Octomus');
  const owned = page.getByRole('link', { name: /Explain the local development workflow/ });
  await expect(owned).toContainText('· owned by Octomus');
  await expect(owned).not.toContainText('not owned');

  await openNavigation(page, 'Task queue', isMobile);
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  const dialog = page.getByRole('dialog');
  const tab = (name: string | RegExp) => dialog.getByRole('tab', { name });
  await expect(tab('Overview')).toHaveAttribute('aria-selected', 'true');
  // Only the selected tab is in the Tab order.
  await expect(dialog.locator('[role="tab"][tabindex="0"]')).toHaveCount(1);
  await tab('Overview').focus();
  await page.keyboard.press('ArrowRight');
  await expect(tab('Sessions')).toBeFocused();
  await expect(tab('Sessions')).toHaveAttribute('aria-selected', 'true');
  await expect(tab('Sessions')).toHaveAttribute('tabindex', '0');
  await expect(tab('Overview')).toHaveAttribute('aria-selected', 'false');
  await expect(tab('Overview')).toHaveAttribute('tabindex', '-1');
  // The panel is named by the tab that controls it.
  await expect(dialog.getByRole('tabpanel', { name: 'Sessions' })).toBeVisible();
  await page.keyboard.press('End');
  await expect(tab('Activity')).toBeFocused();
  await expect(tab('Activity')).toHaveAttribute('aria-selected', 'true');
  await page.keyboard.press('ArrowRight');
  await expect(tab('Overview')).toBeFocused();
  await page.keyboard.press('ArrowLeft');
  await expect(tab('Activity')).toBeFocused();
  await page.keyboard.press('Home');
  await expect(tab('Overview')).toBeFocused();
  await expect(dialog.getByRole('tabpanel', { name: 'Overview' })).toBeVisible();
  await page.keyboard.press('ArrowLeft');
  await expect(tab(/Reviews/)).not.toBeFocused();
  await expect(tab('Activity')).toBeFocused();
  // Clicking still selects a tab.
  await tab(/Reviews/).click();
  await expect(tab(/Reviews/)).toHaveAttribute('aria-selected', 'true');
  await expect(dialog.getByRole('tabpanel', { name: /Reviews/ })).toBeVisible();
  const accessibility = await new AxeBuilder({ page })
    .include('.task-dialog')
    .withTags(['wcag2a', 'wcag2aa'])
    .analyze();
  expect(
    accessibility.violations.map((v) => ({ rule: v.id, elements: v.nodes.map((n) => n.target) }))
  ).toEqual([]);
  await page.getByRole('button', { name: 'Close task details' }).click();
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
  await expect(page.getByRole('status').filter({ hasText: 'Audit in progress' })).toContainText(
    'Audit in progress'
  );
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
  // The route records the request asynchronously; the click can resolve first.
  await expect.poll(() => drafts).toEqual([{ backend: 'opencode', binary: '/draft/opencode' }]);
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
  await expect(page.getByRole('status').and(page.locator('.settings-feedback'))).toHaveText(
    'Configuration saved.'
  );
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

test('a successful status without a readable JSON body is reported as a lost connection', async ({
  page
}) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  const indicator = page.locator('.live-indicator');
  await expect(indicator).toHaveText('Connected');
  // An access proxy whose session expired answers with its sign-in page, not the service.
  await page.route('**/api/state', (route) =>
    route.fulfill({ status: 200, contentType: 'text/html', body: '<html>Sign in</html>' })
  );
  await expect(indicator).toHaveText('Reconnecting', { timeout: 10000 });
  const notice = page.getByRole('alert').filter({ hasText: 'Connection interrupted' });
  await expect(notice).toContainText('Service returned an unreadable response (200)');
  // The last received state stays on screen.
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  await page.unroute('**/api/state');
  await expect(indicator).toHaveText('Connected', { timeout: 10000 });
  await expect(notice).toHaveCount(0);
  expect(errors).toEqual([]);
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
  let discarded = false;
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
      // Retention cleanup discarded history-2's workspaces without it being archived.
      lifecycle:
        newest - i === 1 && archived
          ? {
              archived_at: '2026-09-10T00:00:00Z',
              ...(discarded ? { discarded_at: '2026-09-10T00:02:00Z' } : {})
            }
          : newest - i === 2
            ? { discarded_at: '2026-09-10T00:03:00Z' }
            : {}
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
  const actions: string[] = [];
  await page.route('**/api/cycles/history-1/*', async (route) => {
    const action = new URL(route.request().url()).pathname.split('/').at(-1)!;
    actions.push(action);
    if (action === 'archive') archived = true;
    if (action === 'discard') discarded = true;
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
  const archive = page.getByRole('button', { name: 'Archive cycle', exact: true });
  const discard = page.getByRole('button', { name: 'Discard cycle workspaces' });
  await expect(discard).toHaveCount(0);
  await expect(archive).toBeVisible();
  // A cycle whose workspaces are already discarded has no lifecycle action left, even
  // when retention cleanup discarded them without an archive.
  await expect(picker.locator('option[value="history-2"]')).toHaveText(
    'Execution cycle #002 · completed · workspaces discarded'
  );
  await picker.selectOption('history-2');
  await expect(archive).toHaveCount(0);
  await expect(discard).toHaveCount(0);
  await picker.selectOption('history-1');
  await archive.click();
  await expect(discard).toBeVisible();
  await expect(picker).toHaveValue('history-1');
  // Archiving again would only restart the cycle's retention clock, so it is not offered.
  await expect(archive).toHaveCount(0);
  await expect(picker.locator('option[value="history-1"]')).toHaveText(
    'Execution cycle #001 · completed · archived'
  );
  await discard.click();
  // Discarded workspaces leave no lifecycle action for this cycle.
  await expect(picker.locator('option[value="history-1"]')).toHaveText(
    'Execution cycle #001 · completed · workspaces discarded'
  );
  await expect(archive).toHaveCount(0);
  await expect(discard).toHaveCount(0);
  await expect(picker).toHaveValue('history-1');
  expect(actions).toEqual(['archive', 'discard']);
});

test('a failed request for older cycles is reported and the control stays usable', async ({
  page
}, testInfo) => {
  let release: (() => void) | undefined;
  let outage = true;
  await page.route('**/api/cycles?*', async (route) => {
    if (new URL(route.request().url()).searchParams.has('before')) {
      if (!outage) {
        const older = {
          id: 'history-older',
          number: 0,
          mode: 'execution',
          status: 'completed',
          started_at: '2026-09-10T00:00:00Z',
          completed_at: '2026-09-10T00:01:00Z',
          error: null,
          session_count: 0,
          decisions: {},
          lifecycle: {}
        };
        await route.fulfill({ json: { items: [older], next_cursor: null, counts: {} } });
        return;
      }
      await new Promise<void>((resolve) => (release = resolve));
      await route.fulfill({ status: 503, json: { error: 'Synthetic cycles outage' } });
      return;
    }
    const newest = await (await route.fetch()).json();
    await route.fulfill({ json: { ...newest, next_cursor: 1 } });
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
  const older = page.getByRole('button', { name: 'Load older cycles' });
  await older.click();
  await expect.poll(() => !!release).toBe(true);
  await expect(older).toBeDisabled();
  release!();
  await expect(page.getByRole('alert')).toContainText(
    'Could not load older cycles. Synthetic cycles outage'
  );
  await expect(older).toBeEnabled();
  // A successful retry loads the page and leaves no stale failure behind.
  outage = false;
  await older.click();
  await expect(
    page.getByLabel('Cycle', { exact: true }).locator('option[value="history-older"]')
  ).toHaveCount(1);
  await expect(older).toHaveCount(0);
  await expect(page.getByRole('alert')).toHaveCount(0);
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
