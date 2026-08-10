package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Проектное измерение mute'ов (мультипроект, миграция 000018, tracker #957).

// Матчер разводит ТРИ случая, и третий — несущий: проектный mute НЕ глушит
// платформенный алерт (у того project пустой). Иначе оператор проекта А,
// заглушивший MasterDown «у себя», уронил бы сигнал и проекту Б — а выглядело
// бы это как «мьют работает».
func TestAlertMuteMatchesProject(t *testing.T) {
	all := store.AlertMute{Alertname: "NodeDown"}                             // ни региона, ни проекта
	alpha := store.AlertMute{Alertname: "NodeDown", Project: strptr("alpha")} // только проект alpha
	alphaEU := store.AlertMute{Alertname: "NodeDown", Project: strptr("alpha"), Region: strptr("eu")}

	cases := []struct {
		name            string
		mute            store.AlertMute
		region, project string
		want            bool
	}{
		{"mute без проекта кроет проектный алерт", all, "eu", "alpha", true},
		{"mute без проекта кроет платформенный алерт", all, "eu", "", true},
		{"проектный mute кроет свой проект", alpha, "eu", "alpha", true},
		{"проектный mute НЕ кроет чужой проект", alpha, "eu", "beta", false},
		// Тот самый капкан: пустой project — это платформенный алерт, а не
		// «любой». Пропусти его здесь — и проектная заглушка получит власть над
		// общим сигналом.
		{"проектный mute НЕ кроет ПЛАТФОРМЕННЫЙ алерт", alpha, "eu", "", false},
		{"обе оси совпали", alphaEU, "eu", "alpha", true},
		{"проект совпал, регион нет", alphaEU, "us", "alpha", false},
		{"регион совпал, проект нет", alphaEU, "eu", "beta", false},
	}
	for _, c := range cases {
		if got := c.mute.Matches("NodeDown", c.region, c.project); got != c.want {
			t.Errorf("%s: Matches(NodeDown, %q, %q) = %v, want %v", c.name, c.region, c.project, got, c.want)
		}
	}
	// Имя алерта всё так же решает первым — иначе таблица выше проверяла бы
	// проект у мьюта, который вообще не про этот алерт.
	if alpha.Matches("DiskHigh", "eu", "alpha") {
		t.Error("чужой alertname не должен матчиться ни при каком проекте")
	}
}

// Цель upsert'а — ТРОЙКА: (NodeDown, eu, alpha), (NodeDown, eu, beta) и
// (NodeDown, eu, все проекты) — три разные строки, а повтор тройки обновляет
// свою на месте. Иначе mute проекта Б перезаписывал бы mute проекта А.
func TestAlertMutesProjectIsDistinctTarget(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()

	mk := func(project *string, note string) store.AlertMute {
		t.Helper()
		m, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
			Alertname: "NodeDown", Region: strptr("eu"), Project: project, Note: note, CreatedBy: "admin",
		})
		if err != nil {
			t.Fatalf("upsert %v: %v", project, err)
		}
		return m
	}

	alpha := mk(strptr("alpha"), "alpha maint")
	beta := mk(strptr("beta"), "beta maint")
	global := mk(nil, "all projects")
	if alpha.ID == beta.ID || alpha.ID == global.ID || beta.ID == global.ID {
		t.Fatalf("три разные цели должны дать три строки: %s %s %s", alpha.ID, beta.ID, global.ID)
	}
	if alpha.Project == nil || *alpha.Project != "alpha" || global.Project != nil {
		t.Fatalf("project не доехал до строки: %+v / %+v", alpha, global)
	}

	// Повтор той же тройки — та же строка, обновлён note (дедуп, как у региона).
	if again := mk(strptr("alpha"), "extended"); again.ID != alpha.ID || again.Note != "extended" {
		t.Fatalf("повтор тройки должен обновить строку на месте: %+v", again)
	}

	// Пустой/пробельный project — это «все проекты», а не отдельная цель:
	// иначе `"project":""` из панели плодил бы строки-двойники глобального mute.
	if blank := mk(strptr("   "), "blank"); blank.ID != global.ID || blank.Project != nil {
		t.Fatalf("пробельный project должен схлопнуться в «все проекты»: %+v", blank)
	}

	mutes, err := st.ListAlertMutes(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(mutes) != 3 {
		t.Fatalf("активных мьютов должно остаться 3, got %d: %+v", len(mutes), mutes)
	}
}

