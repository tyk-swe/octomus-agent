import { expect } from '@playwright/test';
import type { Page, Snapshot, Task, TaskRow } from '../src/lib/types';
import { login, openNavigation, test } from './synthetic';

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('task history counts retain archived work and recover with the displayed results', async ({
  page,
  isMobile
}) => {
  let archived = false;
  let failHistory = false;
  let stateReadsAfterArchive = 0;
  const archivedAt = '2026-09-10T00:00:00Z';
  await page.route('**/api/state', async (route) => {
    const state = (await (await route.fetch()).json()) as Snapshot;
    if (archived) {
      delete state.counts.blocked;
      state.attention_tasks = [];
      stateReadsAfterArchive++;
    }
    await route.fulfill({ json: state });
  });
  await page.route('**/api/tasks?*', async (route) => {
    if (failHistory) {
      await route.fulfill({ status: 503, json: { error: 'Synthetic history outage' } });
      return;
    }
    const result = (await (await route.fetch()).json()) as Page<TaskRow>;
    if (archived) {
      result.counts.attention = 0;
      for (const task of result.items)
        if (task.id === 'task-blocked') task.lifecycle.archived_at = archivedAt;
      if (new URL(route.request().url()).searchParams.get('status') === 'attention')
        result.items = [];
    }
    await route.fulfill({ json: result });
  });
  await page.route('**/api/tasks/task-blocked', async (route) => {
    const task = (await (await route.fetch()).json()) as Task;
    if (archived) {
      task.lifecycle.archived_at = archivedAt;
      task.allowed_actions = task.allowed_actions.filter((action) => action !== 'archive');
    }
    await route.fulfill({ json: task });
  });
  await page.route('**/api/tasks/task-blocked/archive', async (route) => {
    archived = true;
    await route.fulfill({ json: { ok: true } });
  });

  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  const filters = page.getByRole('group', { name: 'Task filters' });
  const filter = (name: string) => filters.getByRole('button', { name, exact: true });
  const count = (name: string) => filter(name).locator('.tab-count');
  await expect(page.locator('.task-row')).toHaveCount(3);
  await expect(count('all')).toHaveText('3');
  await expect(count('blocked')).toHaveText('1');
  await expect(count('attention')).toHaveText('1');
  await filter('attention').click();
  await expect(page.locator('.task-row')).toHaveCount(1);
  await page.getByRole('button', { name: /Handle interrupted verification commands/ }).click();
  failHistory = true;
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('button', { name: 'Archive task' }).click();
  await expect(dialog.getByText('Archived · Workspace retained until cleanup')).toBeVisible();
  await expect.poll(() => stateReadsAfterArchive).toBeGreaterThan(0);
  await page.getByRole('button', { name: 'Close task details' }).click();
  await expect(page.getByRole('alert')).toContainText('Showing the last received results');
  await expect(page.locator('.task-row')).toHaveCount(1);
  await expect(count('all')).toHaveText('3');
  await expect(count('blocked')).toHaveText('1');
  await expect(count('attention')).toHaveText('1');

  failHistory = false;
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect(page.getByRole('alert')).toHaveCount(0);
  await expect(page.locator('.task-row')).toHaveCount(0);
  await expect(count('attention')).toHaveCount(0);
  await expect(count('all')).toHaveText('3');
  await expect(count('blocked')).toHaveText('1');
  await filter('blocked').click();
  await expect(page.locator('.task-row')).toHaveCount(1);
  await expect(count('blocked')).toHaveText('1');
  await filter('all').click();
  await expect(page.locator('.task-row')).toHaveCount(3);
  await expect(count('all')).toHaveText('3');
});
