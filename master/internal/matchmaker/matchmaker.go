// Package matchmaker is the v0 matchmaker of birdman-master
// (docs/specs/master.md §4): in-memory ticket queues per (region,
// compat-bucket) on top of the allocation API (store.Allocate). Tickets are
// persisted encrypted in PostgreSQL, together with atomic assignments and an
// allocation command outbox. Identity, invitations and reconnect remain
// application concerns; see docs/party-matchmaking.md.
package matchmaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
)

// Ticket statuses. queued is the only non-terminal one.
const (
	StatusQueued         = "queued"
	StatusMatched        = "matched"
	StatusUpdateRequired = "update_required"
	StatusCancelled      = "cancelled"
	StatusExpired        = "expired"
)

// ErrInvalid marks ticket validation errors (HTTP 400).
var ErrInvalid = errors.New("invalid_ticket")

var ErrRequestConflict = errors.New("request_conflict")

// RegionPing is one client-measured rtt sample (GET /v1/qos targets).
type RegionPing struct {
	Region string `json:"region"`
	RTTms  int32  `json:"rtt_ms"`
}

// Result is what every participant of a formed match receives.
type Result struct {
	Host      string   `json:"host"`
	Port      int32    `json:"port"`
	MatchID   string   `json:"match_id"`
	JoinToken string   `json:"join_token,omitempty"`
	ServerID  string   `json:"server_id,omitempty"`
	VersionID string   `json:"version_id,omitempty"`
	TicketIDs []string `json:"ticket_ids,omitempty"`
}

// Ticket is an immutable snapshot returned to the API layer.
type Ticket struct {
	ID              string       `json:"ticket_id"`
	Project         string       `json:"project"`
	Env             string       `json:"env"`
	PlayerID        string       `json:"player_id"`
	ClientVersion   string       `json:"client_version"`
	Regions         []RegionPing `json:"regions"`
	Status          string       `json:"status"`
	CreatedAt       time.Time    `json:"created_at"`
	Match           *Result      `json:"match,omitempty"`
	Members         []Member     `json:"members,omitempty"`
	PartyID         string       `json:"party_id,omitempty"`
	PartyRevision   int64        `json:"party_revision,omitempty"`
	ClientRequestID string       `json:"client_request_id,omitempty"`
}

type Config struct {
	Tick           time.Duration // queue scan period (default 500ms)
	WidenAfter     time.Duration // widen to the player's next region (default 30s)
	TicketTTL      time.Duration // queued longer → expired (default 120s)
	DefaultProject string        // used when the ticket names no project

	// Compat decides client↔server version compatibility: the default
	// MAJOR.MINOR rule plus compat.overrides from the master config
	// (docs/specs/ops.md §3). nil → plain default rule.
	Compat *Compat

	JoinTokenEnabled bool // off by default (master.md §4: v0 optional)
	JoinTokenSecret  []byte
	JoinTokenTTL     time.Duration // default 60s
	Policies         []QueuePolicy // explicit project/env overrides; full-only by default
}

func (c Config) withDefaults() Config {
	if c.Tick <= 0 {
		c.Tick = 500 * time.Millisecond
	}
	if c.WidenAfter <= 0 {
		c.WidenAfter = 30 * time.Second
	}
	if c.TicketTTL <= 0 {
		c.TicketTTL = 120 * time.Second
	}
	if c.JoinTokenTTL <= 0 {
		c.JoinTokenTTL = 60 * time.Second
	}
	if c.Compat == nil {
		c.Compat = &Compat{} // plain MAJOR.MINOR default rule
	}
	return c
}

// ticket is the internal mutable state, guarded by Matchmaker.mu.
type ticket struct {
	id            string
	project       string
	env           string
	playerID      string
	clientVersion string
	bucket        string // MajorMinor(clientVersion)
	regions       []RegionPing
	createdAt     time.Time
	members       []Member
	partyID       string
	partyRevision int64
	requestID     string
	fingerprint   [32]byte
	payload       string // never included in Ticket or Result

	status     string
	match      *Result
	terminalAt time.Time
	done       chan struct{} // closed once, on leaving queued
}

// playerKey is the anti-dup key: (project, env, player). A new ticket for the
// same player in the same env cancels the previous one; two envs of a player
// are independent (environments v1 §3, M6).
func scopeKey(project, env, id string) string { return project + "\x00" + env + "\x00" + id }

