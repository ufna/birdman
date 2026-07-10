// Package metrics exposes the master Prometheus metrics
// (docs/specs/ops.md §1: canonical metric names).
package metrics

import (
	"context"
	"errors"
	"log/slog"
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
	MMQueueDepth  *prometheus.GaugeVec   // {region} queued tickets by best region
	MMTimeToMatch prometheus.Histogram   // seconds from ticket submit to matched
	MMTickets     *prometheus.CounterVec // {result} matched|cancelled|update_required|expired

	// Deploy manager (итерация 3, docs/specs/master.md §5).
	DeployPrepull prometheus.Histogram // seconds from deploy start to all nodes pulled

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
		AllocFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "birdman_allocation_failures_total",
			Help: "Failed allocations by reason (no_capacity, bad_request, internal).",
		}, []string{"reason"}),
		MMQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "birdman_mm_queue_depth",
			Help: "Queued matchmaking tickets per best (lowest-rtt) region.",
		}, []string{"region"}),
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
	}
	m.agentlink = &agentlinkCollector{st: st, log: log}
	reg.MustRegister(m.AllocDuration, m.AllocFailures, m.MMQueueDepth, m.MMTimeToMatch, m.MMTickets, m.DeployPrepull)
	reg.MustRegister(&dbCollector{st: st, log: log})
	reg.MustRegister(m.agentlink)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

var (
	serversDesc = prometheus.NewDesc(
		"birdman_servers",
		"Server counts by state, region and version (semver).",
		[]string{"state", "region", "version"}, nil)
	heartbeatAgeDesc = prometheus.NewDesc(
		"birdman_node_heartbeat_age_seconds",
		"Seconds since the last agent heartbeat, per node.",
		[]string{"node", "region"}, nil)
	versionsDesc = prometheus.NewDesc(
		"birdman_versions",
		"Registered version counts by project and state (registered, prepulling, active, deprecated, disabled).",
		[]string{"project", "state"}, nil)
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
	agentlinkSessionsDesc = prometheus.NewDesc(
		"birdman_agentlink_sessions",
		"Live agentlink sessions by auth (mtls: verified client cert; token: node_token). token==0 signals readiness for the mtls flip.",
		[]string{"auth"}, nil)
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
	serverCert atomic.Pointer[func() (time.Time, bool)]
}

func (c *agentlinkCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- agentlinkSessionsDesc
	ch <- tlsCertExpiryDesc
}

func (c *agentlinkCollector) Collect(ch chan<- prometheus.Metric) {
	if f := c.sessions.Load(); f != nil {
		mtls, token := (*f)()
		ch <- prometheus.MustNewConstMetric(agentlinkSessionsDesc, prometheus.GaugeValue, float64(mtls), "mtls")
		ch <- prometheus.MustNewConstMetric(agentlinkSessionsDesc, prometheus.GaugeValue, float64(token), "token")
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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

	rows, err := c.st.Pool.Query(ctx, `
		select s.state, n.region, v.semver, count(*)
		from servers s
		join nodes n on n.id = s.node_id
		join versions v on v.id = s.version_id
		group by 1, 2, 3`)
	if err != nil {
		c.log.Error("metrics: servers query failed", "err", err)
	} else {
		for rows.Next() {
			var state, region, semver string
			var count float64
			if err := rows.Scan(&state, &region, &semver, &count); err != nil {
				c.log.Error("metrics: servers scan failed", "err", err)
				break
			}
			ch <- prometheus.MustNewConstMetric(serversDesc, prometheus.GaugeValue,
				count, state, region, semver)
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
	for project, states := range counts {
		for state, n := range states {
			ch <- prometheus.MustNewConstMetric(versionsDesc, prometheus.GaugeValue,
				float64(n), project, state)
		}
	}
}
