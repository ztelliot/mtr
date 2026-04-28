package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/version"
)

type OutboundAgent struct {
	ID           string
	Country      string
	Region       string
	Provider     string
	ISP          string
	BaseURL      string
	Token        string
	Version      string
	Capabilities []model.Tool
	Protocols    model.ProtocolMask
}

const (
	defaultOutboundMaxHealthInterval = 300 * time.Second
	defaultOutboundInvokeAttempts    = 3
)

var outboundInvokeRetryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}

type outboundConnectionError struct {
	err error
}

type outboundHealth struct {
	AgentID      string
	Country      string
	Region       string
	Provider     string
	ISP          string
	Version      string
	Capabilities []model.Tool
	Protocols    model.ProtocolMask
}

func (e *outboundConnectionError) Error() string {
	return e.err.Error()
}

func (e *outboundConnectionError) Unwrap() error {
	return e.err
}

func isOutboundConnectionError(err error) bool {
	var connErr *outboundConnectionError
	return errors.As(err, &connErr)
}

func (h *Hub) SetOutboundAgents(agents []OutboundAgent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outbound = append([]OutboundAgent(nil), agents...)
}

func (h *Hub) startOutboundAgents(ctx context.Context) {
	h.mu.Lock()
	agents := append([]OutboundAgent(nil), h.outbound...)
	h.mu.Unlock()
	for _, agent := range agents {
		if agent.ID == "" || agent.BaseURL == "" {
			h.log.Warn("skip invalid outbound agent", "agent_id", agent.ID, "base_url", agent.BaseURL)
			continue
		}
		go h.outboundLoop(ctx, agent)
	}
}

func (h *Hub) outboundLoop(ctx context.Context, agent OutboundAgent) {
	health, err := probeOutboundAgentHealth(ctx, agent)
	recovering := err != nil
	status := model.AgentOnline
	if recovering {
		status = model.AgentOffline
		h.log.Warn("outbound agent startup health check failed", "agent_id", agent.ID, "base_url", agent.BaseURL, "err", err)
	} else {
		agent.applyHealth(health)
	}
	h.upsertOutboundAgent(ctx, agent, status)
	h.log.Info("outbound agent configured", "agent_id", agent.ID, "base_url", agent.BaseURL, "capabilities", agent.Capabilities, "protocols", agent.Protocols, "version", agent.Version, "status", status)

	recoveryCh := make(chan error, 1)
	healthInterval := h.pollInterval
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = h.store.MarkAgentOffline(context.Background(), agent.ID)
			return
		case err := <-recoveryCh:
			if !recovering {
				h.log.Debug("outbound agent entering recovery health checks", "agent_id", agent.ID, "err", err)
			}
			recovering = true
			healthInterval = h.pollInterval
			ticker.Reset(healthInterval)
			_ = h.store.MarkAgentOffline(ctx, agent.ID)
		case <-ticker.C:
			if recovering {
				health, err := probeOutboundAgentHealth(ctx, agent)
				if err != nil {
					_ = h.store.MarkAgentOffline(ctx, agent.ID)
					maxHealthInterval, _ := h.outboundRuntime()
					healthInterval = nextOutboundHealthInterval(healthInterval, h.pollInterval, maxHealthInterval)
					ticker.Reset(healthInterval)
					h.log.Debug("outbound agent recovery health check failed", "agent_id", agent.ID, "next_check_after", healthInterval.String(), "err", err)
					continue
				}
				agent.applyHealth(health)
				h.upsertOutboundAgent(ctx, agent, model.AgentOnline)
				recovering = false
				h.log.Debug("outbound agent health check recovered", "agent_id", agent.ID)
				healthInterval = h.pollInterval
				ticker.Reset(healthInterval)
			}
			h.touchAgent(agent.ID)
			_ = h.store.TouchAgent(ctx, agent.ID)
			maxInflight := h.outboundInflightLimit()
			remaining := maxInflight - h.inflightCount(agent.ID)
			if remaining <= 0 {
				h.log.Debug("outbound agent at inflight limit", "agent_id", agent.ID, "max_inflight", maxInflight, "transport", "outbound")
				continue
			}
			jobs, err := h.store.ListQueuedJobs(ctx, agent.ID, agent.Capabilities, agent.Protocols, remaining)
			if err != nil {
				h.log.Warn("list outbound queued jobs", "agent_id", agent.ID, "err", err)
				continue
			}
			for _, job := range jobs {
				p, ok := h.policies.Get(job.Tool)
				if !ok {
					h.log.Debug("skip outbound job with disabled tool", "job_id", job.ID, "tool", job.Tool)
					continue
				}
				claimed, err := h.store.ClaimQueuedJob(ctx, job.ID)
				if err != nil {
					h.log.Warn("claim outbound queued job failed", "job_id", job.ID, "err", err)
					continue
				}
				if !claimed {
					h.log.Debug("skip already claimed outbound job", "job_id", job.ID)
					continue
				}
				h.markInflight(agent.ID, job.ID)
				h.emitStarted(ctx, job)
				spec := h.toJobSpec(job, p)
				h.log.Debug("dispatch outbound job", "agent_id", agent.ID, "job_id", job.ID, "tool", job.Tool)
				go h.invokeOutbound(ctx, agent, spec, recoveryCh)
			}
		}
	}
}

