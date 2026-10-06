import { expect } from '@playwright/test';
import {
  configurationFixture,
  deferred,
  login,
  openNavigation,
  test,
  trackWrites,
  unsandboxed
} from './synthetic';

test('a revision conflict offers to discard the draft and reload the saved configuration in place @responsive', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);
  const branch = page.getByLabel('Default branch', { exact: true });
  await expect(branch).toHaveValue('fixture-main');
  const reads = state.reads;
  const alert = page.getByRole('alert').and(page.locator('.settings-feedback'));
  const reload = page.getByRole('button', { name: 'Discard edits and reload' });

  state.saveElsewhere({ default_branch: 'external-main' });
  await branch.fill('stale-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  await expect(branch).toHaveValue('stale-main');
  expect(state.reads).toBe(reads);
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(alert).toHaveCount(0);
  await expect(branch).toHaveValue('fixture-main');
  await branch.fill('stale-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  await expect(reload).toBeVisible();
  expect(state.writes).toHaveLength(2);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  const saved = page.getByRole('status').and(page.locator('.settings-feedback'));
  state.loadGate = deferred();
  await reload.click();
  await expect.poll(() => state.reads).toBe(reads + 1);
  await branch.fill('typed-during-reload');
  state.loadGate.resolve();
  state.loadGate = null;
  await expect(page.getByRole('button', { name: 'Save configuration' })).toBeEnabled();
  await expect(branch).toHaveValue('typed-during-reload');
  await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible();
  await expect(saved).toHaveCount(0);
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toContainText('Synthetic save conflict');
  expect(state.writes).toHaveLength(3);

  await reload.click();
  await expect(branch).toHaveValue('external-main');
  await expect(saved).toHaveText('Edits discarded. Saved configuration reloaded.');
  await expect(alert).toHaveCount(0);
  await expect(reload).toHaveCount(0);
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
  expect(state.reads).toBe(reads + 2);
  expect(state.writes).toHaveLength(3);

  await branch.fill('current-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes.at(-1)!.config).toEqual({ default_branch: 'current-main' });
  expect(state.saved!.default_branch).toBe('current-main');

  state.failSave = true;
  await branch.fill('paused-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(alert).toHaveText('Synthetic save conflict');
  await expect(reload).toHaveCount(0);
  await expect(branch).toHaveValue('paused-main');
  expect(state.writes).toHaveLength(5);
  expect(state.reads).toBe(reads + 2);
  state.failSave = false;

  await page.route('**/api/model-catalog', (route) =>
    route.fulfill({ status: 503, json: { error: 'Synthetic catalog outage' } })
  );
  await page.getByRole('button', { name: 'Load Codex models' }).click();
  await expect(alert).toContainText('Synthetic catalog outage');
  await expect(reload).toHaveCount(0);
});

test('configuration keeps drafts and catalogs across views, discards locally, and refreshes clean values', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page, { snapshot: unsandboxed });
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
  for (const name of ['Check connection', 'Check audit connection']) {
    const check = page.getByRole('button', { name, exact: true });
    await expect(check).toBeDisabled();
    await expect(check).toHaveAccessibleDescription(/Save or discard edits before checking\./);
  }
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
  state.saveElsewhere({ default_branch: 'externally-saved-main' });
  await navigate('Configuration');
  await expect(branch).toHaveValue('externally-saved-main');
  expect(state.reads).toBe(2);
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

test('setup checklist distinguishes entered, saved, checked, stale and failed states without starting work', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page, {
    unconfigured: true,
    snapshot: (snapshot) => {
      snapshot.configured = false;
      snapshot.audit_configured = false;
      snapshot.cycles = [];
      snapshot.counts = {};
    }
  });
  const writes = trackWrites(page);
  const navigate = (name: string) => openNavigation(page, name, !!isMobile);
  const step = (id: string) => page.locator(`[data-step="${id}"]`);
  const badge = (id: string) => step(id).locator('.badge');
  await login(page);
  await expect(page.getByRole('region', { name: 'Choose your first run' })).toContainText(
    'Your first move: an audit.'
  );
  await expect(page.getByRole('button', { name: 'Set up your project' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Run an audit', exact: true })).toBeDisabled();
  expect(writes).toEqual([]);
  await navigate('Configuration');
  await expect(page.getByRole('heading', { name: 'Setup checklist' })).toBeVisible();
  await expect(page.getByLabel('Setup status meanings')).toContainText('Saved');
  await expect(badge('repository')).toHaveText('Incomplete');
  await expect(badge('routes')).toHaveText('Incomplete');
  await expect(badge('verification')).toHaveText('None');
  await expect(badge('preflight')).toHaveText('Not checked');
  await expect(badge('baseline')).toHaveText('Optional · not run');
  await expect(badge('choose')).toHaveText('Not run');
  await expect(step('choose')).toContainText(
    'Audit: saved configuration incomplete. Run once: saved configuration incomplete.'
  );
  await expect(badge('sandbox')).toHaveText(/^Proven · /);
  await expect(step('sandbox')).toContainText('containment checks passed from inside a sandbox');
  await expect(page.getByText('1 of 7 steps saved, checked or run')).toBeVisible();

  await page.getByLabel('Repository path').fill('/fixture/entered');
  await page.getByLabel('GitHub repository').fill('fixture/entered');
  await expect(badge('repository')).toHaveText('Entered, not saved');
  await expect(badge('preflight')).toHaveText('Unsaved edits');
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Load Codex models' }).click();

  for (const role of ['Orchestrator', 'Discovery agents', 'Proposal reviewers']) {
    await page.getByLabel(`${role} model`, { exact: true }).fill('gpt-6-astra');
    await page.getByLabel(`${role} reasoning effort`, { exact: true }).selectOption('medium');
  }
  await expect(badge('routes')).toHaveText('Entered, not saved');
  await expect(step('routes')).toContainText(
    '3 of 10 execution routes selected (3 of 3 audit routes). 3 match a loaded catalog'
  );
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect(badge('repository')).toHaveText('Saved');
  await expect(badge('routes')).toHaveText('Saved, audit routes only');
  await expect(step('routes')).toContainText('Run once also needs the code reviewer');
  await expect(badge('verification')).toHaveText('None');
  await expect(badge('preflight')).toHaveText('Not checked');
  expect(state.writes).toHaveLength(1);

  for (const role of [
    'Code reviewer',
    'XS execution',
    'S execution',
    'M execution',
    'L execution',
    'XL execution',
    'Repair'
  ]) {
    await page.getByLabel(`${role} model`, { exact: true }).fill('gpt-6-astra');
    await page.getByLabel(`${role} reasoning effort`, { exact: true }).selectOption('medium');
  }
  await expect(badge('routes')).toHaveText('Entered, not saved');
  await expect(step('routes')).toContainText(
    '10 of 10 execution routes selected (3 of 3 audit routes). 10 match a loaded catalog'
  );
  expect(state.writes).toHaveLength(1);
  await page.getByRole('textbox', { name: /^Verification commands/ }).fill('fixture entered test');
  await expect(badge('verification')).toHaveText('Entered, not saved');
  expect(state.writes).toHaveLength(1);

  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect(badge('repository')).toHaveText('Saved');
  await expect(badge('routes')).toHaveText('Saved');
  await expect(badge('verification')).toHaveText('Saved');
  await expect(badge('preflight')).toHaveText('Not checked');
  await expect(step('preflight')).toContainText('does not prove repository push permission');
  await expect(page.getByText('4 of 7 steps saved, checked or run')).toBeVisible();
  expect(state.writes).toHaveLength(2);

  await step('preflight').getByRole('button', { name: 'Open the execution check' }).click();
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeFocused();
  expect(state.checks).toEqual([]);
  await page.keyboard.press('Enter');
  await expect(badge('preflight')).toHaveText(/^Passed · execution · /);
  await expect.poll(() => state.checks).toEqual(['execution']);
  await expect(page.getByText('5 of 7 steps saved, checked or run')).toBeVisible();

  // A reload of the same saved revision keeps the check it passed.
  const result = await badge('preflight').innerText();
  await navigate('Overview');
  const refresh = page.waitForResponse('**/api/config');
  await navigate('Configuration');
  await refresh;
  await expect(page.getByRole('button', { name: 'Check connection', exact: true })).toBeEnabled();
  await expect(badge('preflight')).toHaveText(result);
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0);
  await expect(page.getByText('5 of 7 steps saved, checked or run')).toBeVisible();
  expect(state.checks).toEqual(['execution']);

  await page.getByLabel('Default branch', { exact: true }).fill('edited-main');
  await expect(badge('preflight')).toHaveText('Unsaved edits');
  await expect(step('preflight')).toContainText(
    'covered the previously saved values, not these edits'
  );
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(badge('preflight')).toHaveText(/^Passed · execution · /);
  await page.getByLabel('Default branch', { exact: true }).fill('resaved-main');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  await expect(badge('preflight')).toHaveText('Not checked');

  state.failCheck = true;
  await step('preflight').getByRole('button', { name: 'Open the audit check' }).click();
  await expect(page.getByRole('button', { name: 'Check audit connection' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(badge('preflight')).toHaveText(/^Failed · audit · /);
  await expect(step('preflight')).toContainText('Synthetic connection check failed');
  await expect(page.getByRole('alert')).toHaveText('Synthetic connection check failed');
  expect(state.checks).toEqual(['execution', 'audit']);

  await expect(badge('baseline')).toHaveText('Optional · not run');
  expect(state.baselines).toHaveLength(0);
  await step('baseline').getByRole('button', { name: 'Open the baseline check' }).click();
  await expect(page.locator('#check-baseline')).toBeFocused();
  expect(state.baselines).toHaveLength(0);
  await page.locator('#check-baseline').click();
  await expect(page.getByRole('alertdialog')).toContainText(
    'Run the saved verification commands on a disposable clone of the remote default branch?'
  );
  await page.getByRole('button', { name: 'Back' }).click();
  expect(state.baselines).toHaveLength(0);
  await page.locator('#check-baseline').click();
  await page.getByRole('button', { name: 'Run baseline check' }).click();
  await expect.poll(() => state.baselines.length).toBe(1);
  expect(state.baselines[0]).toEqual({ expected_revision: state.revision });
  await expect(page.getByRole('button', { name: 'Cancel baseline check' })).toBeVisible();
  await page.getByRole('button', { name: 'Cancel baseline check' }).click();
  await expect.poll(() => state.baselines.length).toBe(2);
  expect(state.baselines[1]).toEqual({ cancel: expect.stringContaining('/cancel') });
  expect(writes.map((write) => write.path)).toEqual([
    '/api/model-catalog',
    '/api/config',
    '/api/config',
    '/api/doctor',
    '/api/config',
    '/api/doctor',
    '/api/baseline-checks',
    '/api/baseline-checks/synthetic-check/cancel'
  ]);
});
