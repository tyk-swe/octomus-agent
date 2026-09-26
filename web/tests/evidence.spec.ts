/**
 * The evidence display mapping (src/lib/evidence.ts), checked directly without a page.
 * These rules decide how saved records are named on every surface, so each case pins
 * one rule rather than one screen.
 */
import { test, expect } from '@playwright/test';
import {
  checksVerdict,
  commandExplanation,
  decisionCounts,
  outcomeVerdict,
  planningVerdict,
  plural,
  prVerdict,
  reviewerAgreement,
  reviewerSlot,
  reviewRoundBadge,
  reviewVerdict,
  revisionMatchLabel,
  roundRevisionLabel,
  taskIcon,
  UNKNOWN_VERDICT,
  verdictBadge
} from '../src/lib/evidence';
import { ACTIVE_STATUSES, type ReviewEvidence, type ReviewRoundEvidence } from '../src/lib/types';
import { A, B, command, reviewer, reviewRound, taskEvidence } from './synthetic';

test.skip(({ isMobile }) => isMobile, 'Pure mapping rules run once, on the desktop project.');

test('decision counts list only decisions that occurred, in a fixed order', () => {
  // The cycle summary reports every decision, including those with 0 proposals.
  expect(decisionCounts({ candidate: 0, deferred: 0, rejected: 0, accepted: 3 })).toEqual([
    { decision: 'accepted', count: 3, tone: 'clean' }
  ]);
  expect(decisionCounts({ accepted: 0, rejected: 0, deferred: 0, candidate: 0 })).toEqual([]);
  expect(decisionCounts({})).toEqual([]);
  // Known decisions keep their order; unknown words that occurred follow, sorted. Being
  // unknown never drops a word; only a 0 count does.
  expect(
    decisionCounts({ zeta: 1, candidate: 2, deferred: 1, unknown: 0, rejected: 4, alpha: 5 })
  ).toEqual([
    { decision: 'rejected', count: 4, tone: 'failed' },
    { decision: 'deferred', count: 1, tone: 'blocked' },
    { decision: 'candidate', count: 2, tone: 'cancelled' },
    { decision: 'alpha', count: 5, tone: 'cancelled' },
    { decision: 'zeta', count: 1, tone: 'cancelled' }
  ]);
});

test('an idle cycle is a finished planning pass that accepted nothing, in either mode', () => {
  // The service saves `idle` when execution planning queues no task, and when an audit
  // accepts no recommendation. Both are successful outcomes, never a failure or a
  // claim that work was completed.
  const execution = planningVerdict({ status: 'idle', mode: 'execution' });
  expect(execution).toMatchObject({ label: 'Planning complete · nothing accepted', tone: 'clean' });
  expect(execution.detail).toContain('An empty task set is a successful idle cycle');
  expect(execution.detail).toContain('planning completion is not task completion');
  expect(planningVerdict({ status: 'idle', mode: 'audit' })).toEqual({
    label: 'Audit complete · nothing accepted',
    tone: 'clean',
    detail: 'The audit finished and no recommendation was accepted.'
  });
  // A status the dashboard does not know is still named verbatim and never toned.
  expect(planningVerdict({ status: 'paused', mode: 'execution' })).toMatchObject({
    label: 'Planning paused',
    tone: ''
  });
});

test('a task outcome takes the tone of its finished status; any other status is still running', () => {
  expect(outcomeVerdict({ status: 'published' })).toEqual({
    label: 'published',
    tone: 'clean',
    detail: 'Recorded as published. Published describes delivery, not merge.'
  });
  expect(outcomeVerdict({ status: 'failed' }).tone).toBe('failed');
  expect(outcomeVerdict({ status: 'cancelled' }).tone).toBe('cancelled');
  expect(
    outcomeVerdict({ status: 'blocked', blocked_reason: 'repair_limit', error_recorded: true })
  ).toEqual({
    label: 'blocked',
    tone: 'blocked',
    detail: 'The saved task status, verbatim. Blocked reason: repair limit. An error is recorded.'
  });
  // Queued, active and unknown statuses never read as settled, let alone clean.
  for (const status of ['queued', ...ACTIVE_STATUSES, 'unrecognised'])
    expect(outcomeVerdict({ status })).toMatchObject({ label: status, tone: 'running' });
});

