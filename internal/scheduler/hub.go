package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

type Hub struct {
	store          store.Store
	policies       policy.Set
	labelConfigs   map[string]config.LabelConfig
	registerTokens []string
	log            *slog.Logger
	offlineAfter   time.Duration
	pollInterval   time.Duration
	maxInflight    int

	mu                         sync.Mutex
	meta                       map[string]agentMeta
	running                    map[string]map[string]struct{}
	inflightCancels            map[string]context.CancelFunc
	grpcCancels                map[string]chan string
	subs                       map[string]map[chan model.JobEvent]struct{}
	httpAgents                 []HTTPAgent
	httpAgentCancels           map[string]context.CancelFunc
	httpAgentMaxHealthInterval time.Duration
	httpAgentInvokeAttempts    int
}

type agentMeta struct {
	capabilities []model.Tool
	protocols    model.ProtocolMask
	lastSeen     time.Time
}

const timedOutJobScanLimit = 200

func NewHub(st store.Store, policies policy.Set, offlineAfter time.Duration, pollInterval time.Duration, maxInflight int, log *slog.Logger) *Hub {
	if offlineAfter <= 0 {
		offlineAfter = 90 * time.Second
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	if maxInflight <= 0 {
		maxInflight = 4
	}
	return &Hub{
		store:                      st,
		policies:                   policies,
		log:                        log,
		offlineAfter:               offlineAfter,
		pollInterval:               pollInterval,
		maxInflight:                maxInflight,
		meta:                       map[string]agentMeta{},
		running:                    map[string]map[string]struct{}{},
		inflightCancels:            map[string]context.CancelFunc{},
		grpcCancels:                map[string]chan string{},
		subs:                       map[string]map[chan model.JobEvent]struct{}{},
		httpAgentCancels:           map[string]context.CancelFunc{},
		httpAgentMaxHealthInterval: defaultHTTPAgentMaxHealthInterval,
		httpAgentInvokeAttempts:    defaultHTTPAgentInvokeAttempts,
	}
}

func (h *Hub) SetInflightLimit(limit int) {
	if limit <= 0 {
		limit = 4
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maxInflight = limit
}

func (h *Hub) SetHTTPAgentRuntime(maxHealthInterval time.Duration, invokeAttempts int) {
	if maxHealthInterval <= 0 {
		maxHealthInterval = defaultHTTPAgentMaxHealthInterval
	}
	if invokeAttempts <= 0 {
		invokeAttempts = defaultHTTPAgentInvokeAttempts
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpAgentMaxHealthInterval = maxHealthInterval
	h.httpAgentInvokeAttempts = invokeAttempts
}

func (h *Hub) ApplySettings(settings config.ManagedSettings) {
	_ = config.NormalizeManagedSettings(&settings)
	labelConfigs := config.NormalizeLabelConfigs(settings.LabelConfigs)
	globalScheduler := config.Scheduler{}
	config.DefaultScheduler(&globalScheduler)
	globalRuntime := config.DefaultRuntime()
	basePolicies := h.policiesSnapshot()
	if cfg, ok := labelConfigs[config.AgentAllLabel]; ok {
		if cfg.Scheduler != nil {
			globalScheduler = *cfg.Scheduler
		}
		if cfg.Runtime != nil {
			globalRuntime = *cfg.Runtime
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.policies = basePolicies
	h.labelConfigs = labelConfigs
	h.registerTokens = normalizedRegisterTokens(settings.RegisterTokens)
	h.offlineAfter = time.Duration(globalScheduler.AgentOfflineAfterSec) * time.Second
	h.pollInterval = time.Duration(globalScheduler.PollIntervalSec) * time.Second
	h.maxInflight = globalScheduler.MaxInflightPerAgent
	h.httpAgentMaxHealthInterval = time.Duration(globalRuntime.HTTPMaxHealthIntervalSec) * time.Second
	h.httpAgentInvokeAttempts = globalRuntime.HTTPInvokeAttempts
}

func (h *Hub) Start(ctx context.Context) {
	h.startConfiguredHTTPAgents(ctx)
	ticker := time.NewTicker(h.currentPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(h.currentPollInterval())
			if n, err := h.store.MarkStaleAgentsOffline(ctx, h.offlineAfterSnapshot()); err != nil {
				h.log.Warn("mark stale agents offline", "err", err)
			} else if n > 0 {
				h.log.Debug("marked stale agents offline", "count", n)
			}
			h.failTimedOutJobs(ctx)
			h.runDueSchedules(ctx)
		}
	}
}

func (h *Hub) runDueSchedules(ctx context.Context) {
	now := time.Now().UTC()
	schedules, err := h.store.ListDueScheduledJobs(ctx, now, 50)
	if err != nil {
		h.log.Warn("list due schedules", "err", err)
		return
	}
	for _, sched := range schedules {
		policies := h.policiesForLabels([]string{model.AgentAllLabel})
		dueTargets := dueScheduleTargets(sched, now)
		if len(dueTargets) == 0 {
			continue
		}
		if _, ok := policies.Get(sched.Tool); !ok {
			advanceScheduleTargets(&sched, dueTargets, now)
			_ = h.store.UpdateScheduledJobRun(ctx, sched)
			h.log.Debug("skip scheduled job with disabled tool", "schedule_id", sched.ID, "tool", sched.Tool)
			continue
		}
		job := model.Job{
			ID:                uuid.NewString(),
			ScheduledID:       sched.ID,
			ScheduledRevision: sched.Revision,
			Tool:              sched.Tool,
			Target:            sched.Target,
			Args:              policies.ServerArgs(sched.Tool, sched.Args),
			IPVersion:         sched.IPVersion,
			ResolveOnAgent:    sched.ResolveOnAgent,
			Status:            model.JobQueued,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if sched.ResolveOnAgent {
			literalVersion, literal, err := policy.LiteralIPVersion(sched.Tool, sched.Target)
			if err != nil {
				advanceScheduleTargets(&sched, dueTargets, now)
				_ = h.store.UpdateScheduledJobRun(ctx, sched)
				h.log.Warn("infer scheduled job target version", "schedule_id", sched.ID, "target", sched.Target, "err", err)
				continue
			}
			if literal && job.IPVersion == model.IPAny {
				job.IPVersion = literalVersion
			}
		} else if sched.Tool != model.ToolDNS {
			resolvedTarget, resolvedVersion, err := policies.ResolveTarget(ctx, sched.Tool, sched.Target, sched.IPVersion)
			if err != nil {
				advanceScheduleTargets(&sched, dueTargets, now)
				_ = h.store.UpdateScheduledJobRun(ctx, sched)
				h.log.Warn("resolve scheduled job target", "schedule_id", sched.ID, "target", sched.Target, "err", err)
				continue
			}
			job.ResolvedTarget = resolvedTarget
			if job.IPVersion == model.IPAny && resolvedVersion != model.IPAny {
				job.IPVersion = resolvedVersion
			}
		}
		seenAgentIDs := map[string]struct{}{}
		runs := make([]model.Job, 0)
		for _, target := range dueTargets {
			agents, err := h.scheduledRunAgents(ctx, sched, target, job.IPVersion)
			if err != nil {
				h.log.Warn("list scheduled run agents", "schedule_id", sched.ID, "schedule_target_label", target.Label, "err", err)
				continue
			}
			for _, agent := range agents {
				if _, ok := seenAgentIDs[agent.ID]; ok {
					continue
				}
				agentPolicies := h.policiesForLabels(agent.Labels)
				validateReq := model.CreateJobRequest{
					Tool:           sched.Tool,
					Target:         sched.Target,
					Args:           sched.Args,
					IPVersion:      sched.IPVersion,
					AgentID:        agent.ID,
					ResolveOnAgent: sched.ResolveOnAgent,
				}
				if _, err := agentPolicies.ValidateSchedule(validateReq); err != nil {
					continue
				}
				seenAgentIDs[agent.ID] = struct{}{}
				run := job
				run.ID = uuid.NewString()
				run.AgentID = agent.ID
				run.Args = agentPolicies.ServerArgs(sched.Tool, sched.Args)
				runs = append(runs, run)
			}
		}
		if len(runs) == 0 {
			advanceScheduleTargets(&sched, dueTargets, now)
			_ = h.store.UpdateScheduledJobRun(ctx, sched)
			h.log.Warn("skip scheduled fanout with no matching online agents", "schedule_id", sched.ID, "tool", sched.Tool)
			continue
		}
		if err := h.store.CreateJobs(ctx, runs); err != nil {
			h.log.Warn("create scheduled job runs", "schedule_id", sched.ID, "runs", len(runs), "err", err)
			continue
		}
		advanceScheduleTargets(&sched, dueTargets, now)
		if err := h.store.UpdateScheduledJobRun(ctx, sched); err != nil {
			h.log.Warn("update scheduled job run", "schedule_id", sched.ID, "err", err)
		}
		h.log.Debug("scheduled job queued", "schedule_id", sched.ID, "runs", len(runs), "next_run_at", sched.NextRunAt)
	}
}

func dueScheduleTargets(sched model.ScheduledJob, now time.Time) []model.ScheduleTarget {
	targets := sched.EffectiveScheduleTargets()
	out := make([]model.ScheduleTarget, 0, len(targets))
	for _, target := range targets {
		if target.NextRunAt.IsZero() || !target.NextRunAt.After(now) {
			out = append(out, target)
		}
	}
	return out
}

func advanceScheduleTargets(sched *model.ScheduledJob, dueTargets []model.ScheduleTarget, now time.Time) {
	dueByID := map[string]model.ScheduleTarget{}
	for _, target := range dueTargets {
		dueByID[target.ID] = target
	}
	for i := range sched.ScheduleTargets {
		if _, ok := dueByID[sched.ScheduleTargets[i].ID]; !ok {
			continue
		}
		nextRun := nextScheduleTargetRun(sched.ScheduleTargets[i], now)
		sched.ScheduleTargets[i].LastRunAt = &now
		sched.ScheduleTargets[i].NextRunAt = nextRun
	}
	syncScheduleAggregateRunFields(sched)
	sched.UpdatedAt = now
}

func nextScheduleTargetRun(target model.ScheduleTarget, now time.Time) time.Time {
	interval := time.Duration(target.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Duration(10) * time.Second
	}
	nextRun := now.Add(interval)
	if candidate := target.NextRunAt.Add(interval); candidate.After(now) {
		nextRun = candidate
	}
	return nextRun
}

func syncScheduleAggregateRunFields(sched *model.ScheduledJob) {
	if len(sched.ScheduleTargets) == 0 {
		return
	}
	nextRun := sched.ScheduleTargets[0].NextRunAt
	var lastRunAt *time.Time
	for _, target := range sched.ScheduleTargets {
		if target.NextRunAt.Before(nextRun) {
			nextRun = target.NextRunAt
		}
		if target.LastRunAt != nil && (lastRunAt == nil || target.LastRunAt.After(*lastRunAt)) {
			t := *target.LastRunAt
			lastRunAt = &t
		}
	}
	sched.NextRunAt = nextRun
	sched.LastRunAt = lastRunAt
}

func (h *Hub) scheduledRunAgents(ctx context.Context, sched model.ScheduledJob, target model.ScheduleTarget, version model.IPVersion) ([]model.Agent, error) {
	agents, err := h.agentsWithManagedLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.Agent, 0, len(agents))
	for _, agent := range agents {
		if h.agentConfigDisabled(ctx, agent.ID) {
			continue
		}
		if !scheduledTargetMatchesAgent(target, agent) {
			continue
		}
		if !scheduledTargetAllowsAgent(target, agent.ID) {
			continue
		}
		if policy.AgentSupports(agent, sched.Tool, version) {
			out = append(out, agent)
		}
	}
	return out, nil
}

func scheduledTargetAllowsAgent(target model.ScheduleTarget, agentID string) bool {
	if len(target.AllowedAgentIDs) == 0 {
		return true
	}
	for _, allowed := range target.AllowedAgentIDs {
		if allowed == agentID {
			return true
		}
	}
	return false
}

func scheduledTargetMatchesAgent(target model.ScheduleTarget, agent model.Agent) bool {
	for _, label := range agent.Labels {
		if strings.TrimSpace(label) == strings.TrimSpace(target.Label) {
			return true
		}
	}
	return false
}

func (h *Hub) Connect(stream grpcwire.Control_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.Type != "hello" || first.Agent == nil {
		return errors.New("first message must be hello")
	}
	hello := first.Agent
	if !h.registerTokenAllowed(hello.Token) {
		return errors.New("invalid agent token")
	}
	agent := model.Agent{
		ID:           hello.ID,
		Country:      hello.Country,
		Region:       hello.Region,
		Provider:     hello.Provider,
		ISP:          hello.ISP,
		Version:      hello.Version,
		TokenHash:    hashToken(hello.Token),
		Capabilities: hello.Capabilities,
		Protocols:    hello.Protocols,
		Status:       model.AgentOnline,
		LastSeenAt:   time.Now().UTC(),
		CreatedAt:    time.Now().UTC(),
	}
	if err := h.store.UpsertAgent(stream.Context(), agent); err != nil {
		return err
	}

	cancelQueueSize := h.inflightLimit() * 2
	if cancelQueueSize < 1 {
		cancelQueueSize = 1
	}
	h.mu.Lock()
	h.meta[agent.ID] = agentMeta{capabilities: hello.Capabilities, protocols: hello.Protocols, lastSeen: agent.LastSeenAt}
	if h.running[agent.ID] == nil {
		h.running[agent.ID] = map[string]struct{}{}
	}
	cancelCh := make(chan string, cancelQueueSize)
	h.grpcCancels[agent.ID] = cancelCh
	h.mu.Unlock()
	h.log.Info("agent connected", "agent_id", agent.ID, "country", agent.Country, "region", agent.Region, "provider", agent.Provider, "isp", agent.ISP, "capabilities", hello.Capabilities, "protocols", hello.Protocols)
	defer func() {
		runningJobs := h.runningJobs(agent.ID)
		h.mu.Lock()
		delete(h.meta, agent.ID)
		delete(h.running, agent.ID)
		delete(h.grpcCancels, agent.ID)
		h.mu.Unlock()
		for _, jobID := range runningJobs {
			h.failJob(context.Background(), agent.ID, jobID, "agent disconnected")
		}
		if err := h.store.MarkAgentOffline(context.Background(), agent.ID); err != nil {
			h.log.Warn("mark disconnected agent offline failed", "agent_id", agent.ID, "err", err)
		}
		h.log.Info("agent disconnected", "agent_id", agent.ID, "failed_jobs", len(runningJobs))
	}()

	errCh := make(chan error, 2)
	go h.recvLoop(stream, agent.ID, errCh)
	go h.sendLoop(stream, agent.ID, hello.Capabilities, hello.Protocols, cancelCh, errCh)
	return <-errCh
}

func (h *Hub) agentsWithManagedLabels(ctx context.Context) ([]model.Agent, error) {
	agents, err := h.store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	configs, err := h.store.ListAgentConfigs(ctx)
	if err != nil {
		return nil, err
	}
	grpcLabels := make(map[string][]string, len(configs))
	for _, cfg := range configs {
		grpcLabels[cfg.ID] = cfg.Labels
	}
	httpNodes, err := h.store.ListHTTPAgents(ctx)
	if err != nil {
		return nil, err
	}
	httpLabels := make(map[string][]string, len(httpNodes))
	for _, node := range httpNodes {
		httpLabels[node.ID] = node.Labels
	}
	for i := range agents {
		labels := grpcLabels[agents[i].ID]
		transport := model.AgentTransportGRPC
		if httpNodeLabels, ok := httpLabels[agents[i].ID]; ok {
			labels = httpNodeLabels
			transport = model.AgentTransportHTTP
		}
		agents[i].Labels = model.NormalizeAgentLabels(agents[i].ID, transport, labels)
	}
	return agents, nil
}

func (h *Hub) registerTokenAllowed(token string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, allowed := range h.registerTokens {
		if token == allowed {
			return true
		}
	}
	return false
}

func (h *Hub) recvLoop(stream grpcwire.Control_ConnectServer, agentID string, errCh chan<- error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				errCh <- nil
				return
			}
			errCh <- err
			return
		}
		switch msg.Type {
		case "heartbeat":
			h.touchAgent(agentID)
			_ = h.store.TouchAgent(stream.Context(), agentID)
			h.log.Debug("agent heartbeat", "agent_id", agentID)
		case "result":
			if msg.Result != nil {
				h.handleAgentResult(stream.Context(), agentID, *msg.Result)
			}
		}
	}
}

func (h *Hub) sendLoop(stream grpcwire.Control_ConnectServer, agentID string, caps []model.Tool, protocols model.ProtocolMask, cancelCh <-chan string, errCh chan<- error) {
	ticker := time.NewTicker(h.pollIntervalForAgent(stream.Context(), agentID))
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			errCh <- stream.Context().Err()
			return
		case jobID := <-cancelCh:
			if jobID == "" {
				continue
			}
			if err := stream.Send(&grpcwire.ServerMessage{Type: "cancel", Cancel: &grpcwire.Cancel{JobID: jobID}}); err != nil {
				errCh <- err
				return
			}
		case <-ticker.C:
			ticker.Reset(h.pollIntervalForAgent(stream.Context(), agentID))
			if h.agentConfigDisabled(stream.Context(), agentID) {
				_ = h.store.MarkAgentOffline(stream.Context(), agentID)
				continue
			}
			offlineAfter := h.offlineAfterForAgent(stream.Context(), agentID)
			if h.agentStaleAfter(agentID, offlineAfter) {
				_ = h.store.MarkAgentOffline(stream.Context(), agentID)
				h.log.Debug("skip stale agent dispatch", "agent_id", agentID, "offline_after", offlineAfter.String())
				continue
			}
			maxInflight := h.inflightLimitForAgent(stream.Context(), agentID)
			remaining := maxInflight - h.inflightCount(agentID)
			if remaining <= 0 {
				h.log.Debug("agent at inflight limit", "agent_id", agentID, "max_inflight", maxInflight, "transport", "grpc")
				continue
			}
			policies := h.policiesForAgent(stream.Context(), agentID)
			policyCaps := policyCapabilities(policies, caps)
			jobs, err := h.store.ListQueuedJobs(stream.Context(), agentID, policyCaps, protocols, remaining)
			if err != nil {
				h.log.Warn("list queued jobs", "err", err)
				continue
			}
			for _, job := range jobs {
				p, ok := policies.Get(job.Tool)
				if !ok {
					h.log.Debug("skip job with disabled tool", "job_id", job.ID, "tool", job.Tool)
					continue
				}
				claimed, err := h.store.ClaimQueuedJob(stream.Context(), job.ID, agentID)
				if err != nil {
					h.log.Warn("claim queued job failed", "job_id", job.ID, "err", err)
					continue
				}
				if !claimed {
					h.log.Debug("skip already claimed job", "job_id", job.ID)
					continue
				}
				h.markInflight(agentID, job.ID)
				h.emitStarted(stream.Context(), job)
				h.log.Debug("dispatch job", "agent_id", agentID, "job_id", job.ID, "tool", job.Tool, "ip_version", job.IPVersion)
				if err := stream.Send(toServerJobWithPolicies(job, p, policies)); err != nil {
					errCh <- err
					return
				}
			}
		}
	}
}

