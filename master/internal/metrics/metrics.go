// Package metrics exposes the master Prometheus metrics
// (docs/specs/ops.md §1: canonical metric names).
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/ufna/birdman/master/internal/store"
)

type Metrics struct {
	Registry *prometheus.Registry

	AllocDuration prometheus.Histogram
	AllocFailures *prometheus.CounterVec

	// Matchmaker (docs/specs/ops.md §1).
	MMQueueDepth  *prometheus.GaugeVec   // {region, env} queued tickets by best region
	MMTimeToMatch prometheus.Histogram   // seconds from ticket submit to matched
	MMTickets     *prometheus.CounterVec // {result} matched|cancelled|update_required|expired

	// Deploy manager (итерация 3, docs/specs/master.md §5).
	DeployPrepull prometheus.Histogram // seconds from deploy start to all nodes pulled

	// AgentlinkRegistriesWithheld counts SetRegistries deliveries skipped by
	// the registries gate — sessions that are neither cert-authenticated nor
	// loopback (mTLS agentlink v1, design §3). Incremented by the hub via
	// SetRegistriesWithheldCounter (main.go wiring).
	AgentlinkRegistriesWithheld prometheus.Counter

	// ImageRemovals counts the RESULT an agent reports for each RemoveImage
	// (ImageReport.status, environments v1 §6б): removed|absent mean the image is
	// gone from that node (the version's image_cleanup_at marker may be stamped),
	// busy means it still backs a live container and error that the removal failed
	// — both leave the version unmarked for the next 60s sweep to retry. A fleet
	// stuck on busy/error is a disk leak that used to be INVISIBLE: the master
	// stamped the marker blind, because the protocol carried no result at all.
	// Incremented by the image cleaner via SetRemovalCounter (main.go wiring).
	ImageRemovals *prometheus.CounterVec // {status}

	// agentlink holds the late-wired callbacks behind the sessions{auth} and
	// tls_cert_expiry{cert="server"} samples (mTLS agentlink v1, design §3):
	// the hub and the server-leaf holder are constructed around the same time
	// as Metrics in main.go, so New registers the collector immediately and
	// the callbacks arrive via the Wire* methods right after.
	agentlink *agentlinkCollector
}

// WireAgentlinkSessions connects the birdman_agentlink_sessions{auth} gauge
// to the hub's live-session counts (Hub.SessionAuthCounts). Until wired the
// gauge emits nothing; once wired it always emits BOTH auth="mtls" and
// auth="token" samples — the operator's flip-readiness check is precisely
// sessions{auth="token"} == 0, which needs an explicit 0.
func (m *Metrics) WireAgentlinkSessions(f func() (mtls, token int)) {
	m.agentlink.sessions.Store(&f)
}

// WireAgentlinkPendingCommands connects the
// birdman_agentlink_pending_commands{node,node_id} gauge to the hub's
// per-node unacked-queue depths (Hub.PendingCounts, keyed by node_id; the
// collector resolves each id to the node's hostname for the node label on
// scrape). Until wired the gauge emits nothing; once wired it emits ONE
// sample per node that currently has a non-empty queue and NOTHING for the
// rest — a clean fleet produces no series, which is what keeps the
// AgentlinkPendingStuck alert (pending>0 held for `for:`) absent-safe
// (followups §3, ревизия).
func (m *Metrics) WireAgentlinkPendingCommands(f func() map[string]int) {
	m.agentlink.pending.Store(&f)
}

// WireTLSServerCertExpiry connects the
// birdman_tls_cert_expiry_timestamp_seconds{cert="server"} sample to the
// in-memory server-leaf holder (main.go). The callback returns ok=false when
// no leaf (or no parsed Leaf) is available, in which case nothing is emitted.
func (m *Metrics) WireTLSServerCertExpiry(f func() (time.Time, bool)) {
	m.agentlink.serverCert.Store(&f)
}

