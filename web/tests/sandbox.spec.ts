import { expect } from '@playwright/test';
import {
  deferred,
  token,
  login,
  nextPoll,
  openNavigation,
  patchState,
  test,
  unsandboxed
} from './synthetic';

test('the overview proves containment from inside a sandbox, names the deployment limits, then names a failed check', async ({
  page
}) => {
  await page.clock.install();
  await login(page);
  const panel = page.getByRole('region', { name: 'Sandbox' });
  await expect(panel.locator('.badge')).toHaveText('Contained');
  await expect(panel).toContainText('All 11 containment checks passed inside a real sandbox.');
  await expect(panel).toContainText('One container per agent turn and per verification command');
  await expect(panel).toContainText(
    '2 CPU · 4 GiB memory, no swap · 1024 processes · up to 12 at once'
  );
  await expect(panel).toContainText('api.openai.com, auth.openai.com, chatgpt.com');
  await expect(panel).toContainText('proxy.golang.org, registry.npmjs.org');
  const checks = panel.getByRole('list', { name: 'Containment checks' }).getByRole('listitem');
  await expect(checks).toHaveCount(11);
  await expect(checks.first()).toContainText('Runs as an unprivileged user');
  await expect(page.getByRole('status', { name: 'Sandbox status' })).toHaveCount(0);

  // The next poll reports what the probe observed.
  await patchState(page, (snapshot) => {
    const test = snapshot.sandbox.self_test!;
    test.passed = false;
    test.checks[6] = { ...test.checks[6], passed: false, detail: 'reached 1.1.1.1:443' };
  });
  await nextPoll(page);
  await expect(panel.locator('.badge')).toHaveText('1 check failed', { timeout: 10000 });
  await expect(panel.locator('li.failed')).toContainText(
    'Has no direct route to the internet reached 1.1.1.1:443'
  );
});

test('an unsandboxed service carries a permanent warning and a failed setup step', async ({
  page,
  isMobile
}) => {
  await patchState(page, unsandboxed);
  await login(page);
  await expect(page.getByRole('status', { name: 'Sandbox status' })).toContainText(
    'Unsandboxed. Agents and verification commands run with this service user’s permissions.'
  );
  const panel = page.getByRole('region', { name: 'Sandbox' });
  await expect(panel.locator('.badge')).toHaveText('Unsandboxed');
  await expect(panel.getByRole('button', { name: 'Run self-test' })).toHaveCount(0);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(page.locator('[data-step="sandbox"] .badge')).toHaveText('Off');
  await expect(page.locator('[data-step="sandbox"]')).toContainText('--sandbox off');
  await expect(page.getByLabel('Codex executable')).toBeEditable();
});

test('a self-test completing after reconnect cannot release the current control command', async ({
  page
}) => {
  await page.addInitScript(() => {
    const original = window.fetch.bind(window);
    window.fetch = (input, options) =>
      original(
        input,
        String(input).endsWith('/sandbox/self-test') ? { ...options, signal: undefined } : options
      );
  });
  const old = deferred(),
    command = deferred();
  let selfTests = 0,
    controls = 0;
  await patchState(page, (snapshot) => {
    snapshot.capabilities.cycle = { allowed: true, reason: '' };
  });
  await page.route('**/api/sandbox/self-test', async (route) => {
    selfTests++;
    await old.promise;
    await route.fulfill({ json: {} });
  });
  await page.route('**/api/control/cycle', async (route) => {
    controls++;
    await command.promise;
    await route.fulfill({ json: { paused: false, mode: 'batch' } });
  });
  try {
    await login(page);
    await page
      .getByRole('region', { name: 'Sandbox' })
      .getByRole('button', { name: 'Run self-test' })
      .click();
    await expect.poll(() => selfTests).toBe(1);
    await page.getByRole('button', { name: /Disconnect/ }).click();
    await page.getByLabel('Operator access token').fill(token);
    await page.getByRole('button', { name: 'Open dashboard' }).click();
    await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
    await page.getByRole('button', { name: 'Run once', exact: true }).click();
    await expect.poll(() => controls).toBe(1);
    await expect(page.getByRole('button', { name: 'Starting run…' })).toBeDisabled();
    const oldResponse = page.waitForResponse('**/api/sandbox/self-test');
    old.resolve();
    await (await oldResponse).finished();
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(resolve)));
    await expect(page.getByRole('button', { name: 'Starting run…' })).toBeDisabled();
    expect(controls).toBe(1);
  } finally {
    old.resolve();
    command.resolve();
  }
});
