import { expect, type Page } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';
import { login, openNavigation, test, token } from './synthetic';

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

async function watchConfigurationWrites(page: Page) {
  const writes: string[] = [];
  await page.route('**/api/config', async (route) => {
    if (route.request().method() !== 'PUT') return route.continue();
    writes.push(route.request().method());
    await route.fulfill({ status: 409, json: { error: 'Synthetic active work conflict' } });
  });
  return writes;
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

  test(`a refused ${action} remains retryable`, async ({ page }) => {
    const state = await controlFixture(page);
    state.refusal = true;
    const button = page.locator(action === 'cycle' ? '#run-once-control' : '#run-audit-control');
    await button.click();
    await expect(page.getByRole('alert')).toHaveText('Synthetic control refusal');
    await expect(button).toBeEnabled();
    state.refusal = false;
    await button.click();
    await expect(button).toBeDisabled();
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(state.writes).toEqual([action, action]);
  });
}

test('a successful resume with failed state refresh still offers Pause', async ({ page }) => {
  const state = await controlFixture(page);
  const oldRead = await pendingPoll(page, state);
  state.outage = true;
  try {
    await page.getByRole('button', { name: 'Start continuous', exact: true }).click();
    await expect.poll(() => state.writes).toEqual(['resume']);
    oldRead.resolve();
    await expect(page.getByRole('alert')).toContainText('Synthetic state refresh outage');
    const pause = page.getByRole('button', { name: 'Pause', exact: true });
    await expect(pause).toBeEnabled();
    await pause.click();
    const resume = page.getByRole('button', { name: 'Start continuous', exact: true });
    await expect(resume).toBeDisabled();
    await expect(page.locator('#run-once-control')).toBeDisabled();
    await expect(page.locator('#run-audit-control')).toBeDisabled();
    await expect(page.getByRole('alert')).toContainText('Synthetic state refresh outage');
    expect(state.writes).toEqual(['resume', 'pause']);
    state.outage = false;
    await page.clock.runFor(4000);
    await expect(resume).toBeEnabled();
    await expect(page.locator('#run-once-control')).toBeEnabled();
    await expect(page.locator('#run-audit-control')).toBeEnabled();
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(state.writes).toEqual(['resume', 'pause']);
  } finally {
    oldRead.resolve();
  }
});

test('a queued audit refresh preserves newer navigation and focus', async ({ page, isMobile }) => {
  const state = await controlFixture(page);
  const oldRead = await pendingPoll(page, state);
  const newRead = deferred();
  state.holds.push(newRead);
  try {
    await page.locator('#run-audit-control').click();
    await expect.poll(() => state.writes).toEqual(['audit']);
    await openNavigation(page, 'Task queue', !!isMobile);
    const search = page.getByLabel('Search work');
    await search.fill('operator-query');
    const before = state.reads;
    oldRead.resolve();
    await expect.poll(() => state.reads).toBe(before + 1);
    const refreshed = page.waitForResponse('**/api/state');
    newRead.resolve();
    await (await refreshed).finished();
    await page.clock.runFor(100);
    await expect(page.getByRole('heading', { name: 'From idea to improvement.' })).toBeVisible();
    await expect(search).toHaveValue('operator-query');
    await expect(search).toBeFocused();
    expect(state.writes).toEqual(['audit']);
  } finally {
    oldRead.resolve();
    newRead.resolve();
  }
});

test('disconnecting a queued audit refresh leaves the next login independent', async ({
  page,
  isMobile
}) => {
  const state = await controlFixture(page);
  const oldRead = await pendingPoll(page, state);
  try {
    await page.locator('#run-audit-control').click();
    await expect.poll(() => state.writes).toEqual(['audit']);
    if (isMobile) await page.getByRole('button', { name: 'Toggle navigation' }).click();
    await page.getByRole('button', { name: /Disconnect/ }).click();
    state.auditing = false;
    await page.getByLabel('Operator access token').fill(token);
    await page.getByRole('button', { name: 'Open dashboard' }).click();
    await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
    oldRead.resolve();
    await page.clock.runFor(100);
    await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
    await expect(page.locator('#run-once-control')).toBeEnabled();
    await expect(page.locator('#run-audit-control')).toBeEnabled();
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(state.writes).toEqual(['audit']);
  } finally {
    oldRead.resolve();
  }
});

