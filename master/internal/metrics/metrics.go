// Package metrics exposes the master Prometheus metrics
// (docs/specs/ops.md §1: canonical metric names).
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
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
	dbc := &dbCollector{st: st, log: log, allocFailures: m.AllocFailures}
	reg.MustRegister(dbc)
	// Нули заводятся СРАЗУ, а не только на скрейпе: коллекторы регистрируются
	// по отдельности, и серии, созданные во время сбора, попали бы в выдачу лишь
	// со следующего раза. Отказ аллокации между стартом и вторым скрейпом
	// остался бы невидимым для increase() — ровно тот сценарий, который и чиним
	// (после каждого рестарта master дыра взводилась заново).
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		dbc.preinitAllocFailures(ctx)
	}()
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
	// birdman_server_info{server_id, project, env} = 1 — the reference series
	// that gives an OWNER to the per-server metrics the AGENT emits (tracker
	// #958). birdman_server_tick_ms / birdman_server_players / the per-container
	// cpu+mem pair carry only server_id, because the agent does not know which
	// project a dedik belongs to at all — StartServer never told it. So a rule
	// over them (TickDegraded) could not be project-scoped, and the alert screen
	// showed project A's operator the tick degradation of project B.
	//
	// This series closes that without touching the agent, the protocol or the
	// fleet: the master already owns server_id → project in Postgres, and a rule
	// joins on server_id (`group_left(project)`) to inherit the label. The
	// alternative — a label minted by the agent — needs a proto/agent change,
	// a fleet-wide agent upgrade AND a full dedik churn (Manager.Restore rebuilds
	// state from container labels, which carry no project), i.e. it delivers only
	// after a rollout; this delivers on the next scrape for the whole fleet.
	//
	// Value is always 1 — it is a join key, nothing reads it as a measurement,
	// so multiplying a rule's left side by it preserves that side's $value.
	// project = projects.slug and env = servers.env, exactly the derivation
	// birdman_servers uses, so both series say "project" about the same thing.
	// Cardinality: one series per LIVE dedik (terminal states are excluded, see
	// the query) — the same order as the four per-server series the agent
	// already exports, and it shrinks back as servers are reaped.
	serverInfoDesc = prometheus.NewDesc(
		"birdman_server_info",
		"Reference series (always 1) mapping a live server_id to its project and env, for joining agent-emitted per-server metrics (tick/players/cpu/mem) to their owner.",
		[]string{"server_id", "project", "env"}, nil)
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
		"Total events by kind and project (append-only feed; crash_loop feeds the CrashLoop alert). "+
			"An empty project means a platform event (backups, CA, panel sessions) that belongs to no project.",
		[]string{"kind", "project"}, nil)
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
	// allocFailures — тот же CounterVec, что инкрементят матчмейкер и allocate.
	// Коллектор его НЕ эмитит (вектор зарегистрирован сам по себе), а только
	// ПРЕ-ИНИЦИАЛИЗИРУЕТ нулями, см. preinitAllocFailures.
	allocFailures *prometheus.CounterVec
}

// allocFailureReasons — закрытый набор причин отказа аллокации (он же в Help
// самого счётчика). Держать список здесь приходится потому, что причина —
// строковый лейбл, а не тип; добавил причину в коде — добавь сюда, иначе её
// первый отказ снова станет невидимым для increase().
var allocFailureReasons = []string{"no_capacity", "bad_request", "env_required", "internal"}

