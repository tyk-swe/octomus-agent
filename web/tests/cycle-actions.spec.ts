import { expect, type Page } from '@playwright/test';
import type { CycleSummary } from '../src/lib/types';
import { login, openNavigation, test, token } from './synthetic';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

async function cycleFixture(page: Page, action: 'archive' | 'discard') {
  const state = {
    outage: false,
    refusal: false,
    writes: 0,
    reads: 0,
    holdNext: null as ReturnType<typeof deferred> | null,
    held: 0
  };
  const cycles: CycleSummary[] = [2, 1].map((number) => ({
    id: `history-${number}`,
    number,
    mode: 'execution',
    status: 'completed',
    started_at: '2026-09-10T00:00:00Z',
    completed_at: '2026-09-10T00:01:00Z',
    error: null,
    session_count: 0,
    decisions: {},
    lifecycle: number === 1 && action === 'discard' ? { archived_at: '2026-09-10T00:02:00Z' } : {}
  }));
  await page.route('**/api/cycles?*', async (route) => {
    state.reads++;
    const items = structuredClone(cycles);
    const outage = state.outage;
    const gate = state.holdNext;
    state.holdNext = null;
    if (gate) {
      state.held++;
      await gate.promise;
    }
    if (outage)
      await route.fulfill({ status: 503, json: { error: 'Synthetic cycle history outage' } });
    else await route.fulfill({ json: { items, next_cursor: null, counts: {} } });
  });
  await page.route(`**/api/cycles/history-1/${action}`, async (route) => {
    state.writes++;
    if (state.refusal) {
      await route.fulfill({ status: 409, json: { error: 'Synthetic cycle action refusal' } });
      return;
    }
    if (action === 'archive') cycles[1].lifecycle.archived_at = '2026-09-10T00:02:00Z';
    else cycles[1].lifecycle.discarded_at = '2026-09-10T00:03:00Z';
    state.outage = true;
    await route.fulfill({ json: { ok: true } });
  });
  return state;
}

