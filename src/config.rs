use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::{collections::BTreeMap, path::PathBuf};

pub const CATEGORIES: [&str; 9] = [
    "features",
    "correctness",
    "performance",
    "ux-dx",
    "refactoring",
    "simplification",
    "tests",
    "dependencies",
    "documentation",
];
pub const ROLES: [&str; 4] = [
    "orchestrator",
    "discovery",
    "proposal_reviewer",
    "code_reviewer",
];
#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, PartialEq, Eq, PartialOrd, Ord)]
#[serde(rename_all = "snake_case")]
pub enum Backend {
    #[default]
    Codex,
    Opencode,
}
impl std::fmt::Display for Backend {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Codex => "Codex",
            Self::Opencode => "OpenCode",
        })
    }
}
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Route {
    #[serde(default)]
    pub backend: Backend,
    pub model: String,
    #[serde(default)]
    pub effort: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provider: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub variant: Option<String>,
}
impl Route {
    pub fn new(model: &str, effort: &str) -> Self {
        Self {
            backend: Backend::Codex,
            model: model.into(),
            effort: effort.into(),
            provider: None,
            variant: None,
        }
    }
    pub fn validate(&self, ready: bool) -> Result<()> {
        let valid =
            |s: &str, max| s.len() <= max && s.trim() == s && !s.chars().any(char::is_control);
        ensure!(
            valid(
                &self.model,
                if self.backend == Backend::Codex {
                    100
                } else {
                    512
                }
            ) && valid(&self.effort, 20)
                && self.provider.as_deref().is_none_or(|s| valid(s, 100))
                && self
                    .variant
                    .as_deref()
                    .is_none_or(|s| !s.is_empty() && valid(s, 100)),
            "Invalid model route"
        );
        match self.backend {
            Backend::Codex => {
                ensure!(
                    self.provider.is_none() && self.variant.is_none(),
                    "Codex routes use reasoning effort, not an OpenCode provider or variant"
                );
                ensure!(
                    !ready || (!self.model.is_empty() && !self.effort.is_empty()),
                    "Set the Codex model and effort for every required route"
                );
            }
            Backend::Opencode => {
                ensure!(
                    self.effort.is_empty(),
                    "OpenCode routes use a variant, not Codex reasoning effort"
                );
                ensure!(
                    !ready
                        || (!self.model.is_empty()
                            && self.provider.as_deref().is_some_and(|s| !s.is_empty())),
                    "Set the OpenCode provider and model for every required route"
                );
            }
        }
        Ok(())
    }
}
impl std::fmt::Display for Route {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self.backend {
            Backend::Codex => write!(f, "Codex · {} / {}", self.model, self.effort),
            Backend::Opencode => write!(
                f,
                "OpenCode · {}/{} / {}",
                self.provider.as_deref().unwrap_or(""),
                self.model,
                self.variant.as_deref().unwrap_or("provider default")
            ),
        }
    }
}
pub fn default_repair_route() -> Route {
    Route::new("gpt-6-astra", "medium")
}
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Config {
    pub repository: PathBuf,
    pub github_repo: String,
    pub default_branch: String,
    pub branch_prefix: String,
    pub codex_binary: String,
    pub opencode_binary: String,
    pub roles: BTreeMap<String, Route>,
    pub tiers: BTreeMap<String, Route>,
    pub repair_route: Route,
    pub categories: Vec<String>,
    pub verification_commands: Vec<String>,
    pub discovery_agents: usize,
    pub execution_concurrency: usize,
    pub cycle_interval_seconds: u64,
    pub maintenance_every_cycles: u64,
    pub large_pr_lines: u64,
    pub long_lived_pr_days: u64,
    pub max_tasks_per_cycle: usize,
    pub max_repair_rounds: usize,
    pub max_no_progress_rounds: usize,
    pub max_retries: usize,
    pub session_timeout_seconds: u64,
    pub task_timeout_seconds: u64,
    pub command_timeout_seconds: u64,
    pub max_sessions_per_day: u64,
    pub max_workspace_bytes: u64,
    pub runner_storage_paths: BTreeMap<String, PathBuf>,
    pub retain_completed_days: u64,
    pub retain_events: usize,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            repository: PathBuf::new(),
            github_repo: String::new(),
            default_branch: "main".into(),
            branch_prefix: "octomus/".into(),
            codex_binary: "codex".into(),
            opencode_binary: "opencode".into(),
            roles: ROLES
                .into_iter()
                .map(|r| (r.into(), Route::new("", "")))
                .collect(),
            tiers: [
                ("XS", "gpt-5.6-luna", "xhigh"),
                ("S", "gpt-5.6-luna", "max"),
                ("M", "gpt-6-astra", "low"),
                ("L", "gpt-6-astra", "medium"),
                ("XL", "gpt-6-astra", "high"),
            ]
            .into_iter()
            .map(|(t, m, e)| (t.into(), Route::new(m, e)))
            .collect(),
            repair_route: default_repair_route(),
            categories: CATEGORIES.map(String::from).to_vec(),
            verification_commands: vec![],
            discovery_agents: 9,
            execution_concurrency: 2,
            cycle_interval_seconds: 1800,
            maintenance_every_cycles: 3,
            large_pr_lines: 1000,
            long_lived_pr_days: 7,
            max_tasks_per_cycle: 5,
            max_repair_rounds: 4,
            max_no_progress_rounds: 2,
            max_retries: 2,
            session_timeout_seconds: 1800,
            task_timeout_seconds: 14400,
            command_timeout_seconds: 600,
            max_sessions_per_day: 150,
            max_workspace_bytes: 20_000_000_000,
            runner_storage_paths: BTreeMap::new(),
            retain_completed_days: 14,
            retain_events: 10000,
        }
    }
}
impl Config {
    pub fn binary(&self, backend: Backend) -> &str {
        match backend {
            Backend::Codex => &self.codex_binary,
            Backend::Opencode => &self.opencode_binary,
        }
    }
    pub fn routes_for(&self, audit: bool) -> Vec<(&str, &Route)> {
        self.roles
            .iter()
            .filter(|(role, _)| !audit || role.as_str() != "code_reviewer")
            .chain(self.tiers.iter().filter(|_| !audit))
            .map(|(name, route)| (name.as_str(), route))
            .chain((!audit).then_some(("repair", &self.repair_route)))
            .collect()
    }
    pub fn validate(&self, ready: bool) -> Result<()> {
        self.validate_mode(ready, false)
    }
    pub fn validate_audit(&self) -> Result<()> {
        self.validate_mode(true, true)
    }
    fn validate_mode(&self, ready: bool, audit: bool) -> Result<()> {
        ensure!(
            (8..=10).contains(&self.discovery_agents),
            "Discovery requires 8–10 agents"
        );
        ensure!(
            (1..=8).contains(&self.execution_concurrency),
            "Execution concurrency must be 1–8"
        );
        ensure!(
            (1..=20).contains(&self.max_tasks_per_cycle),
            "Tasks per cycle must be 1–20"
        );
        ensure!(
            (1..=20).contains(&self.max_repair_rounds) && self.max_no_progress_rounds > 0,
            "Repair limits must be positive (at most 20 rounds)"
        );
        ensure!(self.max_retries <= 10, "Retry limit must be at most 10");
        ensure!(
            (30..=604800).contains(&self.cycle_interval_seconds)
                && (1..=10000).contains(&self.maintenance_every_cycles),
            "Cycle interval must be at least 30 seconds; maintenance cadence must be positive"
        );
        ensure!(
            (10..=604800).contains(&self.session_timeout_seconds)
                && (1..=604800).contains(&self.command_timeout_seconds)
                && self.task_timeout_seconds <= 604800
                && self.task_timeout_seconds >= self.session_timeout_seconds,
            "Invalid time limits"
        );
        ensure!(
            (1..=1000000).contains(&self.max_sessions_per_day)
                && (1_000_000..=1_000_000_000_000_000).contains(&self.max_workspace_bytes)
                && (1..=36500).contains(&self.retain_completed_days)
                && (100..=100000).contains(&self.retain_events),
            "Invalid resource or retention limits"
        );
        ensure!(
            valid_branch(&self.default_branch)
                && valid_branch(&format!("{}task", self.branch_prefix))
                && self.branch_prefix.ends_with('/'),
            "Invalid default branch or branch prefix"
        );
        ensure!(
            !self.default_branch.starts_with(&self.branch_prefix),
            "Owned branch prefix must exclude the default branch"
        );
        ensure!(
            !self.categories.is_empty()
                && self
                    .categories
                    .iter()
                    .all(|v| CATEGORIES.contains(&v.as_str())),
            "Select valid improvement categories"
        );
        ensure!(
            self.roles.len() == 4 && ROLES.iter().all(|r| self.roles.contains_key(*r)),
            "Configure exactly the four planning and review roles"
        );
        ensure!(
            self.tiers.len() == 5
                && ["XS", "S", "M", "L", "XL"]
                    .iter()
                    .all(|r| self.tiers.contains_key(*r)),
            "Configure all five execution tiers"
        );
        for route in self
            .roles
            .values()
            .chain(self.tiers.values())
            .chain(std::iter::once(&self.repair_route))
        {
            route.validate(false)?;
        }
        for (backend, path) in &self.runner_storage_paths {
            ensure!(
                ["codex", "opencode"].contains(&backend.as_str()) && path.is_absolute(),
                "Runner storage measurement requires an absolute path for Codex or OpenCode"
            );
        }
        for binary in [&self.codex_binary, &self.opencode_binary] {
            ensure!(
                binary.len() <= 4096 && !binary.chars().any(char::is_control),
                "Invalid runner executable path"
            );
        }
        ensure!(
            self.verification_commands
                .iter()
                .all(|c| !c.trim().is_empty() && c.len() <= 4096),
            "Invalid executable or verification commands"
        );
        if ready {
            for (name, route) in self.routes_for(audit) {
                route
                    .validate(true)
                    .map_err(|e| anyhow::anyhow!("{name}: {e}"))?;
                validate_binary(self.binary(route.backend))?;
            }
            ensure!(
                self.repository.is_absolute() && self.repository.join(".git").exists(),
                "Repository must be an absolute path to a Git checkout"
            );
            let parts: Vec<_> = self.github_repo.split('/').collect();
            ensure!(
                parts.len() == 2
                    && parts.iter().all(|s| !s.is_empty()
                        && s.bytes()
                            .all(|c| c.is_ascii_alphanumeric() || b"-_.".contains(&c))),
                "GitHub repository must be owner/name"
            );
            ensure!(
                audit || !self.verification_commands.is_empty(),
                "Set at least one meaningful repository verification command"
            );
        }
        Ok(())
    }
}
pub fn validate_binary(binary: &str) -> Result<()> {
    ensure!(
        !binary.trim().is_empty() && binary.len() <= 4096 && !binary.chars().any(char::is_control),
        "Set a valid runner executable path"
    );
    Ok(())
}
pub fn valid_branch(s: &str) -> bool {
    !s.is_empty()
        && !s.starts_with(['-', '/', '.'])
        && !s.ends_with(['/', '.'])
        && !s.contains("..")
        && !s.contains("@{")
        && !s.contains("//")
        && s.split('/')
            .all(|p| !p.starts_with('.') && !p.ends_with(".lock"))
        && s.bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"-_./".contains(&c))
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn routes_are_exact_and_roles_explicit() {
        let c = Config::default();
        c.validate(false).unwrap();
        assert!(c.validate(true).is_err());
        assert_eq!(c.tiers["XS"], Route::new("gpt-5.6-luna", "xhigh"));
        assert_eq!(c.tiers["XL"], Route::new("gpt-6-astra", "high"));
    }
    #[test]
    fn branch_validation() {
        for s in ["-x", "a..b", "main:evil", "a.lock", "a/../b"] {
            assert!(!valid_branch(s));
        }
        assert!(valid_branch("octomus/task-123"));
    }
}
