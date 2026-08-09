package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
	_ "modernc.org/sqlite"
)

type SQLite struct {
	db *sql.DB
}

func NewSQLite(ctx context.Context, dsn string, bootstrapAdminToken string) (*SQLite, Initialization, error) {
	path, err := sqlitePath(dsn)
	if err != nil {
		return nil, Initialization{}, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, Initialization{}, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `pragma journal_mode = wal; pragma busy_timeout = 5000; pragma foreign_keys = on;`); err != nil {
		_ = db.Close()
		return nil, Initialization{}, err
	}
	initialization, err := migrateSQLite(ctx, db, bootstrapAdminToken)
	if err != nil {
		_ = db.Close()
		return nil, Initialization{}, err
	}
	return &SQLite{db: db}, initialization, nil
}

func (s *SQLite) Close() {
	_ = s.db.Close()
}

func (s *SQLite) CreateJob(ctx context.Context, j model.Job) error {
	return insertSQLiteJob(ctx, s.db, j)
}

func (s *SQLite) CreateJobs(ctx context.Context, jobs []model.Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	for _, job := range jobs {
		if err := insertSQLiteJob(ctx, tx, job); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type sqliteJobInserter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertSQLiteJob(ctx context.Context, execer sqliteJobInserter, j model.Job) error {
	args, err := marshalJSONString(j.Args)
	if err != nil {
		return err
	}
	_, err = execer.ExecContext(ctx, `
		insert into jobs (id, parent_id, scheduled_id, scheduled_revision, tool, target, resolved_target, args, ip_version, agent_id, resolve_on_agent, status, created_at, updated_at, started_at, completed_at, error)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, nullString(j.ParentID), nullString(j.ScheduledID), j.ScheduledRevision, j.Tool, j.Target, nullString(j.ResolvedTarget), args, j.IPVersion, nullString(j.AgentID), j.ResolveOnAgent, j.Status, j.CreatedAt, j.UpdatedAt, j.StartedAt, j.CompletedAt, nullString(j.Error))
	return err
}

func (s *SQLite) GetJob(ctx context.Context, id string) (model.Job, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs where id=?`, id)
	return scanSQLiteJob(row)
}

func (s *SQLite) ListChildJobs(ctx context.Context, parentID string) ([]model.Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs where parent_id=?
		order by created_at asc`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanSQLiteJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *SQLite) ListActiveJobs(ctx context.Context, limit int) ([]model.Job, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs
		where status in (?,?)
		order by created_at asc
		limit ?`, model.JobQueued, model.JobRunning, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanSQLiteJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *SQLite) ListQueuedJobs(ctx context.Context, agentID string, caps []model.Tool, protocols model.ProtocolMask, limit int) ([]model.Job, error) {
	if limit <= 0 {
		limit = 1
	}
	placeholders := make([]string, 0, len(caps))
	args := []any{model.JobQueued, agentID}
	for _, cap := range caps {
		placeholders = append(placeholders, "?")
		args = append(args, string(cap))
	}
	if len(placeholders) == 0 {
		return nil, nil
	}
	args = append(args, protocols, protocols, limit)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs
		where status=? and agent_id=? and tool in (%s)
		  and (ip_version=0 or (ip_version=4 and (? & 1) <> 0) or (ip_version=6 and (? & 2) <> 0))
		order by created_at asc
		limit ?`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanSQLiteJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *SQLite) ClaimQueuedJob(ctx context.Context, id string, agentID string) (bool, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		update jobs
		set status=?,
		    updated_at=?,
		    started_at=case when started_at is null then ? else started_at end
		where id=? and agent_id=? and status=?`,
		model.JobRunning, now, now, id, agentID, model.JobQueued)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *SQLite) UpdateJobStatus(ctx context.Context, id string, status model.JobStatus, msg string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		update jobs
		set status=?,
		    updated_at=?,
		    started_at=case when ?='running' and started_at is null then ? else started_at end,
		    completed_at=case when ? in ('succeeded','failed','canceled') then ? else completed_at end,
		    error=case when ?='' then error else ? end
		where id=?`, status, now, status, now, status, now, msg, msg, id)
	return err
}

func (s *SQLite) RequeueRunningJob(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		update jobs
		set status=?, updated_at=?, started_at=null, completed_at=null, error=''
		where id=? and status=?`, model.JobQueued, time.Now().UTC(), id, model.JobRunning)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (s *SQLite) FinishRunningJob(ctx context.Context, id string, status model.JobStatus, msg string) (bool, error) {
	return s.transitionJob(ctx, id, status, msg, "status='running'")
}

func (s *SQLite) FailActiveJob(ctx context.Context, id string, msg string) (bool, error) {
	return s.transitionJob(ctx, id, model.JobFailed, msg, "status in ('queued','running')")
}

func (s *SQLite) transitionJob(ctx context.Context, id string, status model.JobStatus, msg string, predicate string) (bool, error) {
	query := `update jobs set status=?, updated_at=?, completed_at=?, error=? where id=? and ` + predicate
	result, err := s.db.ExecContext(ctx, query, status, time.Now().UTC(), time.Now().UTC(), msg, id)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (s *SQLite) AddJobEvent(ctx context.Context, e model.JobEvent) error {
	event, parsed, err := sqliteJobEventJSON(e)
	if err != nil {
		return err
	}
	return insertSQLiteJobEvent(ctx, s.db, e, event, parsed)
}

func (s *SQLite) AddRunningJobEvent(ctx context.Context, sourceJobID string, agentID string, e model.JobEvent) (bool, error) {
	e.AgentID = agentID
	event, parsed, err := sqliteJobEventJSON(e)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
		insert into job_events (id, job_id, agent_id, stream, event, exit_code, parsed, created_at)
		select ?,?,?,?,?,?,?,?
		where exists (
			select 1 from jobs where id=? and status=? and agent_id=?
		)`,
		e.ID, e.JobID, nullString(e.AgentID), e.Stream, event, e.ExitCode, parsed, e.CreatedAt,
		sourceJobID, model.JobRunning, agentID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (s *SQLite) FinishRunningJobWithEvent(ctx context.Context, sourceJobID string, agentID string, status model.JobStatus, msg string, e model.JobEvent) (bool, error) {
	e.AgentID = agentID
	event, parsed, err := sqliteJobEventJSON(e)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		update jobs set status=?, updated_at=?, completed_at=?, error=?
		where id=? and status=? and agent_id=?`,
		status, now, now, msg, sourceJobID, model.JobRunning, agentID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil
	}
	if err := insertSQLiteJobEvent(ctx, tx, e, event, parsed); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func sqliteJobEventJSON(e model.JobEvent) (sql.NullString, sql.NullString, error) {
	var event sql.NullString
	if e.Event != nil {
		b, err := marshalJSONString(e.Event)
		if err != nil {
			return sql.NullString{}, sql.NullString{}, err
		}
		event = sql.NullString{String: b, Valid: true}
	}
	var parsed sql.NullString
	if e.Parsed != nil {
		b, err := marshalJSONString(e.Parsed)
		if err != nil {
			return sql.NullString{}, sql.NullString{}, err
		}
		parsed = sql.NullString{String: b, Valid: true}
	}
	return event, parsed, nil
}

func insertSQLiteJobEvent(ctx context.Context, execer sqliteJobInserter, e model.JobEvent, event sql.NullString, parsed sql.NullString) error {
	_, err := execer.ExecContext(ctx, `
		insert into job_events (id, job_id, agent_id, stream, event, exit_code, parsed, created_at)
		values (?,?,?,?,?,?,?,?)`,
		e.ID, e.JobID, nullString(e.AgentID), e.Stream, event, e.ExitCode, parsed, e.CreatedAt)
	return err
}

func (s *SQLite) ListJobEvents(ctx context.Context, jobID string) ([]model.JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, job_id, coalesce(agent_id,''), stream, event, exit_code, parsed, created_at
		from job_events where job_id=? order by created_at asc`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []model.JobEvent
	for rows.Next() {
		var e model.JobEvent
		var exitCode sql.NullInt64
		var event sql.NullString
		var parsed sql.NullString
		if err := rows.Scan(&e.ID, &e.JobID, &e.AgentID, &e.Stream, &event, &exitCode, &parsed, &e.CreatedAt); err != nil {
			return nil, err
		}
		if event.Valid {
			var streamEvent model.StreamEvent
			if err := unmarshalJSONString(event.String, &streamEvent); err != nil {
				return nil, err
			}
			if streamEvent.Type != "" {
				e.Event = &streamEvent
			}
		}
		if exitCode.Valid {
			v := int(exitCode.Int64)
			e.ExitCode = &v
		}
		if parsed.Valid {
			var result model.ToolResult
			if err := unmarshalJSONString(parsed.String, &result); err != nil {
				return nil, err
			}
			e.Parsed = &result
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *SQLite) CreateScheduledJob(ctx context.Context, sched model.ScheduledJob) error {
	args, err := marshalJSONString(sched.Args)
	if err != nil {
		return err
	}
	targets, err := marshalJSONString(sched.ScheduleTargets)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		insert into scheduled_jobs (id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sched.ID, sched.Revision, sched.Name, sched.Enabled, sched.Tool, sched.Target, args, sched.IPVersion,
		sched.ResolveOnAgent, sched.CronExpression, sched.NextRunAt, sched.LastRunAt, targets, sched.CreatedAt, sched.UpdatedAt)
	return err
}

func (s *SQLite) GetScheduledJob(ctx context.Context, id string) (model.ScheduledJob, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at
		from scheduled_jobs where id=?`, id)
	return scanSQLiteSchedule(row)
}

func (s *SQLite) ListScheduledJobs(ctx context.Context) ([]model.ScheduledJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at
		from scheduled_jobs order by created_at asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []model.ScheduledJob
	for rows.Next() {
		sched, err := scanSQLiteSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, sched)
	}
	return schedules, rows.Err()
}

