// birdman-master: fleet controller + allocation API + AgentLink gRPC
// (docs/specs/master.md, iteration 1 of docs/05-runtime-iterations.md).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/amsilence"
	"github.com/ufna/birdman/master/internal/backup"
	"github.com/ufna/birdman/master/internal/config"
	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/reconcile"
	"github.com/ufna/birdman/master/internal/secrets"
	"github.com/ufna/birdman/master/internal/statsrollup"
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

	// At-rest secrets key (design §2): load the box key and build the AEAD codec
	// BEFORE opening the store, which now requires it. Fail loud — a missing or
	// invalid key means the master cannot decrypt registries.token /
	// internal_ca.key_pem, so it must NOT start. It NEVER auto-generates a key:
	// that would mask a DR escrow failure by minting a key the existing
	// ciphertext cannot be read with.
	key, err := cfg.SecretsKey()
	if err != nil {
		return fmt.Errorf("secrets key: %w", err)
	}
	codec, err := secrets.New(key)
	if err != nil {
		return fmt.Errorf("secrets codec: %w", err)
	}
	// A key file more permissive than 0600 is a WARN, not a failure (design §2)
	// — and only when a file is actually the source (the dev env value has no
	// file to stat).
	if path, ok := cfg.SecretsKeyFileInUse(); ok {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			log.Warn("secrets key file is more permissive than 0600",
				"path", path, "mode", info.Mode().Perm().String())
		}
	}

	st, err := store.Open(ctx, cfg.DSN, codec)
	if err != nil {
		return err
	}
	defer st.Close()

	// Encrypt any legacy plaintext registries.token / internal_ca.key_pem rows
	// in place, under an advisory lock, IMMEDIATELY after Open and BEFORE the
	// first secret read (EnsureInternalCA below) or any serving — so the strict
	// read paths never trip on pre-encryption data and an old plaintext dump
	// stays restorable (restore → start → this pass encrypts it; design §3).
	// Logs only a count, never a secret value.
	if n, err := st.EncryptExistingSecrets(ctx); err != nil {
		return fmt.Errorf("encrypt existing secrets: %w", err)
	} else if n > 0 {
		log.Info("secrets: encrypted existing rows at-rest", "count", n)
	}

	// Bootstrap: with no API keys in the database, mint an admin key and
	// print it once (docs/specs/master.md §6, уточнено в v0).
	if n, err := st.CountActiveAPIKeys(ctx); err != nil {
		return err
	} else if n == 0 {
		_, key, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			Name: "bootstrap-admin", Scopes: []string{httpapi.ScopeAdmin},
		})
		if err != nil {
			return err
		}
		log.Warn("bootstrap admin API key created — store it now, it is shown exactly once",
			"api_key", key)
	}

	// gRPC AgentLink over TLS (mTLS agentlink v1, design §1, §3). The internal
	// CA (Postgres) backs both directions: it verifies agent client certs
	// (ClientCAs) and, unless external certs are configured, signs the master's
	// own server leaf. It is ensured unconditionally — even with an external
	// server cert, agent client certs are always internal-CA-signed (via the
	// Enroll RPC, a later task), so ClientCAs must be the internal CA regardless
	// of where the server cert comes from.
	caCertPEM, caKeyPEM, err := st.EnsureInternalCA(ctx)
	if err != nil {
		return fmt.Errorf("ensure internal CA: %w", err)
	}
	activeCAs, err := st.ActiveCAs(ctx)
	if err != nil {
		return fmt.Errorf("active CAs: %w", err)
	}
	// TODO(mtls): the ClientCAs pool is built once here at startup. A mid-process
	// CA rotation (adding a second active internal_ca row) would need this pool
	// refreshed to accept leaves signed by the new CA before the old one is
	// retired — a documented runbook follow-up (design §1 "Ротация CA"), not
	// automated here.
	clientCAs, err := tlsutil.CAPool(activeCAs)
	if err != nil {
		return fmt.Errorf("client CA pool: %w", err)
	}

	// Server leaf: external cert_file/key_file keep the current path unchanged
	// (no rotation — managed externally); otherwise the master issues itself a
	// leaf from the internal CA and hot-rotates it (started after loopCtx below).
	hostname, _ := os.Hostname()
	holder := &serverCertHolder{}
	rotateServerLeaf := false
	if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		ext, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("load tls keypair: %w", err)
		}
		holder.set(&ext)
		log.Info("agentlink TLS: serving external server cert", "cert_file", cfg.TLS.CertFile)
	} else {
		leaf, err := issueServerLeaf(caCertPEM, caKeyPEM, hostname, cfg.TLS.ExtraSANs)
		if err != nil {
			return fmt.Errorf("issue server leaf: %w", err)
		}
		holder.set(leaf)
		rotateServerLeaf = true
		log.Info("agentlink TLS: issued server leaf from internal CA", "not_after", leaf.Leaf.NotAfter)
	}

	// VerifyClientCertIfGiven — NOT RequireAndVerifyClientCert: the Enroll RPC
	// (a later task) must work on a connection with no client cert yet. Per-RPC
	// strictness is enforced in agentlink by agentlink_auth (design §3). The
	// leaf is served via GetCertificate (the holder closure), not static
	// Certificates, so hot-rotate can swap it without recreating the listener.
	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{
			GetCertificate: holder.getCertificate,
			ClientCAs:      clientCAs,
			ClientAuth:     tls.VerifyClientCertIfGiven,
			MinVersion:     tls.VersionTLS12,
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
	// mTLS agentlink v1 observability (design §3): live sessions by auth kind
	// (the operator flips agentlink_auth to mtls once {auth="token"}==0) and
	// the served leaf's expiry — external certs work too: since Go 1.23
	// LoadX509KeyPair populates Leaf, and issueServerLeaf always does.
	m.WireAgentlinkSessions(hub.SessionAuthCounts)
	// Per-node unacked-command depth (followups §3): feeds
	// birdman_agentlink_pending_commands{node,node_id} and the AgentlinkPendingStuck
	// alert — a SetRegistries (or any command) that never drains becomes visible.
	m.WireAgentlinkPendingCommands(hub.PendingCounts)
	m.WireTLSServerCertExpiry(func() (time.Time, bool) {
		if c := holder.get(); c != nil && c.Leaf != nil {
			return c.Leaf.NotAfter, true
		}
		return time.Time{}, false
	})
	// Registries gate (design §3): every SetRegistries skipped for an
	// untrusted session (neither cert-auth nor loopback) is counted.
	hub.SetRegistriesWithheldCounter(m.AgentlinkRegistriesWithheld.Inc)
	// Image cleanup dispatcher (environments v1 §6б): RemoveImage on every disabled
	// transition. EXACTLY ONE instance, shared by all three of its collaborators —
	// the deploy manager (flip-demote), the reconcile loop (reap-TTL + retention +
	// the converging sweep) and the agentlink service (which routes the agents'
	// ImageReports back into it). It is no longer stateless: the sweep's
	// image_cleanup_at marker is stamped when EVERY target node reports the image
	// gone (removed|absent), so the object that sends must be the object that hears.
	imageCleaner := reconcile.NewImageCleaner(st, hub, log)
	// birdman_image_removals_total{status}: removed|absent|busy|error, as reported
	// by the agents — a fleet stuck on busy/error is a disk leak that used to be
	// invisible (the marker was stamped blind, the protocol carried no result).
	imageCleaner.SetRemovalCounter(func(status string) { m.ImageRemovals.WithLabelValues(status).Inc() })
	// Deploy manager (итерация 3): PrePull fan-out + PullReport-driven flip.
	dep := deploy.New(deploy.Options{
		Store: st, Sender: hub, Log: log,
		ObservePrepull: m.DeployPrepull.Observe,
		ImageCleaner:   imageCleaner,
	})
	if err := dep.Resume(ctx); err != nil {
		return fmt.Errorf("deploy resume: %w", err)
	}

	agentlinkSvc := agentlink.NewService(st, hub, dep, logRouter, agentlink.AuthMode(cfg.AgentlinkAuth), log).
		// RemoveImage results (§6б): the cleaner marks a version's image retired
		// only once every target node has reported it gone.
		WithImageSink(imageCleaner)
	agentlinkv1.RegisterAgentLinkServer(grpcServer, agentlinkSvc)
	grpcLis, err := net.Listen("tcp", cfg.ListenGRPC)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}

	mmCfg, err := matchmakerConfig(cfg, log)
	if err != nil {
		return err
	}
	mm := matchmaker.New(st, m, mmCfg, log)
	// Backups v1 runner (internal/backup): constructed before httpapi.New so a
	// later task can expose it through the API (manual run / status); it is
	// started with the other background loops below (go backupRunner.Run).
	backupRunner := backup.New(st, cfg.DSN, cfg.Backups, log)
	// Silence mirror (tracker #245): mirrors mute changes into real alertmanager
	// silences best-effort. An empty alertmanager_url disables it (all no-ops);
	// its Run loop below migrates v0 mutes and repairs drift.
	silenceMirror := amsilence.New(st, cfg.Alerts.AlertmanagerURL, log)
	apiHandler := httpapi.New(st, m, mm, dep, hub, logRouter, cfg.Metrics.VictoriaMetricsURL, cfg.Metrics.VictoriaLogsURL, log).
		WithAlertsSources(cfg.Alerts.VmalertURL, cfg.Alerts.LogPath).
		// After every successful POST/DELETE /v1/registries, refresh every
		// connected agent's in-memory credential set (docs/superpowers/specs/
		// 2026-07-09-registries-design.md §2, T3).
		WithRegistriesHook(agentlinkSvc.BroadcastRegistries).
		// Backups v1 (design §4): manual run-now via the runner, and the
		// "test connection" button verifying the saved S3 config.
		WithBackups(backupRunner, func(ctx context.Context) error { return backup.TestS3(ctx, st) }).
		// Mirror mute/unmute into alertmanager silences best-effort (tracker #245).
		WithSilenceMirror(silenceMirror)
	api := &http.Server{
		Addr:              cfg.ListenAPI,
		Handler:           apiHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Background loops: reconcile (1s), lease checker (1s), matchmaker (500ms),
	// stats rollup (backfill once + tail recompute every StatsRollupInterval).
	loopCtx, cancelLoops := context.WithCancel(context.Background())
	defer cancelLoops()
	// Hot-rotate the internally issued server leaf (design §1): daily check,
	// re-issue when <14 days from expiry. Skipped for external certs.
	if rotateServerLeaf {
		go holder.rotateLoop(loopCtx, caCertPEM, caKeyPEM, hostname, cfg.TLS.ExtraSANs, log)
	}
	// WithImageCleaner hands the loop the SHARED cleaner (не свой собственный):
	// the ~60s cleanup sweep dispatches RemoveImage through it, and that same object
	// receives the agents' ImageReports from the agentlink service above — that is
	// what lets the sweep stamp image_cleanup_at only for images actually gone.
	// WithOrphanSweeper wires the deploy manager's orphan-prepull sweep (W2-реестр).
	go reconcile.New(st, hub, log).
		WithImageCleaner(imageCleaner).
		WithOrphanSweeper(dep).
		Run(loopCtx, time.Second)
	go reconcile.NewLeaseChecker(st, log, time.Duration(cfg.NodeDownAfterMin)*time.Minute).Run(loopCtx, time.Second)
	go mm.Run(loopCtx)
	// statsrollup's startup Backfill runs inside this goroutine and
	// deliberately does not block API/gRPC serving from starting -- stats
	// endpoints may read zero/partial rollups for older days until it
	// completes (allocation/critical-path serving must never wait on stats).
	go statsrollup.New(st, cfg.StatsRollupInterval, log).Run(loopCtx)
	// Backups v1 scheduler: ticks every minute, dumps when enabled &&
	// now ≥ last_ok+interval (policy read from backup_settings each run).
	go backupRunner.Run(loopCtx)
	// Silence-mirror reconcile (tracker #245): migrates v0 mutes into real
	// alertmanager silences, re-issues lost/expired ones, repairs endsAt drift
	// and sweeps orphans. A disabled (empty alertmanager_url) mirror returns at once.
	go silenceMirror.Run(loopCtx)
	// Прогрев канареек VL/VM (tracker #1007): сломанный апстрим виден в логе при
	// загрузке, а не в момент, когда об него споткнётся первый привязанный ключ.
	// Именно ГОРУТИНА и именно после Serve: проба ходит по сети, а старт master'а
	// не имеет права зависеть от того, поднялась ли уже наблюдаемость.
	go apiHandler.WarmNarrowProbes(loopCtx)

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

// serverCertHolder holds the current gRPC server leaf behind a mutex so the
// hot-rotate goroutine can swap it atomically while GetCertificate serves it on
// every handshake (mTLS agentlink v1, design §1). Serving via GetCertificate
// (not static tls.Config.Certificates) is precisely what lets the leaf be
// replaced without recreating the listener.
type serverCertHolder struct {
	mu   sync.RWMutex
	cert *tls.Certificate
}

func (h *serverCertHolder) set(c *tls.Certificate) {
	h.mu.Lock()
	h.cert = c
	h.mu.Unlock()
}

func (h *serverCertHolder) get() *tls.Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cert
}

