package store

const maintenanceCandidatesQuery = `SELECT m.seq,r.data
    FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id
    WHERE m.kind='pr' AND m.seq>?1 AND (
        json_extract(r.data,'$.auto_merge.status') IN ('waiting','merging','uncertain')
        OR (json_extract(r.data,'$.auto_merge.status')='manual'
            AND json_extract(r.data,'$.auto_merge.authorized')=1)
    )
    ORDER BY m.seq ASC LIMIT ?2`

const maintenanceBatchWaitQuery = `SELECT count(*)
    FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id
    JOIN batch_members b ON b.task_id=json_extract(r.data,'$.auto_merge.task_id')
    JOIN records t ON t.kind='task' AND t.id=b.task_id
    WHERE m.kind='pr' AND b.run_id=?1
        AND json_extract(t.data,'$.run_id')=?1
        AND m.repository=json_extract(t.data,'$.config.github_repo') COLLATE NOCASE
        AND json_extract(r.data,'$.pr.number')=json_extract(t.data,'$.pr_number')
        AND json_extract(r.data,'$.auto_merge.status') IN ('waiting','merging','uncertain')`

const maintenanceUnfinishedBranchQuery = `SELECT EXISTS(
    SELECT 1 FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id
    WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.id!=?3
        AND m.archived IS NULL AND json_extract(r.data,'$.branch')=?2
        AND m.status IN ('queued','executing','reviewing','repairing','verifying',
            'publishing','blocked','failed','cancelled')
)`

const maintenanceCountsQuery = `SELECT json_extract(r.data,'$.auto_merge.status'),count(*)
    FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id
    WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE
        AND json_extract(r.data,'$.auto_merge.status') IS NOT NULL
    GROUP BY 1`

const maintenanceRevokeQuery = `SELECT m.id,r.data
    FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id
    WHERE m.kind='pr' AND json_extract(r.data,'$.auto_merge.task_id')=?1
        AND (json_extract(r.data,'$.auto_merge.status') IN ('waiting','merging','uncertain')
            OR json_extract(r.data,'$.auto_merge.authorized')=1)`