func (t *ticket) playerKeys() []string {
	if len(t.members) == 0 {
		return []string{scopeKey(t.project, t.env, t.playerID)}
	}
	keys := make([]string, 0, len(t.members))
	for _, m := range t.members {
		keys = append(keys, scopeKey(t.project, t.env, m.PlayerID))
	}
	return keys
}

func (t *ticket) snapshot() Ticket {
	regions := make([]RegionPing, len(t.regions))
	copy(regions, t.regions)
	var match *Result
	if t.match != nil {
		m := *t.match
		m.TicketIDs = append([]string(nil), t.match.TicketIDs...)
		match = &m
	}
	return Ticket{
		ID: t.id, Project: t.project, Env: t.env, PlayerID: t.playerID,
		ClientVersion: t.clientVersion, Regions: regions,
		Status: t.status, CreatedAt: t.createdAt, Match: match,
		Members: cloneMembers(t.members), PartyID: t.partyID, PartyRevision: t.partyRevision, ClientRequestID: t.requestID,
	}
}

type Matchmaker struct {
	st  *store.Store
	m   *metrics.Metrics
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	tickets     map[string]*ticket
	byPlayer    map[string]*ticket // active (queued) ticket per (project, env, player)
	byParty     map[string]*ticket
	byRequest   map[string]*ticket // cache; durable request mapping remains in SQL
	dirty       map[string]bool
	regionsSeen map[regionEnv]bool // (region, env) pairs ever seen — for zeroing the queue-depth gauge
}

func New(st *store.Store, m *metrics.Metrics, cfg Config, log *slog.Logger) *Matchmaker {
	return &Matchmaker{
		st: st, m: m, cfg: cfg.withDefaults(), log: log,
		tickets:     map[string]*ticket{},
		byPlayer:    map[string]*ticket{},
		byParty:     map[string]*ticket{},
		byRequest:   map[string]*ticket{},
		dirty:       map[string]bool{},
		regionsSeen: map[regionEnv]bool{},
	}
}

// Run ticks the queues every cfg.Tick until ctx is done.
func (mm *Matchmaker) Run(ctx context.Context) {
	t := time.NewTicker(mm.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := mm.RunOnce(tctx); err != nil && ctx.Err() == nil {
				mm.log.Error("mm: tick failed", "err", err)
			}
			cancel()
		}
	}
}

type SubmitParams struct {
	Project           string
	Env               string // окружение тикета (опционально); резолв — §3
	PlayerID          string
	ClientVersion     string
	Regions           []RegionPing
	Members           []Member
	PartyID           string
	PartyRevision     int64
	ClientRequestID   string
	AllocationPayload string
}

