package store_test

// Каскад удаления проекта и ПРИНАДЛЕЖНОСТЬ ПО СЛАГУ (находка ревью по #981,
// вторая половина — карточка #117).
//
// У проекта два способа быть ключом: uuid (`project_id` + FK, за него отвечает
// сама БД) и текстовый слаг в колонке `project` — за него не отвечает никто,
// FK там нет по построению. Строка со слагом переживает свой проект молча, и
// это не абстрактный риск: переименования слага нет by design, поэтому
// пересоздать проект под тем же именем — штатный путь, и новый арендатор
// получает чужое наследство.
//
// Три известные таблицы: match_stats_daily (закрыто в 57d2fec),
// match_ccu_daily (закрыто в 05405ee) и alert_mutes — здесь. Ниже два уровня
// проверки: точечный про мьюты и ОБЩИЙ инвариант, который сам перечисляет
// таблицы со слагом, чтобы четвёртая не проехала молча.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/utctime"
)

// muteAlertFor кладёт БЕССРОЧНЫЙ мьют alertname/eu на указанный проект (nil =
// «все проекты»). Бессрочность здесь несущая: именно она и делает строку
// переживающей проект — expires_at nullable с миграции 000004.
func muteAlertFor(t *testing.T, st *store.Store, alertname string, project *string) store.AlertMute {
	t.Helper()
	m, err := st.UpsertAlertMute(context.Background(), store.CreateAlertMuteParams{
		Alertname: alertname, Region: strptr("eu"), Project: project,
		Note: "maintenance", CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("upsert mute %s/%v: %v", alertname, project, err)
	}
	return m
}

// muteFor — muteAlertFor на NodeDown, самом обычном проектном алерте.
func muteFor(t *testing.T, st *store.Store, project *string) store.AlertMute {
	t.Helper()
	return muteAlertFor(t, st, "NodeDown", project)
}

// mutedNow отвечает ровно тем предикатом, которым /v1/alerts/* решает, гасить
// ли алерт: активные мьюты + AlertMute.Matches (httpapi/alerts.go).
func mutedNow(t *testing.T, st *store.Store, alertname, region, project string) bool {
	t.Helper()
	mutes, err := st.ListAlertMutes(context.Background(), false)
	if err != nil {
		t.Fatalf("list mutes: %v", err)
	}
	for _, m := range mutes {
		if m.Matches(alertname, region, project) {
			return true
		}
	}
	return false
}

// TestDeleteProjectClearsOwnAlertMutes: мьют, адресованный удаляемому проекту,
// уезжает вместе с ним — иначе он молча глушит алерты СЛЕДУЮЩЕГО арендатора
// того же слага (Matches сравнивает слаг строгим равенством).
//
// Три проверки идут вместе, потому что порознь каждая выполняется неверной
// правкой: свои мьюты ушли; мьют ЧУЖОГО проекта цел; мьют БЕЗ проекта
// («все проекты», хранится как NULL — 000018 запрещает `''` в этой роли) цел.
// Последний — не мелочь: снести его каскадом одного арендатора значило бы
// снять подавление платформенного сигнала для всех.
func TestDeleteProjectClearsOwnAlertMutes(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	doomed := muteFor(t, st, strptr(f.Project))
	muteFor(t, st, strptr("neighbour"))
	// Мьют БЕЗ проекта — на ДРУГОМ alertname намеренно: на том же он покрыл бы
	// и пересозданный проект (это его смысл — «все проекты»), и проверка «слаг
	// больше не заглушён» стала бы неотличима от «мьют без проекта работает».
	muteAlertFor(t, st, "MasterDown", nil)

	// Истёкший мьют того же проекта: он не активен, но строка его — тоже его,
	// и на неё смотрит панель по ?all=1. Каскад обязан унести и её.
	past := time.Now().Add(-time.Hour)
	if _, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "DiskHigh", Region: strptr("eu"), Project: strptr(f.Project),
		ExpiresAt: &past, CreatedBy: "admin",
	}); err != nil {
		t.Fatalf("seed expired mute: %v", err)
	}

	if !mutedNow(t, st, "NodeDown", "eu", f.Project) {
		t.Fatal("контроль: до удаления проект обязан быть заглушён — иначе тест ниже проверяет пустоту")
	}

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	res, err := st.DeleteProject(ctx, f.Project, f.Project)
	if err != nil {
		t.Fatalf("delete project: %v", err)
	}

	// Пересоздаём проект под тем же слагом — ровно тот путь, на котором утечка
	// и видна.
	if _, err := st.CreateProject(ctx, f.Project, 10); err != nil {
		t.Fatalf("recreate project: %v", err)
	}
	if mutedNow(t, st, "NodeDown", "eu", f.Project) {
		t.Errorf("новый проект с тем же слагом унаследовал мьют мёртвого — его алерты глушатся молча")
	}
	if !mutedNow(t, st, "NodeDown", "eu", "neighbour") {
		t.Errorf("снесён мьют ЧУЖОГО проекта: у соседа подавление пропало без его ведома")
	}
	if !mutedNow(t, st, "MasterDown", "eu", "") {
		t.Errorf("снесён мьют БЕЗ проекта: он платформенный, удаляемому арендатору не принадлежит")
	}

	// Ни одной строки под этим слагом, включая истёкшую (её видно по ?all=1).
	all, err := st.ListAlertMutes(ctx, true)
	if err != nil {
		t.Fatalf("list all mutes: %v", err)
	}
	for _, m := range all {
		if m.Project != nil && *m.Project == f.Project {
			t.Errorf("строка мьюта пережила свой проект: %+v", m)
		}
	}
	if len(all) != 2 {
		t.Errorf("остаться должны ровно два мьюта (сосед + платформенный), got %d: %+v", len(all), all)
	}

	// Отчёт каскада несёт снятые строки целиком — по ним httpapi снимает
	// зеркальный alertmanager-silence, а без silence_id снять его нечем.
	if res.AlertMutes != 2 {
		t.Errorf("каскад обязан отчитаться о ДВУХ снятых мьютах (активном и истёкшем), got %d", res.AlertMutes)
	}
	if len(res.RemovedMutes) != res.AlertMutes {
		t.Errorf("счётчик и строки разошлись: %d против %d", res.AlertMutes, len(res.RemovedMutes))
	}
	var sawDoomed bool
	for _, m := range res.RemovedMutes {
		if m.ID == doomed.ID {
			sawDoomed = true
		}
		if m.Project == nil || *m.Project != f.Project {
			t.Errorf("в отчёт попала чужая строка: %+v", m)
		}
	}
	if !sawDoomed {
		t.Errorf("снятый мьют %s не попал в отчёт — снимать зеркальный silence будет нечем", doomed.ID)
	}

	// Снятие мьюта — аудируемое действие на ручном пути; каскад не имеет права
	// быть тише.
	n, err := st.CountEvents(ctx, store.EventAlertUnmuted)
	if err != nil {
		t.Fatalf("count alert_unmuted: %v", err)
	}
	if n != 2 {
		t.Errorf("каскад снял мьюты молча: событий alert_unmuted %d, ждём 2", n)
	}
}

