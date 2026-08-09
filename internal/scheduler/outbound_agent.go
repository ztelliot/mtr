package scheduler

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/huaweisign"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/tlsutil"
	"github.com/ztelliot/mtr/internal/version"
	"golang.org/x/net/http/httpguts"
)

type OutboundAgent struct {
	ProtocolVersion   int
	ID                string
	Country           string
	Region            string
	Provider          string
	ISP               string
	Labels            []string
	Transport         string
	URL               string
	Headers           map[string]string
	Auth              config.OutboundAgentAuth
	EventResponse     string
	HTTPClient        *http.Client
	Version           string
	Capabilities      []model.Tool
	Protocols         model.ProtocolMask
	MaxConcurrentJobs int
	configHash        [sha256.Size]byte
}

type OutboundAgentTLS struct {
	CAFiles    []string
	CertFile   string
	KeyFile    string
	ServerName string // Optional SNI only; never used for certificate identity verification.
}

const (
	defaultOutboundAgentMaxHealthInterval = 300 * time.Second
	defaultOutboundAgentInvokeAttempts    = 3
	outboundAgentReconcileInterval        = 30 * time.Second
	outboundAgentHealthBodyLimit          = 64 << 10
	outboundAgentResultEventLimit         = 1024
	outboundAgentResultBytesLimit         = 4 << 20
	outboundEventHealthHTTPBodyLimit      = 2*outboundAgentHealthBodyLimit + 1024
	outboundEventResultHTTPBodyLimit      = 2*outboundAgentResultBytesLimit + 4096
	httpTransportNDJSONLineLimit          = 1 << 20
	httpTransportHealthTimeout            = 3 * time.Second
	eventAgentHealthTimeout               = 30 * time.Second
	outboundAgentCleanupTimeout           = 10 * time.Second
)

var httpTransportInvokeRetryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}

var outboundAgentInvokeGrace = func(transport string) time.Duration {
	if transport == config.OutboundTransportEvent {
		return 30 * time.Second
	}
	return 5 * time.Second
}

var errOutboundAgentBusy = errors.New(model.JobErrorAgentBusy)

type outboundAgentConnectionError struct {
	err                    error
	requestMayHaveStarted  bool
	requestStartStateKnown bool
}

func (e *outboundAgentConnectionError) Error() string {
	return "outbound agent connection failed"
}

func (e *outboundAgentConnectionError) Unwrap() error {
	return e.err
}

func isOutboundAgentConnectionError(err error) bool {
	var connectionError *outboundAgentConnectionError
	return errors.As(err, &connectionError)
}

func isRetryableOutboundAgentConnectionError(err error) bool {
	var connectionError *outboundAgentConnectionError
	return errors.As(err, &connectionError) && connectionError.requestStartStateKnown && !connectionError.requestMayHaveStarted
}

type outboundAgentHealth struct {
	ProtocolVersion   int
	AgentID           string
	Country           string
	Region            string
	Provider          string
	ISP               string
	Version           string
	Capabilities      []model.Tool
	Protocols         model.ProtocolMask
	MaxConcurrentJobs int
}

type outboundAgentNetworkProbe struct {
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

type outboundAgentNetworkHealth struct {
	CapNetRaw struct {
		Effective bool `json:"effective"`
		Bounded   bool `json:"bounded"`
	} `json:"cap_net_raw"`
	ICMPEchoDatagram struct {
		IPv4 outboundAgentNetworkProbe `json:"ipv4"`
		IPv6 outboundAgentNetworkProbe `json:"ipv6"`
	} `json:"icmp_echo_datagram"`
	PingGroupRange string `json:"ping_group_range"`
}

type outboundRequestAuthorizer interface {
	Authorize(*http.Request) error
}

type outboundNoAuth struct{}

func (outboundNoAuth) Authorize(*http.Request) error { return nil }

type outboundBearerAuth struct{ secret string }

func (a outboundBearerAuth) Authorize(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.secret)
	return nil
}

type outboundHeaderAuth struct {
	header string
	secret string
}

func (a outboundHeaderAuth) Authorize(req *http.Request) error {
	req.Header.Set(a.header, a.secret)
	return nil
}

type outboundHuaweiCloudAKSKAuth struct {
	signer *huaweisign.Signer
}

func (a outboundHuaweiCloudAKSKAuth) Authorize(req *http.Request) error {
	return a.signer.Sign(req)
}

func newOutboundRequestAuthorizer(auth config.OutboundAgentAuth) (outboundRequestAuthorizer, error) {
	switch auth.Type {
	case config.OutboundAuthNone:
		if auth.Header != "" || auth.AccessKey != "" || auth.ProjectID != "" || auth.Secret != "" {
			return nil, errors.New("none outbound auth must not include credentials")
		}
		return outboundNoAuth{}, nil
	case config.OutboundAuthBearer:
		if auth.Header != "" || auth.AccessKey != "" || auth.ProjectID != "" || auth.Secret == "" || !httpguts.ValidHeaderFieldValue(auth.Secret) {
			return nil, errors.New("invalid bearer outbound auth")
		}
		return outboundBearerAuth{secret: auth.Secret}, nil
	case config.OutboundAuthHeader:
		header := http.CanonicalHeaderKey(strings.TrimSpace(auth.Header))
		if auth.AccessKey != "" || auth.ProjectID != "" || !httpguts.ValidHeaderFieldName(header) || config.ReservedHTTPAuthHeader(header) || auth.Secret == "" || !httpguts.ValidHeaderFieldValue(auth.Secret) {
			return nil, errors.New("invalid header outbound auth")
		}
		return outboundHeaderAuth{header: header, secret: auth.Secret}, nil
	case config.OutboundAuthHuaweiCloudAKSK:
		if auth.Header != "" {
			return nil, errors.New("invalid Huawei Cloud AK/SK outbound auth")
		}
		signer, err := huaweisign.New(auth.AccessKey, auth.Secret, auth.ProjectID)
		if err != nil {
			return nil, errors.New("invalid Huawei Cloud AK/SK outbound auth")
		}
		return outboundHuaweiCloudAKSKAuth{signer: signer}, nil
	default:
		return nil, errors.New("unsupported outbound auth type")
	}
}

