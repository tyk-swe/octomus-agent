import { expect, type Page } from '@playwright/test';
import { login, openNavigation, test } from './synthetic';

const stat = (page: Page, name: string) =>
  page.locator('.stat', { has: page.locator('.stat-label', { hasText: name }) });

test('insights reads seeded usage and stays readable when refresh fails @responsive', async ({
  page,
  isMobile
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await login(page);
  await openNavigation(page, 'Insights', isMobile);
  await expect(page.getByRole('heading', { name: 'What the work adds up to.' })).toBeVisible();
  await expect(stat(page, 'Admissions')).toContainText('9');
  await expect(stat(page, 'Admissions')).toContainText('avg 0.3/day');
  await expect(stat(page, 'Execution cycles')).toContainText('1');
  await expect(stat(page, 'Execution cycles')).toContainText('0 audits · 0 failed');
  await expect(stat(page, 'Accepted proposals')).toContainText('3');
  await expect(stat(page, 'Accepted proposals')).toContainText('100% of decided proposals');
  await expect(stat(page, 'Tasks published')).toContainText('1');
  await expect(stat(page, 'Tasks published')).toContainText('of 3 created');
  await expect(stat(page, 'PRs merged')).toContainText('0');
  await expect(stat(page, 'PRs merged')).toContainText('of 1 delivered');
  const documentationRow = page
    .locator('tr')
    .filter({ has: page.getByText('documentation', { exact: true }) });
  await expect(documentationRow.locator('td').nth(6)).toHaveText('1');
  await expect(page.locator('[aria-label="History pagination"]')).toBeHidden();

  const week = page.waitForRequest('**/api/insights?days=7');
  await page.getByRole('button', { name: '7 days' }).click();
  await week;

  await page.route('**/api/insights**', (route) =>
    route.fulfill({ status: 500, json: { error: 'synthetic insights failure' } })
  );
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect(page.getByRole('alert')).toContainText(
    'Could not refresh insights. Showing the last loaded results.'
  );
  await expect(stat(page, 'Admissions')).toContainText('9');
  expect(errors).toEqual([]);
});
