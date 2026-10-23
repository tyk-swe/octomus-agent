import { expect } from '@playwright/test';
import { login, openNavigation, test } from './synthetic';
import type { Snapshot } from '../src/lib/types';

test('switching repositories reloads state and scopes subsequent settings requests', async ({
  page
}) => {
  const repositories = ['fixture/project', 'fixture/second'];
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/repositories') {
      await route.fulfill({
        json: {
          repositories: repositories.map((github_repo, i) => ({ github_repo, primary: i === 0 })),
          instance_max_sessions_per_day: 0
        }
      });
      return;
    }
    const selected = url.searchParams.get('repository') ?? repositories[0];
    url.searchParams.delete('repository');
    const response = await route.fetch({ url: url.toString() });
    if (url.pathname === '/api/state' && response.ok()) {
      const state: Snapshot = await response.json();
      state.repository = selected;
      await route.fulfill({ json: state });
    } else {
      await route.fulfill({ response });
    }
  });
  await login(page);
  const selection = page.getByRole('combobox', { name: 'Repository', exact: true });
  const stateRead = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return url.pathname === '/api/state' && url.searchParams.get('repository') === repositories[1];
  });
  await selection.selectOption(repositories[1]);
  await stateRead;
  await expect(selection).toHaveValue(repositories[1]);
  await expect(page.locator('.repository-switch strong')).toHaveText('second');
  expect(await page.evaluate(() => localStorage.getItem('octomus-repository'))).toBe(
    repositories[1]
  );

  const settingsRead = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return url.pathname === '/api/config' && url.searchParams.get('repository') === repositories[1];
  });
  await openNavigation(page, 'Settings', false);
  await settingsRead;
});