async function openCycle(page: Page, mobile: boolean) {
  await page.clock.install();
  await login(page);
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
  await openNavigation(page, 'Proposals', mobile);
  await page.getByLabel('Cycle', { exact: true }).selectOption('history-1');
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

for (const action of ['archive', 'discard'] as const) {
  for (const recovery of ['retry', 'poll'] as const) {
    test(`a successful cycle ${action} with failed history refresh recovers through ${recovery} without another mutation`, async ({
      page,
      isMobile
    }) => {
      const state = await cycleFixture(page, action);
      await openCycle(page, !!isMobile);
      const label = action === 'archive' ? 'Archive cycle' : 'Discard cycle workspaces';
      const actionButton = page.getByRole('button', { name: label, exact: true });
      await actionButton.click();
      const feedback = page.getByRole('alert').filter({ hasText: 'Cycle history could not' });
      await expect(feedback).toContainText(
        action === 'archive' ? 'Cycle archived.' : 'Cycle workspaces discarded.'
      );
      await expect(feedback).toContainText('Synthetic cycle history outage');
      await expect(actionButton).toBeDisabled();
      await actionButton.dispatchEvent('click');
      expect(state.writes).toBe(1);

      const picker = page.getByLabel('Cycle', { exact: true });
      await picker.selectOption('history-2');
      await expect(page.getByRole('button', { name: 'Archive cycle', exact: true })).toBeDisabled();
      const retry = page.getByRole('button', { name: 'Retry cycle history' });
      if (recovery === 'retry') {
        const reads = state.reads;
        const failedRetry = (state.holdNext = deferred());
        const button = await retry.elementHandle();
        try {
          await retry.focus();
          await page.keyboard.press('Enter');
          await expect.poll(() => state.reads).toBe(reads + 1);
          const waiting = page.getByRole('button', { name: 'Retrying cycle history…' });
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
        await expect(feedback).toContainText('Synthetic cycle history outage');
      }
      state.outage = false;
      await retry.focus();
      if (recovery === 'retry') await retry.click();
      else await page.clock.runFor(4000);
      await expect(feedback).toHaveCount(0);
      await expect(retry).toHaveCount(0);
      await expect(picker).toHaveValue('history-2');
      await expect(picker).toBeFocused();
      await expect(page.getByRole('button', { name: 'Archive cycle', exact: true })).toBeEnabled();
      await picker.selectOption('history-1');
      await expect(picker.locator('option[value="history-1"]')).toContainText(
        action === 'archive' ? 'archived' : 'workspaces discarded'
      );
      await expect(actionButton).toHaveCount(0);
      if (action === 'archive')
        await expect(page.getByRole('button', { name: 'Discard cycle workspaces' })).toBeEnabled();
      expect(state.writes).toBe(1);
    });
  }

  test(`a refused cycle ${action} remains retryable and distinct from history recovery`, async ({
    page,
    isMobile
  }) => {
    const state = await cycleFixture(page, action);
    state.refusal = true;
    await openCycle(page, !!isMobile);
    const actionButton = page.getByRole('button', {
      name: action === 'archive' ? 'Archive cycle' : 'Discard cycle workspaces',
      exact: true
    });
    await actionButton.click();
    await expect(page.getByRole('alert')).toHaveText(
      'Cycle action failed. Synthetic cycle action refusal'
    );
    await expect(actionButton).toBeEnabled();
    await page.clock.runFor(4000);
    await expect(page.getByRole('alert')).toHaveText(
      'Cycle action failed. Synthetic cycle action refusal'
    );
    await expect(page.getByRole('button', { name: 'Retry cycle history' })).toHaveCount(0);
    state.refusal = false;
    await actionButton.click();
    await expect(page.getByRole('alert')).toContainText('Cycle history could not be refreshed');
    await expect(page.getByText('Synthetic cycle action refusal', { exact: false })).toHaveCount(0);
    expect(state.writes).toBe(2);
  });
}

test('a queued pre-action history response cannot clear cycle recovery, and retry respects newer navigation', async ({
  page,
  isMobile
}) => {
  const state = await cycleFixture(page, 'archive');
  await openCycle(page, !!isMobile);
  const oldHistory = (state.holdNext = deferred());
  try {
    await page.clock.runFor(4000);
    await expect.poll(() => state.held).toBe(1);
    await page.getByRole('button', { name: 'Archive cycle', exact: true }).click();
    await expect(page.getByRole('status').filter({ hasText: 'Cycle archived.' })).toBeVisible();
    oldHistory.resolve();
    await expect(page.getByRole('alert')).toContainText('Cycle history could not be refreshed');
    await expect(page.getByRole('button', { name: 'Archive cycle', exact: true })).toBeDisabled();

    state.outage = false;
    const newHistory = (state.holdNext = deferred());
    try {
      await page.getByRole('button', { name: 'Retry cycle history' }).click();
      await expect.poll(() => state.held).toBe(2);
      await openNavigation(page, 'Task queue', !!isMobile);
      await page.getByLabel('Search work').fill('documentation');
      newHistory.resolve();
      await expect(page.getByRole('heading', { name: 'From idea to improvement.' })).toBeVisible();
      await expect(page.getByLabel('Search work')).toHaveValue('documentation');
      await expect(page.getByLabel('Search work')).toBeFocused();
      await openNavigation(page, 'Proposals', !!isMobile);
      await expect(page.getByLabel('Cycle', { exact: true })).toHaveValue('history-1');
      await expect(page.getByRole('button', { name: 'Discard cycle workspaces' })).toBeEnabled();
      await expect(page.getByRole('button', { name: 'Retry cycle history' })).toHaveCount(0);
      expect(state.writes).toBe(1);
    } finally {
      newHistory.resolve();
    }
  } finally {
    oldHistory.resolve();
  }
});

for (const recovery of ['retry', 'poll'] as const) {
  test(`cycle history ${recovery} recovery preserves focus moved to search`, async ({
    page,
    isMobile
  }) => {
    const state = await cycleFixture(page, 'archive');
    await openCycle(page, !!isMobile);
    await page.getByRole('button', { name: 'Archive cycle', exact: true }).click();
    const retry = page.getByRole('button', { name: 'Retry cycle history' });
    await retry.focus();
    state.outage = false;
    const gate = (state.holdNext = deferred());
    try {
      if (recovery === 'retry') await page.keyboard.press('Enter');
      else await page.clock.runFor(4000);
      await expect.poll(() => state.held).toBe(1);
      const search = page.getByLabel('Search work');
      await search.focus();
      gate.resolve();
      await expect(page.getByRole('button', { name: 'Discard cycle workspaces' })).toBeEnabled();
      await expect(search).toBeFocused();
      expect(state.writes).toBe(1);
    } finally {
      gate.resolve();
    }
  });
}

test('a completed cycle action cannot restore recovery feedback after a concurrent session expiry', async ({
  page,
  isMobile
}) => {
  const state = await cycleFixture(page, 'archive');
  await openCycle(page, !!isMobile);
  await page.route('**/api/state', (route) =>
    route.fulfill({ status: 401, json: { error: 'Synthetic expired session' } })
  );
  await page.evaluate(() => {
    const originalFetch = window.fetch;
    let finishPost: (() => void) | undefined;
    let finishState: (() => void) | undefined;
    window.fetch = async (...args) => {
      const response = await originalFetch(...args);
      const path = new URL(response.url).pathname;
      if (path === '/api/cycles/history-1/archive' || path === '/api/state') {
        const read = response.text.bind(response);
        response.text = async () => {
          const text = await read();
          return new Promise<string>((resolve) => {
            if (path === '/api/cycles/history-1/archive') finishPost = () => resolve(text);
            else finishState = () => resolve(text);
            if (finishPost && finishState) {
              // Resolve success first, then expire the session before its caller resumes.
              window.fetch = originalFetch;
              finishPost();
              finishState();
            }
          });
        };
      }
      return response;
    };
  });
  await page.getByRole('button', { name: 'Archive cycle', exact: true }).click();
  await expect.poll(() => state.writes).toBe(1);
  await page.clock.runFor(4000);
  await expect(page.getByLabel('Operator access token')).toBeVisible();
  await page.unroute('**/api/state');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await openNavigation(page, 'Proposals', !!isMobile);
  await expect(page.getByRole('alert')).toContainText('Synthetic cycle history outage');
  await expect(page.getByText('Cycle archived.', { exact: false })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Retry cycle history' })).toHaveCount(0);
  expect(state.writes).toBe(1);
});
