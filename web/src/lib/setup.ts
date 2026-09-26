import { relative } from './api';
import { baselineStatusLabel } from './evidence';
import type {
  Backend,
  BaselineSummary,
  Config,
  CycleSummary,
  ModelCatalog,
  NotificationHealth,
  Route
} from './types';

/**
 * First-run setup checklist states, derived from the configuration draft, the last
 * saved configuration, loaded catalogs, the latest explicit connection check and the
 * polled service snapshot. Nothing here is persisted or sent to the service.
 *
 * Entered: typed in this tab. Saved: written to the service. Checked: the exact saved
 * configuration passed an explicit connection check. Ran: a cycle actually executed.
 * Populated fields, catalog matches and a passed check never prove repository push
 * permission or model inference; only a run's recorded evidence does.
 */
export type SetupTone = 'missing' | 'draft' | 'saved' | 'checked' | 'failed' | 'ran';
export type SetupStep = { tone: SetupTone; label: string; detail: string };
export type Preflight = {
  mode: 'execution' | 'audit';
  ok: boolean;
  detail: string;
  /** Canonical configuration revision the server checked; any other revision is stale. */
  checkedRevision: string;
  at: string;
};
export type SetupStatus = {
  configured: boolean;
  audit_configured: boolean;
  paused: boolean;
  mode: 'paused' | 'run_once' | 'continuous';
  active_tasks: number;
  cycle_active: boolean;
  baseline_active: boolean;
  baseline: BaselineSummary | null;
  notifications: NotificationHealth;
  active_cycle_mode: 'execution' | 'audit' | null;
  queued: number;
  latest: CycleSummary | null;
};

const REPOSITORY_FIELDS = ['repository', 'github_repo', 'default_branch', 'branch_prefix'] as const;
const filled = (value: unknown) => typeof value === 'string' && value.trim() !== '';
const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
const plural = (count: number, noun: string) => `${count} ${noun}${count === 1 ? '' : 's'}`;

export function repositoryStep(draft: Config, saved: Config | null): SetupStep {
  const pick = (config: Config) => REPOSITORY_FIELDS.map((field) => config[field]);
  const complete = (config: Config) => pick(config).every(filled);
  if (saved && complete(saved) && same(pick(draft), pick(saved)))
    return {
      tone: 'saved',
      label: 'Saved',
      detail: `${saved.github_repo} at ${saved.repository}, default branch ${saved.default_branch}, owned prefix ${saved.branch_prefix}. Saved values are not checked until you run a connection check.`
    };
  if (complete(draft))
    return {
      tone: 'draft',
      label: 'Entered, not saved',
      detail:
        saved && complete(saved)
          ? 'Edited in this tab. The previously saved values still apply until you save.'
          : 'Entered in this tab only. Save configuration to send these values to the service.'
    };
  return {
    tone: 'missing',
    label: 'Incomplete',
    detail:
      'Enter the absolute checkout path, the owner/repository on GitHub, the default branch and an owned branch prefix.'
  };
}

export function routeComplete(route: Route | undefined): boolean {
  if (!route) return false;
  return (route.backend ?? 'codex') === 'opencode'
    ? filled(route.provider) && filled(route.model)
    : filled(route.model) && filled(route.effort);
}

/** True only when a catalog loaded for the entered executable lists this exact route as available. */
export function routeValidated(
  route: Route,
  config: Config,
  catalogs: Partial<Record<Backend, ModelCatalog>>
): boolean {
  const backend = route.backend ?? 'codex';
  const catalog = catalogs[backend];
  if (!catalog?.loaded || catalog.binary !== config[`${backend}_binary`]) return false;
  const model = catalog.models.find(
    (entry) => entry.model === route.model && (entry.provider ?? null) === (route.provider ?? null)
  );
  if (!model?.available) return false;
  return backend === 'codex'
    ? model.efforts.includes(route.effort)
    : !route.variant || model.variants.includes(route.variant);
}

/** Mirrors config.Config.RoutesFor (internal/config): audits skip the code reviewer, execution tiers and repair. */
export function requiredRoutes(config: Config, audit: boolean): [string, Route][] {
  const roles = Object.entries(config.roles).filter(([role]) => !audit || role !== 'code_reviewer');
  return audit
    ? roles
    : [...roles, ...Object.entries(config.tiers), ['repair', config.repair_route]];
}

export function routesStep(
  draft: Config,
  saved: Config | null,
  catalogs: Partial<Record<Backend, ModelCatalog>>
): SetupStep {
  const execution = requiredRoutes(draft, false);
  const audit = requiredRoutes(draft, true);
  const selected = execution.filter(([, route]) => routeComplete(route)).length;
  const auditSelected = audit.filter(([, route]) => routeComplete(route)).length;
  const validated = execution.filter(
    ([, route]) => routeComplete(route) && routeValidated(route, draft, catalogs)
  ).length;
  const routing = (config: Config) => [
    config.codex_binary,
    config.opencode_binary,
    config.roles,
    config.tiers,
    config.repair_route
  ];
  const counts = `${selected} of ${execution.length} execution routes selected (${auditSelected} of ${audit.length} audit routes). ${validated} match a loaded catalog for the entered executable; a match is not a connection check.`;
  if (auditSelected < audit.length)
    return {
      tone: 'missing',
      label: 'Incomplete',
      detail: `An audit needs the orchestrator, discovery and proposal reviewer routes. ${counts}`
    };
  if (!saved || !same(routing(draft), routing(saved)))
    return { tone: 'draft', label: 'Entered, not saved', detail: counts };
  if (selected < execution.length)
    return {
      tone: 'saved',
      label: 'Saved, audit routes only',
      detail: `Run once also needs the code reviewer, five execution tiers and repair. ${counts}`
    };
  return { tone: 'saved', label: 'Saved', detail: counts };
}