test('task rows show one icon per status family, and every active status shares one', () => {
  expect(taskIcon('published')).toBe('check');
  expect(taskIcon('blocked')).toBe('alert');
  expect(taskIcon('failed')).toBe('alert');
  expect(taskIcon('queued')).toBe('clock');
  for (const status of ACTIVE_STATUSES) expect(taskIcon(status)).toBe('activity');
  // A cancelled task and any status the dashboard does not know show the plain work icon.
  expect(taskIcon('cancelled')).toBe('code');
  expect(taskIcon('unrecognised')).toBe('code');
});

test('reviewer slots are named by position, and an unknown slot stays verbatim', () => {
  expect(reviewerSlot('adversary-a')).toBe('Reviewer A');
  expect(reviewerSlot('adversary-b')).toBe('Reviewer B');
  expect(reviewerSlot('adversary-c')).toBe('adversary-c');
});

/** Task evidence whose latest review is `review`, with `latest` overridden when given. */
function withReview(review: Partial<ReviewEvidence>, latest: Partial<ReviewRoundEvidence> = {}) {
  return taskEvidence('synthetic-task', {
    latest_review: {
      rounds_recorded: 1,
      latest: reviewRound(latest),
      clean: false,
      clean_at_output_revision: false,
      ...review
    }
  });
}

test('review standing is clean only when the server reports it clean at the output commit', () => {
  const finding = { title: 'Synthetic finding', file: 'f.go', priority: 'P1', detail: 'd' };
  expect(reviewVerdict(null)).toBe(UNKNOWN_VERDICT);
  expect(UNKNOWN_VERDICT.tone).toBe('cancelled');
  const cases: [string, ReturnType<typeof withReview>, string, string][] = [
    [
      'no round',
      withReview({ rounds_recorded: 0, latest: null }),
      'No review recorded',
      'cancelled'
    ],
    // A count without the round itself is still no review evidence.
    ['a count without a round', withReview({ latest: null }), 'No review recorded', 'cancelled'],
    [
      'clean at the output commit',
      withReview({ clean: true, clean_at_output_revision: true }),
      'Clean at the output commit',
      'clean'
    ],
    [
      'clean at another commit',
      withReview({ clean: true }, { matches_output_revision: false }),
      'Clean at another revision',
      'blocked'
    ],
    [
      'clean without an output commit',
      withReview({ clean: true }, { matches_output_revision: null }),
      'Clean, output revision unknown',
      'blocked'
    ],
    ['one finding', withReview({}, { findings: [finding] }), '1 recorded finding', 'blocked'],
    // Findings outweigh a clean-looking round, even at the output commit.
    [
      'two findings',
      withReview({}, { findings: [finding, finding] }),
      '2 recorded findings',
      'blocked'
    ],
    ['an incomplete round', withReview({}, { completed: false }), 'Review incomplete', 'running'],
    [
      'a blank summary',
      withReview({}, { summary_present: false }),
      'No review summary recorded',
      'blocked'
    ],
    // Complete, summarised and without findings, but the server did not call it clean.
    ['no clean report', withReview({}), 'Review standing unknown', 'blocked']
  ];
  for (const [name, evidence, label, tone] of cases)
    expect(reviewVerdict(evidence), name).toMatchObject({ label, tone });
  expect(
    reviewVerdict(withReview({ clean: true, clean_at_output_revision: true })).detail
  ).toContain('The latest of 1 recorded round is complete');
  expect(reviewVerdict(withReview({ clean: true }, { matches_output_revision: null })).detail).toBe(
    'The latest of 1 recorded round is clean, but no output commit is recorded for comparison.'
  );
  expect(reviewVerdict(withReview({ rounds_recorded: 3 }, { completed: false })).detail).toBe(
    'The latest recorded review round never completed, so zero findings proves nothing. 3 recorded rounds.'
  );
});

