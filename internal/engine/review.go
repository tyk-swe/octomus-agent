package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

func (a *App) reviewRevision(ctx context.Context, task *model.Task, client *runner.Runners, revision string) (model.Review, error) {
	cfg := task.ExecutionConfig()
	ws := task.Workspace
	if err := a.transition(task, model.StatusReviewing); err != nil {
		return model.Review{}, err
	}
	route, ok := cfg.Roles["code_reviewer"]
	if !ok {
		return model.Review{}, errors.New("code_reviewer route is missing")
	}
	trusted, err := trustedChangeSet(ctx, cfg, ws, task.ComparisonBase, revision)
	if err != nil {
		return model.Review{}, err
	}
	maintenance := cfg.DeliveryMode == config.DeliveryModeMaintenance
	schema := schemas.ReviewSchema()
	if maintenance {
		schema = schemas.MaintenanceReviewSchema()
	}
	var review model.Review
	var assessment *model.MaintenanceAssessment
	judge := func(thread, answer string) (string, error) {
		if maintenance {
			var document model.MaintenanceReviewDocument
			if err := json.Unmarshal([]byte(answer), &document); err != nil {
				return "", fmt.Errorf("%w: Unparseable maintenance review is not clean: %s", model.BlockedInvalidReview, redact.Text(err.Error()))
			}
			review = document.Review
			if strings.TrimSpace(document.Maintenance.Reason) == "" {
				return "", fmt.Errorf("%w: Maintenance assessment needs a reason", model.BlockedInvalidReview)
			}
			assessment = &document.Maintenance
		} else if err := json.Unmarshal([]byte(answer), &review); err != nil {
			return "", fmt.Errorf("%w: Unparseable review is not clean: %s", model.BlockedInvalidReview, redact.Text(err.Error()))
		}
		if !review.Valid() {
			return "", model.BlockedInvalidReview
		}
		if err := ensureWorkspaceAt(ctx, cfg, ws, revision); err != nil {
			return "", err
		}
		task.Reviews = append(task.Reviews, model.ReviewRound{SessionID: thread, Revision: revision, ComparisonBase: task.ComparisonBase, Result: review, CreatedAt: model.Now(), Maintenance: assessment, TrustedDiffComplete: len(trusted.omitted) == 0})
		return review.Summary, nil
	}
	if _, err := a.invoke(ctx, client, invocation{
		cycleID: task.CycleID, task: task, role: "reviewer", route: route, workspace: ws,
		prompt: reviewPrompt(task, revision, trusted), schema: schema, judge: judge,
	}); err != nil {
		return model.Review{}, err
	}
	return review, nil
}

// The review prompt carries the trusted change set within these bounds: every changed file listed, or no review at
// all, and as many whole per-file diffs as fit, the smallest first.
const (
	reviewListLimit = 32 << 10
	reviewDiffLimit = 64 << 10
)

// changeSet is a revision's change set as the orchestrator's trusted git shows it: the totals, every changed file with
// its line counts and its creation, deletion and mode changes, the diffs that fit in reviewDiffLimit, and the files,
// as listed, whose diffs do not. The diff keeps its final newline, so a CR that ends it still precedes one.
type changeSet struct {
	totals, files, diff string
	omitted             []string
}

// changedFile is a line of git diff --numstat: the file as listed, the path git names, and its added plus deleted
// lines, or math.MaxInt for a file git counts as binary and so gives no count.
type changedFile struct {
	listed, path string
	lines        int
}

var numstatLine = regexp.MustCompile(`^(\d+|-)\t(\d+|-)\t(.+)$`)

// trustedChangeSet reads the change set from base to revision with the orchestrator's git against the trusted
// metadata, as text whatever attributes a sandbox left in the work tree and whatever bytes the files hold. The fresh
// reviewer's own git runs in a sandbox whose home and runner configuration earlier turns of the task could change,
// so it must not be the only account of what changed. Every changed file is listed, or the review is refused: a file
// the account leaves out entirely could be hidden by an altered git in the sandbox. Past the diff budget, the whole
// diffs of the smallest files are embedded and the rest named, so the reviewer knows which content Octomus did not show.
func trustedChangeSet(ctx context.Context, cfg config.Config, ws, base, revision string) (changeSet, error) {
	diff := func(limit int, args ...string) (string, bool, error) {
		return gitops.DiffText(ctx, cfg, ws, append(args, base, revision), limit)
	}
	totals, counted, err := diff(reviewListLimit, "--shortstat")
	if err != nil {
		return changeSet{}, err
	}
	files, listed, err := diff(reviewListLimit-len(totals), "--numstat", "--summary")
	if err != nil {
		return changeSet{}, err
	}
	if !counted || !listed {
		return changeSet{}, fmt.Errorf("%w: The change set from %s to %s lists more files than a review prompt carries (over %d bytes); narrow the task",
			model.BlockedInvalidReview, base, revision, reviewListLimit)
	}
	set := changeSet{totals: strings.TrimSpace(totals), files: strings.TrimRight(files, "\n")}
	changed, err := changedFiles(set.files)
	if err != nil {
		return changeSet{}, err
	}
	if len(changed) == 0 {
		return set, nil
	}
	whole, fits, err := diff(reviewDiffLimit)
	if err != nil {
		return changeSet{}, err
	}
	if fits {
		set.diff = whole
		return set, nil
	}
	smallest := make([]int, len(changed))
	for i := range smallest {
		smallest[i] = i
	}
	slices.SortStableFunc(smallest, func(a, b int) int { return cmp.Compare(changed[a].lines, changed[b].lines) })
	shown := make([]string, len(changed))
	remaining := reviewDiffLimit
	for _, i := range smallest {
		text, complete, err := gitops.DiffText(ctx, cfg, ws, []string{base, revision, "--", ":(literal)" + changed[i].path}, remaining)
		if err != nil {
			return changeSet{}, err
		}
		if !complete {
			// Line count does not predict byte size: later files can still fit.
			continue
		}
		if text == "" {
			return changeSet{}, fmt.Errorf("Git shows no diff for the changed file %s", changed[i].listed)
		}
		shown[i] = text
		remaining -= len(text)
	}
	var diffs strings.Builder
	for i, file := range changed {
		if shown[i] == "" {
			set.omitted = append(set.omitted, file.listed)
		}
		diffs.WriteString(shown[i])
	}
	set.diff = diffs.String()
	return set, nil
}