func authorizeOutboundRequest(req *http.Request, auth config.OutboundAgentAuth) error {
	authorizer, err := newOutboundRequestAuthorizer(auth)
	if err != nil {
		return err
	}
	return authorizer.Authorize(req)
}

func applyOutboundHeaders(req *http.Request, headers map[string]string) error {
	seen := make(map[string]struct{}, len(headers))
	for rawName, value := range headers {
		name := http.CanonicalHeaderKey(strings.TrimSpace(rawName))
		if !httpguts.ValidHeaderFieldName(name) || config.ReservedHTTPAuthHeader(name) || outboundSensitiveHeaderName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return errors.New("invalid outbound request header")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("duplicate outbound request header")
		}
		seen[name] = struct{}{}
		req.Header.Set(name, value)
	}
	return nil
}

func outboundSensitiveHeaderName(header string) bool {
	header = strings.ToLower(header)
	for _, marker := range []string{"auth", "token", "key", "secret", "signature", "credential", "cookie"} {
		if strings.Contains(header, marker) {
			return true
		}
	}
	return false
}

func NewOutboundAgentHTTPClient(tlsConfig OutboundAgentTLS) (*http.Client, error) {
	cfg, err := tlsutil.OutboundClientTLSConfig(tlsConfig.CAFiles, tlsConfig.CertFile, tlsConfig.KeyFile, tlsConfig.ServerName)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport:     outboundHTTPTransport(cfg),
		CheckRedirect: rejectOutboundAgentRedirect,
	}, nil
}

func rejectOutboundAgentRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func outboundHTTPTransport(tlsConfig *tls.Config) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.DialContext = outboundDialContext()
	return transport
}

func outboundDialContext() func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid outbound dial address")
		}
		addresses, err := resolveOutboundHost(ctx, host)
		if err != nil {
			return nil, errors.New("resolve outbound agent host failed")
		}
		if err := validateManagedAgentEndpointAddresses(addresses); err != nil {
			return nil, err
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		var lastErr error
		for _, address := range addresses {
			candidateNetwork := network
			if strings.HasPrefix(network, "tcp") {
				if address.Is4() {
					candidateNetwork = "tcp4"
				} else {
					candidateNetwork = "tcp6"
				}
			}
			conn, dialErr := dialer.DialContext(ctx, candidateNetwork, net.JoinHostPort(address.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("outbound agent host has no addresses")
		}
		return nil, lastErr
	}
}

func validateManagedAgentEndpointAddresses(addresses []netip.Addr) error {
	if len(addresses) == 0 {
		return errors.New("outbound agent host has no addresses")
	}
	for _, address := range addresses {
		if !policy.ManagedAgentEndpointAddressAllowed(address) {
			return errors.New("outbound agent resolves to a forbidden endpoint address")
		}
	}
	return nil
}

func resolveOutboundHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address.Unmap()}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		out = append(out, address.Unmap())
	}
	if len(out) == 0 {
		return nil, errors.New("host has no addresses")
	}
	return out, nil
}

func outboundHTTPClient(agent OutboundAgent) *http.Client {
	if agent.HTTPClient != nil {
		client := *agent.HTTPClient
		client.CheckRedirect = rejectOutboundAgentRedirect
		return &client
	}
	return &http.Client{
		Transport:     outboundHTTPTransport(nil),
		CheckRedirect: rejectOutboundAgentRedirect,
	}
}

func (h *Hub) SetOutboundAgents(agents []OutboundAgent) {
	h.outboundReloadMu.Lock()
	defer h.outboundReloadMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outboundAgents = append([]OutboundAgent(nil), agents...)
}

func (h *Hub) MutateOutboundAgents(runCtx context.Context, agentIDs []string, mutate func() error) error {
	h.outboundReloadMu.Lock()
	defer h.outboundReloadMu.Unlock()
	unlockAgents := h.lockAgentLifecycles(agentIDs...)
	defer unlockAgents()
	if err := mutate(); err != nil {
		return err
	}
	h.invalidateOutboundAgentRuntimes(runCtx, agentIDs)
	agents, err := h.configuredOutboundAgents(runCtx)
	if err != nil {
		// The persistent mutation already succeeded. Keep the affected runtime
		// offline instead of reviving credentials or endpoints from the previous
		// generation; Start's reconciliation loop will retry the authoritative
		// snapshot after a transient store failure.
		h.log.Warn("outbound agent configuration saved; runtime reload deferred", "err", err)
		return nil
	}
	return h.applyOutboundAgentsLocked(runCtx, agents)
}

func (h *Hub) invalidateOutboundAgentRuntimes(ctx context.Context, agentIDs []string) {
	type interruptedJob struct {
		agentID string
		jobID   string
	}
	var interrupted []interruptedJob
	var invalidated []string
	h.mu.Lock()
	for _, agentID := range agentIDs {
		generation := h.outboundAgentGenerations[agentID]
		if generation == 0 {
			continue
		}
		if cancel := h.outboundAgentCancels[agentID]; cancel != nil {
			cancel()
		}
		for _, jobID := range h.takeOutboundRunningJobsLocked(agentID, generation) {
			interrupted = append(interrupted, interruptedJob{agentID: agentID, jobID: jobID})
		}
		if meta, ok := h.meta[agentID]; ok && meta.outboundGeneration == generation {
			delete(h.meta, agentID)
		}
		delete(h.outboundAgentCancels, agentID)
		delete(h.outboundAgentGenerations, agentID)
		if len(h.running[agentID]) == 0 {
			delete(h.running, agentID)
		}
		invalidated = append(invalidated, agentID)
	}
	h.mu.Unlock()
	cleanupCtx, cancelCleanup := newOutboundAgentCleanupContext()
	defer cancelCleanup()
	for _, agentID := range invalidated {
		_ = h.store.MarkAgentOffline(cleanupCtx, agentID)
	}
	for _, job := range interrupted {
		h.failJob(cleanupCtx, job.agentID, job.jobID, model.JobErrorAgentDisconnected)
	}
}