func New(st *store.Store, log *slog.Logger) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		AllocDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "birdman_allocation_duration_seconds",
			Help:    "Duration of POST /v1/allocate (SLO p95 < 1s warm).",
			Buckets: prometheus.DefBuckets,
		}),
		// project is the allocation's project — an allocation is ALWAYS about one
		// (allocateRequest.Project is required), so the label makes BufferEmptyAllocFail
		// and AllocationFailures project-scoped without touching their expr, and it
		// costs one series per project. It is empty ONLY where the request has not
		// been parsed yet (the earliest decodeJSON failure): an empty label is the
		// same as an absent one in Prometheus, so such an alert stays PLATFORM-scoped
		// and remains visible under any ?project= (httpapi/alerts.go).
		AllocFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "birdman_allocation_failures_total",
			Help: "Failed allocations by reason (no_capacity, bad_request, env_required, internal) and project.",
		}, []string{"reason", "project"}),
		MMQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "birdman_mm_queue_depth",
			Help: "Queued matchmaking tickets per best (lowest-rtt) region and env (environments v1 §7).",
		}, []string{"region", "env"}),
		MMTimeToMatch: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "birdman_mm_time_to_match_seconds",
			Help:    "Time from ticket submit to matched.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120},
		}),
		MMTickets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "birdman_mm_tickets_total",
			Help: "Finished matchmaking tickets by result (matched, cancelled, update_required, expired).",
		}, []string{"result"}),
		DeployPrepull: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "birdman_deploy_prepull_seconds",
			Help:    "Time from POST /v1/deploy to every fleet node reporting the image pulled.",
			Buckets: []float64{1, 2, 5, 10, 30, 60, 120, 300, 600, 900},
		}),
		AgentlinkRegistriesWithheld: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "birdman_agentlink_registries_withheld_total",
			Help: "SetRegistries deliveries withheld from agentlink sessions that are neither cert-authenticated nor loopback (registries gate, mTLS v1).",
		}),
		ImageRemovals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "birdman_image_removals_total",
			Help: "RemoveImage results reported by agents, by status (removed, absent, busy, error) — environments v1 §6б.",
		}, []string{"status"}),
	}
	m.agentlink = &agentlinkCollector{st: st, log: log}
	reg.MustRegister(m.AllocDuration, m.AllocFailures, m.MMQueueDepth, m.MMTimeToMatch, m.MMTickets, m.DeployPrepull,
		m.AgentlinkRegistriesWithheld, m.ImageRemovals)
	reg.MustRegister(&dbCollector{st: st, log: log})
	reg.MustRegister(m.agentlink)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

