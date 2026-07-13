package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }
func strPtr(s string) *string { return &s }

func TestBackupSettingsDefaultsAndPatch(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	// Seed-строка миграции: дефолты старого таймера.
	s, err := st.GetBackupSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !s.Enabled || s.IntervalHours != 6 || s.RetentionLocal != 14 || s.S3Enabled || s.HasS3Secret || s.RetentionS3 != 30 {
		t.Fatalf("unexpected defaults: %+v", s)
	}

	// Частичный PATCH: не переданные поля не меняются.
	s2, err := st.PatchBackupSettings(ctx, store.BackupSettingsPatch{IntervalHours: intPtr(12)})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if s2.IntervalHours != 12 || s2.RetentionLocal != 14 || !s2.Enabled {
		t.Fatalf("partial patch broke untouched fields: %+v", s2)
	}

	// Валидация границ.
	if _, err := st.PatchBackupSettings(ctx, store.BackupSettingsPatch{IntervalHours: intPtr(0)}); err == nil {
		t.Fatal("interval 0 must be rejected")
	}
	if _, err := st.PatchBackupSettings(ctx, store.BackupSettingsPatch{RetentionLocal: intPtr(0)}); err == nil {
		t.Fatal("retention 0 must be rejected")
	}
}

func TestBackupSettingsS3SecretWriteOnly(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	// Включение S3 без обязательных полей — ошибка.
	if _, err := st.PatchBackupSettings(ctx, store.BackupSettingsPatch{S3Enabled: boolPtr(true)}); err == nil {
		t.Fatal("s3_enabled without endpoint/bucket/creds must be rejected")
	}
	// Endpoint обязан быть http(s) URL.
	bad := store.BackupSettingsPatch{
		S3Enabled: boolPtr(true), S3Endpoint: strPtr("s3.example.com"),
		S3Bucket: strPtr("b"), S3AccessKey: strPtr("ak"), S3SecretKey: strPtr("sk"),
	}
	if _, err := st.PatchBackupSettings(ctx, bad); err == nil {
		t.Fatal("endpoint without scheme must be rejected")
	}

	good := bad
	good.S3Endpoint = strPtr("https://s3.example.com")
	s, err := st.PatchBackupSettings(ctx, good)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if !s.HasS3Secret {
		t.Fatal("HasS3Secret must be true after rotate")
	}

	// Секрет в БД — только конвертом.
	var raw string
	if err := st.Pool.QueryRow(ctx, `select s3_secret_key from backup_settings`).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !strings.HasPrefix(raw, "birdman:v1:") {
		t.Fatalf("secret stored in plaintext: %q", raw)
	}

	// keep: nil-поле секрета не трогает хранимое значение.
	if _, err := st.PatchBackupSettings(ctx, store.BackupSettingsPatch{S3Region: strPtr("eu")}); err != nil {
		t.Fatalf("keep patch: %v", err)
	}
	cfg, err := st.BackupS3Config(ctx)
	if err != nil {
		t.Fatalf("s3 config: %v", err)
	}
	if cfg.SecretKey != "sk" || cfg.Endpoint != "https://s3.example.com" || cfg.RetentionS3 != 30 {
		t.Fatalf("round-trip mismatch: %+v", cfg)
	}
}

func TestBackupRunsLifecycle(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	if _, ok, err := st.LastBackupSuccess(ctx); err != nil || ok {
		t.Fatalf("empty history: ok=%v err=%v", ok, err)
	}
	id, err := st.InsertBackupRun(ctx, "manual")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.FinishBackupRun(ctx, id, "ok", 4096, false, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	when, ok, err := st.LastBackupSuccess(ctx)
	if err != nil || !ok || time.Since(when) > time.Minute {
		t.Fatalf("last success: %v %v %v", when, ok, err)
	}
	runs, err := st.ListBackupRuns(ctx, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("list: %v %v", runs, err)
	}
	r := runs[0]
	if r.Result != "ok" || r.Kind != "manual" || r.SizeBytes == nil || *r.SizeBytes != 4096 || r.FinishedAt == nil {
		t.Fatalf("run row: %+v", r)
	}

	// Ротация истории: 5 строк, keep 3.
	for range 5 {
		id, _ := st.InsertBackupRun(ctx, "scheduled")
		_ = st.FinishBackupRun(ctx, id, "error", 0, false, "boom")
	}
	if _, err := st.PruneBackupRuns(ctx, 3); err != nil {
		t.Fatalf("prune: %v", err)
	}
	runs, _ = st.ListBackupRuns(ctx, 50)
	if len(runs) != 3 {
		t.Fatalf("prune kept %d, want 3", len(runs))
	}
}

func TestAcquireBackupLock(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	rel1, ok, err := st.AcquireBackupLock(ctx)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	_, ok2, err := st.AcquireBackupLock(ctx)
	if err != nil || ok2 {
		t.Fatalf("second acquire must be busy: ok=%v err=%v", ok2, err)
	}
	rel1()
	rel2, ok3, err := st.AcquireBackupLock(ctx)
	if err != nil || !ok3 {
		t.Fatalf("acquire after release: ok=%v err=%v", ok3, err)
	}
	rel2()
}
