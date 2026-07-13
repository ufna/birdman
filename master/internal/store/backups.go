package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Backups v1 (docs/superpowers/specs/2026-07-13-backups-admin-v1-design.md).
// backup_settings — singleton; s3_secret_key write-only (AEAD-конверт),
// наружу отдаётся только HasS3Secret. Секрет-несущий read единственный —
// BackupS3Config (для раннера), strict decrypt как ListRegistryCreds.

type BackupSettings struct {
	Enabled        bool      `json:"enabled"`
	IntervalHours  int       `json:"interval_hours"`
	RetentionLocal int       `json:"retention_local"`
	S3Enabled      bool      `json:"s3_enabled"`
	S3Endpoint     string    `json:"s3_endpoint"`
	S3Region       string    `json:"s3_region"`
	S3Bucket       string    `json:"s3_bucket"`
	S3Prefix       string    `json:"s3_prefix"`
	S3AccessKey    string    `json:"s3_access_key"`
	HasS3Secret    bool      `json:"has_s3_secret"`
	RetentionS3    int       `json:"retention_s3"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type BackupSettingsPatch struct {
	Enabled        *bool
	IntervalHours  *int
	RetentionLocal *int
	S3Enabled      *bool
	S3Endpoint     *string
	S3Region       *string
	S3Bucket       *string
	S3Prefix       *string
	S3AccessKey    *string
	// S3SecretKey: nil — не трогать (keep), непустая строка — ротация.
	// Пустая строка — очистить (валидно только при s3_enabled=false).
	S3SecretKey *string
	RetentionS3 *int
}

type BackupS3Config struct {
	Endpoint    string
	Region      string
	Bucket      string
	Prefix      string
	AccessKey   string
	SecretKey   string
	RetentionS3 int
}

type BackupRun struct {
	ID         int64      `json:"id"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Kind       string     `json:"kind"`
	Result     string     `json:"result"`
	SizeBytes  *int64     `json:"size_bytes"`
	S3Uploaded bool       `json:"s3_uploaded"`
	Error      string     `json:"error"`
}

const settingsCols = `enabled, interval_hours, retention_local, s3_enabled,
	s3_endpoint, s3_region, s3_bucket, s3_prefix, s3_access_key,
	s3_secret_key <> '' as has_s3_secret, retention_s3, updated_at`

func scanBackupSettings(row interface{ Scan(...any) error }) (BackupSettings, error) {
	var s BackupSettings
	err := row.Scan(&s.Enabled, &s.IntervalHours, &s.RetentionLocal, &s.S3Enabled,
		&s.S3Endpoint, &s.S3Region, &s.S3Bucket, &s.S3Prefix, &s.S3AccessKey,
		&s.HasS3Secret, &s.RetentionS3, &s.UpdatedAt)
	return s, err
}

func (s *Store) GetBackupSettings(ctx context.Context) (BackupSettings, error) {
	return scanBackupSettings(s.Pool.QueryRow(ctx,
		`select `+settingsCols+` from backup_settings where id`))
}

// validateBackupSettings проверяет границы и согласованность полей. Вызывается
// из PatchBackupSettings — та же проверка покрывает и PATCH из httpapi (он
// ходит через этот store-метод), так что валидатор один на оба входа.
func validateBackupSettings(intervalHours, retentionLocal, retentionS3 int, s3Enabled bool, endpoint, bucket, accessKey string, hasSecret bool) error {
	if intervalHours < 1 || intervalHours > 168 {
		return errors.New("interval_hours must be within 1..168")
	}
	if retentionLocal < 1 || retentionLocal > 365 {
		return errors.New("retention_local must be within 1..365")
	}
	if retentionS3 < 1 || retentionS3 > 3650 {
		return errors.New("retention_s3 must be within 1..3650")
	}
	if !s3Enabled {
		return nil
	}
	if endpoint == "" || bucket == "" || accessKey == "" {
		return errors.New("s3_enabled requires s3_endpoint, s3_bucket and s3_access_key")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("s3_endpoint must be an http(s) URL, e.g. https://s3.eu-central-003.backblazeb2.com")
	}
	if !hasSecret {
		return errors.New("s3_enabled requires s3_secret_key (set it in this request or earlier)")
	}
	return nil
}

