package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
	"github.com/ztelliot/mtr/internal/tlsutil"
	"github.com/ztelliot/mtr/internal/version"
)

type HTTPAgent struct {
	ID           string
	Country      string
	Region       string
	Provider     string
	ISP          string
	Labels       []string
	BaseURL      string
	Token        string
	HTTPClient   *http.Client
	Version      string
	Capabilities []model.Tool
	Protocols    model.ProtocolMask
}

type HTTPAgentTLS struct {
	Enabled  bool
	CAFiles  []string
	CertFile string
	KeyFile  string
}

const (
	defaultHTTPAgentMaxHealthInterval = 300 * time.Second
	defaultHTTPAgentInvokeAttempts    = 3
	httpAgentNDJSONLineLimit          = 1 << 20
)

var httpAgentInvokeRetryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}

var fallbackHTTPAgentHTTPClient = &http.Client{Transport: httpAgentTransport(nil)}

type httpAgentConnectionError struct {
	err error
}

type httpAgentHealth struct {
	AgentID      string
	Country      string
	Region       string
	Provider     string
	ISP          string
	Version      string
	Capabilities []model.Tool
	Protocols    model.ProtocolMask
}

func (e *httpAgentConnectionError) Error() string {
	return e.err.Error()
}

func (e *httpAgentConnectionError) Unwrap() error {
	return e.err
}

func isHTTPAgentConnectionError(err error) bool {
	var connErr *httpAgentConnectionError
	return errors.As(err, &connErr)
}

func (h *Hub) SetHTTPAgents(agents []HTTPAgent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpAgents = append([]HTTPAgent(nil), agents...)
}

func (h *Hub) ReloadHTTPAgents(ctx context.Context) {
	agents, err := h.configuredHTTPAgents(ctx)
	if err != nil {
		h.log.Warn("reload http agents", "err", err)
		return
	}
	h.mu.Lock()
	running := map[string]struct{}{}
	for _, agent := range agents {
		running[agent.ID] = struct{}{}
	}
	for id, cancel := range h.httpAgentCancels {
		if _, ok := running[id]; ok {
			continue
		}
		cancel()
		delete(h.httpAgentCancels, id)
	}
	h.httpAgents = append([]HTTPAgent(nil), agents...)
	for _, agent := range agents {
		if h.httpAgentCancels[agent.ID] != nil {
			continue
		}
		loopCtx, cancel := context.WithCancel(ctx)
		h.httpAgentCancels[agent.ID] = cancel
		go h.httpAgentLoop(loopCtx, agent)
	}
	h.mu.Unlock()
}

func NewHTTPAgentHTTPClient(tlsConfig HTTPAgentTLS) (*http.Client, error) {
	cfg, err := tlsutil.ClientTLSConfig(tlsConfig.CAFiles, tlsConfig.CertFile, tlsConfig.KeyFile, tlsConfig.Enabled)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &http.Client{Transport: httpAgentTransport(nil)}, nil
	}
	return &http.Client{Transport: httpAgentTransport(cfg)}, nil
}

func httpAgentTransport(tlsConfig *tls.Config) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	return transport
}

func (h *Hub) startHTTPAgents(ctx context.Context) {
	h.mu.Lock()
	agents := append([]HTTPAgent(nil), h.httpAgents...)
	h.mu.Unlock()
	for _, agent := range agents {
		if agent.ID == "" || agent.BaseURL == "" {
			h.log.Warn("skip invalid http agent", "agent_id", agent.ID, "base_url", agent.BaseURL)
			continue
		}
		loopCtx, cancel := context.WithCancel(ctx)
		h.mu.Lock()
		if h.httpAgentCancels[agent.ID] != nil {
			h.mu.Unlock()
			cancel()
			continue
		}
		h.httpAgentCancels[agent.ID] = cancel
		h.mu.Unlock()
		go h.httpAgentLoop(loopCtx, agent)
	}
}

