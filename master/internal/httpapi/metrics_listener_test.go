package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Прометеевский реестр master'а НЕ отдаётся с того же листенера, что `/v1/*`
// (tracker #1003). Он не пер-тенантный и не аутентифицирован: один
// `curl http://<master>/metrics` перечислял все слаги проектов, все имена
// окружений, состав флота по регионам/версиям и все живые `server_id` — те
// самые данные, которые #993/#989/#988/#974 закрыли на `/v1/*` для
// привязанного ключа.
//
// Гейт существовал и раньше, но в чужом файле — `location = /metrics
// { return 403; }` в nginx нашей ansible-роли. Ни один тест мастера его не
// видел, у self-host-оператора со своим прокси его не было вовсе, а изнутри
// периметра (соседний контейнер, ssh-туннель) он не закрывал ничего. Этот файл
// — перенос той границы в код: он краснеет ровно тогда, когда маршрут
// возвращают на API-мультиплексор.
func TestMetricsNotOnAPIMux(t *testing.T) {
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)

	_, adminKey, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Ни анониму, ни admin-ключу: маршрута на этом мультиплексоре НЕТ вовсе,
	// поэтому ответ — 404 от ServeMux, а не 401/403. Проверяются обе стороны
	// именно затем, чтобы «закрыли скоупом» (вариант, отвергнутый в карточке:
	// он не решает пер-тенантность и ломает скрейпер) не прошло за починку.
	for _, c := range []struct{ name, key string }{
		{"аноним", ""},
		{"admin-ключ", adminKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", ts.URL+"/metrics", nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.key != "" {
				req.Header.Set("Authorization", "Bearer "+c.key)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("GET /metrics на API-листенере = %d, want 404 (реестр не должен отдаваться отсюда): %.200s",
					resp.StatusCode, raw)
			}
			// И тела реестра в ответе нет ни в каком виде.
			if strings.Contains(string(raw), "birdman_") || strings.Contains(string(raw), "go_goroutines") {
				t.Fatalf("тело реестра утекло в ответ API-листенера: %.300s", raw)
			}
		})
	}
}

// Экспозиция при этом не потеряна: её отдаёт ОТДЕЛЬНЫЙ хендлер, который
// main.go вешает на `config.ListenMetrics` (деф. `127.0.0.1:9102`). Без этой
// половины «починка» свелась бы к удалению метрик — и алерты умерли бы молча.
func TestMetricsHandlerServesRegistry(t *testing.T) {
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	ts := httptest.NewServer(httpapi.MetricsHandler(m))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics на своём листенере = %d, want 200: %.200s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "birdman_allocation_failures_total") &&
		!strings.Contains(string(raw), "go_goroutines") {
		t.Fatalf("экспозиция выглядит пустой: %.200s", raw)
	}
	// Хендлер отдаёт ТОЛЬКО метрики: ключей, ручек `/v1/*` и панели на этом
	// адресе нет — иначе loopback-порт стал бы вторым входом в API.
	for _, path := range []string{"/v1/nodes", "/healthz", "/"} {
		r, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		code := r.StatusCode
		r.Body.Close()
		if code != http.StatusNotFound {
			t.Fatalf("GET %s на листенере метрик = %d, want 404", path, code)
		}
	}
}
