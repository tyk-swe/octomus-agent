import { expect, test } from '@playwright/test';
import { sandboxVerdict } from '../src/lib/sandbox';
import { sandboxStep, type SetupStatus } from '../src/lib/setup';
import type { SandboxPosture, SandboxSelfTest } from '../src/lib/types';

test.skip(({ isMobile }) => isMobile, 'Pure sandbox verdict rules run once.');

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

for (const [name, report, detail] of [
  ['empty failed report', { passed: false, checks: [] }, 'No containment checks were recorded'],
  ['empty passing report', { passed: true, checks: [] }, 'No containment checks were recorded'],
  ['recorded failure with passing checks', { passed: false }, 'did not pass'],
  ['empty recorded error', { error: '' }, 'did not complete'],
  ['recorded error', { error: 'Synthetic probe failure' }, 'Synthetic probe failure']
] satisfies [string, Partial<SandboxSelfTest>, string][]) {
  test(`${name} cannot claim containment or a proven setup step`, () => {
    const sandbox = posture(report);
    expect(sandboxVerdict(sandbox).tone).toBe('failed');
    expect(sandboxVerdict(sandbox).detail).toContain(detail);
    expect(setup(sandbox).tone).toBe('failed');
    expect(setup(sandbox).label).toBe('Self-test failed');
    expect(setup(sandbox).detail).toContain(detail);
  });
}

test('a failed individual check overrides an inconsistent passing report', () => {
  const sandbox = posture({
    checks: [
      {
        id: 'synthetic-check',
        label: 'Synthetic containment check',
        passed: false,
        detail: 'Synthetic exposure'
      }
    ]
  });
  expect(sandboxVerdict(sandbox)).toMatchObject({ label: '1 check failed', tone: 'failed' });
  expect(setup(sandbox)).toMatchObject({ tone: 'failed', label: 'Self-test failed' });
  expect(setup(sandbox).detail).toContain('Synthetic containment check (Synthetic exposure)');
});

test('only a passing report with recorded passing checks establishes containment', () => {
  const sandbox = posture();
  expect(sandboxVerdict(sandbox)).toMatchObject({ label: 'Contained', tone: 'clean' });
  expect(setup(sandbox).tone).toBe('checked');
  expect(setup(sandbox).label).toMatch(/^Proven/);
  sandbox.self_test = null;
  expect(sandboxVerdict(sandbox)).toMatchObject({ label: 'Not yet proven', tone: 'blocked' });
  expect(setup(sandbox)).toMatchObject({ label: 'Not yet proven', tone: 'missing' });
});