func (h *Hub) startConfiguredHTTPAgents(ctx context.Context) {
	agents, err := h.configuredHTTPAgents(ctx)
	if err != nil {
		h.log.Warn("list http agents", "err", err)
		h.startHTTPAgents(ctx)
		return
	}
	if len(agents) == 0 {
		h.startHTTPAgents(ctx)
		return
	}
	h.SetHTTPAgents(agents)
	h.startHTTPAgents(ctx)
}

func (h *Hub) configuredHTTPAgents(ctx context.Context) ([]HTTPAgent, error) {
	nodes, err := h.store.ListHTTPAgents(ctx)
	if err != nil {
		return nil, err
	}
	agents := make([]HTTPAgent, 0, len(nodes))
	for _, node := range nodes {
		if !node.Enabled {
			continue
		}
		client, err := NewHTTPAgentHTTPClient(HTTPAgentTLS{
			Enabled:  node.TLS.Enabled,
			CAFiles:  node.TLS.CAFiles,
			CertFile: node.TLS.CertFile,
			KeyFile:  node.TLS.KeyFile,
		})
		if err != nil {
			h.log.Warn("load http agent tls", "agent_id", node.ID, "err", err)
			continue
		}
		agents = append(agents, HTTPAgent{
			ID:         node.ID,
			Labels:     model.NormalizeAgentLabels(node.ID, model.AgentTransportHTTP, node.Labels),
			BaseURL:    node.BaseURL,
			Token:      node.HTTPToken,
			HTTPClient: client,
		})
	}
	return agents, nil
}

func (h *Hub) httpAgentLoop(ctx context.Context, agent HTTPAgent) {
	health, err := probeHTTPAgentHealth(ctx, agent)
	recovering := err != nil
	status := model.AgentOnline
	if recovering {
		status = model.AgentOffline
		h.log.Warn("http agent startup health check failed", "agent_id", agent.ID, "base_url", agent.BaseURL, "err", err)
	} else if err := agent.applyHealth(health); err != nil {
		recovering = true
		status = model.AgentOffline
		h.log.Warn("http agent startup health check identity mismatch", "agent_id", agent.ID, "base_url", agent.BaseURL, "err", err)
		if markErr := h.store.MarkAgentOffline(ctx, agent.ID); markErr != nil && !errors.Is(markErr, store.ErrNotFound) {
			h.log.Warn("mark http agent offline failed", "agent_id", agent.ID, "err", markErr)
		}
	}
	h.upsertHTTPAgent(ctx, agent, status)
	h.log.Info("http agent configured", "agent_id", agent.ID, "base_url", agent.BaseURL, "capabilities", agent.Capabilities, "protocols", agent.Protocols, "version", agent.Version, "status", status)

	recoveryCh := make(chan error, 1)
	healthInterval := h.pollIntervalForHTTPAgent(agent)
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = h.store.MarkAgentOffline(context.Background(), agent.ID)
			h.mu.Lock()
			delete(h.httpAgentCancels, agent.ID)
			h.mu.Unlock()
			return
		case err := <-recoveryCh:
			if !recovering {
				h.log.Debug("http agent entering recovery health checks", "agent_id", agent.ID, "err", err)
			}
			recovering = true
			healthInterval = h.pollIntervalForHTTPAgent(agent)
			ticker.Reset(healthInterval)
			_ = h.store.MarkAgentOffline(ctx, agent.ID)
		case <-ticker.C:
			if recovering {
				health, err := probeHTTPAgentHealth(ctx, agent)
				if err != nil {
					_ = h.store.MarkAgentOffline(ctx, agent.ID)
					maxHealthInterval, _ := h.httpAgentRuntimeForAgent(agent)
					healthInterval = nextHTTPAgentHealthInterval(healthInterval, h.pollIntervalForHTTPAgent(agent), maxHealthInterval)
					ticker.Reset(healthInterval)
					h.log.Debug("http agent recovery health check failed", "agent_id", agent.ID, "next_check_after", healthInterval.String(), "err", err)
					continue
				}
				if err := agent.applyHealth(health); err != nil {
					_ = h.store.MarkAgentOffline(ctx, agent.ID)
					h.log.Warn("http agent recovery health check identity mismatch", "agent_id", agent.ID, "err", err)
					continue
				}
				h.upsertHTTPAgent(ctx, agent, model.AgentOnline)
				recovering = false
				h.log.Debug("http agent health check recovered", "agent_id", agent.ID)
				healthInterval = h.pollIntervalForHTTPAgent(agent)
				ticker.Reset(healthInterval)
			}
			h.touchAgent(agent.ID)
			_ = h.store.TouchAgent(ctx, agent.ID)
			maxInflight := h.inflightLimitForHTTPAgent(agent)
			remaining := maxInflight - h.inflightCount(agent.ID)
			if remaining <= 0 {
				h.log.Debug("http agent at inflight limit", "agent_id", agent.ID, "max_inflight", maxInflight, "transport", "http")
				continue
			}
			agentPolicies := h.policiesForHTTPAgent(agent)
			policyCaps := policyCapabilities(agentPolicies, agent.Capabilities)
			jobs, err := h.store.ListQueuedJobs(ctx, agent.ID, policyCaps, agent.Protocols, remaining)
			if err != nil {
				h.log.Warn("list http agent queued jobs", "agent_id", agent.ID, "err", err)
				continue
			}
			for _, job := range jobs {
				p, ok := agentPolicies.Get(job.Tool)
				if !ok {
					h.log.Debug("skip http agent job with disabled tool", "job_id", job.ID, "tool", job.Tool)
					continue
				}
				claimed, err := h.store.ClaimQueuedJob(ctx, job.ID, agent.ID)
				if err != nil {
					h.log.Warn("claim http agent queued job failed", "job_id", job.ID, "err", err)
					continue
				}
				if !claimed {
					h.log.Debug("skip already claimed http agent job", "job_id", job.ID)
					continue
				}
				h.markInflight(agent.ID, job.ID)
				h.emitStarted(ctx, job)
				spec := toJobSpecWithPolicies(job, p, agentPolicies)
				h.log.Debug("dispatch http agent job", "agent_id", agent.ID, "job_id", job.ID, "tool", job.Tool)
				go h.invokeHTTPAgent(ctx, agent, spec, recoveryCh)
			}
		}
	}
}

