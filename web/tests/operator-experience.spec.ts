import { test, expect, type Page } from '@playwright/test';
import type { Config, Model, Snapshot } from '../src/lib/types';
import { login, openNavigation, token } from './synthetic';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

// All writes terminate in browser fixtures; neither runner nor GitHub is contacted.
async function configurationFixture(page: Page) {
  const state = {
    saved: null as Config | null,
    reads: 0,
    writes: [] as Config[],
    catalogs: [] as { backend: string; binary: string }[],
    checks: [] as string[],
    failLoad: false,
    failSave: false,
    failCheck: false,
    loadGate: null as ReturnType<typeof deferred> | null,
    saveGate: null as ReturnType<typeof deferred> | null
  };
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.control.paused = true;
    snapshot.active_tasks = 0;
    snapshot.cycle_active = false;
    snapshot.active_cycle_mode = null;
    await route.fulfill({ json: snapshot });
  });
  await page.route('**/api/config', async (route) => {
    if (route.request().method() === 'PUT') {
      const draft: Config = route.request().postDataJSON();
      state.writes.push(draft);
      await state.saveGate?.promise;
      if (state.failSave) {
        await route.fulfill({ status: 409, json: { error: 'Synthetic save conflict' } });
      } else {
        state.saved = draft;
        await route.fulfill({ json: { ok: true } });
      }
      return;
    }
    state.reads++;
    if (!state.saved) {
      const response = await route.fetch();
      const initial: Config = await response.json();
      const model = { backend: 'codex' as const, model: 'gpt-6-astra', effort: 'medium' };
      state.saved = {
        ...initial,
        repository: '/fixture/repository',
        github_repo: 'fixture/project',
        default_branch: 'fixture-main',
        codex_binary: '/fixture/codex',
        opencode_binary: '/fixture/opencode',
        verification_commands: ['fixture saved test'],
        roles: Object.fromEntries(Object.keys(initial.roles).map((key) => [key, { ...model }])),
        tiers: Object.fromEntries(Object.keys(initial.tiers).map((key) => [key, { ...model }])),
        repair_route: { ...model }
      };
    }
    // Snapshot before a delayed refresh so edits can race a real stale response.
    const saved = structuredClone(state.saved);
    await state.loadGate?.promise;
    await route.fulfill(
      state.failLoad
        ? { status: 503, json: { error: 'Synthetic configuration unavailable' } }
        : { json: saved }
    );
  });
  await page.route('**/api/model-catalog', async (route) => {
    state.catalogs.push(route.request().postDataJSON());
    await route.fulfill({
      json: [
        {
          backend: 'codex',
          provider: null,
          provider_name: null,
          model: 'gpt-6-astra',
          display_name: 'Astra',
          efforts: ['medium', 'high'],
          variants: [],
          available: true,
          unavailable_reason: null
        }
      ] satisfies Model[]
    });
  });
  await page.route('**/api/doctor?*', async (route) => {
    state.checks.push(new URL(route.request().url()).searchParams.get('mode')!);
    await route.fulfill(
      state.failCheck
        ? { status: 500, json: { error: 'Synthetic connection check failed' } }
        : { json: { message: 'Synthetic saved configuration checked' } }
    );
  });
  return state;
}

test('configuration keeps drafts and catalogs across views, discards locally, and refreshes clean values', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
  await login(page);
  await navigate('Configuration');
  const branch = page.getByLabel('Default branch', { exact: true });
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });
  await expect(branch).toHaveValue('fixture-main');
  await page.getByLabel('Codex executable', { exact: true }).fill('/draft/codex');
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await branch.fill('draft-main');
  const draftCommands = '  fixture draft test  \n\nfixture draft build\n';
  await commands.fill(draftCommands);
  await page.getByLabel('Repair reasoning effort', { exact: true }).selectOption('high');
  await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Check audit connection' })).toBeDisabled();
  await navigate('Task queue');
  await expect(branch).toBeHidden();
  await navigate('Configuration');
  await expect(branch).toHaveValue('draft-main');
  await expect(commands).toHaveValue(draftCommands);
  await expect(page.getByLabel('Repair reasoning effort', { exact: true })).toHaveValue('high');
  await expect(
    page.getByLabel('Repair reasoning effort', { exact: true }).locator('option[value="high"]')
  ).toHaveCount(1);
  expect(state.reads).toBe(1);
  expect(state.catalogs).toEqual([{ backend: 'codex', binary: '/draft/codex' }]);
  expect(state.checks).toEqual([]);
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(branch).toHaveValue('fixture-main');
  await expect(commands).toHaveValue('fixture saved test');
  expect(state.writes).toEqual([]);
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Check audit connection' }).click();
  await expect.poll(() => state.checks).toEqual(['audit']);
  await navigate('Overview');
  state.saved!.default_branch = 'externally-saved-main';
  await navigate('Configuration');
  await expect(branch).toHaveValue('externally-saved-main');
  expect(state.reads).toBe(2);
  // A clean revisit must not overwrite typing started during that request.
  await navigate('Overview');
  state.loadGate = deferred();
  await navigate('Configuration');
  await expect.poll(() => state.reads).toBe(3);
  await branch.fill('edited-during-refresh');
  state.loadGate.resolve();
  await expect(page.getByRole('button', { name: 'Save configuration' })).toBeEnabled();
  await expect(branch).toHaveValue('edited-during-refresh');
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(branch).toHaveValue('externally-saved-main');
});

