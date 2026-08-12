package metrics_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/tlsutil"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
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

// birdman_agentlink_pending_commands{node,node_id} reads the hub's
// unacked-queue depths on scrape via the wired callback (Hub.PendingCounts,
// keyed by node_id) and resolves each id to the node's hostname for the node
// label — the stack convention (heartbeat/cert-expiry gauges use hostname
// too); node_id carries the exact uuid and keeps labelsets unique should two
// hostnames ever collide. ONLY nodes with a non-empty queue emit a sample — a
// drained/empty queue produces NO series (the snapshot never emits a 0), so
// the AgentlinkPendingStuck alert (pending>0 held for `for:`) is absent-safe
// (followups §3, ревизия). Enqueue → the node's series is 1; Ack → the series
// disappears; a second node is independent; an id that no longer resolves
// (node row deleted, queue still alive) falls back to node=<uuid>.
func TestPendingCommandsGauge(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8) // hostname node-1, uuid f.NodeID
	node2 := f.AddNode(t, "node-2", "203.0.113.11", 8)

	m := metrics.New(st, testLog())
	hub := agentlink.NewHub(testLog())
	m.WireAgentlinkPendingCommands(hub.PendingCounts)

	start := func(serverID string) *agentlinkv1.MasterMsg {
		return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{ServerId: serverID}}}
	}

	// Non-SetRegistries commands queue regardless of any live session, so no
	// attach is needed to build pending depth. Hub keys = node uuids.
	cmd1 := hub.Send(f.NodeID, start("srv-1"))
	hub.Send(node2, start("srv-2"))

	if got := findGauge(t, m.Registry, "birdman_agentlink_pending_commands", map[string]string{"node": "node-1", "node_id": f.NodeID}); got != 1 {
		t.Fatalf("pending{node=node-1} = %v, want 1 (node label must be the hostname, node_id the uuid)", got)
	}
	if got := findGauge(t, m.Registry, "birdman_agentlink_pending_commands", map[string]string{"node": "node-2", "node_id": node2}); got != 1 {
		t.Fatalf("pending{node=node-2} = %v, want 1 (second node independent)", got)
	}

	// Ack node-1's only command: its queue drains to empty, so its series must
	// vanish entirely (the snapshot emits no 0) while node-2 is untouched.
	hub.Ack(f.NodeID, cmd1)
	if gaugeSeriesPresent(t, m.Registry, "birdman_agentlink_pending_commands", map[string]string{"node": "node-1", "node_id": f.NodeID}) {
		t.Fatalf("pending{node=node-1} still present after Ack — a drained queue must emit no series (0 is never emitted)")
	}
	if got := findGauge(t, m.Registry, "birdman_agentlink_pending_commands", map[string]string{"node": "node-2", "node_id": node2}); got != 1 {
		t.Fatalf("pending{node=node-2} = %v after node-1 Ack, want 1 (independent)", got)
	}

	// Fallback: node-2's row is deleted from nodes while its queue still holds
	// the command — the id no longer resolves to a hostname, so the node label
	// falls back to the uuid (the alert stays absent-safe, but never blind).
	if _, err := st.Pool.Exec(t.Context(), `delete from nodes where id = $1::uuid`, node2); err != nil {
		t.Fatalf("delete node-2 row: %v", err)
	}
	if got := findGauge(t, m.Registry, "birdman_agentlink_pending_commands", map[string]string{"node": node2, "node_id": node2}); got != 1 {
		t.Fatalf("pending{node=<uuid>} = %v after node row deletion, want 1 (fallback node label = uuid)", got)
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

// birdman_agentlink_registries_withheld_total counts SetRegistries deliveries
// the gate skipped for untrusted sessions (design §3 «Гейт SetRegistries»).
// The hub increments it via SetRegistriesWithheldCounter (main.go wiring); a
// plain counter is present at 0 from registration.
func TestAgentlinkRegistriesWithheldCounter(t *testing.T) {
	st := testdb.New(t)
	m := metrics.New(st, testLog())

	if got := findCounter(t, m.Registry, "birdman_agentlink_registries_withheld_total", nil); got != 0 {
		t.Fatalf("withheld counter = %v at registration, want 0", got)
	}
	m.AgentlinkRegistriesWithheld.Inc()
	if got := findCounter(t, m.Registry, "birdman_agentlink_registries_withheld_total", nil); got != 1 {
		t.Fatalf("withheld counter = %v after Inc, want 1", got)
	}
}

// findCounter is findGauge for counters.
func findCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
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
				return met.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("metric %s%v not found", name, labels)
	return 0
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

// gaugeSeriesPresent reports whether reg has a gauge series matching name and
// exactly the given label set — the absence check for a metric that emits no
// sample under some states (here: a drained pending queue).
func gaugeSeriesPresent(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) bool {
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
				return true
			}
		}
	}
	return false
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

