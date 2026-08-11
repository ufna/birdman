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
	ssePollInterval = time.Second
	sseKeepAlive    = 15 * time.Second
	sseBatchLimit   = 500
)

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
	// СКОУП ЗАМОРАЖИВАЕТСЯ НА КОННЕКТЕ. Стрим — один запрос длиной в сутки,
	// поэтому «пересчитывается на каждом запросе» здесь означает «на каждом
	// ОТКРЫТИИ»: отзыв ключа уже открытую ленту не закрывает, он закрывает
	// следующее открытие. Свойство унаследованное (аутентификация посреди стрима
	// не перепроверялась и до #999), разбор — tracker #1016.
	project, _, ok := s.tenantScope(w, r, false)
	if !ok {
		return
	}

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
			cursor = watermark
			if len(events) == 0 {
				continue
			}
			for _, e := range events {
				data, err := json.Marshal(e)
				if err != nil {
					s.log.Error("sse: marshal event failed", "id", e.ID, "err", err)
					continue
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Kind, data); err != nil {
					return
				}
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
// ключа законно приезжают: события удалённого проекта целиком (`on delete set
// null`) и события, которым атрибуции не досталось, — например `apikey_revoked`
// от `DELETE /v1/apikeys/{id}`, который кладёт в payload имя ключа без проекта
// (tracker #1017). Это следствие не-скрывающего правила, названное в spec §6,
// границы (4), а не дыра курсора, но обещать «сосед не увидит ничего своего»
// нельзя.
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
