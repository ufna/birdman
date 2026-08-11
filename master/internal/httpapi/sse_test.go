package httpapi_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// apiServer spins the REST API without a running matchmaker loop.
func apiServer(t *testing.T, st *store.Store) *httptest.Server {
	ts, _ := apiServerRec(t, st)
	return ts
}

// apiServerRec is apiServer + the recorder behind the deploy manager.
func apiServerRec(t *testing.T, st *store.Store) (*httptest.Server, *testdb.CommandRecorder) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	rec := &testdb.CommandRecorder{}
	dep := deploy.New(deploy.Options{Store: st, Sender: rec, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	return ts, rec
}

func scopedKey(t *testing.T, st *store.Store, name string, scopes ...string) string {
	t.Helper()
	_, key, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{Name: name, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// sseEvent is one parsed SSE frame.
type sseEvent struct {
	id   int64
	kind string
	data string
}

// sseClient reads an event stream line by line off the test goroutine.
type sseClient struct {
	resp  *http.Response
	lines chan string
}

func openSSE(t *testing.T, url, key string) *sseClient {
	t.Helper()
	return openSSEWith(t, url, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+key)
	})
}

// openSSEWith — тот же openSSE, но заголовки ставит вызывающий: реконнект
// EventSource приходит с `Last-Event-ID`, а панель — вообще по cookie сессии.
func openSSEWith(t *testing.T, url string, prep func(*http.Request)) *sseClient {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	prep(req)
	resp, err := http.DefaultClient.Do(req) // no timeout: endless stream
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("stream: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("stream content-type: %s", ct)
	}
	c := &sseClient{resp: resp, lines: make(chan string, 256)}
	go func() {
		defer close(c.lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
	}()
	t.Cleanup(c.close)
	return c
}

func (c *sseClient) close() { c.resp.Body.Close() }

// next returns the next complete event frame (comments skipped).
func (c *sseClient) next(t *testing.T, timeout time.Duration) sseEvent {
	t.Helper()
	ev, ok := c.nextOrNone(timeout)
	if !ok {
		t.Fatalf("no SSE event within %v (или стрим закрылся)", timeout)
	}
	return ev
}

// nextOrNone — тот же next, но по таймауту и по закрытию стрима не валит тест
// сам, а отдаёт решение вызывающему: readUntil обязан показать, ЧТО он успел
// собрать, иначе самое частое падение («маркер не пришёл») выглядит как «no SSE
// event within 15s» без единой подсказки, чего не хватило (найдено вторым
// проходом: под мутацией курсора тест падал именно этим бесполезным текстом).
func (c *sseClient) nextOrNone(timeout time.Duration) (sseEvent, bool) {
	deadline := time.After(timeout)
	var ev sseEvent
	seen := false
	for {
		select {
		case <-deadline:
			return sseEvent{}, false
		case line, ok := <-c.lines:
			if !ok {
				return sseEvent{}, false
			}
			switch {
			case strings.HasPrefix(line, ":"), strings.HasPrefix(line, "retry:"):
				// comment / reconnect hint
			case strings.HasPrefix(line, "id: "):
				ev.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
				seen = true
			case strings.HasPrefix(line, "event: "):
				ev.kind = strings.TrimPrefix(line, "event: ")
				seen = true
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
				seen = true
			case line == "" && seen:
				return ev, true
			}
		}
	}
}

// TestEventsStream: auth, live delivery of new events, cursor replay via
// ?after_id (docs/specs/master.md §6 SSE).
func TestEventsStream(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	ctx := t.Context()
	roKey := scopedKey(t, st, "ro", httpapi.ScopeReadonly)

	// Auth matrix: anon and non-readonly scopes are rejected.
	if code, _ := (&client{t: t, base: ts.URL}).do("GET", "/v1/events/stream", nil); code != 401 {
		t.Fatalf("anon stream: want 401, got %d", code)
	}
	mmc := &client{t: t, base: ts.URL, key: scopedKey(t, st, "mm", httpapi.ScopeMatchmaking)}
	if code, _ := mmc.do("GET", "/v1/events/stream", nil); code != 403 {
		t.Fatalf("matchmaking-scope stream: want 403, got %d", code)
	}
	roc := &client{t: t, base: ts.URL, key: roKey}
	if code, _ := roc.do("GET", "/v1/events/stream?after_id=nope", nil); code != 400 {
		t.Fatal("bad after_id: want 400")
	}

	// Live delivery: events inserted after connect arrive with kind and data.
	c := openSSE(t, ts.URL+"/v1/events/stream", roKey)
	if err := st.InsertEvent(ctx, store.EventNodeQuarantine, store.EventRef{},
		map[string]any{"hostname": "n1"}); err != nil {
		t.Fatal(err)
	}
	ev := c.next(t, 5*time.Second)
	if ev.kind != store.EventNodeQuarantine {
		t.Fatalf("kind: want %s, got %+v", store.EventNodeQuarantine, ev)
	}
	var payload store.Event
	if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
		t.Fatalf("data is not json: %q: %v", ev.data, err)
	}
	if payload.ID != ev.id || fmt.Sprint(payload.Payload["hostname"]) != "n1" {
		t.Fatalf("payload mismatch: %+v vs id %d", payload, ev.id)
	}

	// Ordering: two more events arrive in insertion order.
	if err := st.InsertEvent(ctx, store.EventNodeRecovered, store.EventRef{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, store.EventCrashLoop, store.EventRef{}, nil); err != nil {
		t.Fatal(err)
	}
	ev2 := c.next(t, 5*time.Second)
	ev3 := c.next(t, 5*time.Second)
	if ev2.kind != store.EventNodeRecovered || ev3.kind != store.EventCrashLoop || ev3.id <= ev2.id {
		t.Fatalf("order: got %+v then %+v", ev2, ev3)
	}
	c.close()

	// Cursor replay: reconnect after the first event replays the rest.
	c2 := openSSE(t, fmt.Sprintf("%s/v1/events/stream?after_id=%d", ts.URL, ev.id), roKey)
	r2 := c2.next(t, 5*time.Second)
	r3 := c2.next(t, 5*time.Second)
	if r2.kind != store.EventNodeRecovered || r3.kind != store.EventCrashLoop {
		t.Fatalf("replay: got %+v then %+v", r2, r3)
	}
}

// --- арендаторская граница на СТРИМЕ (tracker #999) ---
//
// Стрим — ВТОРОЙ ВХОД В ТУ ЖЕ ТАБЛИЦУ, что `GET /v1/events`. До карточки у него
// фильтра не было ВООБЩЕ (не «был и не энфорсил»), поэтому сужение ленты #993
// обходилось подпиской: привязанный readonly-ключ получал живую ленту всей
// платформы с payload'ами — замер карточки, `node_created` с
// `hostname: secret-node-1` у чужого проекта.
//
// Тесты держат те же половины, что и у листингов (#993), плюс СВОЮ специфику,
// ради которой карточка не сведена к «дописать фильтр»: курсор `after_id` и
// `Last-Event-ID` на реконнекте не должны отдавать чужое ЗАДНИМ ЧИСЛОМ.

type streamFixture struct {
	st   *store.Store
	base string

	nbKey     string // readonly, привязан к neighbour/dev
	globalKey string // readonly без привязки
	adminKey  string // admin (привязать его нельзя — CreateAPIKey отвергает)
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	st := testdb.New(t)
	ts := apiServer(t, st)
	ctx := t.Context()

	// Ноды заводят и сами проекты, и историю `node_created` с hostname в
	// payload — ровно то, что утекало в замере.
	for _, n := range []struct{ project, env, hostname, ip string }{
		{"game", "dev", "secret-node-1", "203.0.113.11"},
		{"neighbour", "dev", "nb-node-1", "198.51.100.10"},
	} {
		if _, _, err := st.CreateNode(ctx, store.CreateNodeParams{
			Project: n.project, Env: n.env, Region: "eu",
			Hostname: n.hostname, PublicIP: n.ip, CapacitySlots: 4,
		}); err != nil {
			t.Fatalf("create node %s/%s: %v", n.project, n.env, err)
		}
	}

	nbProject, nbEnv := "neighbour", "dev"
	key := func(name string, scope string, project, env *string) string {
		t.Helper()
		_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			Name: name, Scopes: []string{scope}, Project: project, Env: env,
		})
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		return secret
	}
	return &streamFixture{
		st:        st,
		base:      ts.URL,
		nbKey:     key("ro-neighbour", httpapi.ScopeReadonly, &nbProject, &nbEnv),
		globalKey: key("ro-global", httpapi.ScopeReadonly, nil, nil),
		adminKey:  key("admin", httpapi.ScopeAdmin, nil, nil),
	}
}

