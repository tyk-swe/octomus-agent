import { access, mkdir, readFile } from 'node:fs/promises';
import { execFileSync, spawn } from 'node:child_process';
import { once } from 'node:events';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { chromium } from '@playwright/test';

const web = fileURLToPath(new URL('../', import.meta.url));
const root = resolve(web, '..');
const output = resolve(root, 'docs/launch-assets');
const dashboardUrl = 'http://127.0.0.1:4299';
const showcaseUrl = 'http://127.0.0.1:4308/showcase/';
const token = 'browser-test-operator-token-32-characters';

await access(resolve(root, 'target/debug/octomus-agent'));
await access(resolve(web, 'build/200.html'));
for (const url of [dashboardUrl, showcaseUrl]) {
  if (
    await fetch(url).then(
      () => true,
      () => false
    )
  )
    throw new Error(`A server is already using ${url}; stop it before capturing launch assets.`);
}
execFileSync(
  process.execPath,
  [
    resolve(web, 'scripts/build-showcase.mjs'),
    '--mode',
    'fixture',
    '--input',
    resolve(web, 'launch/sample.public.json')
  ],
  { cwd: web, stdio: 'inherit' }
);
await mkdir(output, { recursive: true });

/** @param {string[]} args */
function server(args) {
  const child = spawn('python3', args, { cwd: root, stdio: 'ignore' });
  const exited = once(child, 'exit');
  return { child, exited };
}
const dashboard = server(['tests/serve_ui.py']);
const showcase = server([
  '-m',
  'http.server',
  '4308',
  '--bind',
  '127.0.0.1',
  '--directory',
  'dist'
]);
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
    if (dashboard.child.exitCode !== null || showcase.child.exitCode !== null)
      throw new Error('A synthetic capture server exited before becoming ready.');
    await delay(100);
  }
  throw new Error(`Timed out waiting for ${url}`);
}
/** @param {string} path @param {string} mime */
async function dataUrl(path, mime) {
  return `data:${mime};base64,${(await readFile(path)).toString('base64')}`;
}

