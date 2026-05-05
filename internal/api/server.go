package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/scheduler"
	"github.com/ztelliot/mtr/internal/store"
	"github.com/ztelliot/mtr/internal/version"
)

const (
	defaultJSONBodyLimit      int64 = 1 << 20
	managedAgentBodyLimit     int64 = 64 << 10
	geoIPUpstreamBodyLimit    int64 = 64 << 10
	geoIPUpstreamBodyLimitPad int64 = 1 << 10
)

type Server struct {
	store        store.Store
	policies     policy.Set
	labelConfigs map[string]config.LabelConfig
	limiter      *abuse.Limiter
	hub          *scheduler.Hub
	rootCtx      context.Context
	tokens       map[string]TokenScope
	requireAuth  bool
	runtimeMu    sync.RWMutex
	geoIPURL     string
	log          *slog.Logger
	clientIP     clientIPResolver
}

type dispatchTarget struct {
	AgentID        string
	ResolvedTarget string
	IPVersion      model.IPVersion
}

type Options struct {
	TrustedProxies  []string
	ClientIPHeaders []string
	RootContext     context.Context
	RequireAuth     bool
	LabelConfigs    map[string]config.LabelConfig
}

var (
	errAgentNotFound = errors.New("agent not found")
	errAgentOffline  = errors.New("agent is offline")
)

func New(st store.Store, policies policy.Set, limiter *abuse.Limiter, hub *scheduler.Hub, geoIPURL string, log *slog.Logger, tokenConfigs ...TokenConfig) http.Handler {
	handler, err := NewWithOptions(st, policies, limiter, hub, geoIPURL, log, Options{}, tokenConfigs...)
	if err != nil {
		panic(err)
	}
	return handler
}

func NewWithOptions(st store.Store, policies policy.Set, limiter *abuse.Limiter, hub *scheduler.Hub, geoIPURL string, log *slog.Logger, opts Options, tokenConfigs ...TokenConfig) (http.Handler, error) {
	if log == nil {
		log = slog.Default()
	}
	resolver, err := newClientIPResolver(opts.TrustedProxies, opts.ClientIPHeaders)
	if err != nil {
		return nil, err
	}
	s := &Server{
		store:        st,
		policies:     policies,
		labelConfigs: config.NormalizeLabelConfigs(opts.LabelConfigs),
		limiter:      limiter,
		hub:          hub,
		rootCtx:      opts.RootContext,
		tokens:       map[string]TokenScope{},
		requireAuth:  opts.RequireAuth || len(tokenConfigs) > 0,
		geoIPURL:     strings.TrimSpace(geoIPURL),
		log:          log,
		clientIP:     resolver,
	}
	if s.rootCtx == nil {
		s.rootCtx = context.Background()
	}
	if len(opts.LabelConfigs) == 0 {
		if settings, err := st.GetManagedSettings(s.rootCtx); err == nil {
			s.labelConfigs = config.NormalizeLabelConfigs(settings.LabelConfigs)
		}
	}
	for _, config := range tokenConfigs {
		if config.Token != "" {
			s.tokens[config.Token] = normalizeTokenScope(config.Scope)
		}
	}
	r := chi.NewRouter()
	r.Get("/healthz", s.getHealth)
	r.Get("/v1/version", s.getVersion)
	if s.geoIPURL != "" {
		r.Get("/v1/geoip/{ip}", s.getGeoIP)
	}
	r.Group(func(r chi.Router) {
		r.Use(s.auth)
		r.Get("/v1/permissions", s.getPermissions)
		r.Post("/v1/jobs", s.createJob)
		r.Get("/v1/jobs/{id}", s.getJob)
		r.Get("/v1/jobs/{id}/events", s.listJobEvents)
		r.Get("/v1/jobs/{id}/stream", s.streamJobEvents)
		r.Post("/v1/schedules", s.createSchedule)
		r.Get("/v1/schedules", s.listSchedules)
		r.Get("/v1/schedules/{id}", s.getSchedule)
		r.Put("/v1/schedules/{id}", s.updateSchedule)
		r.Delete("/v1/schedules/{id}", s.deleteSchedule)
		r.Get("/v1/schedules/{id}/history", s.listScheduleHistory)
		r.Get("/v1/schedules/{id}/summary", s.listScheduleHistorySummary)
		r.Get("/v1/agents", s.listAgents)
		r.Get("/v1/manage/rate-limit", s.getManagedRateLimit)
		r.Put("/v1/manage/rate-limit", s.updateManagedRateLimit)
		r.Get("/v1/manage/labels", s.getManagedLabels)
		r.Put("/v1/manage/labels", s.updateManagedLabels)
		r.Put("/v1/manage/labels/state", s.updateManagedLabelState)
		r.Get("/v1/manage/tokens", s.listManagedAPITokens)
		r.Post("/v1/manage/tokens", s.createManagedAPIToken)
		r.Patch("/v1/manage/tokens/{id}", s.updateManagedAPIToken)
		r.Post("/v1/manage/tokens/{id}/rotate", s.rotateManagedAPIToken)
		r.Delete("/v1/manage/tokens/{id}", s.deleteManagedAPIToken)
		r.Get("/v1/manage/register-tokens", s.listManagedRegisterTokens)
		r.Post("/v1/manage/register-tokens", s.createManagedRegisterToken)
		r.Patch("/v1/manage/register-tokens/{id}", s.updateManagedRegisterToken)
		r.Post("/v1/manage/register-tokens/{id}/rotate", s.rotateManagedRegisterToken)
		r.Delete("/v1/manage/register-tokens/{id}", s.deleteManagedRegisterToken)
		r.Get("/v1/manage/agents", s.listManagedAgents)
		r.Post("/v1/manage/agents", s.createManagedAgent)
		r.Put("/v1/manage/agents/labels", s.updateManagedAgentLabels)
		r.Get("/v1/manage/agents/{id}", s.getManagedAgent)
		r.Put("/v1/manage/agents/{id}", s.updateManagedAgent)
		r.Delete("/v1/manage/agents/{id}", s.deleteManagedAgent)
	})
	return r, nil
}

func (s *Server) getHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version.Current()})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := s.clientIP.Resolve(r)
		s.log.Debug("http request", "method", r.Method, "path", r.URL.Path, "client_ip", clientIP, "remote_addr", r.RemoteAddr)
		if !s.limiterSnapshot().AllowRequest(clientIP) {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		tokens, requireAuth := s.tokenSnapshot()
		if len(tokens) > 0 {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if r.Header.Get("Authorization") != "" && !ok {
				writeError(w, http.StatusUnauthorized, "authorization header must use Bearer scheme")
				return
			}
			if token == "" {
				token = r.URL.Query().Get("access_token")
			}
			scope, ok := tokens[token]
			if !ok {
				writeError(w, http.StatusUnauthorized, "invalid api token")
				return
			}
			r = r.WithContext(withPrincipal(r.Context(), principal{token: token, scope: scope}))
		} else if requireAuth {
			writeError(w, http.StatusUnauthorized, "api token is required")
			return
		} else {
			r = r.WithContext(withPrincipal(r.Context(), principal{scope: allTokenScope}))
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) (string, bool) {
	if header == "" {
		return "", true
	}
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", false
	}
	return token, true
}

func decodeStrictJSON(body io.Reader, dst any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid json")
	}
	return nil
}

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeStrictJSON(http.MaxBytesReader(w, r.Body, defaultJSONBodyLimit), dst)
}

func (s *Server) getPermissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.permissionsForContext(r.Context()))
}

func (s *Server) policiesSnapshot() policy.Set {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.policies
}

func (s *Server) labelConfigsSnapshot() map[string]config.LabelConfig {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return cloneLabelConfigs(s.labelConfigs)
}