// Submit validates and enqueues a ticket. Anti-dup (master.md §4, env v1 §3 M6):
// a new ticket for the same (project, env, player) cancels the previous queued one.
// A client version incompatible with every live server version (active or
// deprecated in the multi-version window, per compat rules) is rejected as
// update_required right away (the ticket is still stored for GET).
func (mm *Matchmaker) Submit(ctx context.Context, p SubmitParams) (Ticket, error) {
	members, regions, err := normalizeParty(p)
	if err != nil {
		return Ticket{}, err
	}
	bucket, err := mm.cfg.Compat.BucketOf(p.ClientVersion)
	if err != nil {
		return Ticket{}, fmt.Errorf("%w: client_version: %v", ErrInvalid, err)
	}
	project := p.Project
	if project == "" {
		project = mm.cfg.DefaultProject
	}
	if project == "" {
		// v0 convenience: a single-project master needs no project field.
		project, err = mm.st.SoleProjectSlug(ctx)
		if err != nil {
			return Ticket{}, fmt.Errorf("resolve project: %w", err)
		}
	}
	proj, err := mm.st.GetProject(ctx, project)
	if err != nil {
		return Ticket{}, err
	}

	// Environment resolution (environments v1 §3): an explicit env is validated;
	// otherwise fall back to the project's sole env with live nodes (a
	// single-env stand needs no env field). Zero or several such envs → the
	// ticket must name one (409-shaped, surfaced as bad_request here).
	env := p.Env
	if env == "" {
		// Только ErrConflict от sole-fallback (ноль или несколько env с живыми
		// нодами — двусмысленность) — это клиентская «env обязателен» (ErrInvalid →
		// 400). Любая другая ошибка (сбой БД и т.п.) идёт НАРУЖУ как есть (→500):
		// иначе временный сбой стора маскировался бы под невалидный тикет (M-2).
		env, err = mm.st.SoleEnvWithActiveNodes(ctx, project)
		if errors.Is(err, store.ErrConflict) {
			return Ticket{}, fmt.Errorf("%w: env is required (zero or several environments have active nodes)", ErrInvalid)
		}
		if err != nil {
			return Ticket{}, err
		}
	} else if _, err := mm.st.GetEnvironment(ctx, project, env); err != nil {
		return Ticket{}, err
	}
	// Fingerprint normalized input, not the transport spelling of a bound
	// project/env. Check retries before the live version gate: deployments must
	// not change the result of an already accepted request.
	p.Project, p.Env, p.Members, p.Regions = project, env, members, regions
	fingerprint := requestFingerprint(p)
	mm.mu.Lock()
	defer mm.mu.Unlock()
	tx, err := mm.beginLocked(ctx)
	if err != nil {
		return Ticket{}, err
	}
	defer mm.finishLocked(tx)
	if p.ClientRequestID != "" {
		if mm.byRequest[scopeKey(project, env, p.ClientRequestID)] == nil {
			records, err := tx.FindRequest(ctx, project, env, p.ClientRequestID)
			if err != nil {
				return Ticket{}, err
			}
			if err = mm.merge(records); err != nil {
				return Ticket{}, err
			}
		}
		if prev := mm.byRequest[scopeKey(project, env, p.ClientRequestID)]; prev != nil {
			if prev.fingerprint != fingerprint {
				return Ticket{}, ErrRequestConflict
			}
			return prev.snapshot(), nil
		}
	}
	if len(members) > int(proj.MatchSize) {
		return Ticket{}, fmt.Errorf("%w: party exceeds project match_size", ErrInvalid)
	}

	// Version gate at submit time; re-checked every tick for queued tickets
	// (an active version swap must not strand incompatible clients). Candidates
	// are scoped to the ticket's env.
	candidates, err := mm.st.ActiveRegionVersions(ctx, project, env)
	if err != nil {
		return Ticket{}, err
	}
	status := StatusQueued
	if len(candidates) > 0 && !mm.compatibleWithAny(p.ClientVersion, candidates) {
		status = StatusUpdateRequired
	}

	now := time.Now()
	t := &ticket{
		id: uuid.NewString(), project: project, env: env, playerID: p.PlayerID,
		clientVersion: p.ClientVersion, bucket: bucket, regions: regions,
		createdAt: now, status: status, done: make(chan struct{}),
		members: members, partyID: p.PartyID, partyRevision: p.PartyRevision,
		requestID: p.ClientRequestID, fingerprint: fingerprint, payload: p.AllocationPayload,
	}

	if t.partyID != "" {
		records, err := tx.LatestParty(ctx, project, env, t.partyID)
		if err != nil {
			return Ticket{}, err
		}
		if err = mm.merge(records); err != nil {
			return Ticket{}, err
		}
		if prev := mm.byParty[scopeKey(project, env, t.partyID)]; prev != nil {
			if t.partyRevision <= prev.partyRevision {
				return Ticket{}, ErrRequestConflict
			}
			mm.finalize(prev, StatusCancelled, nil, now)
		}
	}
	for _, key := range t.playerKeys() {
		if prev := mm.byPlayer[key]; prev != nil {
			mm.finalize(prev, StatusCancelled, nil, now)
		}
	}
	mm.tickets[t.id] = t
	mm.dirty[t.id] = true
	if t.partyID != "" {
		mm.byParty[scopeKey(project, env, t.partyID)] = t
	}
	if t.requestID != "" {
		mm.byRequest[scopeKey(project, env, t.requestID)] = t
	}
	if status == StatusQueued {
		for _, key := range t.playerKeys() {
			mm.byPlayer[key] = t
		}
	} else {
		// Terminal at birth (update_required).
		t.terminalAt = now
		close(t.done)
		mm.m.MMTickets.WithLabelValues(status).Inc()
	}
	if err = mm.commitLocked(ctx, tx); err != nil {
		return Ticket{}, err
	}
	return t.snapshot(), nil
}

