package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

type Memory struct {
	mu         sync.Mutex
	jobs       map[string]model.Job
	events     map[string][]model.JobEvent
	agents     map[string]model.Agent
	agentCfgs  map[string]config.AgentConfig
	schedules  map[string]model.ScheduledJob
	settings   config.ManagedSettings
	httpAgents map[string]config.HTTPAgent
	audit      []AuditEvent
}

func NewMemory() *Memory {
	settings := config.CloneManagedSettings(config.DefaultManagedSettings())
	settings.Revision = 1
	settings.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return &Memory{
		jobs:       map[string]model.Job{},
		events:     map[string][]model.JobEvent{},
		agents:     map[string]model.Agent{},
		agentCfgs:  map[string]config.AgentConfig{},
		schedules:  map[string]model.ScheduledJob{},
		settings:   settings,
		httpAgents: map[string]config.HTTPAgent{},
		audit:      []AuditEvent{},
	}
}

func (m *Memory) CreateJob(_ context.Context, j model.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.ID] = j
	return nil
}

func (m *Memory) CreateJobs(_ context.Context, jobs []model.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range jobs {
		m.jobs[job.ID] = job
	}
	return nil
}

func (m *Memory) GetJob(_ context.Context, id string) (model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return model.Job{}, ErrNotFound
	}
	return j, nil
}

