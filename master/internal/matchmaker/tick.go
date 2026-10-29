package matchmaker

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// qt is a queued-ticket snapshot used for unlocked candidate selection.
// Server claim and assignment publication revalidate it under the queue lock.
type qt struct {
	id      string
	player  string
	bucket  string
	version string // client_version (compat checks against candidates)
	created time.Time
	regions []RegionPing
	players int
	payload string
	party   bool
}

// projectEnv keys the per-tick match pass: tickets are grouped and candidates
// resolved per (project, env) — a dev and a prod ticket of the same project have
// different candidate sets and allocate onto different fleets (environments
// v1 §3).
type projectEnv struct{ project, env string }

// regionEnv keys the birdman_mm_queue_depth gauge: depth is reported per
// (region, env) so dev and prod tickets resting in one region stay distinct
// series (environments v1 §7).
type regionEnv struct{ region, env string }

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
	depth := map[regionEnv]int{}
	for id, t := range mm.tickets {
		switch {
		case t.status == StatusQueued && now.Sub(t.createdAt) > mm.cfg.TicketTTL:
			mm.finalize(t, StatusExpired, nil, now)
		case t.status == StatusQueued:
			pe := projectEnv{t.project, t.env}
			queued[pe] = append(queued[pe], qt{
				id: t.id, player: t.playerID, bucket: t.bucket,
				version: t.clientVersion, created: t.createdAt, regions: t.regions,
				players: max(1, len(t.members)), payload: t.payload, party: len(t.members) > 0,
			})
			depth[regionEnv{t.regions[0].Region, t.env}]++ // depth by the player's best (region, env)
		case now.Sub(t.terminalAt) > mm.retention():
			if key := scopeKey(t.project, t.env, t.partyID); mm.byParty[key] == t {
				delete(mm.byParty, key)
			}
			if t.requestID != "" {
				delete(mm.byRequest, scopeKey(t.project, t.env, t.requestID))
			}
			delete(mm.tickets, id)
		}
	}
	for re := range mm.regionsSeen {
		if _, ok := depth[re]; !ok {
			mm.m.MMQueueDepth.WithLabelValues(re.region, re.env).Set(0)
		}
	}
	for re, n := range depth {
		mm.regionsSeen[re] = true
		mm.m.MMQueueDepth.WithLabelValues(re.region, re.env).Set(float64(n))
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
		mm.matchBucket(ctx, project, env, size, list, verFor, now)
	}
	return nil
}

// matchBucket forms and allocates matches inside one (project, env,
// compat-bucket). Allocation is scoped to env (environments v1 §3): candidates
// are already env-scoped, and Allocate claims a server of this env only.
func (mm *Matchmaker) matchBucket(ctx context.Context, project, env string, size int,
	list []qt, verFor map[string]store.RegionVersion, now time.Time) {

	pool := make(map[string]qt, len(list))
	for _, t := range list {
		pool[t.id] = t
	}
	exhausted := map[string]bool{} // regions that answered no_capacity this tick

	for {
		region, group := mm.bestGroup(pool, size, verFor, exhausted, now, mm.policy(project, env, size))
		if region == "" {
			return
		}
		ver := verFor[region]
		matchID := uuid.NewString()
		// Keep cancel/revision and concurrent ticks on one side of the claim.
		// The store call is bounded by the tick context. Durable assignment and
		// restart recovery are a separate required step before deployment.
		mm.mu.Lock()
		valid := true
		metadata := map[string]string{}
		for _, g := range group {
			if t := mm.tickets[g.id]; t == nil || t.status != StatusQueued {
				delete(pool, g.id)
				valid = false
			}
			if g.payload != "" {
				metadata["ticket_"+g.id] = g.payload
			}
		}
		if !valid {
			mm.mu.Unlock()
			continue
		}
		if len(metadata) > 16 {
			mm.mu.Unlock()
			mm.log.Error("mm: allocation exceeds metadata ticket limit", "project", project)
			return
		}
		alloc, err := mm.st.Allocate(ctx, project, env, region, &ver.VersionID, matchID, int32(groupSeats(group)), metadata)
		if errors.Is(err, store.ErrNoCapacity) {
			mm.mu.Unlock()
			// Tickets stay queued; the warm pool catches up via reconcile and
			// the next tick retries. Feeds the BufferEmpty alert (ops.md §1).
			mm.m.AllocFailures.WithLabelValues("no_capacity", project).Inc()
			exhausted[region] = true
			continue
		}
		if err != nil {
			mm.mu.Unlock()
			mm.log.Error("mm: allocate failed", "project", project, "region", region, "err", err)
			return
		}
		if err := mm.st.RecordMatch(ctx, matchID, project, region, alloc.ServerID, ver.VersionID, env); err != nil {
			mm.log.Error("mm: record match failed", "match_id", matchID, "err", err) // non-fatal
		}
		mm.deliverLocked(group, matchID, ver.VersionID, alloc)
		mm.mu.Unlock()
		for _, t := range group {
			delete(pool, t.id)
		}
		mm.log.Info("mm: match formed", "project", project, "region", region,
			"match_id", matchID, "server_id", alloc.ServerID, "version", ver.Semver, "players", groupSeats(group))
	}
}

