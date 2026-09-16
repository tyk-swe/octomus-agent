// The public boundary accepts a deliberately authored wrapper, never an operator export.
// This is a shape/consistency gate over `RunEvidenceV1` evidence, not an exporter or redactor.
export const REVIEW_WARNING =
  'Requires review before sharing. This is a private operator export of saved records, not a public-safe or publication-approved artifact.';
export const LIMITATIONS = [
  'Recorded review and check evidence only. No live HEAD, workspace, remote, authorization or current pull-request checks were performed while producing this export.',
  'Planning completion is not task completion: a completed cycle records decisions, not delivered work.',
  'Deferred is not rejected.',
  'A recorded pull request describes delivery, not merge. Published is not merged.',
  'Audit acceptance is a recommendation. Audit cycles never create an execution queue, so an accepted audit proposal has no linked task by design.',
  'Saved session routes are requested routes. Runtime model identity is not independently reported here.',
  'Costs, delivery time and any replay timeline are not inferred from these records.',
  "Zero or multiple task matches are preserved as recorded. No single task is selected on the caller's behalf.",
  'Free text carried here (proposal problem, benefit, scope and evidence, and code-review findings) is model-authored and still requires manual review before sharing.'
];
/** @param {any} schema */
const nullable = (schema) => ({ nullable: schema });
/** @param {any} schema */
const optional = (schema) => ({ optional: schema });
/** @param {...any} values */
const choices = (...values) => ({ choices: values });
const texts = ['string'];
const verdict = {
  reviewer: choices('adversary-a', 'adversary-b'),
  state: choices('recorded', 'missing', 'duplicate', 'malformed'),
  decision: nullable(choices('accepted', 'rejected', 'deferred')),
  reason: nullable('string'),
  note: nullable('string')
};
const command = {
  command: 'string',
  state: choices('passed', 'passed_at_other_revision', 'failed', 'no_result'),
  results_recorded: 'count',
  latest_success: nullable('boolean'),
  latest_revision: nullable('string'),
  latest_created_at: nullable('string'),
  matches_output_revision: nullable('boolean')
};
const task = {
  id: 'string',
  cycle_id: 'string',
  proposal_id: 'string',
  status: choices(
    'queued',
    'executing',
    'reviewing',
    'repairing',
    'verifying',
    'publishing',
    'published',
    'blocked',
    'failed',
    'cancelled'
  ),
  branch: 'string',
  attempts: 'count',
  blocked_reason: nullable('string'),
  error_recorded: 'boolean',
  created_at: 'string',
  updated_at: 'string',
  gaps: texts,
  revisions: {
    source: 'string',
    comparison_base: nullable('string'),
    default_branch: 'string',
    output: nullable('string')
  },
  sessions: [
    {
      id: 'string',
      role: 'string',
      status: 'string',
      started_at: 'string',
      requested_route: {
        backend: choices('codex', 'opencode'),
        model: 'string',
        effort: 'string',
        provider: optional(nullable('string')),
        variant: optional(nullable('string'))
      }
    }
  ],
  latest_review: {
    rounds_recorded: 'count',
    clean: 'boolean',
    clean_at_output_revision: 'boolean',
    latest: nullable({
      session_id: 'string',
      revision: 'string',
      comparison_base: 'string',
      created_at: 'string',
      completed: 'boolean',
      summary_present: 'boolean',
      matches_output_revision: nullable('boolean'),
      findings: [{ title: 'string', file: 'string', priority: 'string', detail: 'string' }]
    })
  },
  required_commands: {
    state: choices('not_configured', 'recorded'),
    commands: [command],
    all_passed_at_output_revision: 'boolean'
  },
  pull_request: nullable({
    number: nullable('count'),
    url: nullable('string'),
    source: choices('recorded_task_reference')
  })
};
const schema = {
  public_schema_version: choices(1),
  mode: choices('fixture', 'recorded'),
  evidence: {
    schema_version: choices(1),
    kind: choices('recorded_review_check_evidence'),
    generated_at: 'string',
    review_required_before_sharing: choices(true),
    review_requirement: choices(REVIEW_WARNING),
    limitations: texts,
    gaps: texts,
    cycle: {
      id: 'string',
      number: 'count',
      mode: choices('execution', 'audit'),
      status: 'string',
      started_at: 'string',
      completed_at: nullable('string'),
      repository: 'string',
      grounding_revision: nullable('string'),
      planning: {
        status: 'string',
        planning_finished: 'boolean',
        proposal_count: 'count',
        decisions: { dictionary: 'count' },
        creates_execution_queue: 'boolean',
        error_recorded: 'boolean',
        reviewer_batches_saved: 'count'
      }
    },
    proposals: [
      {
        id: 'string',
        title: 'string',
        target: 'string',
        tier: 'string',
        category: 'string',
        problem: 'string',
        benefit: 'string',
        scope: 'string',
        evidence: texts,
        final_decision: 'string',
        final_reason: 'string',
        reviewer_verdicts: [verdict],
        linked_tasks: [task],
        gaps: texts
      }
    ]
  }
};
/**
 * @param {unknown} condition
 * @param {string} path
 */
function requireFact(condition, path) {
  if (!condition) throw new Error(`Unsupported or inconsistent public input: ${path}`);
}
// Construct only allowlisted keys. Reject extras instead of silently dropping private or adverse data.
/**
 * Input arrives untrusted and unshaped, so it is typed as `any` on purpose:
 * the shape table below, not the type system, decides what may be carried.
 * @param {any} value
 * @param {any} shape
 * @param {string} path
 * @returns {any}
 */
