package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AlertMute is one row of alert_mutes — a master-level mute/suppression for
// alerts (docs/specs/master.md §6). alert_mutes is the source of truth: master
// tags matching alerts muted:true in /v1/alerts/{active,history} so the panel
// can dim/hide them and an audit trail exists, AND mirrors the mute into a real
// alertmanager silence best-effort (internal/amsilence; ops.md §1, tracker
// #245) — real suppression of the log sink/Discord. Without a reachable
// alertmanager the mirror is a silent no-op and the mute degrades to pure v0
// annotation semantics. Region and ExpiresAt are nullable: a nil Region matches
// every region, a nil ExpiresAt never expires. SilenceID holds the mirrored
// silence's id (nil = not yet mirrored — a pre-upgrade v0 mute or AM was down;
// the reconcile sweep fills it in). All three serialize as JSON null (not
// omitted) so the panel always sees the fields.
type AlertMute struct {
	ID        string     `json:"id"`
	Alertname string     `json:"alertname"`
	Region    *string    `json:"region"`
	Note      string     `json:"note"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedBy string     `json:"created_by"`
	SilenceID *string    `json:"silence_id"`
}

// Matches reports whether this mute covers an alert with the given alertname
// and region. A nil Region ("all regions") matches any region; otherwise the
// region must be exactly equal. Callers pre-filter to active mutes.
func (m AlertMute) Matches(alertname, region string) bool {
	if m.Alertname != alertname {
		return false
	}
	return m.Region == nil || *m.Region == region
}

const alertMuteCols = `id::text, alertname, region, note, created_at, expires_at, created_by, silence_id`

// CreateAlertMuteParams is the input to UpsertAlertMute. A nil Region means
// "all regions", a nil ExpiresAt "never expires"; Note/CreatedBy default to "".
type CreateAlertMuteParams struct {
	Alertname string
	Region    *string
	Note      string
	ExpiresAt *time.Time
	CreatedBy string
}

// UpsertAlertMute creates a mute or — when an ACTIVE mute for the same
// (alertname, region) already exists — updates that row's note/expires_at in
// place. This makes POST /v1/alerts/mutes an idempotent upsert: re-muting the
// same target doubles as "extend/edit" instead of piling up duplicates or 409.
// Region is matched null-aware (`is not distinct from`), so a NULL-region ("all
// regions") mute is a distinct target from a specific-region one. The
// lookup+write share one transaction with FOR UPDATE so concurrent re-mutes of
// the same target converge on one row.
func (s *Store) UpsertAlertMute(ctx context.Context, p CreateAlertMuteParams) (AlertMute, error) {
	p.Alertname = strings.TrimSpace(p.Alertname)
	if p.Alertname == "" {
		return AlertMute{}, errors.New("alertname is required")
	}
	p.Region = normalizeRegion(p.Region)

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AlertMute{}, err
	}
	defer tx.Rollback(ctx)

	var m AlertMute
	err = tx.QueryRow(ctx, `
		select `+alertMuteCols+`
		from alert_mutes
		where alertname = $1 and region is not distinct from $2
		  and (expires_at is null or expires_at > now())
		order by created_at desc
		limit 1
		for update`, p.Alertname, p.Region).
		Scan(&m.ID, &m.Alertname, &m.Region, &m.Note, &m.CreatedAt, &m.ExpiresAt, &m.CreatedBy, &m.SilenceID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `
			insert into alert_mutes (alertname, region, note, expires_at, created_by)
			values ($1, $2, $3, $4, $5)
			returning `+alertMuteCols,
			p.Alertname, p.Region, p.Note, p.ExpiresAt, p.CreatedBy).
			Scan(&m.ID, &m.Alertname, &m.Region, &m.Note, &m.CreatedAt, &m.ExpiresAt, &m.CreatedBy, &m.SilenceID)
	case err != nil:
		return AlertMute{}, err
	default:
		// Re-mute of the same target updates only note/expires_at — silence_id
		// is deliberately left untouched so the mirrored silence survives an
		// "extend/edit"; the mirror call then updates that same silence in place.
		err = tx.QueryRow(ctx, `
			update alert_mutes set note = $2, expires_at = $3
			where id = $1::uuid
			returning `+alertMuteCols,
			m.ID, p.Note, p.ExpiresAt).
			Scan(&m.ID, &m.Alertname, &m.Region, &m.Note, &m.CreatedAt, &m.ExpiresAt, &m.CreatedBy, &m.SilenceID)
	}
	if err != nil {
		return AlertMute{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AlertMute{}, err
	}
	return m, nil
}

// ListAlertMutes returns mutes newest-first. Active-only by default (expires_at
// null or in the future); includeExpired also returns the lapsed ones (the
// ?all=1 read). Never returns nil, so the JSON is always [] not null.
func (s *Store) ListAlertMutes(ctx context.Context, includeExpired bool) ([]AlertMute, error) {
	q := `select ` + alertMuteCols + ` from alert_mutes`
	if !includeExpired {
		q += ` where expires_at is null or expires_at > now()`
	}
	q += ` order by created_at desc, id`
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertMute{}
	for rows.Next() {
		var m AlertMute
		if err := rows.Scan(&m.ID, &m.Alertname, &m.Region, &m.Note, &m.CreatedAt, &m.ExpiresAt, &m.CreatedBy, &m.SilenceID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteAlertMute removes a mute by id. It reports whether a row was actually
// deleted (false → no such id) and returns the deleted row so the caller can
// write the audit event. DELETE is idempotent in effect (end state: not muted);
// the bool lets the handler answer 404 for an unknown/already-removed id and
// emit alert_unmuted only on a real removal. id must be a valid uuid (validated
// by the caller) or the query errors.
func (s *Store) DeleteAlertMute(ctx context.Context, id string) (AlertMute, bool, error) {
	var m AlertMute
	err := s.Pool.QueryRow(ctx, `
		delete from alert_mutes where id = $1::uuid
		returning `+alertMuteCols, id).
		Scan(&m.ID, &m.Alertname, &m.Region, &m.Note, &m.CreatedAt, &m.ExpiresAt, &m.CreatedBy, &m.SilenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertMute{}, false, nil
	}
	if err != nil {
		return AlertMute{}, false, err
	}
	return m, true, nil
}

// SetAlertMuteSilenceID stamps the mirrored alertmanager silence id on a
// mute (nil clears it). A missing row is a no-op, not an error: the mute may
// have been deleted while the mirror call was in flight — the reconcile
// sweep handles the stray silence.
func (s *Store) SetAlertMuteSilenceID(ctx context.Context, id string, silenceID *string) error {
	_, err := s.Pool.Exec(ctx,
		`update alert_mutes set silence_id = $2 where id = $1::uuid`, id, silenceID)
	return err
}

// normalizeRegion collapses an empty/whitespace region to nil ("all regions"),
// so `"region":""` and an absent region behave identically.
func normalizeRegion(r *string) *string {
	if r == nil {
		return nil
	}
	v := strings.TrimSpace(*r)
	if v == "" {
		return nil
	}
	return &v
}
