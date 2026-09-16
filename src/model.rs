use crate::config::{Config, Route};
use serde::{Deserialize, Serialize};
use serde_json::Value;

pub fn now() -> String {
    chrono::Utc::now().to_rfc3339()
}
/// The UTC calendar day (`%F`) containing `at`. Daily admission budgets bucket by
/// this day, so every site derives it identically.
pub fn utc_day(at: impl Into<chrono::DateTime<chrono::Utc>>) -> String {
    at.into().format("%F").to_string()
}
/// Today's UTC day.
pub fn today() -> String {
    utc_day(chrono::Utc::now())
}
pub fn id() -> String {
    uuid::Uuid::new_v4().to_string()
}
/// Reviewer session labels in dispatch order. Planning writes sessions under these
/// names; run evidence attributes saved assessment batches by matching them.
pub const REVIEWER_SLOTS: [&str; 2] = ["adversary-a", "adversary-b"];
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    Queued,
    Executing,
    Reviewing,
    Repairing,
    Verifying,
    Publishing,
    Published,
    Blocked,
    Failed,
    Cancelled,
}
impl Status {
    pub fn active(&self) -> bool {
        matches!(
            self,
            Self::Executing
                | Self::Reviewing
                | Self::Repairing
                | Self::Verifying
                | Self::Publishing
        )
    }
    pub fn retryable(&self) -> bool {
        matches!(self, Self::Failed | Self::Blocked)
    }
    /// Statuses whose tasks hold a scheduler slot or branch writer lock. SQL projections
    /// and history filters derive their literal lists from this single vocabulary.
    pub const ACTIVE: [&'static str; 5] = [
        "executing",
        "reviewing",
        "repairing",
        "verifying",
        "publishing",
    ];
}
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Proposal {
    pub id: String,
    pub title: String,
    pub problem: String,
    pub evidence: Vec<String>,
    pub benefit: String,
    pub category: String,
    pub target: String,
    pub tier: String,
    pub scope: String,
    pub dependencies: Vec<String>,
    pub prompt: String,
    pub decision: String,
    pub reason: String,
    #[serde(default)]
    pub problem_key: String,
    #[serde(default)]
    pub relevant_paths: Vec<String>,
    #[serde(default)]
    pub reconsiders: Vec<String>,
}

impl Proposal {
    pub fn problem_identity(&self) -> String {
        problem_identity(&self.title, &self.problem_key)
    }
    /// Two proposals describe the same work when they share a target and either a title
    /// (case-insensitive) or a stable problem identity.
    pub fn same_work(&self, other: &Proposal) -> bool {
        self.target == other.target
            && (self.title.trim().eq_ignore_ascii_case(other.title.trim())
                || self.problem_identity() == other.problem_identity())
    }
}