for (const check of [
  {
    mode: 'execution',
    button: 'Check connection',
    key: 'codex_binary',
    field: 'Codex executable'
  },
  {
    mode: 'audit',
    button: 'Check audit connection',
    key: 'repository',
    field: 'Repository path'
  }
] as const) {
  test(`${check.mode} check feedback clears when refreshed saved configuration changes`, async ({
    page,
    isMobile
  }) => {
    const state = await configurationFixture(page);
    const navigate = (name: string) => openNavigation(page, name, !!isMobile);
    await login(page);
    await navigate('Configuration');
    const field = page.getByLabel(check.field);
    const button = page.getByRole('button', { name: check.button, exact: true });
    await expect(button).toBeEnabled();

    for (const failed of [false, true]) {
      state.failCheck = failed;
      const feedback = page.getByRole(failed ? 'alert' : 'status');
      const text = failed
        ? 'Synthetic connection check failed'
        : 'Synthetic saved configuration checked';
      await button.click();
      await expect(feedback).toHaveText(text);

      // An unchanged saved configuration keeps its diagnostic result.
      await navigate('Overview');
      const refresh = page.waitForResponse('**/api/config');
      await navigate('Configuration');
      await refresh;
      await expect(button).toBeEnabled();
      await expect(feedback).toHaveText(text);

      // Simulate settings saved by another tab, then accept them on a clean revisit.
      await navigate('Overview');
      const updated = `/external/${check.key}-${failed ? 'after-failure' : 'after-success'}`;
      state.saved![check.key] = updated;
      await navigate('Configuration');
      await expect(field).toHaveValue(updated);
      await expect(feedback).toHaveCount(0);
      await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
    }
    expect(state.checks).toEqual([check.mode, check.mode]);
    expect(state.writes).toEqual([]);
  });
}

test('failed configuration loads retry, failed saves retain exact drafts, and successful saves set the discard baseline', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  state.failLoad = true;
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(page.getByRole('alert')).toContainText('Could not load configuration');
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeDisabled();
  state.failLoad = false;
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  const commands = page.getByRole('textbox', { name: /^Verification commands/ });
  await expect(commands).toHaveValue('fixture saved test');
  const draft = '  fixture new test  \n\nfixture new build\n';
  await commands.fill(draft);
  state.failSave = true;
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('alert')).toContainText('Synthetic save conflict');
  await expect(commands).toHaveValue(draft);
  await openNavigation(page, 'Overview', !!isMobile);
  await openNavigation(page, 'Configuration', !!isMobile);
  await expect(commands).toHaveValue(draft);
  state.failSave = false;
  state.saveGate = deferred();
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByRole('button', { name: 'Saving configuration…' })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Discard changes' })).toBeDisabled();
  // Even a scripted form submission while pending cannot create a duplicate write.
  await page.locator('form').evaluate((form) => (form as HTMLFormElement).requestSubmit());
  state.saveGate.resolve();
  await expect(page.getByRole('status')).toHaveText('Configuration saved.');
  expect(state.writes).toHaveLength(2);
  await expect(commands).toHaveValue('fixture new test\nfixture new build');
  await page.getByRole('button', { name: 'Check connection', exact: true }).click();
  await expect.poll(() => state.checks).toEqual(['execution']);
  await commands.fill('another draft');
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(commands).toHaveValue('fixture new test\nfixture new build');
});