// getCertificate is the tls.Config.GetCertificate closure: it returns the
// current leaf for every ClientHello.
func (h *serverCertHolder) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return h.get(), nil
}

// serverLeafRenewBefore: re-issue the server leaf once it is within this window
// of expiry. Leaves are 90 days (tlsutil), so a daily check with a 14-day
// window rotates well ahead of expiry even across long uptimes (design §1).
const serverLeafRenewBefore = 14 * 24 * time.Hour

// rotateLoop re-issues the server leaf from the internal CA and swaps it into
// the holder once it is <14 days from expiry, checked daily — the master can
// run without a restart longer than a 90-day leaf lives (design §1). It never
// logs key material (tlsutil keeps key bytes out of its errors).
func (h *serverCertHolder) rotateLoop(ctx context.Context, caCertPEM, caKeyPEM []byte, hostname string, extraSANs []string, log *slog.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if cur := h.get(); cur != nil && cur.Leaf != nil && time.Until(cur.Leaf.NotAfter) > serverLeafRenewBefore {
				continue
			}
			leaf, err := issueServerLeaf(caCertPEM, caKeyPEM, hostname, extraSANs)
			if err != nil {
				log.Error("agentlink TLS: server leaf rotation failed", "err", err)
				continue
			}
			h.set(leaf)
			log.Info("agentlink TLS: server leaf rotated", "not_after", leaf.Leaf.NotAfter)
		}
	}
}

// issueServerLeaf issues a server leaf from the internal CA and builds a
// tls.Certificate with its parsed Leaf populated, so the rotate loop can read
// NotAfter without re-parsing. tlsutil.IssueServerLeaf never puts key material
// in its error, so wrapping/returning err is safe.
func issueServerLeaf(caCertPEM, caKeyPEM []byte, hostname string, extraSANs []string) (*tls.Certificate, error) {
	certPEM, keyPEM, err := tlsutil.IssueServerLeaf(caCertPEM, caKeyPEM, hostname, extraSANs)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		cert.Leaf = leaf
	}
	return &cert, nil
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
