/**
 * Display mapping for `RunEvidenceV1` (see docs/run-evidence.md).
 *
 * Every label here is derived from the server's already-normalized evidence statuses.
 * Nothing in this module recomputes an optimistic boolean from raw records: unknown,
 * stale and missing states are preserved and named. Kept in one module so the run
 * evidence panel and the task detail summary cannot describe the same record
 * differently.
 */
import { ACTIVE_STATUSES } from './types';
import type {
  BaselineStatus,
  CommandEvidence,
  CommandResult,
  CommandState,
  ReviewRoundEvidence,
  ReviewerVerdict,
  RunEvidenceV1,
  TaskEvidence,
  VerdictState
} from './types';

/**
 * Badge tones every surface must style. `cancelled` reads as "nothing
 * recorded", not "fine", so a surface that leaves it unstyled hides exactly
 * the adverse evidence this mapping exists to show. Both app.css and
 * showcase/style.css are checked against this list.
 */
export const TONES = ['clean', 'blocked', 'failed', 'running', 'cancelled'] as const;
export type Tone = (typeof TONES)[number] | '';
export type Verdict = { label: string; tone: Tone; detail: string };

/**
 * Reviewer slots are positional and fixed in `src/evidence.rs`; these labels explain the
 * role each slot argues, rather than repeating its internal identity.
 */
const REVIEWER_ROLES: Record<string, string> = {
  'adversary-a': 'Problem and value',
  'adversary-b': 'Feasibility and risk'
};
export function reviewerLabel(reviewer: string): string {
  return REVIEWER_ROLES[reviewer] ?? reviewer;
}
export function reviewerSlot(reviewer: string): string {
  return reviewer === 'adversary-a'
    ? 'Reviewer A'
    : reviewer === 'adversary-b'
      ? 'Reviewer B'
      : reviewer;
}

/** Decision words keep their own badge tone; `deferred` must never read as `rejected`. */
export function decisionTone(decision: string | null): Tone {
  return decision === 'accepted'
    ? 'clean'
    : decision === 'rejected'
      ? 'failed'
      : decision === 'deferred'
        ? 'blocked'
        : 'cancelled';
}

/**
 * Persisted `decision::ALL` words in src/model.rs, in the order the dashboard shows
 * them. Mirrors the server vocabulary the way `ACTIVE_STATUSES` mirrors `Status::ACTIVE`.
 */
export const DECISIONS = ['accepted', 'rejected', 'deferred', 'candidate'] as const;

/** The mode word every surface names a cycle with. */
export function modeLabel(mode: string): string {
  return mode === 'audit' ? 'Audit' : 'Execution';
}

/** One cycle heading, e.g. `Execution cycle #001`. */
export function cycleLabel(cycle: { mode: string; number: number }): string {
  return `${modeLabel(cycle.mode)} cycle #${String(cycle.number).padStart(3, '0')}`;
}

/** Saved baseline statuses, spelled the way the check panel shows them. */
export function baselineStatusLabel(status: BaselineStatus): string {
  const labels: Record<BaselineStatus, string> = {
    running: 'Running',
    passed: 'Passed',
    failed: 'Failed',
    cancelled: 'Cancelled',
    timed_out: 'Timed out',
    interrupted: 'Interrupted'
  };
  return labels[status];
}

const VERDICT_STATES: Record<VerdictState, { label: string; tone: Tone }> = {
  recorded: { label: 'Recorded', tone: '' },
  missing: { label: 'No verdict recorded', tone: 'cancelled' },
  duplicate: { label: 'Duplicate verdicts', tone: 'blocked' },
  malformed: { label: 'Malformed batch', tone: 'failed' }
};

/** The state badge for one reviewer slot. A recorded verdict shows its own decision. */
export function verdictBadge(verdict: ReviewerVerdict): { label: string; tone: Tone } {
  if (verdict.state === 'recorded' && verdict.decision)
    return { label: verdict.decision, tone: decisionTone(verdict.decision) };
  if (verdict.state === 'duplicate' && verdict.decision)
    return { label: `${verdict.decision} (duplicated)`, tone: 'blocked' };
  return VERDICT_STATES[verdict.state];
}

/**
 * Agreement across the two slots. Reported only from verdicts that are actually
 * recorded: a missing or malformed slot makes the reviewer evidence incomplete rather
 * than unanimous.
 */
