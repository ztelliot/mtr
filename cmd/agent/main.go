package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/runner"
	"github.com/ztelliot/mtr/internal/tlsutil"
	"github.com/ztelliot/mtr/internal/version"
	"golang.org/x/net/icmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	defaultSystemAgentConfig     = "/etc/mtr/agent.yaml"
	defaultLocalAgentConfig      = "configs/agent.yaml"
	agentMaxConcurrentJobs       = 4
	agentMaxConcurrentEventReads = 8
	agentMaxConcurrentSpeedtests = 4
	agentMaxEventResultEvents    = 1024
	agentMaxEventResultBytes     = 4 << 20
	agentEventEnvelopeReserve    = 4 << 10
	agentMaxJobIDLength          = 128
	agentHTTPWriteTimeout        = 10 * time.Second
	agentGRPCSendQueueSize       = 128
	agentGRPCSendTimeout         = 10 * time.Second
)

var (
	agentNetworkHealthOnce sync.Once
	agentNetworkHealth     map[string]any
)

func main() {
	prelog := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	cfgPath := flags.String("config", "", "agent config path")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(os.Args[1:]); err != nil {
		prelog.Error("parse flags", "err", err)
		os.Exit(2)
	}
	if *showVersion {
		version.Print("mtr-agent")
		return
	}
	if flags.NArg() > 0 {
		prelog.Error("unexpected agent arguments", "args", flags.Args())
		os.Exit(2)
	}
	if *cfgPath == "" {
		*cfgPath = agentConfigPath()
	}
	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		prelog.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}
	modes, ok := parseAgentModes(cfg.Mode)
	if !ok {
		prelog.Error("invalid agent mode", "mode", cfg.Mode)
		os.Exit(1)
	}
	log := newLogger(cfg.LogLevel)
	log.Info("agent config loaded", "path", *cfgPath, "mode", cfg.Mode, "version", version.String())
	log.Info("agent linux privileges", procStatusLogAttrs()...)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	jobSlots := make(chan struct{}, agentMaxConcurrentJobs)
	errCh := make(chan error, 2)
	var stopHTTP func()
	var httpErr <-chan error
	if modes.http {
		var err error
		stopHTTP, httpErr, err = startHTTPAgentServer(ctx, cfg.HTTPAddr, cfg, log, jobSlots)
		if err != nil {
			log.Error("start http agent", "err", err)
			os.Exit(1)
		}
		defer stopHTTP()
	}
	if modes.event {
		var err error
		stopHTTP, httpErr, err = startEventAgentServer(ctx, cfg.HTTPAddr, cfg, log, jobSlots)
		if err != nil {
			log.Error("start event agent", "err", err)
			os.Exit(1)
		}
		defer stopHTTP()
	}
	if modes.grpc {
		go func() {
			errCh <- run(ctx, cfg, log, jobSlots)
		}()
	}
	if !modes.grpc {
		select {
		case <-ctx.Done():
		case err := <-httpErr:
			if err != nil {
				log.Error("http agent stopped", "err", err)
				os.Exit(1)
			}
		}
		return
	}
	select {
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			log.Error("agent stopped", "err", err)
			os.Exit(1)
		}
	case err := <-httpErr:
		if err != nil && ctx.Err() == nil {
			log.Error("http agent stopped", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
	}
}

func procStatusLogAttrs() []any {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return []any{"proc_status_error", err.Error()}
	}
	wanted := map[string]string{
		"CapBnd":     "",
		"CapEff":     "",
		"CapPrm":     "",
		"NoNewPrivs": "",
		"Seccomp":    "",
	}
	for _, line := range strings.Split(string(status), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if _, exists := wanted[key]; exists {
			wanted[key] = strings.TrimSpace(value)
		}
	}
	return []any{
		"cap_bnd", wanted["CapBnd"],
		"cap_eff", wanted["CapEff"],
		"cap_prm", wanted["CapPrm"],
		"no_new_privs", wanted["NoNewPrivs"],
		"seccomp", wanted["Seccomp"],
	}
}

func agentConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("AGENT_CONFIG")); path != "" {
		return path
	}
	return firstExistingConfig(defaultSystemAgentConfig, defaultLocalAgentConfig)
}

func firstExistingConfig(systemPath string, localPath string) string {
	if _, err := os.Stat(systemPath); err == nil {
		return systemPath
	}
	return localPath
}

type agentModes struct {
	grpc  bool
	http  bool
	event bool
}

func parseAgentModes(raw string) (agentModes, bool) {
	var modes agentModes
	for _, part := range strings.Split(raw, ",") {
		switch part {
		case "grpc":
			if modes.grpc {
				return agentModes{}, false
			}
			modes.grpc = true
		case "http":
			if modes.http {
				return agentModes{}, false
			}
			modes.http = true
		case "event":
			if modes.event {
				return agentModes{}, false
			}
			modes.event = true
		default:
			return agentModes{}, false
		}
	}
	if modes.http && modes.event {
		return agentModes{}, false
	}
	return modes, modes.grpc || modes.http || modes.event
}

func agentCapabilities(in []string) []model.Tool {
	out := make([]model.Tool, 0, len(in))
	for _, item := range in {
		out = append(out, model.Tool(item))
	}
	return out
}

