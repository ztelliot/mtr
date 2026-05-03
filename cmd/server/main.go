package main

import (
	"context"
	"flag"
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
		mem := store.NewMemory()
		settings, err := mem.GetManagedSettings(ctx)
		if err != nil {
			log.Error("load memory managed settings", "err", err)
			os.Exit(1)
		}
		for _, token := range settings.APITokens {
			if token.ManageAccess == "write" && token.Secret != "" {
				log.Info("initialized admin api token", "token", token.Secret)
				break
			}
		}
		st = mem
		closer = func() {}
	}
	defer closer()

	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		log.Error("load managed settings", "err", err)
		os.Exit(1)
	}
	policies := policy.DefaultPolicies()
	globalScheduler := config.Scheduler{}
	config.DefaultScheduler(&globalScheduler)
	if cfg, ok := settings.LabelConfigs[config.AgentAllLabel]; ok && cfg.Scheduler != nil {
		globalScheduler = *cfg.Scheduler
	}
	hub := scheduler.NewHub(
		st,
		policies,
		time.Duration(globalScheduler.AgentOfflineAfterSec)*time.Second,
		time.Duration(globalScheduler.PollIntervalSec)*time.Second,
		globalScheduler.MaxInflightPerAgent,
		log,
	)
	hub.ApplySettings(settings)
	go hub.Start(ctx)
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
		Tools:       api.AbuseToolLimits(settings.RateLimit.Tools),
		ExemptCIDRs: settings.RateLimit.ExemptCIDRs,
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
		RootContext:     ctx,
		RequireAuth:     true,
		LabelConfigs:    settings.LabelConfigs,
	}, api.TokenConfigsFromPermissions(settings.APITokens)...)
	if err != nil {
		log.Error("configure http client ip", "err", err)
		os.Exit(1)
	}
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
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
