/**
 * Display mapping for `RunEvidenceV1` (see docs/launch/run-evidence.md).
 *
 * Every label here is derived from the server's already-normalized evidence statuses.
 * Nothing in this module recomputes an optimistic boolean from raw records: unknown,
 * stale and missing states are preserved and named. Kept in one module so the run
 * evidence panel and the task detail summary cannot describe the same record
 * differently.
 */
import type {
  CommandEvidence,
  CommandState,
  ReviewRoundEvidence,
  ReviewerVerdict,
  RunEvidenceV1,
  TaskEvidence,
  VerdictState
} from './types';

/** Existing badge tones in app.css. `cancelled` reads as "nothing recorded", not "fine". */
export type Tone = 'clean' | 'blocked' | 'failed' | 'running' | 'cancelled' | '';
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
  if (!outputCommit) return { label: 'No output commit recorded', tone: 'cancelled' };
  return revision === outputCommit
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

const UNKNOWN: Verdict = {
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
  if (!evidence) return UNKNOWN;
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
      label: 'Clean at another revision',
      tone: 'blocked',
      detail: `The latest of ${rounds} is clean but did not run at the recorded output commit.`
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
    label: 'Clean, revision unknown',
    tone: 'blocked',
    detail: 'The latest round is clean but cannot be compared to an output commit.'
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
  if (!evidence) return UNKNOWN;
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
  if (!evidence) return UNKNOWN;
  const pr = evidence.pull_request;
  if (!pr || pr.number === null)
    return {
      label: 'No pull request recorded',
      tone: 'cancelled',
      detail: 'No pull-request reference is saved for this task.'
    };
  return {
    label: `#${pr.number}`,
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
