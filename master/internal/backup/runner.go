// Package backup — Backups v1: master сам исполняет бекапы Postgres
// (docs/superpowers/specs/2026-07-13-backups-admin-v1-design.md §2).
// Планировщик по образцу statsrollup: тик раз в минуту, прогон когда
// enabled && now ≥ last_ok+interval. Политика (интервал/ретеншны/S3) — в БД
// (backup_settings), перечитывается на каждый прогон; деплой-концерны
// (каталог, путь pg_dump) — в config.Backups.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ufna/birdman/master/internal/config"
	"github.com/ufna/birdman/master/internal/store"
)

// ErrBusy — прогон уже идёт (в этом процессе или, через advisory-lock, в другом master).
var ErrBusy = errors.New("a backup run is already in progress")

const (
	tickInterval = time.Minute
	runTimeout   = 30 * time.Minute
	keepRuns     = 200 // ротация истории backup_runs
	stderrCap    = 4 * 1024
)

type Runner struct {
	st      *store.Store
	dsn     string
	dir     string
	pgDump  string
	log     *slog.Logger
	running atomic.Bool
}

func New(st *store.Store, dsn string, cfg config.Backups, log *slog.Logger) *Runner {
	return &Runner{st: st, dsn: dsn, dir: cfg.Dir, pgDump: cfg.PgDumpPath, log: log}
}

// Run — цикл планировщика; блокируется до отмены ctx.
func (r *Runner) Run(ctx context.Context) {
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		r.log.Error("backup: mkdir failed", "dir", r.dir, "err", err)
	}
	r.cleanupPartials()
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.maybeRun(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("backup: scheduled run failed", "err", err)
			}
		}
	}
}

func (r *Runner) maybeRun(ctx context.Context) error {
	s, err := r.st.GetBackupSettings(ctx)
	if err != nil {
		return err
	}
	if !s.Enabled {
		return nil
	}
	last, ok, err := r.st.LastBackupSuccess(ctx)
	if err != nil {
		return err
	}
	if !due(time.Now().UTC(), last, ok, time.Duration(s.IntervalHours)*time.Hour) {
		return nil
	}
	if !r.running.CompareAndSwap(false, true) {
		return nil // ручной прогон уже идёт
	}
	defer r.running.Store(false)
	release, got, err := r.st.AcquireBackupLock(ctx)
	if err != nil {
		return err
	}
	if !got {
		return nil // другой master уже бекапит
	}
	defer release()
	return r.runOnce(ctx, "scheduled")
}

// due — чистая функция планирования (юнит-тестируется без тикера).
func due(now, last time.Time, hasLast bool, interval time.Duration) bool {
	if !hasLast {
		return true
	}
	return now.Sub(last) >= interval
}

// RunNow — ручной запуск из панели: захватывает флаг и лок синхронно
// (чтобы 409 отдавался сразу), сам прогон — в фоне.
func (r *Runner) RunNow(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return ErrBusy
	}
	release, got, err := r.st.AcquireBackupLock(ctx)
	if err != nil {
		r.running.Store(false)
		return err
	}
	if !got {
		r.running.Store(false)
		return ErrBusy
	}
	go func() {
		defer r.running.Store(false)
		defer release()
		bg := context.WithoutCancel(ctx) // HTTP-запрос завершится раньше прогона
		if err := r.runOnce(bg, "manual"); err != nil {
			r.log.Error("backup: manual run failed", "err", err)
		}
	}()
	return nil
}

