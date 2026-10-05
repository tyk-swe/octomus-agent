import { fileURLToPath } from 'node:url';
import { login, patchState, test } from './synthetic';

// Captures docs/dashboard.png; runs only through `npm run launch:assets`.
test.skip(
  !process.env.OCTOMUS_LAUNCH_ASSETS,
  'Set OCTOMUS_LAUNCH_ASSETS=1 to capture launch assets'
);
test.use({ viewport: { width: 1440, height: 1080 }, reducedMotion: 'reduce' });

test('dashboard screenshot', async ({ page, baseURL }) => {
  await page.route('**/*', (route) => {
    const request = route.request();
    if (request.method() !== 'GET' || !request.url().startsWith(`${baseURL}/`))
      return route.abort();
    return route.continue();
  });
  await patchState(page, (snapshot) => {
    snapshot.repository = 'synthetic-example/field-notes';
    snapshot.configured = true;
    snapshot.audit_configured = true;
    snapshot.pr_capacity = {
      limit: 5,
      owned_open: 1,
      reserved: 0,
      remaining: 4,
      observed_at: new Date().toISOString(),
      status: 'ready',
      reason: null
    };
  });
  await login(page);
  await page.getByText('Handle interrupted verification commands', { exact: true }).waitFor();
  await page.evaluate(() => {
    const label = document.createElement('div');
    label.textContent = 'SYNTHETIC INTERFACE PREVIEW · No live run or delivery is represented';
    label.style.cssText =
      'position:fixed;bottom:0;left:0;right:0;z-index:9999;padding:9px;background:#e2d4f4;color:#163e35;text-align:center;font:600 12px system-ui;border-top:1px solid #b4a8c6';
    document.body.append(label);
  });
  await page.screenshot({
    path: fileURLToPath(new URL('../../docs/dashboard.png', import.meta.url)),
    animations: 'disabled'
  });
});