func (s *Server) policiesForJob(ctx context.Context, req model.CreateJobRequest) policy.Set {
	if req.AgentID != "" {
		return s.policiesForAgent(ctx, req.AgentID)
	}
	return s.policiesForLabels([]string{model.AgentAllLabel})
}

func (s *Server) policiesForAgent(ctx context.Context, agentID string) policy.Set {
	labels := model.NormalizeAgentLabels(agentID, model.AgentTransportGRPC, nil)
	if agents, err := s.agentsWithManagedLabels(ctx); err == nil {
		for _, agent := range agents {
			if agent.ID == agentID {
				labels = agent.Labels
				break
			}
		}
	}
	if len(labels) == 3 {
		if node, err := s.store.GetHTTPAgent(ctx, agentID); err == nil {
			labels = model.NormalizeAgentLabels(agentID, model.AgentTransportHTTP, node.Labels)
		} else if cfg, err := s.store.GetAgentConfig(ctx, agentID); err == nil {
			labels = model.NormalizeAgentLabels(agentID, model.AgentTransportGRPC, cfg.Labels)
		}
	}
	return s.policiesForLabels(labels)
}

func (s *Server) policiesForLabels(labels []string) policy.Set {
	base := s.policiesSnapshot()
	runtime := base.Runtime()
	labelConfigs := s.labelConfigsSnapshot()
	toolPolicies := toolPoliciesForLabels(labelConfigs, labels)
	for _, cfg := range labelConfigsForLabels(labelConfigs, labels) {
		if cfg.Runtime != nil {
			runtime = minRuntime(runtime, *cfg.Runtime)
		}
	}
	return base.WithIntersection(toolPolicies, runtime)
}

func (s *Server) limiterSnapshot() *abuse.Limiter {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.limiter
}

