/**
 * Browser coverage for the authenticated "Inspect run" experience.
 *
 * Every fixture here is explicitly synthetic. No reviewer text is copied from the Day 1
 * rehearsal report, and no test asserts a live model identity or a fresh GitHub
 * observation, because the feature does not claim either.
 */
import { test, expect, type Page, type Route } from '@playwright/test';
import type {
  CommandResult,
  CommandState,
  ProposalEvidence,
  ProposalRow,
  ReviewerVerdict,
  ReviewRoundEvidence,
  RunEvidenceV1,
  TaskEvidence
} from '../src/lib/types';

const token = 'browser-test-operator-token-32-characters';
const SYNTHETIC = 'Synthetic browser-test verdict text. Not a real reviewer statement.';
const now = new Date().toISOString();
const A = 'a'.repeat(40);
const B = 'b'.repeat(40);
const Z = 'z'.repeat(40);

async function login(page: Page) {
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
}

/** Records every non-GET call so read-only interaction can be proven read-only. */
function trackWrites(page: Page) {
  const writes: { path: string; method: string }[] = [];
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname;
    if (path.startsWith('/api/') && request.method() !== 'GET')
      writes.push({ path, method: request.method() });
  });
  return writes;
}

function reviewer(slot: string, over: Partial<ReviewerVerdict> = {}): ReviewerVerdict {
  return {
    reviewer: slot,
    state: 'recorded',
    decision: 'accepted',
    reason: `${SYNTHETIC} (${slot})`,
    note: null,
    ...over
  };
}

function reviewRound(over: Partial<ReviewRoundEvidence> = {}): ReviewRoundEvidence {
  return {
    session_id: 'synthetic-review-session',
    revision: B,
    comparison_base: A,
    created_at: now,
    completed: true,
    summary_present: true,
    matches_output_revision: true,
    findings: [],
    ...over
  };
}

function command(name: string, state: CommandState, revision: string | null = B): CommandResult {
  return {
    command: name,
    state,
    results_recorded: state === 'no_result' ? 0 : 1,
    latest_success: state === 'no_result' ? null : state !== 'failed',
    latest_revision: revision,
    latest_created_at: state === 'no_result' ? null : now,
    matches_output_revision: state === 'no_result' ? null : revision === B
  };
}

function taskEvidence(id: string, over: Partial<TaskEvidence> = {}): TaskEvidence {
  return {
    id,
    cycle_id: 'synthetic-cycle',
    proposal_id: 'synthetic-proposal',
    status: 'published',
    branch: `tyk/${id}`,
    attempts: 0,
    blocked_reason: null,
    error_recorded: false,
    created_at: now,
    updated_at: now,
    revisions: { source: A, comparison_base: A, default_branch: 'main', output: B },
    sessions: [
      {
        id: 'synthetic-executor-session',
        role: 'executor',
        status: 'completed',
        requested_route: { backend: 'codex', model: 'gpt-6-astra', effort: 'medium' },
        started_at: now
      }
    ],
    latest_review: {
      rounds_recorded: 1,
      latest: reviewRound(),
      clean: true,
      clean_at_output_revision: true
    },
    required_commands: {
      state: 'recorded',
      commands: [command('cargo test', 'passed')],
      all_passed_at_output_revision: true
    },
    pull_request: {
      number: 77,
      url: 'https://github.com/fixture/project/pull/77',
      source: 'recorded_task_reference'
    },
    gaps: [],
    ...over
  };
}

function proposalEvidence(id: string, over: Partial<ProposalEvidence> = {}): ProposalEvidence {
  return {
    id,
    title: `Synthetic proposal ${id}`,
    target: 'main',
    tier: 'S',
    category: 'correctness',
    problem: 'Synthetic recorded problem statement for browser tests.',
    benefit: 'Synthetic recorded benefit statement for browser tests.',
    scope: 'Synthetic recorded scope statement for browser tests.',
    evidence: ['src/main.rs: synthetic fixture reference'],
    final_decision: 'accepted',
    final_reason: 'Synthetic recorded final rationale for browser tests.',
    reviewer_verdicts: [reviewer('adversary-a'), reviewer('adversary-b')],
    linked_tasks: [],
    gaps: [],
    ...over
  };
}

