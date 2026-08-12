package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// SSE live feed for the panel (docs/specs/master.md §6, panel.md §1):
// GET /v1/events/stream, scope readonly+. New `events` rows are streamed as
//
//	id: <event id>
//	event: <kind>
//	data: <event json, same shape as GET /v1/events items>
//
// The cursor is the events.id sequence, polled from Postgres every
// ssePollInterval. By default the stream starts at the current head (only
// events that happen after connect); `?after_id=N` or a Last-Event-ID header
// (sent by EventSource on reconnect) resumes after N. A comment line is
// written every sseKeepAlive so proxies and clients see a live connection.
//
// Стрим — ВТОРОЙ ВХОД В ТУ ЖЕ ТАБЛИЦУ, что `GET /v1/events`, поэтому с #999 он
// сужается ТЕМ ЖЕ tenantScope (#993) и с тем же не-скрывающим правилом. До
// #999 фильтра тут не было ВООБЩЕ — не «был и не энфорсил», а не существовало
// параметра, — и привязанный readonly-ключ получал живую ленту всей платформы с
// payload'ами (hostname нод, image_ref версий): сужение `/v1/events` обходилось
// подпиской на стрим.
const (
	sseKeepAlive  = 15 * time.Second
	sseBatchLimit = 500

	// sseSentCap — потолок памяти на «уже отдано» (см. sseSettle). Пробить его
	// может только лента, генерящая тысячи событий за sseSettle; в этом случае
	// защита от гонки отключается для соединения (курсор снова прыгает на
	// границу окна) — терять память хуже, чем терять узкую гонку.
	sseSentCap = 5000
)

// ssePollInterval и sseSettle — ЕДИНСТВЕННЫЕ на пути стрима ожидания РЕАЛЬНОГО
// времени, и поэтому они переменные, а не константы (tracker #1016).
//
// Тест SSE не может их пересидеть быстрее, чем они идут: пакет
// `internal/httpapi` — 150 тестов, из них дюжина держит живую ленту, и каждая
// такая проверка стоит несколько периодов опроса, а догон курсора — ещё и
// sseSettle на КАЖДОЕ окно (пачка в 1200 событий при sseBatchLimit=500 — это
// три окна, то есть три отсрочки подряд). На CI пакет от этого упёрся в
// дефолтный 600-секундный таймаут `go test`. Значения подменяются на короткие
// ОДИН РАЗ на тестовый бинарь, в init() файла export_test.go — то есть до
// старта любого теста и любой горутины стрима (менять их между тестами нельзя:
// хендлер ещё живой ленты читал бы их одновременно с записью, и это была бы
// честная гонка).
//
// Подмена НЕ ослабляет проверки: и «отзыв закрывает ленту», и «запоздавший
// коммит не потерян», и «окно не двоится при отстающем курсоре» — свойства
// ПОРЯДКА, а не длительности. Тест, которому нужен срок, соотнесённый с
// отсрочкой курсора, обязан выводить его ИЗ sseSettle (SSESettleForTest), а не
// вписывать секунды числом, иначе подмена его расклеит.
var (
	ssePollInterval = time.Second

	// sseSettle — сколько курсор ЖДЁТ, прежде чем перешагнуть уже увиденную
	// границу окна (tracker #1013). `events.id` выдаётся в момент INSERT'а, а
	// события пишутся ВНУТРИ транзакций, поэтому событие с МЕНЬШИМ id может
	// стать видимым ПОЗЖЕ уже отданного: транзакция A взяла id 6 и держит
	// коммит, транзакция B взяла 7 и закоммитилась — опрос видит 7, не видит 6
	// и, двигая курсор на 7, теряет 6 НАВСЕГДА (следующий опрос спрашивает
	// `id > 7`).
	//
	// Отсрочка отсчитывается от МОМЕНТА НАБЛЮДЕНИЯ, а не от `events.ts`, и это
	// принципиально: `ts` — это `now()`, то есть время НАЧАЛА транзакции, оно
	// ничего не говорит о том, когда строка стала видимой. Наблюдение же даёт
	// прямое рассуждение: если граница hi видна нам в момент T, то любая строка
	// с id < hi была вставлена ДО T, значит её транзакция была открыта уже в T;
	// увидев её позже T+sseSettle, мы знаем, что транзакция висела дольше
	// sseSettle. Отсюда гарантия и её честная граница: НЕ теряется ничего,
	// закоммиченное в пределах sseSettle после того, как более новое событие
	// стало видимым; транзакция, висящая дольше, по-прежнему теряется — но у
	// master'а все пишущие транзакции короткие (миллисекунды).
	//
	// Задержки живой ленте это НЕ добавляет: строки отдаются сразу, как только
	// видны; ждёт только курсор. Цена — повторное чтение окна шириной в
	// sseSettle на каждом опросе (индексный скан по id, десятки строк) и память
	// на id, уже отданные выше курсора (sseSentCap).
	sseSettle = 5 * time.Second
)

