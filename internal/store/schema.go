package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// SupportedSchemaVersion identifies state created by the Go-only format.
const SupportedSchemaVersion = 7

//go:embed schema.sql
var schemaSQL string

const taskSummary = "json_object('id',NEW.id,'cycle_id',json_extract(NEW.data,'$.cycle_id'),'title',substr(json_extract(NEW.data,'$.proposal.title'),1,200),'category',json_extract(NEW.data,'$.proposal.category'),'tier',json_extract(NEW.data,'$.proposal.tier'),'target',json_extract(NEW.data,'$.proposal.target'),'branch',json_extract(NEW.data,'$.branch'),'status',json_extract(NEW.data,'$.status'),'pr_url',json_extract(NEW.data,'$.pr_url'),'pr_number',json_extract(NEW.data,'$.pr_number'),'error',substr(json_extract(NEW.data,'$.error'),1,512),'blocked_reason',json_extract(NEW.data,'$.blocked_reason'),'created_at',json_extract(NEW.data,'$.created_at'),'updated_at',json_extract(NEW.data,'$.updated_at'),'lifecycle',json(COALESCE(json_extract(NEW.data,'$.lifecycle'),'{}')),'superseded_by',json(COALESCE(json_extract(NEW.data,'$.superseded_by'),'[]')))"

// Keep the projection's decision counts aligned with the Go model vocabulary.
func cycleSummary() string {
	decisions := make([]string, 0, 4)
	for _, decision := range model.Decisions() {
		decisions = append(decisions, fmt.Sprintf("'%s',(SELECT count(*) FROM json_each(NEW.data,'$.proposals') WHERE json_extract(value,'$.decision')='%s')", decision, decision))
	}
	return fmt.Sprintf("json_object('id',NEW.id,'number',json_extract(NEW.data,'$.number'),'mode',COALESCE(json_extract(NEW.data,'$.mode'),'execution'),'status',json_extract(NEW.data,'$.status'),'started_at',json_extract(NEW.data,'$.started_at'),'completed_at',json_extract(NEW.data,'$.completed_at'),'error',substr(json_extract(NEW.data,'$.error'),1,512),'session_count',json_array_length(NEW.data,'$.sessions'),'decisions',json_object(%s),'lifecycle',json(COALESCE(json_extract(NEW.data,'$.lifecycle'),'{}')))", strings.Join(decisions, ","))
}

const prSummary = "json_set(json_remove(NEW.data,'$.pr.body'),'$.pr.title',substr(json_extract(NEW.data,'$.pr.title'),1,200))"

func projection() string {
	return fmt.Sprintf("INSERT INTO record_meta(kind,id,seq,status,repository,target,title,cycle_id,run_id,archived,discarded,summary) VALUES (NEW.kind,NEW.id,NEW.rowid,COALESCE(json_extract(NEW.data,'$.status'),json_extract(NEW.data,'$.pr.state'),''),COALESCE(json_extract(NEW.data,'$.config.github_repo'),json_extract(NEW.data,'$.repository'),''),COALESCE(json_extract(NEW.data,'$.proposal.target'),''),COALESCE(json_extract(NEW.data,'$.proposal.title'),json_extract(NEW.data,'$.pr.title'),''),COALESCE(json_extract(NEW.data,'$.cycle_id'),''),json_extract(NEW.data,'$.run_id'),json_extract(NEW.data,'$.lifecycle.archived_at'),json_extract(NEW.data,'$.lifecycle.discarded_at'),CASE NEW.kind WHEN 'task' THEN %s WHEN 'cycle' THEN %s WHEN 'pr' THEN %s ELSE '{}' END) ON CONFLICT(kind,id) DO UPDATE SET status=excluded.status,repository=excluded.repository,target=excluded.target,title=excluded.title,cycle_id=excluded.cycle_id,run_id=excluded.run_id,archived=excluded.archived,discarded=excluded.discarded,summary=excluded.summary;", taskSummary, cycleSummary(), prSummary)
}

// schemaStatus inspects an existing database before journal settings or DDL run.
// Only a database without user objects can be initialized as fresh state.
func schemaStatus(ctx context.Context, c *sql.Conn) (bool, error) {
	version, err := userVersion(ctx, c)
	if err != nil {
		return false, err
	}
	if version == SupportedSchemaVersion {
		return false, nil
	}
	if version == 0 {
		var objects int
		err = c.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&objects)
		if err != nil {
			return false, err
		}
		if objects == 0 {
			return true, nil
		}
	}
	return false, unsupportedSchema(version)
}

func requireSchema(ctx context.Context, c *sql.Conn) error {
	version, err := userVersion(ctx, c)
	if err != nil {
		return err
	}
	if version != SupportedSchemaVersion {
		return unsupportedSchema(version)
	}
	return nil
}

func userVersion(ctx context.Context, c *sql.Conn) (int64, error) {
	var version int64
	err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	return version, err
}

func unsupportedSchema(version int64) error {
	return fmt.Errorf("State database schema version %d is unsupported; this release requires a fresh version-%d data directory. Back up existing state before changing data directories", version, SupportedSchemaVersion)
}

func createSchema(ctx context.Context, c *sql.Conn) (err error) {
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = c.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if _, err = c.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	for _, name := range []string{"insert", "update"} {
		ddl := fmt.Sprintf("CREATE TRIGGER project_record_%s AFTER %s ON records WHEN NEW.kind IN ('task','cycle','pr') BEGIN %s END;", name, strings.ToUpper(name), projection())
		if _, err = c.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	if _, err = c.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", SupportedSchemaVersion)); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}
