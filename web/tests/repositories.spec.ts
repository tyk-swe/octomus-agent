import { expect } from '@playwright/test';
import {
  configurationFixture,
  deferred,
  login,
  openNavigation,
  test,
  token,
  unsandboxed
} from './synthetic';
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
  await openNavigation(page, 'Configuration', false);
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

test('Enter in configuration saves settings without submitting fleet actions', async ({ page }) => {
  const state = await configurationFixture(page);
  let registrations = 0;
  await page.route('**/api/repositories', async (route) => {
    if (route.request().method() === 'POST') {
      registrations++;
      await route.fulfill({ json: { repositories: [], instance_max_sessions_per_day: 0 } });
      return;
    }
    await route.fallback();
  });
  await login(page);
  await openNavigation(page, 'Configuration', false);
  await page.getByRole('textbox', { name: /^Add a repository/ }).fill('fixture/extra');
  const branch = page.getByLabel('Default branch', { exact: true });
  await branch.fill('updated-default');
  await branch.press('Enter');
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.saved!.default_branch).toBe('updated-default');
  expect(registrations).toBe(0);
});

test('fleet controls remain available while repository configuration is locked', async ({
  page
}) => {
  await configurationFixture(page, {
    snapshot: (state) => {
      state.control.paused = false;
      state.control.mode = 'continuous';
      state.capabilities.configure = { allowed: false, reason: 'Repository is running' };
    }
  });
  await login(page);
  await openNavigation(page, 'Configuration', false);
  await expect(page.getByLabel('Default branch', { exact: true })).toBeDisabled();
  const add = page.getByRole('textbox', { name: /^Add a repository/ });
  await expect(add).toBeEnabled();
  await add.fill('fixture/extra');
  await expect(page.getByRole('button', { name: 'Add repository', exact: true })).toBeEnabled();
  await expect(
    page.getByRole('button', { name: 'Save instance ceiling', exact: true })
  ).toBeEnabled();
});

test('a legacy rename updates the API scope even when browser storage is denied', async ({
  page
}) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, 'localStorage', {
      value: {
        getItem() {
          throw new Error('Storage denied');
        },
        setItem() {
          throw new Error('Storage denied');
        }
      }
    });
  });
  const state = await configurationFixture(page, {
    snapshot: (snapshot) => {
      unsandboxed(snapshot);
      snapshot.repository = state.saved?.github_repo ?? 'fixture/project';
    }
  });
  await login(page);
  await openNavigation(page, 'Configuration', false);
  await page.getByRole('textbox', { name: /^GitHub repository/ }).fill('fixture/renamed');
  const refreshed = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return url.pathname === '/api/state' && !url.searchParams.has('repository');
  });
  await page.getByRole('button', { name: 'Save configuration', exact: true }).click();
  await refreshed;
  expect(state.saved!.github_repo).toBe('fixture/renamed');
  await expect(page.locator('.repository-switch strong')).toHaveText('renamed');
});