func (s *SQLite) ListDueScheduledJobs(ctx context.Context, now time.Time, limit int) ([]model.ScheduledJob, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		select id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at
		from scheduled_jobs
		where enabled=1 and next_run_at <= ?
		order by next_run_at asc
		limit ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []model.ScheduledJob
	for rows.Next() {
		sched, err := scanSQLiteSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, sched)
	}
	return schedules, rows.Err()
}

func (s *SQLite) UpdateScheduledJob(ctx context.Context, sched model.ScheduledJob) error {
	args, err := marshalJSONString(sched.Args)
	if err != nil {
		return err
	}
	targets, err := marshalJSONString(sched.ScheduleTargets)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		update scheduled_jobs
		set revision=?,
		    name=?,
		    enabled=?,
		    tool=?,
		    target=?,
		    args=?,
		    ip_version=?,
		    resolve_on_agent=?,
		    cron_expression=?,
		    next_run_at=?,
		    last_run_at=?,
		    schedule_targets=?,
		    updated_at=?
		where id=?`,
		sched.Revision, sched.Name, sched.Enabled, sched.Tool, sched.Target, args, sched.IPVersion,
		sched.ResolveOnAgent, sched.CronExpression, sched.NextRunAt, sched.LastRunAt, targets, sched.UpdatedAt, sched.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) DeleteScheduledJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `delete from scheduled_jobs where id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) UpdateScheduledJobRun(ctx context.Context, sched model.ScheduledJob) error {
	_, err := s.db.ExecContext(ctx, `
		update scheduled_jobs set last_run_at=?, next_run_at=?, updated_at=? where id=?`,
		sched.LastRunAt, sched.NextRunAt, time.Now().UTC(), sched.ID)
	return err
}

