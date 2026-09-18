import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { readFile, readdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

test.describe('without JavaScript', () => {
  test.use({ javaScriptEnabled: false });

  test('the landing page works without JavaScript at the Pages subdirectory', async ({ page }) => {
    const requests: string[] = [];
    const failures: string[] = [];
    page.on('request', (request) => requests.push(request.url()));
    page.on('response', (response) => {
      if (response.status() >= 400) failures.push(response.url());
    });
    await page.goto('http://127.0.0.1:4310/octomus-agent/');
    await expect(page).toHaveTitle('Octomus — Your repo’s next improvement. Ready for review.');
    await expect(page.getByRole('heading', { level: 1 })).toContainText(
      'Your repo’s next improvement.'
    );
    await expect(page.getByRole('link', { name: 'Explore a sample run' }).first()).toHaveAttribute(
      'href',
      './showcase/'
    );
    await page.getByText('What does it cost?', { exact: true }).click();
    await expect(page.getByText('not a dollar cap', { exact: false })).toBeVisible();
    expect(requests.every((url) => url.startsWith('http://127.0.0.1:4310/octomus-agent/'))).toBe(
      true
    );
    expect(failures).toEqual([]);
  });
});

test('public navigation, accessibility, and layout work on desktop and mobile', async ({
  page
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.goto('./');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', { name: 'Skip to content' })).toBeFocused();
  await page.keyboard.press('Enter');
  await page.getByRole('link', { name: 'Set up Octomus' }).click();
  await expect(page).toHaveURL(/#setup$/);
  await expect(page.getByRole('link', { name: 'Read the setup guide' })).toBeVisible();
  await expect(page.getByText('A dedicated Ubuntu 24.04 VM', { exact: true })).toBeVisible();
  await page.getByText('Is the demo a real run?', { exact: true }).click();
  await expect(page.getByText(/deliberately synthetic data/)).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true
  );
  expect(await page.evaluate(() => getComputedStyle(document.documentElement).scrollBehavior)).toBe(
    'auto'
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  expect(errors).toEqual([]);
  await page.screenshot({ path: `artifacts/site-${test.info().project.name}.png`, fullPage: true });
});

test('the guided sample preserves delivered, rejected, deferred, and blocked evidence', async ({
  page
}) => {
  const requests: string[] = [];
  page.on('request', (request) => requests.push(request.url()));
  await page.goto('./');
  await page.getByRole('link', { name: /01 \/ Delivered/ }).click();
  await expect(page.getByTestId('provenance')).toContainText('Synthetic example — not a real run.');
  await expect(
    page.getByRole('heading', { name: 'Keep the final line of an export' })
  ).toBeVisible();
  await page.getByRole('button', { name: '02 · Both reviewers' }).click();
  await expect(page.getByRole('heading', { name: 'Both reviewer slots' })).toBeFocused();
  await expect(page).toHaveURL(/#proposal=0$/);
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: /Open recorded PR/ })).toHaveCount(0);
  await page.getByRole('link', { name: /Skip the speculative rewrite/ }).click();
  await expect(page.getByRole('heading', { name: 'Skip the speculative rewrite' })).toBeVisible();
  await expect(
    page.getByText(
      'No demonstrated user problem or repeated complexity justifies a new framework. Keep the small functions.'
    )
  ).toBeVisible();
  await page.getByRole('link', { name: /Measure before adding a cache/ }).click();
  await expect(page.getByRole('heading', { name: 'Measure before adding a cache' })).toBeVisible();
  await page.getByRole('link', { name: /A failing check stops delivery/ }).click();
  await expect(
    page.getByRole('heading', { name: 'P1 · Synthetic retry loses a pending result', exact: true })
  ).toBeVisible();
  await expect(page.getByText('Failed', { exact: true })).toBeVisible();
  await expect(page.getByText('No pull request recorded', { exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('heading', { name: 'A failing check stops delivery' })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('heading', { name: 'Measure before adding a cache' })).toBeVisible();
  const downloadPromise = page.waitForEvent('download');
  await page.getByRole('link', { name: 'Download exact public payload' }).click();
  const download = await downloadPromise;
  expect(await readFile((await download.path())!)).toEqual(
    await readFile(new URL('../launch/sample.public.json', import.meta.url))
  );
  expect(requests.every((url) => url.startsWith('http://127.0.0.1:4310/octomus-agent/'))).toBe(
    true
  );
  expect(requests.some((url) => url.includes('/api/'))).toBe(false);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('the bundle has correct public metadata and only the intended public files', async ({
  page,
  request
}) => {
  await page.goto('./');
  await expect(page.locator('link[rel=canonical]')).toHaveAttribute(
    'href',
    'https://octomus-agent.tyk.sh/'
  );
  await expect(page.locator('meta[property="og:image"]')).toHaveAttribute(
    'content',
    'https://octomus-agent.tyk.sh/social-card.png'
  );
  for (const asset of [
    'social-card.png',
    'dashboard.png',
    'favicon.svg',
    'robots.txt',
    'sitemap.xml'
  ]) {
    expect((await request.get(asset)).ok()).toBe(true);
  }
  const png = await (await request.get('social-card.png')).body();
  expect([png.readUInt32BE(16), png.readUInt32BE(20)]).toEqual([1200, 630]);
  const files = await readdir(fileURLToPath(new URL('../../dist/site/', import.meta.url)), {
    recursive: true,
    withFileTypes: true
  });
  const names = files.filter((file) => file.isFile()).map((file) => file.name);
  expect(
    names.every((name) =>
      /^(index\.html|404\.html|_headers|public-run\.json|search-index\.json|configuration\.example\.json|robots\.txt|sitemap\.xml|favicon\.svg|octopus\.svg|dashboard\.png|social-card\.png|[\w-]+-[\w-]+\.(css|js))$/.test(
        name
      )
    )
  ).toBe(true);
  expect((await request.get('api/state')).status()).toBe(404);
  expect((await request.get('showcase/approval.json')).status()).toBe(404);
});

test.describe('static documentation', () => {
  test.use({ javaScriptEnabled: false });

  test('guides work without JavaScript at the root and a subdirectory', async ({ page }) => {
    for (const base of ['http://127.0.0.1:4310/', 'http://127.0.0.1:4310/octomus-agent/']) {
      await page.goto(base);
      await page
        .getByRole('navigation', { name: 'Main navigation' })
        .getByRole('link', { name: 'Docs', exact: true })
        .click();
      await expect(page.getByRole('heading', { level: 1 })).toHaveText(
        'A few extra arms for your repository.'
      );
      await expect(page.locator('.docs-search')).toBeHidden();
      await page
        .locator('.prose')
        .getByRole('link', { name: 'getting started', exact: true })
        .click();
      await expect(page).toHaveURL(base + 'docs/getting-started/');
      await expect(page.locator('pre').first()).toContainText('sudo apt-get update');
      await page
        .locator('.prose')
        .getByRole('link', { name: 'model routing', exact: true })
        .click();
      await expect(page).toHaveURL(base + 'docs/model-routing/');
      await page.reload();
      await expect(page.getByRole('heading', { level: 1 })).toHaveText('Model routing');
      await page.getByRole('link', { name: 'Octomus home' }).click();
      await expect(page).toHaveURL(base);
    }
  });
});

test('documentation search, copy, navigation, and accessibility work', async ({
  page,
  context
}, testInfo) => {
  const errors: string[] = [];
  const requests: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('request', (request) => requests.push(request.url()));
  await page.goto('docs/');
  await page.keyboard.press('/');
  const search = page.getByRole('searchbox', { name: 'Search the docs' });
  await expect(search).toBeFocused();
  await search.fill('webhook');
  const result = page
    .locator('#search-results')
    .getByRole('link', { name: /Deployment.*Optional attention webhook/ });
  await expect(result).toBeVisible();
  await result.click();
  await expect(page).toHaveURL(/docs\/deployment\/#optional-attention-webhook$/);
  await expect(
    page.getByRole('heading', { name: 'Optional attention webhook', exact: false })
  ).toBeVisible();
  await page.reload();
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  const code = await page.locator('.prose pre code').first().textContent();
  await page.getByRole('button', { name: 'Copy code', exact: true }).first().click();
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(code);
  await page.getByRole('searchbox').fill('zzzznonexistent');
  await expect(page.getByRole('status')).toContainText('No matching sections');
  await page.keyboard.press('Escape');
  await expect(page.locator('.search-panel')).toBeHidden();
  if (testInfo.project.name === 'mobile') {
    await page.getByText('Browse documentation', { exact: false }).click();
    await page
      .getByRole('navigation', { name: 'Mobile documentation' })
      .getByRole('link', { name: 'Configuration', exact: true })
      .click();
  } else {
    await page
      .getByRole('navigation', { name: 'Documentation', exact: true })
      .getByRole('link', { name: 'Configuration', exact: true })
      .click();
  }
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Configuration');
  await page.getByRole('searchbox').fill('max_open_prs');
  await expect(page.getByRole('status')).toContainText('result');
  await expect(page.locator('#search-results')).toContainText('Configuration');
  await page.keyboard.press('Escape');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  expect(errors).toEqual([]);
  expect(requests.every((url) => url.startsWith('http://127.0.0.1:4310/octomus-agent/'))).toBe(
    true
  );
  await page.screenshot({ path: `artifacts/docs-${testInfo.project.name}.png`, fullPage: true });
});

test('nested missing pages return a styled 404 with working navigation', async ({ page }) => {
  const brokenAssets: string[] = [];
  page.on('response', (response) => {
    if (response.request().resourceType() !== 'document' && response.status() >= 400)
      brokenAssets.push(response.url());
  });
  const response = await page.goto('http://127.0.0.1:4310/missing/deep/page');
  expect(response?.status()).toBe(404);
  await expect(page.getByRole('heading', { level: 1 })).toContainText('This arm');
  expect(brokenAssets).toEqual([]);
  await page.getByRole('link', { name: 'Explore the docs', exact: false }).click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText('A few extra arms');
});

test('all published guides, internal links, anchors, and search targets resolve', async ({
  page,
  request
}) => {
  await page.goto('docs/');
  const guides = await page
    .locator('.docs-sidebar nav a')
    .evaluateAll((links) => links.map((link) => (link as HTMLAnchorElement).href));
  expect(guides.length).toBeGreaterThanOrEqual(10);
  const sitemap = await (await request.get('sitemap.xml')).text();
  const search = (await (await request.get('docs/search-index.json')).json()) as { href: string }[];
  const destinations = new Set<string>();
  for (const guide of guides) {
    await page.goto(guide);
    await expect(page.getByRole('heading', { level: 1 })).toHaveCount(1);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
      true
    );
    const canonical = await page.locator('link[rel=canonical]').getAttribute('href');
    expect(sitemap).toContain(`<loc>${canonical}</loc>`);
    for (const link of await page
      .locator('a[href]')
      .evaluateAll((links) => links.map((link) => (link as HTMLAnchorElement).href))) {
      if (link.startsWith('http://127.0.0.1:4310/')) destinations.add(link);
    }
  }
  for (const entry of search)
    destinations.add(new URL(entry.href, 'http://127.0.0.1:4310/octomus-agent/').href);
  const pages = new Map<string, string>();
  for (const destination of destinations) {
    const url = new URL(destination);
    const fragment = decodeURIComponent(url.hash.slice(1));
    url.hash = '';
    if (!pages.has(url.href)) {
      const response = await request.get(url.href);
      expect(response.ok(), url.href).toBe(true);
      pages.set(url.href, await response.text());
    }
    if (fragment && !url.pathname.includes('/showcase/'))
      expect(pages.get(url.href), destination).toContain(`id="${fragment}"`);
  }
  const configuration = await (await request.get('docs/configuration.example.json')).json();
  expect(configuration).toEqual(
    JSON.parse(
      await readFile(new URL('../../docs/configuration.example.json', import.meta.url), 'utf8')
    )
  );
});
