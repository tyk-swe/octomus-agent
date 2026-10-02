import { expect, type Page } from '@playwright/test';
import type { Task } from '../src/lib/types';
import { login, openNavigation, test } from './synthetic';

const taskPath = '**/api/tasks/task-blocked';
const eventsPath = '**/api/events?entity=task-blocked';
const labels = { archive: 'Archive task', discard: 'Discard workspace', retry: 'Retry task' };
const results = {
  archive: 'Task archived.',
  discard: 'Workspace discarded.',
  retry: 'Task retry requested.'
};
type Action = keyof typeof labels;

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

async function taskFixture(page: Page, action: Action, failedRead: 'task' | 'events' = 'task') {
  const state = {
    outage: false,
    refusal: false,
    evidenceOutage: false,
    writes: 0,
    reads: 0,
    holdNext: null as ReturnType<typeof deferred> | null,
    held: 0
  };
  let saved: Task;
  await page.route(taskPath, async (route) => {
    state.reads++;
    if (!saved) {
      saved = (await (await route.fetch()).json()) as Task;
      if (action === 'discard') {
        saved.status = 'cancelled';
        saved.lifecycle.archived_at = '2026-09-10T00:01:00Z';
        saved.allowed_actions = ['discard'];
      }
    }
    const task = structuredClone(saved);
    const outage = state.outage;
    const gate = failedRead === 'task' ? state.holdNext : null;
    if (failedRead === 'task') state.holdNext = null;
    if (gate) {
      state.held++;
      await gate.promise;
    }
    if (outage && failedRead === 'task')
      await route.fulfill({ status: 503, json: { error: 'Synthetic task read outage' } });
    else await route.fulfill({ json: task });
  });
  await page.route(eventsPath, async (route) => {
    const outage = state.outage;
    const gate = failedRead === 'events' ? state.holdNext : null;
    if (failedRead === 'events') state.holdNext = null;
    if (gate) {
      state.held++;
      await gate.promise;
    }
    if (outage && failedRead === 'events')
      await route.fulfill({ status: 503, json: { error: 'Synthetic activity read outage' } });
    else await route.continue();
  });
  await page.route('**/api/cycles/cycle-1/evidence', async (route) => {
    if (state.evidenceOutage)
      await route.fulfill({ status: 503, json: { error: 'Synthetic evidence read outage' } });
    else await route.continue();
  });
  await page.route('**/api/tasks/task-blocked/*', async (route) => {
    state.writes++;
    if (!route.request().url().endsWith(`/${action}`)) {
      await route.fulfill({ status: 409, json: { error: 'Unexpected stale task action' } });
      return;
    }
    if (state.refusal) {
      await route.fulfill({ status: 409, json: { error: 'Synthetic task action refusal' } });
      return;
    }
    saved.updated_at = '2026-09-10T00:02:00Z';
    if (action === 'archive') {
      saved.status = 'cancelled';
      saved.lifecycle.archived_at = saved.updated_at;
      saved.allowed_actions = ['discard'];
    } else if (action === 'discard') {
      saved.lifecycle.discarded_at = saved.updated_at;
      saved.allowed_actions = [];
    } else {
      saved.status = 'queued';
      saved.attempts++;
      saved.allowed_actions = ['cancel'];
    }
    state.outage = !state.evidenceOutage;
    await route.fulfill({ json: { ok: true } });
  });
  return state;
}