// runOnce — один прогон: версия → дамп → ротация → S3 → финализация строки
// истории. Ошибка любого шага фиксируется fail-loud: событием backup_failed
// и (когда строка истории уже создана) error-строкой в backup_runs;
// возвращается наружу для логов/тестов.
func (r *Runner) runOnce(ctx context.Context, kind string) error {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	// runID == 0, пока строка истории не создана: ранние ошибки (settings,
	// insert) фиксируются только событием — финализировать ещё нечего.
	var runID int64
	fail := func(cause error) error {
		// Записи исхода — через отвязанный ctx с собственным коротким
		// таймаутом: фиксация ошибки обязана переживать отмену/таймаут
		// самого прогона (runTimeout, остановка loopCtx), иначе строка
		// навсегда остаётся 'running', а событие backup_failed теряется —
		// fail-loud гас ровно в целевом классе отказов (ревью Task 2).
		// defer в замыкании корректен: fail() зовётся максимум раз на прогон.
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer fcancel()
		msg := cause.Error()
		if runID != 0 {
			if err := r.st.FinishBackupRun(fctx, runID, "error", 0, false, msg); err != nil {
				r.log.Error("backup: finish(error) failed", "err", err)
			}
		}
		if err := r.st.InsertEvent(fctx, store.EventBackupFailed, store.EventRef{},
			map[string]any{"kind": kind, "error": msg}); err != nil {
			r.log.Error("backup: event insert failed", "err", err)
		}
		return cause
	}

	s, err := r.st.GetBackupSettings(ctx)
	if err != nil {
		return fail(err)
	}
	runID, err = r.st.InsertBackupRun(ctx, kind)
	if err != nil {
		return fail(err)
	}

	if err := r.checkVersions(ctx); err != nil {
		return fail(err)
	}

	name := "birdman-" + time.Now().UTC().Format("20060102T150405Z") + ".dump"
	path := filepath.Join(r.dir, name)
	size, err := r.dump(ctx, path)
	if err != nil {
		return fail(err)
	}
	if err := r.rotateLocal(s.RetentionLocal); err != nil {
		return fail(fmt.Errorf("local rotation: %w", err))
	}

	s3Done := false
	if s.S3Enabled {
		cfg, err := r.st.BackupS3Config(ctx)
		if err != nil {
			return fail(fmt.Errorf("s3 config: %w (dump is on disk: %s)", err, name))
		}
		if err := r.syncS3(ctx, cfg, path, name); err != nil {
			return fail(fmt.Errorf("s3 upload failed (dump is on disk: %s): %w", name, err))
		}
		s3Done = true
	}

	// Финализацию УСПЕХА тоже пишем отвязанным ctx с коротким таймаутом
	// (симметрично fail()): дамп уже на диске и, если включён S3, выгружен —
	// смерть боевого ctx в зазоре между дампом/syncS3 и Finish не должна
	// оставить строку висеть в 'running' при живом успешном дампе (ревью
	// Task 2). PruneBackupRuns — косметика ротации истории, её оставляем на
	// боевом ctx: потеря на отмене безобидна.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer fcancel()
	if err := r.st.FinishBackupRun(fctx, runID, "ok", size, s3Done, ""); err != nil {
		return err
	}
	if _, err := r.st.PruneBackupRuns(ctx, keepRuns); err != nil {
		r.log.Error("backup: prune runs failed", "err", err)
	}
	r.log.Info("backup: ok", "file", name, "size", size, "s3", s3Done, "kind", kind)
	return nil
}

// dump — pg_dump -Fc, stdout стримится в .partial, затем fsync+rename.
func (r *Runner) dump(ctx context.Context, path string) (int64, error) {
	partial := path + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, r.pgDump, "-Fc", "-d", r.dsn)
	cmd.Stdout = f
	cmd.Stderr = &capWriter{b: &stderr, cap: stderrCap}
	runErr := cmd.Run()
	syncErr := f.Sync()
	closeErr := f.Close()
	if runErr != nil {
		_ = os.Remove(partial)
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return 0, fmt.Errorf("pg_dump: %s", msg)
	}
	if syncErr != nil || closeErr != nil {
		_ = os.Remove(partial)
		return 0, errors.Join(syncErr, closeErr)
	}
	if err := os.Rename(partial, path); err != nil {
		_ = os.Remove(partial)
		return 0, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// rotateLocal — держим retention свежих birdman-*.dump (ts в имени
// лексикографичен). Чужие файлы (restore-test и пр.) не трогаем.
func (r *Runner) rotateLocal(retention int) error {
	dumps, err := filepath.Glob(filepath.Join(r.dir, "birdman-*.dump"))
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dumps)))
	for _, old := range dumps[min(retention, len(dumps)):] {
		if err := os.Remove(old); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) cleanupPartials() {
	stale, _ := filepath.Glob(filepath.Join(r.dir, "birdman-*.dump.partial"))
	for _, p := range stale {
		r.log.Warn("backup: removing stale partial", "file", p)
		_ = os.Remove(p)
	}
}

// checkVersions — major pg_dump == major сервера, иначе битые дампы молча.
var firstInt = regexp.MustCompile(`(\d+)`)

func (r *Runner) checkVersions(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, r.pgDump, "--version").Output()
	if err != nil {
		return fmt.Errorf("pg_dump not runnable at %q: %w", r.pgDump, err)
	}
	dumpMajor, err := parseMajor(string(out))
	if err != nil {
		return fmt.Errorf("pg_dump --version: %w", err)
	}
	var sv string
	if err := r.st.Pool.QueryRow(ctx, `select current_setting('server_version')`).Scan(&sv); err != nil {
		return err
	}
	serverMajor, err := parseMajor(sv)
	if err != nil {
		return fmt.Errorf("server_version: %w", err)
	}
	if dumpMajor != serverMajor {
		return fmt.Errorf("pg_dump major %d does not match server major %d — install matching postgresql-client", dumpMajor, serverMajor)
	}
	return nil
}

func parseMajor(s string) (int, error) {
	m := firstInt.FindString(s)
	if m == "" {
		return 0, fmt.Errorf("no version number in %q", s)
	}
	return strconv.Atoi(m)
}

// capWriter — ограниченный буфер для stderr pg_dump.
type capWriter struct {
	b   *strings.Builder
	cap int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.b.Len() < w.cap {
		w.b.Write(p[:min(len(p), w.cap-w.b.Len())])
	}
	return len(p), nil
}
