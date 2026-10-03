import { expect, test } from '@playwright/test';
import { chooseStep, type SetupStatus } from '../src/lib/setup';

test.skip(({ isMobile }) => isMobile, 'Pure availability rules run once.');

const pending: SetupStatus = {
  configured: true,
  audit_configured: true,
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
  queued: 2,
  latest: null,
  sandbox: {
    mode: 'off',
    healthy: false,
    error: null,
    broker: null,
    egress: null,
    pinned_repository: null,
    self_test: null
  },
  control_state_pending: true
};

test('accepted controls with unrefreshed activity do not advertise another start', () => {
  const detail = chooseStep(pending).detail;
  expect(detail).toContain('Audit: unavailable. Run once: unavailable.');
  expect(detail).toContain(
    'Control accepted. Current activity is unknown until the service state refreshes.'
  );
  expect(detail).not.toContain('queued tasks would be drained first');
  expect(detail).not.toContain('An audit is in progress.');
  expect(chooseStep({ ...pending, control_state_pending: false }).detail).toContain(
    'Audit: available. Run once: available, and 2 queued tasks would be drained first.'
  );
});

for (const [changes, explanation] of [
  [{ active_cycle_mode: 'audit', baseline_active: true }, 'An audit is in progress.'],
  [{ baseline_active: true, active_tasks: 1 }, 'A baseline check is running.'],
  [{ active_tasks: 1, cycle_active: true }, '1 active task may still finish and publish.'],
  [{ cycle_active: true }, 'A cycle is planning.'],
  [
    { paused: false, mode: 'continuous' },
    'Continuous operation is running; Pause stops new work first.'
  ],
  [{ paused: false, mode: 'run_once' }, 'A run-once cycle is in progress.']
] as [Partial<SetupStatus>, string][]) {
  test(`confirmed activity retains its existing precedence: ${explanation}`, () => {
    const detail = chooseStep({ ...pending, ...changes }).detail;
    expect(detail).toContain(`Unavailable now: ${explanation}`);
    expect(detail).not.toContain('Current activity is unknown');
  });
}

for (const status of ['daily_exhausted', 'limit_too_low'] as const) {
  test(`saved prerequisites and ${status} keep their precedence over pending state`, () => {
    const detail = chooseStep({
      ...pending,
      configured: false,
      planning_capacity: {
        status,
        day: '2026-10-02',
        limit: 150,
        used: 138,
        required: 13,
        remaining: 12,
        next_reset_at: 1790985600
      }
    }).detail;
    expect(detail).toContain('Audit: unavailable. Run once: saved configuration incomplete.');
    expect(detail).toContain('13 daily admissions; 12 remain today.');
    expect(detail).not.toContain('Current activity is unknown');
    expect(detail).not.toContain('queued tasks would be drained first');
  });
}

for (const active_cycle_mode of [null, 'audit'] as const) {
  test(`saved-state recovery blocks checklist starts with cycle mode ${active_cycle_mode}`, () => {
    const detail = chooseStep({
      ...pending,
      control_state_pending: false,
      recovery_error: 'Synthetic recovery refusal',
      active_cycle_mode
    }).detail;
    expect(detail).toContain(
      'Unavailable now: Saved-state recovery is retrying. New work waits until recovery completes.'
    );
    expect(detail).not.toContain('Audit: available');
    expect(detail).not.toContain('Run once: available');
    expect(detail).not.toContain('queued tasks would be drained first');
    expect(detail).not.toContain('An audit is in progress.');
  });
}
