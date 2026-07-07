// Package matchmaker is the v0 matchmaker of birdman-master
// (docs/specs/master.md §4): in-memory ticket queues per (region,
// compat-bucket) on top of the allocation API (store.Allocate). Tickets are
// deliberately not persisted — a master restart loses the queue and clients
// simply re-queue (sdk.md). Explicitly out of v0: skills, parties, backfill,
// reconnect.
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

// RegionPing is one client-measured rtt sample (GET /v1/qos targets).
type RegionPing struct {
	Region string `json:"region"`
	RTTms  int32  `json:"rtt_ms"`
}

// Result is what every participant of a formed match receives.
type Result struct {
	Host      string `json:"host"`
	Port      int32  `json:"port"`
	MatchID   string `json:"match_id"`
	JoinToken string `json:"join_token,omitempty"`
}

// Ticket is an immutable snapshot returned to the API layer.
type Ticket struct {
	ID            string       `json:"ticket_id"`
	Project       string       `json:"project"`
	PlayerID      string       `json:"player_id"`
	ClientVersion string       `json:"client_version"`
	Regions       []RegionPing `json:"regions"`
	Status        string       `json:"status"`
	CreatedAt     time.Time    `json:"created_at"`
	Match         *Result      `json:"match,omitempty"`
}

type Config struct {
	Tick           time.Duration // queue scan period (default 500ms)
	WidenAfter     time.Duration // widen to the player's next region (default 30s)
	TicketTTL      time.Duration // queued longer → expired (default 120s)
	DefaultProject string        // used when the ticket names no project

	JoinTokenEnabled bool // off by default (master.md §4: v0 optional)
	JoinTokenSecret  []byte
	JoinTokenTTL     time.Duration // default 60s
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
	return c
}

// ticket is the internal mutable state, guarded by Matchmaker.mu.
type ticket struct {
	id            string
	project       string
	playerID      string
	clientVersion string
	bucket        string // MajorMinor(clientVersion)
	regions       []RegionPing
	createdAt     time.Time

	status     string
	match      *Result
	terminalAt time.Time
	done       chan struct{} // closed once, on leaving queued
}

func (t *ticket) playerKey() string { return t.project + "\x00" + t.playerID }

func (t *ticket) snapshot() Ticket {
	regions := make([]RegionPing, len(t.regions))
	copy(regions, t.regions)
	var match *Result
	if t.match != nil {
		m := *t.match
		match = &m
	}
	return Ticket{
		ID: t.id, Project: t.project, PlayerID: t.playerID,
		ClientVersion: t.clientVersion, Regions: regions,
		Status: t.status, CreatedAt: t.createdAt, Match: match,
	}
}

type Matchmaker struct {
	st  *store.Store
	m   *metrics.Metrics
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	tickets     map[string]*ticket
	byPlayer    map[string]*ticket // active (queued) ticket per (project, player)
	regionsSeen map[string]bool    // for zeroing the queue-depth gauge
}

func New(st *store.Store, m *metrics.Metrics, cfg Config, log *slog.Logger) *Matchmaker {
	return &Matchmaker{
		st: st, m: m, cfg: cfg.withDefaults(), log: log,
		tickets:     map[string]*ticket{},
		byPlayer:    map[string]*ticket{},
		regionsSeen: map[string]bool{},
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
	Project       string
	PlayerID      string
	ClientVersion string
	Regions       []RegionPing
}

// Submit validates and enqueues a ticket. Anti-dup (master.md §4): a new
// ticket for the same (project, player) cancels the previous queued one.
// A client version incompatible with every active server version is rejected
// as update_required right away (the ticket is still stored for GET).
func (mm *Matchmaker) Submit(ctx context.Context, p SubmitParams) (Ticket, error) {
	if p.PlayerID == "" {
		return Ticket{}, fmt.Errorf("%w: player_id is required", ErrInvalid)
	}
	bucket, err := MajorMinor(p.ClientVersion)
	if err != nil {
		return Ticket{}, fmt.Errorf("%w: client_version: %v", ErrInvalid, err)
	}
	regions, err := normalizeRegions(p.Regions)
	if err != nil {
		return Ticket{}, err
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
	if _, err := mm.st.GetProject(ctx, project); err != nil {
		return Ticket{}, err
	}

	// Version gate at submit time; re-checked every tick for queued tickets
	// (an active version swap must not strand incompatible clients).
	candidates, err := mm.st.ActiveRegionVersions(ctx, project)
	if err != nil {
		return Ticket{}, err
	}
	status := StatusQueued
	if active := candidateBuckets(candidates, mm.log); len(active) > 0 && !active[bucket] {
		status = StatusUpdateRequired
	}

	now := time.Now()
	t := &ticket{
		id: uuid.NewString(), project: project, playerID: p.PlayerID,
		clientVersion: p.ClientVersion, bucket: bucket, regions: regions,
		createdAt: now, status: status, done: make(chan struct{}),
	}

	mm.mu.Lock()
	defer mm.mu.Unlock()
	if prev, ok := mm.byPlayer[t.playerKey()]; ok {
		mm.finalize(prev, StatusCancelled, nil, now)
	}
	mm.tickets[t.id] = t
	if status == StatusQueued {
		mm.byPlayer[t.playerKey()] = t
	} else {
		// Terminal at birth (update_required).
		t.terminalAt = now
		close(t.done)
		mm.m.MMTickets.WithLabelValues(status).Inc()
	}
	return t.snapshot(), nil
}

// Get returns the current ticket snapshot.
func (mm *Matchmaker) Get(id string) (Ticket, bool) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	t, ok := mm.tickets[id]
	if !ok {
		return Ticket{}, false
	}
	return t.snapshot(), true
}

// Wait long-polls: it returns as soon as the ticket leaves queued, or after
// wait, or when ctx is cancelled — whichever comes first.
func (mm *Matchmaker) Wait(ctx context.Context, id string, wait time.Duration) (Ticket, bool) {
	mm.mu.Lock()
	t, ok := mm.tickets[id]
	if !ok {
		mm.mu.Unlock()
		return Ticket{}, false
	}
	if t.status != StatusQueued || wait <= 0 {
		snap := t.snapshot()
		mm.mu.Unlock()
		return snap, true
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
	return mm.Get(id)
}

// Cancel cancels a queued ticket (DELETE /v1/matchmaking/tickets/{id});
// terminal tickets are returned unchanged.
func (mm *Matchmaker) Cancel(id string) (Ticket, bool) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	t, ok := mm.tickets[id]
	if !ok {
		return Ticket{}, false
	}
	if t.status == StatusQueued {
		mm.finalize(t, StatusCancelled, nil, time.Now())
	}
	return t.snapshot(), true
}

// finalize moves a queued ticket to a terminal status. Caller holds mm.mu.
func (mm *Matchmaker) finalize(t *ticket, status string, match *Result, now time.Time) {
	if t.status != StatusQueued {
		return
	}
	t.status = status
	t.match = match
	t.terminalAt = now
	close(t.done)
	if cur, ok := mm.byPlayer[t.playerKey()]; ok && cur == t {
		delete(mm.byPlayer, t.playerKey())
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

// candidateBuckets maps candidate versions to their compat buckets (default
// MAJOR.MINOR rule, ops.md §3). Unparseable server semvers are skipped.
func candidateBuckets(candidates []store.RegionVersion, log *slog.Logger) map[string]bool {
	out := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		b, err := MajorMinor(c.Semver)
		if err != nil {
			log.Warn("mm: server version is not semver, skipped", "semver", c.Semver, "err", err)
			continue
		}
		out[b] = true
	}
	return out
}