func (s *Server) tokenSnapshot() (map[string]TokenScope, bool) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	out := make(map[string]TokenScope, len(s.tokens))
	for token, scope := range s.tokens {
		out[token] = scope
	}
	return out, s.requireAuth
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Current())
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req model.CreateJobRequest
	if err := decodeLimitedJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !s.limiterSnapshot().AllowTool(string(req.Tool), s.clientIP.Resolve(r)) {
		writeError(w, http.StatusTooManyRequests, "tool rate limit exceeded")
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	policies := s.policiesForJob(r.Context(), req)
	req.Args = policies.ServerArgs(req.Tool, req.Args)
	_, err := policies.Validate(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.authorizeJob(r.Context(), req); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	options, err := s.dispatchOptions(r.Context(), req, policies)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	scope := principalFromContext(r.Context()).scope
	if req.AgentID == "" {
		targets, err := s.fanoutDispatchTargets(r.Context(), req, options, scope)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(targets) == 0 {
			writeError(w, http.StatusBadRequest, "no online agents support this job")
			return
		}
		parentResolvedTarget, parentIPVersion := parentDispatchTarget(targets)
		parent := model.Job{
			ID:             uuid.NewString(),
			Tool:           req.Tool,
			Target:         req.Target,
			ResolvedTarget: parentResolvedTarget,
			Args:           req.Args,
			IPVersion:      parentIPVersion,
			ResolveOnAgent: req.ResolveOnAgent,
			Status:         model.JobRunning,
			CreatedAt:      now,
			UpdatedAt:      now,
			StartedAt:      &now,
		}
		jobs := []model.Job{parent}
		for _, target := range targets {
			child := parent
			child.ID = uuid.NewString()
			child.ParentID = parent.ID
			child.AgentID = target.AgentID
			child.ResolvedTarget = target.ResolvedTarget
			child.IPVersion = target.IPVersion
			child.Status = model.JobQueued
			child.StartedAt = nil
			child.CompletedAt = nil
			child.Error = ""
			jobs = append(jobs, child)
		}
		if err := s.store.CreateJobs(r.Context(), jobs); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.publishJobProgress(r.Context(), parent.ID, "started")
		writeJSON(w, http.StatusAccepted, parent)
		return
	}
	target, failureType, err := s.pinnedDispatchTarget(r.Context(), req.AgentID, req.Tool, options)
	if err != nil {
		writeError(w, agentDispatchErrorStatus(err), err.Error())
		return
	}
	if failureType != "" {
		job := model.Job{
			ID:             uuid.NewString(),
			Tool:           req.Tool,
			Target:         req.Target,
			ResolvedTarget: target.ResolvedTarget,
			Args:           req.Args,
			IPVersion:      target.IPVersion,
			AgentID:        req.AgentID,
			ResolveOnAgent: req.ResolveOnAgent,
			Status:         model.JobFailed,
			CreatedAt:      now,
			UpdatedAt:      now,
			CompletedAt:    &now,
			Error:          failureType,
		}
		if err := s.store.CreateJob(r.Context(), job); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.publishJobMessage(r.Context(), job.ID, job.AgentID, failureType)
		writeJSON(w, http.StatusAccepted, job)
		return
	}
	job := model.Job{
		ID:             uuid.NewString(),
		Tool:           req.Tool,
		Target:         req.Target,
		ResolvedTarget: target.ResolvedTarget,
		Args:           req.Args,
		IPVersion:      target.IPVersion,
		AgentID:        req.AgentID,
		ResolveOnAgent: req.ResolveOnAgent,
		Status:         model.JobQueued,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateJob(r.Context(), job); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) fanoutDispatchTargets(ctx context.Context, req model.CreateJobRequest, options []dispatchTarget, scope TokenScope) ([]dispatchTarget, error) {
	agents, err := s.agentsWithManagedLabels(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]dispatchTarget, 0, len(agents))
	for _, agent := range agents {
		if !scopeAllowsAgentRecord(scope, agent) {
			continue
		}
		if s.agentDisabled(ctx, agent.ID) {
			continue
		}
		agentPolicies := s.policiesForLabels(agent.Labels)
		if _, err := agentPolicies.Validate(withAgentID(req, agent.ID)); err != nil {
			continue
		}
		if target, ok := selectDispatchTarget(agent, req.Tool, options, true); ok {
			target.AgentID = agent.ID
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func withAgentID(req model.CreateJobRequest, agentID string) model.CreateJobRequest {
	req.AgentID = agentID
	return req
}

func (s *Server) pinnedDispatchTarget(ctx context.Context, agentID string, tool model.Tool, options []dispatchTarget) (dispatchTarget, string, error) {
	fallback := dispatchTarget{AgentID: agentID}
	if len(options) > 0 {
		fallback = options[0]
		fallback.AgentID = agentID
	}
	agents, err := s.agentsWithManagedLabels(ctx)
	if err != nil {
		return dispatchTarget{}, "", err
	}
	for _, agent := range agents {
		if agent.ID != agentID {
			continue
		}
		if s.agentDisabled(ctx, agent.ID) {
			return fallback, "", errAgentOffline
		}
		agentPolicies := s.policiesForLabels(agent.Labels)
		if _, ok := agentPolicies.Get(tool); !ok {
			return fallback, model.JobErrorUnsupportedTool, nil
		}
		if agent.Status != model.AgentOnline {
			return fallback, "", errAgentOffline
		}
		target, ok, failureType := selectDispatchTargetWithFailure(agent, tool, options, false)
		if !ok {
			return fallback, failureType, nil
		}
		target.AgentID = agentID
		return target, "", nil
	}
	return fallback, "", errAgentNotFound
}

func agentDispatchErrorStatus(err error) int {
	if errors.Is(err, errAgentNotFound) || errors.Is(err, errAgentOffline) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) dispatchOptions(ctx context.Context, req model.CreateJobRequest, policies policy.Set) ([]dispatchTarget, error) {
	literalVersion, literal, err := policy.LiteralIPVersion(req.Tool, req.Target)
	if err != nil {
		return nil, err
	}
	if literal {
		if err := policy.ValidateLiteralTarget(ctx, req.Tool, req.Target, req.IPVersion); err != nil {
			return nil, err
		}
		target := dispatchTarget{IPVersion: literalVersion}
		if !req.ResolveOnAgent {
			resolvedTarget, _, err := policies.ResolveTarget(ctx, req.Tool, req.Target, literalVersion)
			if err != nil {
				return nil, err
			}
			target.ResolvedTarget = resolvedTarget
		}
		return []dispatchTarget{target}, nil
	}
	if req.ResolveOnAgent {
		if err := policy.ValidateLiteralTarget(ctx, req.Tool, req.Target, req.IPVersion); err != nil {
			return nil, err
		}
		return []dispatchTarget{{IPVersion: req.IPVersion}}, nil
	}
	if req.IPVersion != model.IPAny {
		resolvedTarget, resolvedVersion, err := policies.ResolveTarget(ctx, req.Tool, req.Target, req.IPVersion)
		if err != nil {
			return nil, err
		}
		return []dispatchTarget{{ResolvedTarget: resolvedTarget, IPVersion: resolvedVersion}}, nil
	}
	var options []dispatchTarget
	var lastErr error
	for _, version := range []model.IPVersion{model.IPv6, model.IPv4} {
		resolvedTarget, resolvedVersion, err := policies.ResolveTarget(ctx, req.Tool, req.Target, version)
		if err != nil {
			lastErr = err
			continue
		}
		options = append(options, dispatchTarget{ResolvedTarget: resolvedTarget, IPVersion: resolvedVersion})
	}
	if len(options) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("target resolved to no addresses")
	}
	return options, nil
}

func selectDispatchTarget(agent model.Agent, tool model.Tool, options []dispatchTarget, requireOnline bool) (dispatchTarget, bool) {
	target, ok, _ := selectDispatchTargetWithFailure(agent, tool, options, requireOnline)
	return target, ok
}

func selectDispatchTargetWithFailure(agent model.Agent, tool model.Tool, options []dispatchTarget, requireOnline bool) (dispatchTarget, bool, string) {
	if requireOnline && agent.Status != model.AgentOnline {
		return dispatchTarget{}, false, ""
	}
	hasTool := false
	for _, capability := range agent.Capabilities {
		if capability == tool {
			hasTool = true
			break
		}
	}
	if !hasTool {
		return dispatchTarget{}, false, model.JobErrorUnsupportedTool
	}
	protocols := agent.Protocols
	if protocols == 0 {
		protocols = model.ProtocolAll
	}
	for _, option := range options {
		required := policy.RequiredProtocol(option.IPVersion)
		if required == 0 || protocols&required != 0 {
			return option, true, ""
		}
	}
	return dispatchTarget{}, false, model.JobErrorUnsupportedProtocol
}

func parentDispatchTarget(targets []dispatchTarget) (string, model.IPVersion) {
	if len(targets) == 0 {
		return "", model.IPAny
	}
	resolvedTarget := targets[0].ResolvedTarget
	ipVersion := targets[0].IPVersion
	for _, target := range targets[1:] {
		if target.ResolvedTarget != resolvedTarget {
			resolvedTarget = ""
		}
		if target.IPVersion != ipVersion {
			ipVersion = model.IPAny
		}
	}
	return resolvedTarget, ipVersion
}

func (s *Server) publishJobProgress(ctx context.Context, jobID string, message string) {
	event := model.JobEvent{
		ID:        uuid.NewString(),
		JobID:     jobID,
		Stream:    "progress",
		Event:     &model.StreamEvent{Type: "progress", Message: message},
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.AddJobEvent(ctx, event); err != nil {
		s.log.Warn("store job progress event failed", "job_id", jobID, "message", message, "err", err)
		return
	}
	if s.hub != nil {
		s.hub.PublishEvent(event)
	}
}

func (s *Server) publishJobMessage(ctx context.Context, jobID string, agentID string, message string) {
	event := model.JobEvent{
		ID:        uuid.NewString(),
		JobID:     jobID,
		AgentID:   agentID,
		Stream:    "message",
		Event:     &model.StreamEvent{Type: "message", Message: message},
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.AddJobEvent(ctx, event); err != nil {
		s.log.Warn("store job message event failed", "job_id", jobID, "agent_id", agentID, "message", message, "err", err)
		return
	}
	if s.hub != nil {
		s.hub.PublishEvent(event)
	}
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) listJobEvents(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	if _, err := s.store.GetJob(r.Context(), jobID); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	events, err := s.store.ListJobEvents(r.Context(), jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, eventPayloads(events))
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.agentsWithManagedLabels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	scope := principalFromContext(r.Context()).scope
	filtered := make([]model.AgentView, 0, len(agents))
	for _, agent := range agents {
		if s.agentDisabled(r.Context(), agent.ID) {
			continue
		}
		if !scopeAllowsAgentRecord(scope, agent) {
			continue
		}
		tools := s.agentToolPermissions(agent, scope)
		if len(tools) == 0 {
			continue
		}
		filtered = append(filtered, agentView(agent, tools))
	}
	writeJSON(w, http.StatusOK, filtered)
}

func agentView(agent model.Agent, tools map[model.Tool]ToolPermission) model.AgentView {
	return model.AgentView{
		ID:         agent.ID,
		Country:    agent.Country,
		Region:     agent.Region,
		Provider:   agent.Provider,
		ISP:        agent.ISP,
		Version:    agent.Version,
		Labels:     append([]string(nil), agent.Labels...),
		Tools:      tools,
		Protocols:  agent.Protocols,
		Status:     agent.Status,
		LastSeenAt: agent.LastSeenAt,
		CreatedAt:  agent.CreatedAt,
	}
}

func (s *Server) agentDisabled(ctx context.Context, id string) bool {
	if node, err := s.store.GetHTTPAgent(ctx, id); err == nil {
		return !node.Enabled
	}
	cfg, err := s.store.GetAgentConfig(ctx, id)
	return err == nil && cfg.Disabled
}

func (s *Server) agentsWithManagedLabels(ctx context.Context) ([]model.Agent, error) {
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	configs, err := s.store.ListAgentConfigs(ctx)
	if err != nil {
		return nil, err
	}
	grpcLabels := make(map[string][]string, len(configs))
	for _, cfg := range configs {
		grpcLabels[cfg.ID] = cfg.Labels
	}
	httpNodes, err := s.store.ListHTTPAgents(ctx)
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

type ManagedAgent struct {
	model.Agent
	Transport string             `json:"transport"`
	Config    config.AgentConfig `json:"config"`
	HTTP      *config.HTTPAgent  `json:"http,omitempty"`
}

type managedAgentTransportPayload struct {
	Transport string `json:"transport,omitempty"`
}

func (s *Server) listManagedAgents(w http.ResponseWriter, r *http.Request) {
	if !s.manageReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "manage read access is required")
		return
	}
	out, err := s.managedAgents(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	redactManagedAgentsSecretsForReadOnly(r.Context(), out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getManagedAgent(w http.ResponseWriter, r *http.Request) {
	if !s.manageReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "manage read access is required")
		return
	}
	id := chi.URLParam(r, "id")
	agents, err := s.managedAgents(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, agent := range agents {
		if agent.ID == id {
			redactManagedAgentSecretsForReadOnly(r.Context(), &agent)
			writeJSON(w, http.StatusOK, agent)
			return
		}
	}
	writeError(w, http.StatusNotFound, store.ErrNotFound.Error())
}

func (s *Server) createManagedAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireManageWrite(w, r, "manage.http_agent.create", "") {
		return
	}
	raw, transport, ok := readManagedAgentBody(w, r)
	if !ok {
		return
	}
	if transport != "http" {
		writeError(w, http.StatusBadRequest, "transport must be http")
		return
	}
	s.writeHTTPAgent(w, r, "", raw)
}

func (s *Server) updateManagedAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.manageWriteAllowed(r.Context()) {
		action := "manage.agent.update"
		if transport, err := s.managedAgentTransportForID(r.Context(), id); err == nil {
			if transport == "http" {
				action = "manage.http_agent.update"
			} else {
				action = "manage.grpc_agent.update"
			}
		}
		s.auditManage(r.Context(), action, id, "deny", "manage write access is required")
		writeError(w, http.StatusForbidden, "manage write access is required")
		return
	}
	raw, transport, ok := readManagedAgentBody(w, r)
	if !ok {
		return
	}
	if transport == "" {
		var err error
		transport, err = s.managedAgentTransportForID(r.Context(), id)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
	}
	switch transport {
	case "http":
		s.writeHTTPAgent(w, r, id, raw)
	case "grpc":
		s.updateGRPCAgentConfig(w, r, raw)
	default:
		writeError(w, http.StatusBadRequest, "transport must be grpc or http")
	}
}

func readManagedAgentBody(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, managedAgentBodyLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, "", false
	}
	transport, err := managedAgentTransport(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return nil, "", false
	}
	return raw, transport, true
}

func managedAgentTransport(raw []byte) (string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", nil
	}
	var payload managedAgentTransportPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	transport := strings.ToLower(strings.TrimSpace(payload.Transport))
	switch transport {
	case "", "grpc", "http":
		return transport, nil
	default:
		return transport, nil
	}
}

func (s *Server) managedAgentTransportForID(ctx context.Context, id string) (string, error) {
	if _, err := s.store.GetHTTPAgent(ctx, id); err == nil {
		return "http", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	configs, err := s.store.ListAgentConfigs(ctx)
	if err != nil {
		return "", err
	}
	for _, cfg := range configs {
		if cfg.ID == id {
			return "grpc", nil
		}
	}
	liveAgents, err := s.store.ListAgents(ctx)
	if err != nil {
		return "", err
	}
	for _, agent := range liveAgents {
		if agent.ID == id {
			return "grpc", nil
		}
	}
	return "", store.ErrNotFound
}

func (s *Server) updateGRPCAgentConfig(w http.ResponseWriter, r *http.Request, raw []byte) {
	if !s.manageWriteAllowed(r.Context()) {
		s.auditManage(r.Context(), "manage.grpc_agent.update", chi.URLParam(r, "id"), "deny", "manage write access is required")
		writeError(w, http.StatusForbidden, "manage write access is required")
		return
	}
	var cfg config.AgentConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	cfg.ID = chi.URLParam(r, "id")
	if _, err := s.store.GetHTTPAgent(r.Context(), cfg.ID); err == nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("agent id %q is already used by an http agent", cfg.ID))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.UpsertAgentConfig(r.Context(), cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := s.store.GetAgentConfig(r.Context(), cfg.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.grpc_agent.update", cfg.ID, "allow", "")
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) updateManagedAgentLabels(w http.ResponseWriter, r *http.Request) {
	if !s.requireManageWrite(w, r, "manage.agent_labels.update", "agents") {
		return
	}
	var payload managedAgentLabelsPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	grpcUpdates, httpUpdates, status, err := s.prepareManagedAgentLabelUpdates(r.Context(), payload.Agents)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.store.UpdateManagedSettingsAndAgentLabels(r.Context(), settings, grpcUpdates, httpUpdates); err != nil {
		writeManagedSettingsError(w, err)
		return
	}
	if len(httpUpdates) > 0 && s.hub != nil {
		s.hub.ReloadHTTPAgents(s.rootCtx)
	}
	agents, err := s.managedAgents(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.agent_labels.update", "agents", "allow", "")
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) prepareManagedAgentLabelUpdates(ctx context.Context, patches []managedAgentLabelPatch) ([]config.AgentConfig, []config.HTTPAgent, int, error) {
	httpNodes, err := s.store.ListHTTPAgents(ctx)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	httpByID := make(map[string]config.HTTPAgent, len(httpNodes))
	for _, node := range httpNodes {
		httpByID[node.ID] = node
	}
	configs, err := s.store.ListAgentConfigs(ctx)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	configByID := make(map[string]config.AgentConfig, len(configs))
	for _, cfg := range configs {
		configByID[cfg.ID] = cfg
	}
	liveAgents, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	liveAgentIDs := make(map[string]struct{}, len(liveAgents))
	for _, agent := range liveAgents {
		liveAgentIDs[agent.ID] = struct{}{}
	}

	seen := map[string]struct{}{}
	grpcUpdates := make([]config.AgentConfig, 0, len(patches))
	httpUpdates := make([]config.HTTPAgent, 0, len(patches))
	for index, item := range patches {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return nil, nil, http.StatusBadRequest, fmt.Errorf("agents[%d].id is required", index)
		}
		if _, ok := seen[id]; ok {
			return nil, nil, http.StatusBadRequest, fmt.Errorf("agents[%d].id is duplicated", index)
		}
		seen[id] = struct{}{}
		if node, ok := httpByID[id]; ok {
			if _, conflict := configByID[id]; conflict {
				return nil, nil, http.StatusBadRequest, fmt.Errorf("agent id %q is ambiguous", id)
			}
			node.Labels = item.Labels
			httpUpdates = append(httpUpdates, node)
			continue
		}
		cfg, hasConfig := configByID[id]
		if !hasConfig {
			if _, ok := liveAgentIDs[id]; !ok {
				return nil, nil, http.StatusNotFound, fmt.Errorf("agent %q not found", id)
			}
			cfg = config.AgentConfig{ID: id}
		}
		cfg.Labels = item.Labels
		grpcUpdates = append(grpcUpdates, cfg)
	}
	return grpcUpdates, httpUpdates, http.StatusOK, nil
}

func (s *Server) ensureHTTPAgentIDAvailable(ctx context.Context, id string, allowExistingHTTP bool) (int, error) {
	httpNodes, err := s.store.ListHTTPAgents(ctx)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	for _, node := range httpNodes {
		if node.ID == id {
			if !allowExistingHTTP {
				return http.StatusBadRequest, fmt.Errorf("agent id %q is already used by an http agent", id)
			}
			return http.StatusOK, nil
		}
	}
	configs, err := s.store.ListAgentConfigs(ctx)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	for _, cfg := range configs {
		if cfg.ID == id {
			return http.StatusBadRequest, fmt.Errorf("agent id %q is already used by a grpc agent", id)
		}
	}
	liveAgents, err := s.store.ListAgents(ctx)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	for _, agent := range liveAgents {
		if agent.ID == id {
			return http.StatusBadRequest, fmt.Errorf("agent id %q is already used by a grpc agent", id)
		}
	}
	return http.StatusOK, nil
}

func (s *Server) deleteManagedAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	transport, err := s.managedAgentTransportForID(r.Context(), id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	action := "manage.grpc_agent.delete"
	if transport == "http" {
		action = "manage.http_agent.delete"
	}
	if !s.requireManageWrite(w, r, action, id) {
		return
	}
	if transport == "http" {
		if err := s.store.DeleteHTTPAgent(r.Context(), id); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}
		if s.hub != nil {
			s.hub.ReloadHTTPAgents(s.rootCtx)
		}
		s.auditManage(r.Context(), action, id, "allow", "")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.store.DeleteAgent(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			if cfgErr := s.store.DeleteAgentConfig(r.Context(), id); cfgErr == nil {
				s.auditManage(r.Context(), action, id, "allow", "")
				w.WriteHeader(http.StatusNoContent)
				return
			} else if errors.Is(cfgErr, store.ErrNotFound) {
				status = http.StatusNotFound
			} else {
				writeError(w, status, cfgErr.Error())
				return
			}
		}
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), action, id, "allow", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) managedAgents(ctx context.Context, transport string) ([]ManagedAgent, error) {
	configs, err := s.store.ListAgentConfigs(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]config.AgentConfig{}
	for _, cfg := range configs {
		byID[cfg.ID] = cfg
	}
	httpNodes, err := s.store.ListHTTPAgents(ctx)
	if err != nil {
		return nil, err
	}
	httpByID := map[string]config.HTTPAgent{}
	for _, node := range httpNodes {
		httpByID[node.ID] = node
	}
	out := []ManagedAgent{}
	if transport == "" || transport == "grpc" {
		agents, err := s.agentsWithManagedLabels(ctx)
		if err != nil {
			return nil, err
		}
		seen := map[string]struct{}{}
		for _, agent := range agents {
			if _, ok := httpByID[agent.ID]; ok {
				continue
			}
			seen[agent.ID] = struct{}{}
			out = append(out, ManagedAgent{Agent: agent, Transport: "grpc", Config: byID[agent.ID]})
		}
		for _, cfg := range configs {
			if _, ok := seen[cfg.ID]; ok {
				continue
			}
			if _, ok := httpByID[cfg.ID]; ok {
				continue
			}
			createdAt := parseManagedTime(cfg.CreatedAt)
			updatedAt := parseManagedTime(cfg.UpdatedAt)
			out = append(out, ManagedAgent{
				Agent: model.Agent{
					ID:         cfg.ID,
					Labels:     model.NormalizeAgentLabels(cfg.ID, model.AgentTransportGRPC, cfg.Labels),
					Status:     model.AgentOffline,
					LastSeenAt: updatedAt,
					CreatedAt:  createdAt,
				},
				Transport: "grpc",
				Config:    cfg,
			})
		}
	}
	if transport == "" || transport == "http" {
		agents, err := s.agentsWithManagedLabels(ctx)
		if err != nil {
			return nil, err
		}
		agentByID := map[string]model.Agent{}
		for _, agent := range agents {
			agentByID[agent.ID] = agent
		}
		for _, node := range httpNodes {
			createdAt := parseManagedTime(node.CreatedAt)
			updatedAt := parseManagedTime(node.UpdatedAt)
			agent := model.Agent{
				ID:         node.ID,
				Labels:     append([]string(nil), node.Labels...),
				Status:     model.AgentOffline,
				LastSeenAt: updatedAt,
				CreatedAt:  createdAt,
			}
			if live, ok := agentByID[node.ID]; ok {
				agent = live
			}
			agent.Labels = model.NormalizeAgentLabels(node.ID, model.AgentTransportHTTP, node.Labels)
			out = append(out, ManagedAgent{
				Agent:     agent,
				Transport: "http",
				Config: config.AgentConfig{
					ID:        node.ID,
					Disabled:  !node.Enabled,
					Labels:    append([]string(nil), node.Labels...),
					CreatedAt: node.CreatedAt,
					UpdatedAt: node.UpdatedAt,
				},
				HTTP: &node,
			})
		}
	}
	return out, nil
}

func redactManagedAgentSecretsForReadOnly(ctx context.Context, agents ...*ManagedAgent) {
	if effectiveManageAccess(principalFromContext(ctx).scope) == ScheduleAccessWrite {
		return
	}
	for index := range agents {
		if agents[index] != nil && agents[index].HTTP != nil {
			agents[index].HTTP.HTTPToken = ""
		}
	}
}

func redactManagedAgentsSecretsForReadOnly(ctx context.Context, agents []ManagedAgent) {
	if effectiveManageAccess(principalFromContext(ctx).scope) == ScheduleAccessWrite {
		return
	}
	for index := range agents {
		redactManagedAgentSecretsForReadOnly(ctx, &agents[index])
	}
}

func parseManagedTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

type managedLabelsPayload struct {
	Revision     int64                         `json:"revision,omitempty"`
	UpdatedAt    string                        `json:"updated_at,omitempty"`
	LabelConfigs map[string]config.LabelConfig `json:"label_configs,omitempty"`
}

type managedLabelsAndAgentsPayload struct {
	Revision     int64                         `json:"revision,omitempty"`
	LabelConfigs map[string]config.LabelConfig `json:"label_configs,omitempty"`
	Agents       []managedAgentLabelPatch      `json:"agents"`
}

type managedLabelsAndAgentsResponse struct {
	Revision     int64                         `json:"revision,omitempty"`
	UpdatedAt    string                        `json:"updated_at,omitempty"`
	LabelConfigs map[string]config.LabelConfig `json:"label_configs,omitempty"`
	Agents       []ManagedAgent                `json:"agents"`
}

type managedAgentLabelsPayload struct {
	Agents []managedAgentLabelPatch `json:"agents"`
}

type managedAgentLabelPatch struct {
	ID     string   `json:"id"`
	Labels []string `json:"labels"`
}

type managedRateLimitPayload struct {
	Revision  int64            `json:"revision,omitempty"`
	UpdatedAt string           `json:"updated_at,omitempty"`
	RateLimit config.RateLimit `json:"rate_limit"`
}

type managedAPITokenPayload struct {
	Name           string                         `json:"name,omitempty"`
	All            bool                           `json:"all,omitempty"`
	ScheduleAccess string                         `json:"schedule_access,omitempty"`
	ManageAccess   string                         `json:"manage_access,omitempty"`
	Agents         []string                       `json:"agents,omitempty"`
	DeniedAgents   []string                       `json:"denied_agents,omitempty"`
	AgentTags      []string                       `json:"agent_tags,omitempty"`
	DeniedTags     []string                       `json:"denied_tags,omitempty"`
	Tools          map[string]config.APIToolScope `json:"tools,omitempty"`
}

type managedAPITokenPatchPayload struct {
	Name           *string                         `json:"name,omitempty"`
	All            *bool                           `json:"all,omitempty"`
	ScheduleAccess *string                         `json:"schedule_access,omitempty"`
	ManageAccess   *string                         `json:"manage_access,omitempty"`
	Agents         *[]string                       `json:"agents,omitempty"`
	DeniedAgents   *[]string                       `json:"denied_agents,omitempty"`
	AgentTags      *[]string                       `json:"agent_tags,omitempty"`
	DeniedTags     *[]string                       `json:"denied_tags,omitempty"`
	Tools          *map[string]config.APIToolScope `json:"tools,omitempty"`
}

type managedRegisterTokenPayload struct {
	Name string `json:"name,omitempty"`
}

type managedAPITokensResponse struct {
	Revision  int64                       `json:"revision,omitempty"`
	UpdatedAt string                      `json:"updated_at,omitempty"`
	Tokens    []config.APITokenPermission `json:"tokens"`
}

type managedAPITokenResponse struct {
	Revision  int64                     `json:"revision,omitempty"`
	UpdatedAt string                    `json:"updated_at,omitempty"`
	Token     config.APITokenPermission `json:"token"`
}

type managedRegisterTokensResponse struct {
	Revision  int64                  `json:"revision,omitempty"`
	UpdatedAt string                 `json:"updated_at,omitempty"`
	Tokens    []config.RegisterToken `json:"tokens"`
}

type managedRegisterTokenResponse struct {
	Revision  int64                `json:"revision,omitempty"`
	UpdatedAt string               `json:"updated_at,omitempty"`
	Token     config.RegisterToken `json:"token"`
}

func (s *Server) getManagedRateLimit(w http.ResponseWriter, r *http.Request) {
	if !s.manageReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "manage read access is required")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, managedRateLimitPayload{Revision: settings.Revision, UpdatedAt: settings.UpdatedAt, RateLimit: settings.RateLimit})
}

func (s *Server) updateManagedRateLimit(w http.ResponseWriter, r *http.Request) {
	if !s.requireManageWrite(w, r, "manage.rate_limit.update", "rate-limit") {
		return
	}
	var payload managedRateLimitPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	current, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := current
	if payload.Revision > 0 {
		next.Revision = payload.Revision
	}
	next.RateLimit = payload.RateLimit
	saved, status, err := s.saveManagedSettings(r.Context(), next)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.rate_limit.update", "rate-limit", "allow", "")
	writeJSON(w, http.StatusOK, managedRateLimitPayload{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, RateLimit: saved.RateLimit})
}

func (s *Server) getManagedLabels(w http.ResponseWriter, r *http.Request) {
	if !s.manageReadAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "manage read access is required")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, managedLabelsPayload{Revision: settings.Revision, UpdatedAt: settings.UpdatedAt, LabelConfigs: settings.LabelConfigs})
}