for (const boundary of ['disconnect', 'expiry', 'reload']) {
  test(`configuration drafts and catalogs clear on ${boundary}`, async ({ page, isMobile }) => {
    await configurationFixture(page);
    await login(page);
    await openNavigation(page, 'Configuration', !!isMobile);
    await page.getByLabel('Default branch', { exact: true }).fill('private-unsaved-main');
    await page.getByRole('button', { name: 'Load Codex models' }).click();
    await expect(
      page.getByLabel('Repair reasoning effort', { exact: true }).locator('option[value="high"]')
    ).toHaveCount(1);
    if (boundary === 'disconnect') {
      if (isMobile) await page.getByRole('button', { name: 'Toggle navigation' }).click();
      await page.getByRole('button', { name: /Disconnect/ }).click();
    } else if (boundary === 'expiry') {
      await page.route('**/api/state', (route) =>
        route.fulfill({ status: 401, json: { error: 'Synthetic expired session' } })
      );
      await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible(
        { timeout: 10000 }
      );
      await page.unroute('**/api/state');
    } else await page.reload();
    await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible();
    // Reconnect in the same document for disconnect/expiry to exercise unmounting.
    await page.getByLabel('Operator access token').fill(token);
    await page.getByRole('button', { name: 'Open dashboard' }).click();
    await openNavigation(page, 'Configuration', !!isMobile);
    await expect(page.getByLabel('Default branch', { exact: true })).toHaveValue('fixture-main');
    await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
    await expect(
      page.getByLabel('Repair reasoning effort', { exact: true }).locator('option[value="high"]')
    ).toHaveCount(0);
  });
}

test('short desktop sidebars keep Disconnect reachable by mouse', async ({
  page,
  isMobile
}, testInfo) => {
  test.skip(!!isMobile, 'Exercises the desktop sidebar at 1280×680.');
  await page.setViewportSize({ width: 1280, height: 680 });
  await login(page);
  await page.locator('aside').hover();
  await page.mouse.wheel(0, 800);
  const disconnect = page.getByRole('button', { name: /Disconnect/ });
  await expect(disconnect).toBeInViewport({ ratio: 1 });
  await page.screenshot({ path: testInfo.outputPath('sidebar-1280x680.png') });
  await disconnect.click();
  await expect(page.getByRole('heading', { name: 'Your project’s control room.' })).toBeVisible();
});

for (const reducedMotion of ['reduce', 'no-preference'] as const) {
  test(`mobile navigation disclosure supports skip, selected states, Escape and destination focus (${reducedMotion})`, async ({
    page
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.emulateMedia({ reducedMotion });
    await login(page);
    const toggle = page.getByRole('button', { name: 'Toggle navigation' });
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByRole('navigation')).toBeHidden();
    await page.getByRole('link', { name: 'Skip to main content' }).focus();
    await page.keyboard.press('Enter');
    await expect(page.locator('main')).toBeFocused();
    await toggle.focus();
    await page.keyboard.press('Tab');
    expect(await page.evaluate(() => !!document.activeElement?.closest('aside'))).toBe(false);
    await toggle.focus();
    await page.keyboard.press('Enter');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const overview = page
      .getByRole('navigation')
      .getByRole('button', { name: 'Overview', exact: true });
    await expect(overview).toHaveAttribute('aria-current', 'page');
    await expect(overview).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(toggle).toBeFocused();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await page.keyboard.press('Enter');
    const queue = page
      .getByRole('navigation')
      .getByRole('button', { name: 'Task queue', exact: true });
    await page.keyboard.press('Tab');
    await expect(queue).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(page.locator('main')).toBeFocused();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await page
      .getByRole('group', { name: 'Task filters' })
      .getByRole('button', { name: 'queued', exact: true })
      .click();
    await expect(
      page
        .getByRole('group', { name: 'Task filters' })
        .getByRole('button', { name: 'queued', exact: true })
    ).toHaveAttribute('aria-pressed', 'true');
    await toggle.click();
    await expect(queue).toHaveAttribute('aria-current', 'page');
    await page.keyboard.press('Escape');
    await page.setViewportSize({ width: 1280, height: 800 });
    await expect(page.getByRole('navigation')).toBeVisible();
  });
}