func policyCapabilities(policies policy.Set, caps []model.Tool) []model.Tool {
	out := make([]model.Tool, 0, len(caps))
	for _, cap := range caps {
		if _, ok := policies.Get(cap); ok {
			out = append(out, cap)
		}
	}
	return out
}

func (h *Hub) handleResult(ctx context.Context, agentID string, ev grpcwire.ResultEvent) {
	job, err := h.store.GetJob(ctx, ev.JobID)
	if err != nil {
		h.log.Warn("drop result for unknown job", "agent_id", agentID, "job_id", ev.JobID, "err", err)
		return
	}
	typ := eventType(ev.Event)
	if !h.agentOwnsJob(agentID, job) {
		h.log.Warn("drop result for job not assigned to agent", "agent_id", agentID, "job_id", ev.JobID, "assigned_agent_id", job.AgentID, "event_type", typ)
		return
	}
	if terminalJobStatus(job.Status) {
		h.log.Debug("drop result for terminal job", "agent_id", agentID, "job_id", ev.JobID, "status", job.Status, "event_type", typ)
		return
	}
	jobEvent, err := h.jobEventFromWire(job, typ, ev.Event)
	if err != nil {
		h.log.Warn("drop malformed job event", "agent_id", agentID, "job_id", ev.JobID, "event_type", typ, "err", err)
		return
	}
	eventJobID := job.ID
	if job.ParentID != "" {
		eventJobID = job.ParentID
	}
	event := model.JobEvent{
		ID:        uuid.NewString(),
		JobID:     eventJobID,
		AgentID:   agentID,
		Stream:    typ,
		Event:     jobEvent.Event,
		ExitCode:  jobEvent.ExitCode,
		Parsed:    jobEvent.Parsed,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.AddJobEvent(ctx, event); err != nil {
		h.log.Warn("store job event failed", "agent_id", agentID, "job_id", ev.JobID, "event_type", typ, "err", err)
		return
	}
	h.publish(event)
	if jobEvent.ExitCode != nil {
		h.clearInflight(agentID, ev.JobID)
		status := model.JobSucceeded
		msg := ""
		if *jobEvent.ExitCode != 0 {
			status = model.JobFailed
			msg = model.JobErrorToolFailed
			if failureType := parsedFailureType(jobEvent.Parsed); failureType != "" {
				msg = failureType
			}
		}
		if err := h.store.UpdateJobStatus(ctx, ev.JobID, status, msg); err != nil {
			h.log.Warn("update completed job status failed", "agent_id", agentID, "job_id", ev.JobID, "status", status, "err", err)
			return
		}
		if job.ParentID != "" {
			h.completeParentIfDone(ctx, job.ParentID)
		} else {
			h.emitProgress(ctx, ev.JobID, progressMessageForStatus(status))
		}
		h.log.Debug("job completed", "agent_id", agentID, "job_id", ev.JobID, "status", status, "exit_code", *jobEvent.ExitCode)
	}
}

func (h *Hub) agentOwnsJob(agentID string, job model.Job) bool {
	if agentID == "" || job.AgentID != agentID {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	running := h.running[agentID]
	if running == nil {
		return false
	}
	_, ok := running[job.ID]
	return ok
}

func (h *Hub) completeParentIfDone(ctx context.Context, parentID string) {
	h.completeParentIfDoneWithFailure(ctx, parentID, "", false)
}

func (h *Hub) completeParentIfDoneWithFailure(ctx context.Context, parentID string, failureMsg string, emitFailure bool) {
	parent, err := h.store.GetJob(ctx, parentID)
	if err != nil {
		h.log.Warn("load fanout parent failed", "parent_job_id", parentID, "err", err)
		return
	}
	if terminalJobStatus(parent.Status) {
		return
	}
	children, err := h.store.ListChildJobs(ctx, parentID)
	if err != nil {
		h.log.Warn("list fanout children failed", "parent_job_id", parentID, "err", err)
		return
	}
	if len(children) == 0 {
		return
	}
	succeeded := false
	for _, child := range children {
		if !terminalJobStatus(child.Status) {
			return
		}
		if child.Status == model.JobSucceeded {
			succeeded = true
		}
	}
	status := model.JobFailed
	msg := failureMsg
	if succeeded {
		status = model.JobSucceeded
		msg = ""
	} else if msg == "" {
		msg = "one or more fanout jobs failed"
	}
	if err := h.store.UpdateJobStatus(ctx, parentID, status, msg); err != nil {
		h.log.Warn("update fanout parent status failed", "parent_job_id", parentID, "status", status, "err", err)
		return
	}
	if status == model.JobFailed && emitFailure && msg != "" {
		h.emitFailureMessage(ctx, parentID, "", msg)
	}
	h.emitProgress(ctx, parentID, progressMessageForStatus(status))
}

func terminalJobStatus(status model.JobStatus) bool {
	return status == model.JobSucceeded || status == model.JobFailed || status == model.JobCanceled
}

func (h *Hub) emitStarted(ctx context.Context, job model.Job) {
	if job.ParentID != "" {
		return
	}
	h.emitProgress(ctx, job.ID, "started")
}

func (h *Hub) failJob(ctx context.Context, agentID string, jobID string, msg string) {
	if agentID != "" {
		h.clearInflight(agentID, jobID)
	}
	job, err := h.store.GetJob(ctx, jobID)
	if err != nil {
		h.log.Warn("load failed job status failed", "agent_id", agentID, "job_id", jobID, "err", err)
		return
	}
	if terminalJobStatus(job.Status) {
		h.log.Debug("skip failing terminal job", "agent_id", agentID, "job_id", jobID, "status", job.Status)
		return
	}
	if err := h.store.UpdateJobStatus(ctx, jobID, model.JobFailed, msg); err != nil {
		h.log.Warn("mark job failed status failed", "agent_id", agentID, "job_id", jobID, "err", err)
		return
	}
	eventJobID := jobID
	if job.ParentID != "" {
		eventJobID = job.ParentID
	}
	h.emitFailureMessage(ctx, eventJobID, agentID, msg)
	if job.ParentID != "" {
		h.completeParentIfDone(ctx, job.ParentID)
		return
	}
	h.emitProgress(ctx, jobID, "failed")
}

func (h *Hub) failTimedOutJobs(ctx context.Context) {
	jobs, err := h.store.ListActiveJobs(ctx, timedOutJobScanLimit)
	if err != nil {
		h.log.Warn("list active jobs for timeout sweep failed", "err", err)
		return
	}
	now := time.Now().UTC()
	for _, job := range jobs {
		if h.deferFanoutParentTimeout(ctx, job, now) {
			continue
		}
		if !h.jobTimedOut(ctx, job, now) {
			continue
		}
		if h.failFanoutChildren(ctx, job) {
			h.completeParentIfDoneWithFailure(ctx, job.ID, model.JobErrorTimeout, true)
		} else {
			h.failJob(ctx, job.AgentID, job.ID, model.JobErrorTimeout)
		}
		if job.ParentID != "" {
			continue
		}
		updated, err := h.store.GetJob(ctx, job.ID)
		if err != nil || updated.Status == model.JobFailed {
			h.log.Warn("job timed out", "job_id", job.ID, "agent_id", job.AgentID, "tool", job.Tool, "status", job.Status)
		}
	}
}

func (h *Hub) deferFanoutParentTimeout(ctx context.Context, job model.Job, now time.Time) bool {
	if job.ParentID != "" || job.AgentID != "" || job.Status != model.JobRunning {
		return false
	}
	children, err := h.store.ListChildJobs(ctx, job.ID)
	if err != nil {
		h.log.Warn("list fanout children for timeout sweep failed", "parent_job_id", job.ID, "err", err)
		return false
	}
	if len(children) == 0 {
		return false
	}
	start := job.CreatedAt
	if job.StartedAt != nil {
		start = *job.StartedAt
	}
	overallTimeout := h.policiesForLabels([]string{model.AgentAllLabel}).MaxToolTimeout()
	if overallTimeout > 0 && !start.IsZero() && !now.Before(start.Add(overallTimeout)) {
		return false
	}
	for _, child := range children {
		if !terminalJobStatus(child.Status) {
			return true
		}
	}
	h.completeParentIfDone(ctx, job.ID)
	return true
}

func (h *Hub) jobTimedOut(ctx context.Context, job model.Job, now time.Time) bool {
	policies := h.policiesForLabels([]string{model.AgentAllLabel})
	if job.AgentID != "" {
		policies = h.policiesForAgent(ctx, job.AgentID)
	}
	timeout := policies.TimeoutForJob(job)
	if p, ok := policies.Get(job.Tool); ok && p.Timeout > 0 {
		timeout = p.Timeout
	}
	if timeout <= 0 {
		return false
	}
	start := job.CreatedAt
	if job.Status == model.JobRunning && job.StartedAt != nil {
		start = *job.StartedAt
	}
	if start.IsZero() {
		return false
	}
	return !now.Before(start.Add(timeout))
}

func (h *Hub) failFanoutChildren(ctx context.Context, parent model.Job) bool {
	if parent.ParentID != "" || parent.AgentID != "" {
		return false
	}
	children, err := h.store.ListChildJobs(ctx, parent.ID)
	if err != nil {
		h.log.Warn("list fanout children for parent timeout failed", "parent_job_id", parent.ID, "err", err)
		return false
	}
	if len(children) == 0 {
		return false
	}
	for _, child := range children {
		if terminalJobStatus(child.Status) {
			continue
		}
		h.cancelInflight(child.ID)
		if child.AgentID != "" {
			h.sendGRPCCancel(child.AgentID, child.ID)
			h.clearInflight(child.AgentID, child.ID)
		}
		if err := h.store.UpdateJobStatus(ctx, child.ID, model.JobFailed, model.JobErrorTimeout); err != nil {
			h.log.Warn("mark fanout child timed out failed", "parent_job_id", parent.ID, "child_job_id", child.ID, "agent_id", child.AgentID, "err", err)
		}
	}
	return true
}

func (h *Hub) emitFailureMessage(ctx context.Context, jobID string, agentID string, msg string) {
	failureType := model.PublicJobErrorType(msg)
	if failureType == "" {
		failureType = model.JobErrorFailed
	}
	event := model.JobEvent{
		ID:        uuid.NewString(),
		JobID:     jobID,
		AgentID:   agentID,
		Stream:    "message",
		Event:     &model.StreamEvent{Type: "message", Message: failureType},
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.AddJobEvent(ctx, event); err != nil {
		h.log.Warn("store job failure event failed", "job_id", jobID, "agent_id", agentID, "failure_type", failureType, "err", err)
		return
	}
	h.publish(event)
}

func progressMessageForStatus(status model.JobStatus) string {
	switch status {
	case model.JobSucceeded:
		return "completed"
	case model.JobFailed:
		return "failed"
	case model.JobCanceled:
		return "canceled"
	default:
		return string(status)
	}
}

func (h *Hub) handleAgentResult(ctx context.Context, agentID string, result grpcwire.AgentResult) {
	h.handleResult(ctx, agentID, grpcwire.ResultEvent{
		JobID:   result.JobID,
		AgentID: agentID,
		Event:   result.Event,
	})
}

func (h *Hub) jobEventFromWire(job model.Job, typ string, payload map[string]any) (model.JobEvent, error) {
	if typ == "summary" {
		return h.summaryEventFromWire(job, payload)
	}
	var event model.StreamEvent
	if b, err := json.Marshal(payload); err == nil {
		if err := json.Unmarshal(b, &event); err != nil {
			return model.JobEvent{}, err
		}
	} else {
		return model.JobEvent{}, err
	}
	if event.Type == "" {
		event.Type = typ
	}
	return model.JobEvent{Event: &event}, nil
}

func (h *Hub) summaryEventFromWire(job model.Job, payload map[string]any) (model.JobEvent, error) {
	exitCode, ok := intFromPayload(payload["exit_code"])
	if !ok {
		return model.JobEvent{}, errors.New("summary event missing numeric exit_code")
	}
	summary := map[string]any{}
	if rawMetric, ok := payload["metric"]; ok {
		var metricOK bool
		summary, metricOK = metricMapFromPayload(rawMetric)
		if !metricOK {
			return model.JobEvent{}, errors.New("summary metric must be an object")
		}
	}
	parsed := &model.ToolResult{
		Type:      typOrDefault(eventType(payload), "summary"),
		Tool:      job.Tool,
		Target:    job.Target,
		IPVersion: job.IPVersion,
		ExitCode:  exitCode,
		Summary:   summary,
	}
	if records, ok := recordsFromPayload(payload["records"]); ok {
		parsed.Records = records
	}
	if hops, ok := hopsFromPayload(payload["hops"]); ok {
		parsed.Hops = hops
	}
	return model.JobEvent{ExitCode: &parsed.ExitCode, Parsed: parsed}, nil
}

func parsedFailureType(parsed *model.ToolResult) string {
	if parsed == nil {
		return ""
	}
	value, ok := parsed.Summary["status"].(string)
	if !ok {
		return ""
	}
	switch value {
	case model.JobErrorTargetBlocked, model.JobErrorUnsupportedProtocol, model.JobErrorUnsupportedTool:
		return value
	default:
		return ""
	}
}

func metricMapFromPayload(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	if metric, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(metric))
		for key, value := range metric {
			out[key] = value
		}
		return out, true
	}
	var metric map[string]any
	b, err := json.Marshal(v)
	if err != nil || json.Unmarshal(b, &metric) != nil {
		return nil, false
	}
	return metric, true
}

func eventType(payload map[string]any) string {
	if payload != nil {
		if typ, ok := payload["type"].(string); ok && typ != "" {
			return typ
		}
	}
	return "message"
}

func typOrDefault(typ string, fallback string) string {
	if typ == "" || typ == "message" {
		return fallback
	}
	return typ
}

func intFromPayload(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func recordsFromPayload(v any) ([]model.DNSRecord, bool) {
	if v == nil {
		return nil, false
	}
	var records []model.DNSRecord
	b, err := json.Marshal(v)
	if err != nil || json.Unmarshal(b, &records) != nil {
		return nil, false
	}
	return records, len(records) > 0
}

func hopsFromPayload(v any) ([]model.HopResult, bool) {
	if v == nil {
		return nil, false
	}
	var hops []model.HopResult
	b, err := json.Marshal(v)
	if err != nil || json.Unmarshal(b, &hops) != nil {
		return nil, false
	}
	return hops, len(hops) > 0
}

func (h *Hub) SubscribeJob(ctx context.Context, jobID string) <-chan model.JobEvent {
	ch := make(chan model.JobEvent, 32)
	h.mu.Lock()
	if h.subs[jobID] == nil {
		h.subs[jobID] = map[chan model.JobEvent]struct{}{}
	}
	h.subs[jobID][ch] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subs[jobID], ch)
		if len(h.subs[jobID]) == 0 {
			delete(h.subs, jobID)
		}
		h.mu.Unlock()
		close(ch)
	}()
	return ch
}

