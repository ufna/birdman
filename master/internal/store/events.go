package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgconn"
)

// execer is satisfied by *pgxpool.Pool and pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type EventRef struct {
	NodeID    *string
	ServerID  *string
	MatchID   *string
	VersionID *string
}

func insertEvent(ctx context.Context, db execer, kind string, ref EventRef, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// project_id выводится ЗДЕСЬ, а не передаётся вызывающими (эпик #968 шаг 2).
	// Точка записи одна, вызывающих — два десятка, и любой новый вид события
	// иначе молча приезжал бы платформенным: забыть поле в структуре легко,
	// забыть его в общем INSERT'е невозможно.
	//
	// Источники те же и в том же порядке, что у бэкфилла миграции 000019:
	// ссылки на сущности (факт — project_id лежит в их собственной колонке),
	// затем слаг в payload (его несут события без ссылок вовсе: project_created,
	// environment_deleted, deploy_*, fleet_updated). Ничего не нашлось — NULL,
	// и это законное значение: платформенному событию проект не приписывается.
	//
	// РЯДОМ ПИШЕТСЯ СНИМОК СЛАГА (миграция 000020, tracker #1083). FK объявлен
	// `on delete set null`, поэтому удаление проекта обнуляет project_id у всей
	// его истории — и без снимка эта история становится НЕОТЛИЧИМА от
	// платформенной, а платформенное читается не скрывающе, то есть уезжает
	// соседнему арендатору целиком. Снимок ставится ЗДЕСЬ, а не каскадом
	// удаления: `set null` срабатывает всегда и не спрашивает, кто удалял, —
	// маркер из каскада обходится вторым путём удаления и порядком операций
	// внутри транзакции, а записанный в единственной точке записи не обходится.
	//
	// CTE, а не два одинаковых coalesce: выражение атрибуции остаётся ОДНО,
	// иначе две копии однажды разойдутся, и разойдутся молча.
	_, err = db.Exec(ctx, `
		with attr as (
		  select coalesce(
		    (select n.project_id from nodes    n where n.id = $2::uuid),
		    (select s.project_id from servers  s where s.id = $3::uuid),
		    (select v.project_id from versions v where v.id = $5::uuid),
		    (select m.project_id from matches  m where m.id = $4::uuid),
		    (select p.id from projects p where p.slug = ($6::jsonb->>'project'))
		  ) as project_id
		)
		insert into events (kind, node_id, server_id, match_id, version_id, payload, project_id, project_slug)
		select $1, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6::jsonb, attr.project_id,
		       (select p.slug from projects p where p.id = attr.project_id)
		from attr`,
		kind, ref.NodeID, ref.ServerID, ref.MatchID, ref.VersionID, string(raw))
	return err
}

// InsertEvent writes one audit/feed event (docs/specs/master.md §1 events).
func (s *Store) InsertEvent(ctx context.Context, kind string, ref EventRef, payload map[string]any) error {
	return insertEvent(ctx, s.Pool, kind, ref, payload)
}

// ListEvents returns the newest events, up to limit.
// ListEvents returns the newest events, up to limit. Пустой project — вся
// платформа (поведение до мультипроекта).
//
// Сужение НЕ СКРЫВАЮЩЕЕ, как у алертов (#955): уходит только событие ЯВНО
// чужого проекта, а платформенное (бекапы, CA, сессии) видно под любым
// фильтром. Спрятать его значило бы утверждать, что при выбранном проекте на
// платформе ничего не происходит.
//
// ПЛАТФОРМЕННОЕ — ЭТО «ПО РОЖДЕНИЮ», А НЕ «project_id is null» (tracker #1083).
// FK объявлен `on delete set null`, поэтому у истории удалённого проекта
// project_id обнуляется — и по одному лишь этому признаку она неотличима от
// бекапа с ротацией CA, то есть проезжает не скрывающее сужение легально и
// уезжает СОСЕДНЕМУ арендатору целиком, с hostname'ами нод и именами ключей.
// Различает их снимок слага (миграция 000020): у платформенного по рождению он
// пуст, у осиротевшего — остался.
//
// НЕСУЩЕЕ УСЛОВИЕ ЗДЕСЬ — ВТОРОЕ, и его одного было бы достаточно. Констрейнт
// `events_project_named` той же миграции (`check (project_id is null or
// project_slug is not null)`) даёт контрапозицию «`project_slug is null` ⇒
// `project_id is null`», поэтому строки, на которой конъюнкция и одиночное
// `e.project_slug is null` разошлись бы, в таблице существовать не может:
// вставка такой отбивается `SQLSTATE 23514`. Первое условие оставлено ради
// читаемости — оно называет вслух то, что схема держит молча — и как страховка
// на случай, если констрейнт когда-нибудь снимут: тогда несущим станет уже оно,
// потому что `project_id is null` в одиночку ловит и осиротевшее.
//
// Сужать по СЛАГУ вместо project_id нельзя, хотя выглядит дешевле: слаг
// переиспользуем (пересоздание проекта с тем же именем — штатный путь), и
// новый проект унаследовал бы ленту мёртвого.
func (s *Store) ListEvents(ctx context.Context, limit int, project string) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.Pool.Query(ctx, `
		select e.id, e.ts, e.kind, coalesce(p.slug, ''), e.node_id::text, e.server_id::text,
		       e.match_id::text, e.version_id::text, e.payload
		from events e left join projects p on p.id = e.project_id
		where $2 = '' or (e.project_id is null and e.project_slug is null)
		   or e.project_id = (select p.id from projects p where p.slug = $2)
		order by e.ts desc, e.id desc limit $1`, limit, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Project, &e.NodeID, &e.ServerID, &e.MatchID, &e.VersionID, &payload); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEventsAfter returns events with id > afterID that `project` may see,
