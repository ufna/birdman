package httpapi

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// specPath — где лежит закоммиченная спека относительно этого пакета.
const specPath = "../../api/openapi.yaml"

// TestOpenAPISpecIsUpToDate — тот самый CI-гейт, ради которого решение #972
// требовало генерации: закоммиченный контракт обязан совпадать с тем, что
// генератор собирает из таблицы маршрутов ПРЯМО СЕЙЧАС. Пин живёт в обычных
// тестах master'а, поэтому отдельного шага в воркфлоу не нужно: `go test ./...`
// и есть гейт.
func TestOpenAPISpecIsUpToDate(t *testing.T) {
	got, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("генератор спеки упал: %v", err)
	}
	want, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("не читается %s: %v (спека не сгенерирована? `go generate ./...`)", specPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s разошёлся с таблицей маршрутов — пересобери: go generate ./...\n"+
			"(в CI это значит: кто-то поменял ручку и не обновил контракт)", specPath)
	}
}

// routesWithoutResponseSchema — ручки, чей ответ пока не типизирован: они
// собирают тело динамически (`resp["auto_deploy"] = …`), и честная схема
// требует не образца типа, а разбора условий — отдельной работы.
//
// СПИСОК ЗАКРЫТ НА ДОЗАПИСЬ. Новая ручка обязана прийти со своим конвертом
// (dto.go) либо с RespOpaque; расширять этот список нельзя — иначе пробел в
// контракте тихо растёт, а спека постепенно перестаёт что-либо обещать.
var routesWithoutResponseSchema = []string{
	"POST /v1/nodes",
	"PUT /v1/fleets/{region}",
	"POST /v1/agent-upgrade",
	"POST /v1/versions",
	"POST /v1/deploy",
	"POST /v1/rollback",
	"POST /v1/promote",
	"DELETE /v1/session",
}

func TestRoutesHaveResponseSamples(t *testing.T) {
	var undescribed []string
	for _, rt := range routeTable() {
		if rt.Resp == nil && !rt.RespOpaque {
			undescribed = append(undescribed, rt.pattern())
		}
	}

	for _, got := range undescribed {
		if !slices.Contains(routesWithoutResponseSchema, got) {
			t.Errorf("%s не описывает тело ответа и не внесён в список исключений — "+
				"заведи конверт в dto.go (или RespOpaque, если тело не JSON). "+
				"Дописывать исключения нельзя: список только сокращается", got)
		}
	}
	for _, want := range routesWithoutResponseSchema {
		if !slices.Contains(undescribed, want) {
			t.Errorf("%s уже описывает тело ответа — вычеркни его из "+
				"routesWithoutResponseSchema, иначе список перестанет отражать реальный долг", want)
		}
	}
}

// Спека обязана называть каждую ручку таблицы: ручка, выпавшая из контракта,
// для интегратора не существует.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	spec, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("генератор спеки упал: %v", err)
	}
	text := string(spec)
	for _, rt := range routeTable() {
		if !strings.Contains(text, "\n  "+rt.Path+":") {
			t.Errorf("путь %s не попал в спеку", rt.Path)
		}
	}
}

// Публичная по замыслу ручка обязана быть отмечена в спеке пустым security:
// иначе читатель решит, что ключ нужен, и не станет звать её без ключа —
// а `/v1/qos` игровой клиент зовёт именно до того, как ключ у него появится.
func TestOpenAPIMarksPublicRoutes(t *testing.T) {
	spec, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("генератор спеки упал: %v", err)
	}
	if !strings.Contains(string(spec), "security: []") {
		t.Error("в спеке нет ни одной ручки с пустым security — публичные ручки описаны неверно")
	}
}