async function openTask(page: Page, mobile: boolean, action: Action = 'archive') {
  await page.clock.install();
  await login(page);
  await openNavigation(page, 'Task queue', mobile);
  await page.getByRole('button', { name: /Handle interrupted verification commands/ }).click();
  await expect(
    page.getByRole('dialog').getByRole('button', { name: labels[action], exact: true })
  ).toBeEnabled();
  // Let the initial list debounce and detail reads finish before controlling polling.
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

for (const action of ['archive', 'discard', 'retry'] as const) {
  for (const failedRead of ['task', 'events'] as const) {
    for (const recovery of ['retry', 'poll'] as const) {
      test(`successful task ${action} recovers its failed ${failedRead} read through ${recovery} without repeating the action`, async ({
        page,
        isMobile
      }) => {
        const state = await taskFixture(page, action, failedRead);
        await openTask(page, !!isMobile, action);
        const dialog = page.getByRole('dialog');
        const actionButton = dialog.getByRole('button', { name: labels[action], exact: true });
        await actionButton.click();
        const feedback = dialog.getByRole('alert');
        await expect(feedback).toContainText(results[action]);
        await expect(feedback).toContainText('Task details could not be refreshed.');
        await expect(feedback).toContainText(
          failedRead === 'task' ? 'Synthetic task read outage' : 'Synthetic activity read outage'
        );
        await expect(dialog.getByText('Task action failed.', { exact: false })).toHaveCount(0);
        for (const button of await dialog.locator('.actions button').all()) {
          await expect(button).toBeDisabled();
          await button.dispatchEvent('click');
        }
        expect(state.writes).toBe(1);
        const retry = dialog.getByRole('button', { name: 'Retry task details', exact: true });
        await retry.focus();
        if (recovery === 'retry') {
          const failedRetry = (state.holdNext = deferred());
          const reads = state.reads;
          const button = await retry.elementHandle();
          try {
            await page.keyboard.press('Enter');
            await expect.poll(() => state.held).toBe(1);
            const waiting = dialog.getByRole('button', { name: 'Retrying task details…' });
            await expect(waiting).toBeDisabled();
            await expect(waiting).toBeFocused();
            await waiting.dispatchEvent('click');
            expect(state.reads).toBe(reads + 1);
            failedRetry.resolve();
            await expect(retry).toBeEnabled();
            await expect(retry).toBeFocused();
            expect(await button!.evaluate((node) => node.isConnected)).toBe(true);
          } finally {
            failedRetry.resolve();
          }
        } else {
          await page.clock.runFor(4000);
          await expect(feedback).toContainText(results[action]);
          await expect(actionButton).toBeDisabled();
        }
        state.outage = false;
        if (recovery === 'retry') await retry.click();
        else await page.clock.runFor(4000);
        await expect(feedback).toHaveCount(0);
        await expect(retry).toHaveCount(0);
        await expect(actionButton).toHaveCount(0);
        await expect(
          dialog.getByRole('heading', {
            name: 'Handle interrupted verification commands',
            exact: true
          })
        ).toBeFocused();
        if (action === 'archive')
          await expect(dialog.getByRole('button', { name: 'Discard workspace' })).toBeEnabled();
        if (action === 'retry')
          await expect(dialog.getByRole('button', { name: 'Cancel task' })).toBeEnabled();
        expect(state.writes).toBe(1);
      });
    }
  }
}

test('an ordinary stale task read leaves a rejected action retryable', async ({
  page,
  isMobile
}) => {
  const state = await taskFixture(page, 'archive');
  await openTask(page, !!isMobile);
  const dialog = page.getByRole('dialog');
  state.outage = true;
  state.refusal = true;
  await page.clock.runFor(4000);
  const archive = dialog.getByRole('button', { name: 'Archive task' });
  await expect(dialog.getByRole('alert')).toContainText('Retained task details · stale.');
  await expect(archive).toBeEnabled();
  await archive.click();
  await expect(dialog.getByRole('alert').filter({ hasText: 'Task action failed.' })).toContainText(
    'Synthetic task action refusal'
  );
  await expect(archive).toBeEnabled();
  await expect(dialog.getByRole('button', { name: 'Retry task details' })).toHaveCount(0);
  await page.clock.runFor(4000);
  await expect(archive).toBeEnabled();
  state.refusal = false;
  await archive.click();
  await expect(dialog.getByRole('alert')).toContainText('Task archived.');
  await expect(archive).toBeDisabled();
  expect(state.writes).toBe(2);
});

test('an evidence-only outage does not retain obsolete task actions after a successful mutation', async ({
  page,
  isMobile
}) => {
  const state = await taskFixture(page, 'archive');
  await openTask(page, !!isMobile);
  state.evidenceOutage = true;
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('button', { name: 'Archive task' }).click();
  await expect(dialog.getByRole('button', { name: 'Discard workspace' })).toBeEnabled();
  await expect(dialog.getByRole('button', { name: 'Archive task' })).toHaveCount(0);
  await expect(dialog.getByRole('alert')).toHaveCount(0);
  await expect(dialog.getByRole('button', { name: 'Retry evidence' })).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Retry task details' })).toHaveCount(0);
  expect(state.writes).toBe(1);
});

for (const recovery of ['retry', 'poll'] as const) {
  test(`task ${recovery} recovery preserves focus moved to another tab`, async ({
    page,
    isMobile
  }) => {
    const state = await taskFixture(page, 'archive');
    await openTask(page, !!isMobile);
    const dialog = page.getByRole('dialog');
    await dialog.getByRole('button', { name: 'Archive task' }).click();
    const retry = dialog.getByRole('button', { name: 'Retry task details' });
    await retry.focus();
    state.outage = false;
    const gate = (state.holdNext = deferred());
    try {
      if (recovery === 'retry') await page.keyboard.press('Enter');
      else await page.clock.runFor(4000);
      await expect.poll(() => state.held).toBe(1);
      await dialog.getByRole('tab', { name: 'Activity' }).click();
      gate.resolve();
      await expect(dialog.getByRole('button', { name: 'Discard workspace' })).toBeEnabled();
      await expect(dialog.getByRole('tab', { name: 'Activity' })).toBeFocused();
      await expect(dialog.getByRole('tab', { name: 'Activity' })).toHaveAttribute(
        'aria-selected',
        'true'
      );
      expect(state.writes).toBe(1);
    } finally {
      gate.resolve();
    }
  });
}

test('a pre-action read cannot clear recovery and closing it cannot affect a newer panel', async ({
  page,
  isMobile
}) => {
  const state = await taskFixture(page, 'archive');
  await openTask(page, !!isMobile);
  const dialog = page.getByRole('dialog');
  const oldRead = (state.holdNext = deferred());
  try {
    await page.clock.runFor(4000);
    await expect.poll(() => state.held).toBe(1);
    await dialog.getByRole('button', { name: 'Archive task' }).click();
    await expect(dialog.getByRole('alert')).toContainText('Task archived.');
    oldRead.resolve();
    await expect(dialog.getByRole('button', { name: 'Archive task' })).toBeDisabled();
    state.outage = false;
    const recoveryRead = (state.holdNext = deferred());
    try {
      await dialog.getByRole('button', { name: 'Retry task details' }).click();
      await expect.poll(() => state.held).toBe(2);
      await page.getByRole('button', { name: 'Close task details' }).click();
      const opener = page.getByRole('button', { name: /Handle interrupted verification commands/ });
      await expect(opener).toBeFocused();
      await opener.click();
      await expect(dialog.getByRole('button', { name: 'Discard workspace' })).toBeEnabled();
      await dialog.getByRole('tab', { name: 'Sessions' }).click();
      recoveryRead.resolve();
      await expect(dialog.getByRole('alert')).toHaveCount(0);
      await expect(dialog.getByRole('button', { name: 'Retry task details' })).toHaveCount(0);
      await expect(dialog.getByRole('tab', { name: 'Sessions' })).toBeFocused();
      await page.keyboard.press('Escape');
      await expect(dialog).toHaveCount(0);
      await expect(opener).toBeFocused();
      await openNavigation(page, 'Overview', !!isMobile);
      await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
      await openNavigation(page, 'Task queue', !!isMobile);
      await page.clock.runFor(100);
      await opener.click();
      await expect(dialog.getByRole('button', { name: 'Discard workspace' })).toBeEnabled();
      await expect(dialog.getByRole('alert')).toHaveCount(0);
      expect(state.writes).toBe(1);
    } finally {
      recoveryRead.resolve();
    }
  } finally {
    oldRead.resolve();
  }
});
