import { expect, type Page } from '@playwright/test';
import type { CycleSummary } from '../src/lib/types';
import { login, openNavigation, test } from './synthetic';

async function historyFixture(page: Page) {
  const state = {
    newest: 320,
    reads: [] as { before: number | null; rows: number }[],
    writes: [] as string[],
    failOlder: false,
    lifecycle: {} as CycleSummary['lifecycle']
  };
  // Like record_meta.seq, these cursors are exclusive and differ from cycle numbers.
  const cursor = (number: number) => number * 7 + 11;
  await page.route('**/api/cycles?*', async (route) => {
    const params = new URL(route.request().url()).searchParams;
    const before = params.has('before') ? Number(params.get('before')) : null;
    if (before !== null && state.failOlder) {
      await route.fulfill({ status: 503, json: { error: 'Synthetic older history outage' } });
      return;
    }
    const candidates = Array.from({ length: state.newest }, (_, i) => state.newest - i).filter(
      (number) => before === null || cursor(number) < before
    );
    const numbers = candidates.slice(0, Number(params.get('limit')));
    const items: CycleSummary[] = numbers.map((number) => ({
      id: `history-${number}`,
      number,
      mode: 'execution',
      status: 'completed',
      started_at: '2026-09-10T00:00:00Z',
      completed_at: '2026-09-10T00:01:00Z',
      error: null,
      session_count: 0,
      decisions: {},
      lifecycle: number === 221 ? { ...state.lifecycle } : {}
    }));
    state.reads.push({ before, rows: items.length });
    await route.fulfill({
      json: {
        items,
        next_cursor: candidates.length > items.length ? cursor(numbers.at(-1)!) : null,
        counts: {}
      }
    });
  });
  await page.route('**/api/cycles/history-221/*', async (route) => {
    const action = new URL(route.request().url()).pathname.split('/').at(-1)!;
    state.writes.push(action);
    if (action === 'archive') state.lifecycle.archived_at = '2026-09-10T00:02:00Z';
    else state.lifecycle.discarded_at = '2026-09-10T00:03:00Z';
    state.failOlder = true;
    await route.fulfill({ json: { ok: true } });
  });
  return { state, cursor };
}