// event пишет одно событие ленты и возвращает его id. project=="" —
// платформенное событие (`project_id is null`): правило не скрывающее, и это
// обязано остаться верным и для привязанного ключа.
func (f *streamFixture) event(t *testing.T, project, hostname string) int64 {
	t.Helper()
	payload := map[string]any{"hostname": hostname}
	if project != "" {
		payload["project"] = project // insertEvent выводит project_id из слага
	}
	if err := f.st.InsertEvent(t.Context(), store.EventNodeCreated, store.EventRef{}, payload); err != nil {
		t.Fatalf("insert event %s/%s: %v", project, hostname, err)
	}
	var id int64
	if err := f.st.Pool.QueryRow(t.Context(),
		`select id from events where payload->>'hostname' = $1`, hostname).Scan(&id); err != nil {
		t.Fatalf("event id by hostname %s: %v", hostname, err)
	}
	return id
}

func parseStreamEvent(t *testing.T, ev sseEvent) store.Event {
	t.Helper()
	var e store.Event
	if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
		t.Fatalf("кадр не json: %q: %v", ev.data, err)
	}
	if e.ID != ev.id {
		t.Fatalf("id кадра (%d) разошёлся с телом (%d)", ev.id, e.ID)
	}
	return e
}

func hostnameOf(e store.Event) string {
	h, _ := e.Payload["hostname"].(string)
	return h
}

