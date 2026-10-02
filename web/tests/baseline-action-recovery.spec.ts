import { expect, type Page } from '@playwright/test';
import type { BaselineCheck, BaselineView, SettingsView, Snapshot } from '../src/lib/types';
import { login, openNavigation, test, trackWrites } from './synthetic';

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

async function fixture(page: Page) {
  const state = {
    settings: null as SettingsView | null,
    view: {
      check: null,
      eligible: true,
      reason: null,
      config_matches: null,
      config_revision: null,
      revision_status: 'unknown',
      default_observation: null,
      caveat: 'Synthetic baseline caveat'
    } as BaselineView,
    reads: 0,
    stateReads: 0,
    baselineActive: false,
    starts: 0,
    cancels: 0,
    failRead: false,
    reject: '',
    postGate: null as ReturnType<typeof deferred> | null,
    readGate: null as ReturnType<typeof deferred> | null
  };
  await page.route('**/api/state', async (route) => {
    state.stateReads++;
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.control.paused = true;
    snapshot.control.mode = 'paused';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = false;
    snapshot.baseline_active = state.baselineActive;
    await route.fulfill({ json: snapshot });
  });
  await page.route('**/api/config', async (route) => {
    if (!state.settings) {
      const response = await route.fetch();
      state.settings = await response.json();
      state.settings!.revision = 'a'.repeat(64);
    }
    await route.fulfill({ json: state.settings });
  });
  await page.route('**/api/baseline-checks/latest', async (route) => {
    state.reads++;
    const response = state.failRead
      ? { status: 503, json: { error: 'Synthetic baseline read outage' } }
      : { json: structuredClone(state.view) };
    const gate = state.readGate;
    state.readGate = null;
    await gate?.promise;
    await route.fulfill(response);
  });
  await page.route('**/api/baseline-checks', async (route) => {
    state.starts++;
    expect(route.request().postDataJSON()).toEqual({
      expected_revision: state.settings!.revision
    });
    await state.postGate?.promise;
    if (state.reject) {
      await route.fulfill({ status: 409, json: { error: state.reject } });
      return;
    }
    const check: BaselineCheck = {
      id: 'accepted-baseline',
      status: 'running',
      config: structuredClone(state.settings!.config),
      config_fingerprint: state.settings!.revision,
      revision: null,
      started_at: new Date().toISOString(),
      completed_at: null,
      commands: [],
      error: null,
      workspace_removed: false,
      cleanup_error: null
    };
    state.view = {
      ...state.view,
      check,
      eligible: false,
      config_matches: true,
      config_revision: check.config_fingerprint
    };
    await route.fulfill({ status: 202, json: check });
  });
  await page.route('**/api/baseline-checks/*/cancel', async (route) => {
    state.cancels++;
    expect(new URL(route.request().url()).pathname).toBe(
      '/api/baseline-checks/accepted-baseline/cancel'
    );
    await state.postGate?.promise;
    await route.fulfill(
      state.reject ? { status: 409, json: { error: state.reject } } : { json: { ok: true } }
    );
  });
  return state;
}

const panelFor = (page: Page) => page.getByRole('region', { name: 'Clean baseline', exact: true });
async function begin(page: Page) {
  const panel = panelFor(page);
  await panel.getByRole('button', { name: 'Check clean baseline', exact: true }).click();
  const dialog = panel.getByRole('alertdialog', { name: 'Confirm baseline check' });
  await expect(dialog.getByRole('button', { name: 'Run baseline check' })).toBeFocused();
  await dialog.getByRole('button', { name: 'Run baseline check' }).click();
}
async function open(page: Page, mobile: boolean) {
  await page.clock.install();
  await login(page);
  await openNavigation(page, 'Configuration', mobile);
  await expect(panelFor(page).getByRole('button', { name: 'Check clean baseline' })).toBeEnabled();
  // Let initial navigation and its list debounce finish before freezing polling.
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
}
function finish(state: Awaited<ReturnType<typeof fixture>>, status: 'passed' | 'cancelled') {
  state.view = {
    ...state.view,
    eligible: true,
    check: {
      ...state.view.check!,
      status,
      completed_at: new Date().toISOString(),
      workspace_removed: true
    }
  };
}