// Get returns the current ticket snapshot.
func (mm *Matchmaker) Get(id string) (Ticket, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t, ok, err := mm.GetContext(ctx, id)
	if err != nil {
		mm.log.Error("mm: get ticket failed")
		return Ticket{}, false
	}
	return t, ok
}

// Wait long-polls: it returns as soon as the ticket leaves queued, or after
// wait, or when ctx is cancelled — whichever comes first.
func (mm *Matchmaker) Wait(ctx context.Context, id string, wait time.Duration) (Ticket, bool) {
	t, ok, err := mm.WaitContext(ctx, id, wait)
	if err != nil && ctx.Err() != nil {
		return mm.Get(id)
	}
	return t, ok && err == nil
}

func (mm *Matchmaker) WaitContext(ctx context.Context, id string, wait time.Duration) (Ticket, bool, error) {
	_, ok, err := mm.GetContext(ctx, id)
	if err != nil || !ok {
		return Ticket{}, ok, err
	}
	mm.mu.Lock()
	t, ok := mm.tickets[id]
	if !ok {
		mm.mu.Unlock()
		return mm.GetContext(ctx, id)
	}
	if t.status != StatusQueued || wait <= 0 {
		snap := t.snapshot()
		mm.mu.Unlock()
		return snap, true, nil
	}
	done := t.done
	mm.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return mm.GetContext(ctx, id)
}

// Cancel cancels a queued ticket (DELETE /v1/matchmaking/tickets/{id});
// terminal tickets are returned unchanged.
func (mm *Matchmaker) Cancel(id string) (Ticket, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t, ok, err := mm.CancelContext(ctx, id)
	if err != nil {
		mm.log.Error("mm: cancel ticket failed")
		return Ticket{}, false
	}
	return t, ok
}

// finalize moves a queued ticket to a terminal status. Caller holds mm.mu.
func (mm *Matchmaker) finalize(t *ticket, status string, match *Result, now time.Time) {
	if t.status != StatusQueued {
		return
	}
	t.status = status
	t.match = match
	t.terminalAt = now
	mm.dirty[t.id] = true
	close(t.done)
	for _, key := range t.playerKeys() {
		if mm.byPlayer[key] == t {
			delete(mm.byPlayer, key)
		}
	}
	mm.m.MMTickets.WithLabelValues(status).Inc()
	if status == StatusMatched {
		mm.m.MMTimeToMatch.Observe(now.Sub(t.createdAt).Seconds())
	}
}

// retention keeps terminal tickets around for late GETs before the janitor
// drops them from memory.
func (mm *Matchmaker) retention() time.Duration {
	if r := 2 * mm.cfg.TicketTTL; r > 5*time.Minute {
		return r
	}
	return 5 * time.Minute
}

func normalizeRegions(in []RegionPing) ([]RegionPing, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: at least one region is required", ErrInvalid)
	}
	if len(in) > 16 {
		return nil, fmt.Errorf("%w: too many regions (max 16)", ErrInvalid)
	}
	best := map[string]int32{}
	for _, r := range in {
		if r.Region == "" {
			return nil, fmt.Errorf("%w: region name is required", ErrInvalid)
		}
		if r.RTTms < 0 {
			return nil, fmt.Errorf("%w: rtt_ms must be >= 0", ErrInvalid)
		}
		if cur, ok := best[r.Region]; !ok || r.RTTms < cur {
			best[r.Region] = r.RTTms
		}
	}
	out := make([]RegionPing, 0, len(best))
	for region, rtt := range best {
		out = append(out, RegionPing{Region: region, RTTms: rtt})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RTTms != out[j].RTTms {
			return out[i].RTTms < out[j].RTTms
		}
		return out[i].Region < out[j].Region
	})
	return out, nil
}

// compatibleWithAny reports whether the client version may play on at least
// one candidate (compat rules: default MAJOR.MINOR + overrides, ops.md §3).
func (mm *Matchmaker) compatibleWithAny(clientVersion string, candidates []store.RegionVersion) bool {
	for _, c := range candidates {
		if mm.cfg.Compat.Compatible(clientVersion, c.Semver) {
			return true
		}
	}
	return false
}
