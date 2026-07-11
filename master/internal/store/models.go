package store

import "time"

type Node struct {
	ID              string         `json:"id"`
	ProjectID       string         `json:"project_id"`
	Project         string         `json:"project"`
	Region          string         `json:"region"`
	Hostname        string         `json:"hostname"`
	PublicIP        string         `json:"public_ip"`
	CapacitySlots   int32          `json:"capacity_slots"`
	AgentVersion    string         `json:"agent_version"`
	State           string         `json:"state"`
	LastHeartbeatAt *time.Time     `json:"last_heartbeat_at,omitempty"`
	Labels          map[string]any `json:"labels"`
	CreatedAt       time.Time      `json:"created_at"`
	// mTLS client-cert fields (mTLS agentlink v1, migration 000008) — nil
	// until the node enrolls; enrolled_at is the first-enrollment time.
	CertSerial   *string    `json:"cert_serial,omitempty"`
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`
	EnrolledAt   *time.Time `json:"enrolled_at,omitempty"`
}

type Version struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Project   string    `json:"project"`
	Semver    string    `json:"semver"`
	ImageRef  string    `json:"image_ref"`
	Channel   string    `json:"channel"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	// DeprecatedAt is set when the deploy manager demotes the version
	// active → deprecated; reap_ttl_min counts from it (итерация 3).
	DeprecatedAt *time.Time `json:"deprecated_at,omitempty"`
}

type Server struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	NodeID    string    `json:"node_id"`
	VersionID string    `json:"version_id"`
	Region    string    `json:"region"`
	State     string    `json:"state"`
	Port      int32     `json:"port"`
	Players   int32     `json:"players"`
	TickMS    *float32  `json:"tick_ms,omitempty"`
	MatchID   *string   `json:"match_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type FleetConfig struct {
	ProjectID     string  `json:"project_id"`
	Project       string  `json:"project"`
	Region        string  `json:"region"`
	ActiveVersion *string `json:"active_version,omitempty"`
	// Denormalized from versions for reconcile convenience.
	ActiveImageRef string `json:"active_image_ref,omitempty"`
	BufferReady    int32  `json:"buffer_ready"`
	MaxServers     int32  `json:"max_servers"`
	ReapTTLMin     int32  `json:"reap_ttl_min"`
}

type Event struct {
	ID        int64          `json:"id"`
	TS        time.Time      `json:"ts"`
	Kind      string         `json:"kind"`
	NodeID    *string        `json:"node_id,omitempty"`
	ServerID  *string        `json:"server_id,omitempty"`
	MatchID   *string        `json:"match_id,omitempty"`
	VersionID *string        `json:"version_id,omitempty"`
	Payload   map[string]any `json:"payload"`
}

// ServerReport is one ServerState entry from an agent Hello/Heartbeat.
type ServerReport struct {
	ServerID string
	State    string // agent-side: pulling|starting|ready|allocated|draining|stopped|failed
	Players  int32
	TickMS   float32
	MatchID  string
	Port     int32
	Version  string
}

// Event kinds written by master v0.
const (
	EventNodeCreated       = "node_created"
	EventNodeQuarantine    = "node_quarantine"
	EventNodeDown          = "node_down" // quarantine дольше node_dead_after_min → dead (итерация 5 follow-up)
	EventNodeRecovered     = "node_recovered"
	EventNodeDrain         = "node_drain"   // admin drained a node (итерация 4)
	EventNodeUndrain       = "node_undrain" // admin lifted a node drain (итерация 4)
	EventServerFailed      = "server_failed"
	EventServerRecovered   = "server_recovered"
	EventCrashLoop         = "crash_loop"
	EventAllocationFailed  = "allocation_failed"
	EventVersionRegistered = "version_registered"
	EventFleetUpdated      = "fleet_updated"

	// Deploy manager (итерация 3, docs/specs/master.md §5).
	EventDeployStarted    = "deploy_started"     // version → prepulling, PrePull sent
	EventDeployNodePulled = "deploy_node_pulled" // one node reported pulled
	EventDeployActivated  = "deploy_activated"   // atomic flip done
	EventDeployFailed     = "deploy_failed"      // prepull timeout/failure → abort
	EventDeployRolledBack = "deploy_rolled_back" // deprecated ↔ active flip back
	EventVersionDisabled  = "version_disabled"   // deprecated → disabled (TTL/flip)
	EventServerDrain      = "server_drain"       // per-server drain sent (reap TTL)

	// Self-upgrade (итерация 4, docs/specs/agent.md §7, master.md §6).
	EventAgentUpgrade          = "agent_upgrade"           // UpgradeAgent command sent
	EventAgentUpgradeSucceeded = "agent_upgrade_succeeded" // node re-Hello'd with target version
	EventAgentUpgradeFailed    = "agent_upgrade_failed"    // no re-Hello with target version in time

	// API-key admin actions (П2 Access screen, docs/specs/master.md §6). The
	// audit payload carries key_id/name/scopes — never the secret.
	EventAPIKeyCreated = "apikey_created" // POST /v1/apikeys
	EventAPIKeyRevoked = "apikey_revoked" // DELETE /v1/apikeys/{id}
	// EventAPIKeyPurged (registries v1,
	// docs/superpowers/specs/2026-07-09-registries-design.md §6): hard-delete
	// of an already-revoked key, so revoked keys stop piling up in the admin
	// list. Payload carries only {name} — the row (and its id) is gone.
	EventAPIKeyPurged = "apikey_purged" // DELETE /v1/apikeys/{id}?purge=true

	// Alert mute/suppression annotations (П2 Alerts screen, master.md §6). v0:
	// a mute is a master-level annotation, not a real vmalert/Discord silence
	// (ops.md §1 TODO). Payload carries mute_id/alertname/region — no secrets.
	EventAlertMuted   = "alert_muted"   // POST /v1/alerts/mutes
	EventAlertUnmuted = "alert_unmuted" // DELETE /v1/alerts/mutes/{id}

	// Private registry credentials (registries v1, admin panel "Реестры",
	// docs/superpowers/specs/2026-07-09-registries-design.md §1). Payload
	// carries host/username — NEVER the token: it is write-only and must
	// never land in an audit event, a log line, or a GET response.
	EventRegistryUpserted = "registry_upserted" // POST /v1/registries (create or replace-by-host)
	EventRegistryUpdated  = "registry_updated"  // PATCH /v1/registries/{id} (Реестры v2: partial edit)
	EventRegistryRemoved  = "registry_removed"  // DELETE /v1/registries/{id}

	// Agent mTLS enrollment (mTLS agentlink v1,
	// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §3). The Enroll
	// RPC swaps a node_token (or a live client cert) for a signed client cert.
	// Payload carries {serial, not_after, agent_version} — NEVER the node_token
	// or any key material.
	EventNodeEnrolled    = "node_enrolled"     // first token→cert exchange (Enroll by node_token)
	EventNodeCertRenewed = "node_cert_renewed" // client cert renewed over a live mTLS session
)
