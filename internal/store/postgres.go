package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(ctx context.Context, dsn string, bootstrapAdminToken string) (*Postgres, Initialization, error) {
	pool, err := openPostgresPool(ctx, dsn)
	if err != nil {
		return nil, Initialization{}, err
	}
	initialization, err := migratePostgres(ctx, pool, bootstrapAdminToken)
	if err != nil {
		pool.Close()
		return nil, Initialization{}, err
	}
	return &Postgres{pool: pool}, initialization, nil
}

func (p *Postgres) Close() {
	p.pool.Close()
}

func openPostgresPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if !isPostgresCode(err, "3D000") {
			return nil, err
		}
		dbName := cfg.ConnConfig.Database
		if createErr := createPostgresDatabase(ctx, cfg); createErr != nil && !isPostgresCode(createErr, "42P04") {
			return nil, fmt.Errorf("postgres database %q does not exist and automatic create failed: %w", dbName, createErr)
		}
		pool, err = pgxpool.NewWithConfig(ctx, cfg.Copy())
		if err != nil {
			return nil, err
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return pool, nil
}

func createPostgresDatabase(ctx context.Context, target *pgxpool.Config) error {
	dbName := target.ConnConfig.Database
	if dbName == "" {
		return errors.New("postgres database name is empty")
	}
	var lastErr error
	for _, maintenanceDB := range postgresMaintenanceDatabases(dbName) {
		cfg := target.ConnConfig.Copy()
		cfg.Database = maintenanceDB
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			lastErr = err
			if isPostgresCode(err, "3D000") {
				continue
			}
			return err
		}
		_, err = conn.Exec(ctx, "create database "+pgx.Identifier{dbName}.Sanitize())
		_ = conn.Close(ctx)
		if err != nil {
			return err
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no postgres maintenance database candidates")
}

func postgresMaintenanceDatabases(target string) []string {
	switch target {
	case "postgres":
		return []string{"template1"}
	case "template1":
		return []string{"postgres"}
	default:
		return []string{"postgres", "template1"}
	}
}

func isPostgresCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func migratePostgres(ctx context.Context, pool *pgxpool.Pool, bootstrapAdminToken string) (Initialization, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Initialization{}, err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	for _, stmt := range postgresSchemaStatements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return Initialization{}, err
		}
	}
	seeded, err := seedPostgresManagedSettings(ctx, tx, bootstrapAdminToken)
	if err != nil {
		return Initialization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Initialization{}, err
	}
	return Initialization{Seeded: seeded}, nil
}

type postgresSettingsSeeder interface {
	postgresJobInserter
	QueryRow(context.Context, string, ...any) pgx.Row
}

