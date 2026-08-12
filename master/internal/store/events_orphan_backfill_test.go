package store_test

// tracker #1091: строки, осиротевшие ДО миграции 000020, доатрибутируются по
// слагу в payload — и от этого становятся МЕНЕЕ видимыми, а не более.
//
// 000020 забэкфиллила снимок слага только для ЖИВЫХ строк, поэтому история
// проектов, удалённых раньше неё, осталась с пустым снимком, то есть
// неотличимой от платформенной ПО РОЖДЕНИЮ, — и продолжала уезжать привязанному
// ключу любого соседа. Сама 000020 записала это как «восстанавливать не из
// чего»; у целого класса видов имя владельца лежит в payload, и 000021 его
// оттуда достаёт.
//
// Проверяется СЛЕДСТВИЕ (кто что видит), а не «колонка заполнилась»: ассерт про
// колонку пережил бы откат условия в читателях, а про выдачу — нет. Оба входа в
// таблицу (лента + выборка стрима) проверяются вместе — правило у них общее и
// разойтись ему нельзя (#999).
//
// Гоняется САМ файл миграции, а не его копия в тесте (идиома соседних
// stepDownToV16/V17/V18): копия ловит только собственную мутацию, а шиппится
// другой файл.

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// eventSlugSet отвечает, ЗАПОЛНЕН ли снимок слага, отличая NULL от пустой
// строки, — чего eventSlug не умеет (он схлопывает оба в ""). Различение
// несущее: снимок, равный пустой строке, это не «нет снимка», а «есть, и он
// пустой», и такая строка уже прячется от арендаторов.
func eventSlugSet(t *testing.T, st *store.Store, kind string) bool {
	t.Helper()
	var slug *string
	err := st.Pool.QueryRow(context.Background(),
		`select project_slug from events where kind = $1 order by id desc limit 1`, kind).Scan(&slug)
	if err != nil {
		t.Fatalf("read project_slug of %s: %v", kind, err)
	}
	return slug != nil
}