func (h *Hub) publish(event model.JobEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[event.JobID] {
		select {
		case ch <- event:
		default:
			h.log.Debug("drop slow sse subscriber event", "job_id", event.JobID, "event_id", event.ID)
		}
	}
}

func (h *Hub) PublishEvent(event model.JobEvent) {
	h.publish(event)
}

func (h *Hub) toServerJob(job model.Job, p policy.Policy) *grpcwire.ServerMessage {
	return toServerJobWithPolicies(job, p, h.policiesSnapshot())
}

func toServerJobWithPolicies(job model.Job, p policy.Policy, policies policy.Set) *grpcwire.ServerMessage {
	return &grpcwire.ServerMessage{Type: "job", Job: toJobSpecWithPolicies(job, p, policies)}
}

func toJobSpec(job model.Job, p policy.Policy) *grpcwire.JobSpec {
	return toJobSpecWithPolicies(job, p, policy.DefaultPolicies())
}

func (h *Hub) toJobSpec(job model.Job, p policy.Policy) *grpcwire.JobSpec {
	return toJobSpecWithPolicies(job, p, h.policiesSnapshot())
}

func toJobSpecWithPolicies(job model.Job, p policy.Policy, policies policy.Set) *grpcwire.JobSpec {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = policies.TimeoutForJob(job)
	}
	probeTimeout := p.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = policies.ProbeTimeout()
	}
	return &grpcwire.JobSpec{
		ID:                    job.ID,
		Tool:                  job.Tool,
		Target:                job.Target,
		ResolvedTarget:        job.ResolvedTarget,
		Args:                  policies.ServerArgs(job.Tool, job.Args),
		IPVersion:             job.IPVersion,
		ResolveOnAgent:        job.ResolveOnAgent,
		TimeoutSeconds:        int(timeout.Seconds()),
		ProbeTimeoutSeconds:   int(probeTimeout.Seconds()),
		ResolveTimeoutSeconds: policies.ResolveTimeoutSeconds(),
	}
}

