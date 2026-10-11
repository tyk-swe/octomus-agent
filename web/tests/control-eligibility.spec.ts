import { expect, test } from '@playwright/test';
import {
  chooseStep,
  configurationEditable,
  controlEligibility,
  type ControlAction,
  type SetupStatus
} from '../src/lib/setup';

test('controls consume server capabilities while the checklist explains configuration, activity and recovery', () => {
  const idle: SetupStatus = {
    capabilities: {
      pause: { allowed: true, reason: '' },
      resume: { allowed: true, reason: '' },
      cycle: { allowed: true, reason: '' },
      audit: { allowed: true, reason: '' },
      configure: { allowed: true, reason: '' }
    },
    configured: true,
    audit_configured: true,
    paused: true,
    mode: 'paused',
    active_tasks: 0,
    cycle_active: false,
    active_cycle_mode: null,
    baseline_active: false,
    auto_merge_active: false,
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
    queued: 0,
    latest: null,
    sandbox: {
      mode: 'off',
      healthy: true,
      error: null,
      broker: null,
      egress: null,
      pinned_repository: null,
      self_test: null
    }
  };
  const capacity = {
    day: '2026-10-07',
    limit: 10,
    used: 8,
    remaining: 2,
    required: 4,
    next_reset_at: 0
  };
  const cases: [string, Partial<SetupStatus>, ControlAction[], string][] = [
    ['idle', {}, ['resume', 'pause', 'cycle', 'audit'], 'Audit: available. Run once: available.'],
    [
      'configuration missing',
      { configured: false, audit_configured: false },
      [],
      'Audit: saved configuration incomplete. Run once: saved configuration incomplete.'
    ],
    [
      'audit configured independently',
      { configured: false },
      ['audit'],
      'Audit: available. Run once: saved configuration incomplete.'
    ],
    [
      'execution configured independently',
      { audit_configured: false },
      ['resume', 'pause', 'cycle'],
      'Audit: saved configuration incomplete. Run once: available.'
    ],
    [
      'active tasks',
      { active_tasks: 2 },
      ['resume', 'pause'],
      'Unavailable now: 2 active tasks may still finish and publish.'
    ],
    [
      'cycle planning',
      { cycle_active: true },
      ['resume', 'pause'],
      'Unavailable now: A cycle is planning.'
    ],
    [
      'execution preflight without a cycle',
      { active_cycle_mode: 'execution' },
      ['resume', 'pause'],
      'Unavailable now: A cycle is planning.'
    ],
    [
      'audit preflight without a cycle',
      { active_cycle_mode: 'audit' },
      [],
      'Unavailable now: An audit is in progress.'
    ],
    [
      'baseline active',
      { baseline_active: true },
      ['pause'],
      'Unavailable now: A baseline check is running.'
    ],
    [
      'merge worker active after Pause',
      { auto_merge_active: true },
      ['resume', 'pause'],
      'Unavailable now: An automatic merge check is still running.'
    ],
    [
      'continuous operation',
      { paused: false, mode: 'continuous' },
      ['resume', 'pause'],
      'Unavailable now: Continuous operation is running; Pause stops new work first.'
    ],
    [
      'daily admissions exhausted',
      { planning_capacity: { ...capacity, status: 'daily_exhausted' } },
      ['resume', 'pause'],
      'Audit: unavailable. Run once: unavailable. A complete planning pass requires 4 daily admissions; 2 remain today. Wait until midnight UTC or increase the daily limit.'
    ],
    [
      'daily limit too low',
      { planning_capacity: { ...capacity, status: 'limit_too_low' } },
      ['resume', 'pause'],
      'The configured daily limit cannot fund a complete planning pass; increase it in Configuration.'
    ],
    [
      'capacity ready',
      { planning_capacity: { ...capacity, status: 'ready' } },
      ['resume', 'pause', 'cycle', 'audit'],
      'Audit: available. Run once: available.'
    ],
    [
      'awaiting state refresh',
      { control_state_pending: true },
      ['pause'],
      'Audit: unavailable. Run once: unavailable. Control accepted. Current activity is unknown until the service state refreshes.'
    ],
    [
      'saved-state recovery',
      { recovery_error: 'Synthetic recovery failure' },
      ['pause'],
      'Unavailable now: Saved-state recovery is retrying. New work waits until recovery completes.'
    ],
    [
      'Pause remains available during audit recovery',
      { recovery_error: 'Synthetic recovery failure', active_cycle_mode: 'audit' },
      ['pause'],
      'Unavailable now: Saved-state recovery is retrying. New work waits until recovery completes.'
    ],
    [
      'queued tasks',
      { queued: 3 },
      ['resume', 'pause', 'cycle', 'audit'],
      'Audit: available. Run once: available, and 3 queued tasks would be drained first.'
    ]
  ];
  expect(controlEligibility(null)).toEqual({
    resume: false,
    pause: false,
    cycle: false,
    audit: false
  });
  expect(chooseStep(null).detail).toContain('Connect to the service first.');
  expect(configurationEditable(null)).toBe(false);
  expect(configurationEditable(idle)).toBe(true);
  expect(
    configurationEditable({
      ...idle,
      capabilities: { ...idle.capabilities, configure: { allowed: false, reason: 'Active work' } }
    })
  ).toBe(false);
  for (const [name, patch, actions, explanation] of cases) {
    const status = { ...idle, ...patch, capabilities: { ...idle.capabilities } };
    for (const action of ['resume', 'pause', 'cycle', 'audit'] as const)
      status.capabilities[action] = { allowed: actions.includes(action), reason: '' };
    expect(chooseStep(status).detail, name).toContain(explanation);
  }
});

test('server capability choices are authoritative while pending confirmation only blocks new work', () => {
  const status = {
    configured: false,
    audit_configured: false,
    paused: false,
    active_tasks: 99,
    capabilities: {
      resume: { allowed: true, reason: '' },
      pause: { allowed: true, reason: '' },
      cycle: { allowed: false, reason: 'Planning capacity' },
      audit: { allowed: false, reason: 'Active work' },
      configure: { allowed: true, reason: '' }
    }
  } as SetupStatus;
  expect(controlEligibility(status)).toEqual({
    resume: true,
    pause: true,
    cycle: false,
    audit: false
  });
  expect(configurationEditable(status)).toBe(true);
  expect(controlEligibility({ ...status, control_state_pending: true })).toEqual({
    resume: false,
    pause: true,
    cycle: false,
    audit: false
  });
});
