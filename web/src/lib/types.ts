// Generated from Go API and evidence types by api_contract_test.go.
// Regenerate: OCTOMUS_UPDATE_API_CONTRACT=1 go test ./web -run TestAPIContractGenerated

export type Acknowledgement = {
  ok: boolean;
};
export type ActionErrorCode = 'action_conflict' | 'config_revision_conflict' | 'active_work' | 'unresolved_work' | 'recovery_pending';
export type ApiFailure = {
  error: string;
  code?: ActionErrorCode;
};
export type AttemptPolicy = {
  max_repair_rounds: number;
  max_no_progress_rounds: number;
  max_retries: number;
  task_timeout_seconds: number;
  session_timeout_seconds: number;
  command_timeout_seconds: number;
};
export type AutoMergeSnapshot = {
  repository: string;
  pr_number: number;
  state: AutoMergeState;
};
export type AutoMergeState = {
  task_id: string;
  head: string;
  head_branch: string;
  base_branch: string;
  comparison_base: string;
  policy_revision: string;
  authorized: boolean;
  footprint: MaintenanceFootprint | null;
  status: AutoMergeStatus;
  reason: string;
  observed_at: string;
  attempt_id: string | null;
  attempted_at: string | null;
  merge_commit: string | null;
  result_source: string | null;
};
export type AutoMergeStatus = 'waiting' | 'manual' | 'merging' | 'uncertain' | 'merged' | 'closed';
export type AutoMergeView = {
  active: boolean;
  counts: Record<string, number>;
};
export type Backend = 'codex' | 'opencode';
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
export type BaselineCommand = {
  command: string;
  success: boolean;
  output: string;
  output_truncated: boolean;
  created_at: string;
  sandbox: SandboxRecord | null;
};
export type BaselineStatus = 'running' | 'passed' | 'failed' | 'cancelled' | 'timed_out' | 'interrupted';
export type BaselineSummary = {
  id: string;
  status: BaselineStatus;
  started_at: string;
  completed_at: string | null;
  error: string | null;
  config_revision: string;
  config_matches: boolean;
  revision_status: string;
};
export type BaselineView = {
  check: BaselineCheck | null;
  eligible: boolean;
  reason: string | null;
  config_matches: boolean | null;
  config_revision: string | null;
  revision_status: string;
  default_observation: DefaultBranchObservation | null;
  caveat: string;
};
export type BatchPhase = 'draining' | 'planning' | 'executing' | 'merging';
export type BlockedReason = 'budget_exhausted' | 'storage_limit' | 'stale_base' | 'remote_conflict' | 'publication_uncertain' | 'runner_unavailable' | 'invalid_review' | 'verification_failed' | 'dependency_blocked' | 'invalid_plan' | 'workspace_invalid' | 'retry_limit' | 'timeout' | 'unknown';
export type BrokerInfo = {
  instance_id: string;
  version: string;
  docker_version: string;
  api_version: string;
  image: string;
  image_id: string;
  image_digests: string[] | null;
  runtime: string;
  runners: Record<string, string> | null;
  runner_errors: Record<string, string> | null;
  limits: BrokerLimits;
  networks: BrokerNetworks;
  egress: boolean;
  gateway: GatewayPosture | null;
  live: number;
};
export type BrokerLimits = {
  nano_cpus: number;
  memory_bytes: number;
  pids: number;
  tmpfs_bytes: number;
  max_sandboxes: number;
  max_seconds: number;
};
export type BrokerNetworks = {
  runner: string;
  verify: string;
};
export type Capabilities = {
  pause: Capability;
  resume: Capability;
  cycle: Capability;
  audit: Capability;
  configure: Capability;
};
export type Capability = {
  allowed: boolean;
  reason: string;
};
export type ChecksState = 'not_configured' | 'recorded';
export type CommandEvidence = {
  state: ChecksState;
  commands: CommandResult[];
  all_passed_at_output_revision: boolean;
};
export type CommandResult = {
  command: string;
  state: CommandState;
  results_recorded: number;
  latest_success: boolean | null;
  latest_revision: string | null;
  latest_created_at: string | null;
  matches_output_revision: boolean | null;
};
export type CommandState = 'passed' | 'passed_at_other_revision' | 'failed' | 'no_result';
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
  delivery_mode: DeliveryMode;
  auto_merge_max_lines: number;
  auto_merge_max_files: number;
  auto_merge_excluded_paths: string[];
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
export type Control = {
  paused: boolean;
  cycle_number: number;
  next_cycle_at: number;
  error: string | null;
  mode: OperatingMode;
  batch: RunBatch | null;
  idle_streak: number;
  context_fingerprint: string;
};
export type ControlResult = {
  paused: boolean;
  cycle_number: number;
  next_cycle_at: number;
  error: string | null;
  mode: OperatingMode;
  batch: RunBatch | null;
  idle_streak: number;
  context_fingerprint: string;
  planning_capacity?: PlanningCapacity | null;
};
export type Cycle = {
  mode: CycleMode;
  delivery_mode: DeliveryMode;
  id: string;
  number: number;
  status: CycleStatus;
  started_at: string;
  completed_at: string | null;
  grounding: Grounding | null;
  proposals: Proposal[];
  assessments: unknown[];
  sessions: Session[];
  error: string | null;
  repository?: string;
  decision_memory?: DecisionRecord[];
  run_id?: string | null;
  lifecycle?: WorkspaceLifecycle;
};
export type CycleAction = 'archive' | 'discard';
export type CycleEvidence = {
  id: string;
  number: number;
  mode: CycleMode;
  delivery_mode: DeliveryMode;
  status: CycleStatus;
  started_at: string;
  completed_at: string | null;
  repository: string;
  grounding_revision: string | null;
  planning: PlanningOutcome;
};
export type CycleMode = 'execution' | 'audit';
export type CycleStatus = 'running' | 'completed' | 'idle' | 'failed' | 'interrupted';
export type CycleSummary = {
  id: string;
  number: number;
  mode: CycleMode;
  status: CycleStatus;
  started_at: string;
  completed_at: string | null;
  error: string | null;
  session_count: number;
  decisions: DecisionCounts;
  lifecycle: WorkspaceLifecycle;
  allowed_actions: CycleAction[];
};
export type DecisionCounts = {
  accepted: number;
  rejected: number;
  deferred: number;
  candidate: number;
};
export type DecisionRecord = {
  kind?: string;
  id: string;
  mode: CycleMode;
  repository: string;
  target: string;
  problem_key: string;
  relevant_paths: string[];
  decision: string;
  reason: string;
  source_revision: string;
  context_fingerprint: string;
  reconsider_after: string;
  cycle_id: string;
  reconsideration_due?: boolean;
};
export type DefaultBranchObservation = {
  repository: string;
  default_branch: string;
  revision: string;
  observed_at: string;
};
export type DeliveryMode = 'standard' | 'maintenance';
export type Diagnostics = {
  backend: Backend;
  protocol_version: string;
  version: string;
  warning: string | null;
};
export type DoctorFailure = {
  error: string;
  code?: ActionErrorCode;
  checked_config: Config;
  checked_revision: string;
};
export type DoctorResponse = {
  ok: boolean;
  mode: CycleMode;
  models: Model[];
  backends: Diagnostics[];
  warnings: string[];
  message: string;
  sandbox: DoctorSandbox;
  codex_version?: string;
  tested_codex_version?: string;
  checked_config: Config;
  checked_revision: string;
};
export type DoctorSandbox = {
  mode: string;
  self_test?: SandboxSelfTest | null;
};
export type Event = {
  id: number;
  at: string;
  entity_id: string;
  kind: EventKind;
  message: string;
};
export type EventKind = 'attempt_policy' | 'automerge' | 'baseline' | 'baseline_error' | 'cleanup_error' | 'configuration' | 'error' | 'grounding' | 'housekeeping_error' | 'interrupted' | 'maintenance' | 'operator' | 'planning_capacity' | 'planning_error' | 'recovery' | 'recovery_error' | 'run_complete' | 'sandbox_evidence' | 'session_progress' | 'session_started' | 'status' | 'upgrade' | 'worker_error';
export type EvidenceRevisions = {
  source: string;
  comparison_base: string | null;
  default_branch: string;
  output: string | null;
};
export type ExternalPRContext = {
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
  local_head_available: boolean;
};
export type Finding = {
  title: string;
  file: string;
  detail: string;
  priority: string;
};
export type FindingEvidence = {
  title: string;
  file: string;
  priority: string;
  detail: string;
};
export type GatewayPosture = {
  instance_id: string;
  policy_fingerprint: string;
};
export type Grounding = {
  revision: string;
  prs: PullRequest[];
  external_prs: ExternalPRContext[];
  pr_coverage: PRCoverage;
  history: unknown;
  maintenance_due: boolean;
  maintenance_targets: string[];
};
export type HealthResponse = {
  ok: boolean;
  version: string;
};
export type MaintenanceAssessment = {
  qualifies: boolean;
  manual_merge_required: boolean;
  reason: string;
};
export type MaintenanceFootprint = {
  comparison_base: string;
  revision: string;
  changed_lines: number | null;
  changed_files: number | null;
  paths: string[];
  complete: boolean;
  manual_reasons: string[];
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
export type NotificationHealth = {
  state: string;
  configured: boolean;
  pending: number;
  failed: number;
  last_delivered_at: string | null;
  last_error: string | null;
  last_http_status: number | null;
};
export type OperatingMode = 'paused' | 'run_once' | 'continuous';
export type OperatingPolicy = {
  max_sessions_per_day: number;
  max_workspace_bytes: number;
};
export type PR = {
  number: number;
  title: string;
  branch: string;
  head: string;
  base: string;
  url: string;
  state: string;
  changed_lines: number;
  created_at: string;
  owned: boolean;
  head_repository: string;
  base_repository: string;
  review_decision: string;
  check_status: string;
  mergeability: string;
  status_source: string;
  status_observed_at: string;
};
export type PRCapacity = {
  limit: number;
  owned_open: number | null;
  reserved: number;
  remaining: number | null;
  observed_at: string | null;
  status: string;
  reason: string | null;
};
export type PRCapacityStatus = 'ready' | 'daily_exhausted' | 'limit_too_low';
export type PRCoverage = {
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
export type PRObservation = {
  repository: string;
  pr: PR;
  observed_at: string;
  delivered_head: string | null;
  external_head_movement: boolean;
  auto_merge: AutoMergeState | null;
};
export type PRReference = {
  number: number | null;
  url: string | null;
  source: string;
};
export type PlanningCapacity = {
  day: string;
  limit: number;
  used: number;
  remaining: number;
  required: number;
  next_reset_at: number;
  status: PRCapacityStatus;
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
export type ProbeCheck = {
  id: string;
  label: string;
  passed: boolean;
  detail: string;
};
export type Proposal = {
  id: string;
  title: string;
  problem: string;
  evidence: string[];
  benefit: string;
  category: string;
  target: string;
  tier: string;
  scope: string;
  dependencies: string[];
  prompt: string;
  decision: string;
  reason: string;
  problem_key: string;
  relevant_paths: string[];
  reconsiders: string[];
};
export type ProposalDetail = {
  id: string;
  title: string;
  problem: string;
  evidence: string[];
  benefit: string;
  category: string;
  target: string;
  tier: string;
  scope: string;
  dependencies: string[];
  prompt: string;
  decision: string;
  reason: string;
  problem_key: string;
  relevant_paths: string[];
  reconsiders: string[];
  content_revision: number;
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
  linked_tasks: TaskEvidence[];
  gaps: string[];
};
export type ProposalRow = {
  id: string;
  title: string;
  problem: string;
  evidence: string[];
  benefit: string;
  category: string;
  target: string;
  tier: string;
  scope: string;
  dependencies: string[];
  prompt: string;
  decision: string;
  reason: string;
  problem_key: string;
  relevant_paths: string[];
  reconsiders: string[];
  content_revision: number;
  cycle: number;
  cycle_id: string;
  mode: CycleMode;
};
export type PullRequest = {
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
  review_decision: string;
  check_status: string;
  mergeability: string;
  status_source: string;
  status_observed_at: string;
};
export type RepairProgress = {
  revision: string;
  no_progress_rounds: number;
  awaiting_review: boolean;
};
export type Review = {
  completed: boolean;
  summary: string;
  findings: Finding[];
};
export type ReviewEvidence = {
  rounds_recorded: number;
  latest: ReviewRoundEvidence | null;
  clean: boolean;
  clean_at_output_revision: boolean;
};
export type ReviewRound = {
  session_id: string;
  revision: string;
  comparison_base: string;
  result: Review;
  created_at: string;
  maintenance: MaintenanceAssessment | null;
  trusted_diff_complete: boolean;
};
export type ReviewRoundEvidence = {
  session_id: string;
  revision: string;
  comparison_base: string;
  created_at: string;
  completed: boolean;
  summary_present: boolean;
  matches_output_revision: boolean | null;
  maintenance: MaintenanceAssessment | null;
  trusted_diff_complete: boolean;
  findings: FindingEvidence[];
};
export type ReviewerVerdict = {
  reviewer: string;
  state: VerdictState;
  decision: string | null;
  reason: string | null;
  note: string | null;
};
export type Route = {
  backend: Backend;
  model: string;
  effort: string;
  provider?: string | null;
  variant?: string | null;
};
export type RunBatch = {
  id: string;
  phase: BatchPhase;
  cycle_id: string | null;
};
export type RunEvidenceV1 = {
  schema_version: number;
  generated_at: string;
  kind: string;
  review_required_before_sharing: boolean;
  review_requirement: string;
  limitations: string[];
  cycle: CycleEvidence;
  proposals: ProposalEvidence[];
  gaps: string[];
};
export type RunnerStorage = {
  bytes: number | null;
  status: string;
};
export type RunnerTranscripts = {
  bytes: number | null;
  message: string;
  runners: Record<string, RunnerStorage>;
  status: string;
};
export type SandboxEgress = {
  allowed: Record<string, number>;
  denied: Record<string, number>;
  failed: Record<string, number>;
};
export type SandboxPosture = {
  mode: string;
  healthy: boolean;
  error: string | null;
  broker: BrokerInfo | null;
  egress: Record<string, string[]> | null;
  pinned_repository: string | null;
  self_test: SandboxSelfTest | null;
};
export type SandboxRecord = {
  image_id: string;
  runtime: string;
  runs: number;
  oom: boolean;
  incomplete: boolean;
  egress: SandboxEgress;
};
export type SandboxSelfTest = {
  at: string;
  passed: boolean;
  checks: ProbeCheck[];
  kernel: string;
  image_id: string;
  runtime?: string;
  posture_fingerprint?: string | null;
  error: string | null;
};
export type Session = {
  id: string;
  role: string;
  route: Route;
  status: SessionStatus;
  started_at: string;
  summary: string;
  first_turn_started: boolean | null;
  sandbox: SandboxRecord | null;
};
export type SessionRoute = {
  id: string;
  role: string;
  status: SessionStatus;
  requested_route: Route;
  started_at: string;
};
export type SessionStatus = 'running' | 'completed' | 'failed' | 'interrupted';
export type SettingsView = {
  config: Config;
  revision: string;
  transformed_fields: TransformedField[];
};
export type Snapshot = {
  tasks: TaskRow[];
  cycles: CycleSummary[];
  prs: PRObservation[];
  events: Event[];
  counts: Record<string, number>;
  attention_tasks: TaskRow[];
  merged_prs: number;
  sessions_today: number;
  capabilities: Capabilities;
  status: string;
  recovery_error: string | null;
  control: Control;
  repository: string;
  delivery_mode: DeliveryMode;
  auto_merge: AutoMergeView;
  configured: boolean;
  audit_configured: boolean;
  active_cycle_mode: CycleMode | null;
  active_tasks: number;
  cycle_active: boolean;
  baseline_active: boolean;
  baseline: BaselineSummary | null;
  notifications: NotificationHealth;
  session_limit: number;
  storage_limit: number;
  storage: StorageUsage | null;
  planning_capacity: PlanningCapacity;
  pr_capacity: PRCapacity;
  sandbox: SandboxPosture;
};
export type StorageUsage = {
  measured_at: string;
  application_bytes: number;
  task_bytes: number;
  planning_bytes: number;
  runner_transcripts: RunnerTranscripts;
};
export type Task = {
  id: string;
  cycle_id: string;
  proposal: Proposal;
  status: TaskStatus;
  route: Route;
  config: Config;
  source_revision: string;
  comparison_base: string;
  default_revision: string;
  branch: string;
  workspace: string;
  execution_session: string | null;
  repair_session: string | null;
  sessions: Session[];
  reviews: ReviewRound[];
  verification: Verification[];
  output_commit: string | null;
  pr_number: number | null;
  pr_url: string | null;
  attempts: number;
  error: string | null;
  created_at: string;
  updated_at: string;
  attempt_policy: AttemptPolicy | null;
  review_baseline: number;
  repair_rounds: number | null;
  repair_progress: RepairProgress | null;
  blocked_reason: BlockedReason | null;
  run_id: string | null;
  superseded_by: string[];
  supersedes: string[];
  rediscovery_requested: boolean;
  rediscovery_result: string | null;
  lifecycle: WorkspaceLifecycle;
  maintenance_footprint: MaintenanceFootprint | null;
  auto_merge_snapshot: AutoMergeSnapshot | null;
  allowed_actions: TaskAction[];
  effective_attempt_policy: AttemptPolicy;
  operating_policy: OperatingPolicy;
};
export type TaskAction = 'cancel' | 'retry' | 'supersede' | 'archive' | 'discard' | 'reconcile';
export type TaskEvidence = {
  id: string;
  cycle_id: string;
  proposal_id: string;
  status: TaskStatus;
  branch: string;
  attempts: number;
  blocked_reason: string | null;
  error_recorded: boolean;
  created_at: string;
  updated_at: string;
  delivery_mode: DeliveryMode;
  maintenance_footprint: MaintenanceFootprint | null;
  auto_merge: AutoMergeState | null;
  revisions: EvidenceRevisions;
  sessions: SessionRoute[];
  latest_review: ReviewEvidence;
  required_commands: CommandEvidence;
  pull_request: PRReference | null;
  gaps: string[];
};
export type TaskRow = {
  id: string;
  cycle_id: string;
  title: string;
  category: string;
  tier: string;
  target: string;
  branch: string;
  status: TaskStatus;
  pr_url: string | null;
  pr_number: number | null;
  error: string | null;
  blocked_reason: BlockedReason | null;
  created_at: string;
  updated_at: string;
  lifecycle: WorkspaceLifecycle;
  superseded_by: string[];
};
export type TaskStatus = 'queued' | 'executing' | 'reviewing' | 'repairing' | 'verifying' | 'publishing' | 'published' | 'blocked' | 'failed' | 'cancelled';
export type TransformedField = {
  field: string;
  kinds: ('redacted' | 'shortened')[];
  paths: (string | number)[][];
};
export type VerdictState = 'recorded' | 'missing' | 'duplicate' | 'malformed';
export type Verification = {
  command: string;
  success: boolean;
  output: string;
  revision: string;
  created_at: string;
  sandbox: SandboxRecord | null;
};
export type WorkspaceLifecycle = {
  archived_at: string | null;
  discarded_at: string | null;
};

export type Page<T> = {
  items: T[];
  counts: Record<string, number>;
  next_cursor: number | null;
};
export type ModelCatalog = { binary: string; models: Model[]; loaded: boolean; error?: string };
export const ACTIVE_STATUSES: string[] = ['executing', 'reviewing', 'repairing', 'verifying', 'publishing'];
export const ERROR_EVENT_KINDS: EventKind[] = ['error', 'planning_error', 'worker_error', 'recovery_error', 'cleanup_error', 'housekeeping_error', 'baseline_error'];
