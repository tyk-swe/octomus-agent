import { expect, type Page } from '@playwright/test';
import type { CycleSummary } from '../src/lib/types';
import { login, openNavigation, test } from './synthetic';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

async function cycleFixture(page: Page, action: 'archive' | 'discard') {
  const state = {
    outage: false,
    outageAfterAction: true,
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
    state.outage = state.outageAfterAction;
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