async function openHistory(page: Page, mobile: boolean) {
  await page.clock.install();
  await login(page);
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
  await openNavigation(page, 'Proposals', mobile);
  const picker = page.getByLabel('Cycle', { exact: true });
  await expect(picker.locator('option')).toHaveCount(101);
  return picker;
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

for (const scenario of ['plain', 'later action failure', 'navigation away and back']) {
  test(`an initial cycle-history outage clears on polling recovery: ${scenario}`, async ({
    page,
    isMobile
  }) => {
    const refusedAction = scenario === 'later action failure';
    let outage = true;
    let reads = 0;
    let writes = 0;
    await page.route('**/api/cycles?*', async (route) => {
      reads++;
      if (outage)
        await route.fulfill({ status: 503, json: { error: 'Synthetic initial history outage' } });
      else await route.fulfill({ json: await (await route.fetch()).json() });
    });
    await page.route('**/api/state', async (route) => {
      const snapshot = await (await route.fetch()).json();
      snapshot.configured = true;
      snapshot.active_cycle_mode = null;
      snapshot.baseline_active = false;
      snapshot.control.paused = true;
      snapshot.control.mode = 'paused';
      await route.fulfill({ json: snapshot });
    });
    await page.route('**/api/control/resume', async (route) => {
      writes++;
      await route.fulfill({ status: 409, json: { error: 'Synthetic control refusal' } });
    });
    await page.clock.install();
    await login(page);
    await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
    await openNavigation(page, 'Proposals', !!isMobile);
    const picker = page.getByLabel('Cycle', { exact: true });
    const historyError = page
      .getByRole('alert')
      .filter({ hasText: 'Synthetic initial history outage' });
    await expect(historyError).toBeVisible();
    await expect(picker.locator('option')).toHaveCount(1);
    if (refusedAction) {
      await page.getByRole('button', { name: 'Start continuous', exact: true }).click();
      await expect(
        page.getByRole('alert').filter({ hasText: 'Synthetic control refusal' })
      ).toBeVisible();
    }
    outage = false;
    if (scenario === 'navigation away and back') {
      await openNavigation(page, 'Overview', !!isMobile);
      await openNavigation(page, 'Proposals', !!isMobile);
      await expect(picker.locator('option[value="cycle-1"]')).toHaveCount(1);
    }
    const beforeRecovery = reads;
    await page.clock.runFor(4000);
    await expect.poll(() => reads).toBeGreaterThan(beforeRecovery);
    await expect(picker.locator('option[value="cycle-1"]')).toHaveCount(1);
    await expect(historyError).toHaveCount(0);
    if (refusedAction)
      await expect(page.getByRole('alert')).toHaveText('Synthetic control refusal');
    else await expect(page.getByRole('alert')).toHaveCount(0);
    expect(writes).toBe(refusedAction ? 1 : 0);
  });
}

test('a successful history poll keeps a failed older-page request visible until it is retried', async ({
  page,
  isMobile
}) => {
  const { state } = await historyFixture(page);
  const picker = await openHistory(page, !!isMobile);
  const older = page.getByRole('button', { name: 'Load older cycles' });
  state.failOlder = true;
  await older.click();
  const failure = page.getByRole('alert');
  await expect(failure).toHaveText('Could not load older cycles. Synthetic older history outage');
  state.failOlder = false;
  const reads = state.reads.length;
  await page.clock.runFor(4000);
  await expect.poll(() => state.reads.length).toBeGreaterThan(reads);
  await expect(picker.locator('option')).toHaveCount(101);
  await expect(failure).toHaveText('Could not load older cycles. Synthetic older history outage');
  await expect(older).toBeEnabled();
  await older.click();
  await expect(picker.locator('option')).toHaveCount(201);
  await expect(failure).toHaveCount(0);
  expect(state.writes).toEqual([]);
});

test('new cycles do not expand loaded history by whole pages or skip older cycles', async ({
  page,
  isMobile
}) => {
  const { state, cursor } = await historyFixture(page);
  const picker = await openHistory(page, !!isMobile);
  await picker.selectOption('history-221');
  let reads = state.reads.length;
  await page.clock.runFor(4000);
  await expect.poll(() => state.reads.length).toBe(reads + 1);
  await expect(picker.locator('option')).toHaveCount(101);

  for (let added = 1; added <= 5; added++) {
    state.newest++;
    reads = state.reads.length;
    await page.clock.runFor(4000);
    await expect(picker.locator(`option[value="history-${state.newest}"]`)).toHaveCount(1);
    await expect(picker.locator('option')).toHaveCount(101 + added);
    expect(state.reads.length - reads).toBe(2);
    expect(state.reads.slice(reads).reduce((sum, read) => sum + read.rows, 0)).toBe(200);
    await expect(picker).toHaveValue('history-221');
  }

  const older = page.getByRole('button', { name: 'Load older cycles' });
  await older.click();
  await expect(picker.locator('option')).toHaveCount(206);
  expect(state.reads.at(-1)?.before).toBe(cursor(221));
  expect(
    await picker
      .locator('option')
      .evaluateAll((options) => options.map((o) => o.getAttribute('value')))
  ).toEqual(['all', ...Array.from({ length: 205 }, (_, i) => `history-${325 - i}`)]);
  state.newest++;
  await page.clock.runFor(4000);
  await expect(picker.locator('option')).toHaveCount(207);
  await older.click();
  await expect(picker.locator('option')).toHaveCount(307);
  expect(state.reads.at(-1)?.before).toBe(cursor(121));
  await older.click();
  await expect(picker.locator('option')).toHaveCount(327);
  await expect(older).toHaveCount(0);
  state.newest++;
  await page.clock.runFor(4000);
  await expect(picker.locator('option')).toHaveCount(328);
  await expect(older).toHaveCount(0);
  await expect(picker).toHaveValue('history-221');
});

for (const action of ['archive', 'discard'] as const) {
  test(`a failed later history page preserves the selected cycle and ${action} recovery`, async ({
    page,
    isMobile
  }) => {
    const { state } = await historyFixture(page);
    if (action === 'discard') state.lifecycle.archived_at = '2026-09-10T00:02:00Z';
    const picker = await openHistory(page, !!isMobile);
    await picker.selectOption('history-221');
    state.newest++;
    await page.clock.runFor(4000);
    await expect(picker.locator('option')).toHaveCount(102);
    await page
      .getByRole('button', {
        name: action === 'archive' ? 'Archive cycle' : 'Discard cycle workspaces',
        exact: true
      })
      .click();
    const feedback = page.getByRole('alert').filter({ hasText: 'Cycle history could not' });
    await expect(feedback).toContainText('Synthetic older history outage');
    await expect(picker.locator('option')).toHaveCount(102);
    await expect(picker).toHaveValue('history-221');
    state.failOlder = false;
    const retry = page.getByRole('button', { name: 'Retry cycle history' });
    await retry.focus();
    await page.keyboard.press('Enter');
    await expect(feedback).toHaveCount(0);
    await expect(picker).toBeFocused();
    await expect(picker).toHaveValue('history-221');
    await expect(picker.locator('option')).toHaveCount(102);
    await expect(picker.locator('option[value="history-221"]')).toContainText(
      action === 'archive' ? 'archived' : 'workspaces discarded'
    );
    expect(state.writes).toEqual([action]);
  });
}
