/**
 * The evidence display mapping (src/lib/evidence.ts), checked directly without a page.
 * These rules decide how saved records are named on every surface, so each case pins
 * one rule rather than one screen.
 */
import { test, expect } from '@playwright/test';
import {
  decisionCounts,
  outcomeVerdict,
  planningVerdict,
  reviewerSlot,
  taskIcon
} from '../src/lib/evidence';
import { ACTIVE_STATUSES } from '../src/lib/types';

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
