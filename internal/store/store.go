package store

import (
	"context"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

type Store interface {
	CreateJob(context.Context, model.Job) error
	CreateJobs(context.Context, []model.Job) error
	GetJob(context.Context, string) (model.Job, error)
	ListChildJobs(context.Context, string) ([]model.Job, error)
	ListActiveJobs(context.Context, int) ([]model.Job, error)
	ListQueuedJobs(context.Context, string, []model.Tool, model.ProtocolMask, int) ([]model.Job, error)
	ClaimQueuedJob(context.Context, string, string) (bool, error)
	UpdateJobStatus(context.Context, string, model.JobStatus, string) error
	AddJobEvent(context.Context, model.JobEvent) error
	ListJobEvents(context.Context, string) ([]model.JobEvent, error)
	CreateScheduledJob(context.Context, model.ScheduledJob) error
	GetScheduledJob(context.Context, string) (model.ScheduledJob, error)
	ListScheduledJobs(context.Context) ([]model.ScheduledJob, error)
	ListDueScheduledJobs(context.Context, time.Time, int) ([]model.ScheduledJob, error)
	UpdateScheduledJob(context.Context, model.ScheduledJob) error
	DeleteScheduledJob(context.Context, string) error
	UpdateScheduledJobRun(context.Context, model.ScheduledJob) error
	ListScheduledJobHistory(context.Context, string, ScheduledJobHistoryFilter) ([]model.Job, error)
	UpsertAgent(context.Context, model.Agent) error
	TouchAgent(context.Context, string) error
	MarkAgentOffline(context.Context, string) error
	DeleteAgent(context.Context, string) error
	MarkStaleAgentsOffline(context.Context, time.Duration) (int64, error)
	ListAgents(context.Context) ([]model.Agent, error)
	ListAgentConfigs(context.Context) ([]config.AgentConfig, error)
	GetAgentConfig(context.Context, string) (config.AgentConfig, error)
	UpsertAgentConfig(context.Context, config.AgentConfig) error
	DeleteAgentConfig(context.Context, string) error
	GetManagedSettings(context.Context) (config.ManagedSettings, error)
	UpdateManagedSettings(context.Context, config.ManagedSettings) (config.ManagedSettings, error)
	UpdateManagedSettingsAndAgentLabels(context.Context, config.ManagedSettings, []config.AgentConfig, []config.HTTPAgent) (config.ManagedSettings, error)
	AddAuditEvent(context.Context, AuditEvent) error
	ListHTTPAgents(context.Context) ([]config.HTTPAgent, error)
	GetHTTPAgent(context.Context, string) (config.HTTPAgent, error)
	UpsertHTTPAgent(context.Context, config.HTTPAgent) error
	DeleteHTTPAgent(context.Context, string) error
}

type AuditEvent struct {
	Subject   string
	Action    string
	Target    string
	Decision  string
	Reason    string
	CreatedAt time.Time
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