func (s *Server) updateManagedLabels(w http.ResponseWriter, r *http.Request) {
	if !s.requireManageWrite(w, r, "manage.labels.update", "labels") {
		return
	}
	var payload managedLabelsPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	current, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := current
	if payload.Revision > 0 {
		next.Revision = payload.Revision
	}
	next.LabelConfigs = payload.LabelConfigs
	saved, status, err := s.saveManagedSettings(r.Context(), next)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.labels.update", "labels", "allow", "")
	writeJSON(w, http.StatusOK, managedLabelsPayload{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, LabelConfigs: saved.LabelConfigs})
}

func (s *Server) updateManagedLabelState(w http.ResponseWriter, r *http.Request) {
	if !s.requireManageWrite(w, r, "manage.labels.state.update", "labels") {
		return
	}
	var payload managedLabelsAndAgentsPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	current, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := current
	if payload.Revision > 0 {
		next.Revision = payload.Revision
	}
	next.LabelConfigs = payload.LabelConfigs
	if err := config.NormalizeManagedSettings(&next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	runtime, err := buildManagedRuntime(next)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	grpcUpdates, httpUpdates, status, err := s.prepareManagedAgentLabelUpdates(r.Context(), payload.Agents)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	saved, err := s.store.UpdateManagedSettingsAndAgentLabels(r.Context(), next, grpcUpdates, httpUpdates)
	if err != nil {
		writeManagedSettingsError(w, err)
		return
	}
	s.applyManagedRuntime(saved, runtime)
	if len(httpUpdates) > 0 && s.hub != nil {
		s.hub.ReloadHTTPAgents(s.rootCtx)
	}
	agents, err := s.managedAgents(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.labels.state.update", "labels", "allow", "")
	writeJSON(w, http.StatusOK, managedLabelsAndAgentsResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, LabelConfigs: saved.LabelConfigs, Agents: agents})
}

func (s *Server) listManagedAPITokens(w http.ResponseWriter, r *http.Request) {
	if !s.manageWriteAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "manage write access is required")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, managedAPITokensResponse{Revision: settings.Revision, UpdatedAt: settings.UpdatedAt, Tokens: redactAPITokens(settings.APITokens, -1)})
}

