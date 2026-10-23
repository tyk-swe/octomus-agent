import { expect } from '@playwright/test';
import {
  login,
  nextPoll,
  openNavigation,
  patchState,
  proposalEvidence,
  runEvidence,
  test
} from './synthetic';

type RequestRecord = {
  id: string;
  repository: string;
  title: string;
  description: string;
  target: string;
  status: string;
  created_at: string;
  updated_at: string;
  assessment: {
    cycle_id: string;
    proposal_id: string | null;
    mode: string;
    decision: string;
    reason: string;
    at: string;
  } | null;
  task_id: string | null;
};

const now = '2026-10-13T12:00:00+00:00';
const record = (overrides: Partial<RequestRecord>): RequestRecord => ({
  id: 'request-1',
  repository: 'fixture/project',
  title: 'Add the feature',
  description: 'Create feature.txt',
  target: 'main',
  status: 'pending',
  created_at: now,
  updated_at: now,
  assessment: null,
  task_id: null,
  ...overrides
});

test('operator requests collect, guard and settle work @responsive', async ({ page, isMobile }) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.clock.install();
  const view = {
    repository: 'fixture/project',
    default_branch: 'main',
    pending: [record({ id: 'request-1' })],
    recent: [
      record({
        id: 'request-0',
        title: 'Earlier work',
        status: 'accepted',
        task_id: 'task-1',
        assessment: {
          cycle_id: 'cycle-1',
          proposal_id: 'request-request-0',
          mode: 'execution',
          decision: 'accepted',
          reason: 'Repository evidence supports the operator request.',
          at: now
        }
      })
    ],
    pending_limit: 20,
    seed_limit: 10,
    targets: [{ branch: 'octomus/existing', number: 42, title: 'An existing improvement' }] as {
      branch: string;
      number: number;
      title: string;
    }[]
  };
  let planning = false,
    failCreate = false;
  const createBodies: unknown[] = [],
    actions: string[] = [];
  await patchState(page, (snapshot) => {
    snapshot.control.paused = true;
    snapshot.control.mode = 'paused';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = planning;
    snapshot.active_cycle_mode = planning ? 'execution' : null;
    snapshot.pending_requests = view.pending.length;
  });
  await page.route('**/api/requests**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (request.method() === 'GET') {
      await route.fulfill({ json: view });
      return;
    }
    if (request.method() === 'POST' && url.pathname === '/api/requests') {
      const body = request.postDataJSON() as RequestRecord;
      createBodies.push(body);
      if (failCreate) {
        await route.fulfill({
          status: 409,
          json: {
            error:
              'At most 20 requests can wait for planning; withdraw one or let a cycle assess them'
          }
        });
        return;
      }
      const created = record({
        id: `request-${view.pending.length + view.recent.length + 1}`,
        title: body.title,
        description: body.description,
        target: body.target
      });
      view.pending.push(created);
      await route.fulfill({ status: 201, json: created });
      return;
    }
    const action = url.pathname.match(/^\/api\/requests\/([^/]+)\/([^/]+)$/);
    if (request.method() === 'POST' && action) {
      actions.push(`${action[1]}/${action[2]}`);
      const found = view.pending.find((entry) => entry.id === action[1]);
      if (found) {
        found.status = 'withdrawn';
        view.pending = view.pending.filter((entry) => entry.id !== found.id);
        view.recent.unshift(found);
      }
      await route.fulfill({ json: { ok: true } });
      return;
    }
    await route.fallback();
  });
  await page.route('**/api/proposals?*', async (route) => {
    await route.fulfill({
      json: {
        items: [
          {
            id: 'request-0',
            title: 'Earlier work',
            problem: 'Missing behavior',
            evidence: ['README.md'],
            benefit: 'Useful',
            category: 'features',
            target: 'main',
            tier: 'M',
            scope: 'one file',
            dependencies: [],
            prompt: 'Implement it',
            decision: 'accepted',
            reason: 'Repository evidence supports the operator request.',
            problem_key: 'earlier-work',
            relevant_paths: ['README.md'],
            reconsiders: [],
            content_revision: 1,
            cycle: 1,
            cycle_id: 'cycle-1',
            mode: 'execution'
          }
        ],
        counts: { accepted: 1 },
        next_cursor: null
      }
    });
  });
  await login(page);
  await openNavigation(page, 'Requests', isMobile);
  await expect(page.getByRole('heading', { level: 1, name: 'Requests' })).toBeVisible();
  await expect(page.getByText('Add the feature', { exact: true })).toBeVisible();
  await expect(page.locator('[aria-label="History pagination"]')).toBeHidden();

  // Housekeeping can change eligible PRs without changing cycles or pending requests.
  const target = page.locator('#request-target');
  await target.selectOption('octomus/existing');
  view.targets = [{ branch: 'octomus/new', number: 43, title: 'Newly discovered improvement' }];
  await nextPoll(page);
  await expect(target.locator('option', { hasText: '#43' })).toHaveCount(1);
  await expect(target.locator('option', { hasText: '#42' })).toHaveCount(0);
  await expect(target).toHaveValue('main');

  await page.locator('#request-title').fill('New fixture feature');
  await page.locator('#request-description').fill('Create the next fixture output.');
  await page.getByRole('button', { name: 'Request work' }).click();
  await expect
    .poll(() => createBodies)
    .toEqual([
      {
        title: 'New fixture feature',
        description: 'Create the next fixture output.',
        target: 'main'
      }
    ]);
  await expect(
    page.getByText('Request saved. The next planning cycle will assess it.')
  ).toBeVisible();

  planning = true;
  await nextPoll(page);
  const withdraw = page.getByRole('button', { name: 'Withdraw' }).first();
  await expect(withdraw).toBeDisabled();
  await expect(
    page.getByText('Withdraw is available once planning finishes.').first()
  ).toBeVisible();
  planning = false;
  await nextPoll(page);
  await expect(withdraw).toBeEnabled();
  await withdraw.click();
  await expect.poll(() => actions).toEqual(['request-1/withdraw']);

  const taskCall = page.waitForRequest('**/api/tasks/task-1');
  await page.getByRole('button', { name: 'Open task' }).click();
  await taskCall;
  await page.getByRole('button', { name: 'Close task details' }).click();

  failCreate = true;
  await page.locator('#request-title').fill('Too many');
  await page.locator('#request-description').fill('One more than the limit.');
  await page.getByRole('button', { name: 'Request work' }).click();
  await expect(page.getByRole('alert').first()).toContainText('At most 20 requests');

  await openNavigation(page, 'Proposals', isMobile);
  await expect(page.getByText('Operator request', { exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test('request assessments inspect seeded proposals and dismissed cycles @responsive', async ({
  page,
  isMobile
}) => {
  const assessment = (mode: string, proposal: string | null) => ({
    cycle_id: `${mode}-cycle`,
    proposal_id: proposal,
    mode,
    decision: 'rejected',
    reason: 'The target pull request is closed.',
    at: now
  });
  await page.route('**/api/requests', async (route) => {
    await route.fulfill({
      json: {
        repository: 'fixture/project',
        default_branch: 'main',
        pending: [
          record({
            id: 'audit-dismissed',
            title: 'Audit dismissal',
            assessment: assessment('audit', null)
          }),
          record({
            id: 'seeded',
            title: 'Seeded assessment',
            assessment: assessment('audit', 'request-seeded')
          })
        ],
        recent: [
          record({
            id: 'execution-dismissed',
            title: 'Execution dismissal',
            status: 'rejected',
            assessment: assessment('execution', null)
          })
        ],
        pending_limit: 20,
        seed_limit: 10,
        targets: []
      }
    });
  });
  await page.route('**/api/cycles/*/evidence', async (route) => {
    const mode = route.request().url().includes('audit-cycle') ? 'audit' : 'execution';
    await route.fulfill({
      json: runEvidence(
        { proposals: [proposalEvidence('unrelated'), proposalEvidence('request-seeded')] },
        { id: `${mode}-cycle`, mode }
      )
    });
  });
  await login(page);
  await openNavigation(page, 'Requests', isMobile);
  for (const [title, proposal] of [
    ['Audit dismissal', 'unrelated'],
    ['Execution dismissal', 'unrelated'],
    ['Seeded assessment', 'request-seeded']
  ]) {
    await page
      .locator('article')
      .filter({ has: page.getByRole('heading', { name: title, exact: true }) })
      .getByRole('button', { name: 'Inspect run' })
      .click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByLabel('Proposal', { exact: true })).toHaveValue(proposal);
    await expect(dialog.getByText(/The selected proposal .* is not recorded/)).toHaveCount(0);
    await page.getByRole('button', { name: 'Close run evidence' }).click();
  }
});