test('accepted baseline start keeps its running resource and can still be cancelled during a status outage', async ({
  page,
  isMobile
}) => {
  const state = await fixture(page);
  const writes = trackWrites(page);
  await open(page, !!isMobile);
  state.failRead = true;
  await begin(page);
  const panel = panelFor(page);
  await expect(panel.getByRole('alert')).toContainText('Baseline check started.');
  await expect(panel.getByText('Running', { exact: true })).toBeVisible();
  await expect(panel.getByText('No baseline check has been run.', { exact: true })).toHaveCount(0);
  await expect(panel.getByRole('button', { name: 'Check clean baseline' })).toBeDisabled();
  await panel.getByRole('button', { name: 'Cancel baseline check', exact: true }).click();
  await expect(panel.getByRole('alert')).toContainText('Baseline cancellation requested.');
  await expect(panel.getByText('Running', { exact: true })).toBeVisible();
  await expect(panel.getByText('Cancelled', { exact: true })).toHaveCount(0);
  await expect(panel.getByRole('button', { name: 'Cancellation requested' })).toBeDisabled();
  expect(state.starts).toBe(1);
  expect(state.cancels).toBe(1);
  expect(writes.map((write) => write.path)).toEqual([
    '/api/baseline-checks',
    '/api/baseline-checks/accepted-baseline/cancel'
  ]);
});

