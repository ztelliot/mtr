package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/crontab"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

type Hub struct {
	store        store.Store
	policies     policy.Set
	labelConfigs map[string]config.LabelConfig
	log          *slog.Logger
	offlineAfter time.Duration
	pollInterval time.Duration
	maxInflight  int

	mu                             sync.Mutex
	outboundReloadMu               sync.Mutex
	agentLifecycleMu               sync.Mutex
	agentLifecycles                map[string]*agentLifecycleEntry
	meta                           map[string]agentMeta
	running                        map[string]map[string]struct{}
	grpcJobGenerations             map[string]uint64
	grpcResultBudgets              map[string]grpcResultBudget
	outboundJobGenerations         map[string]uint64
	outboundJobAttempts            map[string]uint64
	nextGRPCGeneration             uint64
	nextOutboundJobAttempt         uint64
	inflightCancels                map[string]context.CancelFunc
	grpcCancels                    map[string]chan string
	subs                           map[string]map[chan model.JobEvent]struct{}
	outboundAgents                 []OutboundAgent
	outboundAgentCancels           map[string]context.CancelFunc
	outboundAgentGenerations       map[string]uint64
	nextOutboundAgentGeneration    uint64
	outboundAgentMaxHealthInterval time.Duration
	outboundAgentInvokeAttempts    int
	grpcSendTimeout                time.Duration
}

type agentMeta struct {
	capabilities       []model.Tool
	protocols          model.ProtocolMask
	maxConcurrentJobs  int
	lastSeen           time.Time
	grpcGeneration     uint64
	grpcDone           chan struct{}
	outboundGeneration uint64
}

type agentLifecycleEntry struct {
	mu   sync.Mutex
	refs int
}

type grpcAgentSession struct {
	generation        uint64
	capabilities      []model.Tool
	protocols         model.ProtocolMask
	maxConcurrentJobs int
	done              <-chan struct{}
}

type grpcResultBudget struct {
	generation       uint64
	nonSummaryEvents int
	totalBytes       int
	exceeded         bool
}

const (
	timedOutJobScanLimit              = 200
	minGRPCHeartbeatInterval          = 5 * time.Second
	grpcAgentResultEventLimit         = 1024
	grpcAgentResultBytesLimit         = 4 << 20
	grpcAgentResultLimitExceededError = "agent_result_limit_exceeded"
	defaultGRPCSendTimeout            = 10 * time.Second
)

var errGRPCSendTimeout = errors.New("grpc server send timed out")
var errScheduledFanoutTargetLimit = errors.New("scheduled fanout target limit exceeded")

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
		store:                          st,
		policies:                       policies,
		log:                            log,
		offlineAfter:                   offlineAfter,
		pollInterval:                   pollInterval,
		maxInflight:                    maxInflight,
		meta:                           map[string]agentMeta{},
		running:                        map[string]map[string]struct{}{},
		grpcJobGenerations:             map[string]uint64{},
		grpcResultBudgets:              map[string]grpcResultBudget{},
		outboundJobGenerations:         map[string]uint64{},
		outboundJobAttempts:            map[string]uint64{},
		inflightCancels:                map[string]context.CancelFunc{},
		grpcCancels:                    map[string]chan string{},
		subs:                           map[string]map[chan model.JobEvent]struct{}{},
		agentLifecycles:                map[string]*agentLifecycleEntry{},
		outboundAgentCancels:           map[string]context.CancelFunc{},
		outboundAgentGenerations:       map[string]uint64{},
		outboundAgentMaxHealthInterval: defaultOutboundAgentMaxHealthInterval,
		outboundAgentInvokeAttempts:    defaultOutboundAgentInvokeAttempts,
		grpcSendTimeout:                defaultGRPCSendTimeout,
	}
}

