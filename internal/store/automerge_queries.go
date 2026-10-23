package store

const mergeCandidatesQuery = `SELECT m.seq,r.data
    FROM ` + fromMeta + `
    WHERE m.kind='pr' AND m.seq>?1 AND (?3='' OR m.repository=?3 COLLATE NOCASE) AND (
        json_extract(r.data,'$.auto_merge.status') IN ('waiting','merging','uncertain')
        OR (json_extract(r.data,'$.auto_merge.status')='manual'
            AND json_extract(r.data,'$.auto_merge.authorized')=1)
    )
    ORDER BY m.seq ASC LIMIT ?2`

const batchMergeWaitQuery = `SELECT count(*)
    FROM ` + fromMeta + `
    JOIN batch_members b ON b.task_id=json_extract(r.data,'$.auto_merge.task_id')
    JOIN records t ON t.kind='task' AND t.id=b.task_id
    WHERE m.kind='pr' AND b.run_id=?1
        AND json_extract(t.data,'$.run_id')=?1
        AND m.repository=json_extract(t.data,'$.config.github_repo') COLLATE NOCASE
        AND json_extract(r.data,'$.pr.number')=json_extract(t.data,'$.pr_number')
        AND json_extract(r.data,'$.auto_merge.status') IN ('waiting','merging','uncertain')`

const unfinishedBranchWorkQuery = `SELECT EXISTS(
    SELECT 1 FROM ` + fromMeta + `
    WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.id!=?3
        AND m.archived IS NULL AND json_extract(r.data,'$.branch')=?2
        AND m.status IN ('queued','executing','reviewing','repairing','verifying',
            'publishing','blocked','failed')
)`

const mergeCountsQuery = `SELECT json_extract(r.data,'$.auto_merge.status'),count(*)
    FROM ` + fromMeta + `
    WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE
        AND json_extract(r.data,'$.auto_merge.status') IS NOT NULL
    GROUP BY 1`

const revokeTaskMergesQuery = `SELECT m.id,r.data
    FROM ` + fromMeta + `
    WHERE m.kind='pr' AND json_extract(r.data,'$.auto_merge.task_id')=?1
        AND (json_extract(r.data,'$.auto_merge.status') IN ('waiting','merging','uncertain')
            OR json_extract(r.data,'$.auto_merge.authorized')=1)`
