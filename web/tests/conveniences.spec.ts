import { expect, type Page } from '@playwright/test';
import { login, nextPoll, openNavigation, patchState, test, token } from './synthetic';

test.use({ locale: 'en-US', timezoneId: 'UTC' });

const TITLE_PATTERN = /^[A-Z][a-z]{2} \d{1,2}, \d{4}, \d{1,2}:\d{2}:\d{2}\s[AP]M UTC$/;

async function loginForm(page: Page) {
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
}

test('the view lives in the URL hash, so history and dialogs follow it @responsive', async ({
  page,
  isMobile
}) => {
  await login(page);
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
  await navigate('Task queue');
  await expect(page).toHaveURL(/#queue$/);
  await expect(page.getByRole('heading', { level: 1, name: 'Task queue' })).toBeVisible();
  await navigate('Pull requests');
  await expect(page).toHaveURL(/#prs$/);
  await expect(page.getByRole('heading', { level: 1, name: 'Pull requests' })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('heading', { level: 1, name: 'Task queue' })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole('heading', { level: 1, name: 'Task queue' })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole('heading', { level: 1, name: 'Pull requests' })).toBeVisible();

  await navigate('Task queue');
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await expect(page.getByRole('dialog')).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.getByRole('heading', { level: 1, name: 'Pull requests' })).toBeVisible();

  await page.getByRole('link', { name: 'Skip to main content' }).dispatchEvent('click');
  await expect(page).toHaveURL(/#main-content$/);
  await expect(page.getByRole('heading', { level: 1, name: 'Pull requests' })).toBeVisible();

  // The brand link is hidden in the mobile navigation drawer; only the wide layout offers it.
  if (!isMobile) {
    await page.locator('a.brand').click();
    await expect(page).toHaveURL(/#overview$/);
    await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible();
  }
});

test('a reload or an expired session returns to the hashed view @responsive', async ({
  page,
  isMobile
}) => {
  await page.clock.install();
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(page).toHaveURL(/#settings$/);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Connect to Octomus' })).toBeVisible();
  await loginForm(page);
  await expect(page.getByRole('heading', { level: 1, name: 'Configuration' })).toBeVisible();

  await openNavigation(page, 'Pull requests', !!isMobile);
  await page.route('**/api/state', (route) =>
    route.fulfill({ status: 401, json: { error: 'Synthetic expired session' } })
  );
  await nextPoll(page);
  await expect(page.getByRole('heading', { name: 'Connect to Octomus' })).toBeVisible();
  await expect(page.getByRole('alert')).toContainText('Session expired');
  await page.unroute('**/api/state');
  await loginForm(page);
  await expect(page.getByRole('heading', { level: 1, name: 'Pull requests' })).toBeVisible();
});

test('the tab title names the view and the attention count', async ({ page }) => {
  let blocked = 3;
  await patchState(page, (snapshot) => {
    snapshot.counts.blocked = blocked;
    snapshot.counts.failed = 0;
  });
  await page.clock.install();
  await login(page);
  await expect(page).toHaveTitle('(3) Overview · Octomus Agent');
  await openNavigation(page, 'Task queue', false);
  await expect(page).toHaveTitle('(3) Task queue · Octomus Agent');
  blocked = 0;
  await nextPoll(page);
  await expect(page).toHaveTitle('Task queue · Octomus Agent');
  await page.getByRole('button', { name: 'Disconnect' }).click();
  await expect(page).toHaveTitle('Octomus Agent');
});

test('filtered empty lists offer to clear search and filters', async ({ page }) => {
  await login(page);
  await openNavigation(page, 'Task queue', false);
  const searchBox = page.getByLabel('Search work');
  const taskFilters = page.getByRole('group', { name: 'Task filters' });
  const row = page.getByRole('button', { name: /Complete the repository setup flow/ });

  await searchBox.fill('nothing matches this');
  await expect(page.getByRole('heading', { name: 'No matching tasks' })).toBeVisible();
  await page.getByRole('button', { name: 'Clear search and filters' }).click();
  await expect(searchBox).toHaveValue('');
  await expect(searchBox).toBeFocused();
  await expect(taskFilters.getByRole('button', { name: 'all', exact: true })).toHaveAttribute(
    'aria-pressed',
    'true'
  );
  await expect(row).toBeVisible();

  await taskFilters.getByRole('button', { name: 'cancelled' }).click();
  await expect(page.getByRole('heading', { name: 'No matching tasks' })).toBeVisible();
  await page.getByRole('button', { name: 'Clear search and filters' }).click();
  await expect(taskFilters.getByRole('button', { name: 'all', exact: true })).toHaveAttribute(
    'aria-pressed',
    'true'
  );
  await expect(row).toBeVisible();

  await openNavigation(page, 'Proposals', false);
  const proposalFilters = page.getByRole('group', { name: 'Proposal filters' });
  await proposalFilters.getByRole('button', { name: 'rejected' }).click();
  await expect(page.getByRole('heading', { name: 'No matching proposals' })).toBeVisible();
  await page.getByRole('button', { name: 'Clear search and filters' }).click();
  await expect(proposalFilters.getByRole('button', { name: 'all', exact: true })).toHaveAttribute(
    'aria-pressed',
    'true'
  );
  await expect(page.getByLabel('Cycle')).toHaveValue('all');
  await expect(page.getByLabel('Search work')).toBeFocused();

  await openNavigation(page, 'Pull requests', false);
  const prFilters = page.getByRole('group', { name: 'PR filters' });
  await prFilters.getByRole('button', { name: 'closed' }).click();
  await expect(page.getByRole('heading', { name: 'No matching pull requests' })).toBeVisible();
  await page.getByRole('button', { name: 'Clear search and filters' }).click();
  await expect(prFilters.getByRole('button', { name: 'all', exact: true })).toHaveAttribute(
    'aria-pressed',
    'true'
  );
  await expect(page.getByLabel('Search work')).toBeFocused();
});

test('an unfiltered empty list offers no clear button', async ({ page }) => {
  await page.route('**/api/tasks?*', (route) =>
    route.fulfill({ json: { items: [], counts: {}, next_cursor: null } })
  );
  await login(page);
  await openNavigation(page, 'Task queue', false);
  await expect(page.getByRole('heading', { name: 'No tasks yet' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Clear search and filters' })).toHaveCount(0);
});

test('relative times carry the exact date and time', async ({ page }) => {
  const task = (await (
    await page.request.get('/api/tasks/task-reviewed', {
      headers: { Authorization: `Bearer ${token}` }
    })
  ).json()) as { updated_at: string; created_at: string };
  await login(page);
  await openNavigation(page, 'Task queue', false);
  const row = page.getByRole('button', { name: /Explain the local development workflow/ });
  const time = row.locator('time');
  await expect(time).toHaveAttribute('datetime', task.updated_at);
  await expect(time).toHaveAttribute('title', TITLE_PATTERN);
  await row.click();
  const dialog = page.getByRole('dialog');
  const created = dialog.locator('.dialog-footer time');
  await expect(created).toHaveAttribute('datetime', task.created_at);
  await expect(created).toHaveAttribute('title', TITLE_PATTERN);
  await dialog.getByRole('button', { name: 'Close task details' }).click();

  await openNavigation(page, 'Overview', false);
  await expect(page.locator('.cycle-panel .section-heading p')).toHaveText(
    /^Started (just now|\d+[mhd] ago) · planning finished (just now|\d+[mhd] ago)$/
  );
});

test('task details and run evidence copy the branch and workspace @responsive', async ({
  page,
  context,
  isMobile
}) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write']).catch(() => {});
  await page.clock.install();
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  const dialog = page.getByRole('dialog');

  await dialog.getByRole('button', { name: 'Copy branch name' }).click();
  await expect(dialog.locator('.dialog-footer .muted')).toHaveText('Branch name copied.');
  if (!isMobile)
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()))
      .toBe('octomus/task-reviewed');

  const workspace = dialog.locator('.copy-text', { hasText: '/srv/project' }).locator('code');
  const box = await workspace.boundingBox();
  expect(box!.x + box!.width).toBeLessThanOrEqual(page.viewportSize()!.width);
  await dialog.getByRole('button', { name: 'Copy workspace path' }).click();
  await expect(dialog.locator('.dialog-footer .muted')).toHaveText('Workspace path copied.');
  if (!isMobile)
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()))
      .toBe('/srv/project/.octomus/tasks/task-reviewed/workspace');

  await page.route('**/api/tasks/task-reviewed', async (route) => {
    const body = (await (await route.fetch()).json()) as { workspace: string };
    body.workspace = '';
    await route.fulfill({ json: body });
  });
  await nextPoll(page, '**/api/tasks/task-reviewed');
  await expect(dialog.getByText('Created when execution starts')).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Copy workspace path' })).toHaveCount(0);
  await dialog.getByRole('button', { name: 'Close task details' }).click();

  await openNavigation(page, 'Overview', !!isMobile);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  const evidence = page.getByRole('dialog');
  await evidence.getByLabel('Proposal', { exact: true }).selectOption('task-reviewed');
  await evidence.getByRole('button', { name: 'Copy branch name' }).click();
  await expect(evidence.locator('.dialog-footer .muted')).toHaveText('Branch name copied.');
});

test('the navigation drawer closes from its scrim, keeps the page inert while open, and every view fits the viewport @responsive', async ({
  page,
  isMobile
}) => {
  test.skip(!isMobile, 'mobile drawer only');
  await login(page);
  const main = page.locator('main.content');
  await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await expect(page.locator('.nav-scrim')).toBeVisible();
  await expect(main).toHaveAttribute('inert', '');
  const width = page.viewportSize()!.width;
  await page.mouse.click(width - 10, 400);
  await expect(page.locator('#workspace-navigation')).not.toHaveClass(/mobile-open/);
  const toggle = page.getByRole('button', { name: 'Toggle navigation' });
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await expect(toggle).toBeFocused();
  await expect(main).not.toHaveAttribute('inert', '');
  for (const name of [
    'Overview',
    'Task queue',
    'Proposals',
    'Requests',
    'Pull requests',
    'Insights',
    'Configuration'
  ]) {
    await openNavigation(page, name, true);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
      await page.evaluate(() => window.innerWidth)
    );
  }
});