// oldest first — the SSE stream cursor read (docs/specs/master.md §6). Пустой
// project — вся платформа (поведение до мультипроекта); сужение то же и НЕ
// СКРЫВАЮЩЕЕ, что у ListEvents (платформенное ПО РОЖДЕНИЮ видно под любым
// фильтром, история удалённого проекта — нет, tracker #1083), потому что это
// ВТОРОЙ ВХОД В ТУ ЖЕ ТАБЛИЦУ и расходиться правилам нельзя: иначе сужение
// ленты обходится подпиской на стрим (tracker #999).
//
// Второе возвращаемое значение — watermark: НАИБОЛЬШИЙ id, который эта выборка
// РАССМОТРЕЛА, а не наибольший из отданных. Стрим двигает курсор именно им.
// Без этого курсор привязанного ключа застревал бы на последнем СВОЁМ событии,
// и каждый односекундный опрос заново сканировал бы весь хвост, накопленный
// соседним проектом, — фильтр в SQL превращает «пусто» не в дешёвый ответ, а в
// скан до конца таблицы.
//
// ПОРЯДОК ДВУХ ЗАПРОСОВ ОБЯЗАТЕЛЕН: сперва граница окна, потом строки внутри
// неё. Возьми мы watermark ПОСЛЕ выборки, событие, вставленное между двумя
// запросами, попало бы в окно второго запроса, но не в выдачу первого — и
// клиент не увидел бы его НИКОГДА, потому что курсор уже ушёл вперёд.
//
// ЧЕГО ЭТОТ ПОРЯДОК НЕ ЛЕЧИТ (и не лечил старый курсор по отданной строке):
// `events.id` выдаётся в момент INSERT'а, а пишутся события внутри транзакций,
// поэтому событие с МЕНЬШИМ id может закоммититься ПОЗЖЕ уже отданного — и
// тогда оно не попадёт в стрим вовсе. Оба запроса ниже берут максимум ВИДИМОГО,
// а невидимое приезжает потом; свойство унаследованное и от привязки не зависит
// (tracker #1013).
//
// У WATERMARK'а ЕСТЬ ЦЕНА, и это не чистая оптимизация: курсор уезжает вперёд
// по строкам, которых клиент не видел, поэтому для ПРИВЯЗАННОГО ключа потеря из
// абзаца выше случается теперь и при ПУСТОЙ выдаче — незакоммиченное своё
// событие с меньшим id перепрыгивается чужим, уже видимым. Двигай мы курсор по
// отданным строкам, этот конкретный случай выжил бы, но каждый опрос молчащего
// проекта сканировал бы хвост соседа до конца таблицы. Выбран watermark;
// расширенный класс потери записан в #1013. Для НЕпривязанного ключа (панель,
// глобальный, admin) разницы нет вовсе: фильтр пуст, поэтому watermark равен id
// последней отданной строки, и поведение байт-в-байт прежнее.
func (s *Store) ListEventsAfter(ctx context.Context, afterID int64, limit int, project string) ([]Event, int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	watermark := afterID
	if err := s.Pool.QueryRow(ctx, `
		select coalesce(max(w.id), $1) from (
			select e.id from events e where e.id > $1 order by e.id limit $2
		) w`, afterID, limit).Scan(&watermark); err != nil {
		return nil, afterID, err
	}
	if watermark <= afterID { // ничего нового — за строками не ходим
		return nil, afterID, nil
	}
	rows, err := s.Pool.Query(ctx, `
		select e.id, e.ts, e.kind, coalesce(p.slug, ''), e.node_id::text, e.server_id::text,
		       e.match_id::text, e.version_id::text, e.payload
		from events e left join projects p on p.id = e.project_id
		where e.id > $1 and e.id <= $2
		  and ($3 = '' or (e.project_id is null and e.project_slug is null)
		       or e.project_id = (select p2.id from projects p2 where p2.slug = $3))
		order by e.id`, afterID, watermark, project)
	if err != nil {
		return nil, afterID, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Project, &e.NodeID, &e.ServerID, &e.MatchID, &e.VersionID, &payload); err != nil {
			return nil, afterID, err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, afterID, err
	}
	return out, watermark, nil
}

// MaxEventID returns the current top of the events feed (0 when empty) —
// the default SSE cursor: stream only what happens after connect. Сужать его
// нечем и незачем: он отдаёт ПОЗИЦИЮ, а не данные, а сам счётчик id и так виден
// привязанному ключу в id его собственных событий (и в /v1/events).
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&id)
	return id, err
}

// CountEvents is a small helper for tests and sanity checks.
func (s *Store) CountEvents(ctx context.Context, kind string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `select count(*) from events where kind = $1`, kind).Scan(&n)
	return n, err
}