func startHTTPAgentServer(ctx context.Context, listen string, cfg config.Agent, log *slog.Logger, jobSlots chan struct{}) (func(), <-chan error, error) {
	httpAuth, err := config.NormalizeAgentHTTPAuth("http", cfg.HTTPAuth)
	if err != nil {
		return nil, nil, fmt.Errorf("http_auth: %w", err)
	}
	cfg.HTTPAuth = httpAuth
	tlsConfig, err := tlsutil.ServerTLSConfig(cfg.HTTPTLS.CAFiles, cfg.HTTPTLS.CertFile, cfg.HTTPTLS.KeyFile, cfg.HTTPTLS.Enabled)
	if err != nil {
		return nil, nil, fmt.Errorf("http_tls: %w", err)
	}
	return startAgentHTTPServer(ctx, listen, newHTTPAgentHandlerWithJobSlots(cfg, log, jobSlots), tlsConfig, "http agent listening", cfg.ID, log)
}

func startEventAgentServer(ctx context.Context, listen string, cfg config.Agent, log *slog.Logger, jobSlots chan struct{}) (func(), <-chan error, error) {
	httpAuth, err := config.NormalizeAgentHTTPAuth("event", cfg.HTTPAuth)
	if err != nil {
		return nil, nil, fmt.Errorf("http_auth: %w", err)
	}
	cfg.HTTPAuth = httpAuth
	if cfg.HTTPPathPrefix != "" {
		return nil, nil, fmt.Errorf("http_path_prefix is not supported in event mode")
	}
	if cfg.HTTPTLS.Enabled {
		return nil, nil, fmt.Errorf("http_tls is not supported in event mode")
	}
	return startAgentHTTPServer(ctx, listen, newEventAgentHandlerWithJobSlots(cfg, log, jobSlots), nil, "event agent listening", cfg.ID, log)
}

func startAgentHTTPServer(ctx context.Context, listen string, handler http.Handler, tlsConfig *tls.Config, message string, agentID string, log *slog.Logger) (func(), <-chan error, error) {
	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info(message, "addr", listen, "agent_id", agentID, "tls", tlsConfig != nil)
		var err error
		if tlsConfig != nil {
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	stop := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	select {
	case err := <-errCh:
		if err != nil {
			return nil, nil, err
		}
		return func() {}, errCh, nil
	case <-time.After(25 * time.Millisecond):
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, errCh, nil
}

func newHTTPAgentHandler(cfg config.Agent, log *slog.Logger) http.Handler {
	return newHTTPAgentHandlerWithJobSlots(cfg, log, make(chan struct{}, agentMaxConcurrentJobs))
}

func newHTTPAgentHandlerWithJobSlots(cfg config.Agent, log *slog.Logger, jobSlots chan struct{}) http.Handler {
	agentID := cfg.ID
	mux := http.NewServeMux()
	r := runner.New()
	policies := policy.DefaultPolicies()
	invokeAuthLimiter := abuse.NewConfiguredLimiter(abuse.RateLimitConfig{})
	mux.Handle("/speedtest/random", newSpeedtestHandler(cfg.Speedtest, cfg.HTTPAuth, log))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if status, message, ok := authorizeAgentRequest(req, cfg.HTTPAuth, invokeAuthLimiter); !ok {
			writeJSON(w, status, map[string]string{"error": message})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, agentHealthPayload(cfg, agentID))
	})
	mux.HandleFunc("/invoke", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if status, message, ok := authorizeAgentRequest(req, cfg.HTTPAuth, invokeAuthLimiter); !ok {
			writeJSON(w, status, map[string]string{"error": message})
			return
		}
		if !tryAcquireAgentJob(jobSlots) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many concurrent jobs"})
			return
		}
		defer releaseAgentJob(jobSlots)

		var spec grpcwire.JobSpec
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		p, err := prepareAgentJob(&spec, agentID, policies, cfg.Capabilities)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeController := http.NewResponseController(w)
		if err := setAgentWriteDeadline(writeController); err != nil {
			log.Warn("set http agent write deadline failed", "job_id", spec.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unavailable"})
			return
		}
		defer clearAgentWriteDeadline(writeController)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		writeLine := func(v any) error {
			if err := setAgentWriteDeadline(writeController); err != nil {
				return err
			}
			if err := json.NewEncoder(w).Encode(v); err != nil {
				return err
			}
			return writeController.Flush()
		}
		executeAgentJob(req.Context(), cfg, spec, p, r, log, writeLine)
	})
	handler := http.Handler(mux)
	if cfg.HTTPPathPrefix != "" {
		prefixed := http.NewServeMux()
		prefixed.Handle(cfg.HTTPPathPrefix+"/", http.StripPrefix(cfg.HTTPPathPrefix, mux))
		handler = prefixed
	}
	return logHTTPRequests(log, handler)
}

func newEventAgentHandler(cfg config.Agent, log *slog.Logger) http.Handler {
	return newEventAgentHandlerWithJobSlots(cfg, log, make(chan struct{}, agentMaxConcurrentJobs))
}