func hostnamesOf(events []store.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Project+"/"+hostnameOf(e))
	}
	return out
}

// readUntil собирает кадры, пока не придёт событие с hostname sentinel, и
// возвращает ВСЕ, включая его. Маркер, а не «подождём и посмотрим»: он
// доказывает, что сервер уже прошёл курсором мимо всего, что записано раньше
// него, — иначе «чужое не пришло» значило бы всего лишь «мы не дождались».
func readUntil(t *testing.T, c *sseClient, sentinel string) []store.Event {
	t.Helper()
	var out []store.Event
	for {
		ev, ok := c.nextOrNone(15 * time.Second)
		if !ok {
			t.Fatalf("маркер %q не пришёл за 15с; собрано кадров %d: %v",
				sentinel, len(out), hostnamesOf(out))
		}
		e := parseStreamEvent(t, ev)
		out = append(out, e)
		if hostnameOf(e) == sentinel {
			return out
		}
	}
}

// streamRefusal ждёт от стрима ОТКАЗА и читает тело с таймаутом. Обычный
// клиент здесь не годится: если гейт исчезнет, ручка ответит 200 и бесконечным
// телом, и тест не упадёт, а ПОВИСНЕТ до паники go test (проверено мутацией —
// снятый tenantScope вешал прогон, вместо того чтобы его завалить).
func streamRefusal(t *testing.T, base, path, key string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s: запрос не завершился: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: тело не дочиталось за 5с — ручка не отказала (%d), а открыла стрим",
			path, resp.StatusCode)
	}
	return resp.StatusCode, raw
}

// assertNoForeign — проверка, ради которой карточка и заведена: в кадрах не
// должно быть ни события чужого проекта, ни его payload'а.
func assertNoForeign(t *testing.T, where string, got []store.Event) {
	t.Helper()
	for _, e := range got {
		if e.Project == "game" || strings.HasPrefix(hostnameOf(e), "secret-") {
			t.Fatalf("%s: в стрим пришло ЧУЖОЕ событие %+v (все кадры: %v)", where, e, hostnamesOf(got))
		}
	}
}

