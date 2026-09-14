//! Narrow, read-only run-evidence read model.
//!
//! This module reports what was **saved**, never what is currently true. It performs no
//! live HEAD, workspace, remote, authorization or pull-request checks, so the result is
//! recorded review/check evidence and not a publication or safety decision. Facts are
//! computed from the saved records first; display strings are redacted afterwards.
use crate::{
    config::Route,
    model::{Cycle, CycleMode, REVIEWER_SLOTS, Session, Status, Task, now},
    store::{Store, redact_json},
};
use anyhow::{Context, Result};
use rusqlite::{Connection, OptionalExtension};
use serde::Serialize;
use serde_json::Value;
use std::{collections::BTreeMap, path::Path};

pub const SCHEMA_VERSION: u32 = 1;
const DECISIONS: [&str; 3] = ["accepted", "rejected", "deferred"];
const LIMITATIONS: [&str; 9] = [
    "Recorded review and check evidence only. No live HEAD, workspace, remote, authorization or current pull-request checks were performed while producing this export.",
    "Planning completion is not task completion: a completed cycle records decisions, not delivered work.",
    "Deferred is not rejected.",
    "A recorded pull request describes delivery, not merge. Published is not merged.",
    "Audit acceptance is a recommendation. Audit cycles never create an execution queue, so an accepted audit proposal has no linked task by design.",
    "Saved session routes are requested routes. Runtime model identity is not independently reported here.",
    "Costs, delivery time and any replay timeline are not inferred from these records.",
    "Zero or multiple task matches are preserved as recorded. No single task is selected on the caller's behalf.",
    "Free text carried here (proposal problem, benefit, scope and evidence, and code-review findings) is model-authored and still requires manual review before sharing.",
];

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize)]
pub struct RunEvidenceV1 {
    pub schema_version: u32,
    pub generated_at: String,
    pub kind: &'static str,
    pub review_required_before_sharing: bool,
    pub review_requirement: &'static str,
    pub limitations: [&'static str; 9],
    pub cycle: CycleEvidence,
    pub proposals: Vec<ProposalEvidence>,
    /// Explicit missing, ambiguous or stale evidence observed across the whole run.
    pub gaps: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct CycleEvidence {
    pub id: String,
    pub number: u64,
    pub mode: CycleMode,
    pub status: String,
    pub started_at: String,
    pub completed_at: Option<String>,
    pub repository: String,
    pub grounding_revision: Option<String>,
    pub planning: PlanningOutcome,
}

#[derive(Debug, Clone, Serialize)]
pub struct PlanningOutcome {
    /// The saved cycle status verbatim; `completed` describes planning only.
    pub status: String,
    pub planning_finished: bool,
    pub proposal_count: usize,
    pub decisions: BTreeMap<String, usize>,
    pub creates_execution_queue: bool,
    pub error_recorded: bool,
    pub reviewer_batches_saved: usize,
}

#[derive(Debug, Clone, Serialize)]
pub struct ProposalEvidence {
    pub id: String,
    pub title: String,
    pub target: String,
    pub tier: String,
    pub category: String,
    pub problem: String,
    pub benefit: String,
    pub scope: String,
    pub evidence: Vec<String>,
    pub final_decision: String,
    pub final_reason: String,
    pub reviewer_verdicts: Vec<ReviewerVerdict>,
    pub linked_tasks: Vec<TaskEvidence>,
    pub gaps: Vec<String>,
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum VerdictState {
    /// Exactly one identifiable verdict for this proposal in this reviewer's batch.
    Recorded,
    /// No batch for this slot, or the batch records no verdict for this proposal.
    Missing,
    /// More than one verdict for this proposal in the same batch.
    Duplicate,
    /// The batch exists but is not a usable assessment list.
    Malformed,
}

#[derive(Debug, Clone, Serialize)]
pub struct ReviewerVerdict {
    pub reviewer: &'static str,
    pub state: VerdictState,
    pub decision: Option<String>,
    pub reason: Option<String>,
    /// Why the verdict is not a plain `recorded` value, when it is not.
    pub note: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct TaskEvidence {
    pub id: String,
    pub cycle_id: String,
    pub proposal_id: String,
    pub status: Status,
    pub branch: String,
    pub attempts: usize,
    pub blocked_reason: Option<String>,
    pub error_recorded: bool,
    pub created_at: String,
    pub updated_at: String,
    pub revisions: Revisions,
    pub sessions: Vec<SessionRoute>,
    pub latest_review: ReviewEvidence,
    pub required_commands: CommandEvidence,
    pub pull_request: Option<PrReference>,
    pub gaps: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Revisions {
    pub source: String,
    pub comparison_base: Option<String>,
    pub default_branch: String,
    pub output: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct SessionRoute {
    pub id: String,
    pub role: String,
    pub status: String,
    pub requested_route: Route,
    pub started_at: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct ReviewEvidence {
    pub rounds_recorded: usize,
    /// The latest saved review round, never the last convenient passing one.
    pub latest: Option<ReviewRoundEvidence>,
    pub clean: bool,
    pub clean_at_output_revision: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct ReviewRoundEvidence {
    pub session_id: String,
    pub revision: String,
    pub comparison_base: String,
    pub created_at: String,
    pub completed: bool,
    pub summary_present: bool,
    pub matches_output_revision: Option<bool>,
    pub findings: Vec<FindingEvidence>,
}

#[derive(Debug, Clone, Serialize)]
pub struct FindingEvidence {
    pub title: String,
    pub file: String,
    pub priority: String,
    pub detail: String,
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ChecksState {
    /// The task's saved execution configuration requires no verification commands.
    NotConfigured,
    Recorded,
}

#[derive(Debug, Clone, Serialize)]
pub struct CommandEvidence {
    pub state: ChecksState,
    pub commands: Vec<CommandResult>,
    pub all_passed_at_output_revision: bool,
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum CommandState {
    /// Latest recorded result succeeded at the recorded output revision.
    Passed,
    /// Latest recorded result succeeded, but not at a recorded output revision.
    PassedAtOtherRevision,
    Failed,
    /// No result is recorded for this required command. Missing is not passing.
    NoResult,
}

#[derive(Debug, Clone, Serialize)]
pub struct CommandResult {
    pub command: String,
    pub state: CommandState,
    pub results_recorded: usize,
    pub latest_success: Option<bool>,
    pub latest_revision: Option<String>,
    pub latest_created_at: Option<String>,
    pub matches_output_revision: Option<bool>,
}

#[derive(Debug, Clone, Serialize)]
pub struct PrReference {
    pub number: Option<u64>,
    pub url: Option<String>,
    pub source: &'static str,
}

// ---------------------------------------------------------------------------
// Reviewer batch normalization
// ---------------------------------------------------------------------------

struct Entry {
    id: String,
    decision: String,
    reason: String,
}
struct Batch {
    /// Positional reviewer slot, or `None` when there is no slot for this batch.
    slot: Option<usize>,
    index: usize,
    entries: Vec<Entry>,
    malformed_entries: usize,
    /// Set when the batch itself cannot be read as an assessment list.
    unusable: Option<String>,
    /// Set when no completed session confirms this slot's identity.
    unconfirmed: bool,
}

fn slot_sessions<'a>(cycle: &'a Cycle, role: &str) -> Vec<&'a Session> {
    cycle.sessions.iter().filter(|s| s.role == role).collect()
}

/// Reads the saved batches positionally and reports every inconsistency instead of
/// repairing it. Malformed batches keep their slot so reviewer identities cannot shift.
fn normalize_batches(cycle: &Cycle) -> (Vec<Batch>, Vec<String>) {
    let mut gaps = Vec::new();
    let mut batches = Vec::new();
    for (index, raw) in cycle.assessments.iter().enumerate() {
        let mut batch = Batch {
            slot: (index < REVIEWER_SLOTS.len()).then_some(index),
            index,
            entries: Vec::new(),
            malformed_entries: 0,
            unusable: None,
            unconfirmed: false,
        };
        match raw.get("assessments").and_then(Value::as_array) {
            None => {
                batch.unusable =
                    Some("saved batch does not contain a recorded assessment list".into())
            }
            Some(items) => {
                for item in items {
                    let id = item.get("id").and_then(Value::as_str).unwrap_or("").trim();
                    let decision = item.get("decision").and_then(Value::as_str).unwrap_or("");
                    if id.is_empty() || !DECISIONS.contains(&decision) {
                        batch.malformed_entries += 1;
                        continue;
                    }
                    batch.entries.push(Entry {
                        id: id.to_owned(),
                        decision: decision.to_owned(),
                        reason: item
                            .get("reason")
                            .and_then(Value::as_str)
                            .unwrap_or("")
                            .to_owned(),
                    });
                }
            }
        }
        batches.push(batch);
    }
    for (slot, role) in REVIEWER_SLOTS.iter().enumerate() {
        let sessions = slot_sessions(cycle, role);
        let completed = sessions.iter().filter(|s| s.status == "completed").count();
        let saved = batches.iter().any(|b| b.slot == Some(slot));
        if sessions.len() > 1 {
            gaps.push(format!(
                "{} {role} sessions are recorded; batch attribution is positional and cannot be confirmed.",
                sessions.len()
            ));
        }
        if saved && completed == 0 {
            if let Some(batch) = batches.iter_mut().find(|b| b.slot == Some(slot)) {
                batch.unconfirmed = true;
            }
            gaps.push(format!(
                "Saved assessment batch {slot} is attributed to {role} positionally, but no completed {role} session confirms it."
            ));
        }
        if !saved {
            gaps.push(if completed > 0 {
                format!(
                    "Reviewer {role} recorded a completed session but no saved assessment batch; its verdicts are missing."
                )
            } else {
                format!(
                    "Reviewer {role} has neither a completed session nor a saved assessment batch."
                )
            });
        }
    }
    for batch in &batches {
        let owner = batch
            .slot
            .map(|slot| REVIEWER_SLOTS[slot].to_owned())
            .unwrap_or_else(|| "no reviewer slot".to_owned());
        if batch.slot.is_none() {
            gaps.push(format!(
                "Saved assessment batch {} exceeds the two recorded reviewer roles and is unattributable.",
                batch.index
            ));
        }
        if let Some(reason) = &batch.unusable {
            gaps.push(format!(
                "Saved assessment batch {} ({owner}) is malformed: {reason}. It was not reassigned to another reviewer.",
                batch.index
            ));
        }
        if batch.malformed_entries > 0 {
            gaps.push(format!(
                "Saved assessment batch {} ({owner}) contains {} unreadable entries, which are not attributed to any proposal.",
                batch.index, batch.malformed_entries
            ));
        }
    }
    (batches, gaps)
}

fn verdict(batches: &[Batch], slot: usize, proposal_id: &str) -> ReviewerVerdict {
    let reviewer = REVIEWER_SLOTS[slot];
    let missing = |note: String| ReviewerVerdict {
        reviewer,
        state: VerdictState::Missing,
        decision: None,
        reason: None,
        note: Some(note),
    };
    let Some(batch) = batches.iter().find(|b| b.slot == Some(slot)) else {
        return missing(format!(
            "No saved assessment batch is attributed to {reviewer}."
        ));
    };
    if let Some(reason) = &batch.unusable {
        return ReviewerVerdict {
            reviewer,
            state: VerdictState::Malformed,
            decision: None,
            reason: None,
            note: Some(format!("Batch {} is malformed: {reason}.", batch.index)),
        };
    }
    let matches: Vec<&Entry> = batch
        .entries
        .iter()
        .filter(|entry| entry.id == proposal_id)
        .collect();
    let unconfirmed = batch.unconfirmed.then(|| {
        format!("Batch {} is attributed to {reviewer} positionally and is unconfirmed by a completed session.", batch.index)
    });
    match matches.as_slice() {
        [] => missing(format!(
            "Batch {} records no verdict for this proposal.",
            batch.index
        )),
        [entry] => ReviewerVerdict {
            reviewer,
            state: VerdictState::Recorded,
            decision: Some(entry.decision.clone()),
            reason: Some(entry.reason.clone()),
            note: unconfirmed,
        },
        many => {
            let decisions: Vec<&str> = many.iter().map(|entry| entry.decision.as_str()).collect();
            let agreed = decisions.windows(2).all(|pair| pair[0] == pair[1]);
            ReviewerVerdict {
                reviewer,
                state: VerdictState::Duplicate,
                decision: agreed.then(|| many[0].decision.clone()),
                reason: None,
                note: Some(format!(
                    "Batch {} records {} verdicts for this proposal ({}); {}{}",
                    batch.index,
                    many.len(),
                    decisions.join(", "),
                    if agreed {
                        "they agree but remain duplicated"
                    } else {
                        "they are inconsistent and no verdict is selected"
                    },
                    unconfirmed
                        .map(|note| format!(". {note}"))
                        .unwrap_or_else(|| ".".into())
                )),
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Task evidence
// ---------------------------------------------------------------------------

fn review_evidence(task: &Task) -> ReviewEvidence {
    let output = task.output_commit.as_deref();
    let latest = task.reviews.last().map(|round| ReviewRoundEvidence {
        session_id: round.session_id.clone(),
        revision: round.revision.clone(),
        comparison_base: round.comparison_base.clone(),
        created_at: round.created_at.clone(),
        completed: round.result.completed,
        summary_present: !round.result.summary.trim().is_empty(),
        matches_output_revision: output.map(|commit| commit == round.revision),
        findings: round
            .result
            .findings
            .iter()
            .map(|finding| FindingEvidence {
                title: finding.title.clone(),
                file: finding.file.clone(),
                priority: finding.priority.clone(),
                detail: finding.detail.clone(),
            })
            .collect(),
    });
    let clean = task
        .reviews
        .last()
        .is_some_and(|round| round.result.clean());
    ReviewEvidence {
        rounds_recorded: task.reviews.len(),
        clean_at_output_revision: clean
            && latest
                .as_ref()
                .is_some_and(|round| round.matches_output_revision == Some(true)),
        latest,
        clean,
    }
}

fn command_evidence(task: &Task) -> CommandEvidence {
    let required = task.execution_config().verification_commands;
    let output = task.output_commit.as_deref();
    let commands: Vec<CommandResult> = required
        .iter()
        .map(|command| {
            let recorded: Vec<_> = task
                .verification
                .iter()
                .filter(|result| result.command == *command)
                .collect();
            // The latest recorded result decides: a newer failure or a different revision
            // invalidates an older pass.
            let latest = recorded.last();
            let matches = latest.and_then(|result| output.map(|commit| commit == result.revision));
            CommandResult {
                command: command.clone(),
                state: match latest {
                    None => CommandState::NoResult,
                    Some(result) if !result.success => CommandState::Failed,
                    Some(_) if matches == Some(true) => CommandState::Passed,
                    Some(_) => CommandState::PassedAtOtherRevision,
                },
                results_recorded: recorded.len(),
                latest_success: latest.map(|result| result.success),
                latest_revision: latest.map(|result| result.revision.clone()),
                latest_created_at: latest.map(|result| result.created_at.clone()),
                matches_output_revision: matches,
            }
        })
        .collect();
    CommandEvidence {
        state: if required.is_empty() {
            ChecksState::NotConfigured
        } else {
            ChecksState::Recorded
        },
        all_passed_at_output_revision: !commands.is_empty()
            && commands
                .iter()
                .all(|result| result.state == CommandState::Passed),
        commands,
    }
}

fn task_evidence(task: &Task) -> TaskEvidence {
    let latest_review = review_evidence(task);
    let required_commands = command_evidence(task);
    let mut gaps = Vec::new();
    if task.comparison_base.trim().is_empty() {
        gaps.push(
            "No comparison base is persisted, so the recorded review scope cannot be reconstructed."
                .to_owned(),
        );
    }
    if task.output_commit.is_some() {
        if !latest_review.clean_at_output_revision {
            gaps.push(
                "An output revision is recorded without a clean latest review at that revision."
                    .to_owned(),
            );
        }
        if required_commands.state == ChecksState::Recorded
            && !required_commands.all_passed_at_output_revision
        {
            gaps.push(
                "An output revision is recorded without every required command passing at that revision."
                    .to_owned(),
            );
        }
    }
    if required_commands.state == ChecksState::NotConfigured {
        gaps.push(
            "The task's saved execution configuration requires no verification commands, so no check evidence exists."
                .to_owned(),
        );
    }
    if task.status == Status::Published {
        if task.pr_number.is_none() {
            gaps.push(
                "The task is recorded as published without a pull-request reference.".to_owned(),
            );
        }
        if task.output_commit.is_none() {
            gaps.push("The task is recorded as published without an output revision.".to_owned());
        }
    }
    if task.pr_number.is_some() && task.output_commit.is_none() {
        gaps.push(
            "A pull-request reference is recorded without an output revision to compare it against."
                .to_owned(),
        );
    }
    TaskEvidence {
        id: task.id.clone(),
        cycle_id: task.cycle_id.clone(),
        proposal_id: task.proposal.id.clone(),
        status: task.status.clone(),
        branch: task.branch.clone(),
        attempts: task.attempts,
        // The same snake_case token the task API serializes, so one saved reason never
        // has two spellings across the task view, the run evidence and the CLI export.
        blocked_reason: task.blocked_reason.and_then(|reason| {
            serde_json::to_value(reason)
                .ok()?
                .as_str()
                .map(str::to_owned)
        }),
        error_recorded: task.error.is_some(),
        created_at: task.created_at.clone(),
        updated_at: task.updated_at.clone(),
        revisions: Revisions {
            source: task.source_revision.clone(),
            comparison_base: Some(task.comparison_base.clone())
                .filter(|base| !base.trim().is_empty()),
            default_branch: task.default_revision.clone(),
            output: task.output_commit.clone(),
        },
        sessions: task
            .sessions
            .iter()
            .map(|session| SessionRoute {
                id: session.id.clone(),
                role: session.role.clone(),
                status: session.status.clone(),
                requested_route: session.route.clone(),
                started_at: session.started_at.clone(),
            })
            .collect(),
        pull_request: task.pr_number.map(|number| PrReference {
            number: Some(number),
            url: task.pr_url.clone(),
            source: "recorded_task_reference",
        }),
        latest_review,
        required_commands,
        gaps,
    }
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

/// Builds the representation from one already-consistent snapshot. Pure: the API and the
/// CLI share this assembler so their facts cannot diverge.
pub fn assemble(cycle: &Cycle, tasks: &[Task]) -> RunEvidenceV1 {
    let (batches, mut gaps) = normalize_batches(cycle);
    let execution = cycle.mode == CycleMode::Execution;
    let mut decisions = BTreeMap::new();
    for proposal in &cycle.proposals {
        *decisions.entry(proposal.decision.clone()).or_default() += 1;
    }
    let foreign = tasks.iter().filter(|t| t.cycle_id != cycle.id).count();
    if foreign > 0 {
        gaps.push(format!(
            "{foreign} saved task records name a different cycle and are excluded from this run."
        ));
    }
    let owned: Vec<&Task> = tasks.iter().filter(|t| t.cycle_id == cycle.id).collect();
    let unmatched = owned
        .iter()
        .filter(|task| {
            !cycle
                .proposals
                .iter()
                .any(|proposal| proposal.id == task.proposal.id)
        })
        .count();
    if unmatched > 0 {
        gaps.push(format!(
            "{unmatched} task records in this cycle have no matching saved proposal identity."
        ));
    }
    let proposals = cycle
        .proposals
        .iter()
        .map(|proposal| {
            // The join key is (cycle_id, proposal_id). Never a title, a proposal ID alone,
            // or whichever task is newest.
            let linked: Vec<&Task> = owned
                .iter()
                .copied()
                .filter(|task| task.proposal.id == proposal.id)
                .collect();
            let mut gaps = Vec::new();
            if proposal.decision == "accepted" && linked.is_empty() && execution {
                gaps.push(
                    "The proposal was accepted but no task is linked in this cycle; acceptance is not execution."
                        .to_owned(),
                );
            }
            if linked.len() > 1 {
                gaps.push(format!(
                    "{} tasks match this proposal in this cycle; every match is preserved and none is selected.",
                    linked.len()
                ));
            }
            if proposal.decision != "accepted" && !linked.is_empty() {
                gaps.push(format!(
                    "The proposal is recorded as {} yet {} task(s) are linked; the saved records are inconsistent.",
                    proposal.decision,
                    linked.len()
                ));
            }
            ProposalEvidence {
                id: proposal.id.clone(),
                title: proposal.title.clone(),
                target: proposal.target.clone(),
                tier: proposal.tier.clone(),
                category: proposal.category.clone(),
                problem: proposal.problem.clone(),
                benefit: proposal.benefit.clone(),
                scope: proposal.scope.clone(),
                evidence: proposal.evidence.clone(),
                final_decision: proposal.decision.clone(),
                final_reason: proposal.reason.clone(),
                reviewer_verdicts: (0..REVIEWER_SLOTS.len())
                    .map(|slot| verdict(&batches, slot, &proposal.id))
                    .collect(),
                linked_tasks: linked.into_iter().map(task_evidence).collect(),
                gaps,
            }
        })
        .collect();
    if cycle.status == "running" {
        gaps.push(
            "Planning is still recorded as running, so this run's evidence is incomplete."
                .to_owned(),
        );
    }
    if cycle.grounding.is_none() {
        gaps.push("The cycle has no saved grounding revision.".to_owned());
    }
    if cycle.error.is_some() {
        gaps.push("The cycle recorded a planning error.".to_owned());
    }
    RunEvidenceV1 {
        schema_version: SCHEMA_VERSION,
        generated_at: now(),
        kind: "recorded_review_check_evidence",
        review_required_before_sharing: true,
        review_requirement: "Requires review before sharing. This is a private operator export of saved records, not a public-safe or publication-approved artifact.",
        limitations: LIMITATIONS,
        cycle: CycleEvidence {
            id: cycle.id.clone(),
            number: cycle.number,
            mode: cycle.mode,
            status: cycle.status.clone(),
            started_at: cycle.started_at.clone(),
            completed_at: cycle.completed_at.clone(),
            repository: cycle.repository.clone(),
            grounding_revision: cycle.grounding.as_ref().map(|g| g.revision.clone()),
            planning: PlanningOutcome {
                status: cycle.status.clone(),
                planning_finished: cycle.completed_at.is_some() && cycle.status != "running",
                proposal_count: cycle.proposals.len(),
                decisions,
                creates_execution_queue: execution,
                error_recorded: cycle.error.is_some(),
                reviewer_batches_saved: cycle.assessments.len(),
            },
        },
        proposals,
        gaps,
    }
}

/// Serializes the assembled facts, then redacts the remaining display strings. Existing
/// redaction is defense in depth here; free text still requires manual review.
pub fn evidence_value(cycle: &Cycle, tasks: &[Task]) -> Result<Value> {
    let mut value = serde_json::to_value(assemble(cycle, tasks))?;
    redact_json(&mut value);
    Ok(value)
}

// ---------------------------------------------------------------------------
// Snapshot reads
// ---------------------------------------------------------------------------

/// Reads the selected cycle and every task naming it from one caller-owned transaction,
/// so the cycle and its task evidence always describe the same database state. This never
/// consults the dashboard's recent task window.
pub fn read_snapshot(c: &Connection, cycle_id: &str) -> Result<Option<(Cycle, Vec<Task>)>> {
    let saved: Option<String> = c
        .query_row(
            "SELECT data FROM records WHERE kind='cycle' AND id=?1",
            [cycle_id],
            |r| r.get(0),
        )
        .optional()?;
    let Some(saved) = saved else {
        return Ok(None);
    };
    let cycle: Cycle = serde_json::from_str(&saved)?;
    let mut statement = c.prepare(
        "SELECT data FROM records WHERE kind='task' AND json_extract(data,'$.cycle_id')=?1 ORDER BY id",
    )?;
    let tasks = statement
        .query_map([cycle_id], |r| r.get::<_, String>(0))?
        .map(|row| Ok(serde_json::from_str(&row?)?))
        .collect::<Result<Vec<Task>>>()?;
    Ok(Some((cycle, tasks)))
}

/// Exports one run from saved state without opening the database for writing, creating
/// directories, taking the service lock or running migrations. A missing state database
/// or cycle is an explicit error, never an empty successful export.
pub fn export_run(state_db: &Path, cycle_id: &str) -> Result<Value> {
    let mut c = Store::open_readonly(state_db, "run evidence export")?;
    // One consistent snapshot even while the service is running.
    let tx = c.transaction()?;
    let snapshot = read_snapshot(&tx, cycle_id)?
        .with_context(|| format!("No saved cycle {cycle_id} in this state database"))?;
    evidence_value(&snapshot.0, &snapshot.1)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn cycle() -> Cycle {
        Cycle {
            mode: CycleMode::Execution,
            id: "cycle-1".into(),
            number: 1,
            status: "completed".into(),
            started_at: "2026-09-12T00:00:00Z".into(),
            completed_at: Some("2026-09-12T01:00:00Z".into()),
            grounding: None,
            proposals: vec![],
            assessments: vec![],
            sessions: vec![],
            error: None,
            repository: "owner/name".into(),
            decision_memory: vec![],
            run_id: None,
            lifecycle: Default::default(),
        }
    }
    fn session(role: &str, status: &str) -> Session {
        Session {
            id: format!("{role}-session"),
            role: role.into(),
            route: Route::new("fixture", "low"),
            status: status.into(),
            started_at: "2026-09-12T00:00:00Z".into(),
            summary: "omitted".into(),
        }
    }

    #[test]
    fn malformed_batch_keeps_its_slot_instead_of_shifting_identities() {
        let mut c = cycle();
        c.sessions = vec![
            session("adversary-a", "completed"),
            session("adversary-b", "completed"),
        ];
        c.assessments = vec![
            json!({"unexpected": true}),
            json!({"assessments":[{"id":"p1","decision":"rejected","reason":"b says no"}]}),
        ];
        let (batches, gaps) = normalize_batches(&c);
        assert_eq!(batches[0].slot, Some(0));
        assert_eq!(batches[1].slot, Some(1));
        let a = verdict(&batches, 0, "p1");
        let b = verdict(&batches, 1, "p1");
        assert_eq!(a.state, VerdictState::Malformed);
        assert_eq!(a.decision, None);
        // Reviewer B's verdict must not be promoted into reviewer A's slot.
        assert_eq!(b.state, VerdictState::Recorded);
        assert_eq!(b.decision.as_deref(), Some("rejected"));
        assert!(gaps.iter().any(|g| g.contains("batch 0")));
    }

    #[test]
    fn duplicate_and_missing_verdicts_are_explicit() {
        let mut c = cycle();
        c.sessions = vec![
            session("adversary-a", "completed"),
            session("adversary-b", "completed"),
        ];
        c.assessments = vec![
            json!({"assessments":[
                {"id":"p1","decision":"accepted","reason":"one"},
                {"id":"p1","decision":"rejected","reason":"two"}
            ]}),
            json!({"assessments":[{"id":"other","decision":"deferred","reason":"x"}]}),
        ];
        let (batches, _) = normalize_batches(&c);
        let a = verdict(&batches, 0, "p1");
        assert_eq!(a.state, VerdictState::Duplicate);
        assert_eq!(a.decision, None);
        assert!(a.note.unwrap().contains("inconsistent"));
        let b = verdict(&batches, 1, "p1");
        assert_eq!(b.state, VerdictState::Missing);
        assert_eq!(b.decision, None);
    }

    #[test]
    fn extra_batches_are_unattributable_and_never_reassigned() {
        let mut c = cycle();
        c.assessments = vec![
            json!({"assessments":[]}),
            json!({"assessments":[]}),
            json!({"assessments":[{"id":"p1","decision":"accepted","reason":"third"}]}),
        ];
        let (batches, gaps) = normalize_batches(&c);
        assert_eq!(batches[2].slot, None);
        assert!(gaps.iter().any(|g| g.contains("unattributable")));
        assert_eq!(verdict(&batches, 0, "p1").state, VerdictState::Missing);
        assert_eq!(verdict(&batches, 1, "p1").state, VerdictState::Missing);
    }
}