var (
	// birdman_servers{project, env, production, state, region, version}
	// (environments v1 §7, M14). env is the SERVER's env (servers.env);
	// production is a join to environments by (project_id, env) — both derived
	// from the server row, NEVER from the node (I6): a node moved between
	// environments must not rewrite the env/production history of servers it
	// hosted. region still comes from the node (nodes.region).
	serversDesc = prometheus.NewDesc(
		"birdman_servers",
		"Server counts by project, env, production, state, region and version (semver).",
		[]string{"project", "env", "production", "state", "region", "version"}, nil)
	heartbeatAgeDesc = prometheus.NewDesc(
		"birdman_node_heartbeat_age_seconds",
		"Seconds since the last agent heartbeat, per node.",
		[]string{"node", "region"}, nil)
	versionsDesc = prometheus.NewDesc(
		"birdman_versions",
		"Registered version counts by project, env and state (registered, prepulling, active, deprecated, disabled).",
		[]string{"project", "env", "state"}, nil)
	// birdman_events_total feeds the CrashLoop alert (increase of
	// {kind="crash_loop"}); the events table is append-only, so a DB-derived
	// count is monotonic and survives a master restart — a real counter.
	eventsTotalDesc = prometheus.NewDesc(
		"birdman_events_total",
		"Total events by kind (append-only feed; crash_loop feeds the CrashLoop alert).",
		[]string{"kind"}, nil)
	matchesRunningDesc = prometheus.NewDesc(
		"birdman_matches_running",
		"Matches currently in the running state (product metric).",
		nil, nil)
	playersOnlineDesc = prometheus.NewDesc(
		"birdman_players_online",
		"Live players across allocated servers, last heartbeat (product metric).",
		nil, nil)
	capacitySlotsDesc = prometheus.NewDesc(
		"birdman_node_capacity_slots",
		"Capacity slots of active nodes, per region.",
		[]string{"region"}, nil)
	// mTLS agentlink v1 (design §3 "Наблюдаемость"): cert-expiry gauges feed
	// the CertExpiry alert ("< 14 дней", ops.md §1); node label = hostname,
	// consistent with birdman_node_heartbeat_age_seconds.
	nodeCertExpiryDesc = prometheus.NewDesc(
		"birdman_node_cert_expiry_timestamp_seconds",
		"Unix time when the node's agentlink client cert expires (nodes.cert_not_after); absent until the node enrolls.",
		[]string{"node"}, nil)
	// Backups v1 (docs/superpowers/specs/2026-07-13-backups-admin-v1-design.md
	// §5): every series is derived from backup_settings/backup_runs on scrape
	// (dbCollector.Collect), so it survives a master restart with nothing held
	// in process memory.
	backupEnabledDesc = prometheus.NewDesc("birdman_backup_enabled",
		"Whether scheduled backups are enabled (backup_settings).", nil, nil)
	backupIntervalDesc = prometheus.NewDesc("birdman_backup_interval_seconds",
		"Configured backup interval.", nil, nil)
	backupLastSuccessDesc = prometheus.NewDesc("birdman_backup_last_success_timestamp_seconds",
		"Unix time of the last successful backup run (0 = never).", nil, nil)
	// last_error — rotation-immune основа алерта BackupFailed (Задача 6):
	// max(started_at) по error-строкам не зависит от ротации истории в
	// пределах окна алерта (1ч — свежая ошибка не успевает выпасть из 200
	// сохраняемых прогонов; после 200 подряд ok-прогонов строка ошибки
	// ротируется и гейдж падает на 0), в отличие от increase(runs_total) —
	// см. комментарий у backupRunsDesc ниже.
	backupLastErrorDesc = prometheus.NewDesc("birdman_backup_last_error_timestamp_seconds",
		"Unix time of the last failed backup run (0 = never).", nil, nil)
	backupLastSizeDesc = prometheus.NewDesc("birdman_backup_last_size_bytes",
		"Size of the last successful dump.", nil, nil)
	backupS3LastSuccessDesc = prometheus.NewDesc("birdman_backup_s3_last_success_timestamp_seconds",
		"Unix time of the last successful S3 upload (0 = never).", nil, nil)
	// runs_total — count(*) по ЖИВЫМ строкам backup_runs: PruneBackupRuns(200)
	// со временем УМЕНЬШАЕТ значение, для Prometheus это counter reset →
	// rate()/increase() поверх этой серии дают ложные срабатывания (в т.ч.
	// ложный BackupFailed). Алерты на ней НЕ строить — для алертов есть
	// last_error_timestamp выше; эта серия — наблюдаемость (панель/дашборды).
	backupRunsDesc = prometheus.NewDesc("birdman_backup_runs_total",
		"Finished backup runs by result (count over live backup_runs rows; history rotation may lower it — do not alert on rate/increase, use last_error_timestamp).", []string{"result"}, nil)
	agentlinkSessionsDesc = prometheus.NewDesc(
		"birdman_agentlink_sessions",
		"Live agentlink sessions by auth (mtls: verified client cert; token: node_token). token==0 signals readiness for the mtls flip.",
		[]string{"auth"}, nil)
	// birdman_agentlink_pending_commands{node,node_id} — unacked master→agent
	// commands still queued for a node (followups §3, ревизия). node = the
	// node's hostname (stack convention — heartbeat/cert-expiry gauges above
	// use hostname too, so rules and dashboards join nodes on one label),
	// resolved from the hub's node_id key on scrape; when the id no longer
	// resolves (node row deleted, queue still alive) node falls back to the
	// uuid — the alert stays absent-safe, never blind. node_id = the exact
	// uuid: precise identification, and it keeps labelsets unique (Gather
	// would fail on duplicates) should two hostnames ever collide. Only nodes
	// with pending>0 emit a sample; a drained queue produces NO series (never
	// a 0), so AgentlinkPendingStuck (pending>0 held for `for:`) resets via
	// staleness on drain.
	agentlinkPendingDesc = prometheus.NewDesc(
		"birdman_agentlink_pending_commands",
		"Unacked master→agent commands queued per node (node=hostname, node_id=uuid; only nodes with a non-empty queue are reported; a drained queue emits no series).",
		[]string{"node", "node_id"}, nil)
	tlsCertExpiryDesc = prometheus.NewDesc(
		"birdman_tls_cert_expiry_timestamp_seconds",
		"Unix time when the given TLS cert expires (ca: newest active internal CA — the signer; server: current gRPC server leaf).",
		[]string{"cert"}, nil)
)

