export type Backend = 'codex' | 'opencode';
export type Route = {
  backend: Backend;
  model: string;
  effort: string;
  provider?: string | null;
  variant?: string | null;
};
export type Config = {
  repository: string;
  github_repo: string;
  default_branch: string;
  branch_prefix: string;
  codex_binary: string;
  opencode_binary: string;
  roles: Record<string, Route>;
  tiers: Record<string, Route>;
  repair_route: Route;
  categories: string[];
  verification_commands: string[];
  discovery_agents: number;
  execution_concurrency: number;
  cycle_interval_seconds: number;
  maintenance_every_cycles: number;
  large_pr_lines: number;
  long_lived_pr_days: number;
  max_tasks_per_cycle: number;
  max_repair_rounds: number;
  max_no_progress_rounds: number;
  max_retries: number;
  session_timeout_seconds: number;
  task_timeout_seconds: number;
  command_timeout_seconds: number;
  max_sessions_per_day: number;
  max_open_prs: number;
  max_workspace_bytes: number;
  runner_storage_paths: Record<string, string>;
  retain_completed_days: number;
  retain_events: number;
};
export type TaskRow = {
  id: string;
  cycle_id: string;
  title: string;
  category: string;
  tier: string;
  target: string;
  branch: string;
  status: string;
  pr_url: string | null;
  pr_number: number | null;
  error: string | null;
  created_at: string;
  updated_at: string;
  blocked_reason?: string | null;
  lifecycle: { archived_at?: string | null; discarded_at?: string | null };
  superseded_by?: string[];
};
export type Proposal = {
  id: string;
  title: string;
  problem: string;
  benefit: string;
  scope: string;
  target: string;
  tier: string;
  evidence: string[];
  dependencies: string[];
  prompt: string;
  category: string;
  decision: string;
  reason: string;
};
export type Session = {
  id: string;
  role: string;
  route: Route;
  status: string;
  started_at: string;
  summary: string;
};
export type ReviewRound = {
  session_id: string;
  revision: string;
  comparison_base: string;
  created_at: string;
  result: {
    completed: boolean;
    summary: string;
    findings: { title: string; file: string; detail: string; priority: string }[];
  };
};
export type AttemptPolicy = Pick<
  Config,
  | 'max_repair_rounds'
  | 'max_no_progress_rounds'
  | 'max_retries'
  | 'task_timeout_seconds'
  | 'session_timeout_seconds'
  | 'command_timeout_seconds'
>;
export type Page<T> = { items: T[]; next_cursor: number | null; counts: Record<string, number> };
export type ProposalDetail = Proposal & { content_revision: number };
export type ProposalRow = ProposalDetail & {
  cycle: number;
  cycle_id: string;
  mode: 'execution' | 'audit';
  detail?: ProposalDetail;
  detailRequested?: boolean;
  detailLoading?: number;
};
export type PrObservation = {
  repository: string;
  pr: PR;
  observed_at: string;
  delivered_head: string | null;
  external_head_movement: boolean;
};
export type CycleSummary = Pick<
  Cycle,
  'id' | 'number' | 'mode' | 'status' | 'started_at' | 'completed_at' | 'error'
