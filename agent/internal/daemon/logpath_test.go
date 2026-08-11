package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ufna/birdman/agent/internal/config"
)

// serverLogPath (tracker #994): пара (project, env) приезжает от master'а в
// env-мапе StartServer и становится КАТАЛОГОМ лога — оттуда её берёт vector и
// вешает на стрим в VictoriaLogs, а master сужает по этим лейблам запрос
// привязанного ключа. Здесь держится две вещи: пара действительно попадает в
// путь, и агент не даёт превратить её в путь куда угодно.
func TestServerLogPath(t *testing.T) {
	root := t.TempDir()
	m := &Manager{logDir: root, logf: func(string, ...any) {}, cfg: &config.Config{LogScopeDirs: true}}

	// Выключатель (config.LogScopeDirs, деф. false) — не украшение, а порядок
	// выката: бинарь агента обновляется сам, конфиг шиппера кладёт ansible.
	// Агент, начавший писать в подкаталоги раньше своего vector'а, перестал бы
	// шипповать логи флота ВООБЩЕ (старый glob `servers/*.log` их не видит).
	t.Run("выключено — прежний плоский путь", func(t *testing.T) {
		off := &Manager{logDir: root, logf: func(string, ...any) {}, cfg: &config.Config{}}
		got := off.serverLogPath("srv-0", map[string]string{"BIRDMAN_PROJECT": "game", "BIRDMAN_ENV": "prod"})
		if want := filepath.Join(root, "srv-0.log"); got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("пара едет в путь", func(t *testing.T) {
		got := m.serverLogPath("srv-1", map[string]string{"BIRDMAN_PROJECT": "game", "BIRDMAN_ENV": "prod"})
		want := filepath.Join(root, "game", "prod", "srv-1.log")
		if got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if _, err := os.Stat(filepath.Dir(want)); err != nil {
			t.Fatalf("каталог не создан: %v", err)
		}
	})

	// Старый master (пары нет) и run-once: лог обязан писаться по-прежнему, а
	// не пропасть. Он просто останется без лейблов — привязанный ключ его не
	// увидит, глобальный увидит. Это записанная в спеке цена решения.
	for _, c := range []struct {
		name string
		env  map[string]string
	}{
		{"пары нет вовсе", nil},
		{"только env", map[string]string{"BIRDMAN_ENV": "dev"}},
		{"только project", map[string]string{"BIRDMAN_PROJECT": "game"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, want := m.serverLogPath("srv-2", c.env), filepath.Join(root, "srv-2.log"); got != want {
				t.Fatalf("path = %q, want плоский %q", got, want)
			}
		})
	}

	// Пара приходит по сети. Она НЕ обязана быть безопасной — проверяется тем
	// же алфавитом, что и слаг на master'е; всё остальное падает в плоский
	// путь, а не наружу из дерева логов.
	for _, c := range []struct{ name, project, env string }{
		{"выход из дерева", "../../etc", "dev"},
		{"слеш в окружении", "game", "dev/../.."},
		{"абсолютный путь", "/etc", "dev"},
		{"точка", ".", "dev"},
		{"верхний регистр", "Game", "dev"},
		{"пробел", "game", "de v"},
		{"слишком длинное", "g012345678901234567890123456789012", "dev"},
	} {
		t.Run("санитайз: "+c.name, func(t *testing.T) {
			got := m.serverLogPath("srv-3", map[string]string{"BIRDMAN_PROJECT": c.project, "BIRDMAN_ENV": c.env})
			if want := filepath.Join(root, "srv-3.log"); got != want {
				t.Fatalf("path = %q, want плоский %q — непроверенная пара стала путём", got, want)
			}
		})
	}
}

// Кадры liba (структурные строки, которые дедик шлёт через SDK) обязаны
// попадать в ТОТ ЖЕ файл, что и вывод шима. Иначе лог одного дедика разорван на
// два файла: шимовская половина размечена парой и видна привязанному ключу,
// liba-половина лежит в плоском пути и не видна ни ему, ни live-tail'у
// (logrot.Stream резолвит размеченный каталог). Второй проход по этой карте
// показал, что инвариант держался ТОЛЬКО на порядке «containerd создаёт файл
// раньше, чем liba успевает подключиться» и не был закреплён ничем: мутация
// «писать кадры по плоскому пути» оставляла весь пакет зелёным.
func TestLibaFramesLandInTheShimLogFile(t *testing.T) {
	env := map[string]string{"BIRDMAN_PROJECT": "game", "BIRDMAN_ENV": "prod"}

	t.Run("сервер, запущенный этим агентом", func(t *testing.T) {
		root := t.TempDir()
		m := &Manager{logDir: root, logf: func(string, ...any) {}, cfg: &config.Config{LogScopeDirs: true}}
		srv := &server{id: "srv-live"}
		shimPath := m.serverLogPath(srv.id, env)
		srv.logPath = shimPath // как в handleStart
		if err := os.WriteFile(shimPath, []byte("stdout from dedik\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		m.appendLibaLog(srv, "info", "frame from liba")
		assertSingleLogFile(t, root, shimPath, "stdout from dedik", "frame from liba")
	})

	// После рестарта агента карта серверов восстанавливается из label'ов
	// контейнера, пары там нет — путь обязан вернуться резолвом по ФС, а не
	// свалиться в плоский.
	t.Run("сервер, восстановленный после рестарта агента", func(t *testing.T) {
		root := t.TempDir()
		m := &Manager{logDir: root, logf: func(string, ...any) {}, cfg: &config.Config{LogScopeDirs: true}}
		shimPath := m.serverLogPath("srv-restored", env)
		if err := os.WriteFile(shimPath, []byte("stdout from dedik\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		srv := &server{id: "srv-restored"} // logPath пуст: Restore его не знает
		m.appendLibaLog(srv, "info", "frame from liba")
		assertSingleLogFile(t, root, shimPath, "stdout from dedik", "frame from liba")
	})

	// Флаг выключен — обе половины по-прежнему в плоском файле.
	t.Run("разметка выключена", func(t *testing.T) {
		root := t.TempDir()
		m := &Manager{logDir: root, logf: func(string, ...any) {}, cfg: &config.Config{}}
		srv := &server{id: "srv-flat"}
		shimPath := m.serverLogPath(srv.id, env)
		srv.logPath = shimPath
		if err := os.WriteFile(shimPath, []byte("stdout from dedik\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		m.appendLibaLog(srv, "info", "frame from liba")
		assertSingleLogFile(t, root, shimPath, "stdout from dedik", "frame from liba")
	})
}

// assertSingleLogFile: в дереве логов ровно ОДИН .log, это want, и в нём обе
// строки. Проверяется именно единственность файла — «кадр попал куда надо» без
// неё зелёное и на разорванном логе.
func assertSingleLogFile(t *testing.T, root, want string, mustContain ...string) {
	t.Helper()
	var logs []string
	if err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(p, ".log") {
			logs = append(logs, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0] != want {
		t.Fatalf("логов в дереве: %v, want ровно [%s] — вывод дедика разорван на два пути", logs, want)
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range mustContain {
		if !strings.Contains(string(body), s) {
			t.Fatalf("в %s нет %q; файл = %q", want, s, body)
		}
	}
}
