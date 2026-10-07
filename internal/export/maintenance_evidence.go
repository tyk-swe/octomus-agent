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
		if len(observations) == 0 {
			continue
		}
		observation := observations[0]
		merge := observation.AutoMerge
		if merge == nil || merge.TaskID != task.ID || merge.Head != *task.OutputCommit ||
			merge.ComparisonBase != task.ComparisonBase || merge.HeadBranch != task.Branch ||
			merge.BaseBranch != task.Config.DefaultBranch {
			continue
		}
		if merge.Status == model.AutoMergeMerged && observation.PR.Head != merge.Head {
			continue
		}
		captured := merge.Clone()
		saved[task.ID] = &captured
	}
	return saved, nil
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