func (s *Server) createManagedAPIToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireManageWrite(w, r, "manage.tokens.create", "tokens") {
		return
	}
	var payload managedAPITokenPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	settings.APITokens = append(settings.APITokens, apiTokenFromPayload(payload, ""))
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	index := len(saved.APITokens) - 1
	s.auditManage(r.Context(), "manage.tokens.create", saved.APITokens[index].ID, "allow", "")
	writeJSON(w, http.StatusCreated, managedAPITokenResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Token: redactAPIToken(saved.APITokens[index], true)})
}

func (s *Server) updateManagedAPIToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireManageWrite(w, r, "manage.tokens.update", id) {
		return
	}
	var payload managedAPITokenPatchPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := managedAPITokenIndex(settings.APITokens, id)
	if index < 0 {
		writeError(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	settings.APITokens[index] = applyAPITokenPatch(settings.APITokens[index], payload)
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.tokens.update", id, "allow", "")
	writeJSON(w, http.StatusOK, managedAPITokenResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Token: redactAPIToken(saved.APITokens[index], false)})
}

func (s *Server) rotateManagedAPIToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireManageWrite(w, r, "manage.tokens.rotate", id) {
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := managedAPITokenIndex(settings.APITokens, id)
	if index < 0 {
		writeError(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	settings.APITokens[index].Secret = ""
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.tokens.rotate", id, "allow", "")
	writeJSON(w, http.StatusOK, managedAPITokenResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Token: redactAPIToken(saved.APITokens[index], true)})
}

func (s *Server) deleteManagedAPIToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireManageWrite(w, r, "manage.tokens.delete", id) {
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := managedAPITokenIndex(settings.APITokens, id)
	if index < 0 {
		writeError(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	settings.APITokens = append(settings.APITokens[:index], settings.APITokens[index+1:]...)
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.tokens.delete", id, "allow", "")
	writeJSON(w, http.StatusOK, managedAPITokensResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Tokens: redactAPITokens(saved.APITokens, -1)})
}

func (s *Server) listManagedRegisterTokens(w http.ResponseWriter, r *http.Request) {
	if !s.manageWriteAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "manage write access is required")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, managedRegisterTokensResponse{Revision: settings.Revision, UpdatedAt: settings.UpdatedAt, Tokens: redactRegisterTokens(settings.RegisterTokens, -1)})
}