func (h *Hub) inflightCount(agentID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.running[agentID])
}

func (h *Hub) inflightLimit() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxInflight
}

func (h *Hub) currentPollInterval() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pollInterval <= 0 {
		return 2 * time.Second
	}
	return h.pollInterval
}

func (h *Hub) schedulerSnapshot() config.Scheduler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return config.Scheduler{
		AgentOfflineAfterSec: int(h.offlineAfter / time.Second),
		PollIntervalSec:      int(h.pollInterval / time.Second),
		MaxInflightPerAgent:  h.maxInflight,
	}
}

func (h *Hub) schedulerForAgent(ctx context.Context, agentID string) config.Scheduler {
	scheduler, _, _ := h.settingsForAgent(ctx, agentID)
	return scheduler
}

func (h *Hub) inflightLimitForAgent(ctx context.Context, agentID string) int {
	scheduler := h.schedulerForAgent(ctx, agentID)
	if scheduler.MaxInflightPerAgent <= 0 {
		return 4
	}
	return scheduler.MaxInflightPerAgent
}

func (h *Hub) pollIntervalForAgent(ctx context.Context, agentID string) time.Duration {
	scheduler := h.schedulerForAgent(ctx, agentID)
	if scheduler.PollIntervalSec <= 0 {
		return 2 * time.Second
	}
	return time.Duration(scheduler.PollIntervalSec) * time.Second
}

