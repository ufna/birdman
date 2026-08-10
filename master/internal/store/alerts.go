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
// annotation semantics. Region, Project and ExpiresAt are nullable: a nil
// Region matches every region, a nil Project every project (tracker #957), a
// nil ExpiresAt never expires. SilenceID holds the mirrored silence's id (nil =
// not yet mirrored — a pre-upgrade v0 mute or AM was down; the reconcile sweep
// fills it in). All four serialize as JSON null (not omitted) so the panel
// always sees the fields.
type AlertMute struct {
	ID        string     `json:"id"`
	Alertname string     `json:"alertname"`
	Region    *string    `json:"region"`
	Project   *string    `json:"project"`
	Note      string     `json:"note"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedBy string     `json:"created_by"`
	SilenceID *string    `json:"silence_id"`
}

// Matches reports whether this mute covers an alert with the given alertname,
// region and project. A nil Region ("all regions") matches any region, a nil
// Project ("all projects") any project; otherwise the value must be exactly
// equal. Callers pre-filter to active mutes.
//
// The project comparison is STRICT, deliberately the opposite of the ?project=
// screen filter (httpapi.keepAlertForProject), which lets an alert with no
// project through under every selection. Copying that idiom here — "an empty
// alert project passes any mute" — would hand a project mute power over the
// PLATFORM signal: the operator of project A mutes MasterDown "for himself" and
// master goes quiet for everyone, including project B, which never hears about
// it. So a project-scoped mute never covers a platform alert (project == "");
// to silence a platform alert you mute it WITHOUT a project — visibly, on
// purpose. Both rules pick the same safe side: showing one alert too many beats
// hiding one, and muting one alert too few beats muting one too many.
func (m AlertMute) Matches(alertname, region, project string) bool {
	if m.Alertname != alertname {
		return false
	}
	if m.Region != nil && *m.Region != region {
		return false
	}
	return m.Project == nil || *m.Project == project
}

const alertMuteCols = `id::text, alertname, region, project, note, created_at, expires_at, created_by, silence_id`

// scanTargets are the Scan destinations for alertMuteCols, in order. Five
// queries read that column list, and a silent drift between it and the
// destinations would land the region in the project field — so both live side
// by side and are edited together.
func (m *AlertMute) scanTargets() []any {
	return []any{&m.ID, &m.Alertname, &m.Region, &m.Project, &m.Note,
		&m.CreatedAt, &m.ExpiresAt, &m.CreatedBy, &m.SilenceID}
}

// CreateAlertMuteParams is the input to UpsertAlertMute. A nil Region means
// "all regions", a nil Project "all projects", a nil ExpiresAt "never expires";
// Note/CreatedBy default to "".
type CreateAlertMuteParams struct {
	Alertname string
	Region    *string
	Project   *string
	Note      string
	ExpiresAt *time.Time
	CreatedBy string
}

// UpsertAlertMute creates a mute or — when an ACTIVE mute for the same
// (alertname, region, project) already exists — updates that row's
// note/expires_at in place. This makes POST /v1/alerts/mutes an idempotent
// upsert: re-muting the same target doubles as "extend/edit" instead of piling
// up duplicates or 409. Region AND project are matched null-aware (`is not
// distinct from`), so a NULL-region ("all regions") / NULL-project ("all
// projects") mute is a distinct target from a specific one — muting NodeDown
// for project A must not overwrite the mute project B put on the same alert.
// The lookup+write share one transaction with FOR UPDATE so concurrent
// re-mutes of the same target converge on one row.
func (s *Store) UpsertAlertMute(ctx context.Context, p CreateAlertMuteParams) (AlertMute, error) {
	p.Alertname = strings.TrimSpace(p.Alertname)
	if p.Alertname == "" {
		return AlertMute{}, errors.New("alertname is required")
	}
	p.Region = normalizeMuteTarget(p.Region)
	p.Project = normalizeMuteTarget(p.Project)

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
		  and project is not distinct from $3
		  and (expires_at is null or expires_at > now())
		order by created_at desc
		limit 1
		for update`, p.Alertname, p.Region, p.Project).
		Scan(m.scanTargets()...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `
			insert into alert_mutes (alertname, region, project, note, expires_at, created_by)
			values ($1, $2, $3, $4, $5, $6)
			returning `+alertMuteCols,
			p.Alertname, p.Region, p.Project, p.Note, p.ExpiresAt, p.CreatedBy).
			Scan(m.scanTargets()...)
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
			Scan(m.scanTargets()...)
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
		if err := rows.Scan(m.scanTargets()...); err != nil {
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
		Scan(m.scanTargets()...)
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

// normalizeMuteTarget collapses an empty/whitespace component of a mute's
// target to nil ("all regions" / "all projects"), so `"region":""` and an
// absent region — likewise for project — behave identically. One helper for
// both because the rule is the same one: the empty string is never a target of
// its own, it is the wildcard spelled sloppily.
func normalizeMuteTarget(v *string) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return nil
	}
	return &s
}
