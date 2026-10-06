import { expect, type Page } from '@playwright/test';
import type { CycleSummary } from '../src/lib/types';
import { deferred, login, nextPoll, openNavigation, test } from './synthetic';

for (const list of [
  {
    view: 'Task queue',
    endpoint: 'tasks',
    noun: 'tasks',
    rows: '.task-row',
    empty: 'The next good idea starts here.',
    filter: 'queued'
  },
  {
    view: 'Proposals',
    endpoint: 'proposals',
    noun: 'proposals',
    rows: '.proposal-card',
    empty: 'Better ideas start with questions.',
    filter: 'rejected'
  },
  {
    view: 'Pull requests',
    endpoint: 'prs',
    noun: 'pull requests',
    rows: '.pr-row',
    empty: 'Room for your next improvement.',
    filter: 'merged'
  }
]) {
  test(`${list.view} distinguishes loading, retry, retained results and filtered emptiness`, async ({
    page,
    isMobile
  }) => {
    let mode: 'failure' | 'rows' | 'empty' = 'failure';
    let gate: ReturnType<typeof deferred> | null = deferred();
    await page.route(`**/api/${list.endpoint}?*`, async (route) => {
      await gate?.promise;
      if (mode === 'failure')
        await route.fulfill({ status: 503, json: { error: 'Synthetic list failure' } });
      else {
        const result = await (await route.fetch()).json();
        if (mode === 'empty') result.items = [];
        await route.fulfill({ json: result });
      }
    });
    await page.clock.install();
    await login(page);
    await openNavigation(page, list.view, !!isMobile);
    await expect(page.getByText(`Loading ${list.noun}…`, { exact: true })).toBeVisible();
    await expect(page.getByRole('heading', { name: list.empty, exact: true })).toHaveCount(0);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText(`Could not load ${list.noun}`);
    mode = 'rows';
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.locator(list.rows).first()).toBeVisible();
    const count = await page.locator(list.rows).count();
    const rowPositions = () =>
      page.locator(list.rows).evaluateAll((rows) =>
        rows.map((row) => {
          const { x, y, width, height } = row.getBoundingClientRect();
          return { x: x + scrollX, y: y + scrollY, width, height };
        })
      );
    const positions = await rowPositions();
    gate = deferred();
    await nextPoll(page);
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toBeVisible({
      timeout: 10000
    });
    await expect(page.locator(list.rows)).toHaveCount(count);
    expect(await rowPositions()).toEqual(positions);
    mode = 'failure';
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText('Showing the last received results');
    await expect(page.locator(list.rows)).toHaveCount(count);
    expect(await rowPositions()).toEqual(positions);
    mode = 'rows';
    gate = deferred();
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toBeVisible();
    expect(await rowPositions()).toEqual(positions);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toHaveCount(0);
    expect(await rowPositions()).toEqual(positions);
    mode = 'empty';
    await nextPoll(page);
    await expect(page.getByRole('heading', { name: list.empty, exact: true })).toBeVisible({
      timeout: 10000
    });
    await page.getByRole('button', { name: list.filter, exact: true }).click();
    await expect(
      page.getByRole('heading', { name: `No matching ${list.noun}`, exact: true })
    ).toBeVisible();
    await expect(page.getByRole('button', { name: list.filter, exact: true })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    if (list.endpoint === 'proposals') {
      await page.getByRole('button', { name: 'all', exact: true }).click();
      await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
      await expect(
        page.getByRole('heading', { name: 'No matching proposals', exact: true })
      ).toBeVisible();
    }
  });
}

test('a refused cycle archive remains retryable and distinct from history recovery', async ({
  page,
  isMobile
}) => {
  const state = { outage: false, refusal: true, writes: 0 };
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
    lifecycle: {}
  }));
  await page.route('**/api/cycles?*', async (route) => {
    if (state.outage)
      await route.fulfill({ status: 503, json: { error: 'Synthetic cycle history outage' } });
    else
      await route.fulfill({
        json: { items: structuredClone(cycles), next_cursor: null, counts: {} }
      });
  });
  await page.route('**/api/cycles/history-1/archive', async (route) => {
    state.writes++;
    if (state.refusal) {
      await route.fulfill({ status: 409, json: { error: 'Synthetic cycle action refusal' } });
      return;
    }
    cycles[1].lifecycle.archived_at = '2026-09-10T00:02:00Z';
    state.outage = true;
    await route.fulfill({ json: { ok: true } });
  });
  await page.clock.install();
  await login(page);
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
  await openNavigation(page, 'Proposals', !!isMobile);
  await page.getByLabel('Cycle', { exact: true }).selectOption('history-1');
  const archive = page.getByRole('button', { name: 'Archive cycle', exact: true });
  await archive.click();
  await expect(page.getByRole('alert')).toHaveText(
    'Cycle action failed. Synthetic cycle action refusal'
  );
  await expect(archive).toBeEnabled();
  await page.clock.runFor(4000);
  await expect(page.getByRole('alert')).toHaveText(
    'Cycle action failed. Synthetic cycle action refusal'
  );
  await expect(page.getByRole('button', { name: 'Retry cycle history' })).toHaveCount(0);
  state.refusal = false;
  await archive.click();
  await expect(page.getByRole('alert')).toContainText('Cycle history could not be refreshed');
  await expect(page.getByText('Synthetic cycle action refusal', { exact: false })).toHaveCount(0);
  expect(state.writes).toBe(2);
});

test('a refused task action remains visible across polling until the next action attempt', async ({
  page,
  isMobile
}) => {
  const taskPath = '**/api/tasks/task-blocked';
  const retry = deferred();
  let writes = 0;
  let failPoll = false;
  await page.route(taskPath, async (route) => {
    if (failPoll)
      await route.fulfill({ status: 503, json: { error: 'Synthetic task read outage' } });
    else await route.continue();
  });
  await page.route('**/api/tasks/task-blocked/archive', async (route) => {
    if (++writes === 1) {
      await route.fulfill({ status: 409, json: { error: 'Synthetic archive refusal' } });
    } else {
      await retry.promise;
      await route.fulfill({ json: { ok: true } });
    }
  });
  async function pollTask(page: Page, change: () => void = () => {}) {
    change();
    await nextPoll(page, taskPath);
  }
  await page.clock.install();
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  await page.getByRole('button', { name: /Handle interrupted verification commands/ }).click();
  const dialog = page.getByRole('dialog');
  const archive = dialog.getByRole('button', { name: 'Archive task' });
  await expect(archive).toBeEnabled();
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