func (h *Hub) policiesSnapshot() policy.Set {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.policies
}

func (h *Hub) policiesForAgent(ctx context.Context, agentID string) policy.Set {
	_, runtime, toolPolicies := h.settingsForAgent(ctx, agentID)
	return h.policiesSnapshot().WithIntersection(toolPolicies, runtime)
}

func (h *Hub) policiesForLabels(labels []string) policy.Set {
	base := h.policiesSnapshot()
	runtime := base.Runtime()
	toolPolicies := h.toolPoliciesForLabels(labels)
	for _, cfg := range h.labelConfigsFor(labels) {
		if cfg.Runtime != nil {
			runtime = minRuntime(runtime, *cfg.Runtime)
		}
	}
	return base.WithIntersection(toolPolicies, runtime)
}

func (h *Hub) settingsForAgent(ctx context.Context, agentID string) (config.Scheduler, config.Runtime, map[string]config.Policy) {
	basePolicies := h.policiesSnapshot()
	runtime := basePolicies.Runtime()
	scheduler := h.schedulerSnapshot()
	labels := h.labelsForAgent(ctx, agentID)
	for _, cfg := range h.labelConfigsFor(labels) {
		if cfg.Runtime != nil {
			runtime = minRuntime(runtime, *cfg.Runtime)
		}
		if cfg.Scheduler != nil {
			scheduler = minScheduler(scheduler, *cfg.Scheduler)
		}
	}
	return scheduler, runtime, h.toolPoliciesForLabels(labels)
}