func newEventAgentHandlerWithJobSlots(cfg config.Agent, log *slog.Logger, jobSlots chan struct{}) http.Handler {
	agentID := cfg.ID
	r := runner.New()
	policies := policy.DefaultPolicies()
	invokeAuthLimiter := abuse.NewConfiguredLimiter(abuse.RateLimitConfig{})
	readSlots := make(chan struct{}, agentMaxConcurrentEventReads)
	mux := http.NewServeMux()
	mux.HandleFunc("/init", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/invoke", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if status, message, ok := authorizeAgentRequest(req, cfg.HTTPAuth, invokeAuthLimiter); !ok {
			writeJSON(w, status, map[string]string{"error": message})
			return
		}
		if !tryAcquireAgentJob(readSlots) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many concurrent requests"})
			return
		}
		readSlotHeld := true
		defer func() {
			if readSlotHeld {
				releaseAgentJob(readSlots)
			}
		}()
		var spec grpcwire.JobSpec
		event := struct {
			Action string `json:"action"`
			*grpcwire.JobSpec
		}{JobSpec: &spec}
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if spec.ProtocolVersion != grpcwire.AgentProtocolVersion {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unsupported agent protocol version %d (required %d)", spec.ProtocolVersion, grpcwire.AgentProtocolVersion)})
			return
		}
		switch event.Action {
		case "healthz":
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, agentHealthPayload(cfg, agentID))
			return
		case "invoke":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported event action"})
			return
		}
		releaseAgentJob(readSlots)
		readSlotHeld = false
		if !tryAcquireAgentJob(jobSlots) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many concurrent jobs"})
			return
		}
		defer releaseAgentJob(jobSlots)

		p, err := prepareAgentJob(&spec, agentID, policies, cfg.Capabilities)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		response := eventInvokeResponse{
			JobID:  spec.ID,
			Events: make([]map[string]any, 0),
		}
		collector := eventResultCollector{
			response:  &response,
			maxEvents: agentMaxEventResultEvents,
			maxBytes:  agentMaxEventResultBytes - agentEventEnvelopeReserve,
		}
		executeAgentJob(req.Context(), cfg, spec, p, r, log, collector.collect)
		if collector.err != nil {
			writeJSONWithDeadline(w, http.StatusRequestEntityTooLarge, map[string]string{"error": collector.err.Error()}, log)
			return
		}
		if response.Result == nil {
			writeJSONWithDeadline(w, http.StatusInternalServerError, map[string]string{"error": "job did not produce a result"}, log)
			return
		}
		encoded, err := json.Marshal(response)
		if err != nil || len(encoded)+1 > agentMaxEventResultBytes {
			writeJSONWithDeadline(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "event result exceeds byte limit"}, log)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSONWithDeadline(w, http.StatusOK, response, log)
	})
	return logHTTPRequests(log, mux)
}

type eventInvokeResponse struct {
	JobID  string           `json:"job_id,omitempty"`
	Events []map[string]any `json:"events"`
	Result map[string]any   `json:"result"`
}

type eventResultCollector struct {
	response  *eventInvokeResponse
	maxEvents int
	maxBytes  int
	bytes     int
	err       error
}

func (c *eventResultCollector) collect(value any) error {
	if c.err != nil {
		return c.err
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("unexpected event payload %T", value)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	size := len(encoded) + 1
	if c.maxBytes <= 0 || size > c.maxBytes-c.bytes {
		c.err = errors.New("event result exceeds byte limit")
		return c.err
	}
	c.bytes += size
	if payload["type"] == "summary" {
		c.response.Result = payload
		return nil
	}
	if c.maxEvents <= 0 || len(c.response.Events) >= c.maxEvents {
		c.err = errors.New("event result exceeds event limit")
		return c.err
	}
	c.response.Events = append(c.response.Events, payload)
	return nil
}

func agentHealthPayload(cfg config.Agent, agentID string) map[string]any {
	return map[string]any{
		"status":  "ok",
		"version": version.Current(),
		"network": agentNetworkHealthPayload(),
		"agent": map[string]any{
			"protocol_version":    grpcwire.AgentProtocolVersion,
			"id":                  agentID,
			"country":             cfg.Country,
			"region":              cfg.Region,
			"provider":            cfg.Provider,
			"isp":                 cfg.ISP,
			"capabilities":        agentCapabilities(cfg.Capabilities),
			"protocols":           model.ProtocolMask(cfg.Protocols),
			"max_concurrent_jobs": agentMaxConcurrentJobs,
		},
	}
}

func agentNetworkHealthPayload() map[string]any {
	agentNetworkHealthOnce.Do(func() {
		agentNetworkHealth = probeAgentNetworkHealth()
	})
	return cloneStringAnyMap(agentNetworkHealth)
}

func probeAgentNetworkHealth() map[string]any {
	capabilities := map[string]any{
		"effective": false,
		"bounded":   false,
	}
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			switch key {
			case "CapEff":
				capabilities["effective"] = capabilitySetContains(strings.TrimSpace(value), 13)
			case "CapBnd":
				capabilities["bounded"] = capabilitySetContains(strings.TrimSpace(value), 13)
			}
		}
	}
	network := map[string]any{
		"cap_net_raw": capabilities,
		"icmp_echo_datagram": map[string]any{
			"ipv4": probeICMPEchoDatagram("udp4", "0.0.0.0"),
			"ipv6": probeICMPEchoDatagram("udp6", "::"),
		},
	}
	if value, err := os.ReadFile("/proc/sys/net/ipv4/ping_group_range"); err == nil {
		network["ping_group_range"] = strings.Join(strings.Fields(string(value)), " ")
	} else {
		network["ping_group_range"] = ""
	}
	return network
}

func cloneStringAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		if nested, ok := value.(map[string]any); ok {
			out[key] = cloneStringAnyMap(nested)
			continue
		}
		out[key] = value
	}
	return out
}

func capabilitySetContains(raw string, capability uint) bool {
	value, err := strconv.ParseUint(raw, 16, 64)
	return err == nil && value&(uint64(1)<<capability) != 0
}

func probeICMPEchoDatagram(network string, address string) map[string]any {
	conn, err := icmp.ListenPacket(network, address)
	if err != nil {
		return map[string]any{"available": false, "error": err.Error()}
	}
	_ = conn.Close()
	return map[string]any{"available": true}
}

