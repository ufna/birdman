package logrot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// С tracker #994 лог дедика лежит в {root}/{project}/{env}/{id}.log — пара
// (project, env) едет вместе с дедиком и становится стрим-лейблом в
// VictoriaLogs. Пара НЕ хранится в памяти агента (после рестарта карта серверов
// восстанавливается из label'ов контейнера, там её нет), поэтому каталог
// резолвится по файловой системе. Эти тесты держат ровно это: ротация,
// финализация, ретенция и tail обязаны находить лог В ПОДКАТАЛОГЕ, иначе
// разметка тихо ломает и live-tail, и уборку диска.

func scopedPath(t *testing.T, root, project, env, id string) string {
	t.Helper()
	dir := filepath.Join(root, project, env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, id+".log")
}

func TestServerDirResolvesScopedAndFlat(t *testing.T) {
	root := t.TempDir()
	write(t, scopedPath(t, root, "game", "dev", "scoped"), "x\n")
	write(t, filepath.Join(root, "flat.log"), "x\n")

	if got, want := ServerDir(root, "scoped"), filepath.Join(root, "game", "dev"); got != want {
		t.Fatalf("ServerDir(scoped) = %q, want %q", got, want)
	}
	if got := ServerDir(root, "flat"); got != root {
		t.Fatalf("ServerDir(flat) = %q, want %q (логи до разметки остаются плоскими)", got, root)
	}
	if got := ServerDir(root, "never-existed"); got != root {
		t.Fatalf("ServerDir(unknown) = %q, want %q", got, root)
	}
	// id подставляется в ГЛОБ и приходит от master'а: метасимвол не должен
	// приводить к чужому файлу, разделитель пути — к выходу из дерева.
	for _, bad := range []string{"*", "sco?ed", "[a-z]", "../../etc/passwd", "back\\slash", "game/dev/scoped"} {
		if got := ServerDir(root, bad); got != root {
			t.Fatalf("ServerDir(%q) = %q, want плоский фоллбэк %q", bad, got, root)
		}
	}
	// Уже сжатый лог тоже должен резолвиться: Finalize зовут по частям.
	write(t, scopedPath(t, root, "game", "dev", "gone")+".gz", "x")
	if got, want := ServerDir(root, "gone"), filepath.Join(root, "game", "dev"); got != want {
		t.Fatalf("ServerDir(архив) = %q, want %q", got, want)
	}
}

func TestRotateFinalizeSweepInScopedDir(t *testing.T) {
	root := t.TempDir()
	path := scopedPath(t, root, "game", "prod", "srv")
	write(t, path, strings.Repeat("a", 64)+"\n")

	r := New(Config{Dir: root, MaxSize: 10, Retention: time.Hour, Logf: t.Logf},
		func() []string { return []string{"srv"} })

	r.RotateOnce()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("ротация не нашла лог в подкаталоге: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 0 {
		t.Fatalf("copy-truncate не обрезал активный файл: %v", err)
	}

	r.Finalize("srv")
	for _, p := range []string{path + ".gz", path + ".1.gz"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("финализация не нашла лог в подкаталоге: %s: %v", p, err)
		}
	}

	// Ретенция: архив старше срока удаляется и из подкаталога тоже. Без обхода
	// дерева диск бы просто копился молча.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path+".gz", old, old); err != nil {
		t.Fatal(err)
	}
	rr := New(Config{Dir: root, Retention: time.Hour, Logf: t.Logf}, func() []string { return nil })
	rr.SweepOnce()
	if _, err := os.Stat(path + ".gz"); !os.IsNotExist(err) {
		t.Fatalf("ретенция не дошла до подкаталога: %v", err)
	}
}

// Живой сервер в подкаталоге не должен попадать под «осиротевший» gzip свипа:
// иначе разметка обрывала бы вывод работающего дедика.
func TestSweepKeepsLiveScopedLog(t *testing.T) {
	root := t.TempDir()
	path := scopedPath(t, root, "game", "dev", "alive")
	write(t, path, "still writing\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Dir: root, SweepEvery: time.Second, Logf: t.Logf},
		func() []string { return []string{"alive"} })
	r.SweepOnce()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("свип сжал лог ЖИВОГО сервера в подкаталоге: %v", err)
	}
}

func TestStreamFindsScopedLog(t *testing.T) {
	root := t.TempDir()
	write(t, scopedPath(t, root, "game", "dev", "srv"), "hello from dedik\nline 2\n")

	var got strings.Builder
	if err := Stream(context.Background(), root, "srv", 0, false, func(b []byte) error {
		got.Write(b)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got.String() != "hello from dedik\nline 2\n" {
		t.Fatalf("tail из подкаталога вернул %q", got.String())
	}
}

// follow стартует ДО того, как дедик что-то написал: каталога с парой ещё нет,
// и путь обязан перерезолвиться, когда файл появится. Без этого live-tail
// молодого сервера следил бы за плоским путём, куда никто не пишет.
func TestStreamFollowResolvesScopedDirLater(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lines := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		done <- Stream(ctx, root, "late", 0, true, func(b []byte) error {
			lines <- string(b)
			return nil
		})
	}()

	time.Sleep(500 * time.Millisecond) // follow уже крутится на плоском пути
	path := scopedPath(t, root, "game", "dev", "late")
	write(t, path, "first line\n")

	select {
	case got := <-lines:
		if !strings.Contains(got, "first line") {
			t.Fatalf("follow вернул %q", got)
		}
	case <-ctx.Done():
		t.Fatal("follow не увидел лог, появившийся в размеченном каталоге")
	}
	cancel()
	<-done
}
