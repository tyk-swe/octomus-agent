import { expect, type Route } from '@playwright/test';
import {
  B,
  login,
  openProposalEvidence,
  proposalEvidence,
  proposalRow,
  reviewer,
  runEvidence,
  serveProposals,
  taskEvidence,
  test,
  token,
  trackWrites
} from './synthetic';

test('inspect run reports recorded reviewer roles, review, checks and delivery, exports without writes, then hands off to the task', async ({
  page,
  context
}) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const writes = trackWrites(page);
  await context.grantPermissions(['clipboard-read', 'clipboard-write']).catch(() => {});
  await login(page);

  await page.getByRole('button', { name: 'Inspect run' }).click();
  const evidence = page.getByRole('dialog');
  await expect(evidence.getByRole('heading', { name: 'Execution cycle #001' })).toBeVisible();
  await expect(page.getByText('Requires review before sharing')).toBeVisible();
  await expect(page.getByText('Planning completion is not task completion').first()).toBeVisible();
  await expect(page.getByText('This is not a replayed event timeline')).toBeVisible();

  await expect(page.getByText('Planning complete', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Work complete')).toHaveCount(0);
  await page.getByLabel('Proposal', { exact: true }).selectOption('task-reviewed');
  await expect(
    page.getByRole('heading', { name: 'Explain the local development workflow' })
  ).toBeVisible();
  await expect(
    page.getByRole('heading', { name: /^Reviewer A\s+Problem and value$/ })
  ).toBeVisible();
  await expect(
    page.getByRole('heading', { name: /^Reviewer B\s+Feasibility and risk$/ })
  ).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Final decision' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Linked task' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Review at the output commit' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Configured checks' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Recorded pull request' })).toBeVisible();
  const cards = page.locator('.reviewer-card');
  await expect(cards).toHaveCount(2);
  const [first, second] = await Promise.all([
    cards.nth(0).boundingBox(),
    cards.nth(1).boundingBox()
  ]);
  if (page.viewportSize()!.width >= 700) {
    expect(first!.y).toBe(second!.y);
    expect(Math.abs(first!.width - second!.width)).toBeLessThan(0.5);
  } else {
    expect(second!.y).toBeGreaterThan(first!.y + first!.height - 1);
  }

  await expect(page.getByText('Both reviewers recorded accepted', { exact: true })).toBeVisible();
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByText('All 1 passed at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByText('At the recorded output commit', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open recorded PR #12' })).toHaveAttribute(
    'href',
    'https://github.com/fixture/project/pull/12'
  );
  await expect(page.getByText('not a fresh observation of the GitHub head').first()).toBeVisible();
  await page.getByText('requested routes, not verified runtime identity').click();
  await expect(page.getByText('Saved routes are the routes that were requested')).toBeVisible();
  await page.getByText('Limitations recorded in this evidence (9)').click();
  await expect(page.getByText('Deferred is not rejected.').first()).toBeVisible();
  await page.getByRole('button', { name: 'Copy cycle ID' }).click();
  await expect(page.locator('.dialog-footer .muted')).toHaveText(/copied/i);

  const download = page.waitForEvent('download');
  await page
    .getByRole('button', { name: 'Download evidence JSON (review before sharing)' })
    .click();
  const artifact = await download;
  expect(artifact.suggestedFilename()).toBe('octomus-run-evidence-cycle-1.json');
  const stream = await artifact.createReadStream();
  let contents = '';
  for await (const chunk of stream!) contents += chunk.toString();
  const exported = JSON.parse(contents);
  const response = await page.request.get('/api/cycles/cycle-1/evidence', {
    headers: { Authorization: `Bearer ${token}` }
  });
  const saved = await response.json();
  expect({ ...exported, generated_at: null }).toEqual({ ...saved, generated_at: null });
  expect(exported.review_required_before_sharing).toBe(true);
  expect(JSON.stringify(exported)).not.toContain('verification_commands');
  for (const proposal of exported.proposals)
    for (const task of proposal.linked_tasks) {
      expect(task).not.toHaveProperty('config');
      expect(task).not.toHaveProperty('workspace');
      expect(task).not.toHaveProperty('verification');
    }
  await expect(page.getByRole('button', { name: /share/i })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /upload/i })).toHaveCount(0);

  await page.getByRole('button', { name: 'Open task details' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(1);
  await expect(page.getByRole('heading', { name: 'Recorded result' })).toBeVisible();
  await expect(page.getByText(B.slice(0, 12), { exact: true })).toBeVisible();
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open on GitHub' })).toHaveAttribute(
    'href',
    'https://github.com/fixture/project/pull/12'
  );
  await expect(page.getByRole('link', { name: 'Open PR #12' })).toBeVisible();
  await page.getByText('Effective operating limits').click();
  await expect(page.getByText('Daily admissions:')).toBeVisible();
  for (const tab of ['Sessions', 'Reviews', 'Verification', 'Activity', 'Overview'])
    await page.getByRole('tab', { name: new RegExp(tab) }).click();
  await expect(page.getByRole('heading', { name: 'Recorded result' })).toBeVisible();

  expect(writes).toEqual([]);
  expect(errors).toEqual([]);
});