// TestOrphanSlugBackfillHidesRecoverableOrphans — приёмка карточки. Строка,
// осиротевшая до 000020 и несущая слаг в payload, после миграции не видна
// привязанному ключу другого проекта; вместе с ней проверяются оба соседних
// инварианта, потому что чинить один за счёт другого здесь легко: платформенное
// ПО РОЖДЕНИЮ событие под фильтром обязано остаться видимым (сужение не
// скрывающее), а аудит — целым (непривязанное чтение видит всё).
func TestOrphanSlugBackfillHidesRecoverableOrphans(t *testing.T) {
	st, m := stepDownToV20(t, func(st *store.Store) {
		testdb.Seed(t, st, "eu", 8) // проект game + нода + версия, события со снимком
		if _, err := st.CreateProject(context.Background(), "neighbour", 2); err != nil {
			t.Fatalf("create neighbour: %v", err)
		}
	})
	ctx := context.Background()

	seedLegacy := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Осиротевшая до 000020 строка удалённого проекта: project_id уже обнулён
	// каскадом, снимка нет (колонки на момент удаления не существовало), слаг
	// владельца остался в payload. Ровно та строка, ради которой карточка.
	seedLegacy(`insert into events (kind, payload)
	            values ('legacy_orphan_dead', '{"project": "ghost", "image_ref": "ghcr.io/example/ghost:1.0.0"}'::jsonb)`)
	// Слаг, которого не существовало НИКОГДА (опечатка, чужое имя): единственный
	// ложноположительный случай, и он ПРИНЯТ. Строка перестаёт быть платформенной,
	// хотя платформенной она и есть, — ошибка идёт в сторону сокрытия от
	// арендатора, аудит непривязанного чтения её сохраняет. Ассерт закрепляет
	// именно принятое решение, а не «так вышло».
	seedLegacy(`insert into events (kind, payload)
	            values ('legacy_unknown_slug', '{"project": "nope"}'::jsonb)`)
	// Слаг ЖИВОГО проекта: проект с этим именем был удалён и пересоздан. Снимок
	// НЕ адресует проект, поэтому доатрибуция обязана спрятать строку и от него
	// тоже — иначе починка одной утечки открыла бы другую.
	seedLegacy(`insert into events (kind, payload)
	            values ('legacy_orphan_reborn', '{"project": "game"}'::jsonb)`)
	// Платформенное ПО РОЖДЕНИЮ (бекап, CA, сессия): слага в payload нет вовсе,
	// снимок обязан остаться пустым, видимость — прежней.
	seedLegacy(`insert into events (kind) values ('legacy_platform_born')`)
	// Слаг ПУСТОЙ строкой: так писал allocation_failed до гейта привязки.
	// Пустой снимок спрятал бы платформенную по существу строку от всех.
	seedLegacy(`insert into events (kind, payload)
	            values ('legacy_empty_slug', '{"project": ""}'::jsonb)`)
	// Слаг JSON-null'ом: так пишет платформенный alert_muted (поле — указатель).
	seedLegacy(`insert into events (kind, payload)
	            values ('legacy_null_slug', '{"project": null}'::jsonb)`)
	// Строка, осиротевшая ШТАТНО уже ПОСЛЕ 000020: снимок у неё честный, а payload
	// называет ДРУГОЙ проект. Бэкфилл обязан пройти мимо — иначе он переписал бы
	// снимок, поставленный точкой записи, догадкой из payload.
	seedLegacy(`insert into events (kind, project_slug, payload)
	            values ('legacy_orphan_snapshotted', 'already_dead', '{"project": "someone_else"}'::jsonb)`)

	applyOrphanSlugMigration(t, m)

	// --- снимок проставлен ровно там, где должен ---
	if got := eventSlug(t, st, "legacy_orphan_dead"); got != "ghost" {
		t.Errorf("осиротевшая строка не доатрибутирована: снимок %q, want %q", got, "ghost")
	}
	if got := eventSlug(t, st, "legacy_unknown_slug"); got != "nope" {
		t.Errorf("строка с несуществующим слагом: снимок %q, want %q (принятый ложноположительный)", got, "nope")
	}
	if got := eventSlug(t, st, "legacy_orphan_reborn"); got != "game" {
		t.Errorf("осиротевшая строка переиспользованного слага не доатрибутирована: снимок %q", got)
	}
	if got := eventSlug(t, st, "legacy_orphan_snapshotted"); got != "already_dead" {
		t.Errorf("бэкфилл переписал честный снимок штатно осиротевшей строки: got %q, want %q", got, "already_dead")
	}
	for _, kind := range []string{"legacy_platform_born", "legacy_empty_slug", "legacy_null_slug"} {
		if eventSlugSet(t, st, kind) {
			t.Errorf("%s: платформенной по рождению строке проставлен снимок %q — бэкфилл забрал лишнее",
				kind, eventSlug(t, st, kind))
		}
	}

	// --- project_id не тронут: доатрибуция даёт ИМЯ, а не ссылку ---
	for _, kind := range []string{
		"legacy_orphan_dead", "legacy_unknown_slug", "legacy_orphan_reborn",
		"legacy_platform_born", "legacy_empty_slug", "legacy_null_slug",
		"legacy_orphan_snapshotted",
	} {
		if got := eventProject(t, st, kind); got != "" {
			t.Errorf("%s: миграция приписала строке проект по ссылке (%q) — сужение поехало бы по id", kind, got)
		}
	}

	// --- следствие: кто что видит ---
	nb := seenBy(t, st, "neighbour")
	hiddenFrom(t, nb, "neighbour", "legacy_orphan_dead",
		"история удалённого проекта уехала соседу")
	hiddenFrom(t, nb, "neighbour", "legacy_unknown_slug",
		"строка с несуществующим слагом уехала соседу")
	hiddenFrom(t, nb, "neighbour", "legacy_orphan_reborn",
		"история мёртвого проекта уехала соседу")
	hiddenFrom(t, nb, "neighbour", "legacy_orphan_snapshotted",
		"штатно осиротевшая строка перестала прятаться")
	visibleTo(t, nb, "neighbour", "legacy_platform_born",
		"платформенное по рождению событие скрыто — сужение обязано остаться не скрывающим")
	visibleTo(t, nb, "neighbour", "legacy_empty_slug",
		"строка с пустым слагом в payload спрятана — бэкфилл забрал лишнее")
	visibleTo(t, nb, "neighbour", "legacy_null_slug",
		"строка с JSON-null слагом спрятана — бэкфилл забрал лишнее")

	// Живой проект того же слага: наследования быть не должно.
	game := seenBy(t, st, "game")
	hiddenFrom(t, game, "game", "legacy_orphan_reborn",
		"пересозданный под тем же слагом проект унаследовал ленту мёртвого")
	hiddenFrom(t, game, "game", "legacy_orphan_dead",
		"история чужого удалённого проекта видна живому проекту")
	// Контроль: собственные события живого проекта на месте — иначе ассерты выше
	// проходили бы и на выборке, которая просто ничего не отдаёт.
	visibleTo(t, game, "game", store.EventNodeCreated,
		"собственное событие живого проекта пропало из его ленты")

	// Аудит не стёрт: непривязанное чтение (панель, admin, глобальный) видит всё.
	all := seenBy(t, st, "")
	for _, kind := range []string{"legacy_orphan_dead", "legacy_unknown_slug", "legacy_orphan_reborn"} {
		visibleTo(t, all, "", kind, "доатрибутированная строка пропала из платформенного чтения")
	}
}

