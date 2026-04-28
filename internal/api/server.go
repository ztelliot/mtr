package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/scheduler"
	"github.com/ztelliot/mtr/internal/store"
	"github.com/ztelliot/mtr/internal/version"
)

type Server struct {
	store    store.Store
	policies policy.Set
	limiter  *abuse.Limiter
	hub      *scheduler.Hub
	tokens   map[string]TokenScope
	geoIPURL string
	log      *slog.Logger
}

type dispatchTarget struct {
	AgentID        string
	ResolvedTarget string
	IPVersion      model.IPVersion
}

var (
	errAgentNotFound = errors.New("agent not found")
	errAgentOffline  = errors.New("agent is offline")
)

func New(st store.Store, policies policy.Set, limiter *abuse.Limiter, hub *scheduler.Hub, geoIPURL string, log *slog.Logger, tokenConfigs ...TokenConfig) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		store:    st,
		policies: policies,
		limiter:  limiter,
		hub:      hub,
		tokens:   map[string]TokenScope{},
		geoIPURL: strings.TrimSpace(geoIPURL),
		log:      log,
	}
	for _, config := range tokenConfigs {
		if config.Token != "" {
			s.tokens[config.Token] = normalizeTokenScope(config.Scope)
		}
	}
	r := chi.NewRouter()
	r.Use(s.auth)
	r.Get("/v1/version", s.getVersion)
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
	if s.geoIPURL != "" {
		r.Get("/v1/geoip/{ip}", s.getGeoIP)
	}
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": version.Current()})
	})
	return r
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := clientIP(r)
		s.log.Debug("http request", "method", r.Method, "path", r.URL.Path, "client_ip", clientIP, "remote_addr", r.RemoteAddr)
		if len(s.tokens) > 0 {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" {
				token = r.URL.Query().Get("access_token")
			}
			scope, ok := s.tokens[token]
			if !ok {
				writeError(w, http.StatusUnauthorized, "invalid api token")
				return
			}
			r = r.WithContext(withPrincipal(r.Context(), principal{token: token, scope: scope}))
		} else {
			r = r.WithContext(withPrincipal(r.Context(), principal{scope: allTokenScope}))
		}
		if !s.limiter.AllowRequest(clientIP) {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) getPermissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.permissionsForContext(r.Context()))
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Current())
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req model.CreateJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !s.limiter.AllowTool(string(req.Tool), clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "tool rate limit exceeded")
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.Args = s.policies.ServerArgs(req.Tool, req.Args)
	_, err := s.policies.Validate(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.authorizeJob(r.Context(), req); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	options, err := s.dispatchOptions(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	scope := principalFromContext(r.Context()).scope
	if req.AgentID == "" {
		targets, err := s.fanoutDispatchTargets(r.Context(), req.Tool, options, scope)
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

func (s *Server) fanoutDispatchTargets(ctx context.Context, tool model.Tool, options []dispatchTarget, scope TokenScope) ([]dispatchTarget, error) {
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]dispatchTarget, 0, len(agents))
	for _, agent := range agents {
		if !scopeAllowsAgent(scope, agent.ID) {
			continue
		}
		if target, ok := selectDispatchTarget(agent, tool, options, true); ok {
			target.AgentID = agent.ID
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func (s *Server) pinnedDispatchTarget(ctx context.Context, agentID string, tool model.Tool, options []dispatchTarget) (dispatchTarget, string, error) {
	fallback := dispatchTarget{AgentID: agentID}
	if len(options) > 0 {
		fallback = options[0]
		fallback.AgentID = agentID
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return dispatchTarget{}, "", err
	}
	for _, agent := range agents {
		if agent.ID != agentID {
			continue
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

func (s *Server) dispatchOptions(ctx context.Context, req model.CreateJobRequest) ([]dispatchTarget, error) {
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
			resolvedTarget, _, err := s.policies.ResolveTarget(ctx, req.Tool, req.Target, literalVersion)
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
		resolvedTarget, resolvedVersion, err := s.policies.ResolveTarget(ctx, req.Tool, req.Target, req.IPVersion)
		if err != nil {
			return nil, err
		}
		return []dispatchTarget{{ResolvedTarget: resolvedTarget, IPVersion: resolvedVersion}}, nil
	}
	var options []dispatchTarget
	var lastErr error
	for _, version := range []model.IPVersion{model.IPv6, model.IPv4} {
		resolvedTarget, resolvedVersion, err := s.policies.ResolveTarget(ctx, req.Tool, req.Target, version)
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
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	scope := principalFromContext(r.Context()).scope
	filtered := agents[:0]
	for _, agent := range agents {
		if scopeAllowsAgent(scope, agent.ID) {
			filtered = append(filtered, agent)
		}
	}
	writeJSON(w, http.StatusOK, filtered)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
