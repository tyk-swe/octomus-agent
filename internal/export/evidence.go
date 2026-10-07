package export

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

const SchemaVersion uint32 = 1

var limitations = [9]string{
	"Recorded review and check evidence only. No live HEAD, workspace, remote, authorization or current pull-request checks were performed while producing this export.",
	"Planning completion is not task completion: a completed cycle records decisions, not delivered work.",
	"Deferred is not rejected.",
	"A recorded pull request describes delivery, not merge. Published is not merged. Automatic merge evidence is the saved durable record: confirmed means the service's own squash merge response; observed records the remote outcome without naming an actor.",
	"Audit acceptance is a recommendation. Audit cycles never create an execution queue, so an accepted audit proposal has no linked task by design.",
	"Saved session routes are requested routes. Runtime model identity is not independently reported here.",
	"Costs, delivery time and any replay timeline are not inferred from these records.",
	"Zero or multiple task matches are preserved as recorded. No single task is selected on the caller's behalf.",
	"Free text carried here (proposal problem, benefit, scope and evidence, and code-review findings) is model-authored and still requires manual review before sharing.",
}

const reviewRequirement = "Requires review before sharing. This is a private operator export of saved records, not a public-safe or publication-approved artifact."

type RunEvidenceV1 struct {
	SchemaVersion     uint32             `json:"schema_version"`
	GeneratedAt       string             `json:"generated_at"`
	Kind              string             `json:"kind"`
	ReviewRequired    bool               `json:"review_required_before_sharing"`
	ReviewRequirement string             `json:"review_requirement"`
	Limitations       [9]string          `json:"limitations"`
	Cycle             CycleEvidence      `json:"cycle"`
	Proposals         []ProposalEvidence `json:"proposals"`
	Gaps              []string           `json:"gaps"`
}

type CycleEvidence struct {
	ID                string              `json:"id"`
	Number            uint64              `json:"number"`
	Mode              model.CycleMode     `json:"mode"`
	DeliveryMode      config.DeliveryMode `json:"delivery_mode"`
	Status            model.CycleStatus   `json:"status"`
	StartedAt         string              `json:"started_at"`
	CompletedAt       *string             `json:"completed_at"`
	Repository        string              `json:"repository"`
	GroundingRevision *string             `json:"grounding_revision"`
	Planning          PlanningOutcome     `json:"planning"`
}

type PlanningOutcome struct {
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
	VerdictRecorded  VerdictState = "recorded"
	VerdictMissing   VerdictState = "missing"
	VerdictDuplicate VerdictState = "duplicate"
	VerdictMalformed VerdictState = "malformed"
)

type ReviewerVerdict struct {
	Reviewer string       `json:"reviewer"`
	State    VerdictState `json:"state"`
	Decision *string      `json:"decision"`
	Reason   *string      `json:"reason"`
	Note     *string      `json:"note"`
}

type TaskEvidence struct {
	ID                   string                      `json:"id"`
	CycleID              string                      `json:"cycle_id"`
	ProposalID           string                      `json:"proposal_id"`
	Status               model.Status                `json:"status"`
	Branch               string                      `json:"branch"`
	Attempts             uint64                      `json:"attempts"`
	BlockedReason        *string                     `json:"blocked_reason"`
	ErrorRecorded        bool                        `json:"error_recorded"`
	CreatedAt            string                      `json:"created_at"`
	UpdatedAt            string                      `json:"updated_at"`
	DeliveryMode         config.DeliveryMode         `json:"delivery_mode"`
	MaintenanceFootprint *model.MaintenanceFootprint `json:"maintenance_footprint"`
	AutoMerge            *model.AutoMergeState       `json:"auto_merge"`
	Revisions            Revisions                   `json:"revisions"`
	Sessions             []SessionRoute              `json:"sessions"`
	LatestReview         ReviewEvidence              `json:"latest_review"`
	RequiredCommands     CommandEvidence             `json:"required_commands"`
	PullRequest          *PRReference                `json:"pull_request"`
	Gaps                 []string                    `json:"gaps"`
}

type Revisions struct {
	Source         string  `json:"source"`
	ComparisonBase *string `json:"comparison_base"`
	DefaultBranch  string  `json:"default_branch"`
	Output         *string `json:"output"`
}

type SessionRoute struct {
	ID             string              `json:"id"`
	Role           string              `json:"role"`
	Status         model.SessionStatus `json:"status"`
	RequestedRoute config.Route        `json:"requested_route"`
	StartedAt      string              `json:"started_at"`
}

type ReviewEvidence struct {
	RoundsRecorded int                  `json:"rounds_recorded"`
	Latest         *ReviewRoundEvidence `json:"latest"`
	Clean          bool                 `json:"clean"`
	CleanAtOutput  bool                 `json:"clean_at_output_revision"`
}