func (s *SQLite) ListScheduledJobHistory(ctx context.Context, scheduleID string, filter ScheduledJobHistoryFilter) ([]model.Job, error) {
	selectClause := `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs`
	query := selectClause + ` where scheduled_id=?`
	args := []any{scheduleID}
	if filter.Revision > 0 {
		query += ` and scheduled_revision=?`
		args = append(args, filter.Revision)
	}
	if filter.HasFrom {
		query += ` and coalesce(started_at, created_at) >= ?`
		args = append(args, filter.From)
	}
	if filter.HasTo {
		query += ` and coalesce(started_at, created_at) <= ?`
		args = append(args, filter.To)
	}
	if filter.BucketSeconds > 0 {
		query = `
			select id, parent_id, scheduled_id, scheduled_revision, tool, target, resolved_target, args, ip_version, agent_id, resolve_on_agent, status, created_at, updated_at, started_at, completed_at, error
			from (
				select id, coalesce(parent_id,'') parent_id, coalesce(scheduled_id,'') scheduled_id, scheduled_revision, tool, target, coalesce(resolved_target,'') resolved_target, args, ip_version, coalesce(agent_id,'') agent_id, resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'') error,
					row_number() over (partition by coalesce(agent_id,''), cast(unixepoch(substr(coalesce(started_at, created_at), 1, 19)) / ? as integer) order by coalesce(started_at, created_at) desc) rn
				from jobs where scheduled_id=?`
		args = []any{filter.BucketSeconds, scheduleID}
		if filter.Revision > 0 {
			query += ` and scheduled_revision=?`
			args = append(args, filter.Revision)
		}
		if filter.HasFrom {
			query += ` and coalesce(started_at, created_at) >= ?`
			args = append(args, filter.From)
		}
		if filter.HasTo {
			query += ` and coalesce(started_at, created_at) <= ?`
			args = append(args, filter.To)
		}
		query += `
			) where rn=1`
	}
	query += ` order by coalesce(started_at, created_at) desc limit ?`
	args = append(args, normalizedHistoryLimit(filter.Limit))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanSQLiteJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *SQLite) RegisterAgent(ctx context.Context, a model.Agent, suppliedToken string) (model.Agent, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Agent{}, false, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	settings := config.DefaultManagedSettings()
	var raw string
	var updated time.Time
	if err := tx.QueryRowContext(ctx, `select value, revision, updated_at from managed_settings where id='default'`).Scan(&raw, &settings.Revision, &updated); err != nil {
		return model.Agent{}, false, err
	}
	if err := unmarshalJSONString(raw, &settings); err != nil {
		return model.Agent{}, false, err
	}
	settings.UpdatedAt = updated.Format(time.RFC3339Nano)
	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		return model.Agent{}, false, err
	}

	var current *model.Agent
	stored, err := scanSQLiteStoredAgent(tx.QueryRowContext(ctx, `
		select id, country, region, provider, isp, coalesce(version,''), token_hash, capabilities, protocols, status, last_seen_at, created_at
		from agents where id=?`, a.ID))
	if err == nil {
		current = &stored
	} else if !errors.Is(err, ErrNotFound) {
		return model.Agent{}, false, err
	}
	registered, created, err := prepareAgentRegistration(&settings, current, a, suppliedToken)
	if err != nil {
		return model.Agent{}, false, err
	}
	if created {
		currentRevision := settings.Revision
		settings.Revision++
		now := time.Now().UTC()
		settings.UpdatedAt = now.Format(time.RFC3339Nano)
		raw, err := marshalJSONString(settings)
		if err != nil {
			return model.Agent{}, false, err
		}
		res, err := tx.ExecContext(ctx, `
			update managed_settings set value=?, revision=revision+1, updated_at=?
			where id='default' and revision=?`, raw, now, currentRevision)
		if err != nil {
			return model.Agent{}, false, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return model.Agent{}, false, err
		}
		if rows == 0 {
			return model.Agent{}, false, ErrConflict
		}
	}
	if err := upsertSQLiteAgent(ctx, tx, registered); err != nil {
		return model.Agent{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Agent{}, false, err
	}
	return registered, created, nil
}