func (h *Hub) labelsForAgent(ctx context.Context, agentID string) []string {
	agents, err := h.agentsWithManagedLabels(ctx)
	if err != nil {
		return model.NormalizeAgentLabels(agentID, model.AgentTransportGRPC, nil)
	}
	for _, agent := range agents {
		if agent.ID == agentID {
			return agent.Labels
		}
	}
	if node, err := h.store.GetHTTPAgent(ctx, agentID); err == nil {
		return model.NormalizeAgentLabels(agentID, model.AgentTransportHTTP, node.Labels)
	}
	if cfg, err := h.store.GetAgentConfig(ctx, agentID); err == nil {
		return model.NormalizeAgentLabels(agentID, model.AgentTransportGRPC, cfg.Labels)
	}
	return model.NormalizeAgentLabels(agentID, model.AgentTransportGRPC, nil)
}

func (h *Hub) labelConfigsSnapshot() map[string]config.LabelConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return cloneLabelConfigs(h.labelConfigs)
}

func (h *Hub) labelConfigsFor(labels []string) []config.LabelConfig {
	configs := h.labelConfigsSnapshot()
	out := make([]config.LabelConfig, 0, len(labels))
	for _, label := range labels {
		if cfg, ok := configs[label]; ok {
			out = append(out, cfg)
		}
	}
	return out
}

