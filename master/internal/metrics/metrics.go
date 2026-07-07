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
	}
	reg.MustRegister(m.AllocDuration, m.AllocFailures, m.MMQueueDepth, m.MMTimeToMatch, m.MMTickets)
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
)

// dbCollector derives gauge metrics from Postgres on scrape.
type dbCollector struct {
	st  *store.Store
	log *slog.Logger
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- serversDesc
	ch <- heartbeatAgeDesc
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

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
}