function runEvidence(
  over: Partial<RunEvidenceV1> = {},
  cycle: Partial<RunEvidenceV1['cycle']> = {}
): RunEvidenceV1 {
  return {
    schema_version: 1,
    generated_at: now,
    kind: 'recorded_review_check_evidence',
    review_required_before_sharing: true,
    review_requirement:
      'Requires review before sharing. This is a private operator export of saved records, not a public-safe or publication-approved artifact.',
    limitations: ['Synthetic limitation recorded for browser tests.', 'Deferred is not rejected.'],
    cycle: {
      id: 'synthetic-cycle',
      number: 7,
      mode: 'execution',
      status: 'completed',
      started_at: now,
      completed_at: now,
      repository: 'fixture/project',
      grounding_revision: A,
      planning: {
        status: 'completed',
        planning_finished: true,
        proposal_count: 1,
        decisions: { accepted: 1 },
        creates_execution_queue: true,
        error_recorded: false,
        reviewer_batches_saved: 2
      },
      ...cycle
    },
    proposals: [],
    gaps: [],
    ...over
  };
}

/** One synthetic proposal summary row, as the paged proposal history returns them. */
function proposalRow(
  id: string,
  cycleId: string,
  cycleNumber: number,
  over: Partial<ProposalRow> = {}
): ProposalRow {
  return {
    id,
    cycle: cycleNumber,
    cycle_id: cycleId,
    mode: 'execution',
    content_revision: 1,
    title: `Synthetic proposal ${id}`,
    problem: 'Synthetic recorded problem statement for browser tests.',
    benefit: 'Synthetic recorded benefit statement for browser tests.',
    scope: 'Synthetic recorded scope statement for browser tests.',
    target: 'main',
    tier: 'S',
    category: 'correctness',
    evidence: ['src/main.rs: synthetic fixture reference'],
    dependencies: [],
    prompt: 'Synthetic execution prompt for browser tests.',
    decision: 'accepted',
    reason: 'Synthetic recorded decision reason for browser tests.',
    ...over
  };
}

async function serveProposals(page: Page, rows: ProposalRow[]) {
  await page.route('**/api/proposals?*', async (route: Route) => {
    await route.fulfill({
      json: { items: rows, next_cursor: null, counts: { all: rows.length } }
    });
  });
}

async function openProposalEvidence(
  page: Page,
  index: number,
  testInfo: { project: { name: string } }
) {
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Proposals', exact: true })
    .click();
  await page
    .locator('.proposal-card')
    .nth(index)
    .getByRole('button', { name: 'Inspect decision evidence' })
    .click();
}

test('inspect run reports recorded reviewer roles, review, checks and delivery, then hands off to the task', async ({
  page
}) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const writes = trackWrites(page);
  await login(page);

  // Entry point: the existing latest-cycle panel on the overview.
  await page.getByRole('button', { name: 'Inspect run' }).click();
  const evidence = page.getByRole('dialog');
  await expect(evidence.getByRole('heading', { name: 'Execution cycle #001' })).toBeVisible();
  await expect(page.getByText('Requires review before sharing')).toBeVisible();
  await expect(page.getByText('Planning completion is not task completion').first()).toBeVisible();
  await expect(page.getByText('This is not a replayed event timeline')).toBeVisible();

  // The recorded sequence, in order, for the one published proposal of this run.
  await page.getByLabel('Proposal', { exact: true }).selectOption('task-reviewed');
  await expect(page.getByRole('heading', { name: 'Proposal evidence' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Reviewer A — Problem and value' })).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'Reviewer B — Feasibility and risk' })
  ).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Final decision' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Associated task' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Review and check evidence' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Recorded pull request' })).toBeVisible();

  // Real server-normalized statuses, not recomputed booleans.
  await expect(page.getByText('Both reviewers recorded accepted', { exact: true })).toBeVisible();
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByText('All 1 passed at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByText('At the recorded output commit', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open recorded PR #12' })).toHaveAttribute(
    'href',
    'https://github.com/fixture/project/pull/12'
  );
  await expect(page.getByText('not a fresh observation of the GitHub head').first()).toBeVisible();
  await expect(page.getByText('requested routes, not verified runtime identity')).toBeVisible();
  await expect(page.getByText('Limitations recorded in this evidence (9)')).toBeVisible();

  // Deep inspection is handed to TaskDetail; the panels never stack.
  await page.getByRole('button', { name: 'Open task details' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(1);
  await expect(page.getByRole('heading', { name: 'Recorded result' })).toBeVisible();
  await expect(page.getByText(B.slice(0, 12), { exact: true })).toBeVisible();
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open on GitHub' })).toHaveAttribute(
    'href',
    'https://github.com/fixture/project/pull/12'
  );
  // Operating limits are preserved, moved beneath the result into a disclosure.
  await expect(page.getByText('Effective operating limits')).toBeVisible();
  await page.getByText('Effective operating limits').click();
  await expect(page.getByText('Daily admissions:')).toBeVisible();

  expect(writes).toEqual([]);
  expect(errors).toEqual([]);
});