/** @type {import('@playwright/test').Browser | undefined} */
let browser;
try {
  browser = await chromium.launch();
  await ready(`${dashboardUrl}/healthz`);
  await ready(showcaseUrl);
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

  const demo = await browser.newPage({
    viewport: { width: 1100, height: 820 },
    reducedMotion: 'reduce'
  });
  await demo.goto(`${showcaseUrl}#proposal=0`);
  await demo.getByRole('heading', { name: 'Keep the final line of an export' }).waitFor();
  const idea = await demo.locator('.detail > section').first().screenshot();
  const reviews = await demo
    .getByRole('heading', { name: 'Both reviewer slots' })
    .locator('..')
    .screenshot();
  await demo.goto(`${showcaseUrl}#proposal=3`);
  await demo.getByRole('heading', { name: 'A failing check stops delivery' }).waitFor();
  const checks = await demo
    .getByRole('heading', { name: 'Configured checks' })
    .locator('..')
    .screenshot();
  const dashboardImage = await dataUrl(resolve(root, 'docs/dashboard.png'), 'image/png');
  const mascot = await dataUrl(resolve(web, 'launch/public/octopus.svg'), 'image/svg+xml');
  const mark = await dataUrl(resolve(web, 'static/favicon.svg'), 'image/svg+xml');
  const art = await browser.newPage({ reducedMotion: 'reduce' });
  /** @param {{width: number, height: number, title: string, kicker: string, copy: string, image: string, color: string, synthetic?: boolean}} content */
  function artwork(content) {
    return `<!doctype html><html lang="en"><meta charset="utf-8"><title>Octomus launch artwork</title><style>
      *{box-sizing:border-box}body{margin:0;background:${content.color};color:#163e35;font-family:'Avenir Next','Segoe UI',system-ui,sans-serif;width:${content.width}px;height:${content.height}px;padding:48px 52px;overflow:hidden;display:flex;flex-direction:column}
      header{display:flex;align-items:center;justify-content:space-between;font-size:25px;font-weight:800;letter-spacing:-1px}header div{display:flex;align-items:center;gap:11px}header img{width:38px;height:38px}header span{font-size:12px;font-weight:650;letter-spacing:1px;text-transform:uppercase}
      main{display:grid;grid-template-columns:.9fr 1.1fr;align-items:center;gap:36px;flex:1;min-height:0}h1{font-size:60px;line-height:1.04;letter-spacing:-3.2px;margin:0 0 24px;font-weight:800}p{font-size:19px;line-height:1.6;margin:0;max-width:390px}.kicker{font-size:11px;letter-spacing:1.5px;font-weight:750;text-transform:uppercase;margin-bottom:20px}
      .frame{padding:18px;background:#fffdf8;border:1.5px solid #163e35;border-radius:16px;box-shadow:8px 8px 0 #163e35;max-height:100%;overflow:hidden}.frame img{display:block;width:100%;max-height:${content.height - 275}px;object-fit:contain}.frame .label{font-size:10px;font-weight:700;letter-spacing:1px;text-transform:uppercase;margin-top:14px;padding-top:12px;border-top:1px solid #d5d8ca}
      footer{font-size:12px;display:flex;justify-content:space-between;gap:15px;border-top:1px solid #163e3540;padding-top:18px}footer strong{font-weight:750}
      </style><body><header><div><img src="${mark}" alt="">octomus</div><span>Self-hosted preview</span></header><main><div><p class="kicker">${content.kicker}</p><h1>${content.title}</h1><p>${content.copy}</p></div><div class="frame"><img src="${content.image}" alt="">${content.synthetic ? '<p class="label">Synthetic example · Not a real run</p>' : ''}</div></main><footer><strong>Your repository. Your boundaries. Your final say.</strong><span>${content.synthetic ? 'Actual interface · Fabricated sample data' : 'Powered by your Codex or OpenCode access'}</span></footer></body></html>`;
  }
  const slides = [
    {
      name: '01-discover.png',
      title: 'Find the work worth doing.',
      kicker: '01 / Discover',
      copy: 'Ground proposals in your repository, then explain the problem, scope, and benefit.',
      image: `data:image/png;base64,${idea.toString('base64')}`,
      color: '#fffaf0'
    },
    {
      name: '02-review.png',
      title: 'Give every idea a second look.',
      kicker: '02 / Challenge',
      copy: 'Two independent reviewers challenge the scope. Rejected and deferred ideas keep their reasons.',
      image: `data:image/png;base64,${reviews.toString('base64')}`,
      color: '#e2d4f4'
    },
    {
      name: '03-verify.png',
      title: 'A failing check stops delivery.',
      kicker: '03 / Verify',
      copy: 'Inspect findings and configured checks. Failed work stays visible and blocked.',
      image: `data:image/png;base64,${checks.toString('base64')}`,
      color: '#ffe7d9'
    },
    {
      name: '04-control.png',
      title: 'Keep the final say.',
      kicker: '04 / Review the PR',
      copy: 'Follow the work, tune your limits, and inspect the evidence. You decide what merges.',
      image: dashboardImage,
      color: '#d6edb1'
    }
  ];
  for (const slide of slides) {
    await art.setViewportSize({ width: 1270, height: 760 });
    await art.setContent(artwork({ ...slide, width: 1270, height: 760, synthetic: true }));
    await art.screenshot({ path: resolve(output, slide.name) });
  }
  await art.setViewportSize({ width: 1200, height: 630 });
  await art.setContent(
    artwork({
      width: 1200,
      height: 630,
      title: 'Your repo’s next improvement. Ready for review.',
      kicker: 'A few extra arms for your repository',
      copy: 'Discover useful work. Challenge the ideas. Get reviewed pull requests.',
      image: mascot,
      color: '#fffaf0'
    })
  );
  await art.screenshot({ path: resolve(output, 'social-card.png') });
  await art.setViewportSize({ width: 240, height: 240 });
  await art.setContent(
    `<html lang="en"><title>Octomus thumbnail</title><body style="margin:0;background:#e2d4f4;display:grid;place-items:center;width:240px;height:240px"><img src="${mascot}" alt="Octomus" style="width:195px;transform:rotate(-7deg)"></body></html>`
  );
  await art.screenshot({ path: resolve(output, 'thumbnail.png') });
  console.log(`Launch assets: ${output}\nDashboard screenshot: docs/dashboard.png`);
} finally {
  try {
    await browser?.close();
  } finally {
    for (const server of [dashboard, showcase]) server.child.kill('SIGINT');
    await Promise.all([dashboard.exited, showcase.exited]);
  }
}