func (s *Server) createManagedRegisterToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireManageWrite(w, r, "manage.register_tokens.create", "register-tokens") {
		return
	}
	var payload managedRegisterTokenPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	settings.RegisterTokens = append(settings.RegisterTokens, config.RegisterToken{Name: payload.Name})
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	index := len(saved.RegisterTokens) - 1
	s.auditManage(r.Context(), "manage.register_tokens.create", saved.RegisterTokens[index].ID, "allow", "")
	writeJSON(w, http.StatusCreated, managedRegisterTokenResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Token: redactRegisterToken(saved.RegisterTokens[index], true)})
}

func (s *Server) updateManagedRegisterToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireManageWrite(w, r, "manage.register_tokens.update", id) {
		return
	}
	var payload managedRegisterTokenPayload
	if err := decodeLimitedJSON(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := managedRegisterTokenIndex(settings.RegisterTokens, id)
	if index < 0 {
		writeError(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	settings.RegisterTokens[index].Name = payload.Name
	settings.RegisterTokens[index].Rotate = false
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.register_tokens.update", id, "allow", "")
	writeJSON(w, http.StatusOK, managedRegisterTokenResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Token: redactRegisterToken(saved.RegisterTokens[index], false)})
}

func (s *Server) rotateManagedRegisterToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireManageWrite(w, r, "manage.register_tokens.rotate", id) {
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := managedRegisterTokenIndex(settings.RegisterTokens, id)
	if index < 0 {
		writeError(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	settings.RegisterTokens[index].Token = ""
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.register_tokens.rotate", id, "allow", "")
	writeJSON(w, http.StatusOK, managedRegisterTokenResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Token: redactRegisterToken(saved.RegisterTokens[index], true)})
}

func (s *Server) deleteManagedRegisterToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireManageWrite(w, r, "manage.register_tokens.delete", id) {
		return
	}
	settings, err := s.store.GetManagedSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	index := managedRegisterTokenIndex(settings.RegisterTokens, id)
	if index < 0 {
		writeError(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	settings.RegisterTokens = append(settings.RegisterTokens[:index], settings.RegisterTokens[index+1:]...)
	saved, status, err := s.saveManagedSettings(r.Context(), settings)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.auditManage(r.Context(), "manage.register_tokens.delete", id, "allow", "")
	writeJSON(w, http.StatusOK, managedRegisterTokensResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt, Tokens: redactRegisterTokens(saved.RegisterTokens, -1)})
}

func managedAPITokenIndex(tokens []config.APITokenPermission, id string) int {
	id = strings.TrimSpace(id)
	for index, token := range tokens {
		if token.ID == id {
			return index
		}
	}
	return -1
}

func managedRegisterTokenIndex(tokens []config.RegisterToken, id string) int {
	id = strings.TrimSpace(id)
	for index, token := range tokens {
		if token.ID == id {
			return index
		}
	}
	return -1
}

func apiTokenFromPayload(payload managedAPITokenPayload, secret string) config.APITokenPermission {
	return config.APITokenPermission{
		Name:           payload.Name,
		Secret:         secret,
		All:            payload.All,
		ScheduleAccess: payload.ScheduleAccess,
		ManageAccess:   payload.ManageAccess,
		Agents:         append([]string(nil), payload.Agents...),
		DeniedAgents:   append([]string(nil), payload.DeniedAgents...),
		AgentTags:      append([]string(nil), payload.AgentTags...),
		DeniedTags:     append([]string(nil), payload.DeniedTags...),
		Tools:          cloneAPIToolScopes(payload.Tools),
	}
}

func applyAPITokenPatch(token config.APITokenPermission, payload managedAPITokenPatchPayload) config.APITokenPermission {
	if payload.Name != nil {
		token.Name = *payload.Name
	}
	if payload.All != nil {
		token.All = *payload.All
	}
	if payload.ScheduleAccess != nil {
		token.ScheduleAccess = *payload.ScheduleAccess
	}
	if payload.ManageAccess != nil {
		token.ManageAccess = *payload.ManageAccess
	}
	if payload.Agents != nil {
		token.Agents = append([]string(nil), (*payload.Agents)...)
	}
	if payload.DeniedAgents != nil {
		token.DeniedAgents = append([]string(nil), (*payload.DeniedAgents)...)
	}
	if payload.AgentTags != nil {
		token.AgentTags = append([]string(nil), (*payload.AgentTags)...)
	}
	if payload.DeniedTags != nil {
		token.DeniedTags = append([]string(nil), (*payload.DeniedTags)...)
	}
	if payload.Tools != nil {
		token.Tools = cloneAPIToolScopes(*payload.Tools)
	}
	return token
}

func cloneAPIToolScopes(in map[string]config.APIToolScope) map[string]config.APIToolScope {
	if in == nil {
		return nil
	}
	out := make(map[string]config.APIToolScope, len(in))
	for tool, scope := range in {
		if scope.AllowedArgs != nil {
			scope.AllowedArgs = cloneStringMap(scope.AllowedArgs)
		}
		if scope.IPVersions != nil {
			scope.IPVersions = append([]int(nil), scope.IPVersions...)
		}
		out[tool] = scope
	}
	return out
}

func redactAPITokens(tokens []config.APITokenPermission, revealIndex int) []config.APITokenPermission {
	out := make([]config.APITokenPermission, len(tokens))
	for i, token := range tokens {
		out[i] = redactAPIToken(token, i == revealIndex)
	}
	return out
}

func redactAPIToken(token config.APITokenPermission, reveal bool) config.APITokenPermission {
	token.Rotate = false
	if !reveal {
		token.Secret = ""
	}
	return token
}

func redactRegisterTokens(tokens []config.RegisterToken, revealIndex int) []config.RegisterToken {
	out := make([]config.RegisterToken, len(tokens))
	for i, token := range tokens {
		out[i] = redactRegisterToken(token, i == revealIndex)
	}
	return out
}

func redactRegisterToken(token config.RegisterToken, reveal bool) config.RegisterToken {
	token.Rotate = false
	if !reveal {
		token.Token = ""
	}
	return token
}

func (s *Server) saveManagedSettings(ctx context.Context, settings config.ManagedSettings) (config.ManagedSettings, int, error) {
	if err := config.NormalizeManagedSettings(&settings); err != nil {
		return settings, http.StatusBadRequest, err
	}
	runtime, err := buildManagedRuntime(settings)
	if err != nil {
		return settings, http.StatusBadRequest, err
	}
	saved, err := s.store.UpdateManagedSettings(ctx, settings)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return settings, http.StatusConflict, err
		}
		return settings, http.StatusInternalServerError, err
	}
	s.applyManagedRuntime(saved, runtime)
	return saved, http.StatusOK, nil
}

