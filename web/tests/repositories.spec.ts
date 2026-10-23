import { expect } from '@playwright/test';
import { deferred, login, openNavigation, test, token } from './synthetic';
import type { Snapshot } from '../src/lib/types';

test('switching repositories reloads state and scopes subsequent settings requests', async ({
  page
}) => {
  const repositories = ['fixture/project', 'fixture/second'];
  const switchRead = deferred();
  let switchedReads = 0;
  await page.clock.install();
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
      if (selected === repositories[1]) {
        switchedReads++;
        await switchRead.promise;
      }
      await route.fulfill({ json: state });
    } else {
      await route.fulfill({ response });
    }
  });
  await login(page);
  await openNavigation(page, 'Proposals', false);
  await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
  const selection = page.getByRole('combobox', { name: 'Repository', exact: true });
  const stateRead = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return url.pathname === '/api/state' && url.searchParams.get('repository') === repositories[1];
  });
  const proposalsRead = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return (
      url.pathname === '/api/proposals' &&
      url.searchParams.get('repository') === repositories[1] &&
      url.searchParams.get('cycle') === 'all'
    );
  });
  await selection.selectOption(repositories[1]);
  await stateRead;
  await expect.poll(() => switchedReads).toBe(1);
  await page.clock.fastForward(8000);
  // A pending switch owns the state read; periodic polling cannot restore
  // a dashboard ahead of that response. Recovery controls stay available.
  await expect(page.getByRole('heading', { name: 'Loading repository' })).toBeVisible();
  expect(switchedReads).toBe(1);
  switchRead.resolve();
  await proposalsRead;
  await expect(selection).toHaveValue(repositories[1]);
  await expect(page.getByLabel('Cycle', { exact: true })).toHaveValue('all');
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

test('a failed repository switch can recover by selecting another repository', async ({ page }) => {
  const repositories = ['fixture/project', 'fixture/unavailable'];
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
    if (url.pathname === '/api/state' && url.searchParams.get('repository') === repositories[1]) {
      await route.fulfill({ status: 503, json: { error: 'Repository temporarily unavailable' } });
      return;
    }
    url.searchParams.delete('repository');
    await route.fulfill({ response: await route.fetch({ url: url.toString() }) });
  });
  await login(page);
  const selection = page.getByRole('combobox', { name: 'Repository', exact: true });
  await selection.selectOption(repositories[1]);
  await expect(page.getByRole('heading', { name: 'Repository unavailable' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Retry', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Disconnect', exact: true })).toBeVisible();
  // A reload remembers the unavailable selection. Authentication must still
  // expose recovery controls for reaching the healthy repository.
  await page.reload();
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('heading', { name: 'Repository unavailable' })).toBeVisible();
  await selection.selectOption(repositories[0]);
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
});
