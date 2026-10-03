import { expect, type Page } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';
import { login, test } from './synthetic';

async function auditSnapshot(
  page: Page,
  wait: (snapshot: Snapshot) => Promise<void> = async () => {}
) {
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.configured = true;
    snapshot.audit_configured = true;
    snapshot.control.paused = true;
    snapshot.control.mode = 'paused';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = false;
    snapshot.active_cycle_mode = null;
    snapshot.baseline_active = false;
    await wait(snapshot);
    await route.fulfill({ response, json: snapshot });
  });
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

for (const capacity of ['daily_exhausted', 'limit_too_low'] as const) {
  test(`planning controls respect ${capacity} and recover with refreshed capacity`, async ({
    page
  }) => {
    let status: Snapshot['planning_capacity']['status'] = capacity;
    let queued = false;
    const writes: string[] = [];
    await auditSnapshot(page, async (snapshot) => {
      snapshot.tasks = [];
      snapshot.attention_tasks = [];
      snapshot.counts = queued ? { queued: 1 } : {};
      snapshot.planning_capacity = {
        ...snapshot.planning_capacity,
        limit: status === 'limit_too_low' ? 12 : 150,
        required: 13,
        used: status === 'daily_exhausted' ? 138 : 0,
        remaining: status === 'ready' ? 150 : 12,
        status
      };
    });
    await page.route('**/api/control/*', async (route) => {
      writes.push(new URL(route.request().url()).pathname);
      await route.fulfill({ json: { paused: true } });
    });
    await page.clock.install();
    await login(page);
    await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
    const run = page.getByRole('button', { name: 'Run once', exact: true });
    const audit = page.getByRole('button', { name: 'Run an audit', exact: true });
    const discover = page.getByRole('button', { name: 'Discover opportunities', exact: true });
    const continuous = page.getByRole('button', { name: 'Start continuous', exact: true });
    const notice = page.getByRole('status').filter({
      hasText: 'Audit and Run once are refused until planning can be funded.'
    });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText('13 daily admissions; 12 remain today.');
    for (const control of [run, audit, discover]) {
      await expect(control).toBeDisabled();
      await control.dispatchEvent('click');
    }
    expect(writes).toEqual([]);
    // Continuous operation may wait for the next allowance, so it stays available.
    await expect(continuous).toBeEnabled();
    await continuous.focus();
    await page.keyboard.press('Enter');
    await expect.poll(() => writes).toEqual(['/api/control/resume']);
    await expect(continuous).toBeEnabled();
    for (const control of [run, audit, discover]) await expect(control).toBeDisabled();

    // Run once also requires a complete planning allowance before draining a queue.
    queued = true;
    await page.clock.runFor(4000);
    await expect(discover).toHaveCount(0);
    await expect(run).toBeDisabled();
    await expect(audit).toBeDisabled();
    await expect(continuous).toBeEnabled();

    queued = false;
    status = 'ready';
    await page.clock.runFor(4000);
    for (const control of [run, audit, discover]) await expect(control).toBeEnabled();
    await expect(notice).toHaveCount(0);
    const recovered = capacity === 'daily_exhausted' ? audit : discover;
    await recovered.focus();
    await page.keyboard.press('Enter');
    await expect
      .poll(() => writes)
      .toEqual([
        '/api/control/resume',
        capacity === 'daily_exhausted' ? '/api/control/audit' : '/api/control/cycle'
      ]);
  });
}
