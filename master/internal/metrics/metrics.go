// Package metrics exposes the master Prometheus metrics
// (docs/specs/ops.md §1: canonical metric names).
package metrics

import (
	"context"
	"log/slog"
	"time"

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
	reg.MustRegister(m.AllocDuration, m.AllocFailures, m.MMQueueDepth, m.MMTimeToMatch, m.MMTickets, m.DeployPrepull)
	reg.MustRegister(&dbCollector{st: st, log: log})
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
)

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