// seenWindow — граница окна опроса и момент, когда мы её УВИДЕЛИ (tracker
// #1013). Курсор перешагивает границу не раньше, чем через sseSettle после
// наблюдения; отсчёт именно от наблюдения, а не от `events.ts` (то есть от
// начала транзакции), — см. комментарий у sseSettle.
type seenWindow struct {
	at time.Time
	id int64
}

func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	// ГЕЙТ ПЕРВЫМ — раньше разбора `?after_id=` и раньше любой записи в ответ:
	// правило «гейт настолько рано, насколько позволяет адресация» (#989) здесь
	// не косметика. Иначе `?after_id=abc&project=<чужой>` отвечал бы 400 вместо
	// 403, то есть у стрима завёлся бы свой порядок исключений, отличный от
	// `/v1/events`.
	//
	// readEnv=false, и это НЕ значит «пара энфорсится целиком»: у событий нет
	// колонки env, поэтому сужение здесь — по ПРОЕКТУ, и привязанный к
	// `game/dev` ключ увидит в ленте `node_created` ноды `game/prod`, которую
	// `GET /v1/nodes` тому же ключу не покажет. Стрим (как и `/v1/events`) —
	// заведомо более СЛАБАЯ граница, чем листинги; это унаследовано от #987/#993
	// и названо в spec §6, границы (4). Параметр `?env=` при этом не гейтится
	// именно потому, что на выдачу он не влияет: гейтить его значило бы отвечать
	// по-разному на запросы с одинаковым результатом.
	//
	// СКОУП БЕРЁТСЯ НА КОННЕКТЕ, НО БОЛЬШЕ НЕ ЗАМОРОЖЕН НАВСЕГДА (tracker
	// #1016): учётка перепроверяется на КАЖДОМ опросе (см. ниже), поэтому отзыв
	// ключа, смена его скоупов или привязки закрывают уже открытую ленту в
	// пределах периода опроса. До этого лага не было вовсе — он был бесконечен,
	// пока клиент держит сокет.
	project, _, ok := s.tenantScope(w, r, false)
	if !ok {
		return
	}
	// Учётка, с которой лента открыта: с ней сравнивается результат
	// перепроверки. Ключ сюда положил requireScope.
	openedWith, _ := keyFromContext(r.Context())

	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "response writer does not support streaming")
		return
	}

	cursor, ok := s.sseCursor(w, r)
	if !ok {
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // no buffering in reverse proxies
	w.WriteHeader(http.StatusOK)
	// Reconnect hint for EventSource; also confirms the stream to the client.
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	fl.Flush()

	ctx := r.Context() // canceled by client disconnect and server shutdown
	// Состояние защиты от гонки коммитов (tracker #1013), ПО СОЕДИНЕНИЮ:
	// `sent` — id, уже отданные выше курсора (окно перечитывается, пока курсор
	// отстаёт); `seen` — очередь наблюдённых границ окна с моментом наблюдения,
	// по ней курсор и догоняется. Разделяемого состояния нет: обе структуры
	// живут ровно столько, сколько это соединение.
	sent := map[int64]struct{}{}
	var seen []seenWindow
	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()
	keepAlive := time.NewTicker(sseKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-poll.C:
			// ОТЗЫВ КЛЮЧА ЗАКРЫВАЕТ ЭТУ ЛЕНТУ (tracker #1016). Перепроверка
			// стоит ДО чтения событий: иначе отозванный ключ получил бы ещё
			// один кадр — тот самый, ради которого он и держит сокет. Цена —
			// поиск в мапе под мьютексом (`authenticate` на попадании в кэш);
			// после `invalidateKey` попадания нет, поэтому первый же опрос
			// идёт в БД и получает отказ, а лаг сводится к периоду опроса.
			// Комментарий SSE перед закрытием — для человека с curl'ом;
			// EventSource его игнорирует, переоткрывает соединение и получает
			// честный 401 от requireScope.
			if !s.auth.stillValid(r, openedWith) {
				_, _ = fmt.Fprint(w, ": credential revoked or changed\n\n")
				fl.Flush()
				return
			}
			// Сужение — В ЭТОМ САМОМ запросе, а не «на живом хвосте»: догон по
			// курсору и живая выдача — один и тот же вызов, поэтому отдельного
			// пути реплея, который можно забыть отфильтровать, не существует.
			events, watermark, err := s.st.ListEventsAfter(ctx, cursor, sseBatchLimit, project)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Error("sse: list events failed", "err", err)
				}
				continue // transient DB error: keep the stream, retry next tick
			}
			// Курсор двигается по РАССМОТРЕННОМУ окну, а не по отданным строкам:
			// иначе он застревал бы на последнем своём событии и каждый тик заново
			// сканировал бы хвост, накопленный соседним проектом. Наружу уходят
			// по-прежнему только id ВИДИМЫХ событий, так что Last-Event-ID клиента
			// остаётся точным — но отсюда и ЦЕНА, которую надо знать: экономия
			// живёт ВНУТРИ соединения, а на реконнекте клиент присылает свой
			// последний ВИДИМЫЙ id, и отставание возвращается целиком. Догон идёт
			// по sseBatchLimit id за тик (замер второго прохода: 2500 чужих id
			// между двумя своими событиями = ~6с тишины после реконнекта).
			// КУРСОР НЕ ПРЫГАЕТ НА ГРАНИЦУ ОКНА СРАЗУ (tracker #1013) — он
			// перешагивает её, только когда она наблюдалась дольше sseSettle,
			// потому что событие с меньшим id может стать видимым позже
			// (id выдаётся при INSERT'е, коммит — потом). Отданные строки
			// запоминаются, чтобы повторное чтение того же окна не отдавало
			// их дважды.
			now := time.Now()
			if watermark > cursor {
				seen = append(seen, seenWindow{at: now, id: watermark})
			}
			wrote := false
			for _, e := range events {
				if _, dup := sent[e.ID]; dup {
					continue
				}
				data, err := json.Marshal(e)
				if err != nil {
					s.log.Error("sse: marshal event failed", "id", e.ID, "err", err)
					continue
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Kind, data); err != nil {
					return
				}
				sent[e.ID] = struct{}{}
				wrote = true
			}
			// Догоняем курсор до самой свежей границы, которую видели уже
			// достаточно давно. Наружу по-прежнему уходят только id ВИДИМЫХ
			// событий, поэтому Last-Event-ID клиента остаётся точным.
			next := cursor
			for len(seen) > 0 && now.Sub(seen[0].at) >= sseSettle {
				next = seen[0].id
				seen = seen[1:]
			}
			if len(sent) > sseSentCap {
				// Защита памяти: лента такой плотности переживёт гонку хуже,
				// чем master — рост мапы. Один раз говорим об этом вслух.
				s.log.Warn("sse: commit-race guard disabled for this stream (too many events in flight)",
					"sent", len(sent), "settle", sseSettle)
				next, seen = watermark, nil
			}
			if next > cursor {
				cursor = next
				for id := range sent {
					if id <= cursor {
						delete(sent, id)
					}
				}
			}
			if !wrote {
				continue
			}
			fl.Flush()
			keepAlive.Reset(sseKeepAlive)
		}
	}
}

