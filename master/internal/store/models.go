package store

import "time"

type Node struct {
	ID              string         `json:"id"`
	ProjectID       string         `json:"project_id"`
	Project         string         `json:"project"`
	Region          string         `json:"region"`
	Env             string         `json:"env"` // окружение ноды (environments v1); новые ноды входят как dev
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
	Env       string    `json:"env"` // окружение версии (environments v1); заменило прежний лейбл (staging→dev, prod→prod)
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	// DeprecatedAt is set when the deploy manager demotes the version
	// active → deprecated; reap_ttl_min counts from it (итерация 3).
	DeprecatedAt *time.Time `json:"deprecated_at,omitempty"`
	// PromotedFrom — provenance версии, созданной промоутом dev→prod (env v1,
	// §4): id исходной версии; nil для обычной регистрации. W1 только протягивает
	// колонку — сам промоут появляется в W2.
	PromotedFrom *string `json:"promoted_from,omitempty"`
}

// DisabledVersion — версия, только что перешедшая в disabled, в объёме, нужном
// диспатчеру RemoveImage (environments v1 §6б): (ProjectID, Env) чтобы
// перечислить ноды окружения, ImageRef — что снять. Один и тот же тип отдают все
// три пути перехода в disabled (флип-демоут старшей deprecated в ActivateVersion,
// TTL DisableExpiredDeprecated, ретеншн RetireVersions), поэтому снятие образов
// единообразно. Semver/VersionID — для логов и событий.
type DisabledVersion struct {
	VersionID string
	ProjectID string
	Env       string
	ImageRef  string
	Semver    string
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
	Env           string  `json:"env"` // окружение флота (environments v1); PK теперь (project, env, region)
	Region        string  `json:"region"`
	ActiveVersion *string `json:"active_version,omitempty"`
	// Denormalized from versions for reconcile convenience.
	ActiveImageRef string `json:"active_image_ref,omitempty"`
	BufferReady    int32  `json:"buffer_ready"`
	MaxServers     int32  `json:"max_servers"`
	ReapTTLMin     int32  `json:"reap_ttl_min"`
}

