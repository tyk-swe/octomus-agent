/**
 * The evidence display mapping (src/lib/evidence.ts), checked directly without a page.
 * These rules decide how saved records are named on every surface, so each case pins
 * one rule rather than one screen.
 */
import { test, expect } from '@playwright/test';
import { decisionCounts, planningVerdict } from '../src/lib/evidence';

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