test('accepted, rejected, deferred and missing reviewer assessments each render honestly', async ({
  page,
  isMobile
}) => {
  const rows = [
    proposalRow('accepted-proposal', 'synthetic-cycle', 7),
    proposalRow('rejected-proposal', 'synthetic-cycle', 7, { decision: 'rejected' }),
    proposalRow('deferred-proposal', 'synthetic-cycle', 7, { decision: 'deferred' }),
    proposalRow('unreviewed-proposal', 'synthetic-cycle', 7)
  ];
  await serveProposals(page, rows);
  await page.route('**/api/cycles/synthetic-cycle/evidence', async (route: Route) => {
    await route.fulfill({
      json: runEvidence({
        proposals: [
          proposalEvidence('accepted-proposal', {
            linked_tasks: [taskEvidence('accepted-task')]
          }),
          proposalEvidence('rejected-proposal', {
            final_decision: 'rejected',
            final_reason: 'Synthetic recorded rejection rationale for browser tests.',
            reviewer_verdicts: [
              reviewer('adversary-a', { decision: 'rejected' }),
              reviewer('adversary-b', { decision: 'rejected' })
            ]
          }),
          proposalEvidence('deferred-proposal', {
            final_decision: 'deferred',
            final_reason: 'Synthetic recorded deferral rationale for browser tests.',
            reviewer_verdicts: [
              reviewer('adversary-a', { decision: 'deferred' }),
              reviewer('adversary-b', { decision: 'accepted' })
            ]
          }),
          proposalEvidence('unreviewed-proposal', {
            reviewer_verdicts: [
              reviewer('adversary-a', {
                state: 'missing',
                decision: null,
                reason: null,
                note: 'No saved assessment batch is attributed to adversary-a.'
              }),
              reviewer('adversary-b', {
                state: 'malformed',
                decision: null,
                reason: null,
                note: 'Batch 1 is malformed: saved batch does not contain a recorded assessment list.'
              })
            ],
            gaps: [
              'The proposal was accepted but no task is linked in this cycle; acceptance is not execution.'
            ]
          })
        ],
        gaps: []
      })
    });
  });
  const writes = trackWrites(page);
  await login(page);
  await openProposalEvidence(page, 0, !!isMobile);

  const picker = page.getByLabel('Proposal', { exact: true });
  await expect(page.getByText('Both reviewers recorded accepted', { exact: true })).toBeVisible();

  await picker.selectOption('rejected-proposal');
  await expect(page.getByText('Both reviewers recorded rejected', { exact: true })).toBeVisible();
  await expect(
    page.getByText('No task is linked, which matches a rejected decision.')
  ).toBeVisible();

  await picker.selectOption('deferred-proposal');
  await expect(page.getByText('Reviewers disagree', { exact: true })).toBeVisible();
  await expect(page.getByText('Recorded verdicts differ')).toBeVisible();
  await expect(page.getByText('Deferred is not rejected.').first()).toBeVisible();

  await picker.selectOption('unreviewed-proposal');
  await expect(page.getByText('Reviewer evidence incomplete', { exact: true })).toBeVisible();
  await expect(page.getByText('No verdict recorded', { exact: true })).toBeVisible();
  await expect(page.getByText('Malformed batch', { exact: true })).toBeVisible();
  await expect(page.getByText("No usable verdict is recorded in this reviewer's slot")).toHaveCount(
    2
  );
  await expect(
    page.getByText('No saved assessment batch is attributed to adversary-a.')
  ).toBeVisible();
  await expect(page.getByText('No linked task', { exact: true })).toBeVisible();
  await expect(
    page.getByText('no task is linked in this cycle. Acceptance is not execution.')
  ).toBeVisible();

  expect(writes).toEqual([]);
});

test('a failed initial evidence request explains itself and offers a retry', async ({ page }) => {
  let fail = true;
  await page.route('**/api/cycles/cycle-1/evidence', async (route: Route) => {
    if (fail) await route.fulfill({ status: 503, json: { error: 'Synthetic evidence outage' } });
    else await route.fulfill({ json: await (await route.fetch()).json() });
  });
  await login(page);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  const dialog = page.getByRole('dialog');
  await expect(
    dialog.getByRole('heading', { name: 'Recorded evidence could not be loaded' })
  ).toBeVisible();
  await expect(dialog).toContainText('Synthetic evidence outage');
  fail = false;
  await dialog.getByRole('button', { name: 'Try again' }).click();
  await expect(dialog.getByRole('heading', { name: 'Execution cycle #001' })).toBeVisible();
});