// agentlinkCollector emits the mTLS agentlink v1 observability samples that
// need late-wired in-process state (design §3): the sessions{auth} gauge
// (hub callback), the server-leaf expiry (cert holder callback) and — kept in
// this collector so ONE collector owns the whole
// birdman_tls_cert_expiry_timestamp_seconds family — the DB-derived internal
// CA expiry. Unwired callbacks simply emit nothing.
type agentlinkCollector struct {
	st         *store.Store
	log        *slog.Logger
	sessions   atomic.Pointer[func() (mtls, token int)]
	pending    atomic.Pointer[func() map[string]int]
	serverCert atomic.Pointer[func() (time.Time, bool)]
}

func (c *agentlinkCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- agentlinkSessionsDesc
	ch <- agentlinkPendingDesc
	ch <- tlsCertExpiryDesc
}

func (c *agentlinkCollector) Collect(ch chan<- prometheus.Metric) {
	// One scrape-scoped DB budget for the whole collector (pending hostnames
	// below + CA expiry at the bottom) — same 3s discipline as dbCollector.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if f := c.sessions.Load(); f != nil {
		mtls, token := (*f)()
		ch <- prometheus.MustNewConstMetric(agentlinkSessionsDesc, prometheus.GaugeValue, float64(mtls), "mtls")
		ch <- prometheus.MustNewConstMetric(agentlinkSessionsDesc, prometheus.GaugeValue, float64(token), "token")
	}
	// Pending-queue depths per node (followups §3, ревизия): the callback
	// already returns ONLY nodes with a non-empty queue (keyed by node_id), so
	// a clean fleet emits nothing — and costs zero extra queries. Otherwise
	// one query resolves ids to hostnames for the node label (stack
	// convention); an id that misses (node row deleted, queue still alive) or
	// a failed query falls back to node=<uuid> — absent-safe, never blind.
	if f := c.pending.Load(); f != nil {
		if counts := (*f)(); len(counts) > 0 {
			ids := make([]string, 0, len(counts))
			for id := range counts {
				ids = append(ids, id)
			}
			hostnames := make(map[string]string, len(ids))
			if prows, err := c.st.Pool.Query(ctx, `
				select id::text, hostname from nodes where id = any($1::uuid[])`, ids); err != nil {
				c.log.Error("metrics: pending-commands hostname query failed", "err", err)
			} else {
				for prows.Next() {
					var id, hostname string
					if err := prows.Scan(&id, &hostname); err != nil {
						c.log.Error("metrics: pending-commands hostname scan failed", "err", err)
						break
					}
					hostnames[id] = hostname
				}
				prows.Close()
				if err := prows.Err(); err != nil {
					c.log.Error("metrics: pending-commands hostname rows failed", "err", err)
				}
			}
			for id, n := range counts {
				node, ok := hostnames[id]
				if !ok {
					node = id
				}
				ch <- prometheus.MustNewConstMetric(agentlinkPendingDesc, prometheus.GaugeValue, float64(n), node, id)
			}
		}
	}
	if f := c.serverCert.Load(); f != nil {
		if notAfter, ok := (*f)(); ok {
			ch <- prometheus.MustNewConstMetric(tlsCertExpiryDesc, prometheus.GaugeValue,
				float64(notAfter.Unix()), "server")
		}
	}

	// CA expiry: the newest active row is the signer (same choice as
	// store.loadActiveCA). No row yet (first boot hasn't ensured the CA) —
	// emit nothing.
	var caExpiry float64
	err := c.st.Pool.QueryRow(ctx, `
		select extract(epoch from not_after) from internal_ca
		where active order by created_at desc limit 1`).Scan(&caExpiry)
	switch {
	case err == nil:
		ch <- prometheus.MustNewConstMetric(tlsCertExpiryDesc, prometheus.GaugeValue, caExpiry, "ca")
	case errors.Is(err, pgx.ErrNoRows): // pre-first-boot: no CA yet, nothing to report
	default:
		c.log.Error("metrics: internal_ca expiry query failed", "err", err)
	}
}

