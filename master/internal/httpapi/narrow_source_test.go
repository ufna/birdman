package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Пара в фильтре сужения приходит ТОЛЬКО из привязки ключа — через ВСЮ цепочку
// HTTP, а не только на уровне функции сборки запроса (tracker #1014).
//
// Батарея #994 проверяла, что клиентские `extra_*` отброшены, но `?project=` /
// `?env=` не слал ни один её тест. Поэтому мутация «раз параметр пришёл —
// взять пару из него» проходила весь сьют зелёной и открывала настоящее
// чтение чужого: ключ, привязанный к `alpha/dev`, получал 200 на
// `?project=beta&env=dev` и строку чужого стрима.
//
// Проверка — СРАВНЕНИЕ двух запросов: с параметрами и без. Подкрутить ожидание
// под сломанный код тут нельзя, потому что ожидание вычисляется из самого же
// поведения.
func TestNarrowingIgnoresClientScopeParamsOverHTTP(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})

	var gotVL, gotVM url.Values
	vl := httptest.NewServer(narrowAwareUpstream("extra_stream_filters", func(w http.ResponseWriter, r *http.Request) {
		gotVL = r.URL.Query()
		w.Header().Set("Content-Type", "application/stream+json")
		_, _ = w.Write([]byte("{}\n"))
	}))
	t.Cleanup(vl.Close)
	vm := httptest.NewServer(narrowAwareUpstream("extra_label", func(w http.ResponseWriter, r *http.Request) {
		gotVM = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(vm.Close)

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, vm.URL, vl.URL, log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	// Соседний проект существует и НЕ выдуман: мутация «взять пару из query»
	// на живом слаге дала бы честные чужие данные, а не пустоту.
	for _, slug := range []string{"alpha", "beta"} {
		if _, err := st.CreateProject(ctx, slug, 2); err != nil {
			t.Fatal(err)
		}
	}
	project, env := "alpha", "dev"
	_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-alpha", Scopes: []string{httpapi.ScopeReadonly}, Project: &project, Env: &env,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound := &client{t: t, base: ts.URL, key: secret}

	// Всё, чем клиент может попытаться назвать ЧУЖУЮ пару. `scope`/`tenant`/
	// `project_id` — не существующие сегодня имена: они здесь затем, чтобы
	// «добавили ещё один способ прислать пару» тоже краснело.
	spike := "&project=beta&env=prod&project_id=beta&tenant=beta&scope=" + url.QueryEscape("beta/prod")

	cases := []struct {
		name, path string
		got        *url.Values
		knob       string
		want       string
	}{
		{
			name: "logs", knob: "extra_stream_filters", want: `{project="alpha",env="dev"}`,
			path: "/v1/logs/query?query=" + url.QueryEscape(`{server_id="s1"}`) + "&start=0&end=10",
			got:  &gotVL,
		},
		{
			name: "metrics", knob: "extra_label", want: "project=alpha",
			path: "/v1/metrics/query?query=up&time=5",
			got:  &gotVM,
		},
		{
			name: "metrics_range", knob: "extra_label", want: "project=alpha",
			path: "/v1/metrics/query_range?query=up&start=0&end=10&step=15",
			got:  &gotVM,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, body := bound.doRaw("GET", c.path); code != 200 {
				t.Fatalf("без параметров: %d (%s)", code, body)
			}
			clean := (*c.got)[c.knob]
			if len(clean) == 0 {
				t.Fatalf("апстрим не получил %s вовсе — сужения нет", c.knob)
			}

			if code, body := bound.doRaw("GET", c.path+spike); code != 200 {
				t.Fatalf("с параметрами: %d (%s)", code, body)
			}
			spiked := (*c.got)[c.knob]
			if len(spiked) != len(clean) {
				t.Fatalf("%s: с ?project=/?env= пришло %q, без них %q", c.knob, spiked, clean)
			}
			for i := range clean {
				if clean[i] != spiked[i] {
					t.Fatalf("%s: пара поехала за клиентским параметром: %q против %q", c.knob, spiked, clean)
				}
			}
			if spiked[0] != c.want {
				t.Fatalf("%s = %q, want %q — пара взята НЕ из привязки", c.knob, spiked[0], c.want)
			}
			// И сами чужие имена до апстрима не доезжают ни под каким видом.
			for _, k := range []string{"project", "env", "project_id", "tenant", "scope"} {
				if v, ok := (*c.got)[k]; ok {
					t.Fatalf("клиентский параметр %q доехал до апстрима: %q", k, v)
				}
			}
		})
	}
}