// Половина 2 (суть решения) на живой ленте: привязанный ключ БЕЗ параметров
// получает только свои и платформенные события. Чужое событие пишется ПЕРВЫМ:
// без фильтра оно пришло бы раньше своего, и маркер поймал бы его.
func TestEventsStreamNarrowsToBindingLive(t *testing.T) {
	f := newStreamFixture(t)
	c := openSSE(t, f.base+"/v1/events/stream", f.nbKey)

	f.event(t, "game", "secret-live-1")
	f.event(t, "", "platform-live-1")
	f.event(t, "neighbour", "nb-live-1")

	got := readUntil(t, c, "nb-live-1")
	assertNoForeign(t, "живая лента", got)

	// Своё событие — с РЕАЛЬНЫМ телом, а не «не 403»: kind, проект и payload.
	own := got[len(got)-1]
	if own.Project != "neighbour" || own.Kind != store.EventNodeCreated || own.ID == 0 {
		t.Fatalf("своё событие пришло выхолощенным: %+v", own)
	}
	// Платформенное видно и привязанному: фильтр НЕ скрывающий (#968/#993).
	var platform bool
	for _, e := range got {
		if e.Project == "" && hostnameOf(e) == "platform-live-1" {
			platform = true
		}
	}
	if !platform {
		t.Fatalf("платформенное событие не дошло — правило перестало быть не-скрывающим: %v",
			hostnamesOf(got))
	}
}

// Чужой трафик не должен ни попадать в стрим, ни рвать выдачу своих: между
// своими событиями лежит ровно столько же чужих, и все свои обязаны прийти по
// одному разу и по порядку. Про СТОИМОСТЬ опроса тест не доказывает ничего:
// курсор по рассмотренному окну экономит скан хвоста, но это свойство
// производительности, и мутация «курсор по отданным строкам» его не роняет.
func TestEventsStreamKeepsDeliveringPastForeignTraffic(t *testing.T) {
	f := newStreamFixture(t)
	c := openSSE(t, f.base+"/v1/events/stream", f.nbKey)

	const n = 15
	for i := range n {
		f.event(t, "game", fmt.Sprintf("secret-burst-%d", i))
		f.event(t, "neighbour", fmt.Sprintf("nb-burst-%d", i))
	}
	got := readUntil(t, c, fmt.Sprintf("nb-burst-%d", n-1))
	assertNoForeign(t, "чередующийся поток", got)
	if len(got) != n {
		t.Fatalf("своих событий пришло %d, want %d — курсор потерял часть: %v",
			len(got), n, hostnamesOf(got))
	}
	for i, e := range got {
		if want := fmt.Sprintf("nb-burst-%d", i); hostnameOf(e) != want {
			t.Fatalf("порядок ленты нарушен на %d: %v", i, hostnamesOf(got))
		}
	}
}

// Пачка БОЛЬШЕ sseBatchLimit, наполовину чужая: окно курсора и лимит выборки —
// разные числа, и путать их нельзя. Двигай стрим курсор не по РАССМОТРЕННОМУ
// окну, а по концу ленты, обрезанный хвост пачки не пришёл бы НИКОГДА — тихая
// потеря событий, которую «чужое не пришло» не ловит.
func TestEventsStreamLosesNothingOnOversizedBatch(t *testing.T) {
	f := newStreamFixture(t)
	c := openSSE(t, f.base+"/v1/events/stream", f.nbKey)

	// СВОИХ событий здесь БОЛЬШЕ sseBatchLimit — иначе тест не кусается: при
	// 300 своих любая ошибка «лимит на выборке вместо окна» проходит незаметно,
	// потому что вся пачка помещается в один батч (проверено мутацией).
	const half = 600
	if _, err := f.st.Pool.Exec(t.Context(), `
		insert into events (kind, payload, project_id)
		select 'node_created', jsonb_build_object('hostname', t.prefix || g),
		       (select id from projects where slug = t.slug)
		from generate_series(0, $1::int - 1) g
		cross join (values ('secret-bulk-', 'game', 0), ('nb-bulk-', 'neighbour', 1))
		            as t(prefix, slug, ord)
		order by g, t.ord`, half); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	got := readUntil(t, c, fmt.Sprintf("nb-bulk-%d", half-1))
	assertNoForeign(t, "пачка больше лимита", got)
	seen := map[string]bool{}
	for _, e := range got {
		seen[hostnameOf(e)] = true
	}
	for i := range half {
		if want := fmt.Sprintf("nb-bulk-%d", i); !seen[want] {
			t.Fatalf("событие %s потеряно: пришло %d из %d", want, len(got), half)
		}
	}
}

