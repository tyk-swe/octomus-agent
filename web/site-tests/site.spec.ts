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
    'https://tyk-swe.github.io/octomus-agent/'
  );
  await expect(page.locator('meta[property="og:image"]')).toHaveAttribute(
    'content',
    'https://tyk-swe.github.io/octomus-agent/social-card.png'
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
      /^(index\.html|public-run\.json|robots\.txt|sitemap\.xml|favicon\.svg|octopus\.svg|dashboard\.png|social-card\.png|index-[\w-]+\.(css|js))$/.test(
        name
      )
    )
  ).toBe(true);
  expect((await request.get('api/state')).status()).toBe(404);
  expect((await request.get('showcase/approval.json')).status()).toBe(404);
});
