import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
const token = 'browser-test-operator-token-32-characters';

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
  await page.getByLabel('Orchestrator model', { exact: true }).fill('gpt-6-astra');
  await page.getByLabel('Orchestrator reasoning effort', { exact: true }).fill('medium');
  await page.getByLabel('Repair model', { exact: true }).fill('gpt-5.6-luna');
  await page.getByLabel('Repair reasoning effort', { exact: true }).fill('high');
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
