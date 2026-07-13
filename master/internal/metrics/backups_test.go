package metrics_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestBackupMetrics: the seven Backups v1 series are derived from Postgres on
// every scrape (dbCollector), so they survive a master restart — nothing lives
// in process memory. Empty history → enabled=1 and interval=6h come from the
// backup_settings seed, last_success=0 and last_error=0 (never). After one ok
// run (S3 uploaded) and one error run: the last-success/last-error/size/s3
// gauges are set and runs_total splits ok=1/error=1 (runs_total is a live-row
// count — observability only, alerts ride last_error_timestamp; see
// backupRunsDesc). Reuses the package test harness (TestMain, findGauge,
// findCounter in metrics_test.go).
func TestBackupMetrics(t *testing.T) {
	st := testdb.New(t)
	m := metrics.New(st, testLog())

	// Empty history: enabled=1 (seed), interval=6h, last_success/last_error=0.
	if got := findGauge(t, m.Registry, "birdman_backup_enabled", nil); got != 1 {
		t.Fatalf("birdman_backup_enabled = %v, want 1 (seed default)", got)
	}
	if got := findGauge(t, m.Registry, "birdman_backup_interval_seconds", nil); got != 6*3600 {
		t.Fatalf("birdman_backup_interval_seconds = %v, want %d", got, 6*3600)
	}
	if got := findGauge(t, m.Registry, "birdman_backup_last_success_timestamp_seconds", nil); got != 0 {
		t.Fatalf("last_success on empty history = %v, want 0", got)
	}
	if got := findGauge(t, m.Registry, "birdman_backup_last_error_timestamp_seconds", nil); got != 0 {
		t.Fatalf("last_error on empty history = %v, want 0", got)
	}

	// One ok run with S3 uploaded, one error run.
	ctx := t.Context()
	id, err := st.InsertBackupRun(ctx, "manual")
	if err != nil {
		t.Fatalf("insert ok run: %v", err)
	}
	if err := st.FinishBackupRun(ctx, id, "ok", 12345, true, ""); err != nil {
		t.Fatalf("finish ok run: %v", err)
	}
	id2, err := st.InsertBackupRun(ctx, "scheduled")
	if err != nil {
		t.Fatalf("insert error run: %v", err)
	}
	if err := st.FinishBackupRun(ctx, id2, "error", 0, false, "boom"); err != nil {
		t.Fatalf("finish error run: %v", err)
	}

	if got := findGauge(t, m.Registry, "birdman_backup_last_success_timestamp_seconds", nil); got == 0 {
		t.Fatal("last_success must be set after an ok run")
	}
	if got := findGauge(t, m.Registry, "birdman_backup_last_size_bytes", nil); got != 12345 {
		t.Fatalf("birdman_backup_last_size_bytes = %v, want 12345", got)
	}
	if got := findGauge(t, m.Registry, "birdman_backup_s3_last_success_timestamp_seconds", nil); got == 0 {
		t.Fatal("s3 last_success must be set after an ok run with s3_uploaded")
	}
	// last_error — the rotation-immune base of the BackupFailed alert (Task 6):
	// it must flip from 0 the moment an error run lands in the history.
	if got := findGauge(t, m.Registry, "birdman_backup_last_error_timestamp_seconds", nil); got == 0 {
		t.Fatal("last_error must be set after an error run")
	}
	if got := findCounter(t, m.Registry, "birdman_backup_runs_total", map[string]string{"result": "ok"}); got != 1 {
		t.Fatalf("birdman_backup_runs_total{result=ok} = %v, want 1", got)
	}
	if got := findCounter(t, m.Registry, "birdman_backup_runs_total", map[string]string{"result": "error"}); got != 1 {
		t.Fatalf("birdman_backup_runs_total{result=error} = %v, want 1", got)
	}
}