func seedPostgresManagedSettings(ctx context.Context, execer postgresSettingsSeeder, bootstrapAdminToken string) (bool, error) {
	var exists bool
	if err := execer.QueryRow(ctx, `select exists(select 1 from managed_settings where id='default')`).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	settings, err := bootstrapManagedSettings(bootstrapAdminToken)
	if err != nil {
		return false, err
	}
	settings.Revision = 1
	now := time.Now().UTC()
	settings.UpdatedAt = now.Format(time.RFC3339Nano)
	raw, err := marshalJSONBytes(settings)
	if err != nil {
		return false, err
	}
	tag, err := execer.Exec(ctx, `
		insert into managed_settings (id, value, updated_at)
		values ('default', $1, $2)
		on conflict (id) do nothing`, raw, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

var postgresSchemaStatements = []string{
	`create table if not exists agents (
  id text primary key,
  country text not null default '',
  region text not null default '',
  provider text not null default '',
  isp text not null default '',
  version text not null default '',
  token_hash text not null default '',
  capabilities jsonb not null default '[]',
  protocols integer not null default 3,
  status text not null,
  last_seen_at timestamptz not null,
  created_at timestamptz not null
)`,
	`create table if not exists scheduled_jobs (
  id text primary key,
  revision integer not null default 1,
  name text not null default '',
  enabled boolean not null default true,
  tool text not null,
  target text not null,
  args jsonb not null default '{}',
  ip_version integer not null default 0,
  resolve_on_agent boolean not null default false,
  cron_expression text not null,
  next_run_at timestamptz not null,
  last_run_at timestamptz null,
  schedule_targets jsonb not null default '[]',
  created_at timestamptz not null,
  updated_at timestamptz not null
)`,
	`create table if not exists jobs (
  id text primary key,
  parent_id text null references jobs(id) on delete cascade,
  scheduled_id text null references scheduled_jobs(id) on delete set null,
  scheduled_revision integer not null default 0,
  tool text not null,
  target text not null,
  resolved_target text null,
  args jsonb not null default '{}',
  ip_version integer not null default 0,
  agent_id text null references agents(id) on delete set null,
  resolve_on_agent boolean not null default false,
  status text not null,
  created_at timestamptz not null,
  updated_at timestamptz not null,
  started_at timestamptz null,
  completed_at timestamptz null,
  error text null
)`,
	`create table if not exists job_events (
  id text primary key,
  job_id text not null references jobs(id) on delete cascade,
  agent_id text null references agents(id) on delete set null,
  stream text not null,
  event jsonb null,
  exit_code integer null,
  parsed jsonb null,
  created_at timestamptz not null
)`,
	`create table if not exists audit_events (
 id bigserial primary key,
  subject text not null,
  action text not null,
  target text not null default '',
  decision text not null,
  reason text not null default '',
  created_at timestamptz not null default now()
)`,
	`create table if not exists managed_settings (
  id text primary key,
  value jsonb not null default '{}',
  revision bigint not null default 1,
  updated_at timestamptz not null
)`,
	`create table if not exists outbound_agents (
  id text primary key,
	transport text not null,
  enabled boolean not null default true,
	url text not null,
	headers jsonb not null default '{}',
	auth jsonb not null,
	event_response text not null default '',
  labels jsonb not null default '[]',
  tls jsonb not null default '{}',
  created_at timestamptz not null,
  updated_at timestamptz not null
)`,
	`create table if not exists agent_configs (
  id text primary key,
  disabled boolean not null default false,
  labels jsonb not null default '[]',
  created_at timestamptz not null,
 updated_at timestamptz not null
)`,
	`create index if not exists idx_scheduled_jobs_due on scheduled_jobs (enabled, next_run_at)`,
	`create index if not exists idx_jobs_queue on jobs (status, created_at)`,
	`create index if not exists idx_jobs_parent on jobs (parent_id, created_at)`,
	`create index if not exists idx_jobs_agent on jobs (agent_id)`,
	`create index if not exists idx_jobs_scheduled on jobs (scheduled_id, scheduled_revision, created_at)`,
	`create index if not exists idx_job_events_job on job_events (job_id, created_at)`,
}

func (p *Postgres) CreateJob(ctx context.Context, j model.Job) error {
	return insertPostgresJob(ctx, p.pool, j)
}

func (p *Postgres) CreateJobs(ctx context.Context, jobs []model.Job) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	for _, job := range jobs {
		if err := insertPostgresJob(ctx, tx, job); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type postgresJobInserter interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func insertPostgresJob(ctx context.Context, execer postgresJobInserter, j model.Job) error {
	args, err := marshalJSONBytes(j.Args)
	if err != nil {
		return err
	}
	_, err = execer.Exec(ctx, `
		insert into jobs (id, parent_id, scheduled_id, scheduled_revision, tool, target, resolved_target, args, ip_version, agent_id, resolve_on_agent, status, created_at, updated_at, started_at, completed_at, error)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		j.ID, nullable(j.ParentID), nullable(j.ScheduledID), j.ScheduledRevision, j.Tool, j.Target, nullable(j.ResolvedTarget), args, j.IPVersion, nullable(j.AgentID), j.ResolveOnAgent, j.Status, j.CreatedAt, j.UpdatedAt, j.StartedAt, j.CompletedAt, nullable(j.Error))
	return err
}

func (p *Postgres) GetJob(ctx context.Context, id string) (model.Job, error) {
	row := p.pool.QueryRow(ctx, `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs where id=$1`, id)
	return scanJob(row)
}

func (p *Postgres) ListChildJobs(ctx context.Context, parentID string) ([]model.Job, error) {
	rows, err := p.pool.Query(ctx, `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs
		where parent_id=$1
		order by created_at asc`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (p *Postgres) ListActiveJobs(ctx context.Context, limit int) ([]model.Job, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs
		where status = any($1)
		order by created_at asc
		limit $2`, []string{string(model.JobQueued), string(model.JobRunning)}, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (p *Postgres) ListQueuedJobs(ctx context.Context, agentID string, caps []model.Tool, protocols model.ProtocolMask, limit int) ([]model.Job, error) {
	rows, err := p.pool.Query(ctx, `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs
		where status=$1 and agent_id=$2 and tool = any($3)
		  and (ip_version=0 or (ip_version=4 and ($5 & 1) <> 0) or (ip_version=6 and ($5 & 2) <> 0))
		order by created_at asc
		limit $4`, model.JobQueued, agentID, toolsToStrings(caps), limit, protocols)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (p *Postgres) ClaimQueuedJob(ctx context.Context, id string, agentID string) (bool, error) {
	now := time.Now().UTC()
	tag, err := p.pool.Exec(ctx, `
		update jobs
		set status=$2,
		    updated_at=$3,
		    started_at=case when started_at is null then $3 else started_at end
		where id=$1 and agent_id=$5 and status=$4`,
		id, model.JobRunning, now, model.JobQueued, agentID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) UpdateJobStatus(ctx context.Context, id string, status model.JobStatus, msg string) error {
	now := time.Now().UTC()
	_, err := p.pool.Exec(ctx, `
		update jobs
		set status=$2,
		    updated_at=$3,
		    started_at=case when $2='running' and started_at is null then $3 else started_at end,
		    completed_at=case when $2 in ('succeeded','failed','canceled') then $3 else completed_at end,
		    error=case when $4='' then error else $4 end
		where id=$1`, id, status, now, msg)
	return err
}

func (p *Postgres) RequeueRunningJob(ctx context.Context, id string) (bool, error) {
	result, err := p.pool.Exec(ctx, `
		update jobs
		set status=$2, updated_at=$3, started_at=null, completed_at=null, error=''
		where id=$1 and status=$4`, id, model.JobQueued, time.Now().UTC(), model.JobRunning)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

func (p *Postgres) FinishRunningJob(ctx context.Context, id string, status model.JobStatus, msg string) (bool, error) {
	return p.transitionJob(ctx, id, status, msg, "status='running'")
}

func (p *Postgres) FailActiveJob(ctx context.Context, id string, msg string) (bool, error) {
	return p.transitionJob(ctx, id, model.JobFailed, msg, "status in ('queued','running')")
}

func (p *Postgres) transitionJob(ctx context.Context, id string, status model.JobStatus, msg string, predicate string) (bool, error) {
	query := `update jobs set status=$2, updated_at=$3, completed_at=$3, error=$4 where id=$1 and ` + predicate
	result, err := p.pool.Exec(ctx, query, id, status, time.Now().UTC(), msg)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

func (p *Postgres) AddJobEvent(ctx context.Context, e model.JobEvent) error {
	event, parsed, err := postgresJobEventJSON(e)
	if err != nil {
		return err
	}
	return insertPostgresJobEvent(ctx, p.pool, e, event, parsed)
}

func (p *Postgres) AddRunningJobEvent(ctx context.Context, sourceJobID string, agentID string, e model.JobEvent) (bool, error) {
	e.AgentID = agentID
	event, parsed, err := postgresJobEventJSON(e)
	if err != nil {
		return false, err
	}
	result, err := p.pool.Exec(ctx, `
		insert into job_events (id, job_id, agent_id, stream, event, exit_code, parsed, created_at)
		select $1,$2,$3,$4,$5,$6,$7,$8
		where exists (
			select 1 from jobs where id=$9 and status=$10 and agent_id=$11
		)`,
		e.ID, e.JobID, nullable(e.AgentID), e.Stream, event, e.ExitCode, parsed, e.CreatedAt,
		sourceJobID, model.JobRunning, agentID)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

func (p *Postgres) FinishRunningJobWithEvent(ctx context.Context, sourceJobID string, agentID string, status model.JobStatus, msg string, e model.JobEvent) (bool, error) {
	e.AgentID = agentID
	event, parsed, err := postgresJobEventJSON(e)
	if err != nil {
		return false, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	now := time.Now().UTC()
	result, err := tx.Exec(ctx, `
		update jobs set status=$2, updated_at=$3, completed_at=$3, error=$4
		where id=$1 and status=$5 and agent_id=$6`,
		sourceJobID, status, now, msg, model.JobRunning, agentID)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() == 0 {
		return false, nil
	}
	if err := insertPostgresJobEvent(ctx, tx, e, event, parsed); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func postgresJobEventJSON(e model.JobEvent) (any, any, error) {
	var event any
	if e.Event != nil {
		b, err := marshalJSONBytes(e.Event)
		if err != nil {
			return nil, nil, err
		}
		event = b
	}
	var parsed any
	if e.Parsed != nil {
		b, err := marshalJSONBytes(e.Parsed)
		if err != nil {
			return nil, nil, err
		}
		parsed = b
	}
	return event, parsed, nil
}

func insertPostgresJobEvent(ctx context.Context, execer postgresJobInserter, e model.JobEvent, event any, parsed any) error {
	_, err := execer.Exec(ctx, `
		insert into job_events (id, job_id, agent_id, stream, event, exit_code, parsed, created_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ID, e.JobID, nullable(e.AgentID), e.Stream, event, e.ExitCode, parsed, e.CreatedAt)
	return err
}

func (p *Postgres) ListJobEvents(ctx context.Context, jobID string) ([]model.JobEvent, error) {
	rows, err := p.pool.Query(ctx, `
		select id, job_id, coalesce(agent_id,''), stream, event, exit_code, parsed, created_at
		from job_events where job_id=$1 order by created_at asc`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []model.JobEvent
	for rows.Next() {
		var e model.JobEvent
		var event []byte
		var parsed []byte
		if err := rows.Scan(&e.ID, &e.JobID, &e.AgentID, &e.Stream, &event, &e.ExitCode, &parsed, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(event) > 0 {
			var streamEvent model.StreamEvent
			if err := unmarshalJSONBytes(event, &streamEvent); err != nil {
				return nil, err
			}
			if streamEvent.Type != "" {
				e.Event = &streamEvent
			}
		}
		if len(parsed) > 0 {
			var result model.ToolResult
			if err := unmarshalJSONBytes(parsed, &result); err != nil {
				return nil, err
			}
			e.Parsed = &result
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (p *Postgres) CreateScheduledJob(ctx context.Context, sched model.ScheduledJob) error {
	args, err := marshalJSONBytes(sched.Args)
	if err != nil {
		return err
	}
	targets, err := marshalJSONBytes(sched.ScheduleTargets)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		insert into scheduled_jobs (id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		sched.ID, sched.Revision, sched.Name, sched.Enabled, sched.Tool, sched.Target, args, sched.IPVersion,
		sched.ResolveOnAgent, sched.CronExpression, sched.NextRunAt, sched.LastRunAt, targets, sched.CreatedAt, sched.UpdatedAt)
	return err
}

func (p *Postgres) GetScheduledJob(ctx context.Context, id string) (model.ScheduledJob, error) {
	row := p.pool.QueryRow(ctx, `
		select id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at
		from scheduled_jobs where id=$1`, id)
	return scanSchedule(row)
}

func (p *Postgres) ListScheduledJobs(ctx context.Context) ([]model.ScheduledJob, error) {
	rows, err := p.pool.Query(ctx, `
		select id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at
		from scheduled_jobs order by created_at asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []model.ScheduledJob
	for rows.Next() {
		sched, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, sched)
	}
	return schedules, rows.Err()
}

func (p *Postgres) ListDueScheduledJobs(ctx context.Context, now time.Time, limit int) ([]model.ScheduledJob, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := p.pool.Query(ctx, `
		select id, revision, name, enabled, tool, target, args, ip_version, resolve_on_agent, cron_expression, next_run_at, last_run_at, schedule_targets, created_at, updated_at
		from scheduled_jobs
		where enabled=true and next_run_at <= $1
		order by next_run_at asc
		limit $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []model.ScheduledJob
	for rows.Next() {
		sched, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, sched)
	}
	return schedules, rows.Err()
}

func (p *Postgres) UpdateScheduledJob(ctx context.Context, sched model.ScheduledJob) error {
	args, err := marshalJSONBytes(sched.Args)
	if err != nil {
		return err
	}
	targets, err := marshalJSONBytes(sched.ScheduleTargets)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		update scheduled_jobs
		set revision=$2,
		    name=$3,
		    enabled=$4,
		    tool=$5,
		    target=$6,
		    args=$7,
		    ip_version=$8,
		    resolve_on_agent=$9,
		    cron_expression=$10,
		    next_run_at=$11,
		    last_run_at=$12,
		    schedule_targets=$13,
		    updated_at=$14
		where id=$1`,
		sched.ID, sched.Revision, sched.Name, sched.Enabled, sched.Tool, sched.Target, args, sched.IPVersion,
		sched.ResolveOnAgent, sched.CronExpression, sched.NextRunAt, sched.LastRunAt, targets, sched.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteScheduledJob(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `delete from scheduled_jobs where id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) UpdateScheduledJobRun(ctx context.Context, sched model.ScheduledJob) error {
	_, err := p.pool.Exec(ctx, `
		update scheduled_jobs set last_run_at=$2, next_run_at=$3, updated_at=$4 where id=$1`,
		sched.ID, sched.LastRunAt, sched.NextRunAt, time.Now().UTC())
	return err
}

func (p *Postgres) ListScheduledJobHistory(ctx context.Context, scheduleID string, filter ScheduledJobHistoryFilter) ([]model.Job, error) {
	query := `
		select id, coalesce(parent_id,''), coalesce(scheduled_id,''), scheduled_revision, tool, target, coalesce(resolved_target,''), args, ip_version, coalesce(agent_id,''), resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'')
		from jobs
		where scheduled_id=$1`
	args := []any{scheduleID}
	if filter.Revision > 0 {
		args = append(args, filter.Revision)
		query += fmt.Sprintf(" and scheduled_revision=$%d", len(args))
	}
	if filter.BucketSeconds > 0 {
		query = `
			select id, parent_id, scheduled_id, scheduled_revision, tool, target, resolved_target, args, ip_version, agent_id, resolve_on_agent, status, created_at, updated_at, started_at, completed_at, error
			from (
				select id, coalesce(parent_id,'') parent_id, coalesce(scheduled_id,'') scheduled_id, scheduled_revision, tool, target, coalesce(resolved_target,'') resolved_target, args, ip_version, coalesce(agent_id,'') agent_id, resolve_on_agent, status, created_at, updated_at, started_at, completed_at, coalesce(error,'') error,
					row_number() over (partition by coalesce(agent_id,''), floor(extract(epoch from coalesce(started_at, created_at)) / $1) order by coalesce(started_at, created_at) desc) rn
				from jobs
				where scheduled_id=$2`
		args = []any{filter.BucketSeconds, scheduleID}
		if filter.Revision > 0 {
			args = append(args, filter.Revision)
			query += fmt.Sprintf(" and scheduled_revision=$%d", len(args))
		}
	}
	if filter.HasFrom {
		args = append(args, filter.From)
		query += fmt.Sprintf(" and coalesce(started_at, created_at) >= $%d", len(args))
	}
	if filter.HasTo {
		args = append(args, filter.To)
		query += fmt.Sprintf(" and coalesce(started_at, created_at) <= $%d", len(args))
	}
	if filter.BucketSeconds > 0 {
		query += ") where rn=1"
	}
	args = append(args, normalizedHistoryLimit(filter.Limit))
	query += fmt.Sprintf(" order by coalesce(started_at, created_at) desc limit $%d", len(args))
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (p *Postgres) RegisterAgent(ctx context.Context, a model.Agent, suppliedToken string) (model.Agent, bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return model.Agent{}, false, err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	settings := config.DefaultManagedSettings()
	var raw []byte
	var updated time.Time
	if err := tx.QueryRow(ctx, `select value, revision, updated_at from managed_settings where id='default' for update`).Scan(&raw, &settings.Revision, &updated); err != nil {
		return model.Agent{}, false, err
	}
	if err := unmarshalJSONBytes(raw, &settings); err != nil {
		return model.Agent{}, false, err
	}
	settings.UpdatedAt = updated.Format(time.RFC3339Nano)
	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		return model.Agent{}, false, err
	}

	var current *model.Agent
	stored, err := scanPostgresStoredAgent(tx.QueryRow(ctx, `
		select id, country, region, provider, isp, coalesce(version,''), token_hash, capabilities, protocols, status, last_seen_at, created_at
		from agents where id=$1`, a.ID))
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
		settings.Revision++
		now := time.Now().UTC()
		settings.UpdatedAt = now.Format(time.RFC3339Nano)
		raw, err := marshalJSONBytes(settings)
		if err != nil {
			return model.Agent{}, false, err
		}
		if _, err := tx.Exec(ctx, `update managed_settings set value=$1, revision=revision+1, updated_at=$2 where id='default'`, raw, now); err != nil {
			return model.Agent{}, false, err
		}
	}
	if err := upsertPostgresAgent(ctx, tx, registered); err != nil {
		return model.Agent{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Agent{}, false, err
	}
	return registered, created, nil
}

func (p *Postgres) UpsertAgent(ctx context.Context, a model.Agent) error {
	return upsertPostgresAgent(ctx, p.pool, a)
}

func upsertPostgresAgent(ctx context.Context, execer postgresJobInserter, a model.Agent) error {
	caps, err := marshalJSONBytes(a.Capabilities)
	if err != nil {
		return err
	}
	_, err = execer.Exec(ctx, `
			insert into agents (id, country, region, provider, isp, version, token_hash, capabilities, protocols, status, last_seen_at, created_at)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
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

func (p *Postgres) TouchAgent(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `update agents set status=$2, last_seen_at=$3 where id=$1`, id, model.AgentOnline, time.Now().UTC())
	return err
}

func (p *Postgres) MarkAgentOffline(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `update agents set status=$2 where id=$1`, id, model.AgentOffline)
	return err
}

func (p *Postgres) DeleteAgent(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `delete from agents where id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, _ = p.pool.Exec(ctx, `delete from agent_configs where id=$1`, id)
	return nil
}

func (p *Postgres) MarkStaleAgentsOffline(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := p.pool.Exec(ctx, `
		update agents set status=$1
		where status=$2 and last_seen_at < $3
		and not exists (select 1 from outbound_agents where outbound_agents.id=agents.id)`,
		model.AgentOffline, model.AgentOnline, time.Now().UTC().Add(-ttl))
	return tag.RowsAffected(), err
}

func (p *Postgres) ListAgents(ctx context.Context) ([]model.Agent, error) {
	rows, err := p.pool.Query(ctx, `select id, country, region, provider, isp, coalesce(version,''), token_hash, capabilities, protocols, status, last_seen_at, created_at from agents order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []model.Agent
	for rows.Next() {
		var a model.Agent
		var caps []byte
		if err := rows.Scan(&a.ID, &a.Country, &a.Region, &a.Provider, &a.ISP, &a.Version, &a.TokenHash, &caps, &a.Protocols, &a.Status, &a.LastSeenAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		if err := unmarshalJSONBytes(caps, &a.Capabilities); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func scanPostgresStoredAgent(row pgx.Row) (model.Agent, error) {
	var a model.Agent
	var caps []byte
	if err := row.Scan(&a.ID, &a.Country, &a.Region, &a.Provider, &a.ISP, &a.Version, &a.TokenHash, &caps, &a.Protocols, &a.Status, &a.LastSeenAt, &a.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return a, ErrNotFound
		}
		return a, err
	}
	if err := unmarshalJSONBytes(caps, &a.Capabilities); err != nil {
		return a, err
	}
	return a, nil
}

func (p *Postgres) ListAgentConfigs(ctx context.Context) ([]config.AgentConfig, error) {
	rows, err := p.pool.Query(ctx, `
		select id, disabled, labels, created_at, updated_at
		from agent_configs order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cfgs []config.AgentConfig
	for rows.Next() {
		cfg, err := scanAgentConfig(rows)
		if err != nil {
			return nil, err
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, rows.Err()
}

func (p *Postgres) GetAgentConfig(ctx context.Context, id string) (config.AgentConfig, error) {
	row := p.pool.QueryRow(ctx, `
		select id, disabled, labels, created_at, updated_at
		from agent_configs where id=$1`, id)
	cfg, err := scanAgentConfig(row)
	if errors.Is(err, ErrNotFound) {
		return config.AgentConfig{ID: id}, nil
	}
	return cfg, err
}

func (p *Postgres) UpsertAgentConfig(ctx context.Context, cfg config.AgentConfig) error {
	if err := normalizeAgentConfig(&cfg); err != nil {
		return err
	}
	labelsRaw, err := marshalJSONBytes(cfg.Labels)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = p.pool.Exec(ctx, `
		insert into agent_configs (id, disabled, labels, created_at, updated_at)
		values ($1,$2,$3,$4,$5)
		on conflict (id) do update set
			disabled=excluded.disabled,
			labels=excluded.labels,
			updated_at=excluded.updated_at`,
		cfg.ID, cfg.Disabled, labelsRaw, now, now)
	return err
}

func (p *Postgres) DeleteAgentConfig(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `delete from agent_configs where id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) GetManagedSettings(ctx context.Context) (config.ManagedSettings, error) {
	settings := config.DefaultManagedSettings()
	row := p.pool.QueryRow(ctx, `select value, revision, updated_at from managed_settings where id='default'`)
	var raw []byte
	var updated time.Time
	if err := row.Scan(&raw, &settings.Revision, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return settings, nil
		}
		return settings, err
	}
	if err := unmarshalJSONBytes(raw, &settings); err != nil {
		return settings, err
	}
	settings.UpdatedAt = updated.Format(time.RFC3339Nano)
	return settings, ValidateAndNormalizeManagedSettings(&settings)
}

func (p *Postgres) UpdateManagedSettings(ctx context.Context, settings config.ManagedSettings) (config.ManagedSettings, error) {
	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		return settings, err
	}
	currentRevision := settings.Revision
	settings.Revision = currentRevision + 1
	raw, err := marshalJSONBytes(settings)
	if err != nil {
		return settings, err
	}
	tag, err := p.pool.Exec(ctx, `
		insert into managed_settings (id, value, updated_at)
		values ('default', $1, $2)
		on conflict (id) do update set
			value=excluded.value,
			revision=managed_settings.revision+1,
			updated_at=excluded.updated_at
		where managed_settings.revision=$3`, raw, time.Now().UTC(), currentRevision)
	if err != nil {
		return settings, err
	}
	if tag.RowsAffected() == 0 {
		return settings, ErrConflict
	}
	return p.GetManagedSettings(ctx)
}

func (p *Postgres) UpdateManagedSettingsAndAgentLabels(ctx context.Context, settings config.ManagedSettings, cfgs []config.AgentConfig, nodes []config.OutboundAgent) (config.ManagedSettings, error) {
	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		return settings, err
	}
	currentRevision := settings.Revision
	settings.Revision = currentRevision + 1
	raw, err := marshalJSONBytes(settings)
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
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return settings, err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	now := time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		insert into managed_settings (id, value, updated_at)
		values ('default', $1, $2)
		on conflict (id) do update set
			value=excluded.value,
			revision=managed_settings.revision+1,
			updated_at=excluded.updated_at
		where managed_settings.revision=$3`, raw, now, currentRevision)
	if err != nil {
		return settings, err
	}
	if tag.RowsAffected() == 0 {
		return settings, ErrConflict
	}
	for _, cfg := range cfgs {
		labelsRaw, err := marshalJSONBytes(cfg.Labels)
		if err != nil {
			return settings, err
		}
		if _, err := tx.Exec(ctx, `
			insert into agent_configs (id, disabled, labels, created_at, updated_at)
			values ($1,$2,$3,$4,$5)
			on conflict (id) do update set
				disabled=excluded.disabled,
				labels=excluded.labels,
				updated_at=excluded.updated_at`,
			cfg.ID, cfg.Disabled, labelsRaw, now, now); err != nil {
			return settings, err
		}
	}
	for _, node := range nodes {
		headersRaw, err := marshalJSONBytes(node.Headers)
		if err != nil {
			return settings, err
		}
		labelsRaw, err := marshalJSONBytes(node.Labels)
		if err != nil {
			return settings, err
		}
		tlsRaw, err := marshalJSONBytes(node.TLS)
		if err != nil {
			return settings, err
		}
		authRaw, err := marshalJSONBytes(node.Auth)
		if err != nil {
			return settings, err
		}
		if _, err := tx.Exec(ctx, `
			insert into outbound_agents (id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
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
	if err := tx.Commit(ctx); err != nil {
		return settings, err
	}
	return p.GetManagedSettings(ctx)
}

func (p *Postgres) AddAuditEvent(ctx context.Context, event AuditEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := p.pool.Exec(ctx, `
		insert into audit_events (subject, action, target, decision, reason, created_at)
		values ($1,$2,$3,$4,$5,$6)`,
		event.Subject, event.Action, event.Target, event.Decision, event.Reason, event.CreatedAt)
	return err
}

func (p *Postgres) ListOutboundAgents(ctx context.Context) ([]config.OutboundAgent, error) {
	rows, err := p.pool.Query(ctx, `
		select id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at
		from outbound_agents order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []config.OutboundAgent
	for rows.Next() {
		node, err := scanOutboundAgent(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (p *Postgres) GetOutboundAgent(ctx context.Context, id string) (config.OutboundAgent, error) {
	row := p.pool.QueryRow(ctx, `
		select id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at
		from outbound_agents where id=$1`, id)
	return scanOutboundAgent(row)
}

func (p *Postgres) UpsertOutboundAgent(ctx context.Context, node config.OutboundAgent) error {
	if err := normalizeOutboundAgent(&node); err != nil {
		return err
	}
	labels, err := marshalJSONBytes(node.Labels)
	if err != nil {
		return err
	}
	tlsRaw, err := marshalJSONBytes(node.TLS)
	if err != nil {
		return err
	}
	authRaw, err := marshalJSONBytes(node.Auth)
	if err != nil {
		return err
	}
	headersRaw, err := marshalJSONBytes(node.Headers)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = p.pool.Exec(ctx, `
		insert into outbound_agents (id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
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

func (p *Postgres) DeleteOutboundAgent(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `delete from outbound_agents where id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanJob(row pgx.Row) (model.Job, error) {
	var j model.Job
	var args []byte
	err := row.Scan(&j.ID, &j.ParentID, &j.ScheduledID, &j.ScheduledRevision, &j.Tool, &j.Target, &j.ResolvedTarget, &args, &j.IPVersion, &j.AgentID, &j.ResolveOnAgent, &j.Status, &j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.CompletedAt, &j.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if err := unmarshalJSONBytes(args, &j.Args); err != nil {
		return j, err
	}
	return j, nil
}

func scanSchedule(row pgx.Row) (model.ScheduledJob, error) {
	var sched model.ScheduledJob
	var args []byte
	var targets []byte
	err := row.Scan(&sched.ID, &sched.Revision, &sched.Name, &sched.Enabled, &sched.Tool, &sched.Target, &args, &sched.IPVersion, &sched.ResolveOnAgent, &sched.CronExpression, &sched.NextRunAt, &sched.LastRunAt, &targets, &sched.CreatedAt, &sched.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return sched, ErrNotFound
	}
	if err != nil {
		return sched, err
	}
	if err := unmarshalJSONBytes(args, &sched.Args); err != nil {
		return sched, err
	}
	if err := unmarshalJSONBytes(targets, &sched.ScheduleTargets); err != nil {
		return sched, err
	}
	return sched, nil
}

func scanOutboundAgent(row pgx.Row) (config.OutboundAgent, error) {
	var node config.OutboundAgent
	var headersRaw []byte
	var authRaw []byte
	var labels []byte
	var tlsRaw []byte
	var created time.Time
	var updated time.Time
	err := row.Scan(&node.ID, &node.Transport, &node.Enabled, &node.URL, &headersRaw, &authRaw, &node.EventResponse, &labels, &tlsRaw, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return node, ErrNotFound
	}
	if err != nil {
		return node, err
	}
	if err := unmarshalJSONBytes(labels, &node.Labels); err != nil {
		return node, err
	}
	if err := unmarshalJSONBytes(headersRaw, &node.Headers); err != nil {
		return node, err
	}
	if err := unmarshalJSONBytes(authRaw, &node.Auth); err != nil {
		return node, err
	}
	node.Labels, err = validatedAgentLabels(node.Labels)
	if err != nil {
		return node, fmt.Errorf("stored outbound agent labels: %w", err)
	}
	if err := unmarshalJSONBytes(tlsRaw, &node.TLS); err != nil {
		return node, err
	}
	node.CreatedAt = created.Format(time.RFC3339)
	node.UpdatedAt = updated.Format(time.RFC3339)
	return node, nil
}

func scanAgentConfig(row pgx.Row) (config.AgentConfig, error) {
	var cfg config.AgentConfig
	var labels []byte
	var created time.Time
	var updated time.Time
	err := row.Scan(&cfg.ID, &cfg.Disabled, &labels, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return cfg, ErrNotFound
	}
	if err != nil {
		return cfg, err
	}
	if err := unmarshalJSONBytes(labels, &cfg.Labels); err != nil {
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

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func toolsToStrings(tools []model.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, string(t))
	}
	return out
}