func prepareAgentJob(spec *grpcwire.JobSpec, agentID string, policies policy.Set, capabilities []string) (policy.Policy, error) {
	if spec.ProtocolVersion != grpcwire.AgentProtocolVersion {
		return policy.Policy{}, fmt.Errorf("unsupported agent protocol version %d (required %d)", spec.ProtocolVersion, grpcwire.AgentProtocolVersion)
	}
	if !agentHasCapability(capabilities, spec.Tool) {
		return policy.Policy{}, fmt.Errorf("%w: tool %q is not enabled by agent capabilities", errUnsupportedAgentTool, spec.Tool)
	}
	if spec.ID == "" || spec.ID != strings.TrimSpace(spec.ID) || len(spec.ID) > agentMaxJobIDLength {
		return policy.Policy{}, fmt.Errorf("job id must contain 1-%d bytes", agentMaxJobIDLength)
	}
	runtime := policies.Runtime()
	spec.Args = boundedAgentArgs(spec.Tool, spec.Args, runtime)
	createReq := model.CreateJobRequest{
		Tool:           spec.Tool,
		Target:         spec.Target,
		Args:           policies.AgentArgs(spec.Tool, spec.Args),
		IPVersion:      spec.IPVersion,
		AgentID:        agentID,
		ResolveOnAgent: spec.ResolveOnAgent,
	}
	spec.Args = createReq.Args
	p, err := policies.ValidateTrusted(createReq)
	if err != nil {
		return policy.Policy{}, err
	}
	spec.TimeoutSeconds = clampAgentSeconds(spec.TimeoutSeconds, int(p.Timeout/time.Second))
	spec.ProbeTimeoutSeconds = clampAgentSeconds(spec.ProbeTimeoutSeconds, int(p.ProbeTimeout/time.Second))
	spec.ResolveTimeoutSeconds = clampAgentSeconds(spec.ResolveTimeoutSeconds, runtime.ResolveTimeoutSec)
	p.Timeout = time.Duration(spec.TimeoutSeconds) * time.Second
	p.ProbeTimeout = time.Duration(spec.ProbeTimeoutSeconds) * time.Second
	return p, nil
}

func agentHasCapability(capabilities []string, tool model.Tool) bool {
	for _, capability := range agentCapabilities(capabilities) {
		if capability == tool {
			return true
		}
	}
	return false
}

func boundedAgentArgs(tool model.Tool, args map[string]string, runtime config.Runtime) map[string]string {
	out := make(map[string]string, len(args)+2)
	for key, value := range args {
		out[key] = value
	}
	bound := func(key string, maximum int) {
		raw := out[key]
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 || value > maximum {
			value = maximum
		}
		out[key] = strconv.Itoa(value)
	}
	switch tool {
	case model.ToolPing:
		bound("count", runtime.Count)
	case model.ToolTraceroute:
		bound("max_hops", runtime.MaxHops)
	case model.ToolMTR:
		bound("count", runtime.Count)
		bound("max_hops", runtime.MaxHops)
	}
	return out
}

func clampAgentSeconds(value int, maximum int) int {
	if maximum <= 0 {
		return 1
	}
	if value <= 0 || value > maximum {
		return maximum
	}
	return value
}

