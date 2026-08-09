package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

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
	maxBootstrapTokenBytes    = 4096
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

	st, closer, initialization, err := openConfiguredStore(ctx, cfg.DatabaseURL, "")
	if errors.Is(err, store.ErrBootstrapAdminTokenRequired) {
		bootstrapToken, readErr := readBootstrapAdminToken(cfg.BootstrapAdminTokenFile)
		if readErr != nil {
			log.Error("read bootstrap admin token", "path", cfg.BootstrapAdminTokenFile, "err", readErr)
			os.Exit(1)
		}
		st, closer, initialization, err = openConfiguredStore(ctx, cfg.DatabaseURL, bootstrapToken)
	}
	if err != nil {
		log.Error("initialize store", "err", err)
		os.Exit(1)
	}
	if initialization.Seeded {
		log.Info("initialized managed settings from bootstrap admin token file")
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
	grpcSrv := grpc.NewServer(grpc.Creds(creds), grpc.ForceServerCodec(grpcwire.JSONCodec{}), grpc.MaxRecvMsgSize(1<<20))
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

func openConfiguredStore(ctx context.Context, databaseURL string, bootstrapAdminToken string) (store.Store, func(), store.Initialization, error) {
	if isSQLiteDSN(databaseURL) {
		sqlite, initialization, err := store.NewSQLite(ctx, databaseURL, bootstrapAdminToken)
		if err != nil {
			return nil, nil, store.Initialization{}, err
		}
		return sqlite, sqlite.Close, initialization, nil
	}
	if databaseURL != "" {
		postgres, initialization, err := store.NewPostgres(ctx, databaseURL, bootstrapAdminToken)
		if err != nil {
			return nil, nil, store.Initialization{}, err
		}
		return postgres, postgres.Close, initialization, nil
	}
	memory, initialization, err := store.NewMemory(bootstrapAdminToken)
	if err != nil {
		return nil, nil, store.Initialization{}, err
	}
	return memory, func() {}, initialization, nil
}

func readBootstrapAdminToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("open bootstrap admin token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect bootstrap admin token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("bootstrap admin token path must be a regular file")
	}
	if info.Mode().Perm()&0o137 != 0 {
		return "", errors.New("bootstrap admin token file must not be executable, group-writable, or accessible by other users")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBootstrapTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("read bootstrap admin token file: %w", err)
	}
	if len(body) > maxBootstrapTokenBytes {
		return "", errors.New("bootstrap admin token exceeds size limit")
	}
	token := string(body)
	if strings.HasSuffix(token, "\n") {
		token = strings.TrimSuffix(token, "\n")
		token = strings.TrimSuffix(token, "\r")
	}
	if len(token) < 16 {
		return "", errors.New("bootstrap admin token must contain at least 16 bytes")
	}
	for _, char := range token {
		if unicode.IsSpace(char) || unicode.IsControl(char) {
			return "", errors.New("bootstrap admin token must not contain whitespace or control characters")
		}
	}
	return token, nil
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