func (s *SQLite) UpsertAgent(ctx context.Context, a model.Agent) error {
	return upsertSQLiteAgent(ctx, s.db, a)
}

func upsertSQLiteAgent(ctx context.Context, execer sqliteJobInserter, a model.Agent) error {
	caps, err := marshalJSONString(a.Capabilities)
	if err != nil {
		return err
	}
	_, err = execer.ExecContext(ctx, `
			insert into agents (id, country, region, provider, isp, version, token_hash, capabilities, protocols, status, last_seen_at, created_at)
			values (?,?,?,?,?,?,?,?,?,?,?,?)
			on conflict (id) do update set
				country=case when agents.country='' then excluded.country else agents.country end,
				region=case when agents.region='' then excluded.region else agents.region end,
				provider=case when agents.provider='' then excluded.provider else agents.provider end,
				isp=case when agents.isp='' then excluded.isp else agents.isp end,
				version=excluded.version,
				token_hash=case when agents.token_hash='' then excluded.token_hash else agents.token_hash end,
				capabilities=excluded.capabilities,
				protocols=excluded.protocols,
				status=excluded.status,
				last_seen_at=excluded.last_seen_at`,
		a.ID, a.Country, a.Region, a.Provider, a.ISP, a.Version, a.TokenHash, caps, a.Protocols, a.Status, a.LastSeenAt, a.CreatedAt)
	return err
}

func (s *SQLite) TouchAgent(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `update agents set status=?, last_seen_at=? where id=?`, model.AgentOnline, time.Now().UTC(), id)
	return err
}

func (s *SQLite) MarkAgentOffline(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `update agents set status=? where id=?`, model.AgentOffline, id)
	return err
}