> & {
  session_count: number;
  decisions: Record<string, number>;
  lifecycle: { archived_at?: string | null; discarded_at?: string | null };
};
export type Task = Omit<TaskRow, 'title' | 'target' | 'tier' | 'category'> & {
  proposal: Proposal;
  allowed_actions: string[];
  config: Config;
  effective_attempt_policy: AttemptPolicy;
  operating_policy: Pick<Config, 'max_sessions_per_day' | 'max_workspace_bytes'>;
  rediscovery_requested: boolean;
  rediscovery_result: string | null;
  supersedes: string[];
  route: Route;
  source_revision: string;
  comparison_base: string;
  workspace: string;
  output_commit: string | null;
  execution_session: string | null;
  repair_session: string | null;
  sessions: Session[];
  reviews: ReviewRound[];
  attempts: number;
  review_baseline: number;
  verification: {
    command: string;
    success: boolean;
    output: string;
    revision: string;
    created_at: string;
  }[];
};
export type PR = {
  number: number;
  title: string;
  branch: string;
  head: string;
  base: string;
  url: string;
  body: string;
  state: string;
  changed_lines: number;
  created_at: string;
  owned: boolean;
  head_repository: string;
  base_repository: string;
};
export type ExternalPrContext = {
  number: number;
  url: string;
  title: string;
  body: string;
  branch: string;
  head: string;
  base: string;
  head_repository: string;
  base_repository: string;
  title_truncated: boolean;
  body_truncated: boolean;
};
export type PrCoverage = {
  observed_at: string | null;
  complete: boolean;
  total_open: number;
  total_external: number;
  included_external: number;
  omitted_external: number;
  max_external: number;
  max_title_chars: number;
  max_body_chars: number;
  max_context_bytes: number;
};
export type Cycle = {
  mode: 'execution' | 'audit';
  id: string;
  number: number;
  status: string;
  started_at: string;
  completed_at: string | null;
  proposals: Proposal[];
  error: string | null;
  sessions: Session[];
  assessments: unknown[];
  grounding: {
    revision: string;
    prs: PR[];
    external_prs?: ExternalPrContext[];
    pr_coverage?: PrCoverage;
    maintenance_due: boolean;
    maintenance_targets: string[];
  } | null;
};
/**
 * GET /api/cycles/{id}/evidence and `--export-run <cycle-id>`. Mirrors src/evidence.rs.
 * Recorded review and check evidence only: no live HEAD, workspace, remote,
 * authorization or current PR state is inspected, and free text still requires
 * manual review before sharing.
 */
export type VerdictState = 'recorded' | 'missing' | 'duplicate' | 'malformed';
export type ReviewerVerdict = {
  reviewer: string;
  state: VerdictState;
  decision: string | null;
  reason: string | null;
  note: string | null;
};
export type EvidenceRevisions = {
  source: string;
  comparison_base: string | null;
  default_branch: string;
  output: string | null;
};
export type SessionRoute = {
  id: string;
  role: string;
  status: string;
  requested_route: Route;
  started_at: string;
};
export type FindingEvidence = { title: string; file: string; priority: string; detail: string };
export type ReviewRoundEvidence = {
  session_id: string;
  revision: string;
  comparison_base: string;
  created_at: string;
  completed: boolean;
  summary_present: boolean;
  matches_output_revision: boolean | null;
  findings: FindingEvidence[];
};
export type ReviewEvidence = {
  rounds_recorded: number;
  latest: ReviewRoundEvidence | null;
  clean: boolean;
  clean_at_output_revision: boolean;
};
export type CommandState = 'passed' | 'passed_at_other_revision' | 'failed' | 'no_result';
export type CommandResult = {
  command: string;
  state: CommandState;
  results_recorded: number;
  latest_success: boolean | null;
  latest_revision: string | null;
  latest_created_at: string | null;
  matches_output_revision: boolean | null;
};
export type CommandEvidence = {
  state: 'not_configured' | 'recorded';
  commands: CommandResult[];
  all_passed_at_output_revision: boolean;
};
export type PrReference = { number: number | null; url: string | null; source: string };
export type TaskEvidence = {
  id: string;
  cycle_id: string;
  proposal_id: string;
  status: string;
  branch: string;
  attempts: number;
  blocked_reason: string | null;
  error_recorded: boolean;
  created_at: string;
  updated_at: string;
  revisions: EvidenceRevisions;
  sessions: SessionRoute[];
  latest_review: ReviewEvidence;
  required_commands: CommandEvidence;
  pull_request: PrReference | null;
  gaps: string[];
};
export type ProposalEvidence = {
  id: string;
  title: string;
  target: string;
  tier: string;
  category: string;
  problem: string;
  benefit: string;
  scope: string;
  evidence: string[];
  final_decision: string;
  final_reason: string;
  reviewer_verdicts: ReviewerVerdict[];
  /** Zero or many: every task matching (cycle_id, proposal_id) is preserved. */
  linked_tasks: TaskEvidence[];
  gaps: string[];
};
export type PlanningOutcome = {
  status: string;
  planning_finished: boolean;
  proposal_count: number;
  decisions: Record<string, number>;
  creates_execution_queue: boolean;
  error_recorded: boolean;
  reviewer_batches_saved: number;
};
export type CycleEvidence = {
  id: string;
  number: number;
  mode: 'execution' | 'audit';
  status: string;
  started_at: string;
  completed_at: string | null;
  repository: string;
  grounding_revision: string | null;
  planning: PlanningOutcome;
};
export type RunEvidenceV1 = {
  schema_version: number;
  generated_at: string;
  kind: 'recorded_review_check_evidence';
  review_required_before_sharing: boolean;
  review_requirement: string;
  limitations: string[];
  cycle: CycleEvidence;
  proposals: ProposalEvidence[];
  gaps: string[];
};
export type Event = { id: number; at: string; entity_id: string; kind: string; message: string };
export type PrCapacity = {
  limit: number;
  owned_open: number | null;
  reserved: number;
  remaining: number | null;
  observed_at: string | null;
  status: string;
  reason: string | null;
};
export type PlanningCapacity = {
  day: string;
  limit: number;
  used: number;
  remaining: number;
  required: number;
  next_reset_at: number;
  status: 'ready' | 'daily_exhausted' | 'limit_too_low';
};
export type BaselineStatus =
  'running' | 'passed' | 'failed' | 'cancelled' | 'timed_out' | 'interrupted';