for (const list of [
  {
    view: 'Task queue',
    endpoint: 'tasks',
    noun: 'tasks',
    rows: '.task-row',
    empty: 'The next good idea starts here.',
    filter: 'queued'
  },
  {
    view: 'Proposals',
    endpoint: 'proposals',
    noun: 'proposals',
    rows: '.proposal-card',
    empty: 'Better ideas start with questions.',
    filter: 'rejected'
  },
  {
    view: 'Pull requests',
    endpoint: 'prs',
    noun: 'pull requests',
    rows: '.pr-row',
    empty: 'Room for your next improvement.',
    filter: 'merged'
  }
]) {
  test(`${list.view} distinguishes loading, retry, retained results and filtered emptiness`, async ({
    page,
    isMobile
  }) => {
    let mode: 'failure' | 'rows' | 'empty' = 'failure';
    let gate: ReturnType<typeof deferred> | null = deferred();
    await page.route(`**/api/${list.endpoint}?*`, async (route) => {
      await gate?.promise;
      if (mode === 'failure')
        await route.fulfill({ status: 503, json: { error: 'Synthetic list failure' } });
      else {
        const response = await route.fetch();
        const result = await response.json();
        if (mode === 'empty') result.items = [];
        await route.fulfill({ json: result });
      }
    });
    await login(page);
    await openNavigation(page, list.view, !!isMobile);
    await expect(page.getByText(`Loading ${list.noun}…`, { exact: true })).toBeVisible();
    await expect(page.getByRole('heading', { name: list.empty, exact: true })).toHaveCount(0);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText(`Could not load ${list.noun}`);
    mode = 'rows';
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.locator(list.rows).first()).toBeVisible();
    const count = await page.locator(list.rows).count();
    const rowPositions = () =>
      page.locator(list.rows).evaluateAll((rows) =>
        rows.map((row) => {
          const { x, y, width, height } = row.getBoundingClientRect();
          return { x: x + scrollX, y: y + scrollY, width, height };
        })
      );
    const positions = await rowPositions();
    gate = deferred();
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toBeVisible({
      timeout: 10000
    });
    await expect(page.locator(list.rows)).toHaveCount(count);
    expect(await rowPositions()).toEqual(positions);
    mode = 'failure';
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText('Showing the last received results');
    await expect(page.locator(list.rows)).toHaveCount(count);
    expect(await rowPositions()).toEqual(positions);
    mode = 'rows';
    gate = deferred();
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toBeVisible();
    expect(await rowPositions()).toEqual(positions);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByText(`Refreshing ${list.noun}…`, { exact: true })).toHaveCount(0);
    expect(await rowPositions()).toEqual(positions);
    mode = 'empty';
    await expect(page.getByRole('heading', { name: list.empty, exact: true })).toBeVisible({
      timeout: 10000
    });
    await page.getByRole('button', { name: list.filter, exact: true }).click();
    await expect(
      page.getByRole('heading', { name: `No matching ${list.noun}`, exact: true })
    ).toBeVisible();
    await expect(page.getByRole('button', { name: list.filter, exact: true })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    if (list.endpoint === 'proposals') {
      await page.getByRole('button', { name: 'all', exact: true }).click();
      await page.getByLabel('Cycle', { exact: true }).selectOption('cycle-1');
      await expect(
        page.getByRole('heading', { name: 'No matching proposals', exact: true })
      ).toBeVisible();
    }
  });
}

test('empty PR outcomes keep delivery history stationary during refresh and retry', async ({
  page,
  isMobile
}) => {
  let gate: ReturnType<typeof deferred> | null = null;
  let fail = false;
  await page.route('**/api/prs?*', async (route) => {
    await gate?.promise;
    if (fail) await route.fulfill({ status: 503, json: { error: 'Synthetic list failure' } });
    else {
      const response = await route.fetch();
      const result = await response.json();
      result.items = [];
      await route.fulfill({ json: result });
    }
  });
  await login(page);
  await openNavigation(page, 'Pull requests', !!isMobile);
  const empty = page.getByRole('heading', { name: 'Room for your next improvement.', exact: true });
  await expect(empty).toBeVisible();
  const target = page.locator('.published-panel .task-row').first();
  await target.scrollIntoViewIfNeeded();
  const position = (await target.boundingBox())!;
  const documentPosition = () =>
    target.evaluate((row) => {
      const { x, y, width, height } = row.getBoundingClientRect();
      return { x: x + scrollX, y: y + scrollY, width, height };
    });
  const savedPosition = await documentPosition();
  gate = deferred();
  try {
    await expect(page.getByText('Refreshing pull requests…', { exact: true })).toBeVisible({
      timeout: 10000
    });
    expect(await target.boundingBox()).toEqual(position);
    await expect(empty).toBeVisible();
    fail = true;
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toContainText('Showing the last received results');
    await expect(empty).toBeVisible();
    expect(await documentPosition()).toEqual(savedPosition);
    fail = false;
    gate = deferred();
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect(page.getByText('Refreshing pull requests…', { exact: true })).toBeVisible();
    expect(await documentPosition()).toEqual(savedPosition);
    gate.resolve();
    gate = null;
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByText('Refreshing pull requests…', { exact: true })).toHaveCount(0);
    await expect(empty).toBeVisible();
    expect(await documentPosition()).toEqual(savedPosition);
  } finally {
    gate?.resolve();
  }
});