func (a *OutboundAgent) applyHealth(health outboundHealth) {
	if health.Country != "" {
		a.Country = health.Country
	}
	if health.Region != "" {
		a.Region = health.Region
	}
	if health.Provider != "" {
		a.Provider = health.Provider
	}
	if health.ISP != "" {
		a.ISP = health.ISP
	}
	if health.Version != "" {
		a.Version = health.Version
	}
	if len(health.Capabilities) > 0 {
		a.Capabilities = health.Capabilities
	}
	if health.Protocols != 0 {
		a.Protocols = health.Protocols
	}
}

func (h *Hub) upsertOutboundAgent(ctx context.Context, agent OutboundAgent, status model.AgentStatus) {
	now := time.Now().UTC()
	_ = h.store.UpsertAgent(ctx, model.Agent{
		ID:           agent.ID,
		Country:      agent.Country,
		Region:       agent.Region,
		Provider:     agent.Provider,
		ISP:          agent.ISP,
		Version:      agent.Version,
		TokenHash:    hashToken(agent.Token),
		Capabilities: agent.Capabilities,
		Protocols:    agent.Protocols,
		Status:       status,
		LastSeenAt:   now,
		CreatedAt:    now,
	})
	h.mu.Lock()
	h.meta[agent.ID] = agentMeta{capabilities: agent.Capabilities, protocols: agent.Protocols, lastSeen: now}
	if h.running[agent.ID] == nil {
		h.running[agent.ID] = map[string]struct{}{}
	}
	h.mu.Unlock()
}

func (h *Hub) outboundRuntime() (time.Duration, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	maxHealthInterval := h.outboundMaxHealthInterval
	invokeAttempts := h.outboundInvokeAttempts
	if maxHealthInterval <= 0 {
		maxHealthInterval = defaultOutboundMaxHealthInterval
	}
	if invokeAttempts <= 0 {
		invokeAttempts = defaultOutboundInvokeAttempts
	}
	return maxHealthInterval, invokeAttempts
}

func nextOutboundHealthInterval(current time.Duration, base time.Duration, maxInterval time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maxInterval <= 0 {
		maxInterval = defaultOutboundMaxHealthInterval
	}
	if current < base {
		current = base
	}
	next := current * 2
	if next > maxInterval {
		return maxInterval
	}
	return next
}

func checkOutboundAgentHealth(parent context.Context, agent OutboundAgent) error {
	_, err := probeOutboundAgentHealth(parent, agent)
	return err
}