export type BaselineCommand = {
  command: string;
  success: boolean;
  output: string;
  output_truncated: boolean;
  created_at: string;
};
export type BaselineCheck = {
  id: string;
  status: BaselineStatus;
  config: Config;
  config_fingerprint: string;
  revision: string | null;
  started_at: string;
  completed_at: string | null;
  commands: BaselineCommand[];
  error: string | null;
  workspace_removed: boolean;
  cleanup_error: string | null;
};
export type DefaultBranchObservation = {
  repository: string;
  default_branch: string;
  revision: string;
  observed_at: string;
};
export type BaselineView = {
  check: BaselineCheck | null;
  eligible: boolean;
  reason: string | null;
  config_matches: boolean | null;
  revision_status: 'matches_last_observation' | 'stale' | 'unknown';
  default_observation: DefaultBranchObservation | null;
  caveat: string;
};
export type BaselineSummary = {
  id: string;
  status: BaselineStatus;
  started_at: string;
  completed_at: string | null;
  error: string | null;
  config_matches: boolean;
  revision_status: 'matches_last_observation' | 'stale' | 'unknown';
};
export type NotificationHealth = {
  state: 'disabled' | 'invalid' | 'enabled';
  configured: boolean;
  pending: number;
  failed: number;
  last_delivered_at: string | null;
  last_error: string | null;
  last_http_status: number | null;
};
export type Snapshot = {
  status: string;
  control: {
    paused: boolean;
    mode: 'paused' | 'run_once' | 'continuous';
    cycle_number: number;
    next_cycle_at: number;
    error: string | null;
    idle_streak: number;
  };
  repository: string;
  configured: boolean;
  audit_configured: boolean;
  active_cycle_mode: 'execution' | 'audit' | null;
  active_tasks: number;
  cycle_active: boolean;
  baseline_active: boolean;
  baseline: BaselineSummary | null;
  notifications: NotificationHealth;
  sessions_today: number;
  session_limit: number;
  planning_capacity: PlanningCapacity;
  pr_capacity: PrCapacity;
  tasks: TaskRow[];
  counts: Record<string, number>;
  attention_tasks: TaskRow[];
  merged_prs: number;
  storage_limit: number;
  storage: {
    measured_at: string;
    application_bytes: number;
    task_bytes: number;
    planning_bytes: number;
    runner_transcripts: {
      bytes: number | null;
      status: string;
      message: string;
      runners: Record<string, { bytes: number | null; status: string }>;
    };
  } | null;
  cycles: CycleSummary[];
  prs: PrObservation[];
  events: Event[];
};
export type Model = {
  backend: Backend;
  provider: string | null;
  provider_name: string | null;
  model: string;
  display_name: string;
  efforts: string[];
  variants: string[];
  available: boolean;
  unavailable_reason: string | null;
};
export type ModelCatalog = { binary: string; models: Model[]; loaded: boolean; error?: string };
