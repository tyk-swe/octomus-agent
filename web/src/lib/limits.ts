import type { Config } from './types';

/** Configuration fields that hold a number. */
type NumericConfigKey = {
  [K in keyof Config]: Config[K] extends number ? K : never;
}[keyof Config];

/**
 * The numeric operating limits the configuration form exposes, with the range
 * each one accepts. The service validates these ranges again on save; the
 * bounds here exist so the form can say what it will accept before asking.
 */
export const LIMITS: {
  key: NumericConfigKey;
  label: string;
  help: string;
  min: number;
  max?: number;
}[] = [
  {
    key: 'discovery_agents',
    label: 'Discovery agents',
    help: 'Complementary agents per cycle · 8–10',
    min: 8,
    max: 10
  },
  {
    key: 'execution_concurrency',
    label: 'Concurrent tasks',
    help: 'Independent implementation workspaces · 1–8',
    min: 1,
    max: 8
  },
  {
    key: 'cycle_interval_seconds',
    label: 'Cycle interval (seconds)',
    help: 'Time to wait between completed cycles',
    min: 30
  },
  {
    key: 'max_tasks_per_cycle',
    label: 'Tasks per cycle',
    help: 'Maximum accepted improvements · 1–20',
    min: 1,
    max: 20
  },
  {
    key: 'maintenance_every_cycles',
    label: 'Maintenance cadence',
    help: 'Prioritize maintenance every N cycles',
    min: 1
  },
  {
    key: 'large_pr_lines',
    label: 'Large PR threshold',
    help: 'Changed lines that trigger maintenance focus · 0 marks every owned open PR',
    min: 0
  },
  {
    key: 'long_lived_pr_days',
    label: 'Long-lived PR (days)',
    help: 'PR age that triggers maintenance focus · 0 marks every owned open PR',
    min: 0
  },
  {
    key: 'max_repair_rounds',
    label: 'Repair rounds',
    help: 'Unresolved work is blocked at this limit',
    min: 1,
    max: 20
  },
  {
    key: 'max_no_progress_rounds',
    label: 'No-progress rounds',
    help: 'Stop repeated repairs without code changes',
    min: 1
  },
  {
    key: 'max_retries',
    label: 'Operator retries',
    help: 'Maximum retries for each blocked task',
    min: 0,
    max: 10
  },
  {
    key: 'session_timeout_seconds',
    label: 'Session timeout (seconds)',
    help: 'Maximum duration of an agent turn',
    min: 10
  },
  {
    key: 'task_timeout_seconds',
    label: 'Task timeout (seconds)',
    help: 'Total limit for execution, review and delivery',
    min: 10
  },
  {
    key: 'command_timeout_seconds',
    label: 'Command timeout (seconds)',
    help: 'Maximum time for Git and verification commands',
    min: 1
  },
  {
    key: 'max_sessions_per_day',
    label: 'Daily session budget',
    help: 'Hard admission limit, resets at UTC midnight',
    min: 1
  },
  {
    key: 'max_open_prs',
    label: 'Open PR capacity',
    help: 'Owned open PRs allowed before new-PR work waits · 1–1000',
    min: 1,
    max: 1000
  },
  {
    key: 'max_workspace_bytes',
    label: 'Workspace budget (bytes)',
    help: 'Block new sessions when storage reaches this limit',
    min: 1000000
  },
  {
    key: 'retain_completed_days',
    label: 'Workspace retention (days)',
    help: 'Published work only; unresolved work is preserved',
    min: 1
  },
  {
    key: 'retain_events',
    label: 'Retained activity events',
    help: 'Most recent events to keep · 100–100,000',
    min: 100,
    max: 100000
  }
];