func (h *Hub) toolPoliciesForLabels(labels []string) map[string]config.Policy {
	var merged map[string]config.Policy
	for _, cfg := range h.labelConfigsFor(labels) {
		if len(cfg.ToolPolicies) == 0 {
			continue
		}
		if merged == nil {
			merged = clonePolicies(cfg.ToolPolicies)
			continue
		}
		merged = intersectConfigPolicies(merged, cfg.ToolPolicies)
	}
	return merged
}

func (h *Hub) agentConfigDisabled(ctx context.Context, agentID string) bool {
	if node, err := h.store.GetHTTPAgent(ctx, agentID); err == nil {
		return !node.Enabled
	}
	cfg, err := h.store.GetAgentConfig(ctx, agentID)
	return err == nil && cfg.Disabled
}

func (h *Hub) offlineAfterSnapshot() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.offlineAfter <= 0 {
		return 90 * time.Second
	}
	return h.offlineAfter
}

func (h *Hub) offlineAfterForAgent(ctx context.Context, agentID string) time.Duration {
	scheduler := h.schedulerForAgent(ctx, agentID)
	if scheduler.AgentOfflineAfterSec <= 0 {
		return 90 * time.Second
	}
	return time.Duration(scheduler.AgentOfflineAfterSec) * time.Second
}

func (h *Hub) runningJobs(agentID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	jobs := make([]string, 0, len(h.running[agentID]))
	for jobID := range h.running[agentID] {
		jobs = append(jobs, jobID)
	}
	return jobs
}

func (h *Hub) markInflight(agentID string, jobID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.markInflightLocked(agentID, jobID)
}