export function reviewerAgreement(verdicts: ReviewerVerdict[]): Verdict {
  if (verdicts.length === 0)
    return {
      label: 'No reviewer slots recorded',
      tone: 'cancelled',
      detail: 'This run records no reviewer slots for this proposal.'
    };
  const unusable = verdicts.filter((v) => v.state !== 'recorded');
  if (unusable.length)
    return {
      label: 'Reviewer evidence incomplete',
      tone: unusable.length === verdicts.length ? 'cancelled' : 'blocked',
      detail: `Not every reviewer slot has exactly one recorded verdict: ${unusable
        .map((v) => `${reviewerSlot(v.reviewer)} (${VERDICT_STATES[v.state].label.toLowerCase()})`)
        .join(', ')}. Agreement cannot be reported.`
    };
  const decisions = [...new Set(verdicts.map((v) => v.decision ?? ''))];
  if (decisions.length === 1)
    return {
      label: `Both reviewers recorded ${decisions[0]}`,
      tone: decisionTone(decisions[0]),
      detail: 'Both recorded reviewer verdicts agree.'
    };
  return {
    label: 'Reviewers disagree',
    tone: 'blocked',
    detail: `Recorded verdicts differ: ${verdicts
      .map((v) => `${reviewerSlot(v.reviewer)} recorded ${v.decision}`)
      .join('; ')}. The final decision below was the orchestrator's.`
  };
}

/**
 * Badge for one saved review round in the task's own review history.
 *
 * Zero findings alone is not "Clean": an incomplete round or a blank summary is
 * reported as such, matching `ReviewRoundResult::clean` on the server.
 */
export function reviewRoundBadge(round: {
  result: { completed: boolean; summary: string; findings: unknown[] };
}): { label: string; tone: Tone } {
  const findings = round.result.findings.length;
  if (findings)
    return { label: `${findings} finding${findings === 1 ? '' : 's'}`, tone: 'blocked' };
  if (!round.result.completed) return { label: 'Incomplete', tone: 'running' };
  if (!round.result.summary.trim()) return { label: 'No summary recorded', tone: 'blocked' };
  return { label: 'Clean', tone: 'clean' };
}

/**
 * Whether a round ran at the recorded output commit. Historical cleanliness and
 * matching the delivered commit are separate facts and are never merged.
 */
export function roundRevisionLabel(
  revision: string,
  outputCommit: string | null
): { label: string; tone: Tone } {
  return revisionMatchLabel(outputCommit ? revision === outputCommit : null);
}

/** The run view uses the server's normalized comparison, including null. */
export function revisionMatchLabel(matches: boolean | null): { label: string; tone: Tone } {
  if (matches === null) return { label: 'No output commit recorded', tone: 'cancelled' };
  return matches
    ? { label: 'At the recorded output commit', tone: 'clean' }
    : { label: 'Not the recorded output commit', tone: 'blocked' };
}

/** The recorded outcome word for a task, with what the status does and does not imply. */
export function outcomeVerdict(task: {
  status: string;
  blocked_reason?: string | null;
  error_recorded?: boolean;
}): Verdict {
  const blocked = task.blocked_reason
    ? ` Blocked reason: ${task.blocked_reason.replaceAll('_', ' ')}.`
    : '';
  const detail =
    task.status === 'published'
      ? 'Recorded as published. Published describes delivery, not merge.'
      : 'The saved task status, verbatim.';
  return {
    label: task.status,
    tone:
      task.status === 'published'
        ? 'clean'
        : task.status === 'failed'
          ? 'failed'
          : task.status === 'blocked'
            ? 'blocked'
            : task.status === 'cancelled'
              ? 'cancelled'
              : 'running',
    detail: detail + blocked + (task.error_recorded ? ' An error is recorded.' : '')
  };
}

/** The shared fallback for evidence that has not loaded; never an optimistic pass. */
export const UNKNOWN_VERDICT: Verdict = {
  label: 'Unknown',
  tone: 'cancelled',
  detail:
    'Recorded evidence has not been loaded for this task, so this is unknown rather than passing.'
};

/**
 * Review standing at the recorded output commit, taken from the server's `clean` and
 * `clean_at_output_revision` rather than recomputed here.
 */
