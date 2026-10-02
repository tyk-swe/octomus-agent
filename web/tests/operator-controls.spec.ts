import { expect, type Page } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';
import { login, openNavigation, test, token } from './synthetic';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

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

for (const delay of ['action', 'refresh']) {
  for (const destination of [
    'configuration',
    'overview',
    'proposal filters',
    'closed task',
    'pagination',
    'inline details',
    'inline collapse'
  ]) {
    test(`an audit waiting for ${delay} respects newer ${destination} navigation`, async ({
      page,
      isMobile
    }) => {
      const gate = deferred();
      let actionFinished = false;
      let waiting = false;
      let writes = 0;
      await auditSnapshot(page, async (snapshot) => {
        if (actionFinished) snapshot.status = 'synthetic-audit-started';
        if (delay === 'refresh' && actionFinished) {
          waiting = true;
          await gate.promise;
        }
      });
      await page.route('**/api/control/audit', async (route) => {
        writes++;
        if (delay === 'action') {
          waiting = true;
          await gate.promise;
        }
        actionFinished = true;
        await route.fulfill({ json: { paused: true } });
      });
      const cursors: (string | null)[] = [];
      if (destination === 'pagination') {
        await page.route('**/api/tasks?*', async (route) => {
          const before = new URL(route.request().url()).searchParams.get('before');
          cursors.push(before);
          const response = await route.fetch();
          const result = await response.json();
          result.next_cursor = before === null ? 42 : null;
          await route.fulfill({ response, json: result });
        });
      }
      await page.clock.install();
      await login(page);
      await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
      if (['proposal filters', 'inline details', 'inline collapse'].includes(destination))
        await openNavigation(page, 'Proposals', !!isMobile);
      if (destination === 'inline details' || destination === 'inline collapse') {
        await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
        await page.clock.runFor(100);
        await expect(page.locator('.proposal-card').first()).toBeVisible();
        if (destination === 'inline collapse') {
          await page.locator('.proposal-card summary').first().click();
          await expect(page.locator('.proposal-card .prompt').first()).not.toHaveText('');
        }
      }
      if (destination === 'pagination') {
        await openNavigation(page, 'Task queue', !!isMobile);
        await page.clock.runFor(100);
        await expect(page.getByRole('button', { name: 'Next page', exact: true })).toBeEnabled();
      }
      await page.getByRole('button', { name: 'Run an audit', exact: true }).click();
      await expect.poll(() => waiting).toBe(true);
      const pending = page.getByRole('button', { name: 'Starting audit…', exact: true });
      await expect(pending).toBeDisabled();
      await pending.dispatchEvent('click');
      try {
        if (destination === 'configuration') {
          await openNavigation(page, 'Configuration', !!isMobile);
          await page.getByLabel('Default branch', { exact: true }).fill('operator-draft');
        } else if (destination === 'overview') {
          await openNavigation(page, 'Task queue', !!isMobile);
          await openNavigation(page, 'Overview', !!isMobile);
        } else if (destination === 'proposal filters') {
          await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
          await page.getByRole('button', { name: 'rejected', exact: true }).click();
        } else if (destination === 'pagination') {
          await page.getByRole('button', { name: 'Next page', exact: true }).click();
          await page.clock.runFor(100);
          await expect(
            page.getByRole('button', { name: 'Previous page', exact: true })
          ).toBeEnabled();
          expect(cursors).toContain('42');
        } else if (destination === 'inline details' || destination === 'inline collapse') {
          await page.locator('.proposal-card summary').first().click();
          if (destination === 'inline details')
            await expect(page.locator('.proposal-card details').first()).toHaveAttribute(
              'open',
              ''
            );
          else
            await expect(page.locator('.proposal-card details').first()).not.toHaveAttribute(
              'open',
              ''
            );
        } else {
          await page
            .getByRole('button', { name: /Handle interrupted verification commands/ })
            .click();
          await page.getByRole('button', { name: 'Close task details' }).click();
        }
        const refreshed = page.waitForResponse(
          async (response) =>
            new URL(response.url()).pathname === '/api/state' &&
            (await response.json()).status === 'synthetic-audit-started'
        );
        gate.resolve();
        await (await refreshed).finished();
        await page.clock.runFor(100);
        if (destination === 'configuration') {
          await expect(page.getByRole('heading', { name: 'Make it work your way.' })).toBeVisible();
          await expect(page.getByLabel('Default branch', { exact: true })).toHaveValue(
            'operator-draft'
          );
          await expect(page.getByLabel('Default branch', { exact: true })).toBeFocused();
        } else {
          await expect(
            page.getByRole('button', { name: 'Run an audit', exact: true })
          ).toBeEnabled();
          if (destination === 'proposal filters') {
            await expect(page.getByLabel('Cycle', { exact: true })).toHaveValue('cycle-1');
            await expect(
              page.getByRole('button', { name: 'rejected', exact: true })
            ).toHaveAttribute('aria-pressed', 'true');
          } else if (destination === 'pagination') {
            await expect(
              page.getByRole('heading', { name: 'From idea to improvement.' })
            ).toBeVisible();
            await expect(
              page.getByRole('button', { name: 'Previous page', exact: true })
            ).toBeEnabled();
          } else if (destination === 'inline details' || destination === 'inline collapse') {
            await expect(page.getByLabel('Cycle', { exact: true })).toHaveValue('cycle-1');
            const details = page.locator('.proposal-card details').first();
            if (destination === 'inline details') await expect(details).toHaveAttribute('open', '');
            else await expect(details).not.toHaveAttribute('open', '');
            await expect(page.locator('.proposal-card summary').first()).toBeFocused();
          } else {
            await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
            await expect(page.getByRole('dialog')).toHaveCount(0);
          }
        }
        expect(writes).toBe(1);
      } finally {
        gate.resolve();
      }
    });
  }
}