// sseCursor resolves the starting cursor: ?after_id → Last-Event-ID →
// current feed head. Writes the error response itself on bad input.
//
// Курсор — ПОЗИЦИЯ, а не токен доступа, и проверять его на «а имел ли клиент
// право дойти досюда» не нужно: скоуп берётся из ключа при открытии стрима
// (tenantScope выше) и применяется к КАЖДОМУ опросу, поэтому любой `after_id` —
// хоть 0, то есть вся история — и любой Last-Event-ID переигрывают ровно свои +
// платформенные события.
//
// «ЧУЖОЕ» ЗДЕСЬ ЗНАЧИТ РОВНО «строка с чужим `project_id`», и шире читать это
// нельзя. Фильтр не скрывающий, а `project_id` проставляется атрибуцией в
// insertEvent, поэтому платформенной (`null`) строкой в ленту привязанного
// ключа законно приезжают события удалённого проекта целиком (`on delete set
// null`) и события, которым атрибуции не досталось по существу (бекапы, CA,
// реестры — у них проекта нет). Событий жизненного цикла ключа среди последних
// БОЛЬШЕ НЕТ: `apikey_revoked`/`apikey_purged` теряли привязку не по существу,
// а потому что выборка стора её не читала, — починено в tracker #1017. Это
// по-прежнему следствие не-скрывающего правила, названное в spec §6, границы
// (4), а не дыра курсора, но обещать «сосед не увидит ничего своего» нельзя.
func (s *Server) sseCursor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("after_id")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "after_id must be a non-negative integer")
			return 0, false
		}
		return id, true
	}
	id, err := s.st.MaxEventID(r.Context())
	if err != nil {
		storeError(w, err)
		return 0, false
	}
	return id, true
}