export function reviewVerdict(evidence: TaskEvidence | null): Verdict {
  if (!evidence) return UNKNOWN_VERDICT;
  const review = evidence.latest_review;
  const rounds = `${review.rounds_recorded} recorded round${review.rounds_recorded === 1 ? '' : 's'}`;
  if (review.rounds_recorded === 0 || !review.latest)
    return {
      label: 'No review recorded',
      tone: 'cancelled',
      detail: 'No review round is recorded for this task. Missing review evidence is not a pass.'
    };
  if (review.clean_at_output_revision)
    return {
      label: 'Clean at the output commit',
      tone: 'clean',
      detail: `The latest of ${rounds} is complete, carries a summary, records zero findings, and ran at the recorded output commit.`
    };
  if (review.clean)
    return {
      label:
        review.latest.matches_output_revision === null
          ? 'Clean, output revision unknown'
          : 'Clean at another revision',
      tone: 'blocked',
      detail:
        review.latest.matches_output_revision === null
          ? `The latest of ${rounds} is clean, but no output commit is recorded for comparison.`
          : `The latest of ${rounds} is clean but did not run at the recorded output commit.`
    };
  const verdict = latestRoundVerdict(review.latest);
  return { ...verdict, detail: `${verdict.detail} ${rounds}.` };
}

function latestRoundVerdict(latest: ReviewRoundEvidence): Verdict {
  const findings = latest.findings.length;
  if (findings)
    return {
      label: `${findings} recorded finding${findings === 1 ? '' : 's'}`,
      tone: 'blocked',
      detail: 'The latest recorded review round reports findings.'
    };
  if (!latest.completed)
    return {
      label: 'Review incomplete',
      tone: 'running',
      detail: 'The latest recorded review round never completed, so zero findings proves nothing.'
    };
  if (!latest.summary_present)
    return {
      label: 'No review summary recorded',
      tone: 'blocked',
      detail: 'The latest recorded review round completed with a blank summary, so it is not clean.'
    };
  return {
    label: 'Review standing unknown',
    tone: 'blocked',
    detail: 'The server has not reported this review as clean.'
  };
}

const COMMAND_STATES: Record<CommandState, { label: string; tone: Tone }> = {
  passed: { label: 'Passed at the output commit', tone: 'clean' },
  passed_at_other_revision: { label: 'Passed at another revision', tone: 'blocked' },
  failed: { label: 'Failed', tone: 'failed' },
  no_result: { label: 'No result recorded', tone: 'cancelled' }
};
export function commandBadge(state: CommandState): { label: string; tone: Tone } {
  return COMMAND_STATES[state];
}

/** Configured-check standing. No configured commands is never reported as passing. */
export function checksVerdict(evidence: TaskEvidence | null): Verdict {
  if (!evidence) return UNKNOWN_VERDICT;
  const checks: CommandEvidence = evidence.required_commands;
  if (checks.state === 'not_configured')
    return {
      label: 'No checks configured',
      tone: 'cancelled',
      detail:
        "The task's saved execution configuration requires no verification commands, so no check evidence exists. This is not a pass."
    };
  const total = checks.commands.length;
  if (checks.all_passed_at_output_revision)
    return {
      label: `All ${total} passed at the output commit`,
      tone: 'clean',
      detail:
        'Every configured command has a latest recorded result that succeeded at the recorded output commit.'
    };
  const counts = new Map<CommandState, number>();
  for (const command of checks.commands)
    counts.set(command.state, (counts.get(command.state) ?? 0) + 1);
  const passed = counts.get('passed') ?? 0;
  return {
    label: `${passed} of ${total} passed at the output commit`,
    tone: (counts.get('failed') ?? 0) > 0 ? 'failed' : 'blocked',
    detail: [...counts]
      .filter(([state]) => state !== 'passed')
      .map(([state, count]) => `${count} ${COMMAND_STATES[state].label.toLowerCase()}`)
      .join(', ')
  };
}

/**
 * The recorded pull-request reference. A saved URL describes delivery at publication
 * time; it is never presented as a fresh observation of the GitHub head.
 */
export function prVerdict(evidence: TaskEvidence | null): Verdict {
  if (!evidence) return UNKNOWN_VERDICT;
  const pr = evidence.pull_request;
  if (!pr)
    return {
      label: 'No pull request recorded',
      tone: 'cancelled',
      detail: 'No pull-request reference is saved for this task.'
    };
  return {
    label: pr.number === null ? 'Recorded PR · number unavailable' : `#${pr.number}`,
    tone: 'clean',
    detail: `Saved task reference (${pr.source}). Delivery, not merge, and not a fresh observation of the GitHub head.`
  };
}

