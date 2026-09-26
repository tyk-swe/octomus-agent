// Package evidence is the narrow, read-only run-evidence read model.
//
// It reports what was saved, never what is currently true. It performs no live
// HEAD, workspace, remote, authorization or pull-request checks, so the result is
// recorded review/check evidence and not a publication or safety decision. Facts
// are computed from the saved records first; display strings are redacted afterwards.
package evidence

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const SchemaVersion uint32 = 1

// Limitations are what this export cannot claim. Every entry must survive
// verbatim into every export; tests/helpers/public_payload.mjs mirrors the list
// and tests/evidence_snapshot.py runs that gate against a real export.
var Limitations = [9]string{
	"Recorded review and check evidence only. No live HEAD, workspace, remote, authorization or current pull-request checks were performed while producing this export.",
	"Planning completion is not task completion: a completed cycle records decisions, not delivered work.",
	"Deferred is not rejected.",
	"A recorded pull request describes delivery, not merge. Published is not merged.",
	"Audit acceptance is a recommendation. Audit cycles never create an execution queue, so an accepted audit proposal has no linked task by design.",
	"Saved session routes are requested routes. Runtime model identity is not independently reported here.",
	"Costs, delivery time and any replay timeline are not inferred from these records.",
	"Zero or multiple task matches are preserved as recorded. No single task is selected on the caller's behalf.",
	"Free text carried here (proposal problem, benefit, scope and evidence, and code-review findings) is model-authored and still requires manual review before sharing.",
}

// ReviewRequirement is the one sentence every export carries above its records.
const ReviewRequirement = "Requires review before sharing. This is a private operator export of saved records, not a public-safe or publication-approved artifact."

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

type RunEvidenceV1 struct {
	SchemaVersion               uint32             `json:"schema_version"`
	GeneratedAt                 string             `json:"generated_at"`
	Kind                        string             `json:"kind"`
	ReviewRequiredBeforeSharing bool               `json:"review_required_before_sharing"`
	ReviewRequirement           string             `json:"review_requirement"`
	Limitations                 [9]string          `json:"limitations"`
	Cycle                       CycleEvidence      `json:"cycle"`
	Proposals                   []ProposalEvidence `json:"proposals"`
	// Explicit missing, ambiguous or stale evidence observed across the whole run.
	Gaps []string `json:"gaps"`
}

type CycleEvidence struct {
	ID                string          `json:"id"`
	Number            uint64          `json:"number"`
	Mode              model.CycleMode `json:"mode"`
	Status            string          `json:"status"`
	StartedAt         string          `json:"started_at"`
	CompletedAt       *string         `json:"completed_at"`
	Repository        string          `json:"repository"`
	GroundingRevision *string         `json:"grounding_revision"`
	Planning          PlanningOutcome `json:"planning"`
}

type PlanningOutcome struct {
	// The saved cycle status verbatim; `completed` describes planning only.
	Status                string         `json:"status"`
	PlanningFinished      bool           `json:"planning_finished"`
	ProposalCount         int            `json:"proposal_count"`
	Decisions             map[string]int `json:"decisions"`
	CreatesExecutionQueue bool           `json:"creates_execution_queue"`
	ErrorRecorded         bool           `json:"error_recorded"`
	ReviewerBatchesSaved  int            `json:"reviewer_batches_saved"`
}

type ProposalEvidence struct {
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Target           string            `json:"target"`
	Tier             string            `json:"tier"`
	Category         string            `json:"category"`
	Problem          string            `json:"problem"`
	Benefit          string            `json:"benefit"`
	Scope            string            `json:"scope"`
	Evidence         []string          `json:"evidence"`
	FinalDecision    string            `json:"final_decision"`
	FinalReason      string            `json:"final_reason"`
	ReviewerVerdicts []ReviewerVerdict `json:"reviewer_verdicts"`
	LinkedTasks      []TaskEvidence    `json:"linked_tasks"`
	Gaps             []string          `json:"gaps"`
}

type VerdictState string

