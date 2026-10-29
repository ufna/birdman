package matchmaker

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Member carries measurements for one player. A party's candidate regions
// are the intersection of these lists, scored by the slowest member.
type Member struct {
	PlayerID string       `json:"player_id"`
	Regions  []RegionPing `json:"regions"`
}

// QueuePolicy changes only the named project/env. A zero PartialWait keeps
// full-only matching; the target size remains the project's match_size.
type QueuePolicy struct {
	Project          string
	Env              string
	MinimumSize      int
	PartialWait      time.Duration
	MaxRTTms         int32
	ExpandedMaxRTTms int32
}

func ValidatePolicies(policies []QueuePolicy) error {
	seen := map[string]bool{}
	for _, p := range policies {
		key := scopeKey(p.Project, p.Env, "")
		if p.Project == "" || p.Env == "" || seen[key] || p.MinimumSize < 1 || p.PartialWait < 0 || p.MaxRTTms < 0 || p.ExpandedMaxRTTms < 0 || (p.ExpandedMaxRTTms > 0 && (p.MaxRTTms == 0 || p.ExpandedMaxRTTms < p.MaxRTTms)) {
			return fmt.Errorf("invalid or duplicate matchmaking policy for %q/%q", p.Project, p.Env)
		}
		seen[key] = true
	}
	return nil
}

func (mm *Matchmaker) policy(project, env string, size int) QueuePolicy {
	for _, p := range mm.cfg.Policies {
		if p.Project == project && p.Env == env {
			p.MinimumSize = min(size, max(1, p.MinimumSize))
			return p
		}
	}
	return QueuePolicy{MinimumSize: size}
}

func validPlayer(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 256 && !strings.ContainsRune(id, '\x00')
}

func normalizeParty(p SubmitParams) ([]Member, []RegionPing, error) {
	bad := func(msg string) ([]Member, []RegionPing, error) {
		return nil, nil, fmt.Errorf("%w: %s", ErrInvalid, msg)
	}
	if len(p.AllocationPayload) > 512 {
		return bad("allocation_payload exceeds 512 bytes")
	}
	if p.ClientRequestID != "" {
		id, err := uuid.Parse(p.ClientRequestID)
		if err != nil || id.String() != p.ClientRequestID {
			return bad("client_request_id must be a canonical UUID")
		}
	}
	if p.Members == nil {
		if p.PlayerID == "" {
			return bad("player_id is required")
		}
		if p.PartyID != "" || p.PartyRevision != 0 {
			return bad("party fields require members")
		}
		r, err := normalizeRegions(p.Regions)
		return nil, r, err
	}
	if p.PlayerID != "" || len(p.Regions) > 0 {
		return bad("members cannot be mixed with player_id or regions")
	}
	if len(p.Members) == 0 || len(p.Members) > 64 {
		return bad("members must contain 1 to 64 players, bounded by project match_size")
	}
	id, err := uuid.Parse(p.PartyID)
	if err != nil || id.String() != p.PartyID || p.PartyRevision < 1 || p.ClientRequestID == "" {
		return bad("members require a canonical party_id UUID, positive party_revision and client_request_id")
	}
	members := cloneMembers(p.Members)
	seen := map[string]bool{}
	worst, counts := map[string]int32{}, map[string]int{}
	for i := range members {
		m := &members[i]
		if !validPlayer(m.PlayerID) || seen[m.PlayerID] {
			return bad("members require unique nonempty player_id values (max 256 bytes, no NUL)")
		}
		seen[m.PlayerID] = true
		m.Regions, err = normalizeRegions(m.Regions)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range m.Regions {
			counts[r.Region]++
			worst[r.Region] = max(worst[r.Region], r.RTTms)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PlayerID < members[j].PlayerID })
	common := []RegionPing{}
	for r, count := range counts {
		if count == len(members) {
			common = append(common, RegionPing{Region: r, RTTms: worst[r]})
		}
	}
	if len(common) == 0 {
		return bad("members have no common measured region")
	}
	common, err = normalizeRegions(common)
	return members, common, err
}

func cloneMembers(in []Member) []Member {
	if in == nil {
		return nil
	}
	out := append([]Member(nil), in...)
	for i := range out {
		out[i].Regions = append([]RegionPing(nil), in[i].Regions...)
	}
	return out
}

func requestFingerprint(p SubmitParams) [32]byte {
	// SubmitParams contains no maps; member and region order is normalized.
	b, _ := json.Marshal(p)
	return sha256.Sum256(b)
}

func (t qt) seats() int { return max(1, t.players) }

// pack anchors each attempt at the oldest eligible party. The bounded subset
// sum avoids greedy traps such as 2,3,1: the three-person party can pair with
// the solo instead of starving behind a two-person party that cannot fit.
func pack(eligible []qt, size int, policy QueuePolicy, now time.Time) []qt {
	sort.Slice(eligible, func(i, j int) bool {
		if !eligible[i].created.Equal(eligible[j].created) {
			return eligible[i].created.Before(eligible[j].created)
		}
		return eligible[i].id < eligible[j].id
	})
	var partial []qt
	partialSeats := 0
	for anchor, first := range eligible {
		if first.seats() > size {
			continue
		}
		// Reachable sums are sparse so a large legacy project match_size
		// cannot cause an unbounded array allocation.
		reach := map[int][]qt{first.seats(): {first}}
		for _, next := range eligible[anchor+1:] {
			updates := map[int][]qt{}
			for n, g := range reach {
				total := n + next.seats()
				if total <= size && reach[total] == nil {
					updates[total] = append(append([]qt(nil), g...), next)
				}
			}
			for n, g := range updates {
				reach[n] = g
			}
			if reach[size] != nil {
				return reach[size]
			}
		}
		if reach[size] != nil {
			return reach[size]
		}
		if anchor == 0 && policy.PartialWait > 0 && now.Sub(first.created) >= policy.PartialWait {
			for n, g := range reach {
				if n >= policy.MinimumSize && n > partialSeats {
					partialSeats, partial = n, g
				}
			}
		}
	}
	return partial
}

func groupSeats(group []qt) int {
	n := 0
	for _, t := range group {
		n += t.seats()
	}
	return n
}

func worstRTT(group []qt, region string) float64 {
	var worst int32
	for _, t := range group {
		for _, r := range t.regions {
			if r.Region == region {
				worst = max(worst, r.RTTms)
			}
		}
	}
	return float64(worst)
}
