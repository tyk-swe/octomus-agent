import { expect } from '@playwright/test';
import {
  configurationFixture,
  deferred,
  login,
  openNavigation,
  test,
  unsandboxed
} from './synthetic';

test('route issue links allow incomplete saves @responsive', async ({ page, isMobile }) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const discovery = page.getByLabel('Discovery agents model', { exact: true });
  const tier = page.getByLabel('XS execution model', { exact: true });
  const repair = page.getByLabel('Repair reasoning effort', { exact: true });
  await discovery.fill('');
  await tier.fill('');
  await repair.selectOption('');
  const issues = page.getByRole('region', { name: 'Model routes needing attention' });
  await expect(issues.getByRole('listitem')).toHaveCount(3);
  const targets = [
    ['Discovery agents', discovery],
    ['XS execution', tier],
    ['Repair', repair]
  ] as const;
  for (const [name, control] of targets) {
    await issues.getByRole('button', { name, exact: true }).click();
    await expect(control).toBeFocused();
    await expect(control).toHaveAttribute('aria-invalid', 'true');
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.getByRole('button', { name: 'Save configuration', exact: true }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.saved!.roles.discovery.model).toBe('');
  expect(state.saved!.tiers.XS.model).toBe('');
  expect(state.saved!.repair_route.effort).toBe('');
});

test('catalog refreshes retain stale choices and recover on retry', async ({ page }) => {
  const state = await configurationFixture(page, { snapshot: unsandboxed });
  await login(page);
  await openNavigation(page, 'Configuration', false);
  const load = page.getByRole('button', { name: 'Load Codex models', exact: true });
  const effort = page.getByLabel('Repair reasoning effort', { exact: true });
  const routes = page.locator('[data-step="routes"]');
  await load.click();
  await expect(routes).toContainText('10 match a loaded catalog');
  state.failCatalog = true;
  await load.click();
  await expect(page.locator('.catalog-note')).toContainText('Showing stale, unchecked choices.');
  await expect(effort.locator('option[value="high"]')).toHaveCount(1);
  await expect(effort).toHaveValue('medium');
  await expect(routes).toContainText('0 match a loaded catalog');
  expect(state.writes).toEqual([]);
  state.failCatalog = false;
  await load.click();
  await expect(routes).toContainText('10 match a loaded catalog');
  await expect(page.locator('.catalog-note')).toHaveCount(0);
  await page.getByLabel('Codex executable', { exact: true }).fill('/other/codex');
  await expect(effort.locator('option[value="high"]')).toHaveCount(0);
  await expect(routes).toContainText('0 match a loaded catalog');
  state.failCatalog = true;
  await load.click();
  await expect(page.locator('.catalog-note')).toContainText('Catalog unavailable.');
  await expect(effort).toHaveValue('medium');
  expect(state.saved!.codex_binary).toBe('/fixture/codex');
});

test('checks populate pickers and diagnostics without saving routes', async ({ page }) => {
  const state = await configurationFixture(page, { doctorWarnings: ['Synthetic runner warning'] });
  await login(page);
  await openNavigation(page, 'Configuration', false);
  const effort = page.getByLabel('Repair reasoning effort', { exact: true });
  await expect(effort.locator('option[value="high"]')).toHaveCount(0);
  await page.getByRole('button', { name: 'Check audit connection', exact: true }).click();
  const details = page.getByRole('region', { name: 'Last successful connection check' });
  await expect(details).toContainText('synthetic-runner');
  await expect(details).toContainText('synthetic-protocol');
  await expect(details).toContainText(state.revision);
  await expect(details.getByRole('listitem')).toHaveText(['Synthetic runner warning']);
  await expect(effort.locator('option[value="high"]')).toHaveCount(1);
  await expect(effort).toHaveValue('medium');
  expect(state.catalogs).toEqual([]);
  expect(state.writes).toEqual([]);
  await page.getByLabel('Default branch', { exact: true }).fill('edited-branch');
  await expect(details).toContainText('These details do not cover the current draft.');
});

test('cancelling diagnostics preserves drafts and prior results', async ({ page }) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', false);
  await page.getByRole('button', { name: 'Check connection', exact: true }).click();
  const details = page.getByRole('region', { name: 'Last successful connection check' });
  await expect(details).toBeVisible();
  const branch = page.getByLabel('Default branch', { exact: true });
  await branch.fill('draft-branch');
  state.catalogGate = deferred();
  await page.getByRole('button', { name: 'Load Codex models', exact: true }).click();
  await expect.poll(() => state.catalogs.length).toBe(1);
  await page.getByRole('button', { name: 'Cancel diagnostic', exact: true }).click();
  await expect(branch).toBeEnabled();
  await expect(branch).toHaveValue('draft-branch');
  await expect(page.locator('.settings-feedback')).toHaveText('Catalog loading cancelled.');
  state.catalogGate.resolve();
  state.catalogGate = null;
  const effort = page.getByLabel('Repair reasoning effort', { exact: true });
  await expect(effort.locator('option[value="high"]')).toHaveCount(1);
  await expect(details).toBeVisible();
  await page.getByRole('button', { name: 'Discard changes', exact: true }).click();
  state.checkGate = deferred();
  await page.getByRole('button', { name: 'Check audit connection', exact: true }).click();
  await expect.poll(() => state.checks.length).toBe(2);
  await page.getByRole('button', { name: 'Cancel diagnostic', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeEnabled();
  await expect(page.locator('.settings-feedback')).toHaveText('Connection check cancelled.');
  state.checkGate.resolve();
  state.checkGate = null;
  await expect(details).toContainText('Execution check');
  await expect(page.locator('[data-step="preflight"] .badge')).toContainText('Passed · execution');
  expect(state.writes).toEqual([]);
});