func tryAcquireAgentJob(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseAgentJob(slots chan struct{}) {
	<-slots
}

func executeAgentJob(ctx context.Context, cfg config.Agent, spec grpcwire.JobSpec, p policy.Policy, r runner.Runner, log *slog.Logger, writeLine func(any) error) {
	jobCtx, cancel := agentJobContext(ctx, p.Timeout)
	defer cancel()
	write := func(value any) error {
		err := writeLine(value)
		if err != nil {
			cancel()
		}
		return err
	}
	sendEvent := func(event model.StreamEvent) error {
		redactStreamEvent(&event, spec.Tool, cfg.HideFirstHops)
		return write(event.WirePayload())
	}

	resolvedTarget, resolvedVersion, err := resolveAgentTarget(jobCtx, spec.Tool, spec.Target, spec.ResolvedTarget, spec.IPVersion, spec.ResolveOnAgent, spec.ResolveTimeoutSeconds, model.ProtocolMask(cfg.Protocols))
	job := model.Job{ID: spec.ID, Tool: spec.Tool, Target: spec.Target, ResolvedTarget: resolvedTarget, Args: spec.Args, IPVersion: resolvedVersion, ResolveOnAgent: spec.ResolveOnAgent}
	if err != nil {
		failureType := agentPreflightFailureType(err)
		_ = sendEvent(model.StreamEvent{Type: failureType, Message: failureType})
		parsed := agentPreflightFailureResult(job, err)
		compactParsedResult(parsed)
		_ = write(parsed.WirePayload())
		log.Warn("agent rejected job", "job_id", spec.ID, "failure_type", failureType, "err", err)
		return
	}

	log.Debug("agent job start", "job_id", spec.ID, "tool", spec.Tool)
	parsed, runErr := r.RunStream(jobCtx, job, p, sendEvent)
	if parsed != nil {
		redactHops(parsed, cfg.HideFirstHops)
		compactParsedResult(parsed)
		_ = write(parsed.WirePayload())
	}
	if runErr != nil {
		log.Warn("agent job failed", "job_id", spec.ID, "err", runErr)
	}
}

func agentJobContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
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

func authorizeAgentRequest(req *http.Request, auth config.HTTPAuth, limiter *abuse.Limiter) (int, string, bool) {
	validScheme := true
	validCredentials := false
	switch auth.Type {
	case config.HTTPAuthNone:
		return 0, "", true
	case config.HTTPAuthBearer:
		values := req.Header.Values("Authorization")
		var token string
		if len(values) == 1 {
			token, validScheme = bearerToken(values[0])
		} else {
			validScheme = false
		}
		validCredentials = validScheme && constantTimeTokenEqual(token, auth.Secret)
	case config.HTTPAuthHeader:
		values := req.Header.Values(auth.Header)
		validCredentials = len(values) == 1 && constantTimeTokenEqual(values[0], auth.Secret)
	default:
		validScheme = false
	}
	if validCredentials {
		return 0, "", true
	}
	if limiter != nil && !limiter.AllowRequest(speedtestClientIP(req)) {
		return http.StatusTooManyRequests, "rate limit exceeded", false
	}
	if auth.Type == config.HTTPAuthBearer && !validScheme {
		return http.StatusUnauthorized, "authorization header must use Bearer scheme", false
	}
	return http.StatusUnauthorized, "invalid token", false
}

func constantTimeTokenEqual(left string, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func logHTTPRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)
		log.Debug("http agent request",
			"method", r.Method,
			"path", r.URL.Path,
			"client_ip", speedtestClientIP(r),
			"status", lrw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *loggingResponseWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func setAgentWriteDeadline(controller *http.ResponseController) error {
	err := controller.SetWriteDeadline(time.Now().Add(agentHTTPWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func clearAgentWriteDeadline(controller *http.ResponseController) {
	_ = controller.SetWriteDeadline(time.Time{})
}

func writeJSONWithDeadline(w http.ResponseWriter, status int, value any, log *slog.Logger) {
	controller := http.NewResponseController(w)
	if err := setAgentWriteDeadline(controller); err != nil {
		log.Warn("set event agent write deadline failed", "err", err)
		return
	}
	defer clearAgentWriteDeadline(controller)
	writeJSON(w, status, value)
}

func run(ctx context.Context, cfg config.Agent, log *slog.Logger, jobSlots chan struct{}) error {
	creds, err := tlsutil.ClientCredentials(cfg.TLS.CAFiles, cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.Enabled, cfg.TLS.ServerName)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		if err := runGRPCSession(ctx, cfg, creds, log, jobSlots); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("agent session ended, retrying", "server_addr", cfg.ServerAddr, "retry_after", backoff.String(), "err", err)
		} else {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("agent session ended, retrying", "server_addr", cfg.ServerAddr, "retry_after", backoff.String())
		}
		if !sleepContext(ctx, backoff) {
			return nil
		}
		backoff = nextRetryDelay(backoff)
	}
}

var errAgentGRPCSendTimeout = errors.New("agent gRPC send timed out")

type agentMessageStream interface {
	Send(*grpcwire.AgentMessage) error
}

type agentSendRequest struct {
	ctx     context.Context
	message *grpcwire.AgentMessage
	done    chan error
}

type agentGRPCSender struct {
	ctx       context.Context
	cancel    context.CancelFunc
	stream    agentMessageStream
	queue     chan agentSendRequest
	timeout   time.Duration
	done      chan struct{}
	errors    chan error
	failOnce  sync.Once
	failureMu sync.Mutex
	failure   error
}

func newAgentGRPCSender(ctx context.Context, cancel context.CancelFunc, stream agentMessageStream, queueSize int, timeout time.Duration) *agentGRPCSender {
	if queueSize <= 0 {
		queueSize = 1
	}
	if timeout <= 0 {
		timeout = agentGRPCSendTimeout
	}
	sender := &agentGRPCSender{
		ctx:     ctx,
		cancel:  cancel,
		stream:  stream,
		queue:   make(chan agentSendRequest, queueSize),
		timeout: timeout,
		done:    make(chan struct{}),
		errors:  make(chan error, 1),
	}
	go sender.run()
	return sender
}

func (s *agentGRPCSender) Send(ctx context.Context, message *grpcwire.AgentMessage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	request := agentSendRequest{ctx: ctx, message: message, done: make(chan error, 1)}
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case s.queue <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.failureError()
	case <-timer.C:
		s.fail(errAgentGRPCSendTimeout)
		return errAgentGRPCSendTimeout
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.failureError()
	case <-timer.C:
		s.fail(errAgentGRPCSendTimeout)
		return errAgentGRPCSendTimeout
	}
}

func (s *agentGRPCSender) Errors() <-chan error {
	return s.errors
}

func (s *agentGRPCSender) run() {
	for {
		select {
		case <-s.ctx.Done():
			s.fail(s.ctx.Err())
			return
		case <-s.done:
			return
		case request := <-s.queue:
			if err := request.ctx.Err(); err != nil {
				request.done <- err
				continue
			}
			result := make(chan error, 1)
			go func(message *grpcwire.AgentMessage) {
				result <- s.stream.Send(message)
			}(request.message)
			timer := time.NewTimer(s.timeout)
			select {
			case err := <-result:
				if !timer.Stop() {
					<-timer.C
				}
				request.done <- err
				if err != nil {
					s.fail(err)
					return
				}
			case <-timer.C:
				request.done <- errAgentGRPCSendTimeout
				s.fail(errAgentGRPCSendTimeout)
				return
			case <-s.done:
				if !timer.Stop() {
					<-timer.C
				}
				request.done <- s.failureError()
				return
			case <-s.ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				request.done <- s.ctx.Err()
				s.fail(s.ctx.Err())
				return
			}
		}
	}
}

func (s *agentGRPCSender) fail(err error) {
	if err == nil {
		err = errors.New("agent gRPC sender stopped")
	}
	s.failOnce.Do(func() {
		s.failureMu.Lock()
		s.failure = err
		s.failureMu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		close(s.done)
		s.errors <- err
	})
}

func (s *agentGRPCSender) failureError() error {
	s.failureMu.Lock()
	defer s.failureMu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	return context.Canceled
}

func runGRPCSession(ctx context.Context, cfg config.Agent, creds credentials.TransportCredentials, log *slog.Logger, jobSlots chan struct{}) error {
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	conn, err := grpc.NewClient(cfg.ServerAddr, grpc.WithTransportCredentials(creds), grpc.WithDefaultCallOptions(grpc.ForceCodec(grpcwire.JSONCodec{}), grpc.MaxCallRecvMsgSize(1<<20)))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := grpcwire.NewControlClient(conn)
	stream, err := client.Connect(sessionCtx)
	if err != nil {
		return err
	}
	hello := newAgentHello(cfg)
	sender := newAgentGRPCSender(sessionCtx, sessionCancel, stream, agentGRPCSendQueueSize, agentGRPCSendTimeout)
	if err := sender.Send(sessionCtx, &grpcwire.AgentMessage{
		Type:  "hello",
		Agent: hello,
	}); err != nil {
		return err
	}
	log.Info("agent connected", "agent_id", cfg.ID, "server_addr", cfg.ServerAddr, "capabilities", hello.Capabilities, "protocols", cfg.Protocols, "protocol_version", hello.ProtocolVersion)

	r := runner.New()
	defaultPolicies := policy.DefaultPolicies()
	var wg sync.WaitGroup
	var runningMu sync.Mutex
	runningCancels := map[string]context.CancelFunc{}
	cancelJob := func(jobID string) bool {
		runningMu.Lock()
		cancel := runningCancels[jobID]
		runningMu.Unlock()
		if cancel == nil {
			return false
		}
		cancel()
		return true
	}
	errCh := make(chan error, 2)
	go heartbeat(sessionCtx, sender)
	go func() {
		errCh <- <-sender.Errors()
	}()
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF || sessionCtx.Err() != nil {
					errCh <- sessionCtx.Err()
					return
				}
				errCh <- err
				return
			}
			if err := validateServerMessage(msg); err != nil {
				errCh <- err
				return
			}
			if msg.Type == "cancel" {
				if cancelJob(msg.Cancel.JobID) {
					log.Debug("job cancel received", "job_id", msg.Cancel.JobID)
				} else {
					log.Debug("job cancel ignored for unknown job", "job_id", msg.Cancel.JobID)
				}
				continue
			}
			jobSpec := msg.Job
			log.Debug("job received", "job_id", jobSpec.ID, "tool", jobSpec.Tool, "ip_version", jobSpec.IPVersion)
			p, err := prepareAgentJob(jobSpec, cfg.ID, defaultPolicies, cfg.Capabilities)
			if err != nil {
				rejectGRPCAgentJob(sessionCtx, sender, jobSpec, err, log)
				continue
			}
			if !tryAcquireAgentJob(jobSlots) {
				rejectGRPCAgentJob(sessionCtx, sender, jobSpec, errAgentBusy, log)
				continue
			}
			jobCtx, cancel := agentJobContext(sessionCtx, p.Timeout)
			runningMu.Lock()
			runningCancels[jobSpec.ID] = cancel
			runningMu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer releaseAgentJob(jobSlots)
				defer cancel()
				defer func() {
					runningMu.Lock()
					delete(runningCancels, jobSpec.ID)
					runningMu.Unlock()
				}()
				start := time.Now()
				resolvedTarget, resolvedVersion, err := resolveAgentTarget(jobCtx, jobSpec.Tool, jobSpec.Target, jobSpec.ResolvedTarget, jobSpec.IPVersion, jobSpec.ResolveOnAgent, jobSpec.ResolveTimeoutSeconds, model.ProtocolMask(cfg.Protocols))
				job := model.Job{ID: jobSpec.ID, Tool: jobSpec.Tool, Target: jobSpec.Target, ResolvedTarget: resolvedTarget, Args: jobSpec.Args, IPVersion: resolvedVersion, ResolveOnAgent: jobSpec.ResolveOnAgent}
				if err != nil {
					failureType := agentPreflightFailureType(err)
					parsed := agentPreflightFailureResult(job, err)
					_ = sendStreamEvent(jobCtx, sender, jobSpec.ID, model.StreamEvent{Type: failureType, Message: failureType})
					_ = sendParsed(jobCtx, sender, jobSpec.ID, parsed)
					log.Warn("agent rejected job", "job_id", jobSpec.ID, "failure_type", failureType, "err", err)
					return
				}
				log.Debug("job start", "job_id", job.ID, "tool", job.Tool)
				parsed, err := r.RunStream(jobCtx, job, p, func(event model.StreamEvent) error {
					redactStreamEvent(&event, jobSpec.Tool, cfg.HideFirstHops)
					return sendStreamEvent(jobCtx, sender, jobSpec.ID, event)
				})
				if parsed != nil {
					redactHops(parsed, cfg.HideFirstHops)
					compactParsedResult(parsed)
					if sendErr := sendParsed(jobCtx, sender, jobSpec.ID, parsed); sendErr != nil {
						log.Warn("send parsed result failed", "job_id", jobSpec.ID, "err", sendErr)
					} else {
						log.Debug("parsed result sent", "job_id", jobSpec.ID, "exit_code", parsed.ExitCode, "duration_ms", time.Since(start).Milliseconds())
					}
				}
				if err != nil {
					log.Warn("job failed", "job_id", jobSpec.ID, "err", err)
				}
			}()
		}
	}()
	err = <-errCh
	sessionCancel()
	wg.Wait()
	return err
}

