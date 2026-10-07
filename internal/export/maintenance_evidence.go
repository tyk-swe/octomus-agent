package export

import (
	"database/sql"
	"fmt"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const maintenanceEvidenceQuery = `SELECT r.data
    FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id
    WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE
        AND json_extract(r.data,'$.pr.number')=?2
    ORDER BY m.seq ASC LIMIT 2`

func maintenanceEvidenceAt(c *sql.Conn, tasks []model.Task) (map[string]*model.AutoMergeState, error) {
	saved := map[string]*model.AutoMergeState{}
	for _, task := range tasks {
		if task.Config.DeliveryMode != config.DeliveryModeMaintenance ||
			task.PRNumber == nil || task.OutputCommit == nil {
			continue
		}
		observations, err := store.QueryRecords[model.PRObservation](c, maintenanceEvidenceQuery,
			task.Config.GitHubRepo, *task.PRNumber)
		if err != nil {
			return nil, err
		}
		if len(observations) > 1 {
			return nil, fmt.Errorf("Multiple saved observations identify maintenance pull request %d", *task.PRNumber)
		}
		var merge *model.AutoMergeState
		if snapshot := task.AutoMergeSnapshot; snapshot != nil &&
			config.EqualASCII(snapshot.Repository, task.Config.GitHubRepo) &&
			snapshot.PRNumber == *task.PRNumber &&
			maintenanceStateMatchesTask(&snapshot.State, task) {
			merge = &snapshot.State
		}
		if len(observations) == 1 {
			observation := observations[0]
			current := observation.AutoMerge
			if maintenanceStateMatchesTask(current, task) &&
				(current.Status != model.AutoMergeMerged || observation.PR.Head == current.Head) {
				merge = current
			}
		}
		if merge == nil {
			continue
		}
		captured := merge.Clone()
		saved[task.ID] = &captured
	}
	return saved, nil
}

func maintenanceStateMatchesTask(merge *model.AutoMergeState, task model.Task) bool {
	return merge != nil && task.OutputCommit != nil &&
		merge.TaskID == task.ID && merge.Head == *task.OutputCommit &&
		merge.ComparisonBase == task.ComparisonBase &&
		merge.HeadBranch == task.Branch && merge.BaseBranch == task.Config.DefaultBranch
}

func withMaintenanceEvidence(run RunEvidenceV1, saved map[string]*model.AutoMergeState) RunEvidenceV1 {
	for i := range run.Proposals {
		for j := range run.Proposals[i].LinkedTasks {
			task := &run.Proposals[i].LinkedTasks[j]
			task.AutoMerge = saved[task.ID]
		}
	}
	return run
}