for (const action of ['start', 'cancel'] as const) {
  test(`acknowledged baseline ${action} recovers with only reads and keeps repeated clicks guarded`, async ({
    page,
    isMobile
  }) => {
    const state = await fixture(page);
    const writes = trackWrites(page);
    await open(page, !!isMobile);
    if (action === 'cancel') await begin(page);
    const panel = panelFor(page);
    if (action === 'cancel')
      await expect(panel.getByRole('button', { name: 'Cancel baseline check' })).toBeEnabled();
    state.failRead = true;
    if (action === 'start') await begin(page);
    else await panel.getByRole('button', { name: 'Cancel baseline check' }).click();
    const message =
      action === 'start' ? 'Baseline check started.' : 'Baseline cancellation requested.';
    await expect(panel.getByRole('alert')).toContainText(message);
    const gate = (state.readGate = deferred());
    const reads = state.reads;
    const retry = panel.getByRole('button', { name: 'Retry baseline status', exact: true });
    await retry.click();
    await expect.poll(() => state.reads).toBe(reads + 1);
    await panel.getByRole('button', { name: 'Retrying baseline status…' }).dispatchEvent('click');
    await page.clock.runFor(4000);
    expect(state.reads).toBe(reads + 1);
    gate.resolve();
    await expect(retry).toBeVisible();
    await expect(panel.getByRole('alert')).toContainText(message);
    state.failRead = false;
    if (action === 'cancel') {
      await retry.click();
      await expect(panel.getByRole('status')).toContainText('Baseline cancellation requested.');
      await expect(panel.getByText('Running', { exact: true })).toBeVisible();
      await page.clock.runFor(4000);
      await expect(panel.getByRole('button', { name: 'Cancellation requested' })).toBeDisabled();
      state.failRead = true;
      await openNavigation(page, 'Overview', !!isMobile);
      await openNavigation(page, 'Configuration', !!isMobile);
      await expect(retry).toBeVisible();
      await expect(panel.getByText('Running', { exact: true })).toBeVisible();
      await expect(panel.getByRole('button', { name: 'Cancellation requested' })).toBeDisabled();
      state.failRead = false;
    }
    finish(state, action === 'start' ? 'passed' : 'cancelled');
    await retry.click();
    await expect(panel.getByRole('heading', { name: 'Clean baseline' })).toBeFocused();
    await expect(panel.getByRole('alert')).toHaveCount(0);
    await expect(panel.getByRole('status')).toHaveCount(0);
    await expect(
      panel.getByText(action === 'start' ? 'Passed' : 'Cancelled', { exact: true })
    ).toBeVisible();
    await expect(panel.getByRole('button', { name: 'Check clean baseline' })).toBeEnabled();
    expect(state.starts).toBe(1);
    expect(state.cancels).toBe(action === 'cancel' ? 1 : 0);
    expect(writes).toHaveLength(action === 'cancel' ? 2 : 1);
    await panel.getByRole('button', { name: 'Check clean baseline' }).click();
    await expect(panel.getByRole('alertdialog')).toBeVisible();
    expect(state.starts).toBe(1);
    await panel.getByRole('button', { name: 'Back', exact: true }).click();
    await expect(panel.getByRole('button', { name: 'Check clean baseline' })).toBeFocused();
  });

  test(`baseline ${action} acknowledgment survives inactive navigation and a new saved configuration`, async ({
    page,
    isMobile
  }) => {
    const state = await fixture(page);
    await open(page, !!isMobile);
    const panel = panelFor(page);
    if (action === 'cancel') {
      await begin(page);
      await expect(panel.getByRole('button', { name: 'Cancel baseline check' })).toBeEnabled();
    }
    const gate = (state.postGate = deferred());
    state.failRead = true;
    if (action === 'start') await begin(page);
    else await panel.getByRole('button', { name: 'Cancel baseline check' }).click();
    await expect.poll(() => (action === 'start' ? state.starts : state.cancels)).toBe(1);
    await openNavigation(page, 'Overview', !!isMobile);
    const reads = state.reads;
    gate.resolve();
    await page.clock.runFor(8000);
    expect(state.reads).toBe(reads);
    await openNavigation(page, 'Configuration', !!isMobile);
    const message =
      action === 'start' ? 'Baseline check started.' : 'Baseline cancellation requested.';
    await expect(panel.getByRole('alert')).toContainText(message);
    await expect(panel.getByText('Running', { exact: true })).toBeVisible();
    finish(state, action === 'start' ? 'passed' : 'cancelled');
    await openNavigation(page, 'Overview', !!isMobile);
    state.settings!.revision = 'b'.repeat(64);
    state.settings!.config.default_branch = 'updated-saved-branch';
    state.view.config_matches = false;
    await openNavigation(page, 'Configuration', !!isMobile);
    await expect(page.getByLabel('Default branch', { exact: true })).toHaveValue(
      'updated-saved-branch'
    );
    await expect(
      panel.getByText('Configuration changed since this check', { exact: true })
    ).toBeVisible();
    await expect(panel.getByRole('button', { name: 'Check clean baseline' })).toBeDisabled();
    state.failRead = false;
    await panel.getByRole('button', { name: 'Retry baseline status' }).click();
    await expect(panel.getByRole('button', { name: 'Check clean baseline' })).toBeEnabled();
    await expect(
      panel.getByText('Configuration changed since this check', { exact: true })
    ).toBeVisible();
    expect(state.starts).toBe(1);
    expect(state.cancels).toBe(action === 'cancel' ? 1 : 0);
  });
}

test('a read that started before the baseline mutation cannot overwrite its accepted resource', async ({
  page,
  isMobile
}) => {
  const state = await fixture(page);
  await open(page, !!isMobile);
  const gate = (state.readGate = deferred());
  const reads = state.reads;
  await page.clock.runFor(4000);
  await expect.poll(() => state.reads).toBe(reads + 1);
  state.failRead = true;
  await begin(page);
  const panel = panelFor(page);
  await expect(panel.getByRole('alert')).toContainText('Baseline check started.');
  gate.resolve();
  await expect(panel.getByText('Running', { exact: true })).toBeVisible();
  await expect(panel.getByRole('button', { name: 'Check clean baseline' })).toBeDisabled();
  await expect(panel.getByRole('button', { name: 'Cancel baseline check' })).toBeEnabled();
  expect(state.starts).toBe(1);
});