func (h *Hub) markInflightLocked(agentID string, jobID string) {
	if h.running[agentID] == nil {
		h.running[agentID] = map[string]struct{}{}
	}
	h.running[agentID][jobID] = struct{}{}
}

func (h *Hub) clearInflight(agentID string, jobID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.running[agentID], jobID)
	delete(h.inflightCancels, jobID)
}

func (h *Hub) setInflightCancel(jobID string, cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cancel == nil {
		delete(h.inflightCancels, jobID)
		return
	}
	h.inflightCancels[jobID] = cancel
}

func (h *Hub) cancelInflight(jobID string) {
	h.mu.Lock()
	cancel := h.inflightCancels[jobID]
	delete(h.inflightCancels, jobID)
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *Hub) sendGRPCCancel(agentID string, jobID string) {
	h.mu.Lock()
	ch := h.grpcCancels[agentID]
	h.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- jobID:
	default:
		h.log.Warn("drop grpc cancel for busy agent cancel queue", "agent_id", agentID, "job_id", jobID)
	}
}

func (h *Hub) touchAgent(agentID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.meta[agentID]
	meta.lastSeen = time.Now().UTC()
	h.meta[agentID] = meta
}

func (h *Hub) agentStale(agentID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agentStaleLocked(agentID)
}

func (h *Hub) agentStaleAfter(agentID string, offlineAfter time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agentStaleAfterLocked(agentID, offlineAfter)
}

func (h *Hub) agentStaleLocked(agentID string) bool {
	offlineAfter := h.offlineAfter
	if offlineAfter <= 0 {
		offlineAfter = 90 * time.Second
	}
	return h.agentStaleAfterLocked(agentID, offlineAfter)
}

func (h *Hub) agentStaleAfterLocked(agentID string, offlineAfter time.Duration) bool {
	meta, ok := h.meta[agentID]
	if !ok {
		return true
	}
	if offlineAfter <= 0 {
		offlineAfter = 90 * time.Second
	}
	return time.Since(meta.lastSeen) > offlineAfter
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizedRegisterTokens(tokens []config.RegisterToken) []string {
	out := make([]string, 0, len(tokens))
	seen := map[string]struct{}{}
	for _, item := range tokens {
		token := strings.TrimSpace(item.Token)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func minRuntime(base config.Runtime, next config.Runtime) config.Runtime {
	config.DefaultRuntimeValues(&base)
	config.DefaultRuntimeValues(&next)
	base.Count = minPositive(base.Count, next.Count)
	base.MaxHops = minPositive(base.MaxHops, next.MaxHops)
	base.ProbeStepTimeoutSec = minPositive(base.ProbeStepTimeoutSec, next.ProbeStepTimeoutSec)
	base.MaxToolTimeoutSec = minPositive(base.MaxToolTimeoutSec, next.MaxToolTimeoutSec)
	base.HTTPTimeoutSec = minPositive(base.HTTPTimeoutSec, next.HTTPTimeoutSec)
	base.DNSTimeoutSec = minPositive(base.DNSTimeoutSec, next.DNSTimeoutSec)
	base.ResolveTimeoutSec = minPositive(base.ResolveTimeoutSec, next.ResolveTimeoutSec)
	base.HTTPInvokeAttempts = minPositive(base.HTTPInvokeAttempts, next.HTTPInvokeAttempts)
	base.HTTPMaxHealthIntervalSec = minPositive(base.HTTPMaxHealthIntervalSec, next.HTTPMaxHealthIntervalSec)
	return base
}

func minScheduler(base config.Scheduler, next config.Scheduler) config.Scheduler {
	config.DefaultScheduler(&base)
	config.DefaultScheduler(&next)
	base.AgentOfflineAfterSec = minPositive(base.AgentOfflineAfterSec, next.AgentOfflineAfterSec)
	base.MaxInflightPerAgent = minPositive(base.MaxInflightPerAgent, next.MaxInflightPerAgent)
	base.PollIntervalSec = minPositive(base.PollIntervalSec, next.PollIntervalSec)
	return base
}

func minPositive(left int, right int) int {
	if left <= 0 {
		return right
	}
	if right <= 0 {
		return left
	}
	if right < left {
		return right
	}
	return left
}

func intersectConfigPolicies(left map[string]config.Policy, right map[string]config.Policy) map[string]config.Policy {
	out := clonePolicies(left)
	for name, rightPolicy := range right {
		leftPolicy, ok := out[name]
		if !rightPolicy.Enabled {
			out[name] = clonePolicy(rightPolicy)
			continue
		}
		if !ok {
			out[name] = clonePolicy(rightPolicy)
			continue
		}
		if !leftPolicy.Enabled {
			out[name] = leftPolicy
			continue
		}
		if rightPolicy.AllowedArgs != nil {
			leftPolicy.AllowedArgs = policy.IntersectAllowedArgs(leftPolicy.AllowedArgs, rightPolicy.AllowedArgs)
		}
		out[name] = leftPolicy
	}
	return out
}

func clonePolicy(cfg config.Policy) config.Policy {
	if cfg.AllowedArgs != nil {
		cfg.AllowedArgs = cloneStringMap(cfg.AllowedArgs)
	}
	return cfg
}

func cloneLabelConfigs(configs map[string]config.LabelConfig) map[string]config.LabelConfig {
	if configs == nil {
		return nil
	}
	out := make(map[string]config.LabelConfig, len(configs))
	for label, cfg := range configs {
		if cfg.Runtime != nil {
			runtime := *cfg.Runtime
			cfg.Runtime = &runtime
		}
		if cfg.Scheduler != nil {
			scheduler := *cfg.Scheduler
			cfg.Scheduler = &scheduler
		}
		cfg.ToolPolicies = clonePolicies(cfg.ToolPolicies)
		out[label] = cfg
	}
	return out
}

func clonePolicies(policies map[string]config.Policy) map[string]config.Policy {
	if policies == nil {
		return nil
	}
	out := make(map[string]config.Policy, len(policies))
	for name, cfg := range policies {
		out[name] = clonePolicy(cfg)
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