type ReviewRoundEvidence struct {
	SessionID           string                       `json:"session_id"`
	Revision            string                       `json:"revision"`
	ComparisonBase      string                       `json:"comparison_base"`
	CreatedAt           string                       `json:"created_at"`
	Completed           bool                         `json:"completed"`
	SummaryPresent      bool                         `json:"summary_present"`
	AtOutput            *bool                        `json:"matches_output_revision"`
	Maintenance         *model.MaintenanceAssessment `json:"maintenance"`
	TrustedDiffComplete bool                         `json:"trusted_diff_complete"`
	Findings            []FindingEvidence            `json:"findings"`
}

type FindingEvidence struct {
	Title    string `json:"title"`
	File     string `json:"file"`
	Priority string `json:"priority"`
	Detail   string `json:"detail"`
}

type ChecksState string

const (
	ChecksNotConfigured ChecksState = "not_configured"
	ChecksRecorded      ChecksState = "recorded"
)

type CommandEvidence struct {
	State     ChecksState     `json:"state"`
	Commands  []CommandResult `json:"commands"`
	AllPassed bool            `json:"all_passed_at_output_revision"`
}

type CommandState string

const (
	CommandPassed          CommandState = "passed"
	CommandPassedElsewhere CommandState = "passed_at_other_revision"
	CommandFailed          CommandState = "failed"
	CommandNoResult        CommandState = "no_result"
)

type CommandResult struct {
	Command         string       `json:"command"`
	State           CommandState `json:"state"`
	ResultsRecorded int          `json:"results_recorded"`
	LatestSuccess   *bool        `json:"latest_success"`
	LatestRevision  *string      `json:"latest_revision"`
	LatestCreatedAt *string      `json:"latest_created_at"`
	AtOutput        *bool        `json:"matches_output_revision"`
}

type PRReference struct {
	Number *uint64 `json:"number"`
	URL    *string `json:"url"`
	Source string  `json:"source"`
}