test('Configuration describes unknown activity after an accepted audit and a failed state refresh', async ({
  page,
  isMobile
}) => {
  const state = await controlFixture(page);
  const configurationWrites = await watchConfigurationWrites(page);
  state.outage = true;
  await page.locator('#run-audit-control').click();
  await expect(page.getByRole('alert')).toContainText('Synthetic state refresh outage');
  await openNavigation(page, 'Configuration', !!isMobile);
  const choose = page.locator('[data-step="choose"]');
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });
  await expect(commands).toBeEnabled();
  await commands.fill('go test ./...');
  const save = page.getByRole('button', { name: 'Save configuration' });
  await expect(save).toBeDisabled();
  await page.locator('form').filter({ has: save }).dispatchEvent('submit');
  await page.clock.runFor(100);
  expect(configurationWrites).toEqual([]);
  await expect(choose).toContainText('Audit: unavailable. Run once: unavailable.');
  await expect(choose).toContainText(
    'Control accepted. Current activity is unknown until the service state refreshes.'
  );
  await expect(choose).not.toContainText('An audit is in progress.');
  await expect(choose).not.toContainText('queued task');

  state.outage = false;
  await page.clock.runFor(4000);
  await expect(choose).toContainText('Unavailable now: An audit is in progress.');
  await expect(commands).toBeDisabled();
  await expect(choose).not.toContainText('Current activity is unknown');
  state.auditing = false;
  await page.clock.runFor(4000);
  await expect(choose).toContainText('Audit: available. Run once: available');
  await expect(commands).toBeEnabled();
  expect(state.writes).toEqual(['audit']);
});

test('Configuration retains draft focus but blocks saving until an accepted audit is confirmed idle', async ({
  page,
  isMobile
}) => {
  const state = await controlFixture(page);
  const configurationWrites = await watchConfigurationWrites(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });
  const draft = `${await commands.inputValue()}\ngo test ./...`;
  await commands.fill(draft);
  const save = page.getByRole('button', { name: 'Save configuration' });
  await expect(save).toBeEnabled();
  await openNavigation(page, 'Overview', !!isMobile);
  const oldRead = await pendingPoll(page, state);
  const newRead = deferred();
  state.holds.push(newRead);
  try {
    await page.locator('#run-audit-control').click();
    await expect.poll(() => state.writes).toEqual(['audit']);
    await openNavigation(page, 'Configuration', !!isMobile);
    await expect(commands).toBeEnabled();
    await commands.focus();
    await expect(save).toBeDisabled();
    await page.locator('form').filter({ has: save }).dispatchEvent('submit');
    await page.clock.runFor(100);
    expect(configurationWrites).toEqual([]);
    await expect(commands).toBeFocused();
    const before = state.reads;
    oldRead.resolve();
    await expect.poll(() => state.reads).toBe(before + 1);
    await expect(commands).toBeEnabled();
    await expect(commands).toBeFocused();
    const refreshed = page.waitForResponse('**/api/state');
    newRead.resolve();
    await (await refreshed).finished();
    await expect(commands).toBeDisabled();
    await expect(save).toBeDisabled();
    state.auditing = false;
    await page.clock.runFor(4000);
    await expect(commands).toBeEnabled();
    await expect(commands).toHaveValue(draft);
    await expect(save).toBeEnabled();
    expect(state.writes).toEqual(['audit']);
  } finally {
    oldRead.resolve();
    newRead.resolve();
  }
});

test('Pause remains responsive during a resume refresh and its pending write survives older cleanup', async ({
  page
}) => {
  const state = await controlFixture(page);
  const oldRead = await pendingPoll(page, state);
  const newRead = deferred();
  const pauseWrite = deferred();
  state.holds.push(newRead);
  try {
    await page.getByRole('button', { name: 'Start continuous', exact: true }).click();
    const pause = page.getByRole('button', { name: 'Pause', exact: true });
    await expect(pause).toBeEnabled();
    const before = state.reads;
    oldRead.resolve();
    await expect.poll(() => state.reads).toBe(before + 1);
    await expect(pause).toBeEnabled();
    state.writeHold = pauseWrite;
    await pause.click();
    const pausing = page.getByRole('button', { name: 'Pausing…', exact: true });
    await expect(pausing).toBeDisabled();
    const refreshed = page.waitForResponse('**/api/state');
    newRead.resolve();
    await (await refreshed).finished();
    await page.clock.runFor(100);
    await expect(pausing).toBeDisabled();
    await pausing.dispatchEvent('click');
    expect(state.writes).toEqual(['resume', 'pause']);
    pauseWrite.resolve();
    await expect(page.getByRole('button', { name: 'Start continuous', exact: true })).toBeEnabled();
    await expect(page.locator('#run-once-control')).toBeEnabled();
    await expect(page.locator('#run-audit-control')).toBeEnabled();
    expect(state.writes).toEqual(['resume', 'pause']);
  } finally {
    oldRead.resolve();
    newRead.resolve();
    pauseWrite.resolve();
  }
});
