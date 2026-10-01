import { expect } from '@playwright/test';
import { login, openNavigation, proposalRow, serveProposals, test } from './synthetic';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

const endpoint = '**/api/proposals/cycle-1/recovery-proposal';
const summary = () =>
  proposalRow('recovery-proposal', 'cycle-1', 1, {
    title: 'Recover proposal details',
    prompt: '',
    evidence: []
  });

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('failed proposal details show an inline retry without losing the expanded summary', async ({
  page,
  isMobile
}) => {
  const row = summary();
  await serveProposals(page, [row]);
  const first = deferred();
  const retry = deferred();
  let reads = 0;
  await page.route(endpoint, async (route) => {
    const attempt = ++reads;
    await (attempt === 1 ? first : retry).promise;
    if (attempt === 1)
      await route.fulfill({ status: 503, json: { error: 'Synthetic detail outage' } });
    else
      await route.fulfill({
        json: { ...row, prompt: 'Recovered execution prompt', evidence: ['Recovered evidence'] }
      });
  });
  await login(page);
  await openNavigation(page, 'Proposals', !!isMobile);
  const card = page.locator('.proposal-card');
  const toggle = card.getByText('Scope, evidence & execution prompt', { exact: true });
  try {
    await toggle.click();
    await expect(card.getByRole('status')).toHaveText('Loading full proposal details…');
    await expect(card).toContainText(row.scope);
    first.resolve();
    await expect(card.getByRole('alert')).toHaveText(
      'Could not load full proposal details. Showing the summary. Synthetic detail outage'
    );
    await expect(page.getByRole('alert')).toHaveCount(1);
    await expect(card.locator('details')).toHaveAttribute('open', '');
    const button = card.getByRole('button', { name: 'Retry details', exact: true });
    await button.click();
    await expect(
      card.getByRole('button', { name: 'Retrying details…', exact: true })
    ).toBeDisabled();
    await card
      .getByRole('button', { name: 'Retrying details…', exact: true })
      .dispatchEvent('click');
    await expect.poll(() => reads).toBe(2);
    retry.resolve();
    await expect(card.getByText('Recovered execution prompt', { exact: true })).toBeVisible();
    await expect(card.getByText('Recovered evidence', { exact: true })).toBeVisible();
    await expect(card.getByRole('alert')).toHaveCount(0);
    await expect(card.getByRole('status')).toHaveCount(0);
    await expect(toggle).toBeFocused();
    await toggle.click();
    await toggle.click();
    await expect(card.getByText('Recovered execution prompt', { exact: true })).toBeVisible();
    expect(reads).toBe(2);
  } finally {
    first.resolve();
    retry.resolve();
  }
});

for (const phase of ['first load', 'retry']) {
  test(`a delayed proposal-detail ${phase} cannot add alerts after navigation`, async ({
    page,
    isMobile
  }) => {
    await serveProposals(page, [summary()]);
    const gate = deferred();
    const errors: string[] = [];
    page.on('pageerror', (error) => errors.push(error.message));
    let started = false;
    let reads = 0;
    await page.route(endpoint, async (route) => {
      if (++reads === 1 && phase === 'retry') {
        await route.fulfill({ status: 503, json: { error: 'Synthetic initial detail failure' } });
        return;
      }
      started = true;
      await gate.promise;
      await route.fulfill({ status: 503, json: { error: 'Synthetic obsolete detail failure' } });
    });
    await login(page);
    await openNavigation(page, 'Proposals', !!isMobile);
    try {
      await page.getByText('Scope, evidence & execution prompt', { exact: true }).click();
      if (phase === 'retry')
        await page.getByRole('button', { name: 'Retry details', exact: true }).click();
      await expect.poll(() => started).toBe(true);
      await openNavigation(page, 'Task queue', !!isMobile);
      await expect(page.getByRole('heading', { name: 'From idea to improvement.' })).toBeVisible();
      const response = page.waitForResponse(endpoint);
      gate.resolve();
      await (await response).finished();
      await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(resolve)));
      await expect(page.getByRole('alert')).toHaveCount(0);
      await expect(page.getByText('Synthetic obsolete detail failure')).toHaveCount(0);
      expect(errors).toEqual([]);
    } finally {
      gate.resolve();
    }
  });
}

test('a changed proposal revision clears its failed detail load and retries the current content', async ({
  page,
  isMobile
}) => {
  const row = summary();
  await serveProposals(page, [row]);
  let reads = 0;
  await page.route(endpoint, async (route) => {
    reads++;
    if (row.content_revision === 1)
      await route.fulfill({ status: 503, json: { error: 'Synthetic old-revision outage' } });
    else
      await route.fulfill({
        json: { ...row, prompt: 'Current revision prompt', evidence: ['Current revision evidence'] }
      });
  });
  await login(page);
  await openNavigation(page, 'Proposals', !!isMobile);
  const card = page.locator('.proposal-card');
  await card.getByText('Scope, evidence & execution prompt', { exact: true }).click();
  await expect(card.getByRole('alert')).toContainText('Synthetic old-revision outage');
  row.content_revision = 2;
  await expect(card.getByText('Current revision prompt', { exact: true })).toBeVisible({
    timeout: 10000
  });
  await expect(card.getByRole('alert')).toHaveCount(0);
  await expect(card.getByRole('button', { name: 'Retry details' })).toHaveCount(0);
  expect(reads).toBe(2);
});