// slugSeeders — реестр таблиц, у которых есть колонка `project` (текстовый
// слаг), и способ посеять в каждую строку заданного проекта.
//
// Реестр обязателен для теста ниже: таблица, перечисленная схемой, но не
// найденная здесь, роняет его. Смысл не в удобстве, а в том, что перечислить
// таблицу и НЕ посеять в неё строку — это зелёный тест, ничего не
// проверяющий; поэтому «перечислено» и «проверено» связаны жёстко.
var slugSeeders = map[string]func(t *testing.T, st *store.Store, project string){
	"match_stats_daily": func(t *testing.T, st *store.Store, project string) {
		seedRollup(t, st, project, "dev", "1.0.0")
	},
	"match_ccu_daily": func(t *testing.T, st *store.Store, project string) {
		seedCCU(t, st, utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -10)), project, 42)
	},
	"alert_mutes": func(t *testing.T, st *store.Store, project string) {
		muteFor(t, st, strptr(project))
	},
}

// slugTables перечисляет по схеме таблицы с колонкой `project` — то есть те,
// где проект держится СЛАГОМ и FK его не стережёт.
func slugTables(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `
		select table_name from information_schema.columns
		 where table_schema = 'public' and column_name = 'project'
		 order by table_name`)
	if err != nil {
		t.Fatalf("list slug tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list slug tables: %v", err)
	}
	return out
}

func slugRows(t *testing.T, st *store.Store, table, project string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`select count(*) from `+pgx.Identifier{table}.Sanitize()+` where project = $1`, project).Scan(&n); err != nil {
		t.Fatalf("count rows of %s in %s: %v", project, table, err)
	}
	return n
}

// TestDeleteProjectLeavesNoRowsUnderItsSlug — ОБЩИЙ инвариант: после
// DeleteProject ни в одной таблице не осталось строк с этим слагом, и ни в
// одной не пострадали строки соседа.
//
// Он не дублирует точечные тесты выше, он закрывает другую дыру. Точечный тест
// пишут вместе с таблицей — а проблема ровно в том, что его НЕ пишут: три
// таблицы получили колонку `project` в три приёма, и каждый раз каскад
// вспоминали задним числом, по ревью. Здесь список таблиц берётся из СХЕМЫ, а
// не из памяти автора, поэтому четвёртая таблица со слагом обязана либо
// приехать с уборкой в каскаде, либо явно объявить в slugSeeders, что слаг
// обязан её пережить, — молча она не проедет.
//
// Граница честная: инвариант видит колонку, НАЗВАННУЮ `project`. Слаг под
// другим именем или внутри jsonb он не увидит — например, events.payload
// хранит слаг и переживает проект НАМЕРЕННО (аудит append-only, миграция
// 000019), и такую строку этот тест трогать и не должен.
func TestDeleteProjectLeavesNoRowsUnderItsSlug(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}

	tables := slugTables(t, st)
	if len(tables) == 0 {
		t.Fatal("схема не отдала ни одной таблицы с колонкой `project` — тест проверял бы пустоту")
	}
	for _, table := range tables {
		seed, ok := slugSeeders[table]
		if !ok {
			t.Fatalf("таблица %q завела колонку `project`, но сидера в slugSeeders нет: "+
				"либо почисти её в DeleteProject и добавь сидер, либо запиши здесь, почему слаг обязан её пережить", table)
		}
		seed(t, st, f.Project)
		seed(t, st, "neighbour")
	}
	// Контроль ДО удаления: если строк не посеялось, всё, что ниже, зелено впустую.
	for _, table := range tables {
		if n := slugRows(t, st, table, f.Project); n == 0 {
			t.Fatalf("контроль: в %s не посеялось ни одной строки проекта — проверка ниже была бы пустой", table)
		}
		if n := slugRows(t, st, table, "neighbour"); n == 0 {
			t.Fatalf("контроль: в %s не посеялось ни одной строки соседа", table)
		}
	}

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	for _, table := range tables {
		if n := slugRows(t, st, table, f.Project); n != 0 {
			t.Errorf("%s: %d строк пережили свой проект — их унаследует новый проект с тем же слагом", table, n)
		}
		if n := slugRows(t, st, table, "neighbour"); n == 0 {
			t.Errorf("%s: каскад одного арендатора снёс строки ЧУЖОГО проекта", table)
		}
	}
}
