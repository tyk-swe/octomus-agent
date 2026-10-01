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

for (const delay of ['action', 'refresh']) {
  for (const destination of [
    'configuration',
    'overview',
    'proposal filters',
    'closed task',
    'pagination',
    'inline details'
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
      if (destination === 'proposal filters' || destination === 'inline details')
        await openNavigation(page, 'Proposals', !!isMobile);
      if (destination === 'inline details') {
        await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
        await page.clock.runFor(100);
        await expect(page.locator('.proposal-card').first()).toBeVisible();
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
        } else if (destination === 'inline details') {
          await page.locator('.proposal-card summary').first().click();
          await expect(page.locator('.proposal-card details').first()).toHaveAttribute('open', '');
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
          } else if (destination === 'inline details') {
            await expect(page.getByLabel('Cycle', { exact: true })).toHaveValue('cycle-1');
            await expect(page.locator('.proposal-card details').first()).toHaveAttribute(
              'open',
              ''
            );
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
