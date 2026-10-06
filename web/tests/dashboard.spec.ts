import { readFileSync } from 'node:fs';
import { expect, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import type { Model } from '../src/lib/types';
import {
  configurationFixture,
  login,
  nextPoll,
  openNavigation,
  patchState,
  test,
  token,
  unsandboxed
} from './synthetic';

const codexModels: Model[] = ['gpt-6-astra', 'gpt-5.6-luna'].map((model) => ({
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
const opencodeModels: Model[] = [
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

async function accessible(page: Page, within?: string) {
  const builder = new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']);
  const { violations } = await (within ? builder.include(within) : builder).analyze();
  expect(violations.map((v) => ({ rule: v.id, elements: v.nodes.map((n) => n.target) }))).toEqual(
    []
  );
}
const fitsViewport = (page: Page) =>
  page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);

test('the private dashboard names its version and tours tasks, proposals, pull requests, run evidence and configuration @responsive', async ({
  page,
  isMobile
}) => {
  const { version } = (await (await page.request.get('/healthz')).json()) as { version: string };
  expect(version).toBe(readFileSync(new URL('../../VERSION', import.meta.url), 'utf8').trim());
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await configurationFixture(page, { catalog: [...codexModels, ...opencodeModels] });
  await page.clock.install();
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible();
  await page.getByLabel('Operator access token').fill('incorrect');
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('alert')).toContainText('operator access token');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  await expect(page.locator('.content-footer')).toContainText(`· v${version}`);
  await expect(page.locator('.disconnect .version')).toHaveText(
    `v${version.split('.').slice(0, 2).join('.')}`
  );
  await expect(page.getByRole('button', { name: 'Run once' })).toBeDisabled();
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(0);
  await accessible(page);
  expect(await fitsViewport(page)).toBe(true);
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
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
  const moved = page.getByRole('link', { name: /Record the first delivered change/ });
  await expect(moved).toContainText('open · external head change');
  await expect(moved).toHaveAttribute('href', 'https://github.com/fixture/project/pull/7');
  for (const title of [/Explain the local development workflow/, /Adjust the retry backoff/])
    await expect(page.getByRole('link', { name: title })).not.toContainText('head change');
  await navigate('Overview');
  await page.getByRole('button', { name: 'Inspect run' }).click();
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
  await nextPoll(page);
  await expect(page.getByLabel('Orchestrator model', { exact: true })).toHaveValue('gpt-6-astra');
  await expect(page.getByLabel('Repair model', { exact: true })).toHaveValue('gpt-5.6-luna');
  await navigate('Overview');
  await navigate('Configuration');
  await expect(page.getByLabel('Repair reasoning effort', { exact: true })).toHaveValue('high');
  expect(await fitsViewport(page)).toBe(true);
  await navigate('Overview');
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  expect(errors).toEqual([]);
});

test('ownership and status are written out, and task tabs follow the arrow-key tabs pattern @responsive', async ({
  page,
  isMobile
}) => {
  const observedAt = new Date(Date.now() - 5 * 60_000).toISOString();
  await patchState(page, (snapshot) => {
    snapshot.pr_capacity = {
      limit: 2,
      owned_open: 2,
      reserved: 0,
      remaining: 0,
      observed_at: observedAt,
      status: 'full',
      reason: 'Capacity full'
    };
  });
  await login(page);
  await expect(page.getByRole('status', { name: 'Operating mode' })).toContainText('active tasks');
  const capacity = page.getByRole('status').filter({ hasText: 'Open-PR capacity is full' });
  await expect(capacity).toHaveCount(1);
  await expect(capacity).not.toContainText('Observed');
  await expect(
    page.locator('.notice').filter({ hasText: 'Open-PR capacity is full' })
  ).toContainText(/Observed \d+m ago\./);

  await openNavigation(page, 'Pull requests', !!isMobile);
  const external = page.getByRole('link', { name: /Adjust the retry backoff/ });
  await expect(external).toContainText('· not owned by Octomus');
  const owned = page.getByRole('link', { name: /Explain the local development workflow/ });
  await expect(owned).toContainText('· owned by Octomus');
  await expect(owned).not.toContainText('not owned');

  await openNavigation(page, 'Task queue', !!isMobile);
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  const dialog = page.getByRole('dialog');
  const tab = (name: string | RegExp) => dialog.getByRole('tab', { name });
  await expect(tab('Overview')).toHaveAttribute('aria-selected', 'true');
  await expect(dialog.locator('[role="tab"][tabindex="0"]')).toHaveCount(1);
  await tab('Overview').focus();
  await page.keyboard.press('ArrowRight');
  await expect(tab('Sessions')).toBeFocused();
  await expect(tab('Sessions')).toHaveAttribute('aria-selected', 'true');
  await expect(tab('Sessions')).toHaveAttribute('tabindex', '0');
  await expect(tab('Overview')).toHaveAttribute('aria-selected', 'false');
  await expect(tab('Overview')).toHaveAttribute('tabindex', '-1');
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
  await tab(/Reviews/).click();
  await expect(tab(/Reviews/)).toHaveAttribute('aria-selected', 'true');
  await expect(dialog.getByRole('tabpanel', { name: /Reviews/ })).toBeVisible();
  await accessible(page, '.task-dialog');
  await page.getByRole('button', { name: 'Close task details' }).click();
});

test('model routing across all roles, provider variants, draft catalogs and unavailable selections @responsive', async ({
  page,
  isMobile
}) => {
  await configurationFixture(page, { snapshot: unsandboxed });
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
  await page.clock.install();
  await login(page);
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
  await navigate('Configuration');
  await page.getByLabel('OpenCode executable', { exact: true }).fill('/draft/opencode');
  await page.getByRole('button', { name: 'Load OpenCode models' }).click();
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
  await nextPoll(page);
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
  expect(await fitsViewport(page)).toBe(true);
  await accessible(page);
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