func (s *SQLite) DeleteAgent(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `delete from agents where id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	_, _ = s.db.ExecContext(ctx, `delete from agent_configs where id=?`, id)
	return nil
}

func (s *SQLite) MarkStaleAgentsOffline(ctx context.Context, ttl time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		update agents set status=?
		where status=? and last_seen_at < ?
		and not exists (select 1 from outbound_agents where outbound_agents.id=agents.id)`,
		model.AgentOffline, model.AgentOnline, time.Now().UTC().Add(-ttl))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLite) ListAgents(ctx context.Context) ([]model.Agent, error) {
	rows, err := s.db.QueryContext(ctx, `select id, country, region, provider, isp, coalesce(version,''), token_hash, capabilities, protocols, status, last_seen_at, created_at from agents order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []model.Agent
	for rows.Next() {
		var a model.Agent
		var caps string
		if err := rows.Scan(&a.ID, &a.Country, &a.Region, &a.Provider, &a.ISP, &a.Version, &a.TokenHash, &caps, &a.Protocols, &a.Status, &a.LastSeenAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		if err := unmarshalJSONString(caps, &a.Capabilities); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func scanSQLiteStoredAgent(row sqliteScanner) (model.Agent, error) {
	var a model.Agent
	var caps string
	if err := row.Scan(&a.ID, &a.Country, &a.Region, &a.Provider, &a.ISP, &a.Version, &a.TokenHash, &caps, &a.Protocols, &a.Status, &a.LastSeenAt, &a.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return a, ErrNotFound
		}
		return a, err
	}
	if err := unmarshalJSONString(caps, &a.Capabilities); err != nil {
		return a, err
	}
	return a, nil
}

func (s *SQLite) ListAgentConfigs(ctx context.Context) ([]config.AgentConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, disabled, labels, created_at, updated_at
		from agent_configs order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cfgs []config.AgentConfig
	for rows.Next() {
		cfg, err := scanSQLiteAgentConfig(rows)
		if err != nil {
			return nil, err
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, rows.Err()
}

func (s *SQLite) GetAgentConfig(ctx context.Context, id string) (config.AgentConfig, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, disabled, labels, created_at, updated_at
		from agent_configs where id=?`, id)
	cfg, err := scanSQLiteAgentConfig(row)
	if errors.Is(err, ErrNotFound) {
		return config.AgentConfig{ID: id}, nil
	}
	return cfg, err
}

func (s *SQLite) UpsertAgentConfig(ctx context.Context, cfg config.AgentConfig) error {
	if err := normalizeAgentConfig(&cfg); err != nil {
		return err
	}
	labelsRaw, err := marshalJSONString(cfg.Labels)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
		insert into agent_configs (id, disabled, labels, created_at, updated_at)
		values (?,?,?,?,?)
		on conflict (id) do update set
			disabled=excluded.disabled,
			labels=excluded.labels,
			updated_at=excluded.updated_at`,
		cfg.ID, cfg.Disabled, labelsRaw, now, now)
	return err
}

func (s *SQLite) DeleteAgentConfig(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `delete from agent_configs where id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) GetManagedSettings(ctx context.Context) (config.ManagedSettings, error) {
	settings := config.DefaultManagedSettings()
	row := s.db.QueryRowContext(ctx, `select value, revision, updated_at from managed_settings where id='default'`)
	var raw string
	var updated time.Time
	if err := row.Scan(&raw, &settings.Revision, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return settings, nil
		}
		return settings, err
	}
	if err := unmarshalJSONString(raw, &settings); err != nil {
		return settings, err
	}
	settings.UpdatedAt = updated.Format(time.RFC3339Nano)
	return settings, ValidateAndNormalizeManagedSettings(&settings)
}