type Event struct {
	ID   int64     `json:"id"`
	TS   time.Time `json:"ts"`
	Kind string    `json:"kind"`
	// Project — слаг ЖИВОГО владельца события (эпик #968): поле проецируется
	// join'ом по `project_id`. Пусто = живого владельца нет, и это ДВА разных
	// состояния, которые по этому полю неразличимы (tracker #1083):
	// платформенное ПО РОЖДЕНИЮ (бекапы, CA, сессии панели проекта не имеют) и
	// осиротевшее — история удалённого проекта, которой FK обнулил `project_id`.
	// Различает их снимок `events.project_slug` (миграция 000020), но ни один
	// читатель его НЕ ОТДАЁТ — он работает только в `where`, поэтому осиротевшая
	// строка приезжает подписанной пустым проектом. Привязанному ключу она не
	// приезжает вовсе (сужение её отсекает), так что пустой project у него —
	// всегда платформенное по рождению; непривязанному (панель, глобальный,
	// admin) фильтр пуст, и осиротевшее видно, тоже с пустым полем. Панели поле нужно
	// для событий, приходящих ЖИВЫМ стримом: стрим один на сессию и открывается
	// выше выбора проекта, поэтому по ВЫБРАННОМУ в панели проекту она фильтрует
	// их сама, и раньше проект приходилось угадывать по payload. Арендаторская
	// граница тут ни при чём: с #999 стрим сужается привязкой ключа, просто у
	// сессии панели привязки нет.
	Project   string         `json:"project,omitempty"`
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
	EventNodeCreated    = "node_created"
	EventNodeQuarantine = "node_quarantine"
	EventNodeDown       = "node_down" // quarantine дольше node_down_after_min → down (итерация 5 follow-up; dead = ручная ревокация, не отсюда)
	EventNodeRecovered  = "node_recovered"
	EventNodeDrain      = "node_drain"   // admin drained a node (итерация 4)
	EventNodeUndrain    = "node_undrain" // admin lifted a node drain (итерация 4)
	// EventNodeRevoked — нода выведена из флота навсегда (POST
	// /v1/nodes/{id}/revoke): state → dead. Payload {from, hostname, region, env}.
	// Единственный путь в `dead`: автоматика туда не переводит НИКОГДА — она
	// доводит молчащую ноду только до `down` (EventNodeDown). Строка ноды
	// остаётся: на неё ссылается история серверов и матчей.
	EventNodeRevoked = "node_revoked"
	// EventProjectCreated / EventProjectDeleted — явное управление проектами из
	// админки (POST/DELETE /v1/projects). До них проект заводился молча внутри
	// ensureProject (побочный эффект регистрации ноды или версии) и не удалялся
	// никак, поэтому следа в аудите у обеих операций не было вовсе.
	EventProjectCreated = "project_created"
	EventProjectDeleted = "project_deleted"
	// EventNodeEnvChanged — нода переведена в другое окружение (environments v1,
	// PATCH /v1/nodes/{id} {env}). Payload {from, to}. Разрешён только пустой
	// ноде в любом стейте, кроме dead (§2).
	EventNodeEnvChanged = "node_env_changed"
	// EventEnvironmentDeleted — окружение удалено вместе с содержимым (DELETE
	// /v1/environments/{project}/{name} с подтверждением вводом имени). Payload
	// {project, name, production, versions, fleets, matches, servers,
	// api_keys_revoked} — сколько чего снёс каскад; пишется в той же транзакции
	// ДО удаления самой строки окружения. Ноды не каскадятся никогда (жёсткое
	// предусловие «ноль нод»), поэтому их в payload нет.
	EventEnvironmentDeleted = "environment_deleted"
	EventServerFailed       = "server_failed"
	EventServerRecovered    = "server_recovered"
	EventCrashLoop          = "crash_loop"
	EventAllocationFailed   = "allocation_failed"
	EventVersionRegistered  = "version_registered"
	// EventVersionPromoted — версия создана промоутом между окружениями
	// (environments v1 §4, POST /v1/promote). Payload {project, semver, from_env,
	// to_env, image_ref}; provenance promoted_from пишется в саму строку версии.
	// Пишется только на insert-пути промоута (идемпотентный реюз не дублирует).
	EventVersionPromoted = "version_promoted"
	EventFleetUpdated    = "fleet_updated"

	// Deploy manager (итерация 3, docs/specs/master.md §5).
	EventDeployStarted    = "deploy_started"     // version → prepulling, PrePull sent
	EventDeployNodePulled = "deploy_node_pulled" // one node reported pulled
	EventDeployActivated  = "deploy_activated"   // atomic flip done
	EventDeployFailed     = "deploy_failed"      // prepull timeout/failure → abort
	EventDeployRolledBack = "deploy_rolled_back" // deprecated ↔ active flip back
	// EventDeployNoNodes — деплой активировался, а греть и запускать было НЕ НА ЧЕМ:
	// у (project, env) ноль живых нод (tracker #1071). Пишется в единственной
	// воронке deploy.startJob, поэтому покрывает все пути сразу — ручной
	// POST /v1/deploy, промоут, авто-деплой из POST /v1/versions, Resume и
	// SweepOrphans. Раньше этот факт жил только в логе мастера, куда оператор
	// нового проекта не ходит: снаружи «деплой прошёл», а дедиков ноль, и причину
	// («нода принадлежит РОВНО одному проекту», #1064) приходилось искать руками.
	// Payload {project, env, semver, image_ref} + ref version_id — та же форма, что
	// у deploy_started, поэтому проектное сужение /v1/events и env-фильтр панели
	// работают без отдельного правила.
	EventDeployNoNodes   = "deploy_no_nodes"
	EventVersionDisabled = "version_disabled" // deprecated → disabled (TTL/flip)
	EventServerDrain     = "server_drain"     // per-server drain sent (reap TTL)
	// EventVersionRetired — версия ушла registered → disabled ретеншном
	// (environments v1 §6а, RetireVersions): env.retention_keep>0, версия сверх keep
	// по created_at desc, старше 1ч. Единственный путь registered→disabled.
	// Payload {semver, env}. Отличается от version_disabled (флип/TTL из
	// deprecated) намеренно — панель красит и фильтрует его отдельно (§8).
	EventVersionRetired = "version_retired"

	// Self-upgrade (итерация 4, docs/specs/agent.md §7, master.md §6).
	EventAgentUpgrade          = "agent_upgrade"           // UpgradeAgent command sent
	EventAgentUpgradeSucceeded = "agent_upgrade_succeeded" // node re-Hello'd with target version
	EventAgentUpgradeFailed    = "agent_upgrade_failed"    // no re-Hello with target version in time

	// Backups v1: прогон бекапа упал (pg_dump/ротация/S3) либо несовпадение
	// major-версий pg_dump и сервера. payload: {kind, error}.
	EventBackupFailed = "backup_failed"

	// API-key admin actions (П2 Access screen, docs/specs/master.md §6). The
	// audit payload carries key_id/name/scopes — never the secret.
	EventAPIKeyCreated = "apikey_created" // POST /v1/apikeys
	EventAPIKeyRevoked = "apikey_revoked" // DELETE /v1/apikeys/{id}
	// EventAPIKeyPurged (registries v1,
	// docs/superpowers/specs/2026-07-09-registries-design.md §6): hard-delete
	// of an already-revoked key, so revoked keys stop piling up in the admin
	// list. Payload carries only {name} — the row (and its id) is gone.
	EventAPIKeyPurged = "apikey_purged" // DELETE /v1/apikeys/{id}?purge=true

	// Alert mute/suppression (П2 Alerts screen, master.md §6). alert_mutes is the
	// source of truth; a mute is reflected muted:true AND mirrored best-effort
	// into a real alertmanager silence (internal/amsilence, ops.md §1, tracker
	// #245). Payload carries mute_id/alertname/region — no secrets.
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
