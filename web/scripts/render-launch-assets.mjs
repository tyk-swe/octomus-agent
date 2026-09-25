import { access } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { chromium } from '@playwright/test';

const web = fileURLToPath(new URL('../', import.meta.url));
const root = resolve(web, '..');
const dashboardUrl = 'http://127.0.0.1:4299';
const token = 'browser-test-operator-token-32-characters';

await access(resolve(root, 'bin/octomus-agent'));
await access(resolve(web, 'build/200.html'));
if (
  await fetch(dashboardUrl).then(
    () => true,
    () => false
  )
)
  throw new Error(
    `A server is already using ${dashboardUrl}; stop it before capturing the dashboard screenshot.`
  );

/** @param {string[]} args */
function server(args) {
  const child = spawn('python3', args, { cwd: root, stdio: 'ignore' });
  const exited = once(child, 'exit');
  return { child, exited };
}
const dashboard = server(['tests/serve_ui.py']);
/** @param {string} url */
async function ready(url) {
  for (let attempt = 0; attempt < 150; attempt++) {
    if (
      await fetch(url).then(
        (r) => r.ok,
        () => false
      )
    )
      return;
    if (dashboard.child.exitCode !== null)
      throw new Error('The synthetic capture server exited before becoming ready.');
    await delay(100);
  }
  throw new Error(`Timed out waiting for ${url}`);
}

/** @type {import('@playwright/test').Browser | undefined} */
let browser;
try {
  browser = await chromium.launch();
  await ready(`${dashboardUrl}/healthz`);
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1080 },
    reducedMotion: 'reduce'
  });
  await page.route('**/*', async (route) => {
    const request = route.request();
    if (request.method() !== 'GET' || !request.url().startsWith(`${dashboardUrl}/`))
      await route.abort();
    else if (new URL(request.url()).pathname === '/api/state') {
      const response = await route.fetch();
      /** @type {import('../src/lib/types.js').Snapshot} */
      const snapshot = await response.json();
      // Show a configured, paused synthetic workspace without configuring the service.
      await route.fulfill({
        response,
        json: {
          ...snapshot,
          repository: 'synthetic-example/field-notes',
          configured: true,
          audit_configured: true,
          pr_capacity: {
            limit: 5,
            owned_open: 1,
            reserved: 0,
            remaining: 4,
            observed_at: new Date().toISOString(),
            status: 'ready',
            reason: null
          }
        }
      });
    } else await route.continue();
  });
  await page.goto(dashboardUrl);
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await page.getByRole('heading', { name: 'The bigger picture.' }).waitFor();
  await page.getByText('Handle interrupted verification commands', { exact: true }).waitFor();
  await page.evaluate(() => {
    const label = document.createElement('div');
    label.textContent = 'SYNTHETIC INTERFACE PREVIEW · No live run or delivery is represented';
    label.style.cssText =
      'position:fixed;bottom:0;left:0;right:0;z-index:9999;padding:9px;background:#e2d4f4;color:#163e35;text-align:center;font:600 12px system-ui;border-top:1px solid #b4a8c6';
    document.body.append(label);
  });
  await page.screenshot({ path: resolve(root, 'docs/dashboard.png'), animations: 'disabled' });
  console.log('Dashboard screenshot: docs/dashboard.png');
} finally {
  try {
    await browser?.close();
  } finally {
    dashboard.child.kill('SIGINT');
    await dashboard.exited;
  }
}