test('accepted, rejected, deferred and missing reviewer assessments each render honestly', async ({
  page
}, testInfo) => {
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
  await openProposalEvidence(page, 0, testInfo);

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

  // A missing or malformed slot is never filled in from the other reviewer or the decision.
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

test('equal proposal identities in different cycles resolve to their own recorded evidence', async ({
  page
}, testInfo) => {
  const requested: string[] = [];
  await serveProposals(page, [
    proposalRow('shared-proposal-id', 'first-cycle', 11),
    proposalRow('shared-proposal-id', 'second-cycle', 12)
  ]);
  for (const [cycle, label] of [
    ['first-cycle', 'first'],
    ['second-cycle', 'second']
  ]) {
    await page.route(`**/api/cycles/${cycle}/evidence`, async (route: Route) => {
      requested.push(cycle);
      await route.fulfill({
        json: runEvidence(
          {
            proposals: [
              proposalEvidence('shared-proposal-id', {
                title: `Recorded only in the ${label} cycle`,
                final_reason: `Synthetic recorded rationale saved in the ${label} cycle.`
              })
            ]
          },
          { id: cycle, number: label === 'first' ? 11 : 12 }
        )
      });
    });
  }
  await login(page);

  await openProposalEvidence(page, 0, testInfo);
  await expect(page.getByRole('heading', { name: 'Execution cycle #011' })).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'Recorded only in the first cycle' })
  ).toBeVisible();
  await expect(page.getByText('saved in the first cycle')).toBeVisible();
  await page.getByRole('button', { name: 'Close run evidence' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);

  await page
    .locator('.proposal-card')
    .nth(1)
    .getByRole('button', { name: 'Inspect decision evidence' })
    .click();
  await expect(page.getByRole('heading', { name: 'Execution cycle #012' })).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'Recorded only in the second cycle' })
  ).toBeVisible();
  await expect(page.getByText('saved in the second cycle')).toBeVisible();
  await expect(page.getByText('saved in the first cycle')).toHaveCount(0);
  expect(requested).toEqual(['first-cycle', 'second-cycle']);
});

test('an audit-only recorded output links no task by design', async ({ page }, testInfo) => {
  await serveProposals(page, [
    proposalRow('audit-only-proposal', 'synthetic-cycle', 4, { mode: 'audit' })
  ]);
  await page.route('**/api/cycles/synthetic-cycle/evidence', async (route: Route) => {
    await route.fulfill({
      json: runEvidence(
        { proposals: [proposalEvidence('audit-only-proposal')] },
        {
          number: 4,
          mode: 'audit',
          planning: {
            status: 'completed',
            planning_finished: true,
            proposal_count: 1,
            decisions: { accepted: 1 },
            creates_execution_queue: false,
            error_recorded: false,
            reviewer_batches_saved: 2
          }
        }
      )
    });
  });
  const writes = trackWrites(page);
  await login(page);
  await openProposalEvidence(page, 0, testInfo);

  await expect(page.getByRole('heading', { name: 'Audit cycle #004' })).toBeVisible();
  await expect(page.getByText('audit cycles never create an execution queue')).toBeVisible();
  await expect(page.getByText('No linked task', { exact: true })).toBeVisible();
  await expect(page.getByText('Audit-only outcome')).toBeVisible();
  await expect(
    page.getByText('No task is linked, so no review or check evidence exists')
  ).toBeVisible();
  await expect(page.getByText('No task is linked, so no delivery is recorded')).toBeVisible();
  await expect(page.getByText('Acceptance is not execution.')).toHaveCount(0);
  expect(writes).toEqual([]);
});

