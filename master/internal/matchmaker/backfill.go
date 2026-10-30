package matchmaker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

func (mm *Matchmaker) backfill(ctx context.Context, project, env, mode string, pool map[string]qt, verFor map[string]store.RegionVersion, now time.Time, policy QueuePolicy) error {
	// An existing battle need not wait for another complete group. Whole
	// tickets, measured RTT, region widening and the build gate still apply.
	policy.MinimumSize, policy.PartialWait = 1, time.Nanosecond
	for len(pool) > 0 {
		mm.mu.Lock()
		tx, err := mm.beginLocked(ctx)
		if err != nil {
			mm.mu.Unlock()
			return err
		}
		for id := range pool {
			if t := mm.tickets[id]; t == nil || t.status != StatusQueued {
				delete(pool, id)
			}
		}
		matches, err := tx.BackfillMatches(ctx, project, env, mode)
		var assigned []qt
		if err == nil {
			for _, m := range matches {
				ver, ok := verFor[m.Region]
				if !ok || ver.VersionID != m.VersionID {
					continue
				}
				occupied := map[string]bool{}
				for _, key := range m.PlayerKeys {
					occupied[key] = true
				}
				eligible := map[string]qt{}
				for id, ticket := range pool {
					overlap := false
					for _, key := range mm.groupPlayerKeys([]qt{ticket}) {
						overlap = overlap || occupied[key]
					}
					if !overlap {
						eligible[id] = ticket
					}
				}
				_, group := mm.bestGroup(eligible, m.Free, map[string]store.RegionVersion{m.Region: ver}, nil, now, policy)
				if len(group) == 0 {
					continue
				}
				metadata := map[string]string{}
				if mode != "" {
					metadata["game_mode"] = mode
				}
				for _, g := range group {
					if g.payload != "" {
						metadata["ticket_"+g.id] = g.payload
					}
				}
				var allocation store.Allocation
				allocation, err = tx.Backfill(ctx, m.ID, int32(groupSeats(group)), metadata, mm.groupPlayerKeys(group))
				if errors.Is(err, store.ErrNoCapacity) {
					err = nil
					continue
				}
				if err != nil {
					break
				}
				mm.deliverLocked(group, m.ID, m.VersionID, allocation)
				err = mm.commitLocked(ctx, tx)
				if err == nil {
					assigned = group
				}
				break
			}
		}
		mm.finishLocked(tx)
		mm.mu.Unlock()
		if err != nil {
			return err
		}
		if len(assigned) == 0 {
			return nil
		}
		for _, g := range assigned {
			delete(pool, g.id)
		}
		if err = mm.st.DispatchAllocations(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Must hold mm.mu, after refreshing the SQL ticket state.
func (mm *Matchmaker) groupPlayerKeys(group []qt) []string {
	var out []string
	for _, g := range group {
		for _, key := range mm.tickets[g.id].playerKeys() {
			hash := sha256.Sum256([]byte(key))
			out = append(out, hex.EncodeToString(hash[:]))
		}
	}
	return out
}