/** Resolves one task's evidence inside a run, keyed on the task identity. */
export function findTaskEvidence(run: RunEvidenceV1, taskId: string): TaskEvidence | null {
  for (const proposal of run.proposals)
    for (const task of proposal.linked_tasks) if (task.id === taskId) return task;
  return null;
}

export function shortCommit(value: string | null): string {
  return value ? value.slice(0, 12) : 'None recorded';
}

/**
 * The planning-only outcome word for a cycle. `completed` means planning finished and
 * decisions are recorded; it never means the run's work is complete.
 */
export function planningVerdict(cycle: { status: string; mode: 'execution' | 'audit' }): Verdict {
  const noun = cycle.mode === 'audit' ? 'Audit' : 'Planning';
  const outputs = cycle.mode === 'audit' ? 'recommendations' : 'decisions';
  switch (cycle.status) {
    case 'completed':
      return {
        label: `${noun} complete`,
        tone: 'clean',
        detail: `Proposal ${outputs} are recorded. Planning completion is not task completion.`
      };
    case 'running':
      return {
        label: `${noun} in progress`,
        tone: 'running',
        detail: `${noun} is still running. ${outputs[0].toUpperCase()}${outputs.slice(1)} are recorded when it finishes.`
      };
    case 'failed':
      return {
        label: `${noun} failed`,
        tone: 'failed',
        detail: `${noun} ended with a recorded error. Anything saved before the failure is shown as recorded.`
      };
    case 'interrupted':
      return {
        label: `${noun} interrupted`,
        tone: 'blocked',
        detail: `${noun} was interrupted by a service stop and did not finish.`
      };
    default:
      return {
        label: `${noun} ${cycle.status}`,
        tone: '',
        detail: 'The saved cycle status, verbatim.'
      };
  }
}

/** Decision counts in a fixed order, so accepted never trades places with deferred. */
export function decisionCounts(
  decisions: Record<string, number>
): { decision: string; count: number; tone: Tone }[] {
  const extra = Object.keys(decisions)
    .filter((key) => !DECISIONS.some((decision) => decision === key))
    .sort();
  return [...DECISIONS.filter((key) => key in decisions), ...extra].map((decision) => ({
    decision,
    count: decisions[decision] ?? 0,
    tone: decisionTone(decision)
  }));
}

const OUTCOME_GROUPS: { label: (count: number) => string; tone: Tone; statuses: string[] }[] = [
  {
    label: (n) => (n === 1 ? 'published PR' : 'published PRs'),
    tone: 'clean',
    statuses: ['published']
  },
  {
    label: () => 'active',
    tone: 'running',
    statuses: ACTIVE_STATUSES
  },
  { label: () => 'queued', tone: '', statuses: ['queued'] },
  { label: () => 'blocked', tone: 'blocked', statuses: ['blocked'] },
  { label: () => 'failed', tone: 'failed', statuses: ['failed'] },
  { label: () => 'cancelled', tone: 'cancelled', statuses: ['cancelled'] }
];
/** Task outcomes grouped for a run summary. Published counts delivered PRs, never merges. */
export function taskOutcomeCounts(
  tasks: { status: string }[]
): { label: string; count: number; tone: Tone }[] {
  return OUTCOME_GROUPS.map((group) => {
    const count = tasks.filter((task) => group.statuses.includes(task.status)).length;
    return { label: group.label(count), count, tone: group.tone };
  }).filter((group) => group.count > 0);
}

/** Why a configured command's state is what it is, naming the revisions involved. */
export function commandExplanation(command: CommandResult, output: string | null): string {
  const at = command.latest_revision ? shortCommit(command.latest_revision) : null;
  switch (command.state) {
    case 'passed':
      return `Latest recorded result passed at the recorded output commit${at ? ` ${at}` : ''}.`;
    case 'passed_at_other_revision':
      return `Latest recorded result passed at ${at ?? 'an unrecorded revision'}, not at the recorded output commit${output ? ` ${shortCommit(output)}` : ''}. A pass at another revision does not count.`;
    case 'failed':
      return `Latest recorded result failed${at ? ` at ${at}` : ''}. A newer failure invalidates any older pass.`;
    case 'no_result':
      return 'Configured, with no result recorded. Not passing.';
  }
}