// changedFiles reads the --numstat lines of a numstat and summary listing. Paths are C-quoted, as core.quotePath
// has git write them, when they hold anything but printable ASCII.
func changedFiles(listing string) ([]changedFile, error) {
	var changed []changedFile
	for line := range strings.SplitSeq(listing, "\n") {
		if line == "" || strings.HasPrefix(line, " ") {
			continue
		}
		fields := numstatLine.FindStringSubmatch(line)
		if fields == nil {
			return nil, fmt.Errorf("Unexpected git numstat line %q", line)
		}
		file := changedFile{listed: fields[3], path: fields[3], lines: math.MaxInt}
		if strings.HasPrefix(file.path, `"`) {
			path, err := strconv.Unquote(file.path)
			if err != nil {
				return nil, fmt.Errorf("Unexpected git path %s: %w", file.path, err)
			}
			file.path = path
		}
		if added, err := strconv.Atoi(fields[1]); err == nil {
			deleted, _ := strconv.Atoi(fields[2])
			file.lines = added + deleted
		}
		changed = append(changed, file)
	}
	return changed, nil
}

func reviewPrompt(task *model.Task, revision string, trusted changeSet) string {
	base := task.ComparisonBase
	prompt := fmt.Sprintf(
		"Perform a fresh code review equivalent to /review of the COMPLETE change set: git diff %s HEAD. Recorded HEAD: %s. Include all accumulated PR changes and all repairs; do not only review the last commit. Task: %s. Scope: %s. Existing PR: %s. Inspect code and evidence, do not modify files. Report actionable correctness, regression, design or missing verification findings with file, priority and technical rationale. Do not invent findings. Set completed=true only after completing the review. A clean review must have an explanatory summary and zero findings.",
		base, revision, task.Proposal.Prompt, task.Proposal.Scope, quoteOption(task.PRURL))
	prompt += maintenanceReviewPolicy(task.Config)
	prompt += fmt.Sprintf("\nThe orchestrator's own git computed the change set from %s to %s below. ", base, revision) +
		"Git inside your sandbox reads configuration and shell startup files earlier turns could change, " +
		"so where it shows other changes or other content, what follows is authoritative and the difference is itself a finding. " +
		"Some differences are expected and are not findings by themselves: this account ignores every .gitattributes file and shows every file as text, " +
		"so git may show a file as binary, count its lines differently or give other hunk headers; it shows a rename as a deletion and an addition, " +
		"and a submodule entry as the commits it points at. Each byte that is not UTF-8 shows as ⟦xNN⟧, and each control, invisible or line-separator character, " +
		"a literal ⟦ included, as ⟦U+XXXX⟧. Lines end only at real newlines: an escape such as ⟦U+000D⟧ or ⟦U+2028⟧ inside a line is a character the file holds, " +
		"which some languages and tools read as a line break.\n"
	if trusted.files == "" {
		return prompt + fmt.Sprintf("The orchestrator's git shows no change between %s and %s.", base, revision)
	}
	prompt += "Totals: " + trusted.totals + "\n" +
		"Changed files (git diff --numstat --summary: lines added, lines deleted and path, - for a file git counts as binary):\n" + trusted.files + "\n"
	if len(trusted.omitted) == 0 {
		return prompt + "Complete diff:\n" + trusted.diff
	}
	if trusted.diff != "" {
		prompt += fmt.Sprintf("Complete diffs of the smallest files, within %d bytes:\n%s", reviewDiffLimit, trusted.diff)
	}
	return prompt + fmt.Sprintf("The diffs of these files do not fit, so the orchestrator has not shown you their content: read each with git diff %s HEAD -- <file>, "+
		"check it against the line counts above and treat it as unverified:\n%s", base, strings.Join(trusted.omitted, "\n"))
}
