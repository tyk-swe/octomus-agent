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
    maintenance_due: boolean;
    maintenance_targets: string[];
  } | null;
};
export type Event = { id: number; at: string; entity_id: string; kind: string; message: string };
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
  sessions_today: number;
  session_limit: number;
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
