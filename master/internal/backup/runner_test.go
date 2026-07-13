package backup

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/config"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakePgDump кладёт исполняемый скрипт и возвращает его путь.
// mode: "ok" — пишет 64 байта payload; "fail" — stderr+exit 1;
// "slow" — sleep 2 перед payload (для таймаут-теста: дамп не успевает
// за runTimeout/ctx); versionMajor — что печатать на --version.
// На реальном прогоне дампа (не --version) обёртка фиксирует свои argv и
// PGPASSWORD в <dir>/pg_dump.args и <dir>/pg_dump.env — на это опирается
// TestRunOnceKeepsPasswordOutOfArgv (пароль обязан ехать env, не argv).
func fakePgDump(t *testing.T, dir, mode string, versionMajor int) string {
	t.Helper()
	argsMarker := filepath.Join(dir, "pg_dump.args")
	envMarker := filepath.Join(dir, "pg_dump.env")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "pg_dump (PostgreSQL) %d.1"
  exit 0
fi
echo "$@" > "%s"
env | grep '^PGPASSWORD=' > "%s" || true
case "%s" in
ok)
  printf 'PGDMP-fake-payload-PGDMP-fake-payload-PGDMP-fake-payload-PGDMP!!'
  ;;
slow)
  sleep 2
  printf 'PGDMP-fake-payload-PGDMP-fake-payload-PGDMP-fake-payload-PGDMP!!'
  ;;
fail)
  echo "connection refused: fake failure" >&2
  exit 1
  ;;
