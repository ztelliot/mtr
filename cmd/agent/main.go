package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	defaultSystemAgentConfig = "/etc/mtr/agent.yaml"
	defaultLocalAgentConfig  = "configs/agent.yaml"
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
	errCh := make(chan error, 2)
	var stopHTTP func()
	var httpErr <-chan error
	if modes.http {
		var err error
		stopHTTP, httpErr, err = startHTTPAgentServer(ctx, cfg.HTTPAddr, cfg, log)
		if err != nil {
			log.Error("start http agent", "err", err)
			os.Exit(1)
		}
		defer stopHTTP()
	}
	if modes.grpc {
		go func() {
			errCh <- run(ctx, cfg, log)
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
	grpc bool
	http bool
}

func parseAgentModes(raw string) (agentModes, bool) {
	var modes agentModes
	for _, part := range strings.Split(raw, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "grpc":
			modes.grpc = true
		case "http":
			modes.http = true
		case "both", "all":
			modes.grpc = true
			modes.http = true
		case "":
		default:
			return agentModes{}, false
		}
	}
	return modes, modes.grpc || modes.http
}

func agentCapabilities(in []string) []model.Tool {
	out := make([]model.Tool, 0, len(in))
	for _, item := range in {
		if item != "" {
			out = append(out, model.Tool(item))
		}
	}
	return out
}

func startHTTPAgentServer(ctx context.Context, listen string, cfg config.Agent, log *slog.Logger) (func(), <-chan error, error) {
	cfg.ID = defaultHTTPAgentID(cfg.ID)
	handler := newHTTPAgentHandler(cfg, log)
	tlsConfig, err := tlsutil.ServerTLSConfig(cfg.HTTPTLS.CAFiles, cfg.HTTPTLS.CertFile, cfg.HTTPTLS.KeyFile, cfg.HTTPTLS.Enabled)
	if err != nil {
		return nil, nil, fmt.Errorf("http_tls: %w", err)
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http agent listening", "addr", listen, "agent_id", cfg.ID, "tls", tlsConfig != nil)
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
	agentID := defaultHTTPAgentID(cfg.ID)
	mux := http.NewServeMux()
	r := runner.New()
	policies := policy.DefaultPolicies()
	mux.Handle("/speedtest/random", newSpeedtestHandler(cfg.Speedtest, cfg.HTTPToken, log))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"version": version.Current(),
			"agent": map[string]any{
				"id":           agentID,
				"country":      cfg.Country,
				"region":       cfg.Region,
				"provider":     cfg.Provider,
				"isp":          cfg.ISP,
				"labels":       cfg.Labels,
				"capabilities": agentCapabilities(cfg.Capabilities),
				"protocols":    model.ProtocolMask(cfg.Protocols),
			},
		})
	})
	mux.HandleFunc("/invoke", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if cfg.HTTPToken != "" && strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ") != cfg.HTTPToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}

		var spec grpcwire.JobSpec
		if err := json.NewDecoder(req.Body).Decode(&spec); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		createReq := model.CreateJobRequest{
			Tool:           spec.Tool,
			Target:         spec.Target,
			Args:           policy.AgentArgs(spec.Tool, spec.Args),
			IPVersion:      spec.IPVersion,
			AgentID:        agentID,
			ResolveOnAgent: spec.ResolveOnAgent,
		}
		spec.Args = createReq.Args
		p, err := policies.ValidateTrusted(createReq)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if spec.TimeoutSeconds > 0 {
			p.Timeout = time.Duration(spec.TimeoutSeconds) * time.Second
		}
		if spec.ProbeTimeoutSeconds > 0 {
			p.ProbeTimeout = time.Duration(spec.ProbeTimeoutSeconds) * time.Second
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		writeLine := func(v any) error {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}
		sendEvent := func(event model.StreamEvent) error {
			redactStreamEvent(&event, spec.Tool, spec.HideFirstHops)
			redactStreamEvent(&event, spec.Tool, cfg.HideFirstHops)
			return writeLine(event.WirePayload())
		}

		resolvedTarget, resolvedVersion, err := resolveAgentTarget(req.Context(), spec.Tool, spec.Target, spec.ResolvedTarget, spec.IPVersion, spec.ResolveOnAgent, spec.ResolveTimeoutSeconds, model.ProtocolMask(cfg.Protocols))
		job := model.Job{ID: spec.ID, Tool: spec.Tool, Target: spec.Target, ResolvedTarget: resolvedTarget, Args: spec.Args, IPVersion: resolvedVersion, ResolveOnAgent: spec.ResolveOnAgent}
		if err != nil {
			_ = sendEvent(model.StreamEvent{Type: "message", Message: "target_blocked"})
			parsed := &model.ToolResult{Tool: spec.Tool, Target: spec.Target, ExitCode: -1, Summary: map[string]any{"error": err.Error()}}
			compactParsedResult(parsed)
			_ = writeLine(parsed.WirePayload())
			log.Warn("target blocked", "job_id", spec.ID, "err", err)
			return
		}

		log.Debug("http job start", "job_id", spec.ID, "tool", spec.Tool, "target", spec.Target)
		parsed, runErr := r.RunStream(req.Context(), job, p, sendEvent)
		if parsed != nil {
			redactHops(parsed, spec.HideFirstHops)
			redactHops(parsed, cfg.HideFirstHops)
			compactParsedResult(parsed)
			_ = writeLine(parsed.WirePayload())
		}
		if runErr != nil {
			log.Warn("http job failed", "job_id", spec.ID, "err", runErr)
		}
	})
	handler := http.Handler(mux)
	if cfg.HTTPPathPrefix != "" {
		prefixed := http.NewServeMux()
		prefixed.Handle(cfg.HTTPPathPrefix+"/", http.StripPrefix(cfg.HTTPPathPrefix, mux))
		handler = prefixed
	}
	return logHTTPRequests(log, handler)
}

func logHTTPRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)
		log.Debug("http agent request",
			"method", r.Method,
			"path", r.URL.Path,
			"client_ip", httpClientIP(r),
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
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func defaultHTTPAgentID(agentID string) string {
	if agentID != "" {
		return agentID
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		return hostname
	}
	return "unknown"
}

func run(ctx context.Context, cfg config.Agent, log *slog.Logger) error {
	creds, err := tlsutil.ClientCredentials(cfg.TLS.CAFiles, cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.Enabled)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		if err := runGRPCSession(ctx, cfg, creds, log); err != nil {
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

func runGRPCSession(ctx context.Context, cfg config.Agent, creds credentials.TransportCredentials, log *slog.Logger) error {
	conn, err := grpc.NewClient(cfg.ServerAddr, grpc.WithTransportCredentials(creds), grpc.WithDefaultCallOptions(grpc.ForceCodec(grpcwire.JSONCodec{})))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := grpcwire.NewControlClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}
	caps := agentCapabilities(cfg.Capabilities)
	if err := stream.Send(&grpcwire.AgentMessage{
		Type: "hello",
		Agent: &grpcwire.AgentHello{
			ID:           cfg.ID,
			Country:      cfg.Country,
			Region:       cfg.Region,
			Provider:     cfg.Provider,
			ISP:          cfg.ISP,
			Version:      version.String(),
			Labels:       cfg.Labels,
			Token:        cfg.RegisterToken,
			Capabilities: caps,
			Protocols:    model.ProtocolMask(cfg.Protocols),
		},
	}); err != nil {
		return err
	}
	log.Info("agent connected", "agent_id", cfg.ID, "server_addr", cfg.ServerAddr, "capabilities", caps, "protocols", cfg.Protocols)

	r := runner.New()
	defaultPolicies := policy.DefaultPolicies()
	var wg sync.WaitGroup
	var sendMu sync.Mutex
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
	go heartbeat(ctx, stream, &sendMu, errCh)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF || ctx.Err() != nil {
					errCh <- ctx.Err()
					return
				}
				errCh <- err
				return
			}
			if msg.Type == "cancel" && msg.Cancel != nil {
				if cancelJob(msg.Cancel.JobID) {
					log.Debug("job cancel received", "job_id", msg.Cancel.JobID)
				} else {
					log.Debug("job cancel ignored for unknown job", "job_id", msg.Cancel.JobID)
				}
				continue
			}
			if msg.Type != "job" || msg.Job == nil {
				continue
			}
			jobSpec := msg.Job
			log.Debug("job received", "job_id", jobSpec.ID, "tool", jobSpec.Tool, "target", jobSpec.Target, "ip_version", jobSpec.IPVersion)
			p, ok := defaultPolicies.Get(jobSpec.Tool)
			if !ok {
				continue
			}
			if jobSpec.TimeoutSeconds > 0 {
				p.Timeout = time.Duration(jobSpec.TimeoutSeconds) * time.Second
			}
			if jobSpec.ProbeTimeoutSeconds > 0 {
				p.ProbeTimeout = time.Duration(jobSpec.ProbeTimeoutSeconds) * time.Second
			}
			jobSpec.Args = policy.AgentArgs(jobSpec.Tool, jobSpec.Args)
			jobCtx, cancel := context.WithCancel(ctx)
			runningMu.Lock()
			runningCancels[jobSpec.ID] = cancel
			runningMu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
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
					parsed := &model.ToolResult{
						Tool:     job.Tool,
						Target:   job.Target,
						ExitCode: -1,
						Summary:  map[string]any{"error": err.Error()},
					}
					_ = sendStreamEvent(stream, &sendMu, jobSpec.ID, model.StreamEvent{Type: "message", Message: "target_blocked"})
					sendParsed(stream, &sendMu, jobSpec.ID, parsed)
					log.Warn("target blocked", "job_id", jobSpec.ID, "err", err)
					return
				}
				log.Debug("job start", "job_id", job.ID, "tool", job.Tool)
				parsed, err := r.RunStream(jobCtx, job, p, func(event model.StreamEvent) error {
					redactStreamEvent(&event, jobSpec.Tool, jobSpec.HideFirstHops)
					redactStreamEvent(&event, jobSpec.Tool, cfg.HideFirstHops)
					return sendStreamEvent(stream, &sendMu, jobSpec.ID, event)
				})
				if parsed != nil {
					redactHops(parsed, jobSpec.HideFirstHops)
					redactHops(parsed, cfg.HideFirstHops)
					compactParsedResult(parsed)
					if sendErr := sendParsed(stream, &sendMu, jobSpec.ID, parsed); sendErr != nil {
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
	wg.Wait()
	return err
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
	if resolveOnAgent {
		runtime := config.DefaultRuntime()
		if resolveTimeoutSeconds > 0 {
			runtime.ResolveTimeoutSec = resolveTimeoutSeconds
		}
		var lastErr error
		for _, candidate := range agentTargetVersions(version, protocols) {
			resolvedTarget, resolvedVersion, err := policy.ResolveTargetWithRuntime(ctx, tool, target, candidate, runtime)
			if err == nil {
				return resolvedTarget, resolvedVersion, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return "", model.IPAny, lastErr
		}
		return "", model.IPAny, fmt.Errorf("agent does not support requested IP version")
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
		return resolvedTarget, resolvedVersion, nil
	}
	if err := policy.ValidateLiteralTarget(ctx, tool, target, version); err != nil {
		return "", model.IPAny, err
	}
	return "", version, nil
}

func agentTargetVersions(version model.IPVersion, protocols model.ProtocolMask) []model.IPVersion {
	if protocols == 0 {
		protocols = model.ProtocolAll
	}
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

func newSpeedtestHandler(cfg config.Speedtest, token string, log *slog.Logger) http.Handler {
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
		clientIP := httpClientIP(r)
		log.Debug("speedtest request", "method", r.Method, "path", r.URL.Path, "client_ip", clientIP)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if token != "" && r.URL.Query().Get("token") != token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		if !limiter.AllowRequest(clientIP) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
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
		if r.Method == http.MethodHead {
			return
		}
		if _, err := io.CopyN(w, rand.Reader, size); err != nil {
			log.Debug("speedtest write failed", "client_ip", clientIP, "err", err)
		}
	})
}

func httpClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
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

func sendStreamEvent(stream grpcwire.Control_ConnectClient, sendMu *sync.Mutex, jobID string, event model.StreamEvent) error {
	sendMu.Lock()
	defer sendMu.Unlock()
	return stream.Send(&grpcwire.AgentMessage{
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

func sendParsed(stream grpcwire.Control_ConnectClient, sendMu *sync.Mutex, jobID string, parsed *model.ToolResult) error {
	compactParsedResult(parsed)
	sendMu.Lock()
	defer sendMu.Unlock()
	return stream.Send(&grpcwire.AgentMessage{
		Type: "result",
		Result: &grpcwire.AgentResult{
			JobID: jobID,
			Event: parsed.WirePayload(),
		},
	})
}

func heartbeat(ctx context.Context, stream grpcwire.Control_ConnectClient, sendMu *sync.Mutex, errCh chan<- error) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		case <-ticker.C:
			sendMu.Lock()
			if err := stream.Send(&grpcwire.AgentMessage{Type: "heartbeat", Heartbeat: &grpcwire.Heartbeat{}}); err != nil {
				sendMu.Unlock()
				errCh <- err
				return
			}
			sendMu.Unlock()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
