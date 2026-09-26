import type { Config } from './types';

/** Configuration fields that hold a number. */
type NumericConfigKey = {
  [K in keyof Config]: Config[K] extends number ? K : never;
}[keyof Config];

/**
 * The numeric operating limits the configuration form exposes, with the range
 * each one accepts. The bounds mirror what the service accepts (internal/config),
 * which checks every bounded limit again on save, together with one cross-field
 * rule: the task timeout must be at least the session timeout. A limit without a
 * maximum has none in the service either. The PR maintenance thresholds
 * (large_pr_lines, long_lived_pr_days) have no range in the service at all: any
 * count is accepted and 0 marks every owned open PR, so their minimum of 0 only
 * says a count is never negative. The bounds here let the form say what it will
 * accept before asking. TestDashboardLimitsMatchValidation (internal/config) holds
 * every entry to the service's validation; it reads each entry's key first and its
 * min, then any max, last.
 */
export type Limit = {
  key: NumericConfigKey;
  label: string;
  /** What the limit does; the accepted range is appended from `min` and `max`. */
  help: string;
  min: number;
  max?: number;
};
export const LIMITS: Limit[] = [
  {
    key: 'discovery_agents',
    label: 'Discovery agents',
    help: 'Complementary agents per cycle',
    min: 8,
    max: 10
  },
  {
    key: 'execution_concurrency',
    label: 'Concurrent tasks',
    help: 'Independent implementation workspaces',
    min: 1,
    max: 8
  },
  {
    key: 'cycle_interval_seconds',
    label: 'Cycle interval (seconds)',
    help: 'Time to wait between completed cycles',
    min: 30,
    max: 604800
  },
  {
    key: 'max_tasks_per_cycle',
    label: 'Tasks per cycle',
    help: 'Maximum accepted improvements',
    min: 1,
    max: 20
  },
  {
    key: 'maintenance_every_cycles',
    label: 'Maintenance cadence',
    help: 'Prioritize maintenance every N cycles',
    min: 1,
    max: 10000
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
    min: 10,
    max: 604800
  },
  {
    key: 'task_timeout_seconds',
    label: 'Task timeout (seconds)',
    help: 'Total limit for execution, review and delivery, at least the session timeout',
    min: 10,
    max: 604800
  },
  {
    key: 'command_timeout_seconds',
    label: 'Command timeout (seconds)',
    help: 'Maximum time for Git and verification commands',
    min: 1,
    max: 604800
  },
  {
    key: 'max_sessions_per_day',
    label: 'Daily session budget',
    help: 'Hard admission limit, resets at UTC midnight',
    min: 1,
    max: 1000000
  },
  {
    key: 'max_open_prs',
    label: 'Open PR capacity',
    help: 'Owned open PRs allowed before new-PR work waits',
    min: 1,
    max: 1000
  },
  {
    key: 'max_workspace_bytes',
    label: 'Workspace budget (bytes)',
    help: 'Block new sessions when storage reaches this limit',
    min: 1000000,
    max: 1000000000000000
  },
  {
    key: 'retain_completed_days',
    label: 'Workspace retention (days)',
    help: 'Published work only; unresolved work is preserved',
    min: 1,
    max: 36500
  },
  {
    key: 'retain_events',
    label: 'Retained activity events',
    help: 'Most recent events to keep',
    min: 100,
    max: 100000
  }
];

/** A limit's help text followed by its accepted range, e.g. `… · 1–10,000`. */
export function limitHelp({ help, min, max }: Limit): string {
  const count = (value: number) => value.toLocaleString('en-US');
  return max === undefined ? help : `${help} · ${count(min)}–${count(max)}`;
}
