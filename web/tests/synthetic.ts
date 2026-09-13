/**
 * Deterministic SYNTHETIC fixtures shared by the browser tests and the screenshot
 * captures. Nothing here is copied from a real run: every string is labelled synthetic,
 * repository names are fixture names, and commit identities are repeated letters.
 */
import { expect, type Page, type Route } from '@playwright/test';
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

export const token = 'browser-test-operator-token-32-characters';
export const SYNTHETIC = 'Synthetic browser-test verdict text. Not a real reviewer statement.';
export const now = new Date().toISOString();
export const A = 'a'.repeat(40);
export const B = 'b'.repeat(40);
export const Z = 'z'.repeat(40);

/** An ISO timestamp a fixed number of minutes before the fixture's `now`. */
export function minutesAgo(minutes: number): string {
  return new Date(Date.parse(now) - minutes * 60_000).toISOString();
}

export async function login(page: Page) {
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
}

/** Records every non-GET call so read-only interaction can be proven read-only. */
export function trackWrites(page: Page) {
  const writes: { path: string; method: string }[] = [];
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname;
    if (path.startsWith('/api/') && request.method() !== 'GET')
      writes.push({ path, method: request.method() });
  });
  return writes;
}

export function reviewer(slot: string, over: Partial<ReviewerVerdict> = {}): ReviewerVerdict {
  return {
    reviewer: slot,
    state: 'recorded',
    decision: 'accepted',
    reason: `${SYNTHETIC} (${slot})`,
    note: null,
    ...over
  };
}

export function reviewRound(over: Partial<ReviewRoundEvidence> = {}): ReviewRoundEvidence {
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

export function command(
  name: string,
  state: CommandState,
  revision: string | null = B
): CommandResult {
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

export function taskEvidence(id: string, over: Partial<TaskEvidence> = {}): TaskEvidence {
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

export function proposalEvidence(
  id: string,
  over: Partial<ProposalEvidence> = {}
): ProposalEvidence {
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

export function runEvidence(
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
export function proposalRow(
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

export async function serveProposals(page: Page, rows: ProposalRow[]) {
  await page.route('**/api/proposals?*', async (route: Route) => {
    const status = new URL(route.request().url()).searchParams.get('status') ?? 'all';
    const items = status === 'all' ? rows : rows.filter((row) => row.decision === status);
    const counts: Record<string, number> = { all: rows.length };
    for (const row of rows) counts[row.decision] = (counts[row.decision] ?? 0) + 1;
    await route.fulfill({ json: { items, next_cursor: null, counts } });
  });
}

export async function openNavigation(page: Page, name: string, mobile: boolean) {
  if (mobile) await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page.getByRole('navigation').getByRole('button', { name, exact: true }).click();
}

export async function openProposalEvidence(
  page: Page,
  index: number,
  testInfo: { project: { name: string } }
) {
  await openNavigation(page, 'Proposals', testInfo.project.name === 'mobile');
  await page
    .locator('.proposal-card')
    .nth(index)
    .getByRole('button', { name: 'Inspect decision evidence' })
    .click();
}
