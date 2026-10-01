import { expect, type Page } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';
import { hostMode, login, openNavigation, test } from './synthetic';

async function patchState(page: Page, patch: (snapshot: Snapshot) => void) {
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    patch(snapshot);
    await route.fulfill({ json: snapshot });
  });
}

test('the overview proves containment from inside a sandbox and names the deployment limits', async ({
  page
}) => {
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
});

test('an unsandboxed service carries a permanent warning and a failed setup step', async ({
  page,
  isMobile
}) => {
  await hostMode(page);
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

test('a broker outage explains that no work starts until it returns', async ({ page }) => {
  await patchState(page, (snapshot) => {
    snapshot.sandbox.healthy = false;
    snapshot.sandbox.broker = null;
    snapshot.sandbox.error = 'Sandbox broker is unavailable at /run/octomus/sandboxd.sock';
  });
  await login(page);
  await expect(page.getByRole('status', { name: 'Sandbox status' })).toContainText(
    'Unavailable. Sandbox broker is unavailable at /run/octomus/sandboxd.sock No work starts until it is back.'
  );
  await expect(
    page.getByRole('region', { name: 'Sandbox' }).getByRole('button', { name: 'Run self-test' })
  ).toBeDisabled();
});

test('a failed containment check is named with what the probe observed', async ({ page }) => {
  await patchState(page, (snapshot) => {
    const test = snapshot.sandbox.self_test!;
    test.passed = false;
    test.checks[6] = { ...test.checks[6], passed: false, detail: 'reached 1.1.1.1:443' };
  });
  await login(page);
  const panel = page.getByRole('region', { name: 'Sandbox' });
  await expect(panel.locator('.badge')).toHaveText('1 check failed');
  await expect(panel.locator('li.failed')).toContainText(
    'Has no direct route to the internet reached 1.1.1.1:443'
  );
});

test('Run self-test asks the service once and shows the new proof', async ({ page }) => {
  const calls: string[] = [];
  await page.route('**/api/sandbox/self-test', async (route) => {
    calls.push(route.request().method());
    await route.fulfill({ json: { ok: true } });
  });
  await login(page);
  await page
    .getByRole('region', { name: 'Sandbox' })
    .getByRole('button', { name: 'Run self-test' })
    .click();
  await expect.poll(() => calls).toEqual(['POST']);
  await expect(page.getByRole('region', { name: 'Sandbox' }).locator('.badge')).toHaveText(
    'Contained'
  );
});

test('a repository pinned by the deployment is read-only in configuration', async ({
  page,
  isMobile
}) => {
  await patchState(page, (snapshot) => {
    snapshot.sandbox.pinned_repository = 'fixture/project';
  });
  await login(page);
  await expect(page.getByRole('region', { name: 'Sandbox' })).toContainText(
    'fixture/project, fixed by the deployment'
  );
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(page.getByLabel('Repository path')).not.toBeEditable();
  await expect(page.getByLabel('GitHub repository')).not.toBeEditable();
  await expect(page.getByText('the operator token cannot change it')).toBeVisible();
  await expect(page.getByLabel('Codex executable')).not.toBeEditable();
});

test('task details show each session sandbox and the egress it was refused', async ({
  page,
  isMobile
}) => {
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await page.getByRole('tab', { name: 'Sessions' }).click();
  const record = page.getByLabel('Sandbox record').first();
  await expect(record).toContainText('Sandboxed · 1 container · image ffffffffffff');
  await expect(record.locator('.badge.failed')).toHaveText('example.com:443 ×2');
  await expect(record.locator('.badge.blocked')).toHaveText('registry.npmjs.org:443 ×1');
  await expect(record).toContainText('api.openai.com:443 ×14');
  await page.getByRole('tab', { name: 'Verification' }).click();
  const verification = page.getByLabel('Sandbox record').first();
  await expect(verification).toContainText('proxy.golang.org:443 ×5');
  await expect(verification.locator('.badge.failed')).toHaveCount(0);
  await expect(verification.locator('.badge.blocked')).toHaveCount(0);
});
