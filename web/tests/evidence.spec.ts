import { expect } from '@playwright/test';
import {
  checksVerdict,
  prVerdict,
  reviewerAgreement,
  reviewVerdict,
  UNKNOWN_VERDICT,
  verdictBadge
} from '../src/lib/evidence';
import { sandboxVerdict } from '../src/lib/sandbox';
import { sandboxStep, type SetupStatus } from '../src/lib/setup';
import type {
  ReviewEvidence,
  ReviewRoundEvidence,
  SandboxPosture,
  SandboxSelfTest
} from '../src/lib/types';
import { A, command, reviewer, reviewRound, taskEvidence, test } from './synthetic';

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
  expect(
    checks([command('lint', 'passed'), command('test', 'failed'), command('build', 'no_result')])
  ).toEqual({
    label: '1 of 3 passed at the output commit',
    tone: 'failed',
    detail: '1 failed, 1 no result recorded'
  });
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
  expect(
    reviewerAgreement([unusable('adversary-a', 'missing'), unusable('adversary-b', 'malformed')])
  ).toEqual({
    label: 'Reviewer evidence incomplete',
    tone: 'cancelled',
    detail:
      'Not every reviewer slot has exactly one recorded verdict: Reviewer A (no verdict recorded), Reviewer B (malformed batch). Agreement cannot be reported.'
  });
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

function posture(over: Partial<SandboxSelfTest> = {}): SandboxPosture {
  return {
    mode: 'docker',
    healthy: true,
    error: null,
    broker: null,
    egress: null,
    pinned_repository: null,
    self_test: {
      at: new Date().toISOString(),
      passed: true,
      checks: [
        { id: 'synthetic-check', label: 'Synthetic containment check', passed: true, detail: '' }
      ],
      kernel: 'synthetic',
      image_id: 'synthetic-image',
      error: null,
      ...over
    }
  };
}

function setup(sandbox: SandboxPosture) {
  const status: SetupStatus = {
    configured: false,
    audit_configured: false,
    paused: true,
    mode: 'paused',
    active_tasks: 0,
    cycle_active: false,
    baseline_active: false,
    baseline: null,
    notifications: {
      state: 'disabled',
      configured: false,
      pending: 0,
      failed: 0,
      last_delivered_at: null,
      last_error: null,
      last_http_status: null
    },
    active_cycle_mode: null,
    queued: 0,
    latest: null,
    sandbox
  };
  return sandboxStep(status);
}

test('only a passing self-test with recorded passing checks establishes containment', () => {
  for (const [name, report, detail] of [
    ['empty failed report', { passed: false, checks: [] }, 'No containment checks were recorded'],
    ['empty passing report', { passed: true, checks: [] }, 'No containment checks were recorded'],
    ['recorded failure with passing checks', { passed: false }, 'did not pass'],
    ['empty recorded error', { error: '' }, 'did not complete'],
    ['recorded error', { error: 'Synthetic probe failure' }, 'Synthetic probe failure']
  ] satisfies [string, Partial<SandboxSelfTest>, string][]) {
    const sandbox = posture(report);
    expect(sandboxVerdict(sandbox).tone, name).toBe('failed');
    expect(sandboxVerdict(sandbox).detail, name).toContain(detail);
    expect(setup(sandbox), name).toMatchObject({ tone: 'failed', label: 'Self-test failed' });
    expect(setup(sandbox).detail, name).toContain(detail);
  }
  const sandbox = posture();
  expect(sandboxVerdict(sandbox)).toMatchObject({ label: 'Contained', tone: 'clean' });
  expect(setup(sandbox).tone).toBe('checked');
  expect(setup(sandbox).label).toMatch(/^Proven/);
  sandbox.self_test = null;
  expect(sandboxVerdict(sandbox)).toMatchObject({ label: 'Not yet proven', tone: 'blocked' });
  expect(setup(sandbox)).toMatchObject({ label: 'Not yet proven', tone: 'missing' });
});