// TestHeartbeatAgeCarriesOwningProject: NodeDown is a PROJECT alert, and the
// label comes from the series, not from the rule (tracker #1064 — same shape as
// birdman_allocation_failures_total in #955: the expr never mentions project).
//
// The case that makes it matter is one BOX carrying nodes of different projects
// (#1065): the machine dies, one NodeDown fires per node, and without the label
// the non-hiding ?project= filter (master.md §6) shows every operator their
// neighbours' alerts — noise they cannot act on. So the two nodes below share a
// public_ip on purpose: that is exactly the multi-node box, and each series must
// name its OWN project.
func TestHeartbeatAgeCarriesOwningProject(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 8)

	// Вторая нода того же ЖЕЛЕЗА (тот же public_ip), но чужого проекта:
	// CreateNode заводит проект сам (ensureProject внутри его транзакции).
	node, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project:       "khl-legends",
		Region:        "eu",
		Hostname:      "node-1-khl",
		PublicIP:      "203.0.113.10",
		CapacitySlots: 4,
	})
	if err != nil {
		t.Fatalf("create neighbour node: %v", err)
	}
	f.SetHeartbeatAge(t, node.ID, 0)

	m := metrics.New(st, testLog())
	if !gaugeSeriesPresent(t, m.Registry, "birdman_node_heartbeat_age_seconds", map[string]string{
		"node": "node-1", "region": "eu", "project": "game",
	}) {
		t.Error("серия ноды game не несёт свой проект — NodeDown остался бы платформенным")
	}
	if !gaugeSeriesPresent(t, m.Registry, "birdman_node_heartbeat_age_seconds", map[string]string{
		"node": "node-1-khl", "region": "eu", "project": "khl-legends",
	}) {
		t.Error("нода соседнего проекта на том же железе не получила СВОЙ проект — оператор увидел бы чужой алерт как свой")
	}
}

// TestRevokedNodeEmitsNoAlertableSeries: `dead` is set ONLY by explicit manual
// revocation — the operator saying "this box is gone, stop caring". Its
// heartbeat age grows forever and its cert eventually expires, so as long as
// either series is emitted, NodeDown and CertExpiry fire for it permanently.
// That noise is worse than a missing alert: it masks the real NodeDown, which
// is the one alert that must never be ignored. Dropping the series lets
// staleness resolve the alert on its own.
func TestRevokedNodeEmitsNoAlertableSeries(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 8)

	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetNodeCert(ctx, f.NodeID, "0abc", notAfter, store.EventNodeEnrolled, "0.2.0"); err != nil {
		t.Fatalf("SetNodeCert: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`update nodes set last_heartbeat_at = now() - interval '10 days' where id = $1::uuid`, f.NodeID); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}

	live := metrics.New(st, testLog())
	if !gaugeSeriesPresent(t, live.Registry, "birdman_node_heartbeat_age_seconds", map[string]string{"node": "node-1", "region": "eu", "project": "game"}) {
		t.Fatal("a live node must emit heartbeat age")
	}
	if !gaugeSeriesPresent(t, live.Registry, "birdman_node_cert_expiry_timestamp_seconds", map[string]string{"node": "node-1"}) {
		t.Fatal("a live node must emit cert expiry")
	}

	// Revocation: the only path to `dead` (ops.md §1 — never set by automation).
	if _, err := st.Pool.Exec(ctx, `update nodes set state = 'dead' where id = $1::uuid`, f.NodeID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	revoked := metrics.New(st, testLog())
	if gaugeSeriesPresent(t, revoked.Registry, "birdman_node_heartbeat_age_seconds", map[string]string{"node": "node-1", "region": "eu", "project": "game"}) {
		t.Error("revoked node still emits heartbeat age — NodeDown would fire forever")
	}
	if gaugeSeriesPresent(t, revoked.Registry, "birdman_node_cert_expiry_timestamp_seconds", map[string]string{"node": "node-1"}) {
		t.Error("revoked node still emits cert expiry — CertExpiry would fire forever")
	}
}
