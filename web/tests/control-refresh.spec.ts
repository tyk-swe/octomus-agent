import { expect, type Page } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';
import { login, test } from './synthetic';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

async function controlFixture(page: Page) {
  const state = {
    mode: 'paused' as Snapshot['control']['mode'],
    auditing: false,
    outage: false,
    refusal: false,
    reads: 0,
    writes: [] as string[],
    writeHold: null as ReturnType<typeof deferred> | null,
    holds: [] as ReturnType<typeof deferred>[]
  };
  let control: Snapshot['control'];
  await page.route('**/api/state', async (route) => {
    // Capture the state when this read starts, before another request can mutate it.
    const { mode, auditing, outage } = state;
    const hold = state.holds.shift();
    state.reads++;
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.configured = true;
    snapshot.audit_configured = true;
    snapshot.control.mode = mode;
    snapshot.control.paused = mode === 'paused';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = auditing;
    snapshot.active_cycle_mode = auditing ? 'audit' : null;
    snapshot.baseline_active = false;
    snapshot.planning_capacity.status = 'ready';
    control = snapshot.control;
    if (hold) await hold.promise;
    if (outage)
      await route.fulfill({ status: 503, json: { error: 'Synthetic state refresh outage' } });
    else await route.fulfill({ response, json: snapshot });
  });
  await page.route('**/api/control/*', async (route) => {
    const action = new URL(route.request().url()).pathname.split('/').at(-1)!;
    state.writes.push(action);
    const writeHold = state.writeHold;
    state.writeHold = null;
    if (writeHold) await writeHold.promise;
    if (state.refusal) {
      await route.fulfill({ status: 409, json: { error: 'Synthetic control refusal' } });
      return;
    }
    if (action === 'cycle') state.mode = 'run_once';
    if (action === 'resume') state.mode = 'continuous';
    if (action === 'pause') state.mode = 'paused';
    if (action === 'audit') state.auditing = true;
    await route.fulfill({
      json: { ...control, mode: state.mode, paused: state.mode === 'paused' }
    });
  });
  await page.clock.install();
  await login(page);
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
  return state;
}

async function pendingPoll(page: Page, state: Awaited<ReturnType<typeof controlFixture>>) {
  const oldRead = deferred();
  state.holds.push(oldRead);
  const before = state.reads;
  await page.clock.runFor(4000);
  await expect.poll(() => state.reads).toBe(before + 1);
  return oldRead;
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

for (const action of ['cycle', 'audit'] as const) {
  for (const failure of [false, true]) {
    test(`${action === 'audit' ? 'an audit' : 'a cycle'} waits for its queued state refresh${failure ? ' and recovers from read failure' : ''}`, async ({
      page
    }) => {
      const state = await controlFixture(page);
      const oldRead = await pendingPoll(page, state);
      const newRead = deferred();
      state.holds.push(newRead);
      state.outage = failure;
      const run = page.locator('#run-once-control');
      const audit = page.locator('#run-audit-control');
      const button = action === 'cycle' ? run : audit;
      try {
        await button.click();
        await expect.poll(() => state.writes).toEqual([action]);
        await expect(button).toHaveText(action === 'cycle' ? 'Run once' : 'Starting audit…');
        await expect(run).toBeDisabled();
        await expect(audit).toBeDisabled();
        await run.dispatchEvent('click');
        await audit.dispatchEvent('click');
        expect(state.writes).toEqual([action]);
        if (action === 'cycle')
          await expect(page.getByRole('button', { name: 'Pause', exact: true })).toBeVisible();

        const before = state.reads;
        oldRead.resolve();
        await expect.poll(() => state.reads).toBe(before + 1);
        await expect(button).toHaveText(action === 'cycle' ? 'Run once' : 'Starting audit…');
        await expect(run).toBeDisabled();
        await expect(audit).toBeDisabled();
        if (action === 'cycle')
          await expect(page.getByRole('button', { name: 'Pause', exact: true })).toBeVisible();
        await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();

        const refreshed = page.waitForResponse('**/api/state');
        newRead.resolve();
        await (await refreshed).finished();
        await page.clock.runFor(100);
        await expect(button).toHaveText(action === 'cycle' ? 'Run once' : 'Run an audit');
        await expect(run).toBeDisabled();
        await expect(audit).toBeDisabled();
        if (action === 'audit')
          await expect(
            page.getByRole('heading', { name: 'Worth doing. Before doing.' })
          ).toBeVisible();
        if (failure) {
          await expect(page.getByRole('alert')).toContainText('Synthetic state refresh outage');
          await run.dispatchEvent('click');
          await audit.dispatchEvent('click');
          expect(state.writes).toEqual([action]);
          state.outage = false;
          state.mode = 'paused';
          state.auditing = false;
          await page.clock.runFor(4000);
          await expect(page.getByRole('alert')).toHaveCount(0);
          await expect(run).toBeEnabled();
          await expect(audit).toBeEnabled();
        }
        expect(state.writes).toEqual([action]);
      } finally {
        oldRead.resolve();
        newRead.resolve();
      }
    });
  }
}