func validateServerMessage(msg *grpcwire.ServerMessage) error {
	if msg == nil {
		return errors.New("nil server message")
	}
	switch msg.Type {
	case "job":
		if msg.Job == nil || msg.Cancel != nil {
			return errors.New("invalid job server message")
		}
	case "cancel":
		if msg.Cancel == nil || msg.Job != nil || msg.Cancel.JobID == "" || msg.Cancel.JobID != strings.TrimSpace(msg.Cancel.JobID) {
			return errors.New("invalid cancel server message")
		}
	default:
		return fmt.Errorf("unsupported server message type %q", msg.Type)
	}
	return nil
}

func newAgentHello(cfg config.Agent) *grpcwire.AgentHello {
	return &grpcwire.AgentHello{
		ProtocolVersion:   grpcwire.AgentProtocolVersion,
		ID:                cfg.ID,
		Country:           cfg.Country,
		Region:            cfg.Region,
		Provider:          cfg.Provider,
		ISP:               cfg.ISP,
		Version:           version.String(),
		Token:             cfg.RegisterToken,
		Capabilities:      agentCapabilities(cfg.Capabilities),
		Protocols:         model.ProtocolMask(cfg.Protocols),
		MaxConcurrentJobs: agentMaxConcurrentJobs,
	}
}