type entry struct {
	id       string
	decision string
	reason   string
}
type batch struct {
	slot             int
	index            int
	entries          []entry
	malformedEntries int
	unusable         *string
	unconfirmed      bool
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
				id, _ := fields["id"].(string)
				id = strings.TrimSpace(id)
				decision, _ := fields["decision"].(string)
				if id == "" || !slices.Contains(model.Assessments(), decision) {
					b.malformedEntries++
					continue
				}
				reason, _ := fields["reason"].(string)
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

func verdict(batches []batch, slot int, proposalID string) ReviewerVerdict {
	reviewer := model.ReviewerSlots()[slot]
	missing := func(note string) ReviewerVerdict {
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictMissing, Note: new(note)}
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
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictMalformed, Note: new(fmt.Sprintf("Batch %d is malformed: %s.", found.index, *found.unusable))}
	}
	var matches []entry
	for _, e := range found.entries {
		if e.id == proposalID {
			matches = append(matches, e)
		}
	}
	var unconfirmed *string
	if found.unconfirmed {
		unconfirmed = new(fmt.Sprintf("Batch %d is attributed to %s positionally and is unconfirmed by a completed session.", found.index, reviewer))
	}
	switch len(matches) {
	case 0:
		return missing(fmt.Sprintf("Batch %d records no verdict for this proposal.", found.index))
	case 1:
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictRecorded, Decision: new(matches[0].decision), Reason: new(matches[0].reason), Note: unconfirmed}
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
			decision = new(matches[0].decision)
			verdictText = "they agree but remain duplicated"
		}
		suffix := "."
		if unconfirmed != nil {
			suffix = ". " + *unconfirmed
		}
		return ReviewerVerdict{Reviewer: reviewer, State: VerdictDuplicate, Decision: decision, Note: new(fmt.Sprintf("Batch %d records %d verdicts for this proposal (%s); %s%s", found.index, len(matches), strings.Join(decisions, ", "), verdictText, suffix))}
	}
}

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
			matches = new(*task.OutputCommit == round.Revision)
		}
		latest = &ReviewRoundEvidence{
			SessionID:           round.SessionID,
			Revision:            round.Revision,
			ComparisonBase:      round.ComparisonBase,
			CreatedAt:           round.CreatedAt,
			Completed:           round.Result.Completed,
			SummaryPresent:      strings.TrimSpace(round.Result.Summary) != "",
			AtOutput:            matches,
			Maintenance:         round.Maintenance,
			TrustedDiffComplete: round.TrustedDiffComplete,
			Findings:            findings,
		}
		clean = round.Result.Clean()
	}
	return ReviewEvidence{
		RoundsRecorded: len(task.Reviews),
		Latest:         latest,
		Clean:          clean,
		CleanAtOutput:  clean && latest != nil && latest.AtOutput != nil && *latest.AtOutput,
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
		out := CommandResult{Command: command, ResultsRecorded: len(recorded), State: CommandNoResult}
		if n := len(recorded); n > 0 {
			latest := recorded[n-1]
			var matches *bool
			if task.OutputCommit != nil {
				matches = new(*task.OutputCommit == latest.Revision)
			}
			switch {
			case !latest.Success:
				out.State = CommandFailed
			case matches != nil && *matches:
				out.State = CommandPassed
			default:
				out.State = CommandPassedElsewhere
			}
			out.LatestSuccess = new(latest.Success)
			out.LatestRevision = new(latest.Revision)
			out.LatestCreatedAt = new(latest.CreatedAt)
			out.AtOutput = matches
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
	return CommandEvidence{State: state, Commands: commands, AllPassed: allPassed}
}

func taskEvidence(task model.Task) TaskEvidence {
	latestReview := reviewEvidence(task)
	requiredCommands := commandEvidence(task)
	gaps := []string{}
	if strings.TrimSpace(task.ComparisonBase) == "" {
		gaps = append(gaps, "No comparison base is persisted, so the recorded review scope cannot be reconstructed.")
	}
	if task.OutputCommit != nil {
		if !latestReview.CleanAtOutput {
			gaps = append(gaps, "An output revision is recorded without a clean latest review at that revision.")
		}
		if requiredCommands.State == ChecksRecorded && !requiredCommands.AllPassed {
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
	var blocked *string
	if task.BlockedReason != nil {
		blocked = new(task.BlockedReason.String())
	}
	var comparison *string
	if strings.TrimSpace(task.ComparisonBase) != "" {
		comparison = new(task.ComparisonBase)
	}
	sessions := make([]SessionRoute, 0, len(task.Sessions))
	for _, s := range task.Sessions {
		sessions = append(sessions, SessionRoute{ID: s.ID, Role: s.Role, Status: s.Status, RequestedRoute: s.Route.Clone(), StartedAt: s.StartedAt})
	}
	var pr *PRReference
	if task.PRNumber != nil {
		pr = &PRReference{Number: new(*task.PRNumber), URL: wirejson.Clone(task.PRURL), Source: "recorded_task_reference"}
	}
	return TaskEvidence{
		ID:                   task.ID,
		CycleID:              task.CycleID,
		ProposalID:           task.Proposal.ID,
		Status:               task.Status,
		Branch:               task.Branch,
		Attempts:             task.Attempts,
		BlockedReason:        blocked,
		ErrorRecorded:        task.Error != nil,
		CreatedAt:            task.CreatedAt,
		UpdatedAt:            task.UpdatedAt,
		DeliveryMode:         task.Config.DeliveryMode,
		MaintenanceFootprint: task.MaintenanceFootprint,
		Revisions: Revisions{
			Source:         task.SourceRevision,
			ComparisonBase: comparison,
			DefaultBranch:  task.DefaultRevision,
			Output:         wirejson.Clone(task.OutputCommit),
		},
		Sessions:         sessions,
		LatestReview:     latestReview,
		RequiredCommands: requiredCommands,
		PullRequest:      pr,
		Gaps:             gaps,
	}
}

func proposalEvidence(proposal model.Proposal, linked []model.Task, batches []batch, execution bool) ProposalEvidence {
	gaps := []string{}
	if proposal.Decision == model.DecisionAccepted && len(linked) == 0 && execution {
		gaps = append(gaps, "The proposal was accepted but no task is linked in this cycle; acceptance is not execution.")
	}
	if len(linked) > 1 {
		gaps = append(gaps, fmt.Sprintf("%d tasks match this proposal in this cycle; every match is preserved and none is selected.", len(linked)))
	}
	if proposal.Decision != model.DecisionAccepted && len(linked) > 0 {
		gaps = append(gaps, fmt.Sprintf("The proposal is recorded as %s yet %d task(s) are linked; the saved records are inconsistent.", proposal.Decision, len(linked)))
	}
	slots := model.ReviewerSlots()
	verdicts := make([]ReviewerVerdict, 0, len(slots))
	for slot := range slots {
		verdicts = append(verdicts, verdict(batches, slot, proposal.ID))
	}
	linkedTasks := make([]TaskEvidence, 0, len(linked))
	for _, t := range linked {
		linkedTasks = append(linkedTasks, taskEvidence(t))
	}
	return ProposalEvidence{
		ID:               proposal.ID,
		Title:            proposal.Title,
		Target:           proposal.Target,
		Tier:             proposal.Tier,
		Category:         proposal.Category,
		Problem:          proposal.Problem,
		Benefit:          proposal.Benefit,
		Scope:            proposal.Scope,
		Evidence:         append([]string{}, proposal.Evidence...),
		FinalDecision:    proposal.Decision,
		FinalReason:      proposal.Reason,
		ReviewerVerdicts: verdicts,
		LinkedTasks:      linkedTasks,
		Gaps:             gaps,
	}
}

func assemble(cycle model.Cycle, tasks []model.Task) RunEvidenceV1 {
	batches, gaps := normalizeBatches(cycle)
	execution := cycle.Mode == model.CycleModeExecution
	decisions := map[string]int{}
	tasksByProposal := make(map[string][]model.Task, len(cycle.Proposals))
	for _, p := range cycle.Proposals {
		decisions[p.Decision]++
		tasksByProposal[p.ID] = nil
	}
	foreign, unmatched := 0, 0
	for _, t := range tasks {
		if t.CycleID != cycle.ID {
			foreign++
		} else if _, known := tasksByProposal[t.Proposal.ID]; !known {
			unmatched++
		} else {
			tasksByProposal[t.Proposal.ID] = append(tasksByProposal[t.Proposal.ID], t)
		}
	}
	if foreign > 0 {
		gaps = append(gaps, fmt.Sprintf("%d saved task records name a different cycle and are excluded from this run.", foreign))
	}
	if unmatched > 0 {
		gaps = append(gaps, fmt.Sprintf("%d task records in this cycle have no matching saved proposal identity.", unmatched))
	}
	proposals := make([]ProposalEvidence, 0, len(cycle.Proposals))
	for _, p := range cycle.Proposals {
		proposals = append(proposals, proposalEvidence(p, tasksByProposal[p.ID], batches, execution))
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
		groundingRevision = new(cycle.Grounding.Revision)
	}
	return RunEvidenceV1{
		SchemaVersion:     SchemaVersion,
		GeneratedAt:       model.Now(),
		Kind:              "recorded_review_check_evidence",
		ReviewRequired:    true,
		ReviewRequirement: reviewRequirement,
		Limitations:       limitations,
		Cycle: CycleEvidence{
			ID:                cycle.ID,
			Number:            cycle.Number,
			Mode:              cycle.Mode,
			DeliveryMode:      cycle.DeliveryMode,
			Status:            cycle.Status,
			StartedAt:         cycle.StartedAt,
			CompletedAt:       wirejson.Clone(cycle.CompletedAt),
			Repository:        cycle.Repository,
			GroundingRevision: groundingRevision,
			Planning: PlanningOutcome{
				Status:                cycle.Status.String(),
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

const cycleTasksQuery = "SELECT r.data FROM record_meta m INDEXED BY meta_cycle JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.cycle_id=?1 ORDER BY r.id"

// runRecords reads one cycle and its tasks from the snapshot c. A missing cycle
// is a nil cycle, not an error.
func runRecords(c *sql.Conn, cycleID string) (*model.Cycle, []model.Task, error) {
	cycle, err := store.RecordAt[model.Cycle](c, "cycle", cycleID)
	if err != nil || cycle == nil {
		return nil, nil, err
	}
	tasks, err := store.QueryRecords[model.Task](c, cycleTasksQuery, cycleID)
	if err != nil {
		return nil, nil, err
	}
	return cycle, tasks, nil
}

// RunEvidence exports one cycle's run evidence from the live store, or nil
// when no such cycle is saved.
func RunEvidence(s *store.Store, cycleID string) (map[string]any, error) {
	var cycle *model.Cycle
	var tasks []model.Task
	var merges map[string]*model.AutoMergeState
	err := s.Snapshot(func(c *sql.Conn) (err error) {
		cycle, tasks, err = runRecords(c, cycleID)
		if err != nil {
			return err
		}
		merges, err = maintenanceEvidenceAt(c, tasks)
		return err
	})
	if err != nil || cycle == nil {
		return nil, err
	}
	return redacted(withMaintenanceEvidence(assemble(*cycle, tasks), merges))
}

// Run exports one cycle's run evidence from the state database at stateDB,
// opened read-only. A missing cycle is an error, never an empty export.
func Run(stateDB, cycleID string) (map[string]any, error) {
	return export(stateDB, "run evidence export", func(c *sql.Conn) (RunEvidenceV1, error) {
		cycle, tasks, err := runRecords(c, cycleID)
		if err != nil {
			return RunEvidenceV1{}, err
		}
		if cycle == nil {
			return RunEvidenceV1{}, fmt.Errorf("No saved cycle %s in this state database", cycleID)
		}
		merges, err := maintenanceEvidenceAt(c, tasks)
		if err != nil {
			return RunEvidenceV1{}, err
		}
		return withMaintenanceEvidence(assemble(*cycle, tasks), merges), nil
	})
}