// Миграция 000018 вверх и вниз на НЕПУСТОЙ таблице (идиома stepDownToV16 из
// stats_project_test.go). Вверх: mute эры без проекта доезжает с NULL, то есть
// продолжает глушить все проекты. Вниз: проектные мьюты не удаляются и не
// роняют вставку (уникального ключа на цель нет — схлопывать, в отличие от
// 000017, нечего), а РАСШИРЯЮТСЯ до «все проекты» — доv0-семантика.
func TestAlertMutesProjectMigration000018UpDown(t *testing.T) {
	st, m := stepDownToV17(t)
	ctx := context.Background()
	pool := st.Pool

	// Строка в форме ДО миграции (колонки project ещё нет).
	if _, err := pool.Exec(ctx,
		`insert into alert_mutes (alertname, region, note, created_by) values ('NodeDown', 'eu', 'v0 mute', 'admin')`,
	); err != nil {
		t.Fatalf("seed pre-migration mute: %v", err)
	}

	if err := m.Migrate(18); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply 000018: %v", err)
	}

	mutes, err := st.ListAlertMutes(ctx, false)
	if err != nil {
		t.Fatalf("list after up: %v", err)
	}
	if len(mutes) != 1 || mutes[0].Project != nil {
		t.Fatalf("mute эры без проекта обязан приехать с NULL («все проекты»): %+v", mutes)
	}
	if !mutes[0].Matches("NodeDown", "eu", "alpha") || !mutes[0].Matches("NodeDown", "eu", "") {
		t.Fatalf("мигрированный mute обязан крыть и проектный, и платформенный алерт: %+v", mutes[0])
	}

	// Два проектных мьюта на ту же (alertname, region) — ровно та пара строк,
	// на которой наивный откат («просто снять колонку с уникальным ключом»)
	// и ломался бы.
	for _, p := range []string{"alpha", "beta"} {
		if _, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
			Alertname: "NodeDown", Region: strptr("eu"), Project: strptr(p), CreatedBy: "admin",
		}); err != nil {
			t.Fatalf("seed project mute %s: %v", p, err)
		}
	}

	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("down 000018: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from alert_mutes`).Scan(&rows); err != nil {
		t.Fatalf("count after down: %v", err)
	}
	if rows != 3 {
		t.Fatalf("откат не должен терять мьюты (подавление снялось бы молча), want 3, got %d", rows)
	}
	var hasProject int
	if err := pool.QueryRow(ctx,
		`select count(*) from information_schema.columns
		  where table_name = 'alert_mutes' and column_name = 'project'`).Scan(&hasProject); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if hasProject != 0 {
		t.Fatalf("после отката колонки project быть не должно")
	}

	// И схема снова рабочая: upsert эры без проекта берёт активную строку цели
	// (order by created_at desc limit 1) и не спотыкается о дубли.
	if err := m.Migrate(18); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("re-apply 000018: %v", err)
	}
	if _, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", Region: strptr("eu"), Note: "after roundtrip", CreatedBy: "admin",
	}); err != nil {
		t.Fatalf("upsert after down→up roundtrip: %v", err)
	}
}

// stepDownToV17 provisions a fresh DB (migrated to head) and steps the schema
// DOWN to v17 — форма до проектного измерения мьютов.
func stepDownToV17(t *testing.T) (*store.Store, *migrate.Migrate) {
	t.Helper()
	st, dsn := testdb.NewWithCodec(t, codecFilled(t, 0x18))
	m := newMigrateHandle(t, stripPoolParams(t, dsn))
	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("step down to v17: %v", err)
	}
	return st, m
}