esac
`, versionMajor, argsMarker, envMarker, mode)
	p := filepath.Join(dir, "pg_dump")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestRunner(t *testing.T, st *store.Store, mode string, versionMajor int) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	pgDump := fakePgDump(t, dir, mode, versionMajor)
	r := New(st, os.Getenv("BIRDMAN_TEST_DSN"), config.Backups{Dir: dir, PgDumpPath: pgDump}, testLog())
	return r, dir
}

func TestRunOnceOKAndRotation(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	r, dir := newTestRunner(t, st, "ok", 16) // testdb = postgres:16

	// Предзаполнить каталог старыми дампами (имитация снесённого таймера) —
	// формат имён тот же, ротация должна их подхватить.
	for i := range 20 {
		// уникальные лексикографически возрастающие имена:
		name := fmt.Sprintf("birdman-202601%02dT000000Z.dump", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.runOnce(ctx, "manual"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	// Файл дампа появился, .partial не остался.
	dumps, _ := filepath.Glob(filepath.Join(dir, "birdman-*.dump"))
	partials, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
	if len(partials) != 0 {
		t.Fatalf("partial left behind: %v", partials)
	}
	// Ротация: дефолтный retention_local=14.
	if len(dumps) != 14 {
		t.Fatalf("rotation kept %d, want 14", len(dumps))
	}

	// Строка истории ok с размером.
	runs, err := st.ListBackupRuns(ctx, 5)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	if runs[0].Result != "ok" || runs[0].SizeBytes == nil || *runs[0].SizeBytes != 64 {
		t.Fatalf("run row: %+v", runs[0])
	}
}

// TestRunOnceKeepsPasswordOutOfArgv — регрессия: pg_dump кладёт весь -d DSN в
// argv, а /proc/<pid>/cmdline world-readable (master на дев-боксе — хост-
// процесс), поэтому пароль Postgres обязан ехать pg_dump через PGPASSWORD в
// env, а в argv — DSN без пароля. Синтетический DSN с паролем достаточно:
// mode "ok" саму БД не читает (история пишется в живой testdb через store).
func TestRunOnceKeepsPasswordOutOfArgv(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	pgDump := fakePgDump(t, dir, "ok", 16) // testdb = postgres:16
	const dsn = "postgres://postgres:sekret-pw@127.0.0.1:5432/x?sslmode=disable"
	r := New(st, dsn, config.Backups{Dir: dir, PgDumpPath: pgDump}, testLog())

	if err := r.runOnce(ctx, "manual"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	args, err := os.ReadFile(filepath.Join(dir, "pg_dump.args"))
	if err != nil {
		t.Fatalf("read args marker: %v", err)
	}
	if strings.Contains(string(args), "sekret-pw") {
		t.Fatalf("password leaked into pg_dump argv: %q", string(args))
	}
	env, err := os.ReadFile(filepath.Join(dir, "pg_dump.env"))
	if err != nil {
		t.Fatalf("read env marker: %v", err)
	}
	if !strings.Contains(string(env), "PGPASSWORD=sekret-pw") {
		t.Fatalf("password not passed via PGPASSWORD env: %q", string(env))
	}
}

func TestRunOnceDumpFailure(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	r, dir := newTestRunner(t, st, "fail", 16)

	if err := r.runOnce(ctx, "scheduled"); err == nil {
		t.Fatal("runOnce must return the dump error")
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(partials) != 0 {
		t.Fatalf("partial left behind: %v", partials)
	}
	runs, _ := st.ListBackupRuns(ctx, 5)
	if len(runs) != 1 || runs[0].Result != "error" || !strings.Contains(runs[0].Error, "connection refused") {
		t.Fatalf("run row: %+v", runs)
	}
	// Событие backup_failed.
	events, err := st.ListEvents(ctx, 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == store.EventBackupFailed {
			found = true
		}
	}
	if !found {
		t.Fatal("backup_failed event not emitted")
	}
}

func TestRunOnceVersionMismatch(t *testing.T) {
	st := testdb.New(t)
	r, _ := newTestRunner(t, st, "ok", 99)
	err := r.runOnce(context.Background(), "manual")
	if err == nil || !strings.Contains(err.Error(), "major") {
		t.Fatalf("want version mismatch error, got: %v", err)
	}
	runs, _ := st.ListBackupRuns(context.Background(), 5)
	if len(runs) != 1 || runs[0].Result != "error" {
		t.Fatalf("version mismatch must be recorded: %+v", runs)
	}
}

// TestRunOnceRecordsFailureOnDeadContext — fail-loud обязан переживать
// отмену/таймаут самого прогона (runTimeout, остановка loopCtx): упавший
// прогон оставляет либо error-строку, либо (если строка не успела
// создаться — первые DB-вызовы runOnce идут по боевому ctx и падают
// раньше fail()) хотя бы событие backup_failed. Ни одна строка не
// залипает в 'running'.
func TestRunOnceRecordsFailureOnDeadContext(t *testing.T) {
	st := testdb.New(t)
	r, _ := newTestRunner(t, st, "ok", 16)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // прогон стартует с уже мёртвым ctx
	if err := r.runOnce(ctx, "manual"); err == nil {
		t.Fatal("runOnce on dead ctx must return an error")
	}

	// Читаем живым ctx — проверяется состояние БД, а не сам мёртвый ctx.
	runs, err := st.ListBackupRuns(context.Background(), 5)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	for _, run := range runs {
		if run.Result != "error" {
			t.Fatalf("run row stuck in %q (want error or no row at all): %+v", run.Result, run)
		}
	}
	events, err := st.ListEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == store.EventBackupFailed {
			found = true
		}
	}
	if !found {
		t.Fatal("backup_failed event not emitted on dead ctx")
	}
}

// TestRunOnceTimeoutMarksError — реальный сценарий runTimeout: строка истории
// уже создана (settings+insert прошли живым ctx), затем дамп упирается в
// таймаут и боевой ctx умирает. Ветка fail() runID != 0 обязана
// финализировать строку result='error' через отвязанный ctx — иначе она
// залипнет в 'running' при мёртвом дампе. Прошлый регресс-тест
// (pre-cancelled ctx) до этой ветки не доходил: там первый же DB-вызов падал
// раньше InsertBackupRun и runID оставался 0.
func TestRunOnceTimeoutMarksError(t *testing.T) {
	st := testdb.New(t)
	r, _ := newTestRunner(t, st, "slow", 16)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := r.runOnce(ctx, "scheduled"); err == nil {
		t.Fatal("runOnce must time out during the dump and return an error")
	}

	// Читаем живым ctx: строка создана и финализирована error (не 'running').
	runs, err := st.ListBackupRuns(context.Background(), 5)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("want exactly one history row, got %d: %+v", len(runs), runs)
	}
	if runs[0].Result != "error" {
		t.Fatalf("timeout must finalize the row as error, got %q: %+v", runs[0].Result, runs[0])
	}
}

func TestRunNowBusy(t *testing.T) {
	st := testdb.New(t)
	r, _ := newTestRunner(t, st, "ok", 16)
	r.running.Store(true) // симуляция идущего прогона
	if err := r.RunNow(context.Background()); err != ErrBusy {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	if !due(now, time.Time{}, false, 6*time.Hour) {
		t.Fatal("no history → due")
	}
	if due(now, now.Add(-5*time.Hour), true, 6*time.Hour) {
		t.Fatal("5h < 6h → not due")
	}
	if !due(now, now.Add(-7*time.Hour), true, 6*time.Hour) {
		t.Fatal("7h > 6h → due")
	}
}