const (
	// Exactly one identifiable verdict for this proposal in this reviewer's batch.
	VerdictRecorded VerdictState = "recorded"
	// No batch for this slot, or the batch records no verdict for this proposal.
	VerdictMissing VerdictState = "missing"
	// More than one verdict for this proposal in the same batch.
	VerdictDuplicate VerdictState = "duplicate"
	// The batch exists but is not a usable assessment list.
	VerdictMalformed VerdictState = "malformed"
)

type ReviewerVerdict struct {
	Reviewer string       `json:"reviewer"`
	State    VerdictState `json:"state"`
	Decision *string      `json:"decision"`
	Reason   *string      `json:"reason"`
	// Why the verdict is not a plain `recorded` value, when it is not.
	Note *string `json:"note"`
}

type TaskEvidence struct {
	ID               string          `json:"id"`
	CycleID          string          `json:"cycle_id"`
	ProposalID       string          `json:"proposal_id"`
	Status           model.Status    `json:"status"`
	Branch           string          `json:"branch"`
	Attempts         uint64          `json:"attempts"`
	BlockedReason    *string         `json:"blocked_reason"`
	ErrorRecorded    bool            `json:"error_recorded"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
	Revisions        Revisions       `json:"revisions"`
	Sessions         []SessionRoute  `json:"sessions"`
	LatestReview     ReviewEvidence  `json:"latest_review"`
	RequiredCommands CommandEvidence `json:"required_commands"`
	PullRequest      *PrReference    `json:"pull_request"`
	Gaps             []string        `json:"gaps"`
}

type Revisions struct {
	Source         string  `json:"source"`
	ComparisonBase *string `json:"comparison_base"`
	DefaultBranch  string  `json:"default_branch"`
	Output         *string `json:"output"`
}

type SessionRoute struct {
	ID             string       `json:"id"`
	Role           string       `json:"role"`
	Status         string       `json:"status"`
	RequestedRoute config.Route `json:"requested_route"`
	StartedAt      string       `json:"started_at"`
}

type ReviewEvidence struct {
	RoundsRecorded int `json:"rounds_recorded"`
	// The latest saved review round, never the last convenient passing one.
	Latest                *ReviewRoundEvidence `json:"latest"`
	Clean                 bool                 `json:"clean"`
	CleanAtOutputRevision bool                 `json:"clean_at_output_revision"`
}

type ReviewRoundEvidence struct {
	SessionID             string            `json:"session_id"`
	Revision              string            `json:"revision"`
	ComparisonBase        string            `json:"comparison_base"`
	CreatedAt             string            `json:"created_at"`
	Completed             bool              `json:"completed"`
	SummaryPresent        bool              `json:"summary_present"`
	MatchesOutputRevision *bool             `json:"matches_output_revision"`
	Findings              []FindingEvidence `json:"findings"`
}

type FindingEvidence struct {
	Title    string `json:"title"`
	File     string `json:"file"`
	Priority string `json:"priority"`
	Detail   string `json:"detail"`
}

type ChecksState string

const (
	// The task's saved execution configuration requires no verification commands.
	ChecksNotConfigured ChecksState = "not_configured"
	ChecksRecorded      ChecksState = "recorded"
)

type CommandEvidence struct {
	State                     ChecksState     `json:"state"`
	Commands                  []CommandResult `json:"commands"`
	AllPassedAtOutputRevision bool            `json:"all_passed_at_output_revision"`
}

type CommandState string

const (
	// Latest recorded result succeeded at the recorded output revision.
	CommandPassed CommandState = "passed"
	// Latest recorded result succeeded, but not at a recorded output revision.
	CommandPassedAtOtherRevision CommandState = "passed_at_other_revision"
	CommandFailed                CommandState = "failed"
	// No result is recorded for this required command. Missing is not passing.
	CommandNoResult CommandState = "no_result"
)

type CommandResult struct {
	Command               string       `json:"command"`
	State                 CommandState `json:"state"`
	ResultsRecorded       int          `json:"results_recorded"`
	LatestSuccess         *bool        `json:"latest_success"`
	LatestRevision        *string      `json:"latest_revision"`
	LatestCreatedAt       *string      `json:"latest_created_at"`
	MatchesOutputRevision *bool        `json:"matches_output_revision"`
}

type PrReference struct {
	Number *uint64 `json:"number"`
	URL    *string `json:"url"`
	Source string  `json:"source"`
}

// ---------------------------------------------------------------------------
// Reviewer batch normalization
// ---------------------------------------------------------------------------

type entry struct {
	id       string
	decision string
	reason   string
}
type batch struct {
	// Positional reviewer slot, or -1 when there is no slot for this batch.
	slot             int
	index            int
	entries          []entry
	malformedEntries int
	// Set when the batch itself cannot be read as an assessment list.
	unusable *string
	// Set when no completed session confirms this slot's identity.
	unconfirmed bool
}

func slotSessions(cycle model.Cycle, role string) []model.Session {
	var sessions []model.Session
	for _, s := range cycle.Sessions {
		if s.Role == role {
			sessions = append(sessions, s)
		}
	}
	return sessions
}

func stringField(object map[string]any, key string) (string, bool) {
	value, ok := object[key].(string)
	return value, ok
}

// normalizeBatches reads the saved batches positionally and reports every
// inconsistency instead of repairing it. Malformed batches keep their slot so
// reviewer identities cannot shift.
func normalizeBatches(cycle model.Cycle) ([]batch, []string) {
	slots := model.ReviewerSlots()
	gaps := []string{}
	batches := []batch{}
	for index, raw := range cycle.Assessments {
		b := batch{slot: -1, index: index}
		if index < len(slots) {
			b.slot = index
		}
		object, _ := raw.(map[string]any)
		items, ok := object["assessments"].([]any)
		if !ok {
			reason := "saved batch does not contain a recorded assessment list"
			b.unusable = &reason
		} else {
			for _, item := range items {
				fields, _ := item.(map[string]any)
				id, _ := stringField(fields, "id")
				id = strings.TrimSpace(id)
				decision, _ := stringField(fields, "decision")
				if id == "" || !slices.Contains(model.Assessments(), decision) {
					b.malformedEntries++
					continue
				}
				reason, _ := stringField(fields, "reason")
				b.entries = append(b.entries, entry{id: id, decision: decision, reason: reason})
			}
		}
		batches = append(batches, b)
	}
	for slot, role := range slots {
		sessions := slotSessions(cycle, role)
		completed := model.CompletedSessions(sessions)
		saved := -1
		for i := range batches {
			if batches[i].slot == slot {
				saved = i
				break
			}
		}
		if len(sessions) > 1 {
			gaps = append(gaps, fmt.Sprintf("%d %s sessions are recorded; batch attribution is positional and cannot be confirmed.", len(sessions), role))
		}
		if saved >= 0 && completed == 0 {
			batches[saved].unconfirmed = true
			gaps = append(gaps, fmt.Sprintf("Saved assessment batch %d is attributed to %s positionally, but no completed %s session confirms it.", slot, role, role))
		}
		if saved < 0 {
			if completed > 0 {
				gaps = append(gaps, fmt.Sprintf("Reviewer %s recorded a completed session but no saved assessment batch; its verdicts are missing.", role))
			} else {
				gaps = append(gaps, fmt.Sprintf("Reviewer %s has neither a completed session nor a saved assessment batch.", role))
			}
		}
	}
	for _, b := range batches {
		owner := "no reviewer slot"
		if b.slot >= 0 {
			owner = slots[b.slot]
		}
		if b.slot < 0 {
			gaps = append(gaps, fmt.Sprintf("Saved assessment batch %d exceeds the two recorded reviewer roles and is unattributable.", b.index))
		}
		if b.unusable != nil {
			gaps = append(gaps, fmt.Sprintf("Saved assessment batch %d (%s) is malformed: %s. It was not reassigned to another reviewer.", b.index, owner, *b.unusable))
		}
		if b.malformedEntries > 0 {
			gaps = append(gaps, fmt.Sprintf("Saved assessment batch %d (%s) contains %d unreadable entries, which are not attributed to any proposal.", b.index, owner, b.malformedEntries))
		}
	}
	return batches, gaps
}

func str(s string) *string { return &s }

func verdict(batches []batch, slot int, proposalID string) ReviewerVerdict {
	reviewer := model.ReviewerSlots()[slot]
	missing := func(note string) ReviewerVerdict {
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictMissing, Note: str(note)}
	}
	var found *batch
	for i := range batches {
		if batches[i].slot == slot {
			found = &batches[i]
			break
		}
	}
	if found == nil {
		return missing(fmt.Sprintf("No saved assessment batch is attributed to %s.", reviewer))
	}
	if found.unusable != nil {
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictMalformed, Note: str(fmt.Sprintf("Batch %d is malformed: %s.", found.index, *found.unusable))}
	}
	var matches []entry
	for _, e := range found.entries {
		if e.id == proposalID {
			matches = append(matches, e)
		}
	}
	var unconfirmed *string
	if found.unconfirmed {
		unconfirmed = str(fmt.Sprintf("Batch %d is attributed to %s positionally and is unconfirmed by a completed session.", found.index, reviewer))
	}
	switch len(matches) {
	case 0:
		return missing(fmt.Sprintf("Batch %d records no verdict for this proposal.", found.index))
	case 1:
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictRecorded, Decision: str(matches[0].decision), Reason: str(matches[0].reason), Note: unconfirmed}
	default:
		decisions := make([]string, 0, len(matches))
		agreed := true
		for i, m := range matches {
			decisions = append(decisions, m.decision)
			if i > 0 && m.decision != matches[i-1].decision {
				agreed = false
			}
		}
		var decision *string
		verdictText := "they are inconsistent and no verdict is selected"
		if agreed {
			decision = str(matches[0].decision)
			verdictText = "they agree but remain duplicated"
		}
		suffix := "."
		if unconfirmed != nil {
			suffix = ". " + *unconfirmed
		}
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictDuplicate, Decision: decision, Note: str(fmt.Sprintf("Batch %d records %d verdicts for this proposal (%s); %s%s", found.index, len(matches), strings.Join(decisions, ", "), verdictText, suffix))}
	}
}

// ---------------------------------------------------------------------------
// Task evidence
// ---------------------------------------------------------------------------

func reviewEvidence(task model.Task) ReviewEvidence {
	var latest *ReviewRoundEvidence
	clean := false
	if n := len(task.Reviews); n > 0 {
		round := task.Reviews[n-1]
		findings := make([]FindingEvidence, 0, len(round.Result.Findings))
		for _, f := range round.Result.Findings {
			findings = append(findings, FindingEvidence{Title: f.Title, File: f.File, Priority: f.Priority, Detail: f.Detail})
		}
		var matches *bool
		if task.OutputCommit != nil {
			m := *task.OutputCommit == round.Revision
			matches = &m
		}
		latest = &ReviewRoundEvidence{
			SessionID:             round.SessionID,
			Revision:              round.Revision,
			ComparisonBase:        round.ComparisonBase,
			CreatedAt:             round.CreatedAt,
			Completed:             round.Result.Completed,
			SummaryPresent:        strings.TrimSpace(round.Result.Summary) != "",
			MatchesOutputRevision: matches,
			Findings:              findings,
		}
		clean = round.Result.Clean()
	}
	return ReviewEvidence{
		RoundsRecorded:        len(task.Reviews),
		Latest:                latest,
		Clean:                 clean,
		CleanAtOutputRevision: clean && latest != nil && latest.MatchesOutputRevision != nil && *latest.MatchesOutputRevision,
	}
}

func commandEvidence(task model.Task) CommandEvidence {
	required := task.ExecutionConfig().VerificationCommands
	commands := make([]CommandResult, 0, len(required))
	for _, command := range required {
		var recorded []model.Verification
		for _, result := range task.Verification {
			if result.Command == command {
				recorded = append(recorded, result)
			}
		}
		// The latest recorded result decides: a newer failure or a different revision
		// invalidates an older pass.
		out := CommandResult{Command: command, ResultsRecorded: len(recorded), State: CommandNoResult}
		if n := len(recorded); n > 0 {
			latest := recorded[n-1]
			var matches *bool
			if task.OutputCommit != nil {
				m := *task.OutputCommit == latest.Revision
				matches = &m
			}
			switch {
			case !latest.Success:
				out.State = CommandFailed
			case matches != nil && *matches:
				out.State = CommandPassed
			default:
				out.State = CommandPassedAtOtherRevision
			}
			success := latest.Success
			out.LatestSuccess = &success
			out.LatestRevision = str(latest.Revision)
			out.LatestCreatedAt = str(latest.CreatedAt)
			out.MatchesOutputRevision = matches
		}
		commands = append(commands, out)
	}
	state := ChecksRecorded
	if len(required) == 0 {
		state = ChecksNotConfigured
	}
	allPassed := len(commands) > 0
	for _, c := range commands {
		if c.State != CommandPassed {
			allPassed = false
		}
	}
	return CommandEvidence{State: state, Commands: commands, AllPassedAtOutputRevision: allPassed}
}

func taskEvidence(task model.Task) TaskEvidence {
	latestReview := reviewEvidence(task)
	requiredCommands := commandEvidence(task)
	gaps := []string{}
	if strings.TrimSpace(task.ComparisonBase) == "" {
		gaps = append(gaps, "No comparison base is persisted, so the recorded review scope cannot be reconstructed.")
	}
	if task.OutputCommit != nil {
		if !latestReview.CleanAtOutputRevision {
			gaps = append(gaps, "An output revision is recorded without a clean latest review at that revision.")
		}
		if requiredCommands.State == ChecksRecorded && !requiredCommands.AllPassedAtOutputRevision {
			gaps = append(gaps, "An output revision is recorded without every required command passing at that revision.")
		}
	}
	if requiredCommands.State == ChecksNotConfigured {
		gaps = append(gaps, "The task's saved execution configuration requires no verification commands, so no check evidence exists.")
	}
	if task.Status == model.StatusPublished {
		if task.PRNumber == nil {
			gaps = append(gaps, "The task is recorded as published without a pull-request reference.")
		}
		if task.OutputCommit == nil {
			gaps = append(gaps, "The task is recorded as published without an output revision.")
		}
	}
	if task.PRNumber != nil && task.OutputCommit == nil {
		gaps = append(gaps, "A pull-request reference is recorded without an output revision to compare it against.")
	}
	// The same snake_case token the task API serializes, so one saved reason never
	// has two spellings across the task view, the run evidence and the CLI export.
	var blocked *string
	if task.BlockedReason != nil {
		blocked = str(task.BlockedReason.String())
	}
	var comparison *string
	if strings.TrimSpace(task.ComparisonBase) != "" {
		comparison = str(task.ComparisonBase)
	}
	sessions := make([]SessionRoute, 0, len(task.Sessions))
	for _, s := range task.Sessions {
		sessions = append(sessions, SessionRoute{ID: s.ID, Role: s.Role, Status: s.Status, RequestedRoute: s.Route.Clone(), StartedAt: s.StartedAt})
	}
	var pr *PrReference
	if task.PRNumber != nil {
		number := *task.PRNumber
		pr = &PrReference{Number: &number, URL: cloneString(task.PRURL), Source: "recorded_task_reference"}
	}
	return TaskEvidence{
		ID:            task.ID,
		CycleID:       task.CycleID,
		ProposalID:    task.Proposal.ID,
		Status:        task.Status,
		Branch:        task.Branch,
		Attempts:      task.Attempts,
		BlockedReason: blocked,
		ErrorRecorded: task.Error != nil,
		CreatedAt:     task.CreatedAt,
		UpdatedAt:     task.UpdatedAt,
		Revisions: Revisions{
			Source:         task.SourceRevision,
			ComparisonBase: comparison,
			DefaultBranch:  task.DefaultRevision,
			Output:         cloneString(task.OutputCommit),
		},
		Sessions:         sessions,
		LatestReview:     latestReview,
		RequiredCommands: requiredCommands,
		PullRequest:      pr,
		Gaps:             gaps,
	}
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	copied := *s
	return &copied
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

// Assemble builds the representation from one already-consistent snapshot. Pure:
// the API and the CLI share this assembler so their facts cannot diverge.
func Assemble(cycle model.Cycle, tasks []model.Task) RunEvidenceV1 {
	batches, gaps := normalizeBatches(cycle)
	execution := cycle.Mode == model.CycleModeExecution
	decisions := map[string]int{}
	for _, p := range cycle.Proposals {
		decisions[p.Decision]++
	}
	foreign := 0
	var owned []model.Task
	for _, t := range tasks {
		if t.CycleID != cycle.ID {
			foreign++
		} else {
			owned = append(owned, t)
		}
	}
	if foreign > 0 {
		gaps = append(gaps, fmt.Sprintf("%d saved task records name a different cycle and are excluded from this run.", foreign))
	}
	unmatched := 0
	for _, t := range owned {
		matched := false
		for _, p := range cycle.Proposals {
			if p.ID == t.Proposal.ID {
				matched = true
				break
			}
		}
		if !matched {
			unmatched++
		}
	}
	if unmatched > 0 {
		gaps = append(gaps, fmt.Sprintf("%d task records in this cycle have no matching saved proposal identity.", unmatched))
	}
	proposals := make([]ProposalEvidence, 0, len(cycle.Proposals))
	slots := model.ReviewerSlots()
	for _, proposal := range cycle.Proposals {
		// The join key is (cycle_id, proposal_id). Never a title, a proposal ID alone,
		// or whichever task is newest.
		var linked []model.Task
		for _, t := range owned {
			if t.Proposal.ID == proposal.ID {
				linked = append(linked, t)
			}
		}
		proposalGaps := []string{}
		if proposal.Decision == model.DecisionAccepted && len(linked) == 0 && execution {
			proposalGaps = append(proposalGaps, "The proposal was accepted but no task is linked in this cycle; acceptance is not execution.")
		}
		if len(linked) > 1 {
			proposalGaps = append(proposalGaps, fmt.Sprintf("%d tasks match this proposal in this cycle; every match is preserved and none is selected.", len(linked)))
		}
		if proposal.Decision != model.DecisionAccepted && len(linked) > 0 {
			proposalGaps = append(proposalGaps, fmt.Sprintf("The proposal is recorded as %s yet %d task(s) are linked; the saved records are inconsistent.", proposal.Decision, len(linked)))
		}
		verdicts := make([]ReviewerVerdict, 0, len(slots))
		for slot := range slots {
			verdicts = append(verdicts, verdict(batches, slot, proposal.ID))
		}
		linkedTasks := make([]TaskEvidence, 0, len(linked))
		for _, t := range linked {
			linkedTasks = append(linkedTasks, taskEvidence(t))
		}
		evidence := append([]string{}, proposal.Evidence...)
		proposals = append(proposals, ProposalEvidence{
			ID:               proposal.ID,
			Title:            proposal.Title,
			Target:           proposal.Target,
			Tier:             proposal.Tier,
			Category:         proposal.Category,
			Problem:          proposal.Problem,
			Benefit:          proposal.Benefit,
			Scope:            proposal.Scope,
			Evidence:         evidence,
			FinalDecision:    proposal.Decision,
			FinalReason:      proposal.Reason,
			ReviewerVerdicts: verdicts,
			LinkedTasks:      linkedTasks,
			Gaps:             proposalGaps,
		})
	}
	if cycle.Status == model.CycleRunning {
		gaps = append(gaps, "Planning is still recorded as running, so this run's evidence is incomplete.")
	}
	if cycle.Grounding == nil {
		gaps = append(gaps, "The cycle has no saved grounding revision.")
	}
	if cycle.Error != nil {
		gaps = append(gaps, "The cycle recorded a planning error.")
	}
	var groundingRevision *string
	if cycle.Grounding != nil {
		groundingRevision = str(cycle.Grounding.Revision)
	}
	return RunEvidenceV1{
		SchemaVersion:               SchemaVersion,
		GeneratedAt:                 model.Now(),
		Kind:                        "recorded_review_check_evidence",
		ReviewRequiredBeforeSharing: true,
		ReviewRequirement:           ReviewRequirement,
		Limitations:                 Limitations,
		Cycle: CycleEvidence{
			ID:                cycle.ID,
			Number:            cycle.Number,
			Mode:              cycle.Mode,
			Status:            cycle.Status,
			StartedAt:         cycle.StartedAt,
			CompletedAt:       cloneString(cycle.CompletedAt),
			Repository:        cycle.Repository,
			GroundingRevision: groundingRevision,
			Planning: PlanningOutcome{
				Status:                cycle.Status,
				PlanningFinished:      cycle.CompletedAt != nil && cycle.Status != model.CycleRunning,
				ProposalCount:         len(cycle.Proposals),
				Decisions:             decisions,
				CreatesExecutionQueue: execution,
				ErrorRecorded:         cycle.Error != nil,
				ReviewerBatchesSaved:  len(cycle.Assessments),
			},
		},
		Proposals: proposals,
		Gaps:      gaps,
	}
}

// Value serializes the assembled facts, then redacts the remaining display
// strings. Existing redaction is defense in depth here; free text still
// requires manual review.
func Value(cycle model.Cycle, tasks []model.Task) (map[string]any, error) {
	return store.RedactedValue(Assemble(cycle, tasks))
}

// ---------------------------------------------------------------------------
// Snapshot reads
// ---------------------------------------------------------------------------

// cycleTasksQuery selects every task naming one cycle, in task id order,
// through the record_meta projection (cycle_id is the task's saved cycle_id).
// Without INDEXED BY the planner prefers walking every saved task in id order
// to satisfy ORDER BY, and RunEvidence runs this read under the store mutex.
const cycleTasksQuery = "SELECT r.data FROM record_meta m INDEXED BY meta_cycle JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.cycle_id=?1 ORDER BY r.id"

// ReadSnapshot reads the selected cycle and every task naming it from one
// caller-owned transaction, so the cycle and its task evidence always describe
// the same database state. This never consults the dashboard's recent task window.
func ReadSnapshot(c *sql.Conn, cycleID string) (*model.Cycle, []model.Task, error) {
	var saved string
	err := c.QueryRowContext(store.Background(), "SELECT data FROM records WHERE kind='cycle' AND id=?1", cycleID).Scan(&saved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var cycle model.Cycle
	if err := json.Unmarshal([]byte(saved), &cycle); err != nil {
		return nil, nil, err
	}
	rows, err := c.QueryContext(store.Background(), cycleTasksQuery, cycleID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	tasks := []model.Task{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, nil, err
		}
		var task model.Task
		if err := json.Unmarshal([]byte(data), &task); err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, task)
	}
	return &cycle, tasks, rows.Err()
}

// snapshotter runs fn inside one read transaction: the service store or a
// read-only export connection.
type snapshotter interface {
	Snapshot(fn func(c *sql.Conn) error) error
}

// readRun reads one cycle and its tasks from a single snapshot. A missing
// cycle is a nil cycle with no error.
func readRun(s snapshotter, cycleID string) (cycle *model.Cycle, tasks []model.Task, err error) {
	err = s.Snapshot(func(c *sql.Conn) error {
		var readErr error
		cycle, tasks, readErr = ReadSnapshot(c, cycleID)
		return readErr
	})
	return cycle, tasks, err
}

// RunEvidence is the service-side export: recorded run evidence for one cycle
// read inside one transaction on the store's connection, then assembled and
// redacted without holding the database lock.
func RunEvidence(s *store.Store, cycleID string) (map[string]any, error) {
	cycle, tasks, err := readRun(s, cycleID)
	if err != nil || cycle == nil {
		return nil, err
	}
	return Value(*cycle, tasks)
}

// ExportRun exports one run from saved state without opening the database for
// writing, creating directories or taking the service lock.
// A missing state database or cycle is an explicit error, never an empty
// successful export.
func ExportRun(stateDB, cycleID string) (map[string]any, error) {
	r, err := store.OpenReadOnly(stateDB, "run evidence export")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// One consistent snapshot even while the service is running.
	cycle, tasks, err := readRun(r, cycleID)
	if err != nil {
		return nil, err
	}
	if cycle == nil {
		return nil, fmt.Errorf("No saved cycle %s in this state database", cycleID)
	}
	return Value(*cycle, tasks)
}