// TestOrphanSlugBackfillLeavesLiveRowsAlone: у строки живого проекта снимок уже
// стоит с 000020, и 000021 не имеет права его тронуть — ни переписать из
// payload, ни обнулить. Без этого пина бэкфилл, забывший `project_id is null`,
// молча переписал бы снимок живой строки слагом из payload, и разошлись бы
// ссылка и имя — то есть сломался бы сам смысл констрейнта events_project_named.
func TestOrphanSlugBackfillLeavesLiveRowsAlone(t *testing.T) {
	var live string
	st, m := stepDownToV20(t, func(st *store.Store) {
		f := testdb.Seed(t, st, "eu", 8)
		live = projectID(t, st, f.Project)
	})
	ctx := context.Background()

	// Живая строка, у которой payload называет ДРУГОЙ проект: если бэкфилл
	// возьмёт её, снимок разъедется со ссылкой, и это будет видно.
	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, project_id, project_slug, payload)
		 values ('live_event', $1::uuid, 'game', '{"project": "someone_else"}'::jsonb)`,
		live); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	applyOrphanSlugMigration(t, m)

	if got := eventProject(t, st, "live_event"); got != live {
		t.Errorf("миграция сдвинула ссылку живой строки: got %q, want %q", got, live)
	}
	if got := eventSlug(t, st, "live_event"); got != "game" {
		t.Errorf("миграция переписала снимок живой строки: got %q, want %q", got, "game")
	}
	seen := seenBy(t, st, "game")
	visibleTo(t, seen, "game", "live_event", "живое событие пропало из ленты своего проекта")
}

// stepDownToV20 поднимает свежую БД (мигрированную на head), даёт вызывающему
// засеять граф сущностей ЧЕРЕЗ STORE, пока схема ещё head, и только потом
// шагает схему ВНИЗ до v20 — форма ДО доатрибуции осиротевших строк.
//
// Колбэк, а не «сначала шаг вниз, потом сидинг», — та же вынужденная форма, что
// у stepDownToV18: сидинг через стор пишет события, и делать это надо на схеме,
// про которую код собран. Исторические строки после шага вниз сеются сырым SQL:
// на v20 колонка project_slug уже есть, но у осиротевших строк она пуста — ровно
// то состояние, которое чинит 000021.
//
// Шаг вниз проверяется по ВЕРСИИ, а не по форме схемы, и это не мелочь: 000021
// схему не меняет, поэтому «колонки нет» тут спросить не о чем, а вот
// незасчитанный шаг вниз сделал бы Migrate(21) пустышкой (ErrNoChange) — и
// ассерты прошли бы, не прогнав миграцию ни разу.
func stepDownToV20(t *testing.T, seed func(*store.Store)) (*store.Store, *migrate.Migrate) {
	t.Helper()
	st, dsn := testdb.NewWithCodec(t, codecFilled(t, 0x21))
	if seed != nil {
		seed(st)
	}
	m := newMigrateHandle(t, stripPoolParams(t, dsn))
	if err := m.Migrate(20); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("step down to v20: %v", err)
	}
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version after step down: %v", err)
	}
	if dirty || v != 20 {
		t.Fatalf("после шага вниз ожидалась чистая версия 20, got %d (dirty=%v)", v, dirty)
	}
	return st, m
}

// applyOrphanSlugMigration прогоняет НАСТОЯЩИЙ
// 000021_events_orphan_slug_backfill.up.sql — то, что и шиппится, а не копию SQL
// в тесте (карточка #1080 заведена ровно об этом).
func applyOrphanSlugMigration(t *testing.T, m *migrate.Migrate) {
	t.Helper()
	if err := m.Migrate(21); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply 000021: %v", err)
	}
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version after migrate up: %v", err)
	}
	if dirty || v != 21 {
		t.Fatalf("после наката ожидалась чистая версия 21, got %d (dirty=%v)", v, dirty)
	}
}