pub(crate) fn problem_identity(title: &str, problem_key: &str) -> String {
    let key = if problem_key.trim().is_empty() {
        title
    } else {
        problem_key
    };
    key.trim().to_lowercase()
}
#[derive(Debug, Default, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum BlockedReason {
    BudgetExhausted,
    StorageLimit,
    StaleBase,
    RemoteConflict,
    PublicationUncertain,
    RunnerUnavailable,
    InvalidReview,
    VerificationFailed,
    DependencyBlocked,
    InvalidPlan,
    WorkspaceInvalid,
    RetryLimit,
    Timeout,
    #[default]
    Unknown,
}
impl BlockedReason {
    pub fn from_error(error: &anyhow::Error) -> Self {
        // anyhow contexts support typed downcasting but do not appear as their
        // context value in std::error::Error::source(). Prefer a concrete cause.
        error
            .chain()
            .filter_map(|cause| cause.downcast_ref::<Self>().copied())
            .last()
            .or_else(|| error.downcast_ref::<Self>().copied())
            .unwrap_or_default()
    }
}
impl std::fmt::Display for BlockedReason {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::BudgetExhausted => "Daily admission budget exhausted; adjust the current limit or wait until UTC midnight",
            Self::StorageLimit => "Storage admission limit reached; resolve retained workspaces or adjust the limit",
            Self::StaleBase => "Source or default branch moved; supersede this task and rediscover against current context",
            Self::RemoteConflict => "Remote branch moved outside recorded task outputs; reconcile the preserved work",
            Self::PublicationUncertain => "Publication result is uncertain; reconcile the preserved output commit",
            Self::RunnerUnavailable => "Runner request failed; inspect the saved route and runner diagnostics",
            Self::InvalidReview => "Incomplete or invalid review cannot authorize publication",
            Self::VerificationFailed => "Verification or repairs remain unresolved; evidence is preserved",
            Self::DependencyBlocked => "A dependency is unresolved; deliver it or rediscover dependent work",
            Self::InvalidPlan => "The saved dependency plan cannot execute; rediscover a valid task order",
            Self::WorkspaceInvalid => "Workspace initialization or recorded evidence is inconsistent; preserve and inspect it",
            Self::RetryLimit => "Attempt or repair limit exhausted; inspect evidence before adjusting attempt limits",
            Self::Timeout => "Task time limit exceeded; inspect the preserved workspace",
            Self::Unknown => "Unclassified task failure; inspect the recorded diagnostics",
        })
    }
}
impl std::error::Error for BlockedReason {}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum PlanningCapacityStatus {
    Ready,
    DailyExhausted,
    LimitTooLow,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PlanningCapacity {
    pub day: String,
    pub limit: u64,
    pub used: u64,
    pub remaining: u64,
    pub required: u64,
    pub next_reset_at: i64,
    pub status: PlanningCapacityStatus,
}
impl PlanningCapacity {
    pub fn available(&self) -> bool {
        self.status == PlanningCapacityStatus::Ready
    }
    pub fn message(&self) -> String {
        let guidance = match self.status {
            PlanningCapacityStatus::Ready => "Planning can start.",
            PlanningCapacityStatus::DailyExhausted => {
                "Wait until UTC midnight or increase the daily limit."
            }
            PlanningCapacityStatus::LimitTooLow => {
                "The configured daily limit cannot fund a complete planning pass; increase it."
            }
        };
        format!(
            "A complete planning pass requires {} daily admissions; {} remain ({} of {} used). {}",
            self.required, self.remaining, self.used, self.limit, guidance
        )
    }
    pub fn ensure_available(&self) -> anyhow::Result<()> {
        if self.available() {
            return Ok(());
        }
        Err(anyhow::Error::new(BlockedReason::BudgetExhausted).context(self.message()))
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AttemptPolicy {
    pub max_repair_rounds: usize,
    pub max_no_progress_rounds: usize,
    pub max_retries: usize,
    pub task_timeout_seconds: u64,
    pub session_timeout_seconds: u64,
    pub command_timeout_seconds: u64,
}
impl AttemptPolicy {
    pub fn from_config(c: &Config) -> Self {
        Self {
            max_repair_rounds: c.max_repair_rounds,
            max_no_progress_rounds: c.max_no_progress_rounds,
            max_retries: c.max_retries,
            task_timeout_seconds: c.task_timeout_seconds,
            session_timeout_seconds: c.session_timeout_seconds,
            command_timeout_seconds: c.command_timeout_seconds,
        }
    }
    pub fn apply(&self, c: &mut Config) {
        c.max_repair_rounds = self.max_repair_rounds;
        c.max_no_progress_rounds = self.max_no_progress_rounds;
        c.max_retries = self.max_retries;
        c.task_timeout_seconds = self.task_timeout_seconds;
        c.session_timeout_seconds = self.session_timeout_seconds;
        c.command_timeout_seconds = self.command_timeout_seconds;
    }
}

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct WorkspaceLifecycle {
    pub archived_at: Option<String>,
    pub discarded_at: Option<String>,
}
impl WorkspaceLifecycle {
    pub fn is_empty(&self) -> bool {
        self.archived_at.is_none() && self.discarded_at.is_none()
    }
}
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Finding {
    pub title: String,
    pub file: String,
    pub detail: String,
    pub priority: String,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Review {
    pub completed: bool,
    pub summary: String,
    pub findings: Vec<Finding>,
}
impl Review {
    pub fn clean(&self) -> bool {
        self.completed && !self.summary.trim().is_empty() && self.findings.is_empty()
    }
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReviewRound {
    pub session_id: String,
    pub revision: String,
    pub comparison_base: String,
    pub result: Review,
    pub created_at: String,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Verification {
    pub command: String,
    pub success: bool,
    pub output: String,
    pub revision: String,
    pub created_at: String,
}
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum BaselineStatus {
    Running,
    Passed,
    Failed,
    Cancelled,
    TimedOut,
    Interrupted,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BaselineCommand {
    pub command: String,
    pub success: bool,
    pub output: String,
    pub output_truncated: bool,
    pub created_at: String,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BaselineCheck {
    pub id: String,
    pub status: BaselineStatus,
    pub config: Config,
    pub config_fingerprint: String,
    pub revision: Option<String>,
    pub started_at: String,
    pub completed_at: Option<String>,
    pub commands: Vec<BaselineCommand>,
    pub error: Option<String>,
    pub workspace_removed: bool,
    pub cleanup_error: Option<String>,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DefaultBranchObservation {
    pub repository: String,
    pub default_branch: String,
    pub revision: String,
    pub observed_at: String,
}
/// Persisted `Session.status` values. These strings are the durable record's
/// vocabulary; saved sessions stay readable, so they never change.
pub mod session_status {
    pub const RUNNING: &str = "running";
    pub const COMPLETED: &str = "completed";
    pub const FAILED: &str = "failed";
    pub const INTERRUPTED: &str = "interrupted";
}
/// Persisted `Cycle.status` values, likewise part of the durable vocabulary.
pub mod cycle_status {
    pub const RUNNING: &str = "running";
    pub const COMPLETED: &str = "completed";
    pub const IDLE: &str = "idle";
    pub const FAILED: &str = "failed";
    pub const INTERRUPTED: &str = "interrupted";
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Session {
    pub id: String,
    pub role: String,
    pub route: Route,
    pub status: String,
    pub started_at: String,
    pub summary: String,
}
impl Session {
    /// A freshly started session record: running, started now, no summary yet.
    pub fn new(id: String, role: &str, route: Route) -> Self {
        Self {
            id,
            role: role.into(),
            route,
            status: session_status::RUNNING.into(),
            started_at: now(),
            summary: String::new(),
        }
    }
    pub fn mark_completed(&mut self, summary: String) {
        self.status = session_status::COMPLETED.into();
        self.summary = summary;
    }
    pub fn mark_failed(&mut self, summary: String) {
        self.status = session_status::FAILED.into();
        self.summary = summary;
    }
    pub fn mark_interrupted(&mut self) {
        self.status = session_status::INTERRUPTED.into();
    }
}
/// Marks every still-running session interrupted. Sessions that already reached a
/// terminal state keep their recorded outcome.
pub fn interrupt_running(sessions: &mut [Session]) {
    for session in sessions {
        if session.status == session_status::RUNNING {
            session.mark_interrupted();
        }
    }
}
/// Marks every still-running session failed, backfilling an empty summary with
/// the task's terminal error so the session record explains its ending.
pub fn fail_running(sessions: &mut [Session], summary: &str) {
    for session in sessions {
        if session.status != session_status::RUNNING {
            continue;
        }
        session.status = session_status::FAILED.into();
        if session.summary.is_empty() {
            session.summary = summary.to_owned();
        }
    }
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Task {
    pub id: String,
    pub cycle_id: String,
    pub proposal: Proposal,
    pub status: Status,
    pub route: Route,
    pub config: Config,
    pub source_revision: String,
    pub comparison_base: String,
    pub default_revision: String,
    pub branch: String,
    pub workspace: String,
    pub execution_session: Option<String>,
    pub repair_session: Option<String>,
    pub sessions: Vec<Session>,
    pub reviews: Vec<ReviewRound>,
    pub verification: Vec<Verification>,
    pub output_commit: Option<String>,
    pub pr_number: Option<u64>,
    pub pr_url: Option<String>,
    pub attempts: usize,
    pub error: Option<String>,
    pub created_at: String,
    pub updated_at: String,
    #[serde(default)]
    pub attempt_policy: Option<AttemptPolicy>,
    /// Reviews recorded before the current attempt began; repair rounds count from here.
    #[serde(default)]
    pub review_baseline: usize,
    #[serde(default)]
    pub blocked_reason: Option<BlockedReason>,
    #[serde(default)]
    pub run_id: Option<String>,
    #[serde(default)]
    pub superseded_by: Vec<String>,
    #[serde(default)]
    pub supersedes: Vec<String>,
    #[serde(default)]
    pub rediscovery_requested: bool,
    #[serde(default)]
    pub rediscovery_result: Option<String>,
    #[serde(default)]
    pub lifecycle: WorkspaceLifecycle,
}
impl Task {
    /// Review rounds recorded during the current attempt.
    pub fn attempt_reviews(&self) -> usize {
        self.reviews.len().saturating_sub(self.review_baseline)
    }
    pub fn execution_config(&self) -> Config {
        let mut c = self.config.clone();
        if let Some(policy) = &self.attempt_policy {
            policy.apply(&mut c);
        }
        c
    }
    pub fn allowed_actions(&self) -> Vec<&'static str> {
        if self.lifecycle.archived_at.is_some() {
            return if self.lifecycle.discarded_at.is_some() {
                vec![]
            } else {
                vec!["discard"]
            };
        }
        if self.status == Status::Published {
            return vec!["archive"];
        }
        if self.status == Status::Cancelled {
            let mut actions = vec!["archive"];
            if self.lifecycle.discarded_at.is_none()
                && self.output_commit.is_none()
                && !self.rediscovery_requested
                && self.superseded_by.is_empty()
            {
                actions.push("supersede");
            }
            return actions;
        }
        if self.lifecycle.discarded_at.is_some() {
            return vec![];
        }
        if self.status == Status::Publishing {
            return vec![];
        }
        if !self.status.retryable() {
            return if self.output_commit.is_none() {
                vec!["cancel"]
            } else {
                vec![]
            };
        }
        let mut actions = vec![];
        if self.output_commit.is_none() {
            actions.push("cancel");
        }
        actions.push("archive");
        match self.blocked_reason.unwrap_or_default() {
            BlockedReason::StaleBase
            | BlockedReason::InvalidPlan
            | BlockedReason::WorkspaceInvalid => actions.push("supersede"),
            BlockedReason::RemoteConflict | BlockedReason::PublicationUncertain => {
                actions.push("reconcile")
            }
            BlockedReason::DependencyBlocked | BlockedReason::RunnerUnavailable => {
                actions.push("retry");
                actions.push("supersede");
            }
            _ => actions.push("retry"),
        }
        actions
    }
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PullRequest {
    pub number: u64,
    pub title: String,
    pub branch: String,
    pub head: String,
    pub base: String,
    pub url: String,
    pub body: String,
    pub state: String,
    pub changed_lines: u64,
    pub created_at: String,
    pub owned: bool,
    #[serde(default)]
    pub head_repository: String,
    #[serde(default)]
    pub base_repository: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PrObservation {
    pub repository: String,
    pub pr: PullRequest,
    pub observed_at: String,
    pub delivered_head: Option<String>,
    pub external_head_movement: bool,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Grounding {
    pub revision: String,
    pub prs: Vec<PullRequest>,
    #[serde(default)]
    pub external_prs: Vec<ExternalPrContext>,
    #[serde(default)]
    pub pr_coverage: PrCoverage,
    pub history: Value,
    pub maintenance_due: bool,
    pub maintenance_targets: Vec<String>,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExternalPrContext {
    pub number: u64,
    pub url: String,
    pub title: String,
    pub body: String,
    pub branch: String,
    pub head: String,
    pub base: String,
    pub head_repository: String,
    pub base_repository: String,
    pub title_truncated: bool,
    pub body_truncated: bool,
}
#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct PrCoverage {
    pub observed_at: Option<String>,
    pub complete: bool,
    pub total_open: usize,
    pub total_external: usize,
    pub included_external: usize,
    pub omitted_external: usize,
    pub max_external: usize,
    pub max_title_chars: usize,
    pub max_body_chars: usize,
    pub max_context_bytes: usize,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OpenPrInventory {
    pub repository: String,
    pub observed_at: String,
    pub prs: Vec<PullRequest>,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PrCapacity {
    pub limit: usize,
    pub owned_open: Option<usize>,
    pub reserved: usize,
    pub remaining: Option<usize>,
    pub observed_at: Option<String>,
    pub status: String,
    pub reason: Option<String>,
}
#[derive(Debug, Default, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum CycleMode {
    #[default]
    Execution,
    Audit,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Cycle {
    #[serde(default)]
    pub mode: CycleMode,
    pub id: String,
    pub number: u64,
    pub status: String,
    pub started_at: String,
    pub completed_at: Option<String>,
    pub grounding: Option<Grounding>,
    pub proposals: Vec<Proposal>,
    pub assessments: Vec<Value>,
    pub sessions: Vec<Session>,
    pub error: Option<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub repository: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub decision_memory: Vec<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub run_id: Option<String>,
    #[serde(default, skip_serializing_if = "WorkspaceLifecycle::is_empty")]
    pub lifecycle: WorkspaceLifecycle,
}

#[derive(Debug, Default, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum OperatingMode {
    #[default]
    Paused,
    RunOnce,
    Continuous,
}
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum BatchPhase {
    Draining,
    Planning,
    Executing,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RunBatch {
    pub id: String,
    pub phase: BatchPhase,
    pub cycle_id: Option<String>,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(from = "SavedControl")]
pub struct Control {
    /// Serialized derivative of `mode` for the dashboard contract; kept in sync by
    /// `set_mode` and the `SavedControl` migration. Never set it directly.
    pub paused: bool,
    pub cycle_number: u64,
    pub next_cycle_at: i64,
    pub error: Option<String>,
    pub mode: OperatingMode,
    pub batch: Option<RunBatch>,
    pub idle_streak: u32,
    pub context_fingerprint: String,
}
#[derive(Deserialize)]
#[serde(default)]
struct SavedControl {
    paused: bool,
    mode: Option<OperatingMode>,
    cycle_number: u64,
    next_cycle_at: i64,
    error: Option<String>,
    batch: Option<RunBatch>,
    idle_streak: u32,
    context_fingerprint: String,
}
impl Default for SavedControl {
    fn default() -> Self {
        Self {
            paused: true,
            mode: None,
            cycle_number: 0,
            next_cycle_at: 0,
            error: None,
            batch: None,
            idle_streak: 0,
            context_fingerprint: String::new(),
        }
    }
}
impl From<SavedControl> for Control {
    fn from(c: SavedControl) -> Self {
        let mode = c.mode.unwrap_or(if c.paused {
            OperatingMode::Paused
        } else {
            OperatingMode::Continuous
        });
        Self {
            paused: mode == OperatingMode::Paused,
            mode,
            cycle_number: c.cycle_number,
            next_cycle_at: c.next_cycle_at,
            error: c.error,
            batch: c.batch,
            idle_streak: c.idle_streak,
            context_fingerprint: c.context_fingerprint,
        }
    }
}
impl Control {
    pub fn set_mode(&mut self, mode: OperatingMode) {
        self.mode = mode;
        self.paused = mode == OperatingMode::Paused;
        if mode != OperatingMode::RunOnce {
            self.batch = None;
        }
    }
}
impl Default for Control {
    fn default() -> Self {
        Self {
            paused: true,
            cycle_number: 0,
            next_cycle_at: 0,
            error: None,
            mode: OperatingMode::Paused,
            batch: None,
            idle_streak: 0,
            context_fingerprint: String::new(),
        }
    }
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Event {
    pub id: i64,
    pub at: String,
    pub entity_id: String,
    pub kind: String,
    pub message: String,
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn empty_or_incomplete_review_never_clean() {
        assert!(
            !Review {
                completed: true,
                summary: "".into(),
                findings: vec![]
            }
            .clean()
        );
        assert!(serde_json::from_str::<Review>("{}").is_err());
        assert!(
            !Review {
                completed: false,
                summary: "interrupted".into(),
                findings: vec![]
            }
            .clean()
        );
    }
}