test('an audit whose refresh expires the session cannot navigate the next login', async ({
  page
}) => {
  await auditSnapshot(page);
  let expire = false;
  let expireNextAudit = true;
  await page.route('**/api/state', async (route) => {
    if (expire) {
      await route.fulfill({ status: 401, json: { error: 'Synthetic expired session' } });
    } else await route.fallback();
  });
  await page.route('**/api/control/audit', async (route) => {
    expire = expireNextAudit;
    await route.fulfill({ json: { paused: true } });
  });
  await login(page);
  await page.getByRole('button', { name: 'Run an audit', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible();
  expire = false;
  expireNextAudit = false;
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
  await page.getByRole('button', { name: 'Run an audit', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Worth doing. Before doing.' })).toBeVisible();
});

test('passive history refresh does not cancel the audit redirect', async ({ page, isMobile }) => {
  const gate = deferred();
  let reads = 0;
  await auditSnapshot(page);
  await page.route('**/api/tasks?*', async (route) => {
    reads++;
    await route.continue();
  });
  await page.route('**/api/control/audit', async (route) => {
    await gate.promise;
    await route.fulfill({ json: { paused: true } });
  });
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  await expect(page.locator('.task-row').first()).toBeVisible();
  try {
    await page.getByRole('button', { name: 'Run an audit', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Starting audit…', exact: true })).toBeDisabled();
    const before = reads;
    await expect.poll(() => reads, { timeout: 10000 }).toBeGreaterThan(before);
    gate.resolve();
    await expect(page.getByRole('heading', { name: 'Worth doing. Before doing.' })).toBeVisible();
  } finally {
    gate.resolve();
  }
});

test('passive expanded proposal reloads do not cancel the audit redirect', async ({
  page,
  isMobile
}) => {
  const gate = deferred();
  let revision = 1;
  await auditSnapshot(page);
  await page.route('**/api/proposals?*', async (route) => {
    const response = await route.fetch();
    const result = await response.json();
    result.items = result.items.map((row: object) => ({ ...row, content_revision: revision }));
    await route.fulfill({ response, json: result });
  });
  await page.route('**/api/proposals/cycle-1/*', async (route) => {
    const response = await route.fetch();
    const result = await response.json();
    await route.fulfill({
      response,
      json: { ...result, content_revision: revision, prompt: `Passive detail revision ${revision}` }
    });
  });
  await page.route('**/api/control/audit', async (route) => {
    await gate.promise;
    await route.fulfill({ json: { paused: true } });
  });
  await login(page);
  await openNavigation(page, 'Proposals', !!isMobile);
  await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
  await page.locator('.proposal-card summary').first().click();
  await expect(page.getByText('Passive detail revision 1', { exact: true })).toBeVisible();
  try {
    await page.getByRole('button', { name: 'Run an audit', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Starting audit…', exact: true })).toBeDisabled();
    revision = 2;
    await expect(page.getByText('Passive detail revision 2', { exact: true })).toBeVisible({
      timeout: 10000
    });
    gate.resolve();
    await expect(page.getByLabel('Cycle', { exact: true })).toHaveValue('all');
    await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeEnabled();
  } finally {
    gate.resolve();
  }
});

async function pendingAudit(page: Page, delay: string) {
  const gate = deferred();
  let actionFinished = false;
  let waiting = false;
  await auditSnapshot(page, async (snapshot) => {
    if (actionFinished) snapshot.status = 'synthetic-intent-audit-started';
    if (delay === 'refresh' && actionFinished) {
      waiting = true;
      await gate.promise;
    }
  });
  await page.route('**/api/control/audit', async (route) => {
    if (delay === 'action') {
      waiting = true;
      await gate.promise;
    }
    actionFinished = true;
    await route.fulfill({ json: { paused: true } });
  });
  return {
    release: gate.resolve,
    async start() {
      await page.getByRole('button', { name: 'Run an audit', exact: true }).click();
      await expect.poll(() => waiting).toBe(true);
    },
    async finish() {
      const refreshed = page.waitForResponse(
        async (response) =>
          new URL(response.url()).pathname === '/api/state' &&
          (await response.json()).status === 'synthetic-intent-audit-started'
      );
      gate.resolve();
      await (await refreshed).finished();
      await page.clock.runFor(100);
    }
  };
}

for (const delay of ['action', 'refresh']) {
  for (const intent of [
    { action: 'older cycles', view: 'Proposals', heading: 'Worth doing. Before doing.' },
    { action: 'task filter', view: 'Task queue', heading: 'From idea to improvement.' },
    { action: 'proposal filter', view: 'Proposals', heading: 'Worth doing. Before doing.' },
    { action: 'PR filter', view: 'Pull requests', heading: 'Progress, ready for review.' },
    { action: 'history retry', view: 'Task queue', heading: 'From idea to improvement.' },
    { action: 'search focus', view: 'Task queue', heading: 'From idea to improvement.' },
    { action: 'search shortcut', view: 'Task queue', heading: 'From idea to improvement.' },
    { action: 'cycle picker focus', view: 'Proposals', heading: 'Worth doing. Before doing.' }
  ]) {
    test(`an audit waiting for ${delay} preserves explicit ${intent.action} intent`, async ({
      page,
      isMobile
    }) => {
      const audit = await pendingAudit(page, delay);
      if (intent.action === 'older cycles') {
        await page.route('**/api/cycles?*', async (route) => {
          const response = await route.fetch();
          const result = await response.json();
          if (new URL(route.request().url()).searchParams.has('before')) {
            // Fetch the first page only to seed the synthetic older cycle consistently.
            const url = new URL(route.request().url());
            url.searchParams.delete('before');
            const first = await (await route.fetch({ url: url.toString() })).json();
            result.items = [{ ...first.items[0], id: 'older-cycle', number: 0 }];
            result.next_cursor = null;
          } else result.next_cursor = 42;
          await route.fulfill({ response, json: result });
        });
      }
      let failList = intent.action === 'history retry';
      if (failList)
        await page.route('**/api/tasks?*', async (route) => {
          if (failList)
            await route.fulfill({ status: 503, json: { error: 'Synthetic history failure' } });
          else await route.continue();
        });
      await page.clock.install();
      await login(page);
      await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
      await openNavigation(page, intent.view, !!isMobile);
      await page.clock.runFor(100);
      const cycle = page.getByLabel('Cycle', { exact: true });
      const search = page.getByLabel('Search work');
      const filter = page.getByRole('button', { name: 'all', exact: true });
      if (intent.action === 'older cycles' || intent.action === 'cycle picker focus')
        await cycle.selectOption('cycle-1');
      if (intent.action === 'history retry')
        await expect(page.getByRole('alert')).toContainText('Synthetic history failure');
      try {
        await audit.start();
        if (intent.action === 'older cycles') {
          await page.getByRole('button', { name: 'Load older cycles', exact: true }).click();
          await expect(cycle.locator('option[value="older-cycle"]')).toHaveCount(1);
        } else if (intent.action.endsWith('filter')) await filter.click();
        else if (intent.action === 'history retry') {
          failList = false;
          await page.getByRole('button', { name: 'Retry', exact: true }).click();
          await page.clock.runFor(100);
          await expect(page.getByRole('alert')).toHaveCount(0);
        } else if (intent.action === 'search focus') await search.focus();
        else if (intent.action === 'search shortcut') await page.keyboard.press('/');
        else await cycle.focus();
        await audit.finish();
        await expect(page.getByRole('heading', { name: intent.heading })).toBeVisible();
        await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeEnabled();
        if (intent.action.endsWith('filter')) await expect(filter).toBeFocused();
        if (intent.action.startsWith('search')) {
          await expect(search).toBeFocused();
          await expect(search).toHaveValue('');
        }
        if (intent.action === 'older cycles' || intent.action === 'cycle picker focus')
          await expect(cycle).toHaveValue('cycle-1');
        if (intent.action === 'cycle picker focus') await expect(cycle).toBeFocused();
      } finally {
        audit.release();
      }
    });
  }

  for (const menu of ['open', 'toggle closed', 'Escape closed']) {
    test(`an audit waiting for ${delay} preserves a mobile menu ${menu}`, async ({
      page,
      isMobile
    }) => {
      test.skip(!isMobile, 'The menu toggle is a mobile navigation control.');
      const audit = await pendingAudit(page, delay);
      await page.clock.install();
      await login(page);
      await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
      const toggle = page.getByRole('button', { name: 'Toggle navigation' });
      try {
        await audit.start();
        await toggle.click();
        await expect(toggle).toHaveAttribute('aria-expanded', 'true');
        if (menu === 'toggle closed') await toggle.click();
        if (menu === 'Escape closed') await page.keyboard.press('Escape');
        await audit.finish();
        await expect(toggle).toHaveAttribute('aria-expanded', menu === 'open' ? 'true' : 'false');
        await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
        if (menu === 'open')
          await expect(
            page.getByRole('navigation').getByRole('button', { name: 'Overview', exact: true })
          ).toBeFocused();
        else await expect(toggle).toBeFocused();
      } finally {
        audit.release();
      }
    });
  }
}