test('a running baseline survives successful refresh followed by inactive navigation and a status outage', async ({
  page,
  isMobile
}) => {
  const state = await fixture(page);
  const writes = trackWrites(page);
  await open(page, !!isMobile);
  await begin(page);
  const panel = panelFor(page);
  // Prove the successful running GET was applied before hiding the component.
  await expect(panel.getByRole('status')).toHaveCount(0);
  await expect(panel.getByText('Matches the saved configuration', { exact: true })).toBeVisible();
  state.failRead = true;
  await openNavigation(page, 'Overview', !!isMobile);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(panel.getByRole('alert')).toContainText('Synthetic baseline read outage');
  await expect(panel.getByText('Running', { exact: true })).toBeVisible();
  await expect(panel.getByRole('button', { name: 'Check clean baseline' })).toBeDisabled();
  await panel.getByRole('button', { name: 'Cancel baseline check', exact: true }).click();
  await expect(panel.getByRole('alert')).toContainText('Baseline cancellation requested.');
  await expect(panel.getByRole('button', { name: 'Cancellation requested' })).toBeDisabled();
  state.failRead = false;
  finish(state, 'cancelled');
  await panel.getByRole('button', { name: 'Retry baseline status' }).click();
  await expect(panel.getByText('Cancelled', { exact: true })).toBeVisible();
  // A terminal read must release the retained running resource.
  state.failRead = true;
  await openNavigation(page, 'Overview', !!isMobile);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(panel.getByRole('alert')).toContainText('Synthetic baseline read outage');
  await expect(panel.getByText('Running', { exact: true })).toHaveCount(0);
  await expect(
    panel.getByRole('button', { name: /Cancel baseline check|Cancellation requested/ })
  ).toHaveCount(0);
  expect(state.starts).toBe(1);
  expect(state.cancels).toBe(1);
  expect(writes).toHaveLength(2);
});

for (const action of ['start', 'cancel'] as const) {
  for (const leaveBeforeResponse of [false, true]) {
    test(`accepted baseline ${action} refreshes global controls ${leaveBeforeResponse ? 'while inactive' : 'before a stalled detail read'}`, async ({
      page,
      isMobile
    }) => {
      const state = await fixture(page);
      const writes = trackWrites(page);
      await open(page, !!isMobile);
      const panel = panelFor(page);
      if (action === 'cancel') {
        state.baselineActive = true;
        const stateReads = state.stateReads;
        await begin(page);
        await expect.poll(() => state.stateReads).toBe(stateReads + 1);
        await expect(panel.getByRole('button', { name: 'Cancel baseline check' })).toBeEnabled();
        await expect(page.getByLabel('Default branch', { exact: true })).toBeDisabled();
      }
      const postGate = (state.postGate = deferred());
      if (action === 'start') await begin(page);
      else await panel.getByRole('button', { name: 'Cancel baseline check' }).click();
      await expect.poll(() => (action === 'start' ? state.starts : state.cancels)).toBe(1);
      if (leaveBeforeResponse) await openNavigation(page, 'Overview', !!isMobile);
      const reads = state.reads;
      const stateReads = state.stateReads;
      const readGate = (state.readGate = deferred());
      state.baselineActive = action === 'start';
      if (action === 'cancel') finish(state, 'cancelled');
      postGate.resolve();
      try {
        // The clock stays paused, so only the accepted action can refresh /state.
        await expect.poll(() => state.stateReads).toBe(stateReads + 1);
        if (leaveBeforeResponse) expect(state.reads).toBe(reads);
        else {
          await expect.poll(() => state.reads).toBe(reads + 1);
          await openNavigation(page, 'Overview', !!isMobile);
        }
        for (const name of ['Start continuous', 'Run once', 'Run an audit']) {
          const control = page.getByRole('button', { name, exact: true });
          if (action === 'start') await expect(control).toBeDisabled();
          else await expect(control).toBeEnabled();
        }
        expect(state.starts).toBe(1);
        expect(state.cancels).toBe(action === 'cancel' ? 1 : 0);
        expect(writes).toHaveLength(action === 'cancel' ? 2 : 1);
      } finally {
        readGate.resolve();
      }
    });
  }
}
