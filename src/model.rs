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
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Grounding {
    pub revision: String,
    pub prs: Vec<PullRequest>,
    pub history: Value,
    pub maintenance_due: bool,
    pub maintenance_targets: Vec<String>,
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