func (s *SQLite) UpdateManagedSettings(ctx context.Context, settings config.ManagedSettings) (config.ManagedSettings, error) {
	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		return settings, err
	}
	currentRevision := settings.Revision
	settings.Revision = currentRevision + 1
	raw, err := marshalJSONString(settings)
	if err != nil {
		return settings, err
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		insert into managed_settings (id, value, updated_at)
		values ('default', ?, ?)
		on conflict (id) do update set
			value=excluded.value,
			revision=revision+1,
			updated_at=excluded.updated_at
		where revision=?`, raw, now, currentRevision)
	if err != nil {
		return settings, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return settings, err
	}
	if rows == 0 {
		return settings, ErrConflict
	}
	return s.GetManagedSettings(ctx)
}

func (s *SQLite) UpdateManagedSettingsAndAgentLabels(ctx context.Context, settings config.ManagedSettings, cfgs []config.AgentConfig, nodes []config.OutboundAgent) (config.ManagedSettings, error) {
	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		return settings, err
	}
	currentRevision := settings.Revision
	settings.Revision = currentRevision + 1
	raw, err := marshalJSONString(settings)
	if err != nil {
		return settings, err
	}
	cfgs = append([]config.AgentConfig(nil), cfgs...)
	for i := range cfgs {
		if err := normalizeAgentConfig(&cfgs[i]); err != nil {
			return settings, err
		}
	}
	nodes = append([]config.OutboundAgent(nil), nodes...)
	for i := range nodes {
		if err := normalizeOutboundAgent(&nodes[i]); err != nil {
			return settings, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return settings, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
		insert into managed_settings (id, value, updated_at)
		values ('default', ?, ?)
		on conflict (id) do update set
			value=excluded.value,
			revision=revision+1,
			updated_at=excluded.updated_at
		where revision=?`, raw, now, currentRevision)
	if err != nil {
		return settings, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return settings, err
	}
	if rows == 0 {
		return settings, ErrConflict
	}
	for _, cfg := range cfgs {
		labelsRaw, err := marshalJSONString(cfg.Labels)
		if err != nil {
			return settings, err
		}
		if _, err := tx.ExecContext(ctx, `
			insert into agent_configs (id, disabled, labels, created_at, updated_at)
			values (?,?,?,?,?)
			on conflict (id) do update set
				disabled=excluded.disabled,
				labels=excluded.labels,
				updated_at=excluded.updated_at`,
			cfg.ID, cfg.Disabled, labelsRaw, now, now); err != nil {
			return settings, err
		}
	}
	for _, node := range nodes {
		headersRaw, err := marshalJSONString(node.Headers)
		if err != nil {
			return settings, err
		}
		labelsRaw, err := marshalJSONString(node.Labels)
		if err != nil {
			return settings, err
		}
		tlsRaw, err := marshalJSONString(node.TLS)
		if err != nil {
			return settings, err
		}
		authRaw, err := marshalJSONString(node.Auth)
		if err != nil {
			return settings, err
		}
		if _, err := tx.ExecContext(ctx, `
			insert into outbound_agents (id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at)
			values (?,?,?,?,?,?,?,?,?,?,?)
			on conflict (id) do update set
				transport=excluded.transport,
				enabled=excluded.enabled,
				url=excluded.url,
				headers=excluded.headers,
				auth=excluded.auth,
				event_response=excluded.event_response,
				labels=excluded.labels,
				tls=excluded.tls,
				updated_at=excluded.updated_at`,
			node.ID, node.Transport, node.Enabled, node.URL, headersRaw, authRaw, node.EventResponse, labelsRaw, tlsRaw, now, now); err != nil {
			return settings, err
		}
	}
	if err := tx.Commit(); err != nil {
		return settings, err
	}
	return s.GetManagedSettings(ctx)
}

func (s *SQLite) AddAuditEvent(ctx context.Context, event AuditEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		insert into audit_events (subject, action, target, decision, reason, created_at)
		values (?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(event.Subject),
		strings.TrimSpace(event.Action),
		strings.TrimSpace(event.Target),
		strings.TrimSpace(event.Decision),
		strings.TrimSpace(event.Reason),
		event.CreatedAt)
	return err
}

