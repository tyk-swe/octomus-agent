import { expect } from '@playwright/test';
import { login, openNavigation, patchState, test, unsandboxed } from './synthetic';

test('the overview proves containment from inside a sandbox, names the deployment limits, then names a failed check', async ({
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

  // The next poll reports what the probe observed.
  await patchState(page, (snapshot) => {
    const test = snapshot.sandbox.self_test!;
    test.passed = false;
    test.checks[6] = { ...test.checks[6], passed: false, detail: 'reached 1.1.1.1:443' };
  });
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
