package metrics_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/tlsutil"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestNodeCapacityMetric: with 2 active nodes in region=eu (capacity_slots
// 8+8), birdman_node_capacity_slots{region="eu"} must report the summed
// capacity — the panel's utilization-over-time chart (Statistics v1) reads
// this per region on top of the existing point-in-time
// store.RegionUtilization snapshot. A third, non-active (draining) node must
// NOT count: the gauge is the denominator of true utilization, so it follows
// the same active-nodes-only rule as store.RegionUtilization's capacity CTE.
func TestNodeCapacityMetric(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	f.AddNode(t, "node-2", "203.0.113.11", 8)
	draining := f.AddNode(t, "node-3", "203.0.113.12", 100)
	if _, err := st.Pool.Exec(t.Context(),
		`update nodes set state = 'draining' where id = $1::uuid`, draining); err != nil {
		t.Fatalf("drain node-3: %v", err)
	}

	m := metrics.New(st, testLog())

	got := findGauge(t, m.Registry, "birdman_node_capacity_slots", map[string]string{"region": "eu"})
	if got != 16 {
		t.Fatalf("capacity eu = %v, want 16 (active nodes only; draining node-3's 100 slots must not count)", got)
	}
}

// --- mTLS agentlink v1 observability (design §3 "Наблюдаемость") ---

// birdman_agentlink_sessions{auth} reads live-session counts from the hub
// via the wired callback: the operator flips agentlink_auth to mtls once
// {auth="token"} hits 0, so BOTH series must be present even at 0.
func TestAgentlinkSessionsMetric(t *testing.T) {
	st := testdb.New(t)
	m := metrics.New(st, testLog())
	m.WireAgentlinkSessions(func() (mtls, token int) { return 2, 0 })

	if got := findGauge(t, m.Registry, "birdman_agentlink_sessions", map[string]string{"auth": "mtls"}); got != 2 {
		t.Fatalf("sessions{auth=mtls} = %v, want 2", got)
	}
	if got := findGauge(t, m.Registry, "birdman_agentlink_sessions", map[string]string{"auth": "token"}); got != 0 {
		t.Fatalf("sessions{auth=token} = %v, want an explicit 0 sample", got)
	}
}

// birdman_tls_cert_expiry_timestamp_seconds: cert="ca" is DB-derived from
// internal_ca.not_after (newest active = the signer); cert="server" reads the
// in-memory server-leaf holder via the wired callback. Both feed the
// CertExpiry alert (ops.md §1 TODO).
func TestTLSCertExpiryMetrics(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	caCert, _, err := st.EnsureInternalCA(ctx)
	if err != nil {
		t.Fatalf("EnsureInternalCA: %v", err)
	}
	caNotAfter, err := tlsutil.CertNotAfter(caCert)
	if err != nil {
		t.Fatal(err)
	}

	m := metrics.New(st, testLog())
	serverNotAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	m.WireTLSServerCertExpiry(func() (time.Time, bool) { return serverNotAfter, true })

	if got := findGauge(t, m.Registry, "birdman_tls_cert_expiry_timestamp_seconds", map[string]string{"cert": "ca"}); got != float64(caNotAfter.Unix()) {
		t.Fatalf("tls_cert_expiry{cert=ca} = %v, want %v", got, float64(caNotAfter.Unix()))
	}
	if got := findGauge(t, m.Registry, "birdman_tls_cert_expiry_timestamp_seconds", map[string]string{"cert": "server"}); got != float64(serverNotAfter.Unix()) {
		t.Fatalf("tls_cert_expiry{cert=server} = %v, want %v", got, float64(serverNotAfter.Unix()))
	}
}

// birdman_node_cert_expiry_timestamp_seconds{node} is DB-derived from
// nodes.cert_not_after — the node label is the hostname, consistent with
// birdman_node_heartbeat_age_seconds. Nodes that never enrolled emit nothing.
func TestNodeCertExpiryMetric(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 8)
	f.AddNode(t, "node-2", "203.0.113.11", 8) // never enrolled — no sample

	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetNodeCert(ctx, f.NodeID, "0abc", notAfter, store.EventNodeEnrolled, "0.2.0"); err != nil {
		t.Fatalf("SetNodeCert: %v", err)
	}

	m := metrics.New(st, testLog())
	if got := findGauge(t, m.Registry, "birdman_node_cert_expiry_timestamp_seconds", map[string]string{"node": "node-1"}); got != float64(notAfter.Unix()) {
		t.Fatalf("node_cert_expiry{node=node-1} = %v, want %v", got, float64(notAfter.Unix()))
	}
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "birdman_node_cert_expiry_timestamp_seconds" {
			continue
		}
		for _, met := range mf.GetMetric() {
			for _, p := range met.GetLabel() {
				if p.GetName() == "node" && p.GetValue() == "node-2" {
					t.Fatalf("node-2 never enrolled and must not emit a cert-expiry sample")
				}
			}
		}
	}
}

// findGauge gathers reg and returns the value of the metric matching name and
// exactly the given label set, failing the test if none matches.
func findGauge(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if labelsMatch(met.GetLabel(), labels) {
				return met.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("metric %s%v not found", name, labels)
	return 0
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, p := range pairs {
		if want[p.GetName()] != p.GetValue() {
			return false
		}
	}
	return true
}