test('stale check evidence and an incomplete review are never reported as clean', async ({
  page
}, testInfo) => {
  await serveProposals(page, [proposalRow('stale-evidence-proposal', 'synthetic-cycle', 7)]);
  await page.route('**/api/cycles/synthetic-cycle/evidence', async (route: Route) => {
    await route.fulfill({
      json: runEvidence({
        proposals: [
          proposalEvidence('stale-evidence-proposal', {
            linked_tasks: [
              taskEvidence('stale-evidence-task', {
                status: 'blocked',
                blocked_reason: 'verification_timeout',
                error_recorded: true,
                latest_review: {
                  rounds_recorded: 2,
                  latest: reviewRound({
                    revision: Z,
                    completed: false,
                    summary_present: false,
                    matches_output_revision: false
                  }),
                  clean: false,
                  clean_at_output_revision: false
                },
                required_commands: {
                  state: 'recorded',
                  commands: [
                    command('cargo test', 'passed_at_other_revision', Z),
                    command('cargo clippy', 'failed', B)
                  ],
                  all_passed_at_output_revision: false
                },
                pull_request: null,
                gaps: [
                  'An output revision is recorded without a clean latest review at that revision.'
                ]
              })
            ]
          })
        ]
      })
    });
  });
  await login(page);
  await openProposalEvidence(page, 0, testInfo);

  await expect(page.getByText('Review incomplete', { exact: true })).toBeVisible();
  await expect(page.getByText('Not the recorded output commit', { exact: true })).toBeVisible();
  await expect(page.getByText('Never completed · no summary recorded · 0 findings')).toBeVisible();
  await expect(page.getByText('zero findings proves nothing')).toBeVisible();
  await expect(page.getByText('0 of 2 passed at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByText('Passed at another revision', { exact: true })).toBeVisible();
  await expect(page.getByText('Failed', { exact: true })).toBeVisible();
  await expect(page.getByText('No pull request recorded', { exact: true })).toBeVisible();
  await expect(page.getByText('Clean at the output commit', { exact: true })).toHaveCount(0);
  await expect(page.getByText('Recorded gaps for this task (1)')).toBeVisible();
});

test('a review round with zero findings is not clean when incomplete or unsummarised', async ({
  page
}, testInfo) => {
  const rounds = [
    {
      session_id: 'round-blank',
      revision: Z,
      comparison_base: A,
      created_at: now,
      result: { completed: true, summary: '   ', findings: [] }
    },
    {
      session_id: 'round-incomplete',
      revision: Z,
      comparison_base: A,
      created_at: now,
      result: { completed: false, summary: 'Synthetic partial review.', findings: [] }
    },
    {
      session_id: 'round-clean',
      revision: B,
      comparison_base: A,
      created_at: now,
      result: { completed: true, summary: 'Synthetic complete review summary.', findings: [] }
    }
  ];
  await page.route('**/api/tasks/task-reviewed', async (route: Route) => {
    const body = await (await route.fetch()).json();
    body.reviews = rounds;
    await route.fulfill({ json: body });
  });
  await login(page);
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Task queue', exact: true })
    .click();
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await page.getByRole('tab', { name: /Reviews/ }).click();

  await expect(page.getByText('A round is clean only when it completed')).toBeVisible();
  await expect(page.getByText('No summary recorded', { exact: true })).toBeVisible();
  await expect(page.getByText('Incomplete', { exact: true })).toBeVisible();
  // Exactly one of three zero-finding rounds is clean.
  await expect(page.getByText('Clean', { exact: true })).toHaveCount(1);
  // Historical cleanliness and matching the output commit stay separate facts.
  await expect(page.getByText('Not the recorded output commit', { exact: true })).toHaveCount(2);
  await expect(page.getByText('At the recorded output commit', { exact: true })).toHaveCount(1);
  await expect(page.getByText('No review summary was recorded for this round.')).toBeVisible();
});

test('slow evidence loads finish before polling resumes, including after a failed refresh', async ({
  page
}) => {
  await page.clock.install();
  const requests: Route[] = [];
  await page.route('**/api/cycles/cycle-1/evidence', (route) => {
    requests.push(route);
  });
  await login(page);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  await expect.poll(() => requests.length).toBe(1);
  await page.clock.runFor(11000);
  expect(requests).toHaveLength(1);
  await expect(page.getByText('Loading recorded evidence…')).toBeVisible();

  const payload = runEvidence({
    proposals: [
      proposalEvidence('synthetic-proposal', {
        linked_tasks: [taskEvidence('recovered-task', { attempts: 2 })]
      })
    ]
  });
  await requests[0].fulfill({ json: payload });
  await expect(page.getByRole('heading', { name: 'Execution cycle #007' })).toBeVisible();
  await expect(page.getByText('2 retries', { exact: true })).toBeVisible();

  await page.clock.runFor(10000);
  await expect.poll(() => requests.length).toBe(2);
  await page.clock.runFor(11000);
  expect(requests).toHaveLength(2);
  await requests[1].fulfill({ status: 503, json: { error: 'Synthetic refresh failure' } });
  await expect(page.getByRole('alert')).toContainText('Synthetic refresh failure');

  await page.clock.runFor(10000);
  await expect.poll(() => requests.length).toBe(3);
  await requests[2].fulfill({ json: payload });
  await expect(page.getByText('Synthetic refresh failure')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Execution cycle #007' })).toBeVisible();
  await page.getByRole('button', { name: 'Close run evidence' }).click();
  await page.clock.runFor(20000);
  expect(requests).toHaveLength(3);
});

test('delayed evidence never overwrites a newer selection and is dropped on close', async ({
  page
}, testInfo) => {
  let release: () => void = () => {};
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  let calls = 0;
  await serveProposals(page, [
    proposalRow('slow-proposal', 'slow-cycle', 21),
    proposalRow('fast-proposal', 'fast-cycle', 22)
  ]);
  await page.route('**/api/cycles/slow-cycle/evidence', async (route: Route) => {
    calls += 1;
    if (calls === 1) await blocked;
    await route.fulfill({
      json: runEvidence(
        {
          proposals: [
            proposalEvidence('slow-proposal', { title: 'Recorded in the abandoned selection' })
          ]
        },
        { id: 'slow-cycle', number: 21 }
      )
    });
  });
  await page.route('**/api/cycles/fast-cycle/evidence', async (route: Route) => {
    await route.fulfill({
      json: runEvidence(
        {
          proposals: [
            proposalEvidence('fast-proposal', { title: 'Recorded in the current selection' })
          ]
        },
        { id: 'fast-cycle', number: 22 }
      )
    });
  });
  await login(page);

  await openProposalEvidence(page, 0, testInfo);
  await expect(page.getByText('Loading recorded evidence…')).toBeVisible();
  await page.getByRole('button', { name: 'Close run evidence' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);

  await page
    .locator('.proposal-card')
    .nth(1)
    .getByRole('button', { name: 'Inspect decision evidence' })
    .click();
  await expect(
    page.getByRole('heading', { name: 'Recorded in the current selection' })
  ).toBeVisible();
  release();
  await page.waitForTimeout(1000);
  // The abandoned response must never replace the newer run.
  await expect(page.getByRole('heading', { name: 'Execution cycle #022' })).toBeVisible();
  await expect(page.getByText('Recorded in the abandoned selection')).toHaveCount(0);
});

test('a delayed task evidence response is discarded after the selected task changes', async ({
  page
}, testInfo) => {
  let release: () => void = () => {};
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  let calls = 0;
  await page.route('**/api/cycles/cycle-1/evidence', async (route: Route) => {
    calls += 1;
    if (calls === 1) await blocked;
    await route.fulfill({ json: await (await route.fetch()).json() });
  });
  await login(page);
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Task queue', exact: true })
    .click();

  // Published task first: its evidence request is held open.
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await expect(page.getByRole('heading', { name: 'Recorded result' })).toBeVisible();
  await page.getByRole('button', { name: 'Close task details' }).click();

  // A different task in the same cycle resolves immediately.
  await page.getByRole('button', { name: /Complete the repository setup flow/ }).click();
  await expect(page.getByText('No review recorded', { exact: true })).toBeVisible();
  await expect(page.getByText('No pull request recorded', { exact: true })).toBeVisible();
  release();
  await page.waitForTimeout(1000);
  // The earlier task's delivery evidence must not appear under this task.
  await expect(page.getByText('No pull request recorded', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open on GitHub' })).toHaveCount(0);
});

test('long recorded text is previewed and kept in full behind a disclosure', async ({
  page
}, testInfo) => {
  const problem = 'Recorded problem evidence. '.repeat(60) + 'Problem ending';
  const reason = 'Recorded reviewer reasoning. '.repeat(60) + 'Reason ending';
  const rationale = 'Recorded final rationale. '.repeat(60) + 'Rationale ending';
  await serveProposals(page, [proposalRow('long-text-proposal', 'synthetic-cycle', 7)]);
  await page.route('**/api/cycles/synthetic-cycle/evidence', async (route: Route) => {
    await route.fulfill({
      json: runEvidence({
        proposals: [
          proposalEvidence('long-text-proposal', {
            problem,
            final_reason: rationale,
            reviewer_verdicts: [
              reviewer('adversary-a', { reason }),
              reviewer('adversary-b', { reason: SYNTHETIC })
            ]
          })
        ]
      })
    });
  });
  await login(page);
  await openProposalEvidence(page, 0, testInfo);

  await expect(
    page.getByText(`Show the full problem (${problem.length} characters)`)
  ).toBeVisible();
  await expect(page.getByText(problem, { exact: true })).toBeHidden();
  await page.getByText(`Show the full problem (${problem.length} characters)`).click();
  await expect(page.getByText(problem, { exact: true })).toBeVisible();

  await page.getByText(`Show the full reviewer reason (${reason.length} characters)`).click();
  await expect(page.getByText(reason, { exact: true })).toBeVisible();

  await page.getByText(`Show the full final rationale (${rationale.length} characters)`).click();
  await expect(page.getByText(rationale, { exact: true })).toBeVisible();
});

test('read-only inspection and the evidence download perform no writes', async ({
  page,
  context
}) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const writes = trackWrites(page);
  await context.grantPermissions(['clipboard-read', 'clipboard-write']).catch(() => {});
  await login(page);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  await expect(page.getByRole('heading', { name: 'Execution cycle #001' })).toBeVisible();

  await page.getByLabel('Proposal', { exact: true }).selectOption('task-reviewed');
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
  // Nothing in the panel offers upload or public sharing.
  await expect(page.getByRole('button', { name: /share/i })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /upload/i })).toHaveCount(0);

  await page.getByRole('button', { name: 'Open task details' }).click();
  for (const tab of ['Sessions', 'Reviews', 'Verification', 'Activity', 'Overview'])
    await page.getByRole('tab', { name: new RegExp(tab) }).click();
  await expect(page.getByRole('heading', { name: 'Recorded result' })).toBeVisible();
  // Owner actions remain present as explicit, separate actions.
  await expect(page.getByRole('link', { name: 'Open PR #12' })).toBeVisible();

  expect(writes).toEqual([]);
  expect(errors).toEqual([]);
});

test('retained evidence is labelled stale after a failed refresh and cleared on 401', async ({
  page
}, testInfo) => {
  let evidenceCalls = 0;
  let taskCalls = 0;
  let mode: 'ok' | 'fail' | 'unauthorized' = 'ok';
  await page.route('**/api/cycles/cycle-1/evidence', async (route: Route) => {
    evidenceCalls += 1;
    if (mode === 'fail')
      await route.fulfill({ status: 503, json: { error: 'Service returned 503' } });
    else if (mode === 'unauthorized')
      await route.fulfill({ status: 401, json: { error: 'Unauthorized' } });
    else await route.fulfill({ json: await (await route.fetch()).json() });
  });
  // Each task poll reports a new revision so the evidence key changes and refetches.
  await page.route('**/api/tasks/task-reviewed', async (route: Route) => {
    const body = await (await route.fetch()).json();
    taskCalls += 1;
    body.updated_at = new Date(Date.now() + taskCalls * 1000).toISOString();
    await route.fulfill({ json: body });
  });
  await login(page);
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Task queue', exact: true })
    .click();
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();

  mode = 'fail';
  await expect(page.getByText('Retained · stale')).toBeVisible({ timeout: 15000 });
  // Retained records stay visible, explicitly labelled rather than silently refreshed.
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await expect(page.getByText('which may now be out of date')).toBeVisible();

  mode = 'unauthorized';
  // A rejected session must not keep showing the previous session's records.
  await expect(page.getByText('Clean at the output commit', { exact: true })).toHaveCount(0, {
    timeout: 15000
  });
  await expect(page.getByLabel('Operator access token')).toBeVisible();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(
    page.getByText('Explain the local development workflow', { exact: true })
  ).toHaveCount(0);
  expect(evidenceCalls).toBeGreaterThan(1);
});

test('multiple task matches require a choice; unavailable tasks and saved replacements stay explicit', async ({
  page
}) => {
  await page.route('**/api/cycles/cycle-1/evidence', (route) =>
    route.fulfill({
      json: runEvidence(
        {
          proposals: [
            proposalEvidence('rediscover-unverified-identity', {
              linked_tasks: [
                taskEvidence('missing-task'),
                taskEvidence('task-blocked', { status: 'cancelled' })
              ]
            })
          ]
        },
        { id: 'cycle-1' }
      )
    })
  );
  await page.route('**/api/tasks/missing-task', (route) =>
    route.fulfill({ status: 404, json: { error: 'Synthetic missing task' } })
  );
  await page.route('**/api/tasks/task-blocked', async (route) => {
    const body = await (await route.fetch()).json();
    await route.fulfill({ json: { ...body, status: 'cancelled', superseded_by: ['task-active'] } });
  });
  const writes = trackWrites(page);
  await login(page);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  await expect(page.getByText('2 matches', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Open task details' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Open the superseded task' })).toHaveCount(0);
  await page.getByRole('button', { name: /missing-task.*retries/ }).click();
  await page.getByRole('button', { name: 'Open task details' }).click();
  await expect(page.getByRole('heading', { name: 'Task unavailable' })).toBeVisible();
  await expect(page.getByText('Loading task…')).toHaveCount(0);
  await page.getByRole('button', { name: 'Close task details' }).click();
  await page.getByRole('button', { name: 'Inspect run' }).click();
  await page.getByRole('button', { name: /task-blocked.*retries/ }).click();
  await expect(page.getByText('Supersession relationships are not included')).toBeVisible();
  await page.getByRole('button', { name: 'Open task details' }).click();
  await expect(page.getByText('Replacement tasks:')).toBeVisible();
  await page.getByRole('button', { name: 'task-act', exact: true }).click();
  await expect(page.getByRole('dialog')).toHaveCount(1);
  await expect(
    page.getByRole('heading', { name: 'Complete the repository setup flow' })
  ).toBeVisible();
  expect(writes).toEqual([]);
});

test('unknown output comparisons, unconfigured checks and duplicate verdicts do not become passes', async ({
  page
}) => {
  await page.route('**/api/cycles/cycle-1/evidence', (route) =>
    route.fulfill({
      json: runEvidence(
        {
          proposals: [
            proposalEvidence('unknown-output', {
              reviewer_verdicts: [
                reviewer('adversary-a', {
                  state: 'duplicate',
                  reason: null,
                  note: 'Synthetic duplicate entries agree, but remain duplicated.'
                }),
                reviewer('adversary-b', {
                  note: 'Synthetic batch is unconfirmed by a completed session.'
                })
              ],
              linked_tasks: [
                taskEvidence('unknown-output-task', {
                  revisions: {
                    source: A,
                    comparison_base: A,
                    default_branch: 'main',
                    output: null
                  },
                  latest_review: {
                    clean: true,
                    clean_at_output_revision: false,
                    rounds_recorded: 1,
                    latest: reviewRound({ matches_output_revision: null })
                  },
                  required_commands: {
                    state: 'not_configured',
                    commands: [],
                    all_passed_at_output_revision: false
                  },
                  pull_request: {
                    number: null,
                    url: 'https://github.com/fixture/repo/pull/77',
                    source: 'saved_task_pr_reference'
                  }
                })
              ]
            })
          ]
        },
        { id: 'cycle-1' }
      )
    })
  );
  await login(page);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  await expect(page.getByText('Clean, output revision unknown', { exact: true })).toBeVisible();
  await expect(page.getByText('Clean at another revision', { exact: true })).toHaveCount(0);
  await expect(page.getByText('No output commit recorded', { exact: true })).toBeVisible();
  await expect(page.getByText('No checks configured', { exact: true })).toBeVisible();
  await expect(page.getByText('Recorded PR · number unavailable', { exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Open recorded PR', exact: true })).toHaveAttribute(
    'href',
    'https://github.com/fixture/repo/pull/77'
  );
  await expect(page.getByText('accepted (duplicated)', { exact: true })).toBeVisible();
  await expect(page.getByText('Several verdicts are recorded')).toBeVisible();
  await expect(page.getByText('Reviewer evidence incomplete', { exact: true })).toBeVisible();
  await expect(
    page.getByText('Synthetic batch is unconfirmed by a completed session.')
  ).toBeVisible();
});

test('refresh preserves proposal choice, disclosures and scroll, and never substitutes a removed selection', async ({
  page
}) => {
  await page.clock.install();
  let calls = 0;
  let remove = false;
  const reason = 'Synthetic long reasoning. '.repeat(80);
  await page.route('**/api/cycles/cycle-1/evidence', (route) => {
    calls++;
    const proposals = [
      proposalEvidence('first'),
      proposalEvidence('selected', { final_reason: reason })
    ];
    return route.fulfill({
      json: runEvidence({ proposals: remove ? [proposals[0]] : proposals }, { id: 'cycle-1' })
    });
  });
  await login(page);
  await page.clock.runFor(12000);
  expect(calls).toBe(0);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  await page.getByLabel('Proposal', { exact: true }).selectOption('selected');
  const summary = page.getByText(`Show the full final rationale (${reason.length} characters)`);
  await summary.click();
  const content = page.locator('.evidence-dialog .detail-content');
  const scroll = await content.evaluate((el) => el.scrollTop);
  await page.clock.runFor(10000);
  await expect.poll(() => calls).toBe(2);
  await expect(page.getByLabel('Proposal', { exact: true })).toHaveValue('selected');
  await expect(page.getByText(reason, { exact: true })).toBeVisible();
  expect(await content.evaluate((el) => el.scrollTop)).toBe(scroll);
  remove = true;
  await page.clock.runFor(10000);
  await expect(page.getByText('The selected proposal (selected) is not recorded')).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'Synthetic proposal first', exact: true })
  ).toHaveCount(0);
  await page.getByLabel('Proposal', { exact: true }).selectOption('first');
  await expect(
    page.getByRole('heading', { name: 'Synthetic proposal first', exact: true })
  ).toBeVisible();
});

test('refresh never substitutes another task when a selected match disappears', async ({
  page
}) => {
  await page.clock.install();
  let remove = false;
  await page.route('**/api/cycles/cycle-1/evidence', (route) =>
    route.fulfill({
      json: runEvidence(
        {
          proposals: [
            proposalEvidence('multiple', {
              linked_tasks: remove
                ? [taskEvidence('remaining', { pull_request: null })]
                : [taskEvidence('selected'), taskEvidence('remaining', { pull_request: null })]
            })
          ]
        },
        { id: 'cycle-1' }
      )
    })
  );
  await login(page);
  await page.getByRole('button', { name: 'Inspect run' }).click();
  await page.getByRole('button', { name: /selected.*retries/ }).click();
  await expect(page.getByRole('link', { name: 'Open recorded PR #77' })).toBeVisible();
  remove = true;
  await page.clock.runFor(10000);
  await expect(page.getByText('The selected task (selected) is no longer recorded')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Open task details' })).toHaveCount(0);
  await expect(page.getByText('No pull request recorded', { exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: /remaining.*retries/ }).click();
  await expect(page.getByText('No pull request recorded', { exact: true })).toBeVisible();
});

test('slow task evidence finishes before another task revision triggers a refresh', async ({
  page
}, testInfo) => {
  await page.clock.install();
  const requests: Route[] = [];
  let taskReads = 0;
  await page.route('**/api/cycles/cycle-1/evidence', (route) => {
    requests.push(route);
  });
  await page.route('**/api/tasks/task-reviewed', async (route) => {
    const body = await (await route.fetch()).json();
    await route.fulfill({ json: { ...body, updated_at: `synthetic-revision-${++taskReads}` } });
  });
  await login(page);
  if (testInfo.project.name === 'mobile')
    await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page
    .getByRole('navigation')
    .getByRole('button', { name: 'Task queue', exact: true })
    .click();
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  await expect.poll(() => requests.length).toBe(1);
  await page.clock.runFor(12000);
  expect(requests).toHaveLength(1);
  expect(taskReads).toBe(1);
  await requests[0].fulfill({ json: await (await requests[0].fetch()).json() });
  await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
  await page.clock.runFor(4000);
  await expect.poll(() => requests.length).toBe(2);
  await expect(page.getByText('Retained · stale')).toBeVisible();
  await page.getByRole('button', { name: 'Close task details' }).click();
  await requests[1].fulfill({ json: await (await requests[1].fetch()).json() });
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

for (const source of ['run', 'task', 'state'] as const) {
  test(`a ${source} 401 clears the private session, including delayed responses and next-login selection`, async ({
    page
  }) => {
    await page.clock.install();
    let reject = false;
    let delayed: Route | undefined;
    const endpoint =
      source === 'run'
        ? 'cycles/cycle-1/evidence'
        : source === 'task'
          ? 'tasks/task-reviewed'
          : 'state';
    await page.route(`**/api/${endpoint}`, async (route) => {
      if (reject)
        await route.fulfill({ status: 401, json: { error: 'Synthetic expired session' } });
      else await route.fulfill({ json: await (await route.fetch()).json() });
    });
    await login(page);
    await page.getByRole('button', { name: 'Inspect run' }).click();
    await page.getByLabel('Proposal', { exact: true }).selectOption('task-reviewed');
    await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
    if (source === 'task') await page.getByRole('button', { name: 'Open task details' }).click();
    if (source !== 'state')
      await page.route('**/api/state', (route) => {
        delayed = route;
      });
    reject = true;
    await page.clock.runFor(12000);
    await expect(page.getByLabel('Operator access token')).toBeVisible();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(page.getByText('Clean at the output commit', { exact: true })).toHaveCount(0);
    if (delayed) await delayed.fulfill({ json: await (await delayed.fetch()).json() });
    if (source !== 'state') await page.unroute('**/api/state');
    reject = false;
    await page.getByLabel('Operator access token').fill(token);
    await page.getByRole('button', { name: 'Open dashboard' }).click();
    await expect(page.getByRole('button', { name: 'Inspect run' })).toBeVisible();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await page.getByRole('button', { name: 'Inspect run' }).click();
    await expect(page.getByRole('heading', { name: 'Execution cycle #001' })).toBeVisible();
  });
}