func rejectGRPCAgentJob(ctx context.Context, sender *agentGRPCSender, spec *grpcwire.JobSpec, err error, log *slog.Logger) {
	job := model.Job{ID: spec.ID, Tool: spec.Tool, Target: spec.Target, Args: spec.Args, IPVersion: spec.IPVersion, ResolveOnAgent: spec.ResolveOnAgent}
	failureType := agentPreflightFailureType(err)
	parsed := agentPreflightFailureResult(job, err)
	if !errors.Is(err, errAgentBusy) {
		_ = sendStreamEvent(ctx, sender, spec.ID, model.StreamEvent{Type: failureType, Message: failureType})
	}
	_ = sendParsed(ctx, sender, spec.ID, parsed)
	log.Warn("agent rejected job", "job_id", spec.ID, "failure_type", failureType, "err", err)
}

func nextRetryDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return time.Second
	}
	next := current * 2
	if next > 300*time.Second {
		return 300 * time.Second
	}
	return next
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func resolveAgentTarget(ctx context.Context, tool model.Tool, target string, resolvedTarget string, version model.IPVersion, resolveOnAgent bool, resolveTimeoutSeconds int, protocols model.ProtocolMask) (string, model.IPVersion, error) {
	_ = resolveOnAgent
	if protocols == 0 || protocols&^model.ProtocolAll != 0 {
		return "", model.IPAny, errUnsupportedAgentProtocol
	}
	if tool == model.ToolDNS {
		if err := policy.ValidateTarget(tool, target); err != nil {
			return "", model.IPAny, err
		}
		if err := validateAgentProtocol(version, protocols); err != nil {
			return "", version, err
		}
		return "", version, nil
	}
	if resolvedTarget != "" {
		if err := policy.ValidateResolvedIP(resolvedTarget, version); err != nil {
			return "", model.IPAny, err
		}
		resolvedVersion := version
		if resolvedVersion == model.IPAny {
			if ip := net.ParseIP(resolvedTarget); ip != nil {
				if ip.To4() != nil {
					resolvedVersion = model.IPv4
				} else {
					resolvedVersion = model.IPv6
				}
			}
		}
		if err := validateAgentProtocol(resolvedVersion, protocols); err != nil {
			return "", resolvedVersion, err
		}
		return resolvedTarget, resolvedVersion, nil
	}

	literalVersion, literal, err := policy.LiteralIPVersion(tool, target)
	if err != nil {
		return "", model.IPAny, err
	}
	if err := policy.ValidateLiteralTarget(ctx, tool, target, version); err != nil {
		return "", model.IPAny, err
	}
	if literal {
		if err := validateAgentProtocol(literalVersion, protocols); err != nil {
			return "", literalVersion, err
		}
		runtime := config.DefaultRuntime()
		pinnedTarget, resolvedVersion, err := policy.ResolveTargetWithRuntime(ctx, tool, target, literalVersion, runtime)
		if err != nil {
			return "", model.IPAny, err
		}
		return pinnedTarget, resolvedVersion, nil
	}

	runtime := config.DefaultRuntime()
	runtime.ResolveTimeoutSec = clampAgentSeconds(resolveTimeoutSeconds, runtime.ResolveTimeoutSec)
	var lastErr error
	candidates := agentTargetVersions(version, protocols)
	if len(candidates) == 0 {
		return "", version, errUnsupportedAgentProtocol
	}
	for _, candidate := range candidates {
		pinnedTarget, resolvedVersion, err := policy.ResolveTargetWithRuntime(ctx, tool, target, candidate, runtime)
		if err == nil {
			if err := validateAgentProtocol(resolvedVersion, protocols); err != nil {
				return "", resolvedVersion, err
			}
			return pinnedTarget, resolvedVersion, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", model.IPAny, lastErr
	}
	return "", version, errUnsupportedAgentProtocol
}

func agentTargetVersions(version model.IPVersion, protocols model.ProtocolMask) []model.IPVersion {
	switch version {
	case model.IPv4:
		if protocols&model.ProtocolIPv4 == 0 {
			return nil
		}
		return []model.IPVersion{model.IPv4}
	case model.IPv6:
		if protocols&model.ProtocolIPv6 == 0 {
			return nil
		}
		return []model.IPVersion{model.IPv6}
	default:
		out := []model.IPVersion{}
		if protocols&model.ProtocolIPv6 != 0 {
			out = append(out, model.IPv6)
		}
		if protocols&model.ProtocolIPv4 != 0 {
			out = append(out, model.IPv4)
		}
		return out
	}
}

var (
	errUnsupportedAgentProtocol = errors.New(model.JobErrorUnsupportedProtocol)
	errUnsupportedAgentTool     = errors.New(model.JobErrorUnsupportedTool)
	errAgentBusy                = errors.New("too many concurrent jobs")
)

func validateAgentProtocol(version model.IPVersion, protocols model.ProtocolMask) error {
	if protocols == 0 || protocols&^model.ProtocolAll != 0 {
		return errUnsupportedAgentProtocol
	}
	required := policy.RequiredProtocol(version)
	if required == 0 {
		return nil
	}
	if protocols&required == 0 {
		return errUnsupportedAgentProtocol
	}
	return nil
}

func agentPreflightFailureType(err error) string {
	if errors.Is(err, errAgentBusy) {
		return model.JobErrorAgentBusy
	}
	if errors.Is(err, errUnsupportedAgentTool) || model.PublicJobErrorType(err.Error()) == model.JobErrorUnsupportedTool {
		return model.JobErrorUnsupportedTool
	}
	if errors.Is(err, errUnsupportedAgentProtocol) || model.PublicJobErrorType(err.Error()) == model.JobErrorUnsupportedProtocol {
		return model.JobErrorUnsupportedProtocol
	}
	return model.JobErrorTargetBlocked
}

func agentPreflightFailureResult(job model.Job, err error) *model.ToolResult {
	failureType := agentPreflightFailureType(err)
	summary := map[string]any{"status": failureType}
	if err != nil {
		summary["error"] = err.Error()
	}
	return &model.ToolResult{
		Tool:      job.Tool,
		Target:    job.Target,
		IPVersion: job.IPVersion,
		ExitCode:  -1,
		Summary:   summary,
	}
}

func newSpeedtestHandler(cfg config.Speedtest, auth config.HTTPAuth, log *slog.Logger) http.Handler {
	return newSpeedtestHandlerWithSlots(cfg, auth, log, make(chan struct{}, agentMaxConcurrentSpeedtests))
}

func newSpeedtestHandlerWithSlots(cfg config.Speedtest, auth config.HTTPAuth, log *slog.Logger, slots chan struct{}) http.Handler {
	if cfg.MaxBytes <= 0 {
		return http.NotFoundHandler()
	}
	limiter := abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: cfg.GlobalRequestsPerMinute, Burst: cfg.GlobalBurst},
		IP:     abuse.Limit{RequestsPerMinute: cfg.IPRequestsPerMinute, Burst: cfg.IPBurst},
		CIDR: abuse.CIDRLimit{
			Limit:      abuse.Limit{RequestsPerMinute: cfg.GlobalRequestsPerMinute, Burst: cfg.GlobalBurst},
			IPv4Prefix: 32,
			IPv6Prefix: 128,
		},
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := speedtestClientIP(r)
		log.Debug("speedtest request", "method", r.Method, "path", r.URL.Path, "client_ip", clientIP)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !limiter.AllowRequest(clientIP) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		if status, message, ok := authorizeAgentRequest(r, auth, nil); !ok {
			writeJSON(w, status, map[string]string{"error": message})
			return
		}
		if !tryAcquireAgentJob(slots) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many concurrent speedtests"})
			return
		}
		defer releaseAgentJob(slots)
		size := cfg.DefaultBytes
		if raw := r.URL.Query().Get("bytes"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bytes"})
				return
			}
			size = n
		}
		if size > cfg.MaxBytes {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bytes exceeds max"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("X-Speedtest-Bytes", strconv.FormatInt(size, 10))
		writeController := http.NewResponseController(w)
		if err := setAgentWriteDeadline(writeController); err != nil {
			log.Debug("set speedtest write deadline failed", "client_ip", clientIP, "err", err)
			return
		}
		defer clearAgentWriteDeadline(writeController)
		if r.Method == http.MethodHead {
			return
		}
		deadlineWriter := &refreshingDeadlineWriter{writer: w, controller: writeController}
		if _, err := io.CopyN(deadlineWriter, rand.Reader, size); err != nil {
			log.Debug("speedtest write failed", "client_ip", clientIP, "err", err)
		}
	})
}

type refreshingDeadlineWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w *refreshingDeadlineWriter) Write(p []byte) (int, error) {
	if err := setAgentWriteDeadline(w.controller); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

func speedtestClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel}))
}

func redactHops(result *model.ToolResult, count int) {
	if result == nil || count <= 0 || !hideHopsApplies(result.Tool) {
		return
	}
	for i := range result.Hops {
		if i >= count {
			return
		}
		result.Hops[i] = timeoutHop(result.Hops[i].Index)
	}
}

func redactStreamEvent(event *model.StreamEvent, tool model.Tool, count int) {
	if event == nil || event.Hop == nil || count <= 0 || !hideHopsApplies(tool) {
		return
	}
	if event.Hop.Index <= count {
		*event.Hop = timeoutHop(event.Hop.Index)
	}
}

func hideHopsApplies(tool model.Tool) bool {
	return tool == model.ToolTraceroute || tool == model.ToolMTR
}

func timeoutHop(index int) model.HopResult {
	return model.HopResult{Index: index, Timeout: true}
}

func sendStreamEvent(ctx context.Context, sender *agentGRPCSender, jobID string, event model.StreamEvent) error {
	return sender.Send(ctx, &grpcwire.AgentMessage{
		Type: "result",
		Result: &grpcwire.AgentResult{
			JobID: jobID,
			Event: event.WirePayload(),
		},
	})
}

func compactParsedResult(result *model.ToolResult) {
	if result == nil {
		return
	}
	result.Type = "summary"
	switch result.Tool {
	case model.ToolTraceroute, model.ToolMTR:
		result.Hops = nil
	}
}

func sendParsed(ctx context.Context, sender *agentGRPCSender, jobID string, parsed *model.ToolResult) error {
	compactParsedResult(parsed)
	return sender.Send(ctx, &grpcwire.AgentMessage{
		Type: "result",
		Result: &grpcwire.AgentResult{
			JobID: jobID,
			Event: parsed.WirePayload(),
		},
	})
}

func heartbeat(ctx context.Context, sender *agentGRPCSender) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sender.Send(ctx, &grpcwire.AgentMessage{Type: "heartbeat", Heartbeat: &grpcwire.Heartbeat{}}); err != nil {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