// dbCollector derives gauge metrics from Postgres on scrape.
type dbCollector struct {
	st  *store.Store
	log *slog.Logger
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- serversDesc
	ch <- heartbeatAgeDesc
	ch <- versionsDesc
	ch <- eventsTotalDesc
	ch <- matchesRunningDesc
	ch <- playersOnlineDesc
	ch <- capacitySlotsDesc
	ch <- nodeCertExpiryDesc
	ch <- backupEnabledDesc
	ch <- backupIntervalDesc
	ch <- backupLastSuccessDesc
	ch <- backupLastErrorDesc
	ch <- backupLastSizeDesc
	ch <- backupS3LastSuccessDesc
	ch <- backupRunsDesc
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Product + alert-feed metrics (ops.md §1). Each logs-and-continues so one
	// failed query does not blank the rest.
	if erows, err := c.st.Pool.Query(ctx, `select kind, count(*) from events group by kind`); err != nil {
		c.log.Error("metrics: events query failed", "err", err)
	} else {
		for erows.Next() {
			var kind string
			var n float64
			if err := erows.Scan(&kind, &n); err != nil {
				c.log.Error("metrics: events scan failed", "err", err)
				break
			}
			ch <- prometheus.MustNewConstMetric(eventsTotalDesc, prometheus.CounterValue, n, kind)
		}
		erows.Close()
	}
	var running float64
	if err := c.st.Pool.QueryRow(ctx, `select count(*) from matches where state = 'running'`).Scan(&running); err != nil {
		c.log.Error("metrics: matches_running query failed", "err", err)
	} else {
		ch <- prometheus.MustNewConstMetric(matchesRunningDesc, prometheus.GaugeValue, running)
	}
	var players float64
	if err := c.st.Pool.QueryRow(ctx, `select coalesce(sum(players), 0) from servers where state = 'allocated'`).Scan(&players); err != nil {
		c.log.Error("metrics: players_online query failed", "err", err)
	} else {
		ch <- prometheus.MustNewConstMetric(playersOnlineDesc, prometheus.GaugeValue, players)
	}
	if crows, err := c.st.Pool.Query(ctx, `select region, sum(capacity_slots)::int from nodes where state = 'active' group by region`); err != nil {
		c.log.Error("metrics: capacity query failed", "err", err)
	} else {
		for crows.Next() {
			var region string
			var n float64
			if err := crows.Scan(&region, &n); err != nil {
				c.log.Error("metrics: capacity scan failed", "err", err)
				break
			}
			ch <- prometheus.MustNewConstMetric(capacitySlotsDesc, prometheus.GaugeValue, n, region)
		}
		crows.Close()
	}

	// Enrolled nodes' client-cert expiry (mTLS agentlink v1, design §3) —
	// second half of the CertExpiry alert alongside tls_cert_expiry above.
	if nrows, err := c.st.Pool.Query(ctx, `
		select hostname, extract(epoch from cert_not_after)
		from nodes where cert_not_after is not null`); err != nil {
		c.log.Error("metrics: node cert expiry query failed", "err", err)
	} else {
		for nrows.Next() {
			var hostname string
			var expiry float64
			if err := nrows.Scan(&hostname, &expiry); err != nil {
				c.log.Error("metrics: node cert expiry scan failed", "err", err)
				break
			}
			ch <- prometheus.MustNewConstMetric(nodeCertExpiryDesc, prometheus.GaugeValue, expiry, hostname)
		}
		nrows.Close()
	}

	// Backups v1 (design §5): all seven series are DB-derived so they outlive a
	// master restart. Each query logs-and-continues like the blocks above — one
	// failure never blanks the others.
	var bEnabled bool
	var bIntervalH int
	if err := c.st.Pool.QueryRow(ctx,
		`select enabled, interval_hours from backup_settings where id`).
		Scan(&bEnabled, &bIntervalH); err != nil {
		c.log.Error("metrics: backup_settings query failed", "err", err)
	} else {
		ch <- prometheus.MustNewConstMetric(backupEnabledDesc, prometheus.GaugeValue, b2f(bEnabled))
		ch <- prometheus.MustNewConstMetric(backupIntervalDesc, prometheus.GaugeValue, float64(bIntervalH)*3600)
	}
	var lastOK, lastErr, lastS3 float64
	var lastSize int64
	if err := c.st.Pool.QueryRow(ctx, `
		select coalesce(extract(epoch from max(started_at) filter (where result='ok')), 0),
		       coalesce(extract(epoch from max(started_at) filter (where result='error')), 0),
		       coalesce(extract(epoch from max(started_at) filter (where result='ok' and s3_uploaded)), 0),
		       coalesce((select size_bytes from backup_runs where result='ok' order by started_at desc, id desc limit 1), 0)
		from backup_runs`).Scan(&lastOK, &lastErr, &lastS3, &lastSize); err != nil {
		c.log.Error("metrics: backup_runs query failed", "err", err)
	} else {
		ch <- prometheus.MustNewConstMetric(backupLastSuccessDesc, prometheus.GaugeValue, lastOK)
		ch <- prometheus.MustNewConstMetric(backupLastErrorDesc, prometheus.GaugeValue, lastErr)
		ch <- prometheus.MustNewConstMetric(backupS3LastSuccessDesc, prometheus.GaugeValue, lastS3)
		ch <- prometheus.MustNewConstMetric(backupLastSizeDesc, prometheus.GaugeValue, float64(lastSize))
	}
	// runs_total: count(*) по живым строкам — ротация истории (PruneBackupRuns)
	// уменьшает значение, для Prometheus это reset; алерты строятся НЕ здесь
	// (см. комментарий у backupRunsDesc). Ошибка чтения посреди итерации —
	// скипаем ОБЕ серии целиком (как logs-and-continue у соседей): частично
	// заполненные counts эмитили бы ложный «reset» ok=0/error=0.
	if brows, err := c.st.Pool.Query(ctx,
		`select result, count(*) from backup_runs where result in ('ok','error') group by result`); err != nil {
		c.log.Error("metrics: backup runs count failed", "err", err)
	} else {
		counts := map[string]float64{"ok": 0, "error": 0}
		var readErr error
		for brows.Next() {
			var res string
			var n float64
			if err := brows.Scan(&res, &n); err != nil {
				readErr = err
				break
			}
			counts[res] = n
		}
		brows.Close()
		if readErr == nil {
			readErr = brows.Err()
		}
		if readErr != nil {
			c.log.Error("metrics: backup runs count read failed", "err", readErr)
		} else {
			for _, res := range []string{"ok", "error"} {
				ch <- prometheus.MustNewConstMetric(backupRunsDesc, prometheus.CounterValue, counts[res], res)
			}
		}
	}

	// project from projects.slug; env from servers.env; production from
	// environments (project_id, s.env) — NEVER from nodes (I6). region stays
	// nodes.region; version stays versions.semver. The environments join is an
	// inner join safely: servers.env is always a seeded/FK-backed env of the
	// project (dev/prod always exist per ensureProject).
	rows, err := c.st.Pool.Query(ctx, `
		select p.slug, s.env, e.production, s.state, n.region, v.semver, count(*)
		from servers s
		join nodes n on n.id = s.node_id
		join versions v on v.id = s.version_id
		join projects p on p.id = s.project_id
		join environments e on e.project_id = s.project_id and e.name = s.env
		group by 1, 2, 3, 4, 5, 6`)
	if err != nil {
		c.log.Error("metrics: servers query failed", "err", err)
	} else {
		for rows.Next() {
			var project, env, state, region, semver string
			var production bool
			var count float64
			if err := rows.Scan(&project, &env, &production, &state, &region, &semver, &count); err != nil {
				c.log.Error("metrics: servers scan failed", "err", err)
				break
			}
			ch <- prometheus.MustNewConstMetric(serversDesc, prometheus.GaugeValue,
				count, project, env, strconv.FormatBool(production), state, region, semver)
		}
		rows.Close()
	}

	rows, err = c.st.Pool.Query(ctx, `
		select hostname, region, greatest(extract(epoch from (now() - last_heartbeat_at)), 0)
		from nodes where last_heartbeat_at is not null`)
	if err != nil {
		c.log.Error("metrics: heartbeat query failed", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var hostname, region string
		var age float64
		if err := rows.Scan(&hostname, &region, &age); err != nil {
			c.log.Error("metrics: heartbeat scan failed", "err", err)
			return
		}
		ch <- prometheus.MustNewConstMetric(heartbeatAgeDesc, prometheus.GaugeValue,
			age, hostname, region)
	}

	counts, err := c.st.VersionStateCounts(ctx)
	if err != nil {
		c.log.Error("metrics: versions query failed", "err", err)
		return
	}
	for _, vc := range counts {
		ch <- prometheus.MustNewConstMetric(versionsDesc, prometheus.GaugeValue,
			float64(vc.Count), vc.Project, vc.Env, vc.State)
	}
}

// b2f maps a bool to the Prometheus 1/0 gauge convention.
func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