test('polling keeps the second task under the same mouse position', async ({ page, isMobile }) => {
  let gate: ReturnType<typeof deferred> | null = null;
  await page.route('**/api/tasks?*', async (route) => {
    await gate?.promise;
    await route.continue();
  });
  await login(page);
  await openNavigation(page, 'Task queue', !!isMobile);
  const target = page.locator('.task-row').nth(1);
  await expect(target).toBeVisible();
  await target.scrollIntoViewIfNeeded();
  await expect(target).toBeInViewport({ ratio: 1 });
  const title = await target.locator('strong').innerText();
  const position = (await target.boundingBox())!;
  gate = deferred();
  try {
    await expect(page.getByText('Refreshing tasks…', { exact: true })).toBeVisible({
      timeout: 10000
    });
    expect(await target.boundingBox()).toEqual(position);
    // Use the original coordinates: a locator click would follow a shifted row.
    await page.mouse.click(position.x + position.width / 2, position.y + 12);
    await expect(
      page.getByRole('dialog').getByRole('heading', { name: title, exact: true })
    ).toBeVisible();
  } finally {
    gate.resolve();
    gate = null;
  }
});

test('header and empty discovery actions share eligibility and prevent duplicate pending controls', async ({
  page
}) => {
  let restriction = 'continuous';
  const writes: string[] = [];
  const gate = deferred();
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    const snapshot: Snapshot = await response.json();
    snapshot.configured = true;
    snapshot.audit_configured = true;
    snapshot.status = restriction;
    snapshot.control.paused = restriction !== 'continuous';
    snapshot.active_tasks = restriction === 'task' ? 1 : 0;
    snapshot.cycle_active = ['execution', 'audit'].includes(restriction);
    snapshot.active_cycle_mode =
      restriction === 'audit' ? 'audit' : restriction === 'execution' ? 'execution' : null;
    snapshot.tasks = [];
    snapshot.attention_tasks = [];
    snapshot.counts = {};
    await route.fulfill({ json: snapshot });
  });
  await page.route('**/api/control/*', async (route) => {
    writes.push(new URL(route.request().url()).pathname);
    await gate.promise;
    restriction = 'continuous';
    await route.fulfill({ json: { paused: false } });
  });
  await login(page);
  for (const value of ['continuous', 'task', 'execution', 'audit', 'idle']) {
    restriction = value;
    const run = page.getByRole('button', { name: 'Run once', exact: true });
    if (value === 'idle') {
      await expect(run).toBeEnabled({ timeout: 10000 });
      await expect(
        page.getByRole('button', { name: 'Discover opportunities', exact: true })
      ).toBeEnabled();
      await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeEnabled();
    } else {
      await expect(page.locator('.status-value')).toHaveText(value, { timeout: 10000 });
      await expect(run).toBeDisabled();
      const discover = page.getByRole('button', { name: 'Discover opportunities', exact: true });
      if (value === 'task') await expect(discover).toHaveCount(0);
      else await expect(discover).toBeDisabled();
      await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeDisabled();
    }
  }
  expect(writes).toEqual([]);
  await page.getByRole('button', { name: 'Discover opportunities', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Starting run…', exact: true })).toHaveCount(2);
  for (const button of await page.getByRole('button', { name: 'Starting run…', exact: true }).all())
    await expect(button).toBeDisabled();
  await page
    .getByRole('button', { name: 'Starting run…', exact: true })
    .first()
    .dispatchEvent('click');
  gate.resolve();
  await expect(page.getByRole('button', { name: 'Run once', exact: true })).toBeDisabled();
  expect(writes).toEqual(['/api/control/cycle']);
});
