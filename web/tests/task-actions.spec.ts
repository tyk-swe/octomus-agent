import { expect, type Page } from '@playwright/test';
import { login, openNavigation, test } from './synthetic';

const taskPath = '**/api/tasks/task-blocked';
const actionPath = '**/api/tasks/task-blocked/archive';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

async function openTask(page: Page, mobile: boolean) {
  await openNavigation(page, 'Task queue', mobile);
  await page.getByRole('button', { name: /Handle interrupted verification commands/ }).click();
  await expect(
    page.getByRole('dialog').getByRole('button', { name: 'Archive task' })
  ).toBeEnabled();
}

async function pollTask(page: Page, change: () => void = () => {}) {
  change();
  const response = await page.waitForResponse(taskPath, { timeout: 10000 });
  await response.finished();
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(resolve)));
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

for (const status of [409, 503]) {
  test(`a task action ${status} remains visible across polling until the next action attempt`, async ({
    page,
    isMobile
  }) => {
    const retry = deferred();
    let writes = 0;
    let failPoll = false;
    await page.route(taskPath, async (route) => {
      if (failPoll)
        await route.fulfill({ status: 503, json: { error: 'Synthetic task read outage' } });
      else await route.continue();
    });
    await page.route(actionPath, async (route) => {
      if (++writes === 1) {
        await route.fulfill({ status, json: { error: 'Synthetic archive refusal' } });
      } else {
        await retry.promise;
        await route.fulfill({ json: { ok: true } });
      }
    });
    await login(page);
    await openTask(page, !!isMobile);
    const dialog = page.getByRole('dialog');
    const archive = dialog.getByRole('button', { name: 'Archive task' });
    const failure = dialog.getByRole('alert').filter({ hasText: 'Synthetic archive refusal' });
    try {
      await archive.click();
      await expect(failure).toContainText('Synthetic archive refusal');
      const announcement = await failure.elementHandle();
      await pollTask(page);
      expect(await announcement!.evaluate((node) => node.isConnected)).toBe(true);
      await expect(failure).toHaveText('Task action failed. Synthetic archive refusal');
      await pollTask(page, () => (failPoll = true));
      await expect(dialog.getByRole('alert')).toHaveCount(2);
      await expect(
        dialog.getByRole('alert').filter({ hasText: 'Synthetic task read outage' })
      ).toHaveText('Retained task details · stale. Synthetic task read outage');
      await expect(failure).toHaveText('Task action failed. Synthetic archive refusal');
      await pollTask(page, () => (failPoll = false));
      await expect(dialog.getByRole('alert')).toHaveCount(1);
      await expect(failure).toBeVisible();
      await archive.click();
      await expect.poll(() => writes).toBe(2);
      await expect(archive).toBeDisabled();
      await expect(failure).toHaveCount(0);
      await archive.dispatchEvent('click');
      retry.resolve();
      await expect(archive).toBeEnabled();
      await expect(dialog.getByRole('alert')).toHaveCount(0);
      expect(writes).toBe(2);
    } finally {
      retry.resolve();
    }
  });
}