test('configured checks pass only when every command passed at the output commit', () => {
  const checks = (commands: ReturnType<typeof command>[], all_passed_at_output_revision = false) =>
    checksVerdict(
      taskEvidence('synthetic-task', {
        required_commands: { state: 'recorded', commands, all_passed_at_output_revision }
      })
    );
  expect(checksVerdict(null)).toBe(UNKNOWN_VERDICT);
  expect(
    checksVerdict(
      taskEvidence('synthetic-task', {
        required_commands: {
          state: 'not_configured',
          commands: [],
          all_passed_at_output_revision: false
        }
      })
    )
  ).toMatchObject({ label: 'No checks configured', tone: 'cancelled' });
  expect(checks([command('lint', 'passed'), command('test', 'passed')], true)).toMatchObject({
    label: 'All 2 passed at the output commit',
    tone: 'clean'
  });
  // A recorded failure outweighs every other state.
  expect(
    checks([command('lint', 'passed'), command('test', 'failed'), command('build', 'no_result')])
  ).toEqual({
    label: '1 of 3 passed at the output commit',
    tone: 'failed',
    detail: '1 failed, 1 no result recorded'
  });
  // A missing result is not a failure, and never a pass.
  expect(checks([command('lint', 'passed'), command('test', 'no_result')])).toEqual({
    label: '1 of 2 passed at the output commit',
    tone: 'blocked',
    detail: '1 no result recorded'
  });
  expect(
    checks([command('lint', 'passed_at_other_revision', A), command('test', 'no_result')])
  ).toEqual({
    label: '0 of 2 passed at the output commit',
    tone: 'blocked',
    detail: '1 passed at another revision, 1 no result recorded'
  });
});

test('reviewer agreement is reported only from recorded verdicts in every slot', () => {
  expect(reviewerAgreement([])).toMatchObject({
    label: 'No reviewer slots recorded',
    tone: 'cancelled'
  });
  const unusable = (slot: string, state: 'missing' | 'malformed' | 'duplicate') =>
    reviewer(slot, { state, decision: null, reason: null });
  // Nothing usable in any slot is an absence of evidence ...
  expect(
    reviewerAgreement([unusable('adversary-a', 'missing'), unusable('adversary-b', 'malformed')])
  ).toEqual({
    label: 'Reviewer evidence incomplete',
    tone: 'cancelled',
    detail:
      'Not every reviewer slot has exactly one recorded verdict: Reviewer A (no verdict recorded), Reviewer B (malformed batch). Agreement cannot be reported.'
  });
  // ... while one usable slot is incomplete evidence, never unanimity.
  expect(
    reviewerAgreement([reviewer('adversary-a'), unusable('adversary-b', 'duplicate')])
  ).toMatchObject({
    label: 'Reviewer evidence incomplete',
    tone: 'blocked',
    detail: expect.stringContaining('Reviewer B (duplicate verdicts)')
  });
  for (const [decision, tone] of [
    ['accepted', 'clean'],
    ['rejected', 'failed'],
    ['deferred', 'blocked']
  ])
    expect(
      reviewerAgreement([
        reviewer('adversary-a', { decision }),
        reviewer('adversary-b', { decision })
      ])
    ).toMatchObject({ label: `Both reviewers recorded ${decision}`, tone });
  expect(
    reviewerAgreement([reviewer('adversary-a'), reviewer('adversary-b', { decision: 'rejected' })])
  ).toEqual({
    label: 'Reviewers disagree',
    tone: 'blocked',
    detail:
      "Recorded verdicts differ: Reviewer A recorded accepted; Reviewer B recorded rejected. The final decision below was the orchestrator's."
  });
});

test('a reviewer slot badge shows its own decision only when exactly one is recorded', () => {
  expect(verdictBadge(reviewer('adversary-a', { decision: 'deferred' }))).toEqual({
    label: 'deferred',
    tone: 'blocked'
  });
  expect(verdictBadge(reviewer('adversary-a', { state: 'duplicate' }))).toEqual({
    label: 'accepted (duplicated)',
    tone: 'blocked'
  });
  expect(verdictBadge(reviewer('adversary-a', { state: 'duplicate', decision: null }))).toEqual({
    label: 'Duplicate verdicts',
    tone: 'blocked'
  });
  expect(verdictBadge(reviewer('adversary-a', { state: 'missing', decision: null }))).toEqual({
    label: 'No verdict recorded',
    tone: 'cancelled'
  });
  expect(verdictBadge(reviewer('adversary-a', { state: 'malformed', decision: null }))).toEqual({
    label: 'Malformed batch',
    tone: 'failed'
  });
});