func (h *Hub) MutateAgentRegistry(agentID string, mutate func() error) error {
	unlockAgent := h.lockAgentLifecycles(agentID)
	defer unlockAgent()
	return mutate()
}

// DeleteManagedAgent serializes persistent deletion with both transport
// lifecycles. It revokes any current gRPC session and outbound generation even
// when the store mutation fails partway, so a control-plane delete can never
// leave the old runtime authorized to execute more work.
func (h *Hub) DeleteManagedAgent(runCtx context.Context, agentID string, mutate func() error) error {
	h.outboundReloadMu.Lock()
	defer h.outboundReloadMu.Unlock()
	unlockAgent := h.lockAgentLifecycles(agentID)
	defer unlockAgent()

	mutationErr := mutate()
	h.invalidateOutboundAgentRuntimes(runCtx, []string{agentID})
	runningJobs := h.revokeGRPCSessionForDeletion(agentID)
	h.mu.Lock()
	configured := make([]OutboundAgent, 0, len(h.outboundAgents))
	for _, agent := range h.outboundAgents {
		if agent.ID != agentID {
			configured = append(configured, agent)
		}
	}
	h.outboundAgents = configured
	h.mu.Unlock()
	cleanupCtx, cancelCleanup := newOutboundAgentCleanupContext()
	defer cancelCleanup()
	for _, jobID := range runningJobs {
		h.failJob(cleanupCtx, agentID, jobID, model.JobErrorAgentDisconnected)
	}
	if mutationErr != nil {
		return mutationErr
	}
	return nil
}

func (h *Hub) ReloadOutboundAgents(ctx context.Context) {
	h.outboundReloadMu.Lock()
	defer h.outboundReloadMu.Unlock()
	agents, err := h.configuredOutboundAgents(ctx)
	if err != nil {
		h.log.Warn("reload outbound agents", "err", err)
		return
	}
	unlockAgents := h.lockAgentLifecycles(h.outboundAgentIDsForReload(agents)...)
	defer unlockAgents()
	if err := h.applyOutboundAgentsLocked(ctx, agents); err != nil {
		h.log.Warn("reload outbound agents", "err", err)
	}
}

func (h *Hub) outboundAgentIDsForReload(agents []OutboundAgent) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(agents)+len(h.outboundAgents))
	for _, agent := range agents {
		ids = append(ids, agent.ID)
	}
	for _, agent := range h.outboundAgents {
		ids = append(ids, agent.ID)
	}
	return ids
}

// applyOutboundAgentsLocked applies a snapshot while the reload mutex and all
// agent lifecycle locks affected by the snapshot are held by the caller.
func (h *Hub) applyOutboundAgentsLocked(ctx context.Context, agents []OutboundAgent) error {
	type pendingLoop struct {
		ctx        context.Context
		agent      OutboundAgent
		generation uint64
	}
	type interruptedJob struct {
		agentID string
		jobID   string
	}
	var interrupted []interruptedJob
	h.mu.Lock()
	previous := make(map[string]OutboundAgent, len(h.outboundAgents))
	for _, agent := range h.outboundAgents {
		previous[agent.ID] = agent
	}
	next := make(map[string]OutboundAgent, len(agents))
	for _, agent := range agents {
		next[agent.ID] = agent
	}
	for id, cancel := range h.outboundAgentCancels {
		nextAgent, exists := next[id]
		previousAgent, wasConfigured := previous[id]
		if exists && wasConfigured && previousAgent.configHash == nextAgent.configHash {
			continue
		}
		cancel()
		generation := h.outboundAgentGenerations[id]
		for _, jobID := range h.takeOutboundRunningJobsLocked(id, generation) {
			interrupted = append(interrupted, interruptedJob{agentID: id, jobID: jobID})
		}
		if !exists {
			if meta, ok := h.meta[id]; ok && meta.outboundGeneration == generation {
				delete(h.meta, id)
			}
			delete(h.running, id)
			delete(h.outboundAgentCancels, id)
			delete(h.outboundAgentGenerations, id)
		}
	}
	h.outboundAgents = append([]OutboundAgent(nil), agents...)
	loops := make([]pendingLoop, 0, len(agents))
	for _, agent := range agents {
		previousAgent, wasConfigured := previous[agent.ID]
		if h.outboundAgentCancels[agent.ID] != nil && wasConfigured && previousAgent.configHash == agent.configHash {
			continue
		}
		loopCtx, generation := h.registerOutboundAgentLoopLocked(ctx, agent.ID)
		loops = append(loops, pendingLoop{ctx: loopCtx, agent: agent, generation: generation})
	}
	h.mu.Unlock()
	cleanupCtx, cancelCleanup := newOutboundAgentCleanupContext()
	defer cancelCleanup()
	for id := range previous {
		if _, exists := next[id]; !exists {
			_ = h.store.MarkAgentOffline(cleanupCtx, id)
		}
	}
	for _, job := range interrupted {
		h.failJob(cleanupCtx, job.agentID, job.jobID, model.JobErrorAgentDisconnected)
	}
	for _, loop := range loops {
		go h.runOutboundAgentLoop(loop.ctx, loop.agent, loop.generation)
	}
	return nil
}

