package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/api"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/scheduler"
	"github.com/ztelliot/mtr/internal/store"
	"github.com/ztelliot/mtr/internal/tlsutil"
	"github.com/ztelliot/mtr/internal/version"
	"google.golang.org/grpc"
)

const (
	defaultSystemServerConfig = "/etc/mtr/server.yaml"
	defaultLocalServerConfig  = "configs/server.yaml"
)

func main() {
	cfgPath := flag.String("config", "", "server config path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		version.Print("mtr-server")
		return
	}
	if *cfgPath == "" {
		*cfgPath = serverConfigPath()
	}

	cfg, err := config.LoadServer(*cfgPath)
	log := newLogger(cfg.LogLevel)
	if err != nil {
		log.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}
	log.Info("server config loaded", "path", *cfgPath, "version", version.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var st store.Store
	var closer func()
	switch {
	case isSQLiteDSN(cfg.DatabaseURL):
		sqlite, err := store.NewSQLite(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Error("connect sqlite", "err", err)
			os.Exit(1)
		}
		st = sqlite
		closer = sqlite.Close
	case cfg.DatabaseURL != "":
		pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Error("connect postgres", "err", err)
			os.Exit(1)
		}
		st = pg
		closer = pg.Close
	default:
		st = store.NewMemory()
		closer = func() {}
	}
	defer closer()

	policies := policy.FromConfig(cfg.ToolPolicies, cfg.Runtime)
	hub := scheduler.NewHub(
		st,
		policies,
		cfg.RegisterToken,
		time.Duration(cfg.Scheduler.AgentOfflineAfterSec)*time.Second,
		time.Duration(cfg.Scheduler.PollIntervalSec)*time.Second,
		cfg.Scheduler.GRPCMaxInflightPerAgent,
		log,
	)
	hub.SetOutboundRuntime(
		time.Duration(cfg.Runtime.OutboundMaxHealthIntervalSec)*time.Second,
		cfg.Runtime.OutboundInvokeAttempts,
	)
	hub.SetInflightLimits(cfg.Scheduler.GRPCMaxInflightPerAgent, cfg.Scheduler.OutboundMaxInflightPerAgent)
	outboundAgents, err := toSchedulerOutboundAgents(cfg.OutboundAgents, cfg.OutboundTLS)
	if err != nil {
		log.Error("load outbound agent tls", "err", err)
		os.Exit(1)
	}
	hub.SetOutboundAgents(outboundAgents)
	go hub.Start(ctx)
	limiter, err := abuse.NewConfiguredLimiterWithError(abuse.RateLimitConfig{
		Global: abuse.Limit{
			RequestsPerMinute: cfg.RateLimit.Global.RequestsPerMinute,
			Burst:             cfg.RateLimit.Global.Burst,
		},
		IP: abuse.Limit{
			RequestsPerMinute: cfg.RateLimit.IP.RequestsPerMinute,
			Burst:             cfg.RateLimit.IP.Burst,
		},
		CIDR: abuse.CIDRLimit{
			Limit: abuse.Limit{
				RequestsPerMinute: cfg.RateLimit.CIDR.RequestsPerMinute,
				Burst:             cfg.RateLimit.CIDR.Burst,
			},
			IPv4Prefix: cfg.RateLimit.CIDR.IPv4Prefix,
			IPv6Prefix: cfg.RateLimit.CIDR.IPv6Prefix,
		},
		Tools:       toAbuseToolLimits(cfg.RateLimit.Tools),
		ExemptCIDRs: cfg.RateLimit.ExemptCIDRs,
	})
	if err != nil {
		log.Error("configure rate limit", "err", err)
		os.Exit(1)
	}
	creds, err := tlsutil.ServerCredentials(cfg.TLS.CAFiles, cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.Enabled)
	if err != nil {
		log.Error("load grpc tls", "err", err)
		os.Exit(1)
	}
	grpcSrv := grpc.NewServer(grpc.Creds(creds), grpc.ForceServerCodec(grpcwire.JSONCodec{}))
	grpcwire.RegisterControlServer(grpcSrv, hub)
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		log.Error("listen grpc", "err", err)
		os.Exit(1)
	}
	go func() {
		log.Info("grpc listening", "addr", cfg.GRPCAddr)
		if err := grpcSrv.Serve(lis); err != nil {
			log.Error("grpc serve", "err", err)
		}
	}()

	handler, err := api.NewWithOptions(st, policies, limiter, hub, cfg.GeoIPURL, log, api.Options{
		TrustedProxies:  cfg.TrustedProxies,
		ClientIPHeaders: cfg.ClientIPHeaders,
	}, toAPITokenConfigs(cfg.APITokenPermissions)...)
	if err != nil {
		log.Error("configure http client ip", "err", err)
		os.Exit(1)
	}
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http serve", "err", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	grpcSrv.GracefulStop()
}

func serverConfigPath() string {
	return firstExistingConfig(defaultSystemServerConfig, defaultLocalServerConfig)
}

func firstExistingConfig(systemPath string, localPath string) string {
	if _, err := os.Stat(systemPath); err == nil {
		return systemPath
	}
	return localPath
}

func toAbuseToolLimits(in map[string]config.ToolLimitSpec) map[string]abuse.ToolLimit {
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

func toSchedulerOutboundAgents(in []config.OutboundAgent, tlsConfig config.TLS) ([]scheduler.OutboundAgent, error) {
	out := make([]scheduler.OutboundAgent, 0, len(in))
	client, err := scheduler.NewOutboundHTTPClient(scheduler.OutboundTLS{
		Enabled:  tlsConfig.Enabled,
		CAFiles:  tlsConfig.CAFiles,
		CertFile: tlsConfig.CertFile,
		KeyFile:  tlsConfig.KeyFile,
	})
	if err != nil {
		return nil, fmt.Errorf("outbound tls: %w", err)
	}
	for _, agent := range in {
		out = append(out, scheduler.OutboundAgent{
			ID:         agent.ID,
			BaseURL:    agent.BaseURL,
			Token:      agent.HTTPToken,
			Labels:     agent.Labels,
			HTTPClient: client,
		})
	}
	return out, nil
}

func toAPITokenConfigs(in []config.APITokenPermission) []api.TokenConfig {
	out := make([]api.TokenConfig, 0, len(in))
	for _, scope := range in {
		tools := make(map[model.Tool]api.ToolScope, len(scope.Tools))
		for tool, toolScope := range scope.Tools {
			versions := make([]model.IPVersion, 0, len(toolScope.IPVersions))
			for _, version := range toolScope.IPVersions {
				versions = append(versions, model.IPVersion(version))
			}
			tools[model.Tool(tool)] = api.ToolScope{
				AllowedArgs:    toolScope.AllowedArgs,
				ResolveOnAgent: toolScope.ResolveOnAgent,
				IPVersions:     versions,
			}
		}
		out = append(out, api.TokenConfig{
			Token: scope.Secret,
			Scope: api.TokenScope{
				All:            scope.All,
				ScheduleAccess: api.ScheduleAccess(scope.ScheduleAccess),
				Agents:         scope.Agents,
				Tools:          tools,
			},
		})
	}
	return out
}

func isSQLiteDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "sqlite:") ||
		strings.HasPrefix(dsn, "file:") ||
		strings.HasSuffix(dsn, ".db") ||
		strings.HasSuffix(dsn, ".sqlite")
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
