import { expect } from '@playwright/test';
import { login, test, trackWrites } from './synthetic';
import type { Snapshot } from '../src/lib/types';

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