func (h *Hub) takeOutboundRunningJobsLocked(agentID string, generation uint64) []string {
	var jobs []string
	for jobID := range h.running[agentID] {
		if h.outboundJobGenerations[jobID] != generation {
			continue
		}
		jobs = append(jobs, jobID)
		if cancel := h.inflightCancels[jobID]; cancel != nil {
			cancel()
		}
		delete(h.running[agentID], jobID)
		delete(h.outboundJobGenerations, jobID)
		delete(h.outboundJobAttempts, jobID)
		delete(h.inflightCancels, jobID)
	}
	return jobs
}

func (h *Hub) startOutboundAgents(ctx context.Context) {
	h.outboundReloadMu.Lock()
	h.mu.Lock()
	agents := append([]OutboundAgent(nil), h.outboundAgents...)
	h.mu.Unlock()
	ids := make([]string, 0, len(agents))
	for _, agent := range agents {
		ids = append(ids, agent.ID)
	}
	unlockAgents := h.lockAgentLifecycles(ids...)
	h.mu.Lock()
	type pendingLoop struct {
		ctx        context.Context
		agent      OutboundAgent
		generation uint64
	}
	loops := make([]pendingLoop, 0, len(agents))
	for _, agent := range agents {
		if agent.ID == "" || agent.URL == "" || (agent.Transport != config.OutboundTransportHTTP && agent.Transport != config.OutboundTransportEvent) {
			h.log.Warn("skip invalid outbound agent", "agent_id", agent.ID, "transport", agent.Transport)
			continue
		}
		if h.outboundAgentCancels[agent.ID] != nil {
			continue
		}
		loopCtx, generation := h.registerOutboundAgentLoopLocked(ctx, agent.ID)
		loops = append(loops, pendingLoop{ctx: loopCtx, agent: agent, generation: generation})
	}
	h.mu.Unlock()
	unlockAgents()
	h.outboundReloadMu.Unlock()
	for _, loop := range loops {
		go h.runOutboundAgentLoop(loop.ctx, loop.agent, loop.generation)
	}
}

func (h *Hub) registerOutboundAgentLoopLocked(parent context.Context, agentID string) (context.Context, uint64) {
	loopCtx, cancel := context.WithCancel(parent)
	h.nextOutboundAgentGeneration++
	if h.nextOutboundAgentGeneration == 0 {
		h.nextOutboundAgentGeneration++
	}
	generation := h.nextOutboundAgentGeneration
	h.outboundAgentCancels[agentID] = cancel
	h.outboundAgentGenerations[agentID] = generation
	return loopCtx, generation
}

func (h *Hub) stopOutboundAgentLoop(ctx context.Context, agentID string, generation uint64) bool {
	unlockAgent := h.lockAgentLifecycles(agentID)
	defer unlockAgent()
	h.mu.Lock()
	if generation == 0 || h.outboundAgentGenerations[agentID] != generation {
		h.mu.Unlock()
		return false
	}
	h.mu.Unlock()
	_ = h.store.MarkAgentOffline(ctx, agentID)
	h.mu.Lock()
	delete(h.outboundAgentCancels, agentID)
	delete(h.outboundAgentGenerations, agentID)
	h.mu.Unlock()
	return true
}

func (h *Hub) markOutboundAgentOffline(ctx context.Context, agentID string, generation uint64) bool {
	unlockAgent := h.lockAgentLifecycles(agentID)
	defer unlockAgent()
	h.mu.Lock()
	if generation == 0 || h.outboundAgentGenerations[agentID] != generation {
		h.mu.Unlock()
		return false
	}
	h.mu.Unlock()
	_ = h.store.MarkAgentOffline(ctx, agentID)
	return true
}

func (h *Hub) touchOutboundAgent(ctx context.Context, agentID string, generation uint64) bool {
	unlockAgent := h.lockAgentLifecycles(agentID)
	defer unlockAgent()
	h.mu.Lock()
	if generation == 0 || h.outboundAgentGenerations[agentID] != generation {
		h.mu.Unlock()
		return false
	}
	meta, ok := h.meta[agentID]
	if !ok || meta.outboundGeneration != generation {
		h.mu.Unlock()
		return false
	}
	meta.lastSeen = time.Now().UTC()
	h.meta[agentID] = meta
	h.mu.Unlock()
	_ = h.store.TouchAgent(ctx, agentID)
	return true
}

func (h *Hub) startConfiguredOutboundAgents(ctx context.Context) {
	agents, err := h.configuredOutboundAgents(ctx)
	if err != nil {
		h.log.Warn("list outbound agents", "err", err)
		h.startOutboundAgents(ctx)
		return
	}
	if len(agents) > 0 {
		h.SetOutboundAgents(agents)
	}
	h.startOutboundAgents(ctx)
}

func (h *Hub) runOutboundAgentReconciler(ctx context.Context) {
	ticker := time.NewTicker(outboundAgentReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.ReloadOutboundAgents(ctx)
		}
	}
}

func (h *Hub) configuredOutboundAgents(ctx context.Context) ([]OutboundAgent, error) {
	nodes, err := h.store.ListOutboundAgents(ctx)
	if err != nil {
		return nil, err
	}
	agents := make([]OutboundAgent, 0, len(nodes))
	for _, node := range nodes {
		if !node.Enabled {
			continue
		}
		beforeFingerprint, err := tlsutil.MaterialFingerprint(node.TLS.CAFiles, node.TLS.CertFile, node.TLS.KeyFile)
		if err != nil {
			h.log.Warn("fingerprint outbound agent tls", "agent_id", node.ID, "err", err)
			continue
		}
		client, err := NewOutboundAgentHTTPClient(OutboundAgentTLS{
			CAFiles: node.TLS.CAFiles, CertFile: node.TLS.CertFile,
			KeyFile: node.TLS.KeyFile, ServerName: node.TLS.ServerName,
		})
		if err != nil {
			h.log.Warn("load outbound agent tls", "agent_id", node.ID, "err", err)
			continue
		}
		afterFingerprint, err := tlsutil.MaterialFingerprint(node.TLS.CAFiles, node.TLS.CertFile, node.TLS.KeyFile)
		if err != nil || beforeFingerprint != afterFingerprint {
			h.log.Warn("outbound agent tls changed while loading", "agent_id", node.ID, "err", err)
			continue
		}
		transport := model.AgentTransportHTTP
		if node.Transport == config.OutboundTransportEvent {
			transport = model.AgentTransportEvent
		}
		agents = append(agents, OutboundAgent{
			ID: node.ID, Labels: model.NormalizeAgentLabels(node.ID, transport, node.Labels),
			Transport: node.Transport, URL: node.URL, Headers: node.Headers,
			Auth: node.Auth, EventResponse: node.EventResponse, HTTPClient: client,
			configHash: outboundAgentConfigHash(node, afterFingerprint),
		})
	}
	return agents, nil
}

