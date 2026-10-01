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
  for (const destination of ['configuration', 'overview', 'proposal filters', 'closed task']) {
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
      await page.clock.install();
      await login(page);
      await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
      if (destination === 'proposal filters') await openNavigation(page, 'Proposals', !!isMobile);
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
