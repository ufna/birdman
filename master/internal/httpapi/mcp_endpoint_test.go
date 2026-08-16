package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Пины входной двери `/v1/mcp` — оба поставлены ПО СЛЕДАМ ЖИВОЙ ПРОВЕРКИ, а не
// из общих соображений: на стенде эндпоинт отвечал 403 «invalid Host header» на
// собственном домене, потому что защита SDK от DNS-rebinding'а режет запрос,
// пришедший с localhost с не-localhost Host, — то есть ровно нашу топологию
// «мастер на 127.0.0.1 за обратным прокси». Юнит-тесты этого не видели: они
// ходят по httptest-адресу, где Host и адрес совпадают.

func newMCPTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	return httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log)), st
}

func mcpInitBody() []byte {
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "pin", "version": "1"},
		},
	})
	return raw
}

// Запрос с ЧУЖИМ Host должен проходить: наружу мастер публикует обратный прокси,
// и Host в нём — публичный домен, а не адрес листенера.
func TestMCPAcceptsProxiedHost(t *testing.T) {
	ts, st := newMCPTestServer(t)
	defer ts.Close()
	_, key, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{
		Name: "mcp", Scopes: []string{httpapi.ScopeReadonly}})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", ts.URL+"/v1/mcp", bytes.NewReader(mcpInitBody()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	// Ровно тот случай, что ломался на стенде: соединение локальное, Host — домен.
	req.Host = "birdman.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize с проксированным Host → %d, ожидалось 200: "+
			"защита SDK от rebinding'а снова режет штатный деплой за прокси", resp.StatusCode)
	}
}

// Кука сессии панели на этом эндпоинте не принимается. Отказ обязан быть явным:
// в противном случае initialize проходил бы, а первый же tools/call падал с 401,
// потому что инструмент исполняется заголовком вызывающего, которого у куки нет.
func TestMCPRefusesSessionCookie(t *testing.T) {
	ts, st := newMCPTestServer(t)
	defer ts.Close()
	_, key, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{
		Name: "panel", Scopes: []string{httpapi.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}

	login, _ := json.Marshal(map[string]any{"api_key": key})
	resp, err := http.Post(ts.URL+"/v1/session", "application/json", bytes.NewReader(login))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("логин в панель → %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("логин не выдал куку — пин проверял бы пустоту")
	}

	req, err := http.NewRequest("POST", ts.URL+"/v1/mcp", bytes.NewReader(mcpInitBody()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, c := range cookies {
		req.AddCookie(c)
	}

	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusForbidden {
		t.Fatalf("MCP по куке → %d, ожидалось 403: ambient authority на этом пути недопустима", got.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(got.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "bearer_required" {
		t.Errorf("код отказа %q, ожидался bearer_required — по нему клиент понимает, что чинить", body["error"])
	}
}

// Без учётных данных — обычная 401 в формате API, а не протокольная ошибка MCP.
func TestMCPRefusesAnonymous(t *testing.T) {
	ts, _ := newMCPTestServer(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/mcp", "application/json", bytes.NewReader(mcpInitBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("анонимный MCP → %d, ожидалась 401", resp.StatusCode)
	}
}