// СПЕЦИФИКА СТРИМА: курсор — позиция, а не токен доступа. Ни `after_id=0` (вся
// история), ни произвольный курсор прямо перед чужим событием, ни реконнект
// EventSource по `Last-Event-ID` не отдают чужое задним числом.
func TestEventsStreamReconnectDoesNotReplayForeign(t *testing.T) {
	f := newStreamFixture(t)
	f.event(t, "game", "secret-hist-1")
	nbHist := f.event(t, "neighbour", "nb-hist-1")
	secondForeign := f.event(t, "game", "secret-hist-2")

	cases := []struct {
		name, path, lastEventID string
	}{
		{"after_id=0 — вся история", "/v1/events/stream?after_id=0", ""},
		{
			"произвольный курсор прямо перед чужим событием",
			fmt.Sprintf("/v1/events/stream?after_id=%d", secondForeign-1), "",
		},
		{"реконнект EventSource по Last-Event-ID", "/v1/events/stream",
			strconv.FormatInt(nbHist, 10)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := openSSEWith(t, f.base+tc.path, func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+f.nbKey)
				if tc.lastEventID != "" {
					req.Header.Set("Last-Event-ID", tc.lastEventID)
				}
			})
			sentinel := fmt.Sprintf("nb-sentinel-%d", i)
			f.event(t, "neighbour", sentinel)
			assertNoForeign(t, tc.name, readUntil(t, c, sentinel))
		})
	}
}

// Половина 1: явный ЧУЖОЙ `?project=` → 403, и отказ БАЙТ-В-БАЙТ тот же, что у
// листингов #993, — иначе стрим остаётся оракулом существования проектов.
func TestEventsStreamRefusesForeignProject(t *testing.T) {
	f := newStreamFixture(t)

	liveCode, live := streamRefusal(t, f.base, "/v1/events/stream?project=game", f.nbKey)
	ghostCode, ghost := streamRefusal(t, f.base, "/v1/events/stream?project=zzz-nope", f.nbKey)
	if liveCode != 403 || ghostCode != 403 {
		t.Fatalf("живой чужой проект=%d, выдуманный=%d, want 403 у обоих\n живое=%s\n выдуманное=%s",
			liveCode, ghostCode, live, ghost)
	}
	if !bytes.Equal(live, ghost) {
		t.Fatalf("403 отличается телом и потому остаётся оракулом существования:\n живое=%s\n выдуманное=%s",
			live, ghost)
	}
	if want := `{"detail":"key is bound to neighbour/dev","error":"forbidden"}`; strings.TrimSpace(string(live)) != want {
		t.Fatalf("тело отказа разъехалось с листингами: got %s, want %s", live, want)
	}

	// Гейт РАНЬШЕ разбора курсора: битый `after_id` с чужим проектом обязан дать
	// 403, а не 400, иначе у стрима заводится свой порядок исключений (#989).
	if code, body := streamRefusal(t, f.base, "/v1/events/stream?after_id=abc&project=game", f.nbKey); code != 403 {
		t.Fatalf("битый after_id с чужим проектом: %d %s, want 403 — гейт стоит не первым", code, body)
	}
}