func (a *HTTPAgent) applyHealth(health httpAgentHealth) error {
	if health.AgentID == "" {
		return errors.New("health agent id is required")
	}
	if health.AgentID != a.ID {
		return fmt.Errorf("health agent id %q does not match configured id %q", health.AgentID, a.ID)
	}
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
	return nil
}

func (h *Hub) upsertHTTPAgent(ctx context.Context, agent HTTPAgent, status model.AgentStatus) {
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

func (h *Hub) httpAgentRuntime() (time.Duration, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	maxHealthInterval := h.httpAgentMaxHealthInterval
	invokeAttempts := h.httpAgentInvokeAttempts
	if maxHealthInterval <= 0 {
		maxHealthInterval = defaultHTTPAgentMaxHealthInterval
	}
	if invokeAttempts <= 0 {
		invokeAttempts = defaultHTTPAgentInvokeAttempts
	}
	return maxHealthInterval, invokeAttempts
}

func (h *Hub) httpAgentRuntimeForAgent(agent HTTPAgent) (time.Duration, int) {
	_, runtime, _ := h.settingsForHTTPAgent(agent)
	return time.Duration(runtime.HTTPMaxHealthIntervalSec) * time.Second, runtime.HTTPInvokeAttempts
}

func (h *Hub) inflightLimitForHTTPAgent(agent HTTPAgent) int {
	scheduler, _, _ := h.settingsForHTTPAgent(agent)
	if scheduler.MaxInflightPerAgent <= 0 {
		return h.inflightLimit()
	}
	return scheduler.MaxInflightPerAgent
}

func (h *Hub) pollIntervalForHTTPAgent(agent HTTPAgent) time.Duration {
	scheduler, _, _ := h.settingsForHTTPAgent(agent)
	if scheduler.PollIntervalSec <= 0 {
		return h.currentPollInterval()
	}
	return time.Duration(scheduler.PollIntervalSec) * time.Second
}

func (h *Hub) policiesForHTTPAgent(agent HTTPAgent) policy.Set {
	_, runtime, toolPolicies := h.settingsForHTTPAgent(agent)
	return h.policiesSnapshot().WithIntersection(toolPolicies, runtime)
}

func (h *Hub) settingsForHTTPAgent(agent HTTPAgent) (config.Scheduler, config.Runtime, map[string]config.Policy) {
	basePolicies := h.policiesSnapshot()
	runtime := basePolicies.Runtime()
	scheduler := h.schedulerSnapshot()
	labels := agent.Labels
	if len(labels) == 0 {
		labels = model.NormalizeAgentLabels(agent.ID, model.AgentTransportHTTP, nil)
	}
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

func nextHTTPAgentHealthInterval(current time.Duration, base time.Duration, maxInterval time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maxInterval <= 0 {
		maxInterval = defaultHTTPAgentMaxHealthInterval
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

func checkHTTPAgentHealth(parent context.Context, agent HTTPAgent) error {
	_, err := probeHTTPAgentHealth(parent, agent)
	return err
}

func probeHTTPAgentHealth(parent context.Context, agent HTTPAgent) (httpAgentHealth, error) {
	healthURL, err := httpAgentHealthURL(agent)
	if err != nil {
		return httpAgentHealth{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return httpAgentHealth{}, err
	}
	resp, err := httpAgentHTTPClient(agent).Do(req)
	if err != nil {
		return httpAgentHealth{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpAgentHealth{}, fmt.Errorf("http agent health returned HTTP %d", resp.StatusCode)
	}
	health, err := decodeHTTPAgentHealth(body)
	if err != nil {
		return httpAgentHealth{}, err
	}
	if health.AgentID == "" {
		return httpAgentHealth{}, errors.New("http agent health missing agent.id")
	}
	if health.AgentID != agent.ID {
		return httpAgentHealth{}, fmt.Errorf("health agent id %q does not match configured id %q", health.AgentID, agent.ID)
	}
	return health, nil
}

func decodeHTTPAgentHealth(body []byte) (httpAgentHealth, error) {
	var raw struct {
		Status  string          `json:"status"`
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
		return httpAgentHealth{}, fmt.Errorf("decode http agent health: %w", err)
	}
	if strings.TrimSpace(raw.Status) != "ok" {
		return httpAgentHealth{}, fmt.Errorf("http agent health status %q is not ok", raw.Status)
	}
	return httpAgentHealth{
		AgentID:      raw.Agent.ID,
		Country:      raw.Agent.Country,
		Region:       raw.Agent.Region,
		Provider:     raw.Agent.Provider,
		ISP:          raw.Agent.ISP,
		Version:      httpAgentHealthVersion(raw.Version),
		Capabilities: raw.Agent.Capabilities,
		Protocols:    raw.Agent.Protocols,
	}, nil
}

func httpAgentHealthVersion(raw json.RawMessage) string {
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
		out += " " + version.ShortCommit(info.Commit)
	}
	if info.BuiltAt != "" {
		out += " " + info.BuiltAt
	}
	return out
}

func httpAgentHealthURL(agent HTTPAgent) (string, error) {
	return httpAgentURL(agent.BaseURL, "/healthz")
}

func (h *Hub) invokeHTTPAgent(parent context.Context, agent HTTPAgent, spec *grpcwire.JobSpec, recoveryCh chan<- error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(spec.TimeoutSeconds+5)*time.Second)
	h.setInflightCancel(spec.ID, cancel)
	defer cancel()
	defer h.setInflightCancel(spec.ID, nil)

	gotParsed := false
	_, attempts := h.httpAgentRuntimeForAgent(agent)
	err := callHTTPAgentWithAttempts(ctx, agent, spec, attempts, func(ev grpcwire.ResultEvent) {
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
		if isHTTPAgentConnectionError(err) {
			if markErr := h.store.MarkAgentOffline(context.Background(), agent.ID); markErr != nil {
				h.log.Warn("mark http agent offline failed", "agent_id", agent.ID, "err", markErr)
			}
			select {
			case recoveryCh <- err:
			default:
			}
		}
		h.failJob(context.Background(), agent.ID, spec.ID, err.Error())
		h.log.Warn("http agent job failed", "agent_id", agent.ID, "job_id", spec.ID, "err", err)
		return
	}
	h.touchAgent(agent.ID)
	if err := h.store.TouchAgent(context.Background(), agent.ID); err != nil {
		h.log.Warn("touch http agent failed", "agent_id", agent.ID, "err", err)
	}
	if !gotParsed {
		h.clearInflight(agent.ID, spec.ID)
		h.failJob(context.Background(), agent.ID, spec.ID, "http agent did not return parsed result")
		h.log.Warn("http agent job missing parsed result", "agent_id", agent.ID, "job_id", spec.ID)
	}
}

func callHTTPAgent(ctx context.Context, agent HTTPAgent, spec *grpcwire.JobSpec, onEvent func(grpcwire.ResultEvent)) error {
	return callHTTPAgentWithAttempts(ctx, agent, spec, defaultHTTPAgentInvokeAttempts, onEvent)
}

func callHTTPAgentWithAttempts(ctx context.Context, agent HTTPAgent, spec *grpcwire.JobSpec, attempts int, onEvent func(grpcwire.ResultEvent)) error {
	if attempts <= 0 {
		attempts = defaultHTTPAgentInvokeAttempts
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		attemptHadEvent := false
		err = callHTTPAgentOnce(ctx, agent, spec, func(event grpcwire.ResultEvent) {
			attemptHadEvent = true
			onEvent(event)
		})
		if err == nil || attemptHadEvent || !isHTTPAgentConnectionError(err) || attempt == attempts || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(httpAgentInvokeRetryDelay(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
	return err
}

func callHTTPAgentOnce(ctx context.Context, agent HTTPAgent, spec *grpcwire.JobSpec, onEvent func(grpcwire.ResultEvent)) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	invokeURL, err := httpAgentURL(agent.BaseURL, "/invoke")
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
	resp, err := httpAgentHTTPClient(agent).Do(req)
	if err != nil {
		return &httpAgentConnectionError{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http agent returned HTTP %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := readHTTPAgentNDJSONLine(reader)
		if len(bytes.TrimSpace(line)) > 0 {
			ev, decodeErr := decodeHTTPAgentLine(bytes.TrimSpace(line), spec.ID, agent.ID)
			if decodeErr != nil {
				return decodeErr
			}
			onEvent(ev)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return &httpAgentConnectionError{err: err}
		}
	}
}

func readHTTPAgentNDJSONLine(reader *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		part, err := reader.ReadSlice('\n')
		out = append(out, part...)
		if len(out) > httpAgentNDJSONLineLimit {
			return nil, fmt.Errorf("http agent event line exceeds %d bytes", httpAgentNDJSONLineLimit)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}

func httpAgentURL(baseURL string, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func httpAgentHTTPClient(agent HTTPAgent) *http.Client {
	if agent.HTTPClient != nil {
		return agent.HTTPClient
	}
	return fallbackHTTPAgentHTTPClient
}

func decodeHTTPAgentLine(line []byte, jobID string, agentID string) (grpcwire.ResultEvent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return grpcwire.ResultEvent{}, err
	}
	_, hasType := fields["type"]
	if !hasType {
		return grpcwire.ResultEvent{}, fmt.Errorf("http agent event missing type")
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
