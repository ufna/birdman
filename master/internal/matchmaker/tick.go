package matchmaker

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// qt is a queued-ticket snapshot used while matching, so no lock is held
// across store calls (Submit/Cancel stay responsive during a tick).
type qt struct {
	id      string
	player  string
	bucket  string
	version string // client_version (compat checks against candidates)
	created time.Time
	regions []RegionPing
}

// projectEnv keys the per-tick match pass: tickets are grouped and candidates
// resolved per (project, env) — a dev and a prod ticket of the same project have
// different candidate sets and allocate onto different fleets (environments
// v1 §3).
type projectEnv struct{ project, env string }

// RunOnce performs a single matchmaking pass (master.md §4):
//
//	expire TTL → per project: drop update_required tickets →
//	per (region, compat-bucket): groups of match_size, region with the
//	lowest median group rtt first → store.Allocate → deliver matched;
//	no_capacity keeps tickets queued for the next tick (reconcile refills
//	the buffer meanwhile).
func (mm *Matchmaker) RunOnce(ctx context.Context) error {
	now := time.Now()

	// Pass 1 (locked): TTL, janitor, snapshot of queued tickets per (project, env).
	mm.mu.Lock()
	queued := map[projectEnv][]qt{}
	depth := map[string]int{}
	for id, t := range mm.tickets {
		switch {
		case t.status == StatusQueued && now.Sub(t.createdAt) > mm.cfg.TicketTTL:
			mm.finalize(t, StatusExpired, nil, now)
		case t.status == StatusQueued:
			pe := projectEnv{t.project, t.env}
			queued[pe] = append(queued[pe], qt{
				id: t.id, player: t.playerID, bucket: t.bucket,
				version: t.clientVersion, created: t.createdAt, regions: t.regions,
			})
			depth[t.regions[0].Region]++ // depth by the player's best region
		case now.Sub(t.terminalAt) > mm.retention():
			delete(mm.tickets, id)
		}
	}
	for r := range mm.regionsSeen {
		if _, ok := depth[r]; !ok {
			mm.m.MMQueueDepth.WithLabelValues(r).Set(0)
		}
	}
	for r, n := range depth {
		mm.regionsSeen[r] = true
		mm.m.MMQueueDepth.WithLabelValues(r).Set(float64(n))
	}
	mm.mu.Unlock()

	// Pass 2 (unlocked): match per (project, env).
	var errs []error
	for pe, tks := range queued {
		if err := mm.matchProject(ctx, pe.project, pe.env, tks, now); err != nil {
			mm.log.Error("mm: project pass failed", "project", pe.project, "env", pe.env, "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (mm *Matchmaker) matchProject(ctx context.Context, project, env string, tks []qt, now time.Time) error {
	proj, err := mm.st.GetProject(ctx, project)
	if err != nil {
		return err
	}
	size := int(proj.MatchSize)
	if size < 1 {
		size = 1
	}
	candidates, err := mm.st.ActiveRegionVersions(ctx, project, env)
	if err != nil {
		return err
	}

	// A queued ticket compatible with NO live candidate → update_required
	// (active version changed under it / the window closed, ops.md §3). With
	// no candidates at all the fleet is not set up yet — keep waiting (TTL).
	// Compatibility is identical inside a bucket (BucketOf), so it is
	// resolved once per bucket.
	byBucket := map[string][]qt{}
	var stale []string
	for _, t := range tks {
		if len(candidates) > 0 && !mm.compatibleWithAny(t.version, candidates) {
			stale = append(stale, t.id)
			continue
		}
		byBucket[t.bucket] = append(byBucket[t.bucket], t)
	}
	if len(stale) > 0 {
		mm.mu.Lock()
		for _, id := range stale {
			if t, ok := mm.tickets[id]; ok {
				mm.finalize(t, StatusUpdateRequired, nil, now)
			}
		}
		mm.mu.Unlock()
	}

	for _, list := range byBucket {
		// region → preferred version FOR THIS BUCKET: candidates come
		// preference-ordered (fleet-active, active, deprecated window; newest
		// first within a rank) — the first compatible hit wins, so a client
		// covered by an active version never lands on a deprecated one.
		verFor := map[string]store.RegionVersion{}
		for _, c := range candidates {
			if _, ok := verFor[c.Region]; ok {
				continue
			}
			if mm.cfg.Compat.Compatible(list[0].version, c.Semver) {
				verFor[c.Region] = c
			}
		}
		mm.matchBucket(ctx, project, size, list, verFor, now)
	}
	return nil
}

// matchBucket forms and allocates matches inside one (project, compat-bucket).
func (mm *Matchmaker) matchBucket(ctx context.Context, project string, size int,
	list []qt, verFor map[string]store.RegionVersion, now time.Time) {

	pool := make(map[string]qt, len(list))
	for _, t := range list {
		pool[t.id] = t
	}
	exhausted := map[string]bool{} // regions that answered no_capacity this tick

	for {
		region, group := mm.bestGroup(pool, size, verFor, exhausted, now)
		if region == "" {
			return
		}
		ver := verFor[region]
		matchID := uuid.NewString()
		alloc, err := mm.st.Allocate(ctx, project, region, &ver.VersionID, matchID, int32(len(group)))
		if errors.Is(err, store.ErrNoCapacity) {
			// Tickets stay queued; the warm pool catches up via reconcile and
			// the next tick retries. Feeds the BufferEmpty alert (ops.md §1).
			mm.m.AllocFailures.WithLabelValues("no_capacity").Inc()
			exhausted[region] = true
			continue
		}
		if err != nil {
			mm.log.Error("mm: allocate failed", "project", project, "region", region, "err", err)
			return
		}
		if err := mm.st.RecordMatch(ctx, matchID, project, region, alloc.ServerID, ver.VersionID); err != nil {
			mm.log.Error("mm: record match failed", "match_id", matchID, "err", err) // non-fatal
		}
		mm.deliver(group, matchID, alloc)
		for _, t := range group {
			delete(pool, t.id)
		}
		mm.log.Info("mm: match formed", "project", project, "region", region,
			"match_id", matchID, "server_id", alloc.ServerID, "version", ver.Semver, "players", len(group))
	}
}

// bestGroup picks the region with the lowest median group rtt that can field
// a full group of the oldest eligible tickets (master.md §4).
func (mm *Matchmaker) bestGroup(pool map[string]qt, size int,
	verFor map[string]store.RegionVersion, exhausted map[string]bool, now time.Time) (string, []qt) {

	// Deterministic region order for stable tie-breaks.
	regions := make([]string, 0, len(verFor))
	for r := range verFor {
		if !exhausted[r] {
			regions = append(regions, r)
		}
	}
	sort.Strings(regions)

	bestRegion := ""
	var bestGroup []qt
	var bestMedian float64
	for _, r := range regions {
		var elig []qt
		for _, t := range pool {
			if eligibleFor(t, r, mm.cfg.WidenAfter, now) {
				elig = append(elig, t)
			}
		}
		if len(elig) < size {
			continue
		}
		sort.Slice(elig, func(i, j int) bool { // FIFO fairness
			if !elig[i].created.Equal(elig[j].created) {
				return elig[i].created.Before(elig[j].created)
			}
			return elig[i].id < elig[j].id
		})
		group := elig[:size]
		med := medianRTT(group, r)
		if bestRegion == "" || med < bestMedian {
			bestRegion, bestGroup, bestMedian = r, group, med
		}
	}
	return bestRegion, bestGroup
}

// eligibleFor: a ticket starts in its lowest-rtt region and widens to one
// more region per WidenAfter elapsed (master.md §4).
func eligibleFor(t qt, region string, widen time.Duration, now time.Time) bool {
	k := 1 + int(now.Sub(t.created)/widen)
	for i, r := range t.regions {
		if i >= k {
			return false
		}
		if r.Region == region {
			return true
		}
	}
	return false
}

func medianRTT(group []qt, region string) float64 {
	rtts := make([]int32, 0, len(group))
	for _, t := range group {
		for _, r := range t.regions {
			if r.Region == region {
				rtts = append(rtts, r.RTTms)
				break
			}
		}
	}
	if len(rtts) == 0 {
		return 0
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	mid := len(rtts) / 2
	if len(rtts)%2 == 1 {
		return float64(rtts[mid])
	}
	return float64(rtts[mid-1]+rtts[mid]) / 2
}

// deliver flips still-queued group members to matched. A member cancelled
// while the allocation was in flight is skipped (the match starts short —
// accepted v0 race, the game session handles absentees).
func (mm *Matchmaker) deliver(group []qt, matchID string, alloc store.Allocation) {
	now := time.Now()
	mm.mu.Lock()
	defer mm.mu.Unlock()
	for _, g := range group {
		t, ok := mm.tickets[g.id]
		if !ok || t.status != StatusQueued {
			mm.log.Warn("mm: group member left during allocation",
				"ticket_id", g.id, "match_id", matchID)
			continue
		}
		res := &Result{Host: alloc.Host, Port: alloc.Port, MatchID: matchID}
		if mm.cfg.JoinTokenEnabled {
			res.JoinToken = GenerateJoinToken(mm.cfg.JoinTokenSecret,
				matchID, t.playerID, now.Add(mm.cfg.JoinTokenTTL))
		}
		mm.finalize(t, StatusMatched, res, now)
	}
}