// Половина 3: свой проект, названный ЯВНО, работает и отдаёт реальное тело.
func TestEventsStreamAllowsOwnProjectExplicitly(t *testing.T) {
	f := newStreamFixture(t)
	c := openSSE(t, f.base+"/v1/events/stream?project=neighbour", f.nbKey)

	f.event(t, "game", "secret-explicit-1")
	f.event(t, "neighbour", "nb-explicit-1")

	got := readUntil(t, c, "nb-explicit-1")
	assertNoForeign(t, "явный свой проект", got)
	if own := got[len(got)-1]; own.Project != "neighbour" || own.Kind != store.EventNodeCreated {
		t.Fatalf("явный свой проект отдал выхолощенное событие: %+v", own)
	}
}

// Сессия панели наследует привязку ключа логина ЦЕЛИКОМ (#974/#993), поэтому
// оператор, вошедший ПРИВЯЗАННЫМ ключом, обязан видеть суженную живую ленту.
// «Панель работает как раньше» верно про НЕпривязанную сессию, а не про панель
// вообще, и без этого теста разница держалась бы на слове (найдено вторым
// проходом: положительный кейс логинился только admin-ключом).
func TestEventsStreamNarrowsForBoundPanelSession(t *testing.T) {
	f := newStreamFixture(t)
	b := &browser{t: t, base: f.base, csrf: true}
	code, _, resp := b.do("POST", "/v1/session", map[string]any{"api_key": f.nbKey})
	if code != 200 {
		t.Fatalf("логин сессии панели привязанным ключом: %d", code)
	}
	cookie := sessionCookieOf(t, resp)

	c := openSSEWith(t, f.base+"/v1/events/stream", func(req *http.Request) { req.AddCookie(cookie) })
	f.event(t, "game", "secret-bound-session-1")
	f.event(t, "neighbour", "nb-bound-session-1")

	got := readUntil(t, c, "nb-bound-session-1")
	assertNoForeign(t, "сессия панели, залогиненная привязанным ключом", got)
}

// Половина 4, обязательная: глобальный ключ, admin и СЕССИЯ ПАНЕЛИ (панель зовёт
// стрим без параметров — panel/src/lib/sse.ts) видят платформу ровно как раньше.
func TestEventsStreamUnchangedForUnboundKeys(t *testing.T) {
	f := newStreamFixture(t)

	for _, tc := range []struct{ name, key string }{
		{"глобальный readonly", f.globalKey},
		{"admin", f.adminKey},
	} {
		c := openSSE(t, f.base+"/v1/events/stream", tc.key)
		sentinel := "secret-" + strings.ReplaceAll(tc.name, " ", "-")
		f.event(t, "game", sentinel)
		got := readUntil(t, c, sentinel)
		if last := got[len(got)-1]; last.Project != "game" {
			t.Fatalf("%s: чужой проект пропал из стрима — сузили не то: %+v", tc.name, last)
		}
	}

	// Сессия панели: логин admin-ключом, дальше только cookie — как ходит панель.
	b := &browser{t: t, base: f.base, csrf: true}
	code, _, resp := b.do("POST", "/v1/session", map[string]any{"api_key": f.adminKey})
	if code != 200 {
		t.Fatalf("логин сессии панели: %d", code)
	}
	cookie := sessionCookieOf(t, resp)
	c := openSSEWith(t, f.base+"/v1/events/stream", func(req *http.Request) { req.AddCookie(cookie) })
	f.event(t, "game", "secret-session-1")
	if last := readUntil(t, c, "secret-session-1"); last[len(last)-1].Project != "game" {
		t.Fatalf("сессия панели перестала видеть платформу: %v", hostnamesOf(last))
	}

	// Опечатка в `?project=` у непривязанного ключа — прежний 400 (#961), а не
	// 403: закрыли привязанному, а не убрали валидацию.
	code, body := streamRefusal(t, f.base, "/v1/events/stream?project=zzz-nope", f.globalKey)
	if want := `{"detail":"no such project zzz-nope","error":"bad_request"}`; code != 400 ||
		strings.TrimSpace(string(body)) != want {
		t.Fatalf("опечатка в ?project= на стриме: %d %s, want 400 %s", code, body, want)
	}
}