func (m *Memory) ListChildJobs(_ context.Context, parentID string) ([]model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var jobs []model.Job
	for _, j := range m.jobs {
		if j.ParentID == parentID {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	return jobs, nil
}

func (m *Memory) ListActiveJobs(_ context.Context, limit int) ([]model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var jobs []model.Job
	for _, j := range m.jobs {
		if j.Status == model.JobQueued || j.Status == model.JobRunning {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

func (m *Memory) ListQueuedJobs(_ context.Context, agentID string, caps []model.Tool, protocols model.ProtocolMask, limit int) ([]model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	capSet := map[model.Tool]bool{}
	for _, c := range caps {
		capSet[c] = true
	}
	var jobs []model.Job
	for _, j := range m.jobs {
		required := protocolForJob(j)
		if j.Status == model.JobQueued && j.AgentID == agentID && capSet[j.Tool] && (required == 0 || protocols&required != 0) {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

func (m *Memory) ClaimQueuedJob(_ context.Context, id string, agentID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return false, nil
	}
	if j.Status != model.JobQueued || j.AgentID != agentID {
		return false, nil
	}
	now := time.Now().UTC()
	j.Status = model.JobRunning
	j.UpdatedAt = now
	if j.StartedAt == nil {
		j.StartedAt = &now
	}
	m.jobs[id] = j
	return true, nil
}

func protocolForJob(j model.Job) model.ProtocolMask {
	switch j.IPVersion {
	case model.IPv4:
		return model.ProtocolIPv4
	case model.IPv6:
		return model.ProtocolIPv6
	default:
		return 0
	}
}

func (m *Memory) UpdateJobStatus(_ context.Context, id string, status model.JobStatus, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	j.Status = status
	j.UpdatedAt = now
	if status == model.JobRunning && j.StartedAt == nil {
		j.StartedAt = &now
	}
	if status == model.JobSucceeded || status == model.JobFailed || status == model.JobCanceled {
		j.CompletedAt = &now
	}
	if msg != "" {
		j.Error = msg
	}
	m.jobs[id] = j
	return nil
}

func (m *Memory) AddJobEvent(_ context.Context, e model.JobEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[e.JobID]; !ok {
		return ErrNotFound
	}
	m.events[e.JobID] = append(m.events[e.JobID], e)
	return nil
}

func (m *Memory) ListJobEvents(_ context.Context, jobID string) ([]model.JobEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]model.JobEvent(nil), m.events[jobID]...), nil
}

func (m *Memory) CreateScheduledJob(_ context.Context, s model.ScheduledJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedules[s.ID] = s
	return nil
}

func (m *Memory) GetScheduledJob(_ context.Context, id string) (model.ScheduledJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.schedules[id]
	if !ok {
		return model.ScheduledJob{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) ListScheduledJobs(_ context.Context) ([]model.ScheduledJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.ScheduledJob, 0, len(m.schedules))
	for _, s := range m.schedules {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) ListDueScheduledJobs(_ context.Context, now time.Time, limit int) ([]model.ScheduledJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.ScheduledJob
	for _, s := range m.schedules {
		if s.Enabled && !s.NextRunAt.After(now) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextRunAt.Before(out[j].NextRunAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) UpdateScheduledJob(_ context.Context, s model.ScheduledJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.schedules[s.ID]; !ok {
		return ErrNotFound
	}
	m.schedules[s.ID] = s
	return nil
}

func (m *Memory) DeleteScheduledJob(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.schedules[id]; !ok {
		return ErrNotFound
	}
	delete(m.schedules, id)
	return nil
}

func (m *Memory) UpdateScheduledJobRun(_ context.Context, s model.ScheduledJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.schedules[s.ID]
	if !ok {
		return ErrNotFound
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = existing.CreatedAt
	}
	s.UpdatedAt = time.Now().UTC()
	m.schedules[s.ID] = s
	return nil
}

func (m *Memory) ListScheduledJobHistory(_ context.Context, scheduleID string, filter ScheduledJobHistoryFilter) ([]model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Job
	byBucket := map[string]model.Job{}
	for _, j := range m.jobs {
		runAt := historyRunAt(j)
		if j.ScheduledID != scheduleID || !historyFilterMatches(runAt, filter) || !historyRevisionMatches(j, filter) {
			continue
		}
		if filter.BucketSeconds > 0 {
			key := historyBucketKey(j, filter)
			if existing, ok := byBucket[key]; !ok || runAt.After(historyRunAt(existing)) {
				byBucket[key] = j
			}
		} else {
			out = append(out, j)
		}
	}
	if filter.BucketSeconds > 0 {
		for _, j := range byBucket {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return historyRunAt(out[i]).After(historyRunAt(out[j])) })
	limit := normalizedHistoryLimit(filter.Limit)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) UpsertAgent(_ context.Context, a model.Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.agents[a.ID]; ok {
		a.CreatedAt = old.CreatedAt
	}
	m.agents[a.ID] = a
	return nil
}

func (m *Memory) TouchAgent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return ErrNotFound
	}
	a.Status = model.AgentOnline
	a.LastSeenAt = time.Now().UTC()
	m.agents[id] = a
	return nil
}

func (m *Memory) MarkAgentOffline(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return ErrNotFound
	}
	a.Status = model.AgentOffline
	m.agents[id] = a
	return nil
}

func (m *Memory) DeleteAgent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[id]; !ok {
		return ErrNotFound
	}
	delete(m.agents, id)
	delete(m.agentCfgs, id)
	return nil
}

func (m *Memory) MarkStaleAgentsOffline(_ context.Context, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().UTC().Add(-ttl)
	var n int64
	for id, a := range m.agents {
		if a.Status == model.AgentOnline && a.LastSeenAt.Before(cutoff) {
			a.Status = model.AgentOffline
			m.agents[id] = a
			n++
		}
	}
	return n, nil
}

func (m *Memory) ListAgents(_ context.Context) ([]model.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Agent, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) ListAgentConfigs(_ context.Context) ([]config.AgentConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.AgentConfig, 0, len(m.agentCfgs))
	for _, cfg := range m.agentCfgs {
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) GetAgentConfig(_ context.Context, id string) (config.AgentConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.agentCfgs[id]
	if !ok {
		return config.AgentConfig{ID: id}, nil
	}
	return cfg, nil
}

func (m *Memory) UpsertAgentConfig(_ context.Context, cfg config.AgentConfig) error {
	if err := normalizeAgentConfig(&cfg); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	if existing, ok := m.agentCfgs[cfg.ID]; ok {
		cfg.CreatedAt = existing.CreatedAt
	} else {
		cfg.CreatedAt = now
	}
	cfg.UpdatedAt = now
	m.agentCfgs[cfg.ID] = cfg
	return nil
}

func (m *Memory) DeleteAgentConfig(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agentCfgs[id]; !ok {
		return ErrNotFound
	}
	delete(m.agentCfgs, id)
	return nil
}

func (m *Memory) GetManagedSettings(_ context.Context) (config.ManagedSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings := config.CloneManagedSettings(m.settings)
	if err := config.NormalizeManagedSettings(&settings); err != nil {
		return settings, err
	}
	return settings, nil
}

func (m *Memory) UpdateManagedSettings(_ context.Context, settings config.ManagedSettings) (config.ManagedSettings, error) {
	if err := config.NormalizeManagedSettings(&settings); err != nil {
		return settings, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if settings.Revision != m.settings.Revision {
		return settings, ErrConflict
	}
	settings.Revision = m.settings.Revision + 1
	settings.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	m.settings = config.CloneManagedSettings(settings)
	return config.CloneManagedSettings(m.settings), nil
}

func (m *Memory) UpdateManagedSettingsAndAgentLabels(_ context.Context, settings config.ManagedSettings, cfgs []config.AgentConfig, nodes []config.HTTPAgent) (config.ManagedSettings, error) {
	if err := config.NormalizeManagedSettings(&settings); err != nil {
		return settings, err
	}
	cfgs = append([]config.AgentConfig(nil), cfgs...)
	for i := range cfgs {
		if err := normalizeAgentConfig(&cfgs[i]); err != nil {
			return settings, err
		}
	}
	nodes = append([]config.HTTPAgent(nil), nodes...)
	for i := range nodes {
		if err := normalizeHTTPAgent(&nodes[i]); err != nil {
			return settings, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if settings.Revision != m.settings.Revision {
		return settings, ErrConflict
	}
	now := time.Now().UTC().Format(time.RFC3339)
	settings.Revision = m.settings.Revision + 1
	settings.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	m.settings = config.CloneManagedSettings(settings)
	for _, cfg := range cfgs {
		if existing, ok := m.agentCfgs[cfg.ID]; ok {
			cfg.CreatedAt = existing.CreatedAt
		} else {
			cfg.CreatedAt = now
		}
		cfg.UpdatedAt = now
		m.agentCfgs[cfg.ID] = cfg
	}
	for _, node := range nodes {
		if existing, ok := m.httpAgents[node.ID]; ok {
			node.CreatedAt = existing.CreatedAt
		} else {
			node.CreatedAt = now
		}
		node.UpdatedAt = now
		m.httpAgents[node.ID] = node
	}
	return config.CloneManagedSettings(m.settings), nil
}

func (m *Memory) AddAuditEvent(_ context.Context, event AuditEvent) error {
	event.Subject = strings.TrimSpace(event.Subject)
	event.Action = strings.TrimSpace(event.Action)
	event.Target = strings.TrimSpace(event.Target)
	event.Decision = strings.TrimSpace(event.Decision)
	event.Reason = strings.TrimSpace(event.Reason)
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, event)
	return nil
}

func (m *Memory) AuditEvents() []AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AuditEvent, len(m.audit))
	copy(out, m.audit)
	return out
}

func (m *Memory) ListHTTPAgents(_ context.Context) ([]config.HTTPAgent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.HTTPAgent, 0, len(m.httpAgents))
	for _, node := range m.httpAgents {
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) GetHTTPAgent(_ context.Context, id string) (config.HTTPAgent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	node, ok := m.httpAgents[id]
	if !ok {
		return config.HTTPAgent{}, ErrNotFound
	}
	return node, nil
}

func (m *Memory) UpsertHTTPAgent(_ context.Context, node config.HTTPAgent) error {
	if err := normalizeHTTPAgent(&node); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	if existing, ok := m.httpAgents[node.ID]; ok {
		node.CreatedAt = existing.CreatedAt
	} else {
		node.CreatedAt = now
	}
	node.UpdatedAt = now
	m.httpAgents[node.ID] = node
	return nil
}

func (m *Memory) DeleteHTTPAgent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.httpAgents[id]; !ok {
		return ErrNotFound
	}
	delete(m.httpAgents, id)
	return nil
}
