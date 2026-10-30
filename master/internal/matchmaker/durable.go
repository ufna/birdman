package matchmaker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

type persistedTicket struct {
	ID, Project, Env, PlayerID, ClientVersion, Bucket string
	Regions                                           []RegionPing
	CreatedAt                                         time.Time
	Members                                           []Member
	PartyID                                           string
	PartyRevision                                     int64
	RequestID                                         string
	Fingerprint                                       [32]byte
	Payload                                           string
	Status                                            string
	Match                                             *Result
	TerminalAt                                        time.Time
	GameMode                                          string `json:",omitempty"`
}

func record(t *ticket) (store.TicketRecord, error) {
	p := persistedTicket{t.id, t.project, t.env, t.playerID, t.clientVersion, t.bucket, t.regions, t.createdAt, t.members, t.partyID, t.partyRevision, t.requestID, t.fingerprint, t.payload, t.status, t.match, t.terminalAt, t.gameMode}
	b, err := json.Marshal(p)
	return store.TicketRecord{ID: t.id, Project: t.project, Env: t.env, RequestID: t.requestID, PartyID: t.partyID, PartyRevision: t.partyRevision, Status: t.status, CreatedAt: t.createdAt, Body: b}, err
}

func (mm *Matchmaker) merge(records []store.TicketRecord) error {
	for _, r := range records {
		var p persistedTicket
		if err := json.Unmarshal(r.Body, &p); err != nil || p.ID != r.ID || p.Status != r.Status || p.Project != r.Project || p.Env != r.Env {
			return errors.New("invalid persisted matchmaking ticket")
		}
		prev := mm.tickets[p.ID]
		done := make(chan struct{})
		if prev != nil {
			done = prev.done
			if prev.status == StatusQueued && p.Status != StatusQueued {
				close(done)
			}
		} else if p.Status != StatusQueued {
			close(done)
		}
		mm.tickets[p.ID] = &ticket{id: p.ID, project: p.Project, env: p.Env, playerID: p.PlayerID, clientVersion: p.ClientVersion, gameMode: p.GameMode, bucket: p.Bucket, regions: p.Regions, createdAt: p.CreatedAt, members: p.Members, partyID: p.PartyID, partyRevision: p.PartyRevision, requestID: p.RequestID, fingerprint: p.Fingerprint, payload: p.Payload, status: p.Status, match: p.Match, terminalAt: p.TerminalAt, done: done}
	}
	mm.byPlayer = map[string]*ticket{}
	mm.byParty = map[string]*ticket{}
	mm.byRequest = map[string]*ticket{}
	for _, t := range mm.tickets {
		if t.status == StatusQueued {
			for _, key := range t.playerKeys() {
				mm.byPlayer[key] = t
			}
		}
		if t.requestID != "" {
			mm.byRequest[scopeKey(t.project, t.env, t.requestID)] = t
		}
		if t.partyID != "" {
			key := scopeKey(t.project, t.env, t.partyID)
			if prev := mm.byParty[key]; prev == nil || t.partyRevision > prev.partyRevision {
				mm.byParty[key] = t
			}
		}
	}
	return nil
}

// beginLocked refreshes queued state while holding both the process mutex and
// the database transaction fence. SQL remains authoritative after any error.
func (mm *Matchmaker) beginLocked(ctx context.Context) (*store.MatchmakingTx, error) {
	tx, err := mm.st.BeginMatchmaking(ctx)
	if err != nil {
		return nil, err
	}
	records, err := tx.Load(ctx, mm.retention())
	if err == nil {
		present := map[string]bool{}
		for _, r := range records {
			present[r.ID] = true
		}
		for id, t := range mm.tickets {
			if t.status == StatusQueued && !present[id] {
				close(t.done)
				delete(mm.tickets, id)
			}
		}
		err = mm.merge(records)
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	mm.dirty = map[string]bool{}
	return tx, nil
}

func (mm *Matchmaker) commitLocked(ctx context.Context, tx *store.MatchmakingTx) error {
	var records []store.TicketRecord
	for id := range mm.dirty {
		r, err := record(mm.tickets[id])
		if err != nil {
			return err
		}
		records = append(records, r)
	}
	if err := tx.Save(ctx, records); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	mm.dirty = nil
	return nil
}

func (mm *Matchmaker) finishLocked(tx *store.MatchmakingTx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx.Rollback(ctx)
	if len(mm.dirty) > 0 {
		// Commit may have succeeded despite a lost response. Invalidate the
		// cache so the next retry reads the canonical ticket/assignment.
		for _, t := range mm.tickets {
			if t.status == StatusQueued {
				select {
				case <-t.done:
				default:
					close(t.done)
				}
			}
		}
		mm.tickets = map[string]*ticket{}
		mm.byPlayer = map[string]*ticket{}
		mm.byParty = map[string]*ticket{}
		mm.byRequest = map[string]*ticket{}
		mm.dirty = nil
	}
}

func (mm *Matchmaker) Restore(ctx context.Context) error {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	tx, err := mm.beginLocked(ctx)
	if err != nil {
		return err
	}
	defer mm.finishLocked(tx)
	return nil
}

func (mm *Matchmaker) GetContext(ctx context.Context, id string) (Ticket, bool, error) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	tx, err := mm.beginLocked(ctx)
	if err != nil {
		return Ticket{}, false, err
	}
	defer mm.finishLocked(tx)
	if mm.tickets[id] == nil {
		records, err := tx.Find(ctx, id)
		if err != nil {
			return Ticket{}, false, err
		}
		if err = mm.merge(records); err != nil {
			return Ticket{}, false, err
		}
	}
	t := mm.tickets[id]
	if t == nil {
		return Ticket{}, false, nil
	}
	return t.snapshot(), true, nil
}

func (mm *Matchmaker) CancelContext(ctx context.Context, id string) (Ticket, bool, error) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	tx, err := mm.beginLocked(ctx)
	if err != nil {
		return Ticket{}, false, err
	}
	defer mm.finishLocked(tx)
	if mm.tickets[id] == nil {
		records, err := tx.Find(ctx, id)
		if err != nil {
			return Ticket{}, false, err
		}
		if err = mm.merge(records); err != nil {
			return Ticket{}, false, err
		}
	}
	t := mm.tickets[id]
	if t == nil {
		return Ticket{}, false, nil
	}
	if t.status == StatusQueued {
		mm.finalize(t, StatusCancelled, nil, time.Now())
	}
	if err = mm.commitLocked(ctx, tx); err != nil {
		return Ticket{}, false, err
	}
	return t.snapshot(), true, nil
}