func (s *SQLite) ListOutboundAgents(ctx context.Context) ([]config.OutboundAgent, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at
		from outbound_agents order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []config.OutboundAgent
	for rows.Next() {
		node, err := scanSQLiteOutboundAgent(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (s *SQLite) GetOutboundAgent(ctx context.Context, id string) (config.OutboundAgent, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at
		from outbound_agents where id=?`, id)
	return scanSQLiteOutboundAgent(row)
}

func (s *SQLite) UpsertOutboundAgent(ctx context.Context, node config.OutboundAgent) error {
	if err := normalizeOutboundAgent(&node); err != nil {
		return err
	}
	labels, err := marshalJSONString(node.Labels)
	if err != nil {
		return err
	}
	tlsRaw, err := marshalJSONString(node.TLS)
	if err != nil {
		return err
	}
	authRaw, err := marshalJSONString(node.Auth)
	if err != nil {
		return err
	}
	headersRaw, err := marshalJSONString(node.Headers)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
		insert into outbound_agents (id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at)
		values (?,?,?,?,?,?,?,?,?,?,?)
		on conflict (id) do update set
			transport=excluded.transport,
			enabled=excluded.enabled,
			url=excluded.url,
			headers=excluded.headers,
			auth=excluded.auth,
			event_response=excluded.event_response,
			labels=excluded.labels,
			tls=excluded.tls,
			updated_at=excluded.updated_at`,
		node.ID, node.Transport, node.Enabled, node.URL, headersRaw, authRaw, node.EventResponse, labels, tlsRaw, now, now)
	return err
}

func (s *SQLite) DeleteOutboundAgent(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `delete from outbound_agents where id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type sqliteScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteJob(row sqliteScanner) (model.Job, error) {
	var j model.Job
	var args string
	var startedAt sql.NullTime
	var completedAt sql.NullTime
	err := row.Scan(&j.ID, &j.ParentID, &j.ScheduledID, &j.ScheduledRevision, &j.Tool, &j.Target, &j.ResolvedTarget, &args, &j.IPVersion, &j.AgentID, &j.ResolveOnAgent, &j.Status, &j.CreatedAt, &j.UpdatedAt, &startedAt, &completedAt, &j.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if startedAt.Valid {
		j.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		j.CompletedAt = &completedAt.Time
	}
	if err := unmarshalJSONString(args, &j.Args); err != nil {
		return j, err
	}
	return j, nil
}

func scanSQLiteSchedule(row sqliteScanner) (model.ScheduledJob, error) {
	var sched model.ScheduledJob
	var args string
	var targets string
	var lastRunAt sql.NullTime
	err := row.Scan(&sched.ID, &sched.Revision, &sched.Name, &sched.Enabled, &sched.Tool, &sched.Target, &args, &sched.IPVersion, &sched.ResolveOnAgent, &sched.CronExpression, &sched.NextRunAt, &lastRunAt, &targets, &sched.CreatedAt, &sched.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sched, ErrNotFound
	}
	if err != nil {
		return sched, err
	}
	if lastRunAt.Valid {
		sched.LastRunAt = &lastRunAt.Time
	}
	if err := unmarshalJSONString(args, &sched.Args); err != nil {
		return sched, err
	}
	if err := unmarshalJSONString(targets, &sched.ScheduleTargets); err != nil {
		return sched, err
	}
	return sched, nil
}

func scanSQLiteOutboundAgent(row sqliteScanner) (config.OutboundAgent, error) {
	var node config.OutboundAgent
	var headersRaw string
	var authRaw string
	var labels string
	var tlsRaw string
	var created time.Time
	var updated time.Time
	err := row.Scan(&node.ID, &node.Transport, &node.Enabled, &node.URL, &headersRaw, &authRaw, &node.EventResponse, &labels, &tlsRaw, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return node, ErrNotFound
	}
	if err != nil {
		return node, err
	}
	if err := unmarshalJSONString(labels, &node.Labels); err != nil {
		return node, err
	}
	if err := unmarshalJSONString(headersRaw, &node.Headers); err != nil {
		return node, err
	}
	if err := unmarshalJSONString(authRaw, &node.Auth); err != nil {
		return node, err
	}
	node.Labels, err = validatedAgentLabels(node.Labels)
	if err != nil {
		return node, fmt.Errorf("stored outbound agent labels: %w", err)
	}
	if err := unmarshalJSONString(tlsRaw, &node.TLS); err != nil {
		return node, err
	}
	node.CreatedAt = created.Format(time.RFC3339)
	node.UpdatedAt = updated.Format(time.RFC3339)
	return node, nil
}

func scanSQLiteAgentConfig(row sqliteScanner) (config.AgentConfig, error) {
	var cfg config.AgentConfig
	var labelsRaw string
	var created time.Time
	var updated time.Time
	err := row.Scan(&cfg.ID, &cfg.Disabled, &labelsRaw, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, ErrNotFound
	}
	if err != nil {
		return cfg, err
	}
	if err := unmarshalJSONString(labelsRaw, &cfg.Labels); err != nil {
		return cfg, err
	}
	cfg.Labels, err = validatedAgentLabels(cfg.Labels)
	if err != nil {
		return cfg, fmt.Errorf("stored agent labels: %w", err)
	}
	cfg.CreatedAt = created.Format(time.RFC3339)
	cfg.UpdatedAt = updated.Format(time.RFC3339)
	return cfg, nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func sqlitePath(dsn string) (string, error) {
	if dsn == "" || dsn == "sqlite://:memory:" || dsn == "sqlite::memory:" {
		return ":memory:", nil
	}
	if strings.HasPrefix(dsn, "sqlite://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		if u.Host != "" {
			return u.Host + u.Path, nil
		}
		if u.Path != "" {
			return u.Path, nil
		}
		return strings.TrimPrefix(dsn, "sqlite://"), nil
	}
	if strings.HasPrefix(dsn, "file:") || strings.HasSuffix(dsn, ".db") || strings.HasSuffix(dsn, ".sqlite") {
		return dsn, nil
	}
	return "", fmt.Errorf("invalid sqlite dsn %q", dsn)
}

func migrateSQLite(ctx context.Context, db *sql.DB, bootstrapAdminToken string) (Initialization, error) {
	if _, err := db.ExecContext(ctx, `
create table if not exists agents (
  id text primary key,
  country text not null default '',
  region text not null default '',
  provider text not null default '',
  isp text not null default '',
  version text not null default '',
  token_hash text not null default '',
  capabilities text not null default '[]',
  protocols integer not null default 3,
  status text not null,
  last_seen_at timestamp not null,
  created_at timestamp not null
);

create table if not exists jobs (
  id text primary key,
  parent_id text null references jobs(id) on delete cascade,
  scheduled_id text null references scheduled_jobs(id) on delete set null,
  scheduled_revision integer not null default 0,
  tool text not null,
  target text not null,
  resolved_target text null,
  args text not null default '{}',
  ip_version integer not null default 0,
  agent_id text null references agents(id) on delete set null,
  resolve_on_agent integer not null default 0,
  status text not null,
  created_at timestamp not null,
  updated_at timestamp not null,
  started_at timestamp null,
  completed_at timestamp null,
  error text null
);

create index if not exists idx_jobs_queue on jobs (status, created_at);
create index if not exists idx_jobs_parent on jobs (parent_id, created_at);
create index if not exists idx_jobs_agent on jobs (agent_id);
create index if not exists idx_jobs_scheduled on jobs (scheduled_id, scheduled_revision, created_at);

create table if not exists scheduled_jobs (
  id text primary key,
  revision integer not null default 1,
  name text not null default '',
  enabled integer not null default 1,
  tool text not null,
  target text not null,
  args text not null default '{}',
  ip_version integer not null default 0,
  resolve_on_agent integer not null default 0,
  cron_expression text not null,
  next_run_at timestamp not null,
  last_run_at timestamp null,
  schedule_targets text not null default '[]',
  created_at timestamp not null,
  updated_at timestamp not null
);

create index if not exists idx_scheduled_jobs_due on scheduled_jobs (enabled, next_run_at);

create table if not exists job_events (
  id text primary key,
  job_id text not null references jobs(id) on delete cascade,
  agent_id text null references agents(id) on delete set null,
  stream text not null,
  event text null,
  exit_code integer null,
  parsed text null,
  created_at timestamp not null
);

create index if not exists idx_job_events_job on job_events (job_id, created_at);

create table if not exists audit_events (
  id integer primary key autoincrement,
  subject text not null,
  action text not null,
  target text not null default '',
  decision text not null,
  reason text not null default '',
  created_at timestamp not null default current_timestamp
);

create table if not exists managed_settings (
  id text primary key,
  value text not null default '{}',
  revision integer not null default 1,
  updated_at timestamp not null
);

create table if not exists outbound_agents (
  id text primary key,
	transport text not null,
  enabled integer not null default 1,
	url text not null,
	headers text not null default '{}',
	auth text not null,
	event_response text not null default '',
  labels text not null default '[]',
  tls text not null default '{}',
  created_at timestamp not null,
  updated_at timestamp not null
);

create table if not exists agent_configs (
  id text primary key,
  disabled integer not null default 0,
  labels text not null default '[]',
  created_at timestamp not null,
  updated_at timestamp not null
);`); err != nil {
		return Initialization{}, err
	}
	seeded, err := seedSQLiteManagedSettings(ctx, db, bootstrapAdminToken)
	if err != nil {
		return Initialization{}, err
	}
	return Initialization{Seeded: seeded}, nil
}

func seedSQLiteManagedSettings(ctx context.Context, db *sql.DB, bootstrapAdminToken string) (bool, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `select exists(select 1 from managed_settings where id='default')`).Scan(&exists); err != nil {
		return false, err
	}
	if exists != 0 {
		return false, nil
	}
	settings, err := bootstrapManagedSettings(bootstrapAdminToken)
	if err != nil {
		return false, err
	}
	settings.Revision = 1
	now := time.Now().UTC()
	settings.UpdatedAt = now.Format(time.RFC3339Nano)
	raw, err := marshalJSONString(settings)
	if err != nil {
		return false, err
	}
	res, err := db.ExecContext(ctx, `
		insert into managed_settings (id, value, updated_at)
		values ('default', ?, ?)
		on conflict (id) do nothing`, raw, now)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}