test('planning outcomes name planning, never task completion, in both modes', () => {
  const cases: [string, 'execution' | 'audit', string, string][] = [
    ['completed', 'execution', 'Planning complete', 'clean'],
    ['completed', 'audit', 'Audit complete', 'clean'],
    ['running', 'execution', 'Planning in progress', 'running'],
    ['running', 'audit', 'Audit in progress', 'running'],
    ['failed', 'execution', 'Planning failed', 'failed'],
    ['interrupted', 'execution', 'Planning interrupted', 'blocked'],
    ['interrupted', 'audit', 'Audit interrupted', 'blocked']
  ];
  for (const [status, mode, label, tone] of cases)
    expect(planningVerdict({ status, mode }), `${mode} ${status}`).toMatchObject({ label, tone });
  expect(planningVerdict({ status: 'completed', mode: 'execution' }).detail).toBe(
    'Proposal decisions are recorded. Planning completion is not task completion.'
  );
  expect(planningVerdict({ status: 'running', mode: 'audit' }).detail).toBe(
    'Audit is still running. Recommendations are recorded when it finishes.'
  );
  expect(planningVerdict({ status: 'interrupted', mode: 'execution' }).detail).toBe(
    'Planning was interrupted by a service stop and did not finish.'
  );
});

test('a command explanation names the revisions it compared, recorded or not', () => {
  const at = (revision: string) => revision.slice(0, 12);
  expect(commandExplanation(command('test', 'passed'), B)).toBe(
    `Latest recorded result passed at the recorded output commit ${at(B)}.`
  );
  expect(commandExplanation(command('test', 'passed_at_other_revision', A), B)).toBe(
    `Latest recorded result passed at ${at(A)}, not at the recorded output commit ${at(B)}. A pass at another revision does not count.`
  );
  // Neither revision recorded: the sentence says so instead of inventing one.
  expect(commandExplanation(command('test', 'passed_at_other_revision', null), null)).toBe(
    'Latest recorded result passed at an unrecorded revision, not at the recorded output commit. A pass at another revision does not count.'
  );
  expect(commandExplanation(command('test', 'failed', null), B)).toBe(
    'Latest recorded result failed. A newer failure invalidates any older pass.'
  );
  expect(commandExplanation(command('test', 'no_result', null), B)).toBe(
    'Configured, with no result recorded. Not passing.'
  );
});

test('a count takes the singular noun for exactly one, and irregular plurals are spelled', () => {
  expect(plural(0, 'finding')).toBe('0 findings');
  expect(plural(1, 'finding')).toBe('1 finding');
  expect(plural(2, 'recorded round')).toBe('2 recorded rounds');
  expect(plural(1, 'retry', 'retries')).toBe('1 retry');
  expect(plural(0, 'retry', 'retries')).toBe('0 retries');
  expect(plural(3, 'retry', 'retries')).toBe('3 retries');
});

test('review rounds and revisions are judged separately, and a missing output commit is named', () => {
  const round = (result: { completed: boolean; summary: string; findings: unknown[] }) =>
    reviewRoundBadge({ result });
  expect(round({ completed: true, summary: 'Done.', findings: [{}] })).toEqual({
    label: '1 finding',
    tone: 'blocked'
  });
  expect(round({ completed: true, summary: 'Done.', findings: [{}, {}] })).toEqual({
    label: '2 findings',
    tone: 'blocked'
  });
  expect(round({ completed: false, summary: '', findings: [] })).toEqual({
    label: 'Incomplete',
    tone: 'running'
  });
  expect(round({ completed: true, summary: '  \n', findings: [] })).toEqual({
    label: 'No summary recorded',
    tone: 'blocked'
  });
  expect(round({ completed: true, summary: 'Done.', findings: [] })).toEqual({
    label: 'Clean',
    tone: 'clean'
  });
  expect(roundRevisionLabel(B, B)).toEqual(revisionMatchLabel(true));
  expect(roundRevisionLabel(A, B)).toEqual({
    label: 'Not the recorded output commit',
    tone: 'blocked'
  });
  expect(roundRevisionLabel(B, null)).toEqual({
    label: 'No output commit recorded',
    tone: 'cancelled'
  });
});

test('a recorded PR reference is delivery, and its absence is named', () => {
  expect(prVerdict(null)).toBe(UNKNOWN_VERDICT);
  expect(prVerdict(taskEvidence('synthetic-task', { pull_request: null }))).toMatchObject({
    label: 'No pull request recorded',
    tone: 'cancelled'
  });
  expect(prVerdict(taskEvidence('synthetic-task'))).toMatchObject({ label: '#77', tone: 'clean' });
  expect(
    prVerdict(
      taskEvidence('synthetic-task', {
        pull_request: { number: null, url: null, source: 'recorded_task_reference' }
      })
    )
  ).toMatchObject({ label: 'Recorded PR · number unavailable', tone: 'clean' });
});
