use super::*;
use anyhow::Context;
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DecisionRecord {
    pub id: String,
    pub mode: CycleMode,
    pub repository: String,
    pub target: String,
    pub problem_key: String,
    pub relevant_paths: Vec<String>,
    pub decision: String,
    pub reason: String,
    pub source_revision: String,
    pub context_fingerprint: String,
    pub reconsider_after: String,
    pub cycle_id: String,
}

fn consolidated_decisions(records: &[DecisionRecord]) -> impl Iterator<Item = &DecisionRecord> {
    // Rejected alternatives to accepted work in the same consolidation are absorbed
    // candidates, not rejections of the problem. Keep other cycles independent.
    records.iter().filter(|decision| {
        decision.decision == "accepted"
            || !records.iter().any(|accepted| {
                accepted.decision == "accepted"
                    && accepted.cycle_id == decision.cycle_id
                    && accepted
                        .repository
                        .eq_ignore_ascii_case(&decision.repository)
                    && accepted.target == decision.target
                    && accepted.problem_key == decision.problem_key
            })
    })
}

impl App {
    pub(super) async fn planning_memory(
        &self,
        c: &Config,
        g: &Grounding,
        cancel: &CancellationToken,
    ) -> Result<Value> {
        let records: Vec<DecisionRecord> = self
            .store
            .decision_memory(&c.github_repo)?
            .into_iter()
            .map(serde_json::from_value)
            .collect::<serde_json::Result<_>>()?;
        let mut memory = vec![];
        // Normalize older candidate-level records as well as newly saved memory.
        for decision in consolidated_decisions(&records) {
            let mut value = serde_json::to_value(decision)?;
            // Skip decisions whose PR target is no longer an owned open PR.
            let revision = match super::planning::resolve_target(c, &g.prs, &decision.target) {
                Ok(pr) => pr.map_or(g.revision.as_str(), |pr| pr.head.as_str()),
                Err(_) => continue,
            };
            let unchanged = path_fingerprint(c, revision, &decision.relevant_paths, cancel).await?
                == decision.context_fingerprint;
            let expired = chrono::DateTime::parse_from_rfc3339(&decision.reconsider_after)
                .is_ok_and(|d| d <= chrono::Utc::now());
            value["reconsideration_due"] = json!(!unchanged || expired);
            memory.push(value);
        }
        Ok(
            json!({"decisions":memory,"rediscovery_requests":self.store.rediscovery_requests(&c.github_repo)?}),
        )
    }
    pub(super) async fn record_decisions(
        &self,
        c: &Config,
        cycle: &Cycle,
        cancel: &CancellationToken,
    ) -> Result<Vec<Value>> {
        let mut records = vec![];
        let g = cycle
            .grounding
            .as_ref()
            .context("Missing decision context")?;
        for p in &cycle.proposals {
            // Rejected or deferred proposals may name targets that no longer resolve;
            // they deliberately bind to the grounding revision, matching
            // planning_memory's skip of unresolvable targets.
            let revision = super::planning::resolve_target(c, &g.prs, &p.target)
                .ok()
                .flatten()
                .map(|pr| pr.head.as_str())
                .unwrap_or(&g.revision);
            let d = DecisionRecord {
                id: format!("{}:{}", cycle.id, p.id),
                mode: cycle.mode,
                repository: c.github_repo.clone(),
                target: p.target.clone(),
                problem_key: p.problem_identity(),
                relevant_paths: p.relevant_paths.clone(),
                decision: p.decision.clone(),
                reason: crate::store::redact(&p.reason),
                source_revision: revision.into(),
                context_fingerprint: path_fingerprint(c, revision, &p.relevant_paths, cancel)
                    .await?,
                reconsider_after: (chrono::Utc::now() + chrono::Duration::days(30)).to_rfc3339(),
                cycle_id: cycle.id.clone(),
            };
            records.push(d);
        }
        Ok(consolidated_decisions(&records)
            .map(serde_json::to_value)
            .collect::<serde_json::Result<_>>()?)
    }
    pub(super) fn validate_memory(proposals: &[Proposal], memory: &Value) -> Result<()> {
        for p in proposals {
            ensure!(
                p.problem_key.len() <= 200
                    && p.reconsiders.len() <= 100
                    && p.relevant_paths.len() <= 40,
                "Proposal decision metadata exceeds bounds"
            );
            if p.decision == "accepted"
                && p.reconsiders.is_empty()
                && let Some(decisions) = memory["decisions"].as_array()
            {
                ensure!(
                    !decisions.iter().any(|d| d["target"] == p.target
                        && d["problem_key"] == p.problem_identity()
                        && d["reconsideration_due"] == false
                        && !(d["mode"] == "audit" && d["decision"] == "accepted")
                        && ["rejected", "deferred", "accepted"]
                            .iter()
                            .any(|s| d["decision"] == *s)),
                    "Proposal repeats a recorded decision without changed context or elapsed reconsideration period"
                );
            }
            for id in &p.reconsiders {
                ensure!(
                    memory["rediscovery_requests"]
                        .as_array()
                        .is_some_and(|requests| requests
                            .iter()
                            .any(|r| r["id"] == *id && r["target"] == p.target)),
                    "Unknown or mismatched rediscovery request"
                );
            }
        }
        Ok(())
    }
}
async fn path_fingerprint(
    c: &Config,
    revision: &str,
    paths: &[String],
    cancel: &CancellationToken,
) -> Result<String> {
    use sha2::{Digest, Sha256};
    ensure!(paths.len() <= 40, "Decision has too many relevant paths");
    for path in paths {
        ensure!(
            !path.is_empty()
                && Path::new(path)
                    .components()
                    .all(|p| matches!(p, std::path::Component::Normal(_))),
            "Decision paths must be repository-relative files"
        );
    }
    if paths.is_empty() {
        return Ok(revision.into());
    }
    let mut args = vec!["ls-tree", "-r", revision, "--"];
    args.extend(paths.iter().map(String::as_str));
    let output = git::git(c, &c.repository, &args, cancel).await?;
    Ok(format!("{:x}", Sha256::digest(output.as_bytes())))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn decision_memory_reconsiders_context_and_does_not_execute_audit_acceptance() {
        let tmp = tempfile::tempdir().unwrap();
        let store = Store::open(&tmp.path().join("state.db")).unwrap();
        let app = App::new(store.clone(), tmp.path().into());
        let mut c = Config {
            github_repo: "fixture/project".into(),
            ..Default::default()
        };
        let mut decision = DecisionRecord {
            id: "decision".into(),
            mode: CycleMode::Execution,
            repository: c.github_repo.clone(),
            target: "main".into(),
            problem_key: "same-problem".into(),
            relevant_paths: vec![],
            decision: "rejected".into(),
            reason: "No current benefit".into(),
            source_revision: "old".into(),
            context_fingerprint: "old".into(),
            reconsider_after: (chrono::Utc::now() + chrono::Duration::days(30)).to_rfc3339(),
            cycle_id: "cycle".into(),
        };
        store.put("decision", &decision.id, &decision).unwrap();
        c.github_repo = "Fixture/Project".into();
        let mut g = Grounding {
            revision: "old".into(),
            prs: vec![],
            external_prs: vec![],
            pr_coverage: PrCoverage::default(),
            history: json!([]),
            maintenance_due: false,
            maintenance_targets: vec![],
        };
        let p: Proposal = serde_json::from_value(json!({
            "id": "p",
            "title": "New wording",
            "problem": "Same underlying problem",
            "evidence": [],
            "benefit": "x",
            "category": "features",
            "target": "main",
            "tier": "M",
            "scope": "x",
            "dependencies": [],
            "prompt": "x",
            "decision": "accepted",
            "reason": "x",
            "problem_key": "same-problem",
        }))
        .unwrap();
        let memory = app
            .planning_memory(&c, &g, &CancellationToken::new())
            .await
            .unwrap();
        assert!(App::validate_memory(std::slice::from_ref(&p), &memory).is_err());
        g.revision = "new".into();
        let memory = app
            .planning_memory(&c, &g, &CancellationToken::new())
            .await
            .unwrap();
        App::validate_memory(std::slice::from_ref(&p), &memory).unwrap();
        g.revision = "old".into();
        decision.mode = CycleMode::Audit;
        decision.decision = "accepted".into();
        store.put("decision", &decision.id, &decision).unwrap();
        let memory = app
            .planning_memory(&c, &g, &CancellationToken::new())
            .await
            .unwrap();
        App::validate_memory(&[p], &memory).unwrap();
    }

    #[tokio::test]
    async fn audit_absorption_does_not_veto_execution_or_erase_independent_rejections() {
        let tmp = tempfile::tempdir().unwrap();
        let store = Store::open(&tmp.path().join("state.db")).unwrap();
        let app = App::new(store.clone(), tmp.path().into());
        let c = Config {
            github_repo: "fixture/project".into(),
            ..Default::default()
        };
        let p: Proposal = serde_json::from_value(json!({"id":"accepted","title":"Consolidated work","problem":"One problem","evidence":[],"benefit":"x","category":"features","target":"main","tier":"M","scope":"x","dependencies":[],"prompt":"x","decision":"accepted","reason":"x","problem_key":"same-problem"})).unwrap();
        let g = Grounding {
            revision: "source".into(),
            prs: vec![],
            external_prs: vec![],
            pr_coverage: PrCoverage::default(),
            history: json!([]),
            maintenance_due: false,
            maintenance_targets: vec![],
        };
        let accepted = DecisionRecord {
            id: "accepted".into(),
            mode: CycleMode::Audit,
            repository: c.github_repo.clone(),
            target: p.target.clone(),
            problem_key: p.problem_identity(),
            relevant_paths: vec![],
            decision: "accepted".into(),
            reason: "Consolidated scope".into(),
            source_revision: g.revision.clone(),
            context_fingerprint: g.revision.clone(),
            reconsider_after: (chrono::Utc::now() + chrono::Duration::days(30)).to_rfc3339(),
            cycle_id: "audit-cycle".into(),
        };
        // Saved candidate-level memory must also work after an upgrade.
        store.put("decision", &accepted.id, &accepted).unwrap();
        let mut rejected = DecisionRecord {
            id: "absorbed".into(),
            repository: "Fixture/Project".into(),
            decision: "rejected".into(),
            reason: "Absorbed into accepted".into(),
            ..accepted.clone()
        };
        store.put("decision", &rejected.id, &rejected).unwrap();
        let memory = app
            .planning_memory(&c, &g, &CancellationToken::new())
            .await
            .unwrap();
        App::validate_memory(std::slice::from_ref(&p), &memory).unwrap();

        // Acceptance in another cycle must not exempt a real rejection.
        rejected.cycle_id = "independent-cycle".into();
        rejected.reason = "No current benefit".into();
        store.put("decision", &rejected.id, &rejected).unwrap();
        let memory = app
            .planning_memory(&c, &g, &CancellationToken::new())
            .await
            .unwrap();
        assert!(App::validate_memory(std::slice::from_ref(&p), &memory).is_err());

        // Execution acceptance still suppresses repeated work after absorption.
        rejected.cycle_id = accepted.cycle_id.clone();
        store.put("decision", &rejected.id, &rejected).unwrap();
        let executed = DecisionRecord {
            mode: CycleMode::Execution,
            ..accepted
        };
        store.put("decision", &executed.id, &executed).unwrap();
        let memory = app
            .planning_memory(&c, &g, &CancellationToken::new())
            .await
            .unwrap();
        assert!(App::validate_memory(&[p], &memory).is_err());
    }
}
