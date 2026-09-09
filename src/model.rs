use crate::config::{Config, Route};
use serde::{Deserialize, Serialize};
use serde_json::Value;

pub fn now() -> String {
    chrono::Utc::now().to_rfc3339()
}
pub fn id() -> String {
    uuid::Uuid::new_v4().to_string()
}
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
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Session {
    pub id: String,
    pub role: String,
    pub route: Route,
    pub status: String,
    pub started_at: String,
    pub summary: String,
}
/// A recorded response to default-branch movement. Stages: `initialization` adopted the
/// new revision before any work, `pre_review` and `pre_publication` rebased unpublished
/// work (the latter forcing an extra fresh review), `default_refresh` only updated the
/// recorded default revision for existing-PR work.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Reconciliation {
    pub stage: String,
    pub from: String,
    pub to: String,
    pub at: String,
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
    pub reconciliations: Vec<Reconciliation>,
}
impl Task {
    /// Review rounds that count against the repair limit. A rebase after a clean review
    /// forces one extra fresh review that is not a repair round.
    pub fn review_rounds(&self) -> usize {
        self.reviews.len().saturating_sub(
            self.reconciliations
                .iter()
                .filter(|r| r.stage == "pre_publication")
                .count(),
        )
    }
    /// Rebases performed so far, bounded by the configured reconciliation limit.
    pub fn rebases(&self) -> usize {
        self.reconciliations
            .iter()
            .filter(|r| r.stage == "pre_review" || r.stage == "pre_publication")
            .count()
    }
}
/// A review or issue comment recorded on an owned PR: evidence for planning, never instruction.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct PrComment {
    pub author: String,
    pub at: String,
    pub path: Option<String>,
    pub body: String,
}
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
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
    /// approved | changes_requested | commented | none (latest state per reviewer).
    #[serde(default)]
    pub review_decision: String,
    /// success | failure | pending | none, aggregated from check runs on the head.
    #[serde(default)]
    pub ci: String,
    #[serde(default)]
    pub failing_checks: Vec<String>,
    /// clean | conflicts | unknown, from GitHub's mergeability computation.
    #[serde(default)]
    pub mergeable: String,
    #[serde(default)]
    pub comments: Vec<PrComment>,
}
impl PullRequest {
    /// Owned PRs whose recorded feedback needs work: requested changes, red checks or conflicts.
    pub fn needs_feedback_work(&self) -> bool {
        self.owned
            && (self.review_decision == "changes_requested"
                || self.ci == "failure"
                || self.mergeable == "conflicts")
    }
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Grounding {
    pub revision: String,
    pub prs: Vec<PullRequest>,
    pub history: Value,
    pub maintenance_due: bool,
    pub maintenance_targets: Vec<String>,
    #[serde(default)]
    pub feedback_targets: Vec<String>,
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
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Control {
    pub paused: bool,
    pub cycle_number: u64,
    pub next_cycle_at: i64,
    pub error: Option<String>,
}
impl Default for Control {
    fn default() -> Self {
        Self {
            paused: true,
            cycle_number: 0,
            next_cycle_at: 0,
            error: None,
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