func (s *Store) PatchBackupSettings(ctx context.Context, p BackupSettingsPatch) (BackupSettings, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BackupSettings{}, err
	}
	defer tx.Rollback(ctx)

	// Текущее состояние (в tx — против гонки двух PATCH).
	cur, err := scanBackupSettings(tx.QueryRow(ctx,
		`select `+settingsCols+` from backup_settings where id for update`))
	if err != nil {
		return BackupSettings{}, err
	}

	next := cur
	if p.Enabled != nil {
		next.Enabled = *p.Enabled
	}
	if p.IntervalHours != nil {
		next.IntervalHours = *p.IntervalHours
	}
	if p.RetentionLocal != nil {
		next.RetentionLocal = *p.RetentionLocal
	}
	if p.S3Enabled != nil {
		next.S3Enabled = *p.S3Enabled
	}
	if p.S3Endpoint != nil {
		next.S3Endpoint = *p.S3Endpoint
	}
	if p.S3Region != nil {
		next.S3Region = *p.S3Region
	}
	if p.S3Bucket != nil {
		next.S3Bucket = *p.S3Bucket
	}
	if p.S3Prefix != nil {
		next.S3Prefix = *p.S3Prefix
	}
	if p.S3AccessKey != nil {
		next.S3AccessKey = *p.S3AccessKey
	}
	if p.RetentionS3 != nil {
		next.RetentionS3 = *p.RetentionS3
	}

	hasSecret := cur.HasS3Secret
	encSecret := "" // пусто = не трогаем колонку
	if p.S3SecretKey != nil {
		if *p.S3SecretKey == "" {
			hasSecret = false
			encSecret = "-" // маркер очистки, см. SQL ниже
		} else {
			hasSecret = true
			enc, err := s.codec.Encrypt([]byte(*p.S3SecretKey), "backup_settings.s3_secret_key")
			if err != nil {
				return BackupSettings{}, err
			}
			encSecret = enc
		}
	}

	if err := validateBackupSettings(next.IntervalHours, next.RetentionLocal, next.RetentionS3,
		next.S3Enabled, strings.TrimSpace(next.S3Endpoint), strings.TrimSpace(next.S3Bucket),
		strings.TrimSpace(next.S3AccessKey), hasSecret); err != nil {
		return BackupSettings{}, err
	}

	// case-цепочка секрета: '' → keep, '-' → clear, иначе новое значение.
	// Маркеры '' и '-' безопасны: настоящий конверт всегда начинается с
	// birdman:v1:, а ротация пустой строкой запрещена веткой выше.
	row := tx.QueryRow(ctx, `
		update backup_settings set
			enabled = $1, interval_hours = $2, retention_local = $3,
			s3_enabled = $4, s3_endpoint = $5, s3_region = $6, s3_bucket = $7,
			s3_prefix = $8, s3_access_key = $9,
			s3_secret_key = case when $10 = '' then s3_secret_key
			                     when $10 = '-' then ''
			                     else $10 end,
			retention_s3 = $11, updated_at = now()
		where id
		returning `+settingsCols,
		next.Enabled, next.IntervalHours, next.RetentionLocal,
		next.S3Enabled, strings.TrimSpace(next.S3Endpoint), strings.TrimSpace(next.S3Region),
		strings.TrimSpace(next.S3Bucket), strings.TrimSpace(next.S3Prefix),
		strings.TrimSpace(next.S3AccessKey), encSecret, next.RetentionS3)
	out, err := scanBackupSettings(row)
	if err != nil {
		return BackupSettings{}, err
	}
	return out, tx.Commit(ctx)
}

// BackupS3Config — единственный read с расшифрованным секретом (раннер).
// Strict decrypt: не-конверт в колонке = ошибка (как ListRegistryCreds).
func (s *Store) BackupS3Config(ctx context.Context) (BackupS3Config, error) {
	var c BackupS3Config
	var enc string
	err := s.Pool.QueryRow(ctx, `
		select s3_endpoint, s3_region, s3_bucket, s3_prefix, s3_access_key,
		       s3_secret_key, retention_s3
		from backup_settings where id`).
		Scan(&c.Endpoint, &c.Region, &c.Bucket, &c.Prefix, &c.AccessKey, &enc, &c.RetentionS3)
	if err != nil {
		return BackupS3Config{}, err
	}
	if enc == "" {
		return BackupS3Config{}, errors.New("backup s3 secret is not configured")
	}
	plain, err := s.codec.Decrypt(enc, "backup_settings.s3_secret_key")
	if err != nil {
		return BackupS3Config{}, fmt.Errorf("decrypt backup s3 secret: %w", err)
	}
	c.SecretKey = string(plain)
	return c, nil
}

func (s *Store) InsertBackupRun(ctx context.Context, kind string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		insert into backup_runs (kind, result) values ($1, 'running') returning id`, kind).Scan(&id)
	return id, err
}

func (s *Store) FinishBackupRun(ctx context.Context, id int64, result string, sizeBytes int64, s3Uploaded bool, errMsg string) error {
	var size *int64
	if result == "ok" {
		size = &sizeBytes
	}
	_, err := s.Pool.Exec(ctx, `
		update backup_runs set finished_at = now(), result = $2, size_bytes = $3,
			s3_uploaded = $4, error = $5
		where id = $1`, id, result, size, s3Uploaded, errMsg)
	return err
}

func (s *Store) ListBackupRuns(ctx context.Context, limit int) ([]BackupRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		select id, started_at, finished_at, kind, result, size_bytes, s3_uploaded, error
		from backup_runs order by started_at desc, id desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupRun{}
	for rows.Next() {
		var r BackupRun
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Kind, &r.Result,
			&r.SizeBytes, &r.S3Uploaded, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) PruneBackupRuns(ctx context.Context, keep int) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		delete from backup_runs where id not in (
			select id from backup_runs order by started_at desc, id desc limit $1)`, keep)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) LastBackupSuccess(ctx context.Context) (time.Time, bool, error) {
	var t time.Time
	err := s.Pool.QueryRow(ctx, `
		select started_at from backup_runs where result = 'ok'
		order by started_at desc, id desc limit 1`).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// AcquireBackupLock — session-level advisory lock на выделенном соединении
// пула: один прогон бекапа на весь кластер master'ов. Возврат ok=false —
// лок занят (другой прогон идёт). release обязателен при ok=true.
const backupLockKey = "birdman:backups:run"

func (s *Store) AcquireBackupLock(ctx context.Context) (func(), bool, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRow(ctx,
		`select pg_try_advisory_lock(hashtextextended($1, 42))`, backupLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	release := func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			`select pg_advisory_unlock(hashtextextended($1, 42))`, backupLockKey)
		conn.Release()
	}
	return release, true, nil
}