func outboundAgentConfigHash(node config.OutboundAgent, tlsMaterial [sha256.Size]byte) [sha256.Size]byte {
	payload, _ := json.Marshal(struct {
		ID            string                   `json:"id"`
		Transport     string                   `json:"transport"`
		URL           string                   `json:"url"`
		Headers       map[string]string        `json:"headers"`
		Auth          config.OutboundAgentAuth `json:"auth"`
		EventResponse string                   `json:"event_response"`
		Labels        []string                 `json:"labels"`
		TLS           config.TLS               `json:"tls"`
		TLSMaterial   [sha256.Size]byte        `json:"tls_material"`
	}{node.ID, node.Transport, node.URL, node.Headers, node.Auth, node.EventResponse, node.Labels, node.TLS, tlsMaterial})
	return sha256.Sum256(payload)
}

func (h *Hub) outboundAgentLoop(ctx context.Context, agent OutboundAgent) {
	loopCtx, cancel := context.WithCancel(ctx)
	unlockAgent := h.lockAgentLifecycles(agent.ID)
	h.mu.Lock()
	if previous := h.outboundAgentCancels[agent.ID]; previous != nil {
		previous()
	}
	h.nextOutboundAgentGeneration++
	if h.nextOutboundAgentGeneration == 0 {
		h.nextOutboundAgentGeneration++
	}
	generation := h.nextOutboundAgentGeneration
	h.outboundAgentCancels[agent.ID] = cancel
	h.outboundAgentGenerations[agent.ID] = generation
	h.mu.Unlock()
	unlockAgent()
	h.runOutboundAgentLoop(loopCtx, agent, generation)
}

type outboundInvokeOutcomeKind uint8

const (
	outboundInvokeReachable outboundInvokeOutcomeKind = iota
	outboundInvokeSucceeded
	outboundInvokeConnectionFailed
)

type outboundInvokeOutcome struct {
	sequence uint64
	kind     outboundInvokeOutcomeKind
}

func (h *Hub) runOutboundAgentLoop(ctx context.Context, agent OutboundAgent, generation uint64) {
	health, probeErr := probeOutboundAgentHealth(ctx, agent)
	recovering := probeErr != nil
	if probeErr == nil {
		probeErr = agent.applyHealth(health)
		recovering = probeErr != nil
	}
	status := model.AgentOnline
	if recovering {
		status = model.AgentOffline
		h.log.Warn("outbound agent startup health check failed", "agent_id", agent.ID, "transport", agent.Transport, "err", probeErr)
	}
	if !h.upsertOutboundAgent(ctx, agent, status, generation) {
		return
	}
	h.log.Info("outbound agent configured", "agent_id", agent.ID, "transport", agent.Transport, "capabilities", agent.Capabilities, "protocols", agent.Protocols, "protocol_version", agent.ProtocolVersion, "version", agent.Version, "status", status)

	pollInterval := h.pollIntervalForOutboundAgent(agent)
	maxHealthInterval, _ := h.outboundAgentRuntimeForAgent(agent)
	healthInterval := maxHealthInterval
	if recovering {
		healthInterval = pollInterval
	}
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()
	healthTimer := time.NewTimer(healthInterval)
	defer healthTimer.Stop()
	outcomes := make(chan outboundInvokeOutcome, maxAdvertisedAgentConcurrency)
	var dispatchSequence uint64
	var latestOutcomeSequence uint64

	for {
		select {
		case <-ctx.Done():
			withOutboundAgentCleanupContext(func(cleanupCtx context.Context) bool {
				return h.stopOutboundAgentLoop(cleanupCtx, agent.ID, generation)
			})
			return
		case outcome := <-outcomes:
			if outcome.sequence < latestOutcomeSequence {
				continue
			}
			latestOutcomeSequence = outcome.sequence
			switch outcome.kind {
			case outboundInvokeConnectionFailed:
				if !withOutboundAgentCleanupContext(func(cleanupCtx context.Context) bool {
					return h.markOutboundAgentOffline(cleanupCtx, agent.ID, generation)
				}) {
					return
				}
				recovering = true
				healthInterval = pollInterval
				resetOutboundTimer(healthTimer, healthInterval)
			case outboundInvokeSucceeded:
				if recovering {
					if !withOutboundAgentCleanupContext(func(cleanupCtx context.Context) bool {
						return h.upsertOutboundAgent(cleanupCtx, agent, model.AgentOnline, generation)
					}) {
						return
					}
					recovering = false
					healthInterval = maxHealthInterval
					resetOutboundTimer(healthTimer, healthInterval)
				} else if !withOutboundAgentCleanupContext(func(cleanupCtx context.Context) bool {
					return h.touchOutboundAgent(cleanupCtx, agent.ID, generation)
				}) {
					return
				}
			case outboundInvokeReachable:
				if !recovering && !withOutboundAgentCleanupContext(func(cleanupCtx context.Context) bool {
					return h.touchOutboundAgent(cleanupCtx, agent.ID, generation)
				}) {
					return
				}
			}
		case <-healthTimer.C:
			health, err := probeOutboundAgentHealth(ctx, agent)
			if err == nil {
				err = agent.applyHealth(health)
			}
			if err != nil {
				if !withOutboundAgentCleanupContext(func(cleanupCtx context.Context) bool {
					return h.markOutboundAgentOffline(cleanupCtx, agent.ID, generation)
				}) {
					return
				}
				recovering = true
				healthInterval = nextOutboundAgentHealthInterval(healthInterval, pollInterval, maxHealthInterval)
				resetOutboundTimer(healthTimer, healthInterval)
				h.log.Debug("outbound agent health check failed", "agent_id", agent.ID, "transport", agent.Transport, "next_check_after", healthInterval.String(), "err", err)
				continue
			}
			if !h.upsertOutboundAgent(ctx, agent, model.AgentOnline, generation) {
				return
			}
			recovering = false
			healthInterval = maxHealthInterval
			resetOutboundTimer(healthTimer, healthInterval)
		case <-pollTicker.C:
			newPollInterval := h.pollIntervalForOutboundAgent(agent)
			if newPollInterval != pollInterval {
				pollInterval = newPollInterval
				pollTicker.Reset(pollInterval)
			}
			maxHealthInterval, _ = h.outboundAgentRuntimeForAgent(agent)
			if recovering {
				continue
			}
			if !h.touchOutboundAgent(ctx, agent.ID, generation) {
				return
			}
			dispatchSequence = h.dispatchOutboundJobs(ctx, agent, generation, dispatchSequence, outcomes)
		}
	}
}

func newOutboundAgentCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), outboundAgentCleanupTimeout)
}

func withOutboundAgentCleanupContext(run func(context.Context) bool) bool {
	ctx, cancel := newOutboundAgentCleanupContext()
	defer cancel()
	return run(ctx)
}

func resetOutboundTimer(timer *time.Timer, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func (h *Hub) dispatchOutboundJobs(ctx context.Context, agent OutboundAgent, generation uint64, sequence uint64, outcomes chan<- outboundInvokeOutcome) uint64 {
	maxInflight := effectiveAgentInflightLimit(h.inflightLimitForOutboundAgent(agent), agent.MaxConcurrentJobs)
	remaining := maxInflight - h.inflightCount(agent.ID)
	if remaining <= 0 {
		return sequence
	}
	agentPolicies := h.policiesForOutboundAgent(agent)
	jobs, err := h.store.ListQueuedJobs(ctx, agent.ID, schedulerKnownTools(), model.ProtocolAll, remaining)
	if err != nil {
		h.log.Warn("list outbound agent queued jobs", "agent_id", agent.ID, "transport", agent.Transport, "err", err)
		return sequence
	}
	for _, job := range jobs {
		p, failureType, err := compatibleQueuedJobPolicy(agentPolicies, agent.Capabilities, agent.Protocols, job)
		if err != nil {
			h.failJob(ctx, agent.ID, job.ID, failureType)
			h.log.Warn("reject incompatible outbound agent job", "agent_id", agent.ID, "job_id", job.ID, "tool", job.Tool, "failure_type", failureType, "err", err)
			continue
		}
		claimed, err := h.store.ClaimQueuedJob(ctx, job.ID, agent.ID)
		if err != nil {
			h.log.Warn("claim outbound agent queued job failed", "job_id", job.ID, "err", err)
			continue
		}
		if !claimed {
			continue
		}
		attempt, ok := h.markInflightForOutboundAgent(agent.ID, job.ID, generation)
		if !ok {
			h.failJob(ctx, agent.ID, job.ID, model.JobErrorAgentDisconnected)
			return sequence
		}
		h.emitStarted(ctx, job)
		spec := toJobSpecWithPolicies(job, p, agentPolicies)
		sequence++
		go h.invokeOutboundAgent(ctx, agent, spec, outcomes, sequence, generation, attempt)
	}
	return sequence
}

func (a *OutboundAgent) applyHealth(health outboundAgentHealth) error {
	if err := validateAgentWireMetadata(health.ProtocolVersion, health.Capabilities, health.Protocols, health.MaxConcurrentJobs); err != nil {
		return err
	}
	if health.AgentID == "" {
		return errors.New("health agent id is required")
	}
	if health.AgentID != a.ID {
		return errors.New("health agent id does not match configured id")
	}
	a.ProtocolVersion = health.ProtocolVersion
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
	a.Capabilities = append([]model.Tool(nil), health.Capabilities...)
	a.Protocols = health.Protocols
	a.MaxConcurrentJobs = health.MaxConcurrentJobs
	return nil
}

func (h *Hub) upsertOutboundAgent(ctx context.Context, agent OutboundAgent, status model.AgentStatus, generation uint64) bool {
	now := time.Now().UTC()
	unlockAgent := h.lockAgentLifecycles(agent.ID)
	defer unlockAgent()
	h.mu.Lock()
	if generation == 0 || h.outboundAgentGenerations[agent.ID] != generation {
		h.mu.Unlock()
		return false
	}
	h.mu.Unlock()
	_ = h.store.UpsertAgent(ctx, model.Agent{
		ID: agent.ID, Country: agent.Country, Region: agent.Region, Provider: agent.Provider, ISP: agent.ISP,
		Version: agent.Version, Capabilities: agent.Capabilities, Protocols: agent.Protocols,
		Status: status, LastSeenAt: now, CreatedAt: now,
	})
	h.mu.Lock()
	h.meta[agent.ID] = agentMeta{
		capabilities: append([]model.Tool(nil), agent.Capabilities...), protocols: agent.Protocols,
		maxConcurrentJobs: agent.MaxConcurrentJobs, lastSeen: now, outboundGeneration: generation,
	}
	if h.running[agent.ID] == nil {
		h.running[agent.ID] = map[string]struct{}{}
	}
	h.mu.Unlock()
	return true
}

func (h *Hub) outboundAgentRuntime() (time.Duration, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	maxHealthInterval := h.outboundAgentMaxHealthInterval
	invokeAttempts := h.outboundAgentInvokeAttempts
	if maxHealthInterval <= 0 {
		maxHealthInterval = defaultOutboundAgentMaxHealthInterval
	}
	if invokeAttempts <= 0 {
		invokeAttempts = defaultOutboundAgentInvokeAttempts
	}
	return maxHealthInterval, invokeAttempts
}

func (h *Hub) outboundAgentRuntimeForAgent(agent OutboundAgent) (time.Duration, int) {
	_, runtime, _ := h.settingsForOutboundAgent(agent)
	return time.Duration(runtime.OutboundMaxHealthIntervalSec) * time.Second, runtime.OutboundInvokeAttempts
}

func (h *Hub) inflightLimitForOutboundAgent(agent OutboundAgent) int {
	scheduler, _, _ := h.settingsForOutboundAgent(agent)
	if scheduler.MaxInflightPerAgent <= 0 {
		return h.inflightLimit()
	}
	return scheduler.MaxInflightPerAgent
}

func (h *Hub) pollIntervalForOutboundAgent(agent OutboundAgent) time.Duration {
	scheduler, _, _ := h.settingsForOutboundAgent(agent)
	if scheduler.PollIntervalSec <= 0 {
		return h.currentPollInterval()
	}
	return time.Duration(scheduler.PollIntervalSec) * time.Second
}

func (h *Hub) policiesForOutboundAgent(agent OutboundAgent) policy.Set {
	_, runtime, toolPolicies := h.settingsForOutboundAgent(agent)
	return h.policiesSnapshot().WithIntersection(toolPolicies, runtime)
}

func (h *Hub) settingsForOutboundAgent(agent OutboundAgent) (config.Scheduler, config.Runtime, map[string]config.Policy) {
	basePolicies := h.policiesSnapshot()
	runtime := basePolicies.Runtime()
	scheduler := h.schedulerSnapshot()
	labels := agent.Labels
	if len(labels) == 0 {
		labels = model.NormalizeAgentLabels(agent.ID, outboundModelTransport(agent.Transport), nil)
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

func outboundModelTransport(transport string) model.AgentTransport {
	if transport == config.OutboundTransportEvent {
		return model.AgentTransportEvent
	}
	return model.AgentTransportHTTP
}

func nextOutboundAgentHealthInterval(current time.Duration, base time.Duration, maxInterval time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maxInterval <= 0 {
		maxInterval = defaultOutboundAgentMaxHealthInterval
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

func probeOutboundAgentHealth(parent context.Context, agent OutboundAgent) (outboundAgentHealth, error) {
	var body []byte
	var err error
	switch agent.Transport {
	case config.OutboundTransportHTTP:
		body, err = probeHTTPTransportHealth(parent, agent)
	case config.OutboundTransportEvent:
		body, err = probeEventAgentHealth(parent, agent)
	default:
		return outboundAgentHealth{}, errors.New("unsupported outbound transport")
	}
	if err != nil {
		return outboundAgentHealth{}, err
	}
	health, err := decodeOutboundAgentHealth(body)
	if err != nil {
		return outboundAgentHealth{}, err
	}
	if health.AgentID == "" {
		return outboundAgentHealth{}, errors.New("outbound agent health missing agent.id")
	}
	if health.AgentID != agent.ID {
		return outboundAgentHealth{}, errors.New("health agent id does not match configured id")
	}
	return health, nil
}

func decodeOutboundAgentHealth(body []byte) (outboundAgentHealth, error) {
	var raw struct {
		Status  string                     `json:"status"`
		Version version.Info               `json:"version"`
		Network outboundAgentNetworkHealth `json:"network"`
		Agent   struct {
			ProtocolVersion   int                `json:"protocol_version"`
			ID                string             `json:"id"`
			Country           string             `json:"country"`
			Region            string             `json:"region"`
			Provider          string             `json:"provider"`
			ISP               string             `json:"isp"`
			Capabilities      []model.Tool       `json:"capabilities"`
			Protocols         model.ProtocolMask `json:"protocols"`
			MaxConcurrentJobs int                `json:"max_concurrent_jobs"`
		} `json:"agent"`
	}
	if err := unmarshalSingleJSON(body, &raw, true); err != nil {
		return outboundAgentHealth{}, errors.New("decode outbound agent health failed")
	}
	if raw.Status != "ok" {
		return outboundAgentHealth{}, errors.New("outbound agent health status is not ok")
	}
	formattedVersion := versionInfoString(raw.Version)
	if formattedVersion == "" {
		return outboundAgentHealth{}, errors.New("outbound agent health missing version.version")
	}
	if err := validateAgentWireMetadata(raw.Agent.ProtocolVersion, raw.Agent.Capabilities, raw.Agent.Protocols, raw.Agent.MaxConcurrentJobs); err != nil {
		return outboundAgentHealth{}, err
	}
	return outboundAgentHealth{
		ProtocolVersion: raw.Agent.ProtocolVersion, AgentID: raw.Agent.ID, Country: raw.Agent.Country,
		Region: raw.Agent.Region, Provider: raw.Agent.Provider, ISP: raw.Agent.ISP,
		Version: formattedVersion, Capabilities: raw.Agent.Capabilities,
		Protocols: raw.Agent.Protocols, MaxConcurrentJobs: raw.Agent.MaxConcurrentJobs,
	}, nil
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

func (h *Hub) invokeOutboundAgent(parent context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, outcomes chan<- outboundInvokeOutcome, sequence uint64, generation uint64, attempt uint64) {
	grace := outboundAgentInvokeGrace(agent.Transport)
	ctx, cancel := context.WithTimeout(parent, time.Duration(spec.TimeoutSeconds)*time.Second+grace)
	if !h.setInflightCancelForOutboundAttempt(spec.ID, attempt, cancel) {
		cancel()
		return
	}
	defer cancel()
	defer h.setInflightCancelForOutboundAttempt(spec.ID, attempt, nil)

	gotSummary := false
	gotBusy := false
	_, attempts := h.outboundAgentRuntimeForAgent(agent)
	err := callOutboundAgentWithAttempts(ctx, agent, spec, attempts, func(ev grpcwire.ResultEvent) {
		if eventType(ev.Event) == "summary" {
			gotSummary = true
			gotBusy = outboundResultIsBusy(ev)
		}
		h.handleOutboundAgentResult(ctx, agent.ID, generation, attempt, ev)
	})
	outcome := outboundInvokeOutcome{sequence: sequence, kind: outboundInvokeReachable}
	if isOutboundAgentConnectionError(err) {
		outcome.kind = outboundInvokeConnectionFailed
	}
	defer func() {
		select {
		case outcomes <- outcome:
		case <-parent.Done():
		}
	}()
	cleanupCtx, cancelCleanup := newOutboundAgentCleanupContext()
	defer cancelCleanup()
	if gotBusy {
		if h.outboundAgentOwnsAttempt(agent.ID, spec.ID, generation, attempt) {
			h.failJob(cleanupCtx, agent.ID, spec.ID, model.JobErrorAgentBusy)
		}
		return
	}

	if errors.Is(err, errOutboundAgentBusy) {
		job, loadErr := h.store.GetJob(cleanupCtx, spec.ID)
		if loadErr != nil {
			h.log.Warn("load busy outbound agent job failed", "agent_id", agent.ID, "job_id", spec.ID, "err", loadErr)
			h.failJob(cleanupCtx, agent.ID, spec.ID, model.JobErrorAgentBusy)
			return
		}
		if !h.requeueBusyJob(cleanupCtx, agent.ID, job, 0, generation, attempt) {
			h.failJob(cleanupCtx, agent.ID, spec.ID, model.JobErrorAgentBusy)
		}
		return
	}
	if err != nil {
		message := err.Error()
		if isOutboundAgentConnectionError(err) {
			message = model.JobErrorAgentDisconnected
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			message = model.JobErrorTimeout
		}
		h.failJob(cleanupCtx, agent.ID, spec.ID, message)
		h.log.Warn("outbound agent job failed", "agent_id", agent.ID, "job_id", spec.ID, "transport", agent.Transport, "err", err)
		return
	}
	job, loadErr := h.store.GetJob(cleanupCtx, spec.ID)
	if loadErr != nil {
		h.failJob(cleanupCtx, agent.ID, spec.ID, model.JobErrorFailed)
		h.log.Warn("load outbound result state failed", "agent_id", agent.ID, "job_id", spec.ID, "err", loadErr)
		return
	}
	if terminalJobStatus(job.Status) {
		h.clearInflight(agent.ID, spec.ID)
		outcome.kind = outboundInvokeSucceeded
		return
	}
	if job.Status == model.JobQueued {
		return
	}
	message := "outbound agent summary was rejected"
	if !gotSummary {
		message = "outbound agent did not return a summary"
	}
	h.failJob(cleanupCtx, agent.ID, spec.ID, message)
	h.log.Warn("outbound agent job did not reach a terminal state", "agent_id", agent.ID, "job_id", spec.ID, "transport", agent.Transport)
}

func outboundResultIsBusy(event grpcwire.ResultEvent) bool {
	if eventType(event.Event) != "summary" {
		return false
	}
	exitCode, ok := intFromPayload(event.Event["exit_code"])
	if !ok || exitCode == 0 {
		return false
	}
	metric, ok := metricMapFromPayload(event.Event["metric"])
	if !ok {
		return false
	}
	status, _ := metric["status"].(string)
	return status == model.JobErrorAgentBusy
}

func callOutboundAgent(ctx context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, onEvent func(grpcwire.ResultEvent)) error {
	return callOutboundAgentWithAttempts(ctx, agent, spec, defaultOutboundAgentInvokeAttempts, onEvent)
}

func callOutboundAgentWithAttempts(ctx context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, attempts int, onEvent func(grpcwire.ResultEvent)) error {
	if agent.Transport == config.OutboundTransportEvent {
		return callEventAgent(ctx, agent, spec, onEvent)
	}
	if agent.Transport != config.OutboundTransportHTTP {
		return errors.New("unsupported outbound transport")
	}
	if attempts <= 0 {
		attempts = defaultOutboundAgentInvokeAttempts
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		attemptHadEvent := false
		err = callHTTPTransportOnce(ctx, agent, spec, func(event grpcwire.ResultEvent) {
			attemptHadEvent = true
			onEvent(event)
		})
		if err == nil || attemptHadEvent || !isRetryableOutboundAgentConnectionError(err) || attempt == attempts || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(httpTransportInvokeRetryDelay(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
	return err
}

func unmarshalSingleJSON(body []byte, dst any, disallowUnknown bool) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing json")
	}
	return nil
}

func parseOuterStatus(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		status, err := strconv.Atoi(number.String())
		if err == nil && (status == 0 || status >= 200 && status < 300) {
			return nil
		}
		return errors.New("event wrapper reports unsuccessful status")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return errors.New("event wrapper status is invalid")
	}
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" || text == "ok" || text == "success" {
		return nil
	}
	if status, err := strconv.Atoi(text); err == nil && status >= 200 && status < 300 {
		return nil
	}
	return errors.New("event wrapper reports unsuccessful status")
}

func outerErrorPresent(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" || string(raw) == `""` {
		return false
	}
	return true
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
		ID: uuid.NewString(), JobID: eventJobID, Stream: "progress",
		Event: &model.StreamEvent{Type: "progress", Message: message}, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.AddJobEvent(ctx, event); err != nil {
		h.log.Warn("store progress event failed", "job_id", jobID, "message", message, "err", err)
		return
	}
	h.publish(event)
}