func writeManagedSettingsError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func managedSecretHash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Server) applyManagedSettings(settings config.ManagedSettings) error {
	runtime, err := buildManagedRuntime(settings)
	if err != nil {
		return err
	}
	s.applyManagedRuntime(settings, runtime)
	return nil
}

type managedRuntime struct {
	policies     policy.Set
	labelConfigs map[string]config.LabelConfig
	limiter      *abuse.Limiter
	tokens       map[string]TokenScope
}

func buildManagedRuntime(settings config.ManagedSettings) (managedRuntime, error) {
	if len(settings.APITokens) == 0 {
		return managedRuntime{}, errors.New("api_tokens must contain at least one token")
	}
	limiter, err := abuse.NewConfiguredLimiterWithError(abuse.RateLimitConfig{
		Global: abuse.Limit{
			RequestsPerMinute: settings.RateLimit.Global.RequestsPerMinute,
			Burst:             settings.RateLimit.Global.Burst,
		},
		IP: abuse.Limit{
			RequestsPerMinute: settings.RateLimit.IP.RequestsPerMinute,
			Burst:             settings.RateLimit.IP.Burst,
		},
		CIDR: abuse.CIDRLimit{
			Limit: abuse.Limit{
				RequestsPerMinute: settings.RateLimit.CIDR.RequestsPerMinute,
				Burst:             settings.RateLimit.CIDR.Burst,
			},
			IPv4Prefix: settings.RateLimit.CIDR.IPv4Prefix,
			IPv6Prefix: settings.RateLimit.CIDR.IPv6Prefix,
		},
		GeoIP: abuse.Limit{
			RequestsPerMinute: settings.RateLimit.GeoIP.RequestsPerMinute,
			Burst:             settings.RateLimit.GeoIP.Burst,
		},
		Tools:       AbuseToolLimits(settings.RateLimit.Tools),
		ExemptCIDRs: settings.RateLimit.ExemptCIDRs,
	})
	if err != nil {
		return managedRuntime{}, err
	}
	labelConfigs := config.NormalizeLabelConfigs(settings.LabelConfigs)
	tokens := map[string]TokenScope{}
	for _, config := range TokenConfigsFromPermissions(settings.APITokens) {
		if config.Token != "" {
			tokens[config.Token] = normalizeTokenScope(config.Scope)
		}
	}
	return managedRuntime{policies: policy.DefaultPolicies(), labelConfigs: labelConfigs, limiter: limiter, tokens: tokens}, nil
}

func (s *Server) applyManagedRuntime(settings config.ManagedSettings, runtime managedRuntime) {
	s.runtimeMu.Lock()
	s.policies = runtime.policies
	s.labelConfigs = runtime.labelConfigs
	s.limiter = runtime.limiter
	s.tokens = runtime.tokens
	s.requireAuth = true
	s.runtimeMu.Unlock()
	if s.hub != nil {
		s.hub.ApplySettings(settings)
	}
}

