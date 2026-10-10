import { expect, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import type { Snapshot, Task } from '../src/lib/types';
import { deferred, login, nextPoll, openNavigation, patchState, test } from './synthetic';

/** A paused, idle, configured service whose state the test moves between polls. */
function idle(snapshot: Snapshot) {
  snapshot.configured = true;
  snapshot.audit_configured = true;
  snapshot.control.paused = true;
  snapshot.control.mode = 'paused';
  snapshot.active_tasks = 0;
  snapshot.cycle_active = false;
  snapshot.active_cycle_mode = null;
  snapshot.baseline_active = false;
}

async function taskActivityFixture(page: Page, holdInitialEvents: boolean) {
  const taskGate = deferred();
  const eventsGate = deferred();
  const state = {
    task: null as Task | null,
    holdTask: false,
    holdEvents: holdInitialEvents,
    failEvents: false,
    heldTaskReads: 0,
    heldEventsReads: 0,
    completedEventsReads: 0,
    cancelWrites: 0
  };
  await page.route('**/api/tasks/task-active', async (route) => {
    expect(route.request().method()).toBe('GET');
    if (!state.task) {
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      state.task = (await response.json()) as Task;
      expect(state.task.status).toBe('queued');
      expect(state.task.allowed_actions).toEqual(['cancel']);
    }
    const task = structuredClone(state.task);
    if (state.holdTask) {
      state.heldTaskReads++;
      await taskGate.promise;
    }
    await route.fulfill({ json: task });
  });
  await page.route('**/api/events?entity=task-active', async (route) => {
    expect(route.request().method()).toBe('GET');
    if (state.failEvents) {
      await route.fulfill({ status: 503, json: { error: 'Synthetic activity read outage' } });
      return;
    }
    const response = await route.fetch();
    expect(response.status()).toBe(200);
    expect(Array.isArray(await response.json())).toBe(true);
    if (state.holdEvents) {
      state.heldEventsReads++;
      await eventsGate.promise;
    }
    await route.fulfill({ response });
    state.completedEventsReads++;
  });
  await page.route('**/api/tasks/task-active/cancel', async (route) => {
    expect(route.request().method()).toBe('POST');
    expect(state.task?.allowed_actions).toEqual(['cancel']);
    state.cancelWrites++;
    // Keep parallel tests' shared service records unchanged.
    state.task = {
      ...state.task!,
      status: 'cancelled',
      allowed_actions: ['archive', 'supersede'],
      updated_at: new Date().toISOString()
    };
    await route.fulfill({ json: { ok: true } });
  });
  return { state, taskGate, eventsGate };
}

test('an initial queued task exposes Cancel while valid activity is delayed', async ({
  page,
  isMobile
}) => {
  const current = await taskActivityFixture(page, true);
  try {
    await login(page);
    await openNavigation(page, 'Task queue', !!isMobile);
    await page.getByRole('button', { name: /Complete the repository setup flow/ }).click();
    await expect.poll(() => current.state.heldEventsReads).toBeGreaterThan(0);
    await expect(
      page.getByRole('dialog').getByRole('button', { name: 'Cancel task', exact: true })
    ).toBeEnabled();
    expect(current.state.completedEventsReads).toBe(0);
    expect(current.state.cancelWrites).toBe(0);
  } finally {
    current.taskGate.resolve();
    current.eventsGate.resolve();
  }
});

test('task controls stay available and activity recovers after an activity read failure', async ({
  page,
  isMobile
}) => {
  const current = await taskActivityFixture(page, false);
  current.state.failEvents = true;
  await page.clock.install();
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  await page.getByRole('button', { name: /Complete the repository setup flow/ }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog.getByRole('button', { name: 'Cancel task', exact: true })).toBeEnabled();
  await dialog.getByRole('tab', { name: 'Activity', exact: true }).click();
  await expect(dialog.getByRole('alert')).toContainText('Synthetic activity read outage');
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
  current.state.failEvents = false;
  await page.clock.runFor(4000);
  await expect(dialog.getByRole('alert')).toHaveCount(0);
  await expect.poll(() => current.state.completedEventsReads).toBe(1);
  await expect(dialog.getByRole('button', { name: 'Cancel task', exact: true })).toBeEnabled();
  expect(current.state.cancelWrites).toBe(0);
});

test('accepted cancellation waits for its task but not delayed activity to replace controls', async ({
  page,
  isMobile
}) => {
  const current = await taskActivityFixture(page, false);
  try {
    await page.clock.install();
    await login(page);
    await openNavigation(page, 'Task queue', !!isMobile);
    await page.getByRole('button', { name: /Complete the repository setup flow/ }).click();
    const dialog = page.getByRole('dialog');
    const cancel = dialog.getByRole('button', { name: 'Cancel task', exact: true });
    await expect(cancel).toBeEnabled();
    await expect.poll(() => current.state.completedEventsReads).toBe(1);
    await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
    current.state.holdTask = true;
    current.state.holdEvents = true;
    await cancel.click();
    await expect.poll(() => current.state.heldTaskReads).toBe(1);
    await expect.poll(() => current.state.heldEventsReads).toBe(1);
    await expect(cancel).toBeDisabled();
    await cancel.dispatchEvent('click');
    await expect(dialog.getByRole('button', { name: 'Archive task', exact: true })).toHaveCount(0);
    expect(current.state.cancelWrites).toBe(1);
    current.taskGate.resolve();
    await expect(dialog.getByRole('button', { name: 'Archive task', exact: true })).toBeEnabled();
    await expect(cancel).toHaveCount(0);
    expect(current.state.completedEventsReads).toBe(1);
    expect(current.state.cancelWrites).toBe(1);
  } finally {
    current.taskGate.resolve();
    current.eventsGate.resolve();
  }
});

for (const capacity of ['daily_exhausted', 'limit_too_low'] as const) {
  test(`planning controls respect ${capacity} and recover with refreshed capacity`, async ({
    page
  }) => {
    let status: Snapshot['planning_capacity']['status'] = capacity;
    let queued = false;
    const writes: string[] = [];
    await patchState(page, (snapshot) => {
      idle(snapshot);
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

async function controlFixture(page: Page) {
  const state = {
    mode: 'paused' as Snapshot['control']['mode'],
    auditing: false,
    outage: false,
    reads: 0,
    completedReads: 0,
    writes: [] as string[],
    holds: [] as ReturnType<typeof deferred>[]
  };
  let control: Snapshot['control'];
  await page.route('**/api/state', async (route) => {
    // Capture the state when this read starts, before another request can mutate it.
    const { mode, auditing, outage } = state;
    const hold = state.holds.shift();
    state.reads++;
    const snapshot: Snapshot = await (await route.fetch()).json();
    idle(snapshot);
    snapshot.control.mode = mode;
    snapshot.control.paused = mode === 'paused';
    snapshot.cycle_active = auditing;
    snapshot.active_cycle_mode = auditing ? 'audit' : null;
    snapshot.planning_capacity.status = 'ready';
    control = snapshot.control;
    if (hold) await hold.promise;
    if (outage)
      await route.fulfill({ status: 503, json: { error: 'Synthetic state refresh outage' } });
    else await route.fulfill({ json: snapshot });
    state.completedReads++;
  });
  await page.route('**/api/control/*', async (route) => {
    const action = new URL(route.request().url()).pathname.split('/').at(-1)!;
    state.writes.push(action);
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

for (const action of ['cycle', 'audit'] as const) {
  test(`${action === 'audit' ? 'an audit' : 'a cycle'} waits for its queued state refresh and recovers from a read failure`, async ({
    page
  }) => {
    const state = await controlFixture(page);
    // A poll that started before the control is answered only after it; its stale state must not be shown.
    const oldRead = deferred();
    state.holds.push(oldRead);
    const polls = state.reads;
    await page.clock.runFor(4000);
    await expect.poll(() => state.reads).toBe(polls + 1);
    const newRead = deferred();
    state.holds.push(newRead);
    state.outage = true;
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
      expect(state.writes).toEqual([action]);
    } finally {
      oldRead.resolve();
      newRead.resolve();
    }
  });
}

test('controls share eligibility, refuse duplicate pending actions, and an audit records decisions without queuing work @responsive', async ({
  page
}) => {
  let restriction = 'continuous';
  let configured = true;
  let audit: 'none' | 'running' | 'finished' = 'none';
  const writes: string[] = [];
  const gate = deferred();
  await patchState(page, (snapshot) => {
    snapshot.configured = configured;
    snapshot.audit_configured = true;
    snapshot.status = audit === 'running' ? 'auditing' : restriction;
    snapshot.control.paused = restriction !== 'continuous';
    snapshot.active_tasks = restriction === 'task' ? 1 : 0;
    snapshot.cycle_active = ['execution', 'audit'].includes(restriction) || audit === 'running';
    snapshot.active_cycle_mode =
      restriction === 'audit' || audit === 'running'
        ? 'audit'
        : restriction === 'execution'
          ? 'execution'
          : null;
    snapshot.tasks = [];
    snapshot.attention_tasks = [];
    snapshot.counts = {};
    if (audit !== 'none') {
      const cycle = structuredClone(snapshot.cycles[0]);
      cycle.id = 'audit-fixture';
      cycle.number = 2;
      cycle.mode = 'audit';
      cycle.status = audit === 'running' ? 'running' : 'completed';
      cycle.decisions = { accepted: 1, rejected: 1, deferred: 1 };
      snapshot.cycles.unshift(cycle);
    }
  });
  await page.route('**/api/cycles?*', async (route) => {
    const result = await (await route.fetch()).json();
    if (audit !== 'none')
      result.items.unshift({
        ...result.items[0],
        id: 'audit-fixture',
        number: 2,
        mode: 'audit',
        status: audit === 'running' ? 'running' : 'completed'
      });
    await route.fulfill({ json: result });
  });
  await page.route('**/api/proposals?*', async (route) => {
    const result = await (await route.fetch()).json();
    if (audit !== 'none') {
      // The service has no rows for the synthetic audit cycle, so the recommendations need a shape of their own.
      const seed = result.items[0] ?? {
        target: 'main',
        tier: 'M',
        category: 'features',
        problem: 'Concrete evidence',
        scope: 'Small scope',
        benefit: 'Useful',
        evidence: [],
        dependencies: [],
        prompt: ''
      };
      const status = new URL(route.request().url()).searchParams.get('status');
      result.items =
        audit === 'finished'
          ? ['accepted', 'rejected', 'deferred']
              .filter((d) => status === 'all' || d === status)
              .map((decision, index) => ({
                ...seed,
                id: `audit-${index}`,
                cycle_id: 'audit-fixture',
                cycle: 2,
                mode: 'audit',
                title: `Audit ${decision} recommendation`,
                decision,
                reason: `${decision}: both adversaries considered the concrete evidence.`
              }))
          : [];
      result.counts = { accepted: 1, rejected: 1, deferred: 1 };
    }
    await route.fulfill({ json: result });
  });
  await page.route('**/api/control/*', async (route) => {
    const action = new URL(route.request().url()).pathname;
    writes.push(action);
    await gate.promise;
    if (action === '/api/control/audit') {
      audit = 'running';
      await route.fulfill({ json: { paused: true } });
      return;
    }
    restriction = 'continuous';
    await route.fulfill({ json: { paused: false } });
  });
  await page.clock.install();
  await login(page);
  const run = page.getByRole('button', { name: 'Run once', exact: true });
  const runAudit = page.getByRole('button', { name: 'Run an audit', exact: true });
  const discover = page.getByRole('button', { name: 'Discover opportunities', exact: true });
  for (const value of ['continuous', 'task', 'execution', 'audit', 'idle']) {
    restriction = value;
    await nextPoll(page);
    if (value === 'idle') {
      await expect(run).toBeEnabled({ timeout: 10000 });
      await expect(discover).toBeEnabled();
      await expect(runAudit).toBeEnabled();
    } else {
      await expect(page.locator('.status-value')).toHaveText(value, { timeout: 10000 });
      await expect(run).toBeDisabled();
      if (value === 'task') await expect(discover).toHaveCount(0);
      else await expect(discover).toBeDisabled();
      await expect(runAudit).toBeDisabled();
    }
  }
  expect(writes).toEqual([]);
  await discover.click();
  const starting = page.getByRole('button', { name: 'Starting run…', exact: true });
  await expect(starting).toHaveCount(2);
  for (const button of await starting.all()) await expect(button).toBeDisabled();
  await starting.first().dispatchEvent('click');
  gate.resolve();
  await expect(run).toBeDisabled();
  expect(writes).toEqual(['/api/control/cycle']);

  // Only the audit routes are saved: the audit is offered, a run is not.
  configured = false;
  restriction = 'idle';
  await nextPoll(page);
  await expect(runAudit).toBeEnabled({ timeout: 10000 });
  await expect(run).toBeDisabled();
  await runAudit.click();
  await expect(page.getByRole('heading', { name: 'Worth doing. Before doing.' })).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'Audit in progress' })).toContainText(
    'Audit in progress'
  );
  await expect(page.getByRole('button', { name: 'Start continuous', exact: true })).toBeDisabled();
  await expect(runAudit).toBeDisabled();
  audit = 'finished';
  await nextPoll(page);
  await expect(page.getByRole('heading', { name: 'Audit rejected recommendation' })).toBeVisible({
    timeout: 10000
  });
  await page.getByLabel('Cycle', { exact: true }).selectOption('audit-fixture');
  await expect(page.getByLabel('Decision counts')).toContainText('rejected: 1');
  await page.getByRole('button', { name: 'rejected', exact: true }).click();
  await expect(
    page.getByText('rejected: both adversaries considered the concrete evidence.')
  ).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Audit accepted recommendation' })).toHaveCount(0);
  expect(writes).toEqual(['/api/control/cycle', '/api/control/audit']);
  const accessibility = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze();
  expect(accessibility.violations).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

// A planning preflight has no cycle yet: the service reports it only through active_cycle_mode and refuses
// Run once, audits and configuration changes until it ends.
for (const mode of ['execution', 'audit'] as const) {
  test(`a paused ${mode} planning preflight keeps planning controls and configuration locked`, async ({
    page,
    isMobile
  }) => {
    await patchState(page, (snapshot) => {
      idle(snapshot);
      snapshot.status = mode === 'audit' ? 'auditing' : 'paused';
      snapshot.active_cycle_mode = mode;
      snapshot.tasks = [];
      snapshot.attention_tasks = [];
      snapshot.counts = {};
      snapshot.planning_capacity.status = 'ready';
    });
    await login(page);
    await expect(page.locator('.status-value')).toHaveText(
      mode === 'audit' ? 'auditing' : 'paused'
    );
    for (const name of ['Run once', 'Run an audit', 'Discover opportunities'])
      await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    await openNavigation(page, 'Configuration', !!isMobile);
    await expect(
      page.getByText('Pause the service and wait for active work to finish to edit configuration.')
    ).toBeVisible();
    await expect(page.getByLabel('Default branch', { exact: true })).toBeDisabled();
    await expect(page.locator('[data-step="choose"]')).toContainText(
      `Unavailable now: ${mode === 'audit' ? 'An audit is in progress.' : 'A cycle is planning.'}`
    );
  });
}

// A paused, idle service whose automatic merge check is still running must not offer Audit, Run once
// or configuration editing: runtime.idle requires mergeWorker=nil. Clearing the field restores them.
test('an active automatic merge check locks planning controls and configuration, then recovers', async ({
  page,
  isMobile
}) => {
  let merging = true;
  await patchState(page, (snapshot) => {
    idle(snapshot);
    snapshot.status = 'paused';
    snapshot.auto_merge = { active: merging, counts: {} };
    snapshot.tasks = [];
    snapshot.attention_tasks = [];
    snapshot.counts = {};
    snapshot.planning_capacity.status = 'ready';
  });
  await page.clock.install();
  await login(page);
  const run = page.getByRole('button', { name: 'Run once', exact: true });
  const runAudit = page.getByRole('button', { name: 'Run an audit', exact: true });
  await expect(page.locator('.status-value')).toHaveText('paused');
  await expect(run).toBeDisabled();
  await expect(runAudit).toBeDisabled();

  await openNavigation(page, 'Configuration', !!isMobile);
  const branch = page.getByLabel('Default branch', { exact: true });
  const lockNotice = page.getByText(
    'An automatic merge check is still running. Wait for it to finish before editing configuration.',
    { exact: true }
  );
  await expect(lockNotice).toBeVisible();
  await expect(branch).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Save configuration' })).toBeDisabled();
  await expect(page.locator('[data-step="choose"]')).toContainText(
    'Unavailable now: An automatic merge check is still running.'
  );

  merging = false;
  await nextPoll(page);
  await expect(branch).toBeEnabled({ timeout: 10000 });
  await expect(lockNotice).toHaveCount(0);
  await expect(page.locator('[data-step="choose"]')).toContainText('Run once: available');
  await openNavigation(page, 'Overview', !!isMobile);
  await expect(run).toBeEnabled();
  await expect(runAudit).toBeEnabled();
});

test('slow proposal history cannot block state polling, controls, or navigation', async ({
  page
}) => {
  const state = await controlFixture(page);
  const history = deferred();
  let reads = 0;
  let canceled = 0;
  page.on('requestfailed', (request) => {
    if (new URL(request.url()).pathname === '/api/cycles') canceled++;
  });
  await page.route('**/api/cycles?*', async (route) => {
    reads++;
    await history.promise;
    await route.fulfill({ json: { items: [], next_cursor: null, counts: {} } });
  });
  try {
    await openNavigation(page, 'Proposals', false);
    await expect.poll(() => reads).toBe(1);
    for (let i = 0; i < 3; i++) {
      const before = state.completedReads;
      // Count only responses the page received, from inside the route handler:
      // a poll issued by an earlier advancement can land its response inside
      // this wait, and a tick that races an in-flight refresh is correctly
      // skipped while that response is applied, so keep advancing until a
      // completion lands.
      await expect
        .poll(async () => {
          await page.clock.runFor(4000);
          return state.completedReads;
        })
        .toBeGreaterThan(before);
    }
    expect(reads).toBe(1);
    expect(canceled).toBe(0);
    await openNavigation(page, 'Overview', false);
    await expect.poll(() => canceled).toBe(1);
    const after = state.reads;
    await page.locator('#run-audit-control').click();
    await expect.poll(() => state.writes).toEqual(['audit']);
    await expect.poll(() => state.reads).toBeGreaterThan(after);
    await expect(page.locator('#run-audit-control')).toHaveText('Run an audit');
  } finally {
    history.resolve();
  }
});
