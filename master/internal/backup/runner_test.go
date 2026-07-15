package backup

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

// TestBackoffDelay — экспонента tickInterval·2^streak с капом maxBackoff.
func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{5, 32 * time.Minute},
		{6, maxBackoff},   // 64м > 1ч → кап
		{100, maxBackoff}, // большой streak: кап, без переполнения int64
	}
	for _, c := range cases {
		if got := backoffDelay(c.streak); got != c.want {
			t.Fatalf("backoffDelay(%d) = %v, want %v", c.streak, got, c.want)
		}
	}
}

// TestMaybeRunBackoff — упавший scheduled-прогон не ретраится каждый тик:
// второй немедленный maybeRun гейтится бэкоффом (nil, нового прогона нет),
// а после истечения nextRetry прогон повторяется и failStreak растёт.
func TestMaybeRunBackoff(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	r, _ := newTestRunner(t, st, "fail", 16) // дефолты settings: enabled, истории нет → due

	// Первый прогон падает: 1 error-строка, бэкофф взведён (failStreak=1).
	if err := r.maybeRun(ctx); err == nil {
		t.Fatal("first maybeRun must fail (mode fail)")
	}
	if runs, _ := st.ListBackupRuns(ctx, 10); len(runs) != 1 {
		t.Fatalf("after first fail want 1 history row, got %d", len(runs))
	}
	if r.failStreak != 1 {
		t.Fatalf("failStreak want 1, got %d", r.failStreak)
	}

	// Немедленный второй вызов: гейт бэкоффа → nil и НИ ОДНОГО нового прогона.
	if err := r.maybeRun(ctx); err != nil {
		t.Fatalf("backoff-gated maybeRun must return nil, got %v", err)
	}
	if runs, _ := st.ListBackupRuns(ctx, 10); len(runs) != 1 {
		t.Fatalf("backoff gate must skip the run: want 1 row, got %d", len(runs))
	}

	// Отматываем nextRetry в прошлое — гейт открыт, третий прогон снова падает.
	r.nextRetry = time.Now().Add(-time.Second)
	if err := r.maybeRun(ctx); err == nil {
		t.Fatal("third maybeRun (backoff elapsed) must fail again")
	}
	if runs, _ := st.ListBackupRuns(ctx, 10); len(runs) != 2 {
		t.Fatalf("after backoff elapsed want 2 rows, got %d", len(runs))
	}
	if r.failStreak != 2 {
		t.Fatalf("failStreak want 2, got %d", r.failStreak)
	}
}

// TestSweepStuck — стартовый свип финализирует running-строку, осиротевшую
// после kill -9 мастера, но только когда advisory-лок свободен: под удержанным
// локом (легитимный прогон другого master) строку он не трогает.
func TestSweepStuck(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	r, _ := newTestRunner(t, st, "ok", 16)

	stuckID, err := st.InsertBackupRun(ctx, "scheduled") // остаётся 'running'
	if err != nil {
		t.Fatalf("insert stuck: %v", err)
	}
	okID, _ := st.InsertBackupRun(ctx, "manual")
	if err := st.FinishBackupRun(ctx, okID, "ok", 128, false, ""); err != nil {
		t.Fatalf("finish ok: %v", err)
	}

	// Пока лок удержан тестом — свип обязан спасовать: running-строка легитимна.
	rel, ok, err := st.AcquireBackupLock(ctx)
	if err != nil || !ok {
		t.Fatalf("hold lock: ok=%v err=%v", ok, err)
	}
	r.sweepStuck(ctx)
	if res := runResult(t, ctx, st, stuckID); res != "running" {
		t.Fatalf("sweep under a held lock must not touch the row, got %q", res)
	}
	rel()

	// Лок свободен — свип финализирует running как error.
	r.sweepStuck(ctx)

	byID := map[int64]store.BackupRun{}
	runs, _ := st.ListBackupRuns(ctx, 10)
	for _, run := range runs {
		byID[run.ID] = run
	}
	stuck := byID[stuckID]
	if stuck.Result != "error" || stuck.FinishedAt == nil || !strings.Contains(stuck.Error, "restart") {
		t.Fatalf("stuck row not swept to error: %+v", stuck)
	}
	if okRow := byID[okID]; okRow.Result != "ok" || okRow.SizeBytes == nil || *okRow.SizeBytes != 128 {
		t.Fatalf("ok row must be untouched: %+v", okRow)
	}
	if n, err := st.CountEvents(ctx, store.EventBackupFailed); err != nil || n == 0 {
		t.Fatalf("sweep must emit backup_failed event: n=%d err=%v", n, err)
	}
}

func runResult(t *testing.T, ctx context.Context, st *store.Store, id int64) string {
	t.Helper()
	runs, err := st.ListBackupRuns(ctx, 50)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, r := range runs {
		if r.ID == id {
			return r.Result
		}
	}
	t.Fatalf("run %d not found", id)
	return ""
}

// TestRunNowHappyPath — ручной прогон доводит строку до ok и снимает флаг
// running; повторный прогон после полной финализации первого даёт вторую
// ok-строку. Полл до running==false гарантирует, что фоновая горутина (вместе
// с release лока) завершилась до конца теста — иначе testdb-cleanup дропнет БД
// из-под неё.
func TestRunNowHappyPath(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	r, _ := newTestRunner(t, st, "ok", 16)

	if err := r.RunNow(ctx); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	waitOK(t, r, st, 1)

	if err := r.RunNow(ctx); err != nil {
		t.Fatalf("second RunNow: %v", err)
	}
	waitOK(t, r, st, 2)
}

// waitOK ждёт (дедлайн 10с) ровно wantOK строк result=='ok' И r.running==false.
func waitOK(t *testing.T, r *Runner, st *store.Store, wantOK int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := st.ListBackupRuns(context.Background(), 50)
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		ok := 0
		for _, run := range runs {
			if run.Result == "ok" {
				ok++
			}
		}
		if ok == wantOK && !r.running.Load() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d ok run(s) with the runner idle", wantOK)
}

// TestScrubDSNPassword — пароль обязан ехать через PGPASSWORD (env), а не в argv.
func TestScrubDSNPassword(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantArg string
		wantEnv []string
	}{
		{
			name:    "url with password",
			dsn:     "postgres://user:sekret@127.0.0.1:5432/db?sslmode=disable",
			wantArg: "postgres://user@127.0.0.1:5432/db?sslmode=disable",
			wantEnv: []string{"PGPASSWORD=sekret"},
		},
		{
			name:    "url without userinfo",
			dsn:     "postgres://127.0.0.1:5432/db",
			wantArg: "postgres://127.0.0.1:5432/db",
			wantEnv: nil,
		},
		{
			name:    "keyword conninfo",
			dsn:     "host=x dbname=y",
			wantArg: "host=x dbname=y",
			wantEnv: nil,
		},
		{
			name:    "url user without password",
			dsn:     "postgres://user@127.0.0.1:5432/db",
			wantArg: "postgres://user@127.0.0.1:5432/db",
			wantEnv: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			arg, env := scrubDSNPassword(c.dsn)
			if arg != c.wantArg {
				t.Fatalf("arg = %q, want %q", arg, c.wantArg)
			}
			if !reflect.DeepEqual(env, c.wantEnv) {
				t.Fatalf("env = %v, want %v", env, c.wantEnv)
			}
			if strings.Contains(arg, "sekret") {
				t.Fatalf("password leaked into argv DSN: %q", arg)
			}
		})
	}
}