export function verificationStep(draft: string[], saved: string[]): SetupStep {
  if (!draft.length)
    return {
      tone: 'missing',
      label: 'None',
      detail:
        'Run once requires at least one command that must pass before a PR is published. Audits plan only and run none.'
    };
  if (!same(draft, saved))
    return {
      tone: 'draft',
      label: 'Entered, not saved',
      detail: `${plural(draft.length, 'command')} in this tab. Save configuration to make them the publication policy.`
    };
  return {
    tone: 'saved',
    label: 'Saved',
    detail: `${plural(saved.length, 'saved command')} run inside every task workspace; all must pass on the reviewed revision before publication.`
  };
}

export function preflightStep(
  preflight: Preflight | null,
  dirty: boolean,
  revision: string
): SetupStep {
  const scope =
    'It validates the saved origin remote, GitHub CLI login and runner catalogs for the saved routes. It does not prove repository push permission and makes no model call.';
  if (dirty)
    return {
      tone: 'draft',
      label: 'Unsaved edits',
      detail: `${
        preflight && revision && preflight.checkedRevision === revision
          ? `The ${preflight.mode} check at ${preflight.at} covered the previously saved values, not these edits. `
          : ''
      }Save or discard, then check the saved configuration.`
    };
  if (!preflight || !revision || preflight.checkedRevision !== revision)
    return {
      tone: 'missing',
      label: 'Not checked',
      detail: preflight
        ? `The server checked a different saved configuration. Reopen Configuration to refresh this form, then check again. ${scope}`
        : `Not run for this saved configuration. ${scope}`
    };
  if (preflight.ok)
    return {
      tone: 'checked',
      label: `Passed · ${preflight.mode} · ${preflight.at}`,
      detail: `${preflight.detail} ${scope}`
    };
  return {
    tone: 'failed',
    label: `Failed · ${preflight.mode} · ${preflight.at}`,
    detail: `${preflight.detail} Correct the saved configuration or the host, save, and check again.`
  };
}

export function parseCommands(text: string): string[] {
  return text
    .split('\n')
    .map((command) => command.trim())
    .filter(Boolean);
}

export function baselineStep(status: SetupStatus | null): SetupStep {
  const contract =
    'Runs the saved verification commands on a disposable clone of the remote default branch. Optional; a pass is not publication evidence and does not prove later host or remote health.';
  if (!status)
    return {
      tone: 'missing',
      label: 'Optional',
      detail: `Connect to the service first. ${contract}`
    };
  if (status.baseline_active)
    return {
      tone: 'draft',
      label: 'Running',
      detail: `A baseline check is in progress; controls resume when it finishes. ${contract}`
    };
  const baseline = status.baseline;
  if (!baseline) return { tone: 'missing', label: 'Optional · not run', detail: contract };
  if (baseline.status === 'running') return { tone: 'draft', label: 'Running', detail: contract };
  if (baseline.status === 'passed') {
    const label = `Passed · ${relative(baseline.started_at)}`;
    if (baseline.config_matches === false)
      return {
        tone: 'saved',
        label,
        detail: `The saved configuration changed after this check; the pass covered the previous saved values. ${contract}`
      };
    if (baseline.revision_status === 'stale')
      return {
        tone: 'saved',
        label,
        detail: `The observed remote default branch moved after this check. ${contract}`
      };
    if (baseline.revision_status !== 'matches_last_observation')
      return {
        tone: 'saved',
        label,
        detail: `No recent remote observation confirms the checked revision is still current. ${contract}`
      };
    return {
      tone: 'checked',
      label,
      detail: `The saved commands passed on a clone checked at ${baseline.started_at}. ${contract}`
    };
  }
  return {
    tone: 'failed',
    label: `${baselineStatusLabel(baseline.status)} · ${relative(baseline.started_at)}`,
    detail: `The last baseline check did not pass. ${contract}`
  };
}

export function chooseStep(status: SetupStatus | null): SetupStep {
  const contract =
    'An audit plans only: it records decisions and queues nothing, and no later cycle executes its recommendations. Run once drains the existing queue, plans one cycle, finishes accepted tasks and pauses. Continuous operation is a separate, explicit control.';
  if (!status)
    return {
      tone: 'missing',
      label: 'Not run',
      detail: `Connect to the service first. ${contract}`
    };
  const blocker =
    status.active_cycle_mode === 'audit'
      ? 'An audit is in progress.'
      : status.baseline_active
        ? 'A baseline check is running.'
        : status.active_tasks
          ? `${plural(status.active_tasks, 'active task')} may still finish and publish.`
          : status.cycle_active
            ? 'A cycle is planning.'
            : !status.paused
              ? status.mode === 'continuous'
                ? 'Continuous operation is running; Pause stops new work first.'
                : 'A run-once cycle is in progress.'
              : '';
  const availability = blocker
    ? `Unavailable now: ${blocker}`
    : `Audit: ${status.audit_configured ? 'available' : 'saved configuration incomplete'}. Run once: ${
        status.configured ? 'available' : 'saved configuration incomplete'
      }${status.queued ? `, and ${plural(status.queued, 'queued task')} would be drained first` : ''}.`;
  if (status.latest) {
    const latest = status.latest;
    return {
      tone: 'ran',
      label: `${latest.mode === 'audit' ? 'Audit' : 'Execution'} cycle ${latest.number} · ${latest.status}`,
      detail: `Started ${relative(latest.started_at)}; inspect its run evidence on the Overview. ${availability} ${contract}`
    };
  }
  return { tone: 'missing', label: 'Not run', detail: `${availability} ${contract}` };
}