func (h *Hub) lockAgentLifecycles(ids ...string) func() {
	unique := make(map[string]struct{}, len(ids))
	ordered := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	h.agentLifecycleMu.Lock()
	entries := make([]*agentLifecycleEntry, 0, len(ordered))
	for _, id := range ordered {
		entry := h.agentLifecycles[id]
		if entry == nil {
			entry = &agentLifecycleEntry{}
			h.agentLifecycles[id] = entry
		}
		entry.refs++
		entries = append(entries, entry)
	}
	h.agentLifecycleMu.Unlock()
	for _, entry := range entries {
		entry.mu.Lock()
	}
	return func() {
		for index := len(entries) - 1; index >= 0; index-- {
			entries[index].mu.Unlock()
		}
		h.agentLifecycleMu.Lock()
		for index, id := range ordered {
			entry := entries[index]
			entry.refs--
			if entry.refs == 0 && h.agentLifecycles[id] == entry {
				delete(h.agentLifecycles, id)
			}
		}
		h.agentLifecycleMu.Unlock()
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

func agentMetaFromHello(hello *grpcwire.AgentHello, lastSeen time.Time) agentMeta {
	return agentMeta{
		capabilities:      append([]model.Tool(nil), hello.Capabilities...),
		protocols:         hello.Protocols,
		maxConcurrentJobs: hello.MaxConcurrentJobs,
		lastSeen:          lastSeen,
	}
}

func (h *Hub) registerGRPCSession(agentID string, hello *grpcwire.AgentHello, lastSeen time.Time, cancelCh chan string) (grpcAgentSession, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var supersededJobs []string
	if previous, ok := h.meta[agentID]; ok && previous.grpcGeneration != 0 {
		if previous.grpcDone != nil {
			close(previous.grpcDone)
		}
		supersededJobs = h.takeGRPCRunningJobsLocked(agentID, previous.grpcGeneration)
	}
	h.nextGRPCGeneration++
	if h.nextGRPCGeneration == 0 {
		h.nextGRPCGeneration++
	}
	done := make(chan struct{})
	meta := agentMetaFromHello(hello, lastSeen)
	meta.grpcGeneration = h.nextGRPCGeneration
	meta.grpcDone = done
	h.meta[agentID] = meta
	if h.running[agentID] == nil {
		h.running[agentID] = map[string]struct{}{}
	}
	h.grpcCancels[agentID] = cancelCh
	return grpcAgentSession{
		generation:        meta.grpcGeneration,
		capabilities:      append([]model.Tool(nil), meta.capabilities...),
		protocols:         meta.protocols,
		maxConcurrentJobs: meta.maxConcurrentJobs,
		done:              done,
	}, supersededJobs
}

func (h *Hub) unregisterGRPCSession(agentID string, generation uint64) ([]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, ok := h.meta[agentID]
	if !ok || generation == 0 || meta.grpcGeneration != generation {
		return nil, false
	}
	if meta.grpcDone != nil {
		close(meta.grpcDone)
	}
	runningJobs := h.takeGRPCRunningJobsLocked(agentID, generation)
	delete(h.meta, agentID)
	if len(h.running[agentID]) == 0 {
		delete(h.running, agentID)
	}
	delete(h.grpcCancels, agentID)
	return runningJobs, true
}

// revokeGRPCSessionForDeletion is called while the per-agent lifecycle lock is
// held. Closing done makes both stream loops exit; the generation removal also
// makes any result already in flight fail its ownership check.
func (h *Hub) revokeGRPCSessionForDeletion(agentID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, ok := h.meta[agentID]
	if !ok || meta.grpcGeneration == 0 {
		return nil
	}
	if meta.grpcDone != nil {
		close(meta.grpcDone)
	}
	runningJobs := h.takeGRPCRunningJobsLocked(agentID, meta.grpcGeneration)
	delete(h.meta, agentID)
	if len(h.running[agentID]) == 0 {
		delete(h.running, agentID)
	}
	delete(h.grpcCancels, agentID)
	return runningJobs
}

func (h *Hub) takeGRPCRunningJobsLocked(agentID string, generation uint64) []string {
	jobs := make([]string, 0)
	for jobID := range h.running[agentID] {
		if h.grpcJobGenerations[jobID] != generation {
			continue
		}
		jobs = append(jobs, jobID)
		delete(h.running[agentID], jobID)
		delete(h.grpcJobGenerations, jobID)
		delete(h.grpcResultBudgets, jobID)
		delete(h.inflightCancels, jobID)
	}
	return jobs
}

func (h *Hub) isCurrentGRPCSession(agentID string, generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, ok := h.meta[agentID]
	return ok && generation != 0 && meta.grpcGeneration == generation
}

func (h *Hub) SetOutboundAgentRuntime(maxHealthInterval time.Duration, invokeAttempts int) {
	if maxHealthInterval <= 0 {
		maxHealthInterval = defaultOutboundAgentMaxHealthInterval
	}
	if invokeAttempts <= 0 {
		invokeAttempts = defaultOutboundAgentInvokeAttempts
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outboundAgentMaxHealthInterval = maxHealthInterval
	h.outboundAgentInvokeAttempts = invokeAttempts
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
	h.offlineAfter = time.Duration(globalScheduler.AgentOfflineAfterSec) * time.Second
	h.pollInterval = time.Duration(globalScheduler.PollIntervalSec) * time.Second
	h.maxInflight = globalScheduler.MaxInflightPerAgent
	h.outboundAgentMaxHealthInterval = time.Duration(globalRuntime.OutboundMaxHealthIntervalSec) * time.Second
	h.outboundAgentInvokeAttempts = globalRuntime.OutboundInvokeAttempts
}

func (h *Hub) Start(ctx context.Context) {
	h.startConfiguredOutboundAgents(ctx)
	go h.runOutboundAgentReconciler(ctx)
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
		nextRun, err := crontab.Next(sched.CronExpression, now)
		if err != nil {
			sched.Enabled = false
			sched.UpdatedAt = now
			if updateErr := h.store.UpdateScheduledJob(ctx, sched); updateErr != nil {
				h.log.Warn("disable scheduled job with invalid cron expression", "schedule_id", sched.ID, "cron_expression", sched.CronExpression, "err", updateErr)
			} else {
				h.log.Warn("disabled scheduled job with invalid cron expression", "schedule_id", sched.ID, "cron_expression", sched.CronExpression, "err", err)
			}
			continue
		}
		policies := h.policiesForLabels([]string{model.AgentAllLabel})
		if _, ok := policies.Get(sched.Tool); !ok {
			h.advanceScheduledJob(ctx, &sched, now, nextRun)
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
				h.advanceScheduledJob(ctx, &sched, now, nextRun)
				h.log.Warn("infer scheduled job target version", "schedule_id", sched.ID, "tool", sched.Tool, "err", err)
				continue
			}
			if literal && job.IPVersion == model.IPAny {
				job.IPVersion = literalVersion
			}
		} else if sched.Tool != model.ToolDNS {
			resolvedTarget, resolvedVersion, err := policies.ResolveTarget(ctx, sched.Tool, sched.Target, sched.IPVersion)
			if err != nil {
				h.advanceScheduledJob(ctx, &sched, now, nextRun)
				h.log.Warn("resolve scheduled job target", "schedule_id", sched.ID, "tool", sched.Tool, "err", err)
				continue
			}
			job.ResolvedTarget = resolvedTarget
			if job.IPVersion == model.IPAny && resolvedVersion != model.IPAny {
				job.IPVersion = resolvedVersion
			}
		}
		seenAgentIDs := map[string]struct{}{}
		runs := make([]model.Job, 0)
		var selectionErr error
	targets:
		for _, target := range sched.ScheduleTargets {
			agents, err := h.scheduledRunAgents(ctx, sched, target, job.IPVersion)
			if err != nil {
				selectionErr = err
				break
			}
			for _, agent := range agents {
				if _, ok := seenAgentIDs[agent.ID]; ok {
					continue
				}
				if len(runs) >= model.MaxFanoutTargets {
					selectionErr = fmt.Errorf("%w: narrow schedule targets to at most %d agents", errScheduledFanoutTargetLimit, model.MaxFanoutTargets)
					break targets
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
		if selectionErr != nil {
			if errors.Is(selectionErr, errScheduledFanoutTargetLimit) {
				h.advanceScheduledJob(ctx, &sched, now, nextRun)
			}
			h.log.Warn("skip scheduled fanout after target selection failed", "schedule_id", sched.ID, "tool", sched.Tool, "err", selectionErr)
			continue
		}
		if len(runs) == 0 {
			h.advanceScheduledJob(ctx, &sched, now, nextRun)
			h.log.Warn("skip scheduled fanout with no matching online agents", "schedule_id", sched.ID, "tool", sched.Tool)
			continue
		}
		if err := h.store.CreateJobs(ctx, runs); err != nil {
			h.log.Warn("create scheduled job runs", "schedule_id", sched.ID, "runs", len(runs), "err", err)
			continue
		}
		h.advanceScheduledJob(ctx, &sched, now, nextRun)
		h.log.Debug("scheduled job queued", "schedule_id", sched.ID, "runs", len(runs), "next_run_at", sched.NextRunAt)
	}
}

func (h *Hub) advanceScheduledJob(ctx context.Context, sched *model.ScheduledJob, now, nextRun time.Time) {
	sched.LastRunAt = &now
	sched.NextRunAt = nextRun
	sched.UpdatedAt = now
	if err := h.store.UpdateScheduledJobRun(ctx, *sched); err != nil {
		h.log.Warn("update scheduled job run", "schedule_id", sched.ID, "err", err)
	}
}

func (h *Hub) scheduledRunAgents(ctx context.Context, sched model.ScheduledJob, target model.ScheduleTarget, version model.IPVersion) ([]model.Agent, error) {
	agents, err := h.agentsWithManagedLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.Agent, 0, len(agents))
	for _, agent := range agents {
		disabled, err := h.agentConfigDisabled(ctx, agent.ID)
		if err != nil {
			return nil, err
		}
		if disabled {
			continue
		}
		if !scheduledTargetMatchesAgent(target, agent) {
			continue
		}
		if !target.AgentScope.Allows(agent) {
			continue
		}
		if !policy.AgentSupports(agent, sched.Tool, version) {
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
		if len(out) >= model.MaxFanoutTargets {
			return nil, fmt.Errorf("%w: narrow agent tags to at most %d agents", errScheduledFanoutTargetLimit, model.MaxFanoutTargets)
		}
		out = append(out, agent)
	}
	return out, nil
}

func scheduledTargetMatchesAgent(target model.ScheduleTarget, agent model.Agent) bool {
	for _, label := range agent.Labels {
		if label == target.Label {
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
	if err := validateAgentMessageShape(first); err != nil {
		return err
	}
	if first.Type != "hello" {
		return errors.New("first message must be hello")
	}
	hello := first.Agent
	if err := store.ValidateAgentID(hello.ID); err != nil {
		return err
	}
	agentID := hello.ID
	if err := validateAgentWireMetadata(hello.ProtocolVersion, hello.Capabilities, hello.Protocols, hello.MaxConcurrentJobs); err != nil {
		return err
	}
	agent := model.Agent{
		ID:           agentID,
		Country:      hello.Country,
		Region:       hello.Region,
		Provider:     hello.Provider,
		ISP:          hello.ISP,
		Version:      hello.Version,
		Capabilities: hello.Capabilities,
		Protocols:    hello.Protocols,
		Status:       model.AgentOnline,
		LastSeenAt:   time.Now().UTC(),
		CreatedAt:    time.Now().UTC(),
	}
	unlockAgent := h.lockAgentLifecycles(agent.ID)
	if _, err := h.store.GetOutboundAgent(stream.Context(), agent.ID); err == nil {
		unlockAgent()
		return errors.New("agent id is reserved by an outbound agent")
	} else if !errors.Is(err, store.ErrNotFound) {
		unlockAgent()
		return err
	}
	registered, created, err := h.store.RegisterAgent(stream.Context(), agent, hello.Token)
	if errors.Is(err, store.ErrInvalidRegisterToken) {
		unlockAgent()
		return errors.New("invalid agent token")
	}
	if err != nil {
		unlockAgent()
		return err
	}
	agent = registered

	cancelQueueSize := effectiveAgentInflightLimit(h.inflightLimit(), hello.MaxConcurrentJobs) * 2
	if cancelQueueSize < 1 {
		cancelQueueSize = 1
	}
	if cancelQueueSize > 128 {
		cancelQueueSize = 128
	}
	cancelCh := make(chan string, cancelQueueSize)
	session, supersededJobs := h.registerGRPCSession(agent.ID, hello, agent.LastSeenAt, cancelCh)
	for _, jobID := range supersededJobs {
		h.failJob(stream.Context(), agent.ID, jobID, "agent connection replaced")
	}
	h.log.Info("agent connected", "agent_id", agent.ID, "new_agent", created, "country", agent.Country, "region", agent.Region, "provider", agent.Provider, "isp", agent.ISP, "capabilities", hello.Capabilities, "protocols", hello.Protocols, "protocol_version", hello.ProtocolVersion)
	defer h.cleanupGRPCSessionAfterReturn(agent.ID, session.generation)
	if err := h.store.TouchAgent(stream.Context(), agent.ID); err != nil {
		unlockAgent()
		return err
	}
	h.touchGRPCSession(agent.ID, session.generation)
	unlockAgent()

	errCh := make(chan error, 2)
	go h.recvLoop(stream, agent.ID, session.generation, errCh)
	go h.sendLoop(stream, agent.ID, session, cancelCh, errCh)
	return <-errCh
}

// cleanupGRPCSessionAfterReturn must not wait for the lifecycle lock in the
// RPC handler's return path. A timed-out stream.Send is interrupted only after
// Connect returns and gRPC cancels the stream context; synchronous cleanup here
// would deadlock against the sender while it holds that same lifecycle lock.
func (h *Hub) cleanupGRPCSessionAfterReturn(agentID string, generation uint64) {
	go func() {
		unlockDisconnect := h.lockAgentLifecycles(agentID)
		defer unlockDisconnect()
		runningJobs, current := h.unregisterGRPCSession(agentID, generation)
		if !current {
			h.log.Debug("stale agent session disconnected", "agent_id", agentID, "generation", generation)
			return
		}
		cleanupCtx, cancelCleanup := newOutboundAgentCleanupContext()
		defer cancelCleanup()
		h.cleanupDisconnectedGRPCSession(cleanupCtx, agentID, runningJobs)
		h.log.Info("agent disconnected", "agent_id", agentID, "failed_jobs", len(runningJobs))
	}()
}

func (h *Hub) cleanupDisconnectedGRPCSession(ctx context.Context, agentID string, runningJobs []string) {
	if err := h.store.MarkAgentOffline(ctx, agentID); err != nil {
		h.log.Warn("mark disconnected agent offline failed", "agent_id", agentID, "err", err)
	}
	for _, jobID := range runningJobs {
		if ctx.Err() != nil {
			return
		}
		h.failJob(ctx, agentID, jobID, model.JobErrorAgentDisconnected)
	}
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
	outboundNodes, err := h.store.ListOutboundAgents(ctx)
	if err != nil {
		return nil, err
	}
	outboundLabels := make(map[string]config.OutboundAgent, len(outboundNodes))
	for _, node := range outboundNodes {
		outboundLabels[node.ID] = node
	}
	for i := range agents {
		labels := grpcLabels[agents[i].ID]
		transport := model.AgentTransportGRPC
		if node, ok := outboundLabels[agents[i].ID]; ok {
			labels = node.Labels
			transport = outboundModelTransport(node.Transport)
		}
		agents[i].Labels = model.NormalizeAgentLabels(agents[i].ID, transport, labels)
	}
	return agents, nil
}

func (h *Hub) recvLoop(stream grpcwire.Control_ConnectServer, agentID string, generation uint64, errCh chan<- error) {
	var lastHeartbeat time.Time
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
		if !h.isCurrentGRPCSession(agentID, generation) {
			errCh <- nil
			return
		}
		if err := validateAgentMessageShape(msg); err != nil {
			errCh <- err
			return
		}
		switch msg.Type {
		case "heartbeat":
			now := time.Now()
			if !lastHeartbeat.IsZero() && now.Sub(lastHeartbeat) < minGRPCHeartbeatInterval {
				errCh <- errors.New("agent heartbeat interval is below minimum")
				return
			}
			lastHeartbeat = now
			if !h.touchGRPCSession(agentID, generation) {
				errCh <- nil
				return
			}
			_ = h.store.TouchAgent(stream.Context(), agentID)
			h.log.Debug("agent heartbeat", "agent_id", agentID)
		case "result":
			if !h.ownsResultForGeneration(agentID, msg.Result.JobID, generation, 0, 0) {
				continue
			}
			h.handleAgentResultForSession(stream.Context(), agentID, generation, *msg.Result)
		default:
			errCh <- fmt.Errorf("unexpected agent message type %q", msg.Type)
			return
		}
	}
}

func validateAgentMessageShape(msg *grpcwire.AgentMessage) error {
	if msg == nil {
		return errors.New("agent message is required")
	}
	switch msg.Type {
	case "hello":
		if msg.Agent == nil || msg.Heartbeat != nil || msg.Result != nil {
			return errors.New("hello message must contain only agent payload")
		}
	case "heartbeat":
		if msg.Agent != nil || msg.Heartbeat == nil || msg.Result != nil {
			return errors.New("heartbeat message must contain only heartbeat payload")
		}
	case "result":
		if msg.Agent != nil || msg.Heartbeat != nil || msg.Result == nil {
			return errors.New("result message must contain only result payload")
		}
		if msg.Result.JobID == "" || msg.Result.JobID != strings.TrimSpace(msg.Result.JobID) || msg.Result.Event == nil {
			return errors.New("result message requires job_id and event")
		}
		typ, ok := msg.Result.Event["type"].(string)
		if !ok || typ == "" || typ != strings.TrimSpace(typ) {
			return errors.New("result event requires a non-empty string type")
		}
	default:
		return fmt.Errorf("unsupported agent message type %q", msg.Type)
	}
	return nil
}

func (h *Hub) sendLoop(stream grpcwire.Control_ConnectServer, agentID string, session grpcAgentSession, cancelCh <-chan string, errCh chan<- error) {
	ticker := time.NewTicker(h.currentPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			errCh <- stream.Context().Err()
			return
		case <-session.done:
			errCh <- nil
			return
		case jobID := <-cancelCh:
			if jobID == "" {
				continue
			}
			unlockAgent := h.lockAgentLifecycles(agentID)
			current := h.isCurrentGRPCSession(agentID, session.generation)
			var err error
			if current {
				err = h.sendGRPCServerMessage(stream, &grpcwire.ServerMessage{Type: "cancel", Cancel: &grpcwire.Cancel{JobID: jobID}}, errCh)
			}
			unlockAgent()
			if !current {
				errCh <- nil
				return
			}
			if err != nil {
				if errors.Is(err, errGRPCSendTimeout) {
					return
				}
				errCh <- err
				return
			}
		case <-ticker.C:
			pollInterval, current, err := h.dispatchGRPCJobsOnce(stream, agentID, session, errCh)
			ticker.Reset(pollInterval)
			if !current {
				errCh <- nil
				return
			}
			if err != nil {
				if errors.Is(err, errGRPCSendTimeout) {
					return
				}
				errCh <- err
				return
			}
		}
	}
}

// sendGRPCServerMessage keeps stream.Send single-threaded while arranging for
// Connect to return if HTTP/2 flow control stalls. Returning the RPC cancels
// the underlying stream context, which unblocks Send; callers continue holding
// the per-agent lifecycle lock until Send has actually returned, so a delete or
// replacement cannot race a late delivery.
func (h *Hub) sendGRPCServerMessage(stream grpcwire.Control_ConnectServer, message *grpcwire.ServerMessage, errCh chan<- error) error {
	timeout := h.grpcSendTimeout
	if timeout <= 0 {
		timeout = defaultGRPCSendTimeout
	}
	timer := time.NewTimer(timeout)
	sendFinished := make(chan struct{})
	watchdogFinished := make(chan bool, 1)
	go func() {
		select {
		case <-timer.C:
			select {
			case errCh <- errGRPCSendTimeout:
			default:
			}
			watchdogFinished <- true
		case <-sendFinished:
			watchdogFinished <- false
		}
	}()
	err := stream.Send(message)
	close(sendFinished)
	timedOut := <-watchdogFinished
	if !timer.Stop() && !timedOut {
		select {
		case <-timer.C:
		default:
		}
	}
	if timedOut {
		return errGRPCSendTimeout
	}
	return err
}

// dispatchGRPCJobsOnce serializes the complete claim-to-send transition with
// control-plane mutation for this Agent ID. In particular, DeleteManagedAgent
// cannot return while a previous generation is between claiming a job and
// completing stream.Send, and a deleted/replaced generation cannot send after
// the lifecycle lock has changed ownership.
func (h *Hub) dispatchGRPCJobsOnce(stream grpcwire.Control_ConnectServer, agentID string, session grpcAgentSession, errCh chan<- error) (time.Duration, bool, error) {
	pollInterval := h.currentPollInterval()
	unlockAgent := h.lockAgentLifecycles(agentID)
	defer unlockAgent()

	if !h.isCurrentGRPCSession(agentID, session.generation) {
		return pollInterval, false, nil
	}
	schedulerConfig, runtime, toolPolicies, err := h.settingsForAgent(stream.Context(), agentID)
	if err != nil {
		h.log.Warn("load agent settings for dispatch", "agent_id", agentID, "err", err)
		return pollInterval, true, nil
	}
	pollInterval = time.Duration(schedulerConfig.PollIntervalSec) * time.Second
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	disabled, err := h.agentConfigDisabled(stream.Context(), agentID)
	if err != nil {
		h.log.Warn("load agent disabled state for dispatch", "agent_id", agentID, "err", err)
		return pollInterval, true, nil
	}
	if disabled {
		_ = h.store.MarkAgentOffline(stream.Context(), agentID)
		return pollInterval, true, nil
	}
	offlineAfter := time.Duration(schedulerConfig.AgentOfflineAfterSec) * time.Second
	if offlineAfter <= 0 {
		offlineAfter = 90 * time.Second
	}
	if h.agentStaleAfter(agentID, offlineAfter) {
		_ = h.store.MarkAgentOffline(stream.Context(), agentID)
		h.log.Debug("skip stale agent dispatch", "agent_id", agentID, "offline_after", offlineAfter.String())
		return pollInterval, true, nil
	}
	configuredInflight := schedulerConfig.MaxInflightPerAgent
	if configuredInflight <= 0 {
		configuredInflight = 4
	}
	maxInflight := effectiveAgentInflightLimit(configuredInflight, session.maxConcurrentJobs)
	remaining := maxInflight - h.inflightCount(agentID)
	if remaining <= 0 {
		h.log.Debug("agent at inflight limit", "agent_id", agentID, "max_inflight", maxInflight, "transport", "grpc")
		return pollInterval, true, nil
	}
	policies := h.policiesSnapshot().WithIntersection(toolPolicies, runtime)
	jobs, err := h.store.ListQueuedJobs(stream.Context(), agentID, schedulerKnownTools(), model.ProtocolAll, remaining)
	if err != nil {
		h.log.Warn("list queued jobs", "err", err)
		return pollInterval, true, nil
	}
	for _, job := range jobs {
		p, failureType, err := compatibleQueuedJobPolicy(policies, session.capabilities, session.protocols, job)
		if err != nil {
			h.failJob(stream.Context(), agentID, job.ID, failureType)
			h.log.Warn("reject incompatible queued job", "agent_id", agentID, "job_id", job.ID, "tool", job.Tool, "failure_type", failureType, "err", err)
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
		if !h.markInflightForGRPCSession(agentID, job.ID, session.generation) {
			h.failJob(stream.Context(), agentID, job.ID, "agent connection replaced")
			return pollInterval, false, nil
		}
		h.emitStarted(stream.Context(), job)
		h.log.Debug("dispatch job", "agent_id", agentID, "job_id", job.ID, "tool", job.Tool, "ip_version", job.IPVersion)
		if err := h.sendGRPCServerMessage(stream, toServerJobWithPolicies(job, p, policies), errCh); err != nil {
			return pollInterval, true, err
		}
	}
	return pollInterval, true, nil
}

func schedulerKnownTools() []model.Tool {
	return []model.Tool{
		model.ToolPing,
		model.ToolTraceroute,
		model.ToolMTR,
		model.ToolHTTP,
		model.ToolDNS,
		model.ToolPort,
	}
}

const (
	maxAdvertisedAgentConcurrency = 128
)

func validateAgentWireMetadata(protocolVersion int, capabilities []model.Tool, protocols model.ProtocolMask, maxConcurrentJobs int) error {
	if protocolVersion != grpcwire.AgentProtocolVersion {
		return fmt.Errorf("unsupported agent protocol version %d (required %d)", protocolVersion, grpcwire.AgentProtocolVersion)
	}
	if len(capabilities) == 0 {
		return errors.New("agent capabilities are required")
	}
	knownTools := schedulerKnownTools()
	if len(capabilities) > len(knownTools) {
		return fmt.Errorf("too many agent capabilities: %d", len(capabilities))
	}
	known := make(map[model.Tool]struct{}, len(knownTools))
	for _, tool := range knownTools {
		known[tool] = struct{}{}
	}
	seen := make(map[model.Tool]struct{}, len(capabilities))
	for _, tool := range capabilities {
		if _, ok := known[tool]; !ok {
			return fmt.Errorf("unsupported agent capability %q", tool)
		}
		if _, duplicate := seen[tool]; duplicate {
			return fmt.Errorf("duplicate agent capability %q", tool)
		}
		seen[tool] = struct{}{}
	}
	if protocols == 0 || protocols&^model.ProtocolAll != 0 {
		return fmt.Errorf("invalid agent protocols mask %d", protocols)
	}
	if maxConcurrentJobs <= 0 || maxConcurrentJobs > maxAdvertisedAgentConcurrency {
		return fmt.Errorf("invalid agent max_concurrent_jobs %d", maxConcurrentJobs)
	}
	return nil
}

func effectiveAgentInflightLimit(configured int, advertised int) int {
	if configured <= 0 {
		return 0
	}
	if advertised <= 0 {
		return 0
	}
	if advertised > 0 && advertised < configured {
		return advertised
	}
	return configured
}

func compatibleQueuedJobPolicy(policies policy.Set, capabilities []model.Tool, protocols model.ProtocolMask, job model.Job) (policy.Policy, string, error) {
	hasTool := false
	for _, capability := range capabilities {
		if capability == job.Tool {
			hasTool = true
			break
		}
	}
	if !hasTool {
		return policy.Policy{}, model.JobErrorUnsupportedTool, errors.New(model.JobErrorUnsupportedTool)
	}
	if protocols == 0 || protocols&^model.ProtocolAll != 0 {
		return policy.Policy{}, model.JobErrorUnsupportedProtocol, errors.New(model.JobErrorUnsupportedProtocol)
	}
	if required := policy.RequiredProtocol(job.IPVersion); required != 0 && protocols&required == 0 {
		return policy.Policy{}, model.JobErrorUnsupportedProtocol, errors.New(model.JobErrorUnsupportedProtocol)
	}
	p, ok := policies.Get(job.Tool)
	if !ok {
		return policy.Policy{}, model.JobErrorUnsupportedTool, errors.New(model.JobErrorUnsupportedTool)
	}
	if err := policy.ValidateTarget(job.Tool, job.Target); err != nil {
		return policy.Policy{}, model.JobErrorTargetBlocked, err
	}
	validated, err := policies.ValidateTrusted(model.CreateJobRequest{
		Tool:           job.Tool,
		Target:         job.Target,
		Args:           job.Args,
		IPVersion:      job.IPVersion,
		AgentID:        job.AgentID,
		ResolveOnAgent: job.ResolveOnAgent,
	})
	if err == nil {
		return validated, "", nil
	}
	return policy.Policy{}, queuedJobPolicyFailureType(p, job), err
}

func queuedJobPolicyFailureType(p policy.Policy, job model.Job) string {
	effective := policy.EffectiveArgs(job.Tool, job.Args)
	key := ""
	switch job.Tool {
	case model.ToolTraceroute, model.ToolMTR:
		key = "protocol"
	case model.ToolHTTP:
		key = "http_version"
	}
	if key != "" {
		rule, ok := p.AllowedArgs[key]
		if !ok || !policy.AllowedArgPermits(job.Tool, key, rule, effective[key]) {
			return model.JobErrorUnsupportedProtocol
		}
	}
	return model.JobErrorUnsupportedArgument
}

func (h *Hub) handleResultForSession(ctx context.Context, agentID string, generation uint64, ev grpcwire.ResultEvent) {
	h.handleResultForGeneration(ctx, agentID, generation, 0, 0, ev)
}

func (h *Hub) handleOutboundAgentResult(ctx context.Context, agentID string, generation uint64, attempt uint64, ev grpcwire.ResultEvent) {
	h.handleResultForGeneration(ctx, agentID, 0, generation, attempt, ev)
}

func (h *Hub) handleResultForGeneration(ctx context.Context, agentID string, grpcGeneration uint64, outboundGeneration uint64, outboundAttempt uint64, ev grpcwire.ResultEvent) {
	if !h.ownsResultForGeneration(agentID, ev.JobID, grpcGeneration, outboundGeneration, outboundAttempt) {
		return
	}
	job, err := h.store.GetJob(ctx, ev.JobID)
	if err != nil {
		h.log.Warn("drop result for unknown job", "agent_id", agentID, "job_id", ev.JobID, "err", err)
		return
	}
	typ := eventType(ev.Event)
	if !h.agentOwnsJobForGeneration(agentID, job, grpcGeneration, outboundGeneration, outboundAttempt) {
		h.log.Warn("drop result for job not assigned to agent", "agent_id", agentID, "job_id", ev.JobID, "assigned_agent_id", job.AgentID, "event_type", typ)
		return
	}
	if terminalJobStatus(job.Status) {
		h.log.Debug("drop result for terminal job", "agent_id", agentID, "job_id", ev.JobID, "status", job.Status, "event_type", typ)
		return
	}
	eventJSON, err := json.Marshal(ev.Event)
	if err != nil {
		h.log.Warn("drop malformed job event", "agent_id", agentID, "job_id", ev.JobID, "event_type", typ, "err", err)
		return
	}
	// Serialize the in-memory generation/attempt check with same-ID lifecycle
	// replacement. The Store operation below atomically checks the persisted
	// running owner while inserting the event (and, for a summary, finishing
	// the job), closing the cancellation/result TOCTOU without persisting the
	// scheduler's short-lived attempt token.
	unlockAgent := h.lockAgentLifecycles(agentID)
	resultCtx, cancelResult := context.WithTimeout(ctx, outboundAgentCleanupTimeout)
	defer cancelResult()
	if grpcGeneration != 0 {
		accepted, exceeded := h.consumeGRPCResultBudget(agentID, ev.JobID, grpcGeneration, typ, len(eventJSON))
		if !accepted {
			unlockAgent()
			return
		}
		if exceeded {
			ownership, owned := h.takeInflightForGeneration(agentID, ev.JobID, grpcGeneration, 0, 0)
			if !owned {
				unlockAgent()
				return
			}
			cancelCh := h.grpcCancelChannelForSession(agentID, grpcGeneration)
			failed, failErr := h.store.FailActiveJob(resultCtx, ev.JobID, grpcAgentResultLimitExceededError)
			restored := false
			if failErr != nil {
				restored = h.restoreInflightForGeneration(agentID, ev.JobID, ownership)
			}
			unlockAgent()
			h.enqueueGRPCCancel(cancelCh, agentID, ev.JobID)
			if failErr != nil {
				h.log.Warn("fail job after grpc result limit exceeded", "agent_id", agentID, "job_id", ev.JobID, "ownership_restored", restored, "err", failErr)
				return
			}
			if !failed {
				h.log.Debug("skip result-limit failure for job no longer active", "agent_id", agentID, "job_id", ev.JobID)
				return
			}
			h.emitResultLimitFailure(ctx, job, agentID)
			return
		}
	} else if !h.agentOwnsJobForGeneration(agentID, job, 0, outboundGeneration, outboundAttempt) {
		unlockAgent()
		return
	}
	jobEvent, err := h.jobEventFromWire(job, typ, ev.Event)
	if err != nil {
		unlockAgent()
		h.log.Warn("drop malformed job event", "agent_id", agentID, "job_id", ev.JobID, "event_type", typ, "err", err)
		return
	}
	if jobEvent.ExitCode != nil && *jobEvent.ExitCode != 0 && parsedFailureType(jobEvent.Parsed) == model.JobErrorAgentBusy {
		h.requeueBusyJob(resultCtx, agentID, job, grpcGeneration, outboundGeneration, outboundAttempt)
		unlockAgent()
		return
	}
	var completedStatus model.JobStatus
	if jobEvent.ExitCode != nil {
		completedStatus = model.JobSucceeded
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
	if jobEvent.ExitCode == nil {
		if !h.agentOwnsJobForGeneration(agentID, job, grpcGeneration, outboundGeneration, outboundAttempt) {
			unlockAgent()
			h.log.Debug("drop nonterminal result after job ownership changed", "agent_id", agentID, "job_id", ev.JobID, "event_type", typ)
			return
		}
		stored, err := h.store.AddRunningJobEvent(resultCtx, ev.JobID, agentID, event)
		unlockAgent()
		if err != nil {
			h.log.Warn("store running job event failed", "agent_id", agentID, "job_id", ev.JobID, "event_type", typ, "err", err)
			return
		}
		if !stored {
			h.log.Debug("drop nonterminal result for job no longer running", "agent_id", agentID, "job_id", ev.JobID, "event_type", typ)
			return
		}
	} else {
		ownership, owned := h.takeInflightForGeneration(agentID, ev.JobID, grpcGeneration, outboundGeneration, outboundAttempt)
		if !owned {
			unlockAgent()
			h.log.Debug("drop terminal result after job ownership changed", "agent_id", agentID, "job_id", ev.JobID)
			return
		}
		msg := ""
		if *jobEvent.ExitCode != 0 {
			completedStatus = model.JobFailed
			msg = model.JobErrorToolFailed
			if failureType := parsedFailureType(jobEvent.Parsed); failureType != "" {
				msg = failureType
			}
		}
		completed, err := h.store.FinishRunningJobWithEvent(resultCtx, ev.JobID, agentID, completedStatus, msg, event)
		if err != nil {
			restored := h.restoreInflightForGeneration(agentID, ev.JobID, ownership)
			unlockAgent()
			h.log.Warn("finish job with terminal event failed", "agent_id", agentID, "job_id", ev.JobID, "status", completedStatus, "ownership_restored", restored, "err", err)
			return
		}
		unlockAgent()
		if !completed {
			h.log.Debug("drop terminal result for job no longer running", "agent_id", agentID, "job_id", ev.JobID)
			return
		}
	}
	h.publish(event)
	if jobEvent.ExitCode != nil {
		if job.ParentID != "" {
			h.completeParentIfDone(ctx, job.ParentID)
		} else {
			h.emitProgress(ctx, ev.JobID, progressMessageForStatus(completedStatus))
		}
		h.log.Debug("job completed", "agent_id", agentID, "job_id", ev.JobID, "status", completedStatus, "exit_code", *jobEvent.ExitCode)
	}
}

func (h *Hub) emitResultLimitFailure(ctx context.Context, job model.Job, agentID string) {
	eventJobID := job.ID
	if job.ParentID != "" {
		eventJobID = job.ParentID
	}
	h.emitFailureMessage(ctx, eventJobID, agentID, grpcAgentResultLimitExceededError)
	if job.ParentID != "" {
		h.completeParentIfDone(ctx, job.ParentID)
		return
	}
	h.emitProgress(ctx, job.ID, "failed")
}

func (h *Hub) consumeGRPCResultBudget(agentID string, jobID string, generation uint64, typ string, eventBytes int) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, current := h.meta[agentID]
	if !current || generation == 0 || meta.grpcGeneration != generation {
		return false, false
	}
	if _, running := h.running[agentID][jobID]; !running || h.grpcJobGenerations[jobID] != generation {
		return false, false
	}
	budget, ok := h.grpcResultBudgets[jobID]
	if !ok || budget.generation != generation {
		return false, false
	}
	if budget.exceeded {
		return true, true
	}
	exceeded := eventBytes < 0 || eventBytes > grpcAgentResultBytesLimit-budget.totalBytes
	if typ != "summary" && budget.nonSummaryEvents >= grpcAgentResultEventLimit {
		exceeded = true
	}
	if exceeded {
		budget.exceeded = true
		h.grpcResultBudgets[jobID] = budget
		return true, true
	}
	budget.totalBytes += eventBytes
	if typ != "summary" {
		budget.nonSummaryEvents++
	}
	h.grpcResultBudgets[jobID] = budget
	return true, false
}

func (h *Hub) ownsResultForGeneration(agentID string, jobID string, grpcGeneration uint64, outboundGeneration uint64, outboundAttempt uint64) bool {
	if agentID == "" || jobID == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.running[agentID][jobID]; !ok {
		return false
	}
	if grpcGeneration != 0 {
		if outboundGeneration != 0 || outboundAttempt != 0 {
			return false
		}
		meta, ok := h.meta[agentID]
		return ok && meta.grpcGeneration == grpcGeneration && h.grpcJobGenerations[jobID] == grpcGeneration
	}
	return outboundGeneration != 0 && outboundAttempt != 0 &&
		h.outboundAgentGenerations[agentID] == outboundGeneration &&
		h.outboundJobGenerations[jobID] == outboundGeneration &&
		h.outboundJobAttempts[jobID] == outboundAttempt
}

func (h *Hub) agentOwnsJobForGeneration(agentID string, job model.Job, grpcGeneration uint64, outboundGeneration uint64, outboundAttempt uint64) bool {
	if agentID == "" || job.AgentID != agentID {
		return false
	}
	return h.ownsResultForGeneration(agentID, job.ID, grpcGeneration, outboundGeneration, outboundAttempt)
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
		msg = model.JobErrorFanoutFailed
	}
	completed, err := h.store.FinishRunningJob(ctx, parentID, status, msg)
	if err != nil {
		h.log.Warn("update fanout parent status failed", "parent_job_id", parentID, "status", status, "err", err)
		return
	}
	if !completed {
		h.log.Debug("skip completing fanout parent no longer running", "parent_job_id", parentID, "status", status)
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
	job, err := h.store.GetJob(ctx, jobID)
	if err != nil {
		h.log.Warn("load failed job status failed", "agent_id", agentID, "job_id", jobID, "err", err)
		return
	}
	if terminalJobStatus(job.Status) {
		if agentID != "" {
			h.clearInflight(agentID, jobID)
		}
		h.log.Debug("skip failing terminal job", "agent_id", agentID, "job_id", jobID, "status", job.Status)
		return
	}
	failed, err := h.store.FailActiveJob(ctx, jobID, msg)
	if err != nil {
		h.log.Warn("mark job failed status failed", "agent_id", agentID, "job_id", jobID, "err", err)
		return
	}
	if !failed {
		if agentID != "" {
			h.clearInflight(agentID, jobID)
		}
		h.log.Debug("skip failing job no longer active", "agent_id", agentID, "job_id", jobID)
		return
	}
	if agentID != "" {
		h.clearInflight(agentID, jobID)
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

type inflightOwnership struct {
	grpcGeneration      uint64
	outboundGeneration  uint64
	outboundAttempt     uint64
	cancel              context.CancelFunc
	grpcResultBudget    grpcResultBudget
	hasGRPCResultBudget bool
}

func (h *Hub) requeueBusyJob(ctx context.Context, agentID string, job model.Job, grpcGeneration uint64, outboundGeneration uint64, outboundAttempt uint64) bool {
	ownership, ok := h.takeInflightForGeneration(agentID, job.ID, grpcGeneration, outboundGeneration, outboundAttempt)
	if !ok {
		h.log.Debug("drop busy result after job ownership changed", "agent_id", agentID, "job_id", job.ID)
		return true
	}
	requeued, err := h.store.RequeueRunningJob(ctx, job.ID)
	if err != nil {
		restored := h.restoreInflightForGeneration(agentID, job.ID, ownership)
		h.log.Warn("requeue busy agent job failed", "agent_id", agentID, "job_id", job.ID, "err", err)
		return !restored
	}
	if !requeued {
		h.log.Debug("skip requeue for job no longer running", "agent_id", agentID, "job_id", job.ID)
		return true
	}
	h.emitProgress(ctx, job.ID, "queued")
	h.log.Debug("requeued busy agent job", "agent_id", agentID, "job_id", job.ID)
	return true
}

func (h *Hub) takeInflightForGeneration(agentID string, jobID string, grpcGeneration uint64, outboundGeneration uint64, outboundAttempt uint64) (inflightOwnership, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.running[agentID][jobID]; !ok {
		return inflightOwnership{}, false
	}
	if grpcGeneration != 0 {
		if outboundGeneration != 0 || outboundAttempt != 0 || h.grpcJobGenerations[jobID] != grpcGeneration {
			return inflightOwnership{}, false
		}
	} else if outboundGeneration == 0 || outboundAttempt == 0 || h.outboundAgentGenerations[agentID] != outboundGeneration || h.outboundJobGenerations[jobID] != outboundGeneration || h.outboundJobAttempts[jobID] != outboundAttempt {
		return inflightOwnership{}, false
	}
	ownership := inflightOwnership{
		grpcGeneration:     grpcGeneration,
		outboundGeneration: outboundGeneration,
		outboundAttempt:    outboundAttempt,
		cancel:             h.inflightCancels[jobID],
	}
	if budget, ok := h.grpcResultBudgets[jobID]; ok {
		ownership.grpcResultBudget = budget
		ownership.hasGRPCResultBudget = true
	}
	delete(h.running[agentID], jobID)
	delete(h.grpcJobGenerations, jobID)
	delete(h.grpcResultBudgets, jobID)
	delete(h.outboundJobGenerations, jobID)
	delete(h.outboundJobAttempts, jobID)
	delete(h.inflightCancels, jobID)
	return ownership, true
}

func (h *Hub) restoreInflightForGeneration(agentID string, jobID string, ownership inflightOwnership) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.running[agentID][jobID]; exists {
		return false
	}
	if ownership.grpcGeneration != 0 {
		if ownership.outboundGeneration != 0 || ownership.outboundAttempt != 0 {
			return false
		}
		meta, ok := h.meta[agentID]
		if !ok || meta.grpcGeneration != ownership.grpcGeneration {
			return false
		}
	} else if ownership.outboundGeneration == 0 || ownership.outboundAttempt == 0 || h.outboundAgentGenerations[agentID] != ownership.outboundGeneration {
		return false
	}
	h.markInflightLocked(agentID, jobID)
	if ownership.grpcGeneration != 0 {
		h.grpcJobGenerations[jobID] = ownership.grpcGeneration
		if ownership.hasGRPCResultBudget && ownership.grpcResultBudget.generation == ownership.grpcGeneration {
			h.grpcResultBudgets[jobID] = ownership.grpcResultBudget
		}
	}
	if ownership.outboundGeneration != 0 {
		h.outboundJobGenerations[jobID] = ownership.outboundGeneration
		h.outboundJobAttempts[jobID] = ownership.outboundAttempt
	}
	if ownership.cancel != nil {
		h.inflightCancels[jobID] = ownership.cancel
	}
	return true
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
		var err error
		policies, err = h.policiesForAgent(ctx, job.AgentID)
		if err != nil {
			h.log.Warn("load agent policies for timeout sweep", "agent_id", job.AgentID, "job_id", job.ID, "err", err)
			return false
		}
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
		failed, err := h.store.FailActiveJob(ctx, child.ID, model.JobErrorTimeout)
		if err != nil {
			h.log.Warn("mark fanout child timed out failed", "parent_job_id", parent.ID, "child_job_id", child.ID, "agent_id", child.AgentID, "err", err)
		} else if !failed {
			h.log.Debug("skip fanout child no longer active", "parent_job_id", parent.ID, "child_job_id", child.ID, "agent_id", child.AgentID)
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

func (h *Hub) handleAgentResultForSession(ctx context.Context, agentID string, generation uint64, result grpcwire.AgentResult) {
	h.handleResultForSession(ctx, agentID, generation, grpcwire.ResultEvent{
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
	case model.JobErrorTargetBlocked, model.JobErrorUnsupportedProtocol, model.JobErrorUnsupportedTool, model.JobErrorAgentBusy:
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
		if !int64FitsInt(n) {
			return 0, false
		}
		return int(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0, false
		}
		limit := math.Ldexp(1, strconv.IntSize-1)
		if n < -limit || n >= limit {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || !int64FitsInt(i) {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func int64FitsInt(value int64) bool {
	if strconv.IntSize == 64 {
		return true
	}
	return value >= -1<<31 && value <= 1<<31-1
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
	args := policies.ServerArgs(job.Tool, job.Args)
	args = policy.ConstrainAutoArgs(job.Tool, args, p.AllowedArgs)
	return &grpcwire.JobSpec{
		ProtocolVersion:       grpcwire.AgentProtocolVersion,
		ID:                    job.ID,
		Tool:                  job.Tool,
		Target:                job.Target,
		ResolvedTarget:        job.ResolvedTarget,
		Args:                  args,
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

func (h *Hub) policiesSnapshot() policy.Set {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.policies
}

func (h *Hub) policiesForAgent(ctx context.Context, agentID string) (policy.Set, error) {
	_, runtime, toolPolicies, err := h.settingsForAgent(ctx, agentID)
	if err != nil {
		return policy.Set{}, err
	}
	return h.policiesSnapshot().WithIntersection(toolPolicies, runtime), nil
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

func (h *Hub) settingsForAgent(ctx context.Context, agentID string) (config.Scheduler, config.Runtime, map[string]config.Policy, error) {
	basePolicies := h.policiesSnapshot()
	runtime := basePolicies.Runtime()
	scheduler := h.schedulerSnapshot()
	labels, err := h.labelsForAgent(ctx, agentID)
	if err != nil {
		return config.Scheduler{}, config.Runtime{}, nil, err
	}
	for _, cfg := range h.labelConfigsFor(labels) {
		if cfg.Runtime != nil {
			runtime = minRuntime(runtime, *cfg.Runtime)
		}
		if cfg.Scheduler != nil {
			scheduler = minScheduler(scheduler, *cfg.Scheduler)
		}
	}
	return scheduler, runtime, h.toolPoliciesForLabels(labels), nil
}

func (h *Hub) labelsForAgent(ctx context.Context, agentID string) ([]string, error) {
	agents, err := h.agentsWithManagedLabels(ctx)
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		if agent.ID == agentID {
			return agent.Labels, nil
		}
	}
	node, err := h.store.GetOutboundAgent(ctx, agentID)
	if err == nil {
		return model.NormalizeAgentLabels(agentID, outboundModelTransport(node.Transport), node.Labels), nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	cfg, err := h.store.GetAgentConfig(ctx, agentID)
	if err == nil {
		return model.NormalizeAgentLabels(agentID, model.AgentTransportGRPC, cfg.Labels), nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return model.NormalizeAgentLabels(agentID, model.AgentTransportGRPC, nil), nil
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

func (h *Hub) agentConfigDisabled(ctx context.Context, agentID string) (bool, error) {
	node, err := h.store.GetOutboundAgent(ctx, agentID)
	if err == nil {
		return !node.Enabled, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return true, err
	}
	cfg, err := h.store.GetAgentConfig(ctx, agentID)
	if err == nil {
		return cfg.Disabled, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return true, err
}

func (h *Hub) offlineAfterSnapshot() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.offlineAfter <= 0 {
		return 90 * time.Second
	}
	return h.offlineAfter
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

func (h *Hub) markInflightForGRPCSession(agentID string, jobID string, generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, ok := h.meta[agentID]
	if !ok || generation == 0 || meta.grpcGeneration != generation {
		return false
	}
	h.markInflightLocked(agentID, jobID)
	h.grpcJobGenerations[jobID] = generation
	h.grpcResultBudgets[jobID] = grpcResultBudget{generation: generation}
	delete(h.outboundJobGenerations, jobID)
	delete(h.outboundJobAttempts, jobID)
	return true
}

func (h *Hub) markInflightForOutboundAgent(agentID string, jobID string, generation uint64) (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if generation == 0 || h.outboundAgentGenerations[agentID] != generation {
		return 0, false
	}
	h.nextOutboundJobAttempt++
	if h.nextOutboundJobAttempt == 0 {
		h.nextOutboundJobAttempt++
	}
	attempt := h.nextOutboundJobAttempt
	h.markInflightLocked(agentID, jobID)
	h.outboundJobGenerations[jobID] = generation
	h.outboundJobAttempts[jobID] = attempt
	delete(h.grpcJobGenerations, jobID)
	delete(h.grpcResultBudgets, jobID)
	return attempt, true
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
	delete(h.grpcJobGenerations, jobID)
	delete(h.grpcResultBudgets, jobID)
	delete(h.outboundJobGenerations, jobID)
	delete(h.outboundJobAttempts, jobID)
	delete(h.inflightCancels, jobID)
}

func (h *Hub) setInflightCancelForOutboundAttempt(jobID string, attempt uint64, cancel context.CancelFunc) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if attempt == 0 || h.outboundJobAttempts[jobID] != attempt {
		return false
	}
	if cancel == nil {
		delete(h.inflightCancels, jobID)
	} else {
		h.inflightCancels[jobID] = cancel
	}
	return true
}

func (h *Hub) outboundAgentOwnsAttempt(agentID string, jobID string, generation uint64, attempt uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if generation == 0 || attempt == 0 || h.outboundAgentGenerations[agentID] != generation {
		return false
	}
	if _, ok := h.running[agentID][jobID]; !ok {
		return false
	}
	return h.outboundJobGenerations[jobID] == generation && h.outboundJobAttempts[jobID] == attempt
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

func (h *Hub) grpcCancelChannelForSession(agentID string, generation uint64) chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, ok := h.meta[agentID]
	if !ok || generation == 0 || meta.grpcGeneration != generation {
		return nil
	}
	return h.grpcCancels[agentID]
}

func (h *Hub) enqueueGRPCCancel(ch chan string, agentID string, jobID string) {
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
	meta, ok := h.meta[agentID]
	if !ok {
		return
	}
	meta.lastSeen = time.Now().UTC()
	h.meta[agentID] = meta
}

func (h *Hub) touchGRPCSession(agentID string, generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, ok := h.meta[agentID]
	if !ok || generation == 0 || meta.grpcGeneration != generation {
		return false
	}
	meta.lastSeen = time.Now().UTC()
	h.meta[agentID] = meta
	return true
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
	base.OutboundInvokeAttempts = minPositive(base.OutboundInvokeAttempts, next.OutboundInvokeAttempts)
	base.OutboundMaxHealthIntervalSec = minPositive(base.OutboundMaxHealthIntervalSec, next.OutboundMaxHealthIntervalSec)
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
