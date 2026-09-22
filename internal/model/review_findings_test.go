package model

import "testing"

// TestRepairRoundsCountFromTheAttemptBaseline ports
// repair_rounds_count_from_the_attempt_baseline (tests/review_findings.rs, F2):
// the repair budget counts only reviews recorded after the attempt baseline.
func TestRepairRoundsCountFromTheAttemptBaseline(t *testing.T) {
	round := ReviewRound{
		SessionID:      "s",
		Revision:       "r",
		ComparisonBase: "c",
		Result:         Review{Completed: true, Summary: "clean", Findings: []Finding{}},
		CreatedAt:      Now(),
	}
	task := Task{Reviews: []ReviewRound{round, round}}
	const maxRepairRounds = uint64(1)
	if task.AttemptReviews() <= maxRepairRounds {
		t.Fatalf("attempt reviews = %d; the exhausted first attempt is still exhausted", task.AttemptReviews())
	}
	task.ReviewBaseline = uint64(len(task.Reviews))
	if task.AttemptReviews() != 0 {
		t.Fatalf("attempt reviews after baseline = %d; want 0", task.AttemptReviews())
	}
	if task.AttemptReviews() >= maxRepairRounds+1 {
		t.Fatalf("attempt reviews after baseline = %d; want a fresh budget", task.AttemptReviews())
	}
}
