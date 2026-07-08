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
	EventNodeRecovered     = "node_recovered"
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
)
