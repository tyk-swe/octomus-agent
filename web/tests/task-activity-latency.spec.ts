import { expect, type Page, type TestInfo } from '@playwright/test';
import type { Task } from '../src/lib/types';
import { login, openNavigation, test } from './synthetic';

const taskPath = '**/api/tasks/task-active';
const eventsPath = '**/api/events?entity=task-active';
const title = 'Complete the repository setup flow';

function deferred() {
  let release!: () => void;
  const promise = new Promise<void>((done) => (release = done));
  return {
    promise,
    released: false,
    resolve() {
      this.released = true;
      release();
    }
  };
}

async function fixture(page: Page, holdInitialEvents: boolean) {
  const taskGate = deferred();
  const eventsGate = deferred();
  const state = {
    task: null as Task | null,
    holdTask: false,
    holdEvents: holdInitialEvents,
    taskResponses: [] as { status: Task['status']; allowed_actions: string[] }[],
    heldTaskReads: 0,
    heldEventsReads: 0,
    validEventsReads: 0,
    completedEventsReads: 0,
    cancelWrites: 0
  };
  await page.route(taskPath, async (route) => {
    const holdTask = state.holdTask;
    expect(route.request().method()).toBe('GET');
    if (!state.task) {
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      state.task = (await response.json()) as Task;
      expect(state.task.id).toBe('task-active');
      expect(state.task.status).toBe('queued');
      expect(state.task.allowed_actions).toEqual(['cancel']);
    }
    const task = structuredClone(state.task);
    if (holdTask) {
      state.heldTaskReads++;
      await taskGate.promise;
    }
    await route.fulfill({ status: 200, json: task });
    state.taskResponses.push({ status: task.status, allowed_actions: task.allowed_actions });
  });
  await page.route(eventsPath, async (route) => {
    const holdEvents = state.holdEvents;
    expect(route.request().method()).toBe('GET');
    const response = await route.fetch();
    expect(response.status()).toBe(200);
    expect(Array.isArray(await response.json())).toBe(true);
    state.validEventsReads++;
    if (holdEvents) {
      state.heldEventsReads++;
      await eventsGate.promise;
    }
    await route.fulfill({ response });
    state.completedEventsReads++;
  });
  await page.route('**/api/tasks/task-active/cancel', async (route) => {
    expect(route.request().method()).toBe('POST');
    state.cancelWrites++;
    expect(state.task?.allowed_actions).toEqual(['cancel']);
    // A per-page server projection keeps parallel tests' shared task unchanged.
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

async function openTask(page: Page, mobile: boolean) {
  await openNavigation(page, 'Task queue', mobile);
  await page.getByRole('button', { name: new RegExp(title) }).click();
}

async function capture(
  page: Page,
  info: TestInfo,
  name: string,
  fixtureState: Awaited<ReturnType<typeof fixture>>
) {
  const { state, taskGate, eventsGate } = fixtureState;
  await info.attach(`${name}-responses`, {
    contentType: 'application/json',
    body: JSON.stringify({
      ...state,
      task: state.task && {
        id: state.task.id,
        status: state.task.status,
        allowed_actions: state.task.allowed_actions
      },
      taskGateReleased: taskGate.released,
      eventsGateReleased: eventsGate.released
    })
  });
  if (!page.isClosed()) {
    const path = info.outputPath(`${name}.png`);
    await page.screenshot({ path });
    await info.attach(name, { path, contentType: 'image/png' });
  }
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('an initial queued task exposes its canonical Cancel while valid activity is delayed', async ({
  page,
  isMobile
}, info) => {
  const current = await fixture(page, true);
  const taskResponse = page.waitForResponse(taskPath);
  try {
    await login(page);
    await openTask(page, !!isMobile);
    const response = await taskResponse;
    expect(response.status()).toBe(200);
    await response.finished();
    expect(await response.json()).toMatchObject({
      id: 'task-active',
      status: 'queued',
      allowed_actions: ['cancel']
    });
    await expect.poll(() => current.state.taskResponses.length).toBeGreaterThan(0);
    await expect.poll(() => current.state.heldEventsReads).toBeGreaterThan(0);
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('button', { name: 'Cancel task', exact: true })).toBeEnabled();
    await expect(dialog.getByRole('heading', { name: title, exact: true })).toBeVisible();
    expect(current.eventsGate.released).toBe(false);
    expect(current.state.cancelWrites).toBe(0);
  } finally {
    try {
      await capture(page, info, 'initial-held-activity', current);
    } finally {
      current.taskGate.resolve();
      current.eventsGate.resolve();
    }
  }
});

test('accepted cancellation waits for its canonical task but not delayed activity to replace controls', async ({
  page,
  isMobile
}, info) => {
  const current = await fixture(page, false);
  try {
    await page.clock.install();
    await login(page);
    await openTask(page, !!isMobile);
    const dialog = page.getByRole('dialog');
    const cancel = dialog.getByRole('button', { name: 'Cancel task', exact: true });
    await expect(cancel).toBeEnabled();
    await expect.poll(() => current.state.completedEventsReads).toBe(1);
    await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
    current.state.holdTask = true;
    current.state.holdEvents = true;
    const accepted = page.waitForResponse('**/api/tasks/task-active/cancel');
    const taskResponse = page.waitForResponse(taskPath);
    await cancel.click();
    expect((await accepted).status()).toBe(200);
    await expect.poll(() => current.state.heldTaskReads).toBe(1);
    await expect.poll(() => current.state.heldEventsReads).toBe(1);
    // Acknowledgment alone cannot authorize controls from the pre-action task.
    await expect(cancel).toBeDisabled();
    await cancel.dispatchEvent('click');
    await expect(dialog.getByRole('button', { name: 'Archive task', exact: true })).toHaveCount(0);
    expect(current.state.cancelWrites).toBe(1);
    current.taskGate.resolve();
    const response = await taskResponse;
    expect(response.status()).toBe(200);
    await response.finished();
    expect(await response.json()).toMatchObject({
      id: 'task-active',
      status: 'cancelled',
      allowed_actions: ['archive', 'supersede']
    });
    await expect(dialog.getByRole('button', { name: 'Archive task', exact: true })).toBeEnabled();
    await expect(cancel).toHaveCount(0);
    expect(current.eventsGate.released).toBe(false);
    expect(current.state.cancelWrites).toBe(1);
  } finally {
    try {
      await capture(page, info, 'cancel-held-activity', current);
    } finally {
      current.taskGate.resolve();
      current.eventsGate.resolve();
    }
  }
});
