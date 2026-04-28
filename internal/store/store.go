package store

import (
	"context"
	"time"

	"github.com/ztelliot/mtr/internal/model"
)

type Store interface {
	CreateJob(context.Context, model.Job) error
	CreateJobs(context.Context, []model.Job) error
	GetJob(context.Context, string) (model.Job, error)
	ListChildJobs(context.Context, string) ([]model.Job, error)
	ListActiveJobs(context.Context, int) ([]model.Job, error)
	ListQueuedJobs(context.Context, string, []model.Tool, model.ProtocolMask, int) ([]model.Job, error)
	ClaimQueuedJob(context.Context, string) (bool, error)
	UpdateJobStatus(context.Context, string, model.JobStatus, string) error
	AddJobEvent(context.Context, model.JobEvent) error
	ListJobEvents(context.Context, string) ([]model.JobEvent, error)
	CreateScheduledJob(context.Context, model.ScheduledJob) error
	GetScheduledJob(context.Context, string) (model.ScheduledJob, error)
	ListScheduledJobs(context.Context) ([]model.ScheduledJob, error)
	ListDueScheduledJobs(context.Context, time.Time, int) ([]model.ScheduledJob, error)
	UpdateScheduledJob(context.Context, model.ScheduledJob) error
	DeleteScheduledJob(context.Context, string) error
	UpdateScheduledJobRun(context.Context, string, time.Time, time.Time) error
	ListScheduledJobHistory(context.Context, string, ScheduledJobHistoryFilter) ([]model.Job, error)
	UpsertAgent(context.Context, model.Agent) error
	TouchAgent(context.Context, string) error
	MarkAgentOffline(context.Context, string) error
	MarkStaleAgentsOffline(context.Context, time.Duration) (int64, error)
	ListAgents(context.Context) ([]model.Agent, error)
}

type ScheduledJobHistoryFilter struct {
	Limit         int
	BucketSeconds int64
	Revision      int
	From          time.Time
	To            time.Time
	HasFrom       bool
	HasTo         bool
}
