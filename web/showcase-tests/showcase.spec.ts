import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { readFileSync, mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { publicPrUrl } from '../showcase/links';
import { TONES } from '../src/lib/evidence';

const hostile =
  '<img src="https://hostile.invalid/track" onerror="window.pwned=true"><script>alert(1)</script>';
test.beforeAll(() => {
  const payload = JSON.parse(readFileSync('showcase/synthetic.public.json', 'utf8'));
  payload.evidence.proposals[0].problem = hostile;
  payload.evidence.proposals[2].linked_tasks[0].pull_request = {
    number: 9,
    url: 'javascript:alert(document.cookie)',
    source: 'recorded_task_reference'
  };
  const second = payload.evidence.proposals[2].linked_tasks[1];
  second.required_commands = {
    state: 'not_configured',
    commands: [],
    all_passed_at_output_revision: false
  };
  const dir = mkdtempSync(join(tmpdir(), 'showcase-browser-'));
  try {
    const input = join(dir, 'synthetic.public.json');
    writeFileSync(input, JSON.stringify(payload));
    execFileSync(process.execPath, [
      'scripts/build-showcase.mjs',
      '--mode',
      'fixture',
      '--input',
      input
    ]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('static explorer preserves evidence, fragment selection, hostile text and network isolation', async ({
  page
}, testInfo) => {
  const requests: string[] = [],
    errors: string[] = [],
    violations: string[] = [];
  page.on('request', (request) => {
    requests.push(request.url());
    if (request.headers()['authorization']) violations.push('authorization header');
    if (!['document', 'script', 'stylesheet'].includes(request.resourceType()))
      violations.push(request.resourceType());
  });
  page.on('pageerror', (error) => errors.push(error.message));
  await page.addInitScript(() => {
    const denied = () => {
      throw new Error('Forbidden public runtime capability');
    };
    window.fetch = denied;
    window.XMLHttpRequest = denied as unknown as typeof XMLHttpRequest;
    window.WebSocket = denied as unknown as typeof WebSocket;
    window.EventSource = denied as unknown as typeof EventSource;
    Storage.prototype.setItem = denied;
    Storage.prototype.getItem = denied;
    navigator.sendBeacon = denied;
    window.setInterval = denied;
  });
  await page.goto('./');
  await expect(page.getByTestId('provenance')).toContainText('Synthetic example — not a real run.');
  await expect(page.getByText('Planning complete', { exact: true })).toBeVisible();
  await expect(page.getByText(hostile, { exact: false })).toBeVisible();
  await expect(page.locator('img, iframe, form, input, textarea')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: /Reviewer A/ })).toBeVisible();
  await expect(page.getByRole('heading', { name: /Reviewer B/ })).toBeVisible();
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open recorded PR #12' })).toHaveAttribute(
    'href',
    'https://github.com/synthetic-owner/synthetic-repository/pull/12'
  );
  // The installation CTA points at the README first-run path and is a visitor action only.
  const install = page.getByRole('link', { name: 'Read the first-run guide on GitHub' });
  await expect(install).toHaveAttribute(
    'href',
    'https://github.com/tyk-swe/octomus-agent#getting-started'
  );
  await expect(install).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(page.getByRole('heading', { name: 'Run it on your own repository' })).toBeVisible();
  await expect(page.locator('.install')).toContainText('built from source today');
  await expect(page.locator('.install')).toContainText('pending');
  await page.screenshot({
    path: `artifacts/showcase/synthetic-${testInfo.project.name}-overview.png`
  });
  await page.getByRole('navigation', { name: 'Proposals' }).getByRole('link').nth(1).click();
  await expect(page.getByText('No linked task recorded.', { exact: true })).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'Final decision', exact: true }).locator('..')
  ).toContainText('deferred');
  await page.getByRole('navigation', { name: 'Proposals' }).getByRole('link').nth(2).click();
  await expect(
    page.getByText('Every match is preserved; none is selected for you.', { exact: false })
  ).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Latest recorded code review' })).toHaveCount(0);
  await page.getByRole('link', { name: 'synthetic-blocked-a · record 1 · blocked' }).click();
  await expect(page.getByText('1 recorded finding', { exact: true })).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'P1 · Synthetic retry loses a pending result' })
  ).toBeVisible();
  await expect(page.getByText('Failed', { exact: true })).toBeVisible();
  await page.getByRole('heading', { name: 'Latest recorded code review' }).scrollIntoViewIfNeeded();
  await page.screenshot({
    path: `artifacts/showcase/synthetic-${testInfo.project.name}-adverse.png`
  });
  await expect(page.getByText(/URL not linked by the public link policy/)).toBeVisible();
  await expect(page.locator('a[href^="javascript:"]')).toHaveCount(0);
  await page.reload();
  await expect(
    page.getByRole('heading', { name: 'Task record: synthetic-blocked-a' })
  ).toBeVisible();
  await page.getByRole('link', { name: 'synthetic-blocked-b · record 2 · blocked' }).click();
  await expect(page.getByText('No checks configured', { exact: true })).toBeVisible();
  await page.goBack();
  await expect(
    page.getByRole('heading', { name: 'Task record: synthetic-blocked-a' })
  ).toBeVisible();
  await page.goto('./#proposal=3');
  await expect(page.getByText('Malformed batch', { exact: true })).toBeVisible();
  await expect(page.getByText('No verdict recorded', { exact: true })).toBeVisible();
  await page.goto('./#proposal=2&task=99');
  await expect(
    page.getByText('selected task record is unavailable', { exact: false })
  ).toBeVisible();
  await page.goto('./#proposal=999');
  await expect(page.getByRole('heading', { name: 'Proposal selection unavailable' })).toBeVisible();
  await page.goto('./#proposal=0&url=https://hostile.invalid/run.json');
  await page.waitForTimeout(11000); // Crosses the operator panel's ten-second polling interval.
  expect(
    requests.every(
      (url) =>
        /^http:\/\/127\.0\.0\.1:4307\/showcase\/(?:[?#].*)?$/.test(url) ||
        /^http:\/\/127\.0\.0\.1:4307\/showcase\/assets\/[^/]+\.(js|css)$/.test(url)
    )
  ).toBe(true);
  expect(violations).toEqual([]);
  expect(errors).toEqual([]);
  expect(
    await page.evaluate(() => ({
      local: localStorage.length,
      session: sessionStorage.length,
      cookies: document.cookie
    }))
  ).toEqual({ local: 0, session: 0, cookies: '' });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('public PR links reject hostile schemes, credentials, lookalike hosts and private hosts', () => {
  for (const url of [
    'javascript:alert(1)',
    'data:text/html,test',
    '//github.com/a/b/pull/1',
    'http://github.com/a/b/pull/1',
    'https://github.com@evil.test/a/b/pull/1',
    'https://github.com.evil.test/a/b/pull/1',
    'https://127.0.0.1/a/b/pull/1',
    'https://github.com/a/b/pull/1?token=secret',
    'https://github.com/a/b/pull/1#x',
    'https://github.com/a/b/pull/1\n',
    'https://github.com/a/b/pull/0'
  ])
    expect(publicPrUrl(url)).toBeNull();
  expect(publicPrUrl('https://github.com/a/b/pull/1')).toBe('https://github.com/a/b/pull/1');
});

// This attestation is deliberately synthetic and exists only in a temporary test file.
test('recorded mode displays the supplied byte binding without executing approval text', async ({
  page
}) => {
  const payload = JSON.parse(readFileSync('showcase/synthetic.public.json', 'utf8'));
  payload.mode = 'recorded';
  const bytes = JSON.stringify(payload);
  const digest = createHash('sha256').update(bytes).digest('hex');
  const reference = `SYNTHETIC TEST ONLY — no owner approval granted. ${hostile}`;
  const dir = mkdtempSync(join(tmpdir(), 'showcase-recorded-test-'));
  try {
    const input = join(dir, 'public.json'),
      approval = join(dir, 'approval.json');
    writeFileSync(input, bytes);
    writeFileSync(
      approval,
      JSON.stringify({
        approval_schema_version: 1,
        owner_reviewed: true,
        payload_sha256: digest,
        approval_reference: reference
      })
    );
    execFileSync(process.execPath, [
      'scripts/build-showcase.mjs',
      '--mode',
      'recorded',
      '--input',
      input,
      '--approval',
      approval
    ]);
    await page.goto('./');
    await expect(page.getByTestId('provenance')).toContainText(
      'Owner-reviewed public payload — recorded evidence.'
    );
    await expect(page.getByText(digest, { exact: true })).toBeVisible();
    await expect(page.getByText(reference, { exact: true })).toBeVisible();
    await expect(page.locator('img, iframe')).toHaveCount(0);
    await expect(
      page.getByText(
        'A hash binds reviewed bytes, not proof of truth or an independent signature.',
        { exact: true }
      )
    ).toBeVisible();
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// EvidenceFact.svelte and EvidenceText.svelte are shared by the dashboard and
// the showcase, and take their appearance from whichever stylesheet is loaded.
// A tone with no rule falls back to the neutral badge, so "no verdict recorded"
// would look like every other fact on the page.
/** A stylesheet's own text plus every file it imports, in cascade order. */
function wholeSheet(entry: string): string {
  const dir = entry.slice(0, entry.lastIndexOf('/'));
  const text = readFileSync(entry, 'utf8');
  const imports = [...text.matchAll(/@import\s+'([^']+)'/g)];
  return text + imports.map(([, target]) => readFileSync(`${dir}/${target}`, 'utf8')).join('\n');
}

test('every surface styles every badge tone and the shared text classes', () => {
  for (const sheet of ['src/app.css', 'showcase/style.css']) {
    const css = wholeSheet(sheet);
    for (const tone of TONES) {
      expect(css, `${sheet} is missing .badge.${tone}`).toContain(`.badge.${tone}`);
    }
    for (const shared of ['.muted', '.expandable', '.preview']) {
      expect(css, `${sheet} is missing ${shared}`).toContain(shared);
    }
  }
});