function project(value, shape, path) {
  if (typeof shape === 'string') {
    requireFact(
      shape === 'count' ? Number.isSafeInteger(value) && value >= 0 : typeof value === shape,
      path
    );
    return value;
  }
  if (Array.isArray(shape)) {
    requireFact(Array.isArray(value), path);
    return value.map((/** @type {any} */ item, /** @type {number} */ i) =>
      project(item, shape[0], `${path}[${i}]`)
    );
  }
  if ('nullable' in shape) return value === null ? null : project(value, shape.nullable, path);
  if ('optional' in shape)
    return value === undefined ? undefined : project(value, shape.optional, path);
  if ('choices' in shape) {
    requireFact(shape.choices.includes(value), path);
    return value;
  }
  requireFact(value !== null && typeof value === 'object' && !Array.isArray(value), path);
  if ('dictionary' in shape)
    return Object.fromEntries(
      Object.entries(value).map(([k, v]) => [k, project(v, shape.dictionary, `${path}.${k}`)])
    );
  requireFact(
    Object.keys(value).every((key) => Object.hasOwn(shape, key)),
    `${path}: unallowlisted field`
  );
  return Object.fromEntries(
    Object.entries(shape).flatMap(([key, spec]) => {
      const result = project(value[key], spec, `${path}.${key}`);
      return result === undefined ? [] : [[key, result]];
    })
  );
}
/**
 * @param {any} value
 * @param {string} mode
 */
export function publicPayload(value, mode) {
  const payload = project(value, schema, 'payload');
  requireFact(payload.mode === mode, 'mode must match the explicit build mode');
  const run = payload.evidence,
    cycle = run.cycle,
    plan = cycle.planning;
  requireFact(
    LIMITATIONS.every((text) => run.limitations.includes(text)),
    'original limitations must remain'
  );
  requireFact(
    plan.status === cycle.status &&
      plan.proposal_count === run.proposals.length &&
      plan.creates_execution_queue === (cycle.mode === 'execution') &&
      plan.planning_finished === (cycle.completed_at !== null && cycle.status !== 'running'),
    'planning facts'
  );
  const counts = Object.fromEntries(
    [...new Set(run.proposals.map((/** @type {any} */ p) => p.final_decision))].map((d) => [
      d,
      run.proposals.filter((/** @type {any} */ p) => p.final_decision === d).length
    ])
  );
  requireFact(
    Object.entries(counts).every(([d, n]) => plan.decisions[d] === n) &&
      Object.entries(plan.decisions).every(([d, n]) => n === (counts[d] ?? 0)),
    'decision counts'
  );
  for (const p of run.proposals) {
    requireFact(
      p.reviewer_verdicts.length === 2 &&
        p.reviewer_verdicts[0].reviewer === 'adversary-a' &&
        p.reviewer_verdicts[1].reviewer === 'adversary-b',
      'fixed reviewer slots'
    );
    for (const v of p.reviewer_verdicts) {
      requireFact(
        v.state === 'recorded'
          ? v.decision !== null && v.reason !== null
          : v.reason === null && (v.state === 'duplicate' || v.decision === null),
        'reviewer state'
      );
    }
    for (const t of p.linked_tasks) {
      requireFact(t.cycle_id === cycle.id && t.proposal_id === p.id, 'task join identity');
      const review = t.latest_review,
        latest = review.latest,
        output = t.revisions.output;
      /** @param {string | null} revision */
      const matches = (revision) => (output === null ? null : output === revision);
      requireFact((review.rounds_recorded === 0) === (latest === null), 'latest review count');
      requireFact(
        review.clean ===
          !!(latest?.completed && latest.summary_present && latest.findings.length === 0),
        'review cleanliness'
      );
      requireFact(
        !latest || latest.matches_output_revision === matches(latest.revision),
        'review revision'
      );
      requireFact(
        review.clean_at_output_revision ===
          (review.clean && latest?.matches_output_revision === true),
        'review output standing'
      );
      const checks = t.required_commands;
      requireFact(
        (checks.state === 'not_configured') === (checks.commands.length === 0),
        'configured checks'
      );
      for (const c of checks.commands) {
        if (c.results_recorded === 0) {
          requireFact(
            c.state === 'no_result' &&
              [
                c.latest_success,
                c.latest_revision,
                c.latest_created_at,
                c.matches_output_revision
              ].every((v) => v === null),
            'missing check result'
          );
        } else {
          requireFact(
            c.latest_success !== null &&
              c.latest_revision !== null &&
              c.latest_created_at !== null &&
              c.matches_output_revision === matches(c.latest_revision),
            'latest check result'
          );
          requireFact(
            c.state ===
              (!c.latest_success
                ? 'failed'
                : c.matches_output_revision === true
                  ? 'passed'
                  : 'passed_at_other_revision'),
            'check state'
          );
        }
      }
      requireFact(
        checks.all_passed_at_output_revision ===
          (checks.commands.length > 0 &&
            checks.commands.every((/** @type {any} */ c) => c.state === 'passed')),
        'aggregate checks'
      );
    }
  }
  return payload;
}
/**
 * @param {any} value
 * @param {string} hash
 */
export function publicApproval(value, hash) {
  const approval = project(
    value,
    {
      approval_schema_version: choices(1),
      owner_reviewed: choices(true),
      payload_sha256: 'string',
      approval_reference: 'string'
    },
    'approval'
  );
  requireFact(
    /^[a-f0-9]{64}$/.test(approval.payload_sha256) && approval.payload_sha256 === hash,
    'approval hash mismatch'
  );
  requireFact(approval.approval_reference.trim().length > 0, 'approval reference is required');
  return approval;
}