// preinitAllocFailures заводит серии birdman_allocation_failures_total со
// значением 0 для каждой пары (причина, проект).
//
// Зачем: счётчик живёт В ПАМЯТИ процесса, и серия пары рождается только в
// момент первого отказа — сразу со значением 1. increase() по серии, которая
// всегда читалась 1, даёт 0, поэтому BufferEmptyAllocFail и AllocationFailures
// пропускали ПЕРВЫЙ отказ, а после каждого рестарта master дыра взводилась
// заново. Явный ноль даёт increase() от чего расти.
//
// Источник проектов — БД на каждом скрейпе, а не хук на создание проекта:
// коллектор и так ходит в базу, новый проект подхватывается сам, а мёртвая
// серия исчезает вместе с рестартом. Кардинальность — причины × проекты,
// то есть единицы серий, а не открытое множество.
//
// При сбое запроса не делаем НИЧЕГО (та же дисциплина, что у нулевой базы
// событий и collectReadyZeros): выдуманный ноль после икоты базы для
// increase() выглядит как сброс счётчика.
// projectSlugs — desired state проектного измерения: кто есть в БД, тот и
// получает нулевые серии. Общий источник для нулевых баз отказов аллокации
// (#966) и событий (#986): оба обязаны существовать ДО первого факта, иначе
// increase() не увидит его.
func (c *dbCollector) projectSlugs(ctx context.Context) ([]string, error) {
	rows, err := c.st.Pool.Query(ctx, `select slug from projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

func (c *dbCollector) preinitAllocFailures(ctx context.Context) {
	if c.allocFailures == nil {
		return
	}
	slugs, err := c.projectSlugs(ctx)
	if err != nil {
		c.log.Error("metrics: projects query failed", "err", err)
		return
	}
	for _, slug := range slugs {
		for _, reason := range allocFailureReasons {
			c.allocFailures.WithLabelValues(reason, slug)
		}
	}
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- serversDesc
	ch <- serverInfoDesc
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

	// Нулевая база счётчика отказов аллокации — до всего остального: серии
	// должны существовать ДО первого отказа, иначе increase() его не увидит.
	c.preinitAllocFailures(ctx)

	// Product + alert-feed metrics (ops.md §1). Each logs-and-continues so one
	// failed query does not blank the rest.
	// Проект берётся ИЗ КОЛОНКИ events.project_id (эпик #968, миграция 000019),
	// а не угадывается по payload: пустая строка означает платформенное событие
	// (бекапы, CA, сессии) — оно принадлежит не проекту, а установке.
	if erows, err := c.st.Pool.Query(ctx, `
		select e.kind, coalesce(p.slug, ''), count(*)
		from events e left join projects p on p.id = e.project_id
		group by e.kind, coalesce(p.slug, '')`); err != nil {
		c.log.Error("metrics: events query failed", "err", err)
	} else {
		seen := make(map[string]bool, len(alertEventKinds))
		ok := true
		for erows.Next() {
			var kind, project string
			var n float64
			if err := erows.Scan(&kind, &project, &n); err != nil {
				c.log.Error("metrics: events scan failed", "err", err)
				ok = false
				break
			}
			seen[kind+"\x00"+project] = true
			ch <- prometheus.MustNewConstMetric(eventsTotalDesc, prometheus.CounterValue, n, kind, project)
		}
		erows.Close()
		if err := erows.Err(); ok && err != nil {
			c.log.Error("metrics: events rows failed", "err", err)
			ok = false
		}
		// Zero baseline for the alert-feeding kinds (tracker #960). This series
		// is DB-derived, so a kind that has never happened has NO row and thus
		// no series — and it springs into existence reading 1 the moment the
		// first such event lands. increase() over a series that has only ever
		// read 1 is 0, so CrashLoop/AgentUpgradeFailed used to miss the FIRST
		// event of their kind and only fire from the second one on. An explicit
		// 0 gives increase() something to rise from; it costs two series. Only
		// on a clean read — inventing a 0 after a failed/partial query would
		// fake a counter reset and could fire the alert on the next real event
		// twice over.
		//
		// С проектным измерением набор нулей стал (вид × проект) и берётся из
		// DESIRED STATE — списка проектов в БД, — а НЕ из фактических строк
		// events. Если бы серии заводились по факту, вид, которого у проекта ещё
		// не было, снова родился бы сразу с 1: те же грабли, только в новой
		// размерности. Плюс платформенная серия с пустым проектом: у CrashLoop
		// есть события, не принадлежащие никакому проекту.
		if ok {
			projects, perr := c.projectSlugs(ctx)
			if perr != nil {
				c.log.Error("metrics: projects query failed", "err", perr)
			} else {
				for _, kind := range alertEventKinds {
					for _, project := range append([]string{""}, projects...) {
						if !seen[kind+"\x00"+project] {
							ch <- prometheus.MustNewConstMetric(eventsTotalDesc, prometheus.CounterValue, 0, kind, project)
						}
					}
				}
			}
		}
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
		from nodes where cert_not_after is not null and state <> 'dead'`); err != nil {
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
	// readySeen remembers the ready labelsets that ARE backed by rows, so the
	// zero-fill below never emits a duplicate labelset (Gather rejects
	// duplicates and the whole /metrics endpoint would go blank).
	readySeen := make(map[string]bool)
	countedServers := true
	if err != nil {
		c.log.Error("metrics: servers query failed", "err", err)
		countedServers = false
	} else {
		for rows.Next() {
			var project, env, state, region, semver string
			var production bool
			var count float64
			if err := rows.Scan(&project, &env, &production, &state, &region, &semver, &count); err != nil {
				c.log.Error("metrics: servers scan failed", "err", err)
				countedServers = false
				break
			}
			productionStr := strconv.FormatBool(production)
			if state == "ready" {
				readySeen[readyKey(project, env, productionStr, region, semver)] = true
			}
			ch <- prometheus.MustNewConstMetric(serversDesc, prometheus.GaugeValue,
				count, project, env, productionStr, state, region, semver)
		}
		rows.Close()
		if err := rows.Err(); countedServers && err != nil {
			c.log.Error("metrics: servers rows failed", "err", err)
			countedServers = false
		}
	}
	// The zeros are DERIVED from a successful count: after a failed or partial
	// read "no ready servers" is unknown, not zero, and emitting it would turn
	// a database hiccup into a false critical page.
	if countedServers {
		c.collectReadyZeros(ctx, ch, readySeen)
	}

	c.collectServerInfo(ctx, ch)

	// Ревокнутые ноды серии НЕ получают. `dead` ставится ТОЛЬКО ручной
	// ревокацией (ops.md §1, автоматикой никогда) — это заявление оператора
	// «бокса больше нет, перестань за ним следить». Пока серия эмитится, её
	// heartbeat_age растёт вечно и NodeDown по ней горит вечно; такой шум хуже
	// отсутствующего алерта, потому что маскирует НАСТОЯЩИЙ NodeDown. Убираем
	// серию — staleness гасит алерт сам. Поймано на дев-стенде 11.08.2026: две
	// ревокнутые ноды держали NodeDown в firing 24 дня.
	rows, err = c.st.Pool.Query(ctx, `
		select hostname, region, greatest(extract(epoch from (now() - last_heartbeat_at)), 0)
		from nodes where last_heartbeat_at is not null and state <> 'dead'`)
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

// alertEventKinds are the event kinds an alert rule reads through increase()
// (rules.yml.j2: CrashLoop, AgentUpgradeFailed). They carry an explicit 0
// baseline on every scrape so the FIRST event of a kind is visible — see the
// events block of dbCollector.Collect (tracker #960).
var alertEventKinds = []string{store.EventCrashLoop, store.EventAgentUpgradeFailed}

// readyKey is the comparison key of one birdman_servers ready labelset
// (state is fixed to "ready", so it is not part of the key). NUL separates the
// parts: no label value here can contain it.
func readyKey(project, env, production, region, semver string) string {
	return strings.Join([]string{project, env, production, region, semver}, "\x00")
}

// collectReadyZeros emits an explicit birdman_servers{state="ready"} = 0 for
// every fleet that wants a warm buffer and currently has no ready server at
// all (tracker #960).
//
// Why it must exist: birdman_servers is a grouped COUNT, so a combination with
// no rows produces NO series — and a Prometheus aggregation over a missing
// series is EMPTY, not 0. BufferEmptyReadyProd/NonProd
// (`sum by (region, project) (...) == 0`) could therefore never hold: both
// alerts were dead in every fleet state, which is worse than having no alert —
// the case looked covered. The explicit zero is what makes `== 0` reachable;
// the rule expression needs no change.
//
// Where the live combinations come from: fleet_configs — the DESIRED state,
// i.e. exactly the (project, env, region) triples an operator asked to keep
// warm. version is the fleet's active semver (empty when nothing is deployed
// yet: a configured fleet with no version is precisely the "next player gets
// no dedic" state the alert is for). Cardinality is therefore bounded by the
// number of fleet rows — one series each, never a cartesian product over
// versions or states, and only for triples that have no ready row already.
//
// buffer_ready = 0 is skipped on purpose: a fleet that deliberately keeps no
// warm buffer is not an incident, and paging on it would be noise. Keeping
// that filter in the metric is also what keeps the alert expression free of a
// buffer_ready join.
func (c *dbCollector) collectReadyZeros(ctx context.Context, ch chan<- prometheus.Metric, readySeen map[string]bool) {
	// production joins environments by (project_id, env) — same derivation as
	// the server rows (I6), so a zero and a real count share one labelset shape.
	rows, err := c.st.Pool.Query(ctx, `
		select p.slug, f.env, e.production, f.region, coalesce(v.semver, '')
		from fleet_configs f
		join projects p on p.id = f.project_id
		join environments e on e.project_id = f.project_id and e.name = f.env
		left join versions v on v.id = f.active_version
		where f.buffer_ready > 0`)
	if err != nil {
		c.log.Error("metrics: ready-buffer fleets query failed", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var project, env, region, semver string
		var production bool
		if err := rows.Scan(&project, &env, &production, &region, &semver); err != nil {
			c.log.Error("metrics: ready-buffer fleets scan failed", "err", err)
			return
		}
		productionStr := strconv.FormatBool(production)
		key := readyKey(project, env, productionStr, region, semver)
		if readySeen[key] {
			continue
		}
		readySeen[key] = true // fleet rows are unique per (project, env, region), but never risk a duplicate labelset
		ch <- prometheus.MustNewConstMetric(serversDesc, prometheus.GaugeValue, 0,
			project, env, productionStr, "ready", region, semver)
	}
	if err := rows.Err(); err != nil {
		c.log.Error("metrics: ready-buffer fleets rows failed", "err", err)
	}
}

// collectServerInfo emits birdman_server_info{server_id, project, env} = 1 for
// every LIVE dedik (tracker #958) — the join key that lets a rule over an
// agent-emitted per-server metric inherit the owning project.
//
// Only non-terminal states are reported: 'failed' and 'reaped' mean the
// container is gone, so no agent series exists to join against, and including
// them would let the series grow without bound over the lifetime of the
// database. Live rows, by contrast, are bounded by the fleet size.
//
// A failed or partial read emits NOTHING for the affected rows — the #960
// discipline: an invented answer is worse than a missing one. Here the missing
// answer is SAFE by construction, because the consuming rule is written as a
// non-hiding join (rules.yml.j2, TickDegraded): the branch that does not match
// this series still fires, just without the project label, i.e. as a platform
// alert visible under any ?project= filter. Degradation widens the alert, it
// never drops it.
func (c *dbCollector) collectServerInfo(ctx context.Context, ch chan<- prometheus.Metric) {
	// project/env derived exactly like birdman_servers: projects.slug and the
	// SERVER's env (I6 — never the node's), so both series mean the same thing
	// by "project" and a dashboard may join them.
	rows, err := c.st.Pool.Query(ctx, `
		select s.id::text, p.slug, s.env
		from servers s
		join projects p on p.id = s.project_id
		where s.state not in ('failed', 'reaped')`)
	if err != nil {
		c.log.Error("metrics: server info query failed", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var serverID, project, env string
		if err := rows.Scan(&serverID, &project, &env); err != nil {
			c.log.Error("metrics: server info scan failed", "err", err)
			return
		}
		// servers.id is the primary key, so the labelset is unique by
		// construction — Gather cannot trip over a duplicate here.
		ch <- prometheus.MustNewConstMetric(serverInfoDesc, prometheus.GaugeValue, 1, serverID, project, env)
	}
	if err := rows.Err(); err != nil {
		c.log.Error("metrics: server info rows failed", "err", err)
	}
}

// b2f maps a bool to the Prometheus 1/0 gauge convention.
func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
