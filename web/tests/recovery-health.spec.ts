import { expect } from '@playwright/test';
import { login, openNavigation, test, trackWrites } from './synthetic';
import type { Snapshot } from '../src/lib/types';

// Polling and acknowledged controls can leave a state fetch in flight after the
// final assertion. Join those handlers while the page is still available.
test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'wait' });
});

for (const { mode, orphanedAudit } of [
  { mode: 'paused', orphanedAudit: false },
  { mode: 'continuous', orphanedAudit: false },
  { mode: 'continuous', orphanedAudit: true }
] as const) {
  test(`${mode} recovery ${orphanedAudit ? 'with an orphaned audit' : 'without a running cycle'} blocks new work but permits pause and heals on polling`, async ({
    page
  }) => {
    let recovering = true;
    let paused = mode === 'paused';
    let control: Snapshot['control'];
    await page.route('**/api/state', async (route) => {
      const response = await route.fetch();
      const snapshot = (await response.json()) as Snapshot;
      snapshot.status = recovering ? 'unhealthy' : paused ? 'paused' : 'idle';
      snapshot.recovery_error = recovering ? 'Synthetic saved-state recovery refusal' : null;
      snapshot.configured = true;
      snapshot.audit_configured = true;
      snapshot.control.error = null;
      snapshot.control.paused = paused;
      snapshot.control.mode = paused ? 'paused' : 'continuous';
      // StateView exposes durable running cycles even when their workers are gone.
      snapshot.active_cycle_mode = recovering && orphanedAudit ? 'audit' : null;
      snapshot.cycle_active = recovering && orphanedAudit;
      snapshot.active_tasks = 0;
      snapshot.baseline_active = false;
      snapshot.planning_capacity.status = 'ready';
      control = snapshot.control;
      await route.fulfill({ json: snapshot });
    });
    await page.route('**/api/control/*', async (route) => {
      const action = new URL(route.request().url()).pathname.split('/').at(-1);
      paused = action !== 'resume';
      await route.fulfill({
        json: { ...control, paused, mode: paused ? 'paused' : 'continuous' }
      });
    });
    await page.clock.install();
    await login(page);
    await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
    const writes = trackWrites(page);
    const alert = page.getByRole('alert', { name: 'Recovery status' });
    await expect(alert).toBeVisible();
    for (const name of ['Run once', 'Run an audit'])
      await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();

    if (mode === 'continuous') {
      const pause = page.getByRole('button', { name: 'Pause', exact: true });
      await expect(pause).toBeEnabled();
      await pause.click();
      await expect.poll(() => writes).toEqual([{ path: '/api/control/pause', method: 'POST' }]);
    }
    const expectedWrites = [...writes];
    for (const name of ['Start continuous', 'Run once', 'Run an audit']) {
      const button = page.getByRole('button', { name, exact: true });
      await expect(button).toBeDisabled();
      // Remove the native disabled flag to exercise the handler's admission guard.
      await button.evaluate((element: HTMLButtonElement) => {
        element.disabled = false;
        element.click();
        element.disabled = true;
      });
    }
    expect(writes).toEqual(expectedWrites);

    recovering = false;
    await page.clock.runFor(4000);
    await expect(alert).toHaveCount(0);
    for (const name of ['Start continuous', 'Run once', 'Run an audit'])
      await expect(page.getByRole('button', { name, exact: true })).toBeEnabled();
    expect(writes).toEqual(expectedWrites);

    const action = mode === 'paused' ? 'Run an audit' : 'Start continuous';
    await page.getByRole('button', { name: action, exact: true }).click();
    await expect
      .poll(() => writes)
      .toEqual([
        ...expectedWrites,
        { path: `/api/control/${mode === 'paused' ? 'audit' : 'resume'}`, method: 'POST' }
      ]);
  });
}

for (const mode of ['continuous', 'audit'] as const) {
  test(`retrying ${mode} recovery stays visible and clears when polling heals`, async ({
    page
  }) => {
    let recovering = true;
    const cause = 'Synthetic saved-state recovery refusal';
    await page.route('**/api/state', async (route) => {
      const response = await route.fetch();
      const snapshot = (await response.json()) as Snapshot;
      snapshot.status = recovering ? 'unhealthy' : mode === 'audit' ? 'paused' : 'idle';
      snapshot.recovery_error = recovering ? cause : null;
      snapshot.control.error = null;
      snapshot.control.paused = mode === 'audit';
      snapshot.control.mode = mode === 'audit' ? 'paused' : 'continuous';
      snapshot.active_cycle_mode = recovering && mode === 'audit' ? 'audit' : null;
      snapshot.cycle_active = recovering;
      await route.fulfill({ json: snapshot });
    });
    await login(page);
    const writes = trackWrites(page);
    const alert = page.getByRole('alert', { name: 'Recovery status' });
    await expect(alert).toContainText('Recovery is retrying.');
    await expect(alert).toContainText(cause);
    await expect(page.locator('.status-value')).toHaveText('unhealthy');
    await expect(
      page.getByText('Waiting for saved state to recover', { exact: true })
    ).toBeVisible();
    await expect(page.getByText('Discovering the next opportunity', { exact: true })).toHaveCount(
      0
    );
    await expect(
      page.getByText('Audit in progress. Execution stays paused;', { exact: false })
    ).toHaveCount(0);

    recovering = false;
    await expect(alert).toHaveCount(0, { timeout: 10000 });
    await expect(page.locator('.status-value')).toHaveText(mode === 'audit' ? 'paused' : 'idle');
    await expect(page.getByText('Waiting for saved state to recover', { exact: true })).toHaveCount(
      0
    );
    expect(writes).toEqual([]);
  });
}

test('setup checklist reflects recovery and heals without reopening Configuration', async ({
  page,
  isMobile
}) => {
  let recovering = true;
  await page.route('**/api/state', async (route) => {
    const snapshot = (await (await route.fetch()).json()) as Snapshot;
    snapshot.recovery_error = recovering ? 'Synthetic recovery refusal' : null;
    snapshot.configured = true;
    snapshot.audit_configured = true;
    snapshot.control.paused = true;
    snapshot.control.mode = 'paused';
    snapshot.active_cycle_mode = null;
    snapshot.cycle_active = false;
    snapshot.active_tasks = 0;
    snapshot.baseline_active = false;
    snapshot.planning_capacity.status = 'ready';
    await route.fulfill({ json: snapshot });
  });
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const step = page.locator('[data-step="choose"]');
  await expect(step).toContainText(
    'Unavailable now: Saved-state recovery is retrying. New work waits until recovery completes.'
  );
  await expect(step).not.toContainText('Audit: available');
  await expect(step).not.toContainText('queued tasks would be drained first');
  recovering = false;
  await expect(step).toContainText('Audit: available. Run once: available', { timeout: 10000 });
  await expect(step).not.toContainText('Saved-state recovery is retrying');
});