func TokenConfigsFromPermissions(in []config.APITokenPermission) []TokenConfig {
	out := make([]TokenConfig, 0, len(in))
	for _, scope := range in {
		tools := make(map[model.Tool]ToolScope, len(scope.Tools))
		for tool, toolScope := range scope.Tools {
			versions := make([]model.IPVersion, 0, len(toolScope.IPVersions))
			for _, version := range toolScope.IPVersions {
				versions = append(versions, model.IPVersion(version))
			}
			tools[model.Tool(tool)] = ToolScope{
				AllowedArgs:    toolScope.AllowedArgs,
				ResolveOnAgent: toolScope.ResolveOnAgent,
				IPVersions:     versions,
			}
		}
		out = append(out, TokenConfig{
			Token: scope.Secret,
			Scope: TokenScope{
				All:            scope.All,
				ScheduleAccess: ScheduleAccess(scope.ScheduleAccess),
				ManageAccess:   ScheduleAccess(scope.ManageAccess),
				Agents:         scope.Agents,
				DeniedAgents:   scope.DeniedAgents,
				AgentTags:      scope.AgentTags,
				DeniedTags:     scope.DeniedTags,
				Tools:          tools,
			},
		})
	}
	return out
}

func AbuseToolLimits(in map[string]config.ToolLimitSpec) map[string]abuse.ToolLimit {
	out := make(map[string]abuse.ToolLimit, len(in))
	for tool, limit := range in {
		out[tool] = abuse.ToolLimit{
			Global: abuse.Limit{
				RequestsPerMinute: limit.Global.RequestsPerMinute,
				Burst:             limit.Global.Burst,
			},
			CIDR: abuse.Limit{
				RequestsPerMinute: limit.CIDR.RequestsPerMinute,
				Burst:             limit.CIDR.Burst,
			},
			IP: abuse.Limit{
				RequestsPerMinute: limit.IP.RequestsPerMinute,
				Burst:             limit.IP.Burst,
			},
		}
	}
	return out
}

func (s *Server) writeHTTPAgent(w http.ResponseWriter, r *http.Request, id string, raw []byte) {
	action := "manage.http_agent.create"
	if id != "" {
		action = "manage.http_agent.update"
	}
	if !s.manageWriteAllowed(r.Context()) {
		s.auditManage(r.Context(), action, id, "deny", "manage write access is required")
		writeError(w, http.StatusForbidden, "manage write access is required")
		return
	}
	var node config.HTTPAgent
	if err := json.Unmarshal(raw, &node); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if id != "" {
		node.ID = id
	}
	if status, err := s.ensureHTTPAgentIDAvailable(r.Context(), node.ID, id != ""); err != nil {
		writeError(w, status, err.Error())
		return
	}
	if err := s.store.UpsertHTTPAgent(r.Context(), node); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.hub != nil {
		s.hub.ReloadHTTPAgents(s.rootCtx)
	}
	saved, err := s.store.GetHTTPAgent(r.Context(), node.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditManage(r.Context(), action, saved.ID, "allow", "")
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) requireManageWrite(w http.ResponseWriter, r *http.Request, action, target string) bool {
	if s.manageWriteAllowed(r.Context()) {
		return true
	}
	s.auditManage(r.Context(), action, target, "deny", "manage write access is required")
	writeError(w, http.StatusForbidden, "manage write access is required")
	return false
}

func (s *Server) auditManage(ctx context.Context, action, target, decision, reason string) {
	if s == nil || s.store == nil {
		return
	}
	event := store.AuditEvent{
		Subject:   auditSubject(ctx),
		Action:    action,
		Target:    target,
		Decision:  decision,
		Reason:    reason,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.AddAuditEvent(ctx, event); err != nil {
		s.log.Warn("failed to write audit event", "action", action, "target", target, "error", err)
	}
}

func auditSubject(ctx context.Context) string {
	token := principalFromContext(ctx).token
	if token == "" {
		return "anonymous"
	}
	return "token:" + managedSecretHash(token)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type clientIPResolver struct {
	trustedProxies  []netip.Prefix
	clientIPHeaders []string
}

func newClientIPResolver(trustedProxies []string, clientIPHeaders []string) (clientIPResolver, error) {
	resolver := clientIPResolver{
		clientIPHeaders: normalizeClientIPHeaders(clientIPHeaders),
	}
	for _, raw := range trustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := parseTrustedProxy(raw)
		if err != nil {
			return clientIPResolver{}, fmt.Errorf("invalid trusted proxy %q: %w", raw, err)
		}
		resolver.trustedProxies = append(resolver.trustedProxies, prefix)
	}
	return resolver, nil
}

func normalizeClientIPHeaders(headers []string) []string {
	seen := map[string]struct{}{}
	normalized := make([]string, 0, len(headers))
	for _, raw := range headers {
		header := http.CanonicalHeaderKey(strings.TrimSpace(raw))
		if header == "" {
			continue
		}
		key := strings.ToLower(header)
		if key == "x-real-ip" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, header)
	}
	return normalized
}

func parseTrustedProxy(raw string) (netip.Prefix, error) {
	if strings.Contains(raw, "/") {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return netip.Prefix{}, err
		}
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func (r clientIPResolver) Resolve(req *http.Request) string {
	if ip, ok := r.configuredHeaderClientIP(req.Header); ok {
		return ip.String()
	}
	remote, ok := requestRemoteIP(req.RemoteAddr)
	if !ok {
		return req.RemoteAddr
	}
	if !r.isTrustedProxy(remote) {
		return remote.String()
	}
	if ip, ok := r.forwardedForClientIP(req.Header.Values("X-Forwarded-For")); ok {
		return ip.String()
	}
	if ip, ok := headerIP(req.Header.Get("X-Real-IP")); ok {
		return ip.String()
	}
	return remote.String()
}

func (r clientIPResolver) configuredHeaderClientIP(header http.Header) (netip.Addr, bool) {
	for _, name := range r.clientIPHeaders {
		if ip, ok := headerIP(header.Get(name)); ok {
			return ip, true
		}
	}
	return netip.Addr{}, false
}

func (r clientIPResolver) forwardedForClientIP(values []string) (netip.Addr, bool) {
	var ips []netip.Addr
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			ip, ok := headerIP(part)
			if ok {
				ips = append(ips, ip)
			}
		}
	}
	if len(ips) == 0 {
		return netip.Addr{}, false
	}
	for i := len(ips) - 1; i >= 0; i-- {
		if !r.isTrustedProxy(ips[i]) {
			return ips[i], true
		}
	}
	return ips[0], true
}

func (r clientIPResolver) isTrustedProxy(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, prefix := range r.trustedProxies {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func requestRemoteIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = strings.Trim(remoteAddr, "[]")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func headerIP(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, false
	}
	ip, ok := requestRemoteIP(raw)
	if ok {
		return ip, true
	}
	ip, err := netip.ParseAddr(strings.Trim(raw, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func labelConfigsForLabels(configs map[string]config.LabelConfig, labels []string) []config.LabelConfig {
	out := make([]config.LabelConfig, 0, len(labels))
	for _, label := range labels {
		if cfg, ok := configs[label]; ok {
			out = append(out, cfg)
		}
	}
	return out
}

func toolPoliciesForLabels(configs map[string]config.LabelConfig, labels []string) map[string]config.Policy {
	var merged map[string]config.Policy
	for _, cfg := range labelConfigsForLabels(configs, labels) {
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
		if !rightPolicy.Enabled {
			out[name] = clonePolicy(rightPolicy)
			continue
		}
		leftPolicy, ok := out[name]
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

func clonePolicy(cfg config.Policy) config.Policy {
	if cfg.AllowedArgs != nil {
		cfg.AllowedArgs = cloneStringMap(cfg.AllowedArgs)
	}
	return cfg
}