// bestGroup preserves FIFO between fitting parties, then minimizes their
// worst RTT. Legacy-only queues retain median RTT scoring.
func (mm *Matchmaker) bestGroup(pool map[string]qt, size int,
	verFor map[string]store.RegionVersion, exhausted map[string]bool, now time.Time, policy QueuePolicy) (string, []qt) {

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
	parties := false
	for _, t := range pool {
		parties = parties || t.party
	}
	for _, r := range regions {
		var elig []qt
		for _, t := range pool {
			limit := policy.MaxRTTms
			if now.Sub(t.created) >= mm.cfg.WidenAfter && policy.ExpandedMaxRTTms > 0 {
				limit = policy.ExpandedMaxRTTms
			}
			if eligibleFor(t, r, mm.cfg.WidenAfter, now) && (limit == 0 || worstRTT([]qt{t}, r) <= float64(limit)) {
				elig = append(elig, t)
			}
		}
		group := pack(elig, size, policy, now)
		if len(group) == 0 {
			continue
		}
		med := medianRTT(group, r)
		if parties {
			med = worstRTT(group, r)
		}
		// Prefer full matches; preserve legacy median scoring for old clients.
		better := bestRegion == "" || groupSeats(group) > groupSeats(bestGroup)
		if !better && groupSeats(group) == groupSeats(bestGroup) {
			if parties && !group[0].created.Equal(bestGroup[0].created) {
				better = group[0].created.Before(bestGroup[0].created)
			} else {
				better = med < bestMedian
			}
		}
		if better {
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

// deliverLocked publishes the assignment while still holding the claim fence.
func (mm *Matchmaker) deliverLocked(group []qt, matchID, versionID string, alloc store.Allocation) {
	now := time.Now()
	refs := make([]string, 0, len(group))
	for _, g := range group {
		refs = append(refs, g.id)
	}
	sort.Strings(refs)
	for _, g := range group {
		t, ok := mm.tickets[g.id]
		if !ok || t.status != StatusQueued {
			mm.log.Warn("mm: group member left during allocation",
				"ticket_id", g.id, "match_id", matchID)
			continue
		}
		res := &Result{Host: alloc.Host, Port: alloc.Port, MatchID: matchID, ServerID: alloc.ServerID, VersionID: versionID, TicketIDs: refs}
		if mm.cfg.JoinTokenEnabled && len(t.members) == 0 {
			res.JoinToken = GenerateJoinToken(mm.cfg.JoinTokenSecret,
				matchID, t.playerID, now.Add(mm.cfg.JoinTokenTTL))
		}
		mm.finalize(t, StatusMatched, res, now)
	}
}
