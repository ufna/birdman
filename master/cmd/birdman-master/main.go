// birdman-master: fleet controller + allocation API + AgentLink gRPC
// (docs/specs/master.md, iteration 1 of docs/05-runtime-iterations.md).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/config"
	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/reconcile"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/tlsutil"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to master.yaml (optional; env vars override)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Postgres may still be starting (dev-compose) — retry briefly.
	if err := retry(ctx, 30, time.Second, func() error {
		return store.MigrateUp(cfg.DSN)
	}); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info("migrations applied")

	st, err := store.Open(ctx, cfg.DSN)
	if err != nil {
		return err
	}
	defer st.Close()

	// Bootstrap: with no API keys in the database, mint an admin key and
	// print it once (docs/specs/master.md §6, уточнено в v0).
	if n, err := st.CountActiveAPIKeys(ctx); err != nil {
		return err
	} else if n == 0 {
		_, key, err := st.CreateAPIKey(ctx, "bootstrap-admin", []string{httpapi.ScopeAdmin})
		if err != nil {
			return err
		}
		log.Warn("bootstrap admin API key created — store it now, it is shown exactly once",
			"api_key", key)
	}

	// gRPC AgentLink over TLS (self-signed autogen in dev; real certs via
	// config; agent auth = node_token in Hello — protocol.md §Auth v0 note).
	cert, err := serverCert(cfg, log)
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	)
	hub := agentlink.NewHub(log)
	// Routes TailLogs answer chunks from agents to the REST logs proxy
	// (итерация 4, GET /v1/servers/{id}/logs).
	logRouter := agentlink.NewLogRouter()
	// Allocations notify the dedik's agent (AllocateServer → liba `allocated`,
	// итерация 2) — wire the dispatcher before anything can allocate.
	st.SetCommandSender(hub)

	m := metrics.New(st, log)
	// Deploy manager (итерация 3): PrePull fan-out + PullReport-driven flip.
	dep := deploy.New(deploy.Options{
		Store: st, Sender: hub, Log: log,
		ObservePrepull: m.DeployPrepull.Observe,
	})
	if err := dep.Resume(ctx); err != nil {
		return fmt.Errorf("deploy resume: %w", err)
	}

	agentlinkv1.RegisterAgentLinkServer(grpcServer, agentlink.NewService(st, hub, dep, logRouter, log))
	grpcLis, err := net.Listen("tcp", cfg.ListenGRPC)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}

	mmCfg, err := matchmakerConfig(cfg, log)
	if err != nil {
		return err
	}
	mm := matchmaker.New(st, m, mmCfg, log)
	apiHandler := httpapi.New(st, m, mm, dep, hub, logRouter, cfg.Metrics.VictoriaMetricsURL, log).
		WithAlertsSources(cfg.Alerts.VmalertURL, cfg.Alerts.LogPath)
	api := &http.Server{
		Addr:              cfg.ListenAPI,
		Handler:           apiHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Background loops: reconcile (1s), lease checker (1s), matchmaker (500ms).
	loopCtx, cancelLoops := context.WithCancel(context.Background())
	defer cancelLoops()
	go reconcile.New(st, hub, log).Run(loopCtx, time.Second)
	go reconcile.NewLeaseChecker(st, log).Run(loopCtx, time.Second)
	go mm.Run(loopCtx)

	errCh := make(chan error, 2)
	go func() {
		log.Info("gRPC AgentLink listening", "addr", cfg.ListenGRPC)
		errCh <- grpcServer.Serve(grpcLis)
	}()
	go func() {
		log.Info("REST API listening", "addr", cfg.ListenAPI)
		if err := api.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	// Graceful shutdown: stop accepting API calls, wait in-flight ≤5s
	// (master.md §7). Agents reconnect on their own.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = api.Shutdown(shutdownCtx)
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		grpcServer.Stop()
	}
	cancelLoops()
	return nil
}

// matchmakerConfig maps the yaml section to matchmaker.Config
// (docs/specs/master.md §4; compat.overrides — docs/specs/ops.md §3).
func matchmakerConfig(cfg config.Config, log *slog.Logger) (matchmaker.Config, error) {
	overrides := make([]matchmaker.Override, 0, len(cfg.Compat.Overrides))
	for _, o := range cfg.Compat.Overrides {
		overrides = append(overrides, matchmaker.Override{Client: o.Client, Servers: o.Servers})
	}
	compat, err := matchmaker.NewCompat(overrides)
	if err != nil {
		return matchmaker.Config{}, err
	}
	mc := matchmaker.Config{
		Tick:             time.Duration(cfg.Matchmaking.TickMS) * time.Millisecond,
		WidenAfter:       time.Duration(cfg.Matchmaking.WidenAfterS) * time.Second,
		TicketTTL:        time.Duration(cfg.Matchmaking.TicketTTLS) * time.Second,
		DefaultProject:   cfg.Matchmaking.DefaultProject,
		Compat:           compat,
		JoinTokenEnabled: cfg.Matchmaking.JoinToken.Enabled,
	}
	if !mc.JoinTokenEnabled {
		return mc, nil
	}
	if s := cfg.Matchmaking.JoinToken.Secret; s != "" {
		mc.JoinTokenSecret = []byte(s)
		return mc, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return mc, fmt.Errorf("join token secret: %w", err)
	}
	mc.JoinTokenSecret = secret
	log.Warn("join tokens enabled without a secret — generated an ephemeral one; " +
		"tokens will not survive a master restart (set BIRDMAN_MM_JOIN_SECRET)")
	return mc, nil
}

func serverCert(cfg config.Config, log *slog.Logger) (tls.Certificate, error) {
	if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		return tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	}
	log.Info("no TLS cert configured — using self-signed (dev mode)", "dir", cfg.TLS.AutoCertDir)
	return tlsutil.EnsureServerCert(cfg.TLS.AutoCertDir)
}

func retry(ctx context.Context, attempts int, delay time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}