func probeOutboundAgentHealth(parent context.Context, agent OutboundAgent) (outboundHealth, error) {
	healthURL, err := outboundHealthURL(agent)
	if err != nil {
		return outboundHealth{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return outboundHealth{}, err
	}
	if agent.Token != "" {
		req.Header.Set("Authorization", "Bearer "+agent.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return outboundHealth{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return outboundHealth{}, fmt.Errorf("outbound agent health returned HTTP %d", resp.StatusCode)
	}
	return decodeOutboundHealth(body), nil
}

func decodeOutboundHealth(body []byte) outboundHealth {
	var raw struct {
		Version json.RawMessage `json:"version"`
		Agent   struct {
			ID           string             `json:"id"`
			Country      string             `json:"country"`
			Region       string             `json:"region"`
			Provider     string             `json:"provider"`
			ISP          string             `json:"isp"`
			Capabilities []model.Tool       `json:"capabilities"`
			Protocols    model.ProtocolMask `json:"protocols"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return outboundHealth{}
	}
	return outboundHealth{
		AgentID:      raw.Agent.ID,
		Country:      raw.Agent.Country,
		Region:       raw.Agent.Region,
		Provider:     raw.Agent.Provider,
		ISP:          raw.Agent.ISP,
		Version:      outboundHealthVersion(raw.Version),
		Capabilities: raw.Agent.Capabilities,
		Protocols:    raw.Agent.Protocols,
	}
}

func outboundHealthVersion(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var info version.Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return ""
	}
	return strings.TrimSpace(versionInfoString(info))
}

func versionInfoString(info version.Info) string {
	out := strings.TrimSpace(info.Version)
	if out == "" {
		return ""
	}
	if info.Commit != "" {
		out += " " + info.Commit
	}
	if info.BuiltAt != "" {
		out += " " + info.BuiltAt
	}
	return out
}

func outboundHealthURL(agent OutboundAgent) (string, error) {
	return outboundAgentURL(agent.BaseURL, "/healthz")
}

func (h *Hub) invokeOutbound(parent context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, recoveryCh chan<- error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(spec.TimeoutSeconds+5)*time.Second)
	h.setInflightCancel(spec.ID, cancel)
	defer cancel()
	defer h.setInflightCancel(spec.ID, nil)

	gotParsed := false
	_, attempts := h.outboundRuntime()
	err := callOutboundAgentWithAttempts(ctx, agent, spec, attempts, func(ev grpcwire.ResultEvent) {
		if ev.AgentID == "" {
			ev.AgentID = agent.ID
		}
		if ev.JobID == "" {
			ev.JobID = spec.ID
		}
		if eventType(ev.Event) == "summary" {
			gotParsed = true
		}
		h.handleResult(context.Background(), agent.ID, ev)
	})
	if err != nil {
		h.clearInflight(agent.ID, spec.ID)
		if isOutboundConnectionError(err) {
			if markErr := h.store.MarkAgentOffline(context.Background(), agent.ID); markErr != nil {
				h.log.Warn("mark outbound agent offline failed", "agent_id", agent.ID, "err", markErr)
			}
			select {
			case recoveryCh <- err:
			default:
			}
		}
		h.failJob(context.Background(), agent.ID, spec.ID, err.Error())
		h.log.Warn("outbound job failed", "agent_id", agent.ID, "job_id", spec.ID, "err", err)
		return
	}
	h.touchAgent(agent.ID)
	if err := h.store.TouchAgent(context.Background(), agent.ID); err != nil {
		h.log.Warn("touch outbound agent failed", "agent_id", agent.ID, "err", err)
	}
	if !gotParsed {
		h.clearInflight(agent.ID, spec.ID)
		h.failJob(context.Background(), agent.ID, spec.ID, "outbound agent did not return parsed result")
		h.log.Warn("outbound job missing parsed result", "agent_id", agent.ID, "job_id", spec.ID)
	}
}

func callOutboundAgent(ctx context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, onEvent func(grpcwire.ResultEvent)) error {
	return callOutboundAgentWithAttempts(ctx, agent, spec, defaultOutboundInvokeAttempts, onEvent)
}

func callOutboundAgentWithAttempts(ctx context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, attempts int, onEvent func(grpcwire.ResultEvent)) error {
	if attempts <= 0 {
		attempts = defaultOutboundInvokeAttempts
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		attemptHadEvent := false
		err = callOutboundAgentOnce(ctx, agent, spec, func(event grpcwire.ResultEvent) {
			attemptHadEvent = true
			onEvent(event)
		})
		if err == nil || attemptHadEvent || !isOutboundConnectionError(err) || attempt == attempts || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(outboundInvokeRetryDelay(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
	return err
}

func callOutboundAgentOnce(ctx context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, onEvent func(grpcwire.ResultEvent)) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	invokeURL, err := outboundAgentURL(agent.BaseURL, "/invoke")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, invokeURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if agent.Token != "" {
		req.Header.Set("Authorization", "Bearer "+agent.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &outboundConnectionError{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("outbound agent returned HTTP %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			ev, decodeErr := decodeOutboundLine(bytes.TrimSpace(line), spec.ID, agent.ID)
			if decodeErr != nil {
				return decodeErr
			}
			onEvent(ev)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return &outboundConnectionError{err: err}
		}
	}
}

func outboundAgentURL(baseURL string, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func decodeOutboundLine(line []byte, jobID string, agentID string) (grpcwire.ResultEvent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return grpcwire.ResultEvent{}, err
	}
	_, hasType := fields["type"]
	if !hasType {
		return grpcwire.ResultEvent{}, fmt.Errorf("outbound event missing type")
	}
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return grpcwire.ResultEvent{}, err
	}
	return grpcwire.ResultEvent{
		JobID:   jobID,
		AgentID: agentID,
		Event:   event,
	}, nil
}

func (h *Hub) emitProgress(ctx context.Context, jobID string, message string) {
	job, err := h.store.GetJob(ctx, jobID)
	if err != nil {
		h.log.Warn("load progress job failed", "job_id", jobID, "message", message, "err", err)
		return
	}
	eventJobID := job.ID
	if job.ParentID != "" {
		eventJobID = job.ParentID
	}
	event := model.JobEvent{
		ID:        uuid.NewString(),
		JobID:     eventJobID,
		Stream:    "progress",
		Event:     &model.StreamEvent{Type: "progress", Message: message},
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.AddJobEvent(ctx, event); err != nil {
		h.log.Warn("store progress event failed", "job_id", jobID, "message", message, "err", err)
		return
	}
	h.publish(event)
}
