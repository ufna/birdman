package httpapi

import (
	"github.com/ufna/birdman/master/internal/stats"
	"github.com/ufna/birdman/master/internal/store"
)

// Именованные обёртки ответов.
//
// ЗАЧЕМ. Ответы API — конверты вида `{"nodes": [...]}`, и писались они
// анонимным `map[string]any{"nodes": ...}`. Пока схема API нигде не была
// записана, это стоило дёшево. С генерацией `openapi.yaml` из таблицы
// маршрутов (routes.go) цена появилась: генератору неоткуда узнать форму
// ответа, а вписать её в таблицу отдельно — значит завести ВТОРУЮ копию
// правды, ту самую, против которой стоит решение #972.
//
// Поэтому конверт становится типом: хендлер пишет `nodesResp{Nodes: ...}`, а
// таблица предъявляет `nodesResp{}` образцом — имя ключа и форма живут в одной
// точке, и разъехаться им негде. Тип конверта, а не тип полезной нагрузки:
// `store.Node` и так свой, менять его незачем.
//
// Конверты заведены пока не для всех ручек — незакрытые перечислены в
// routesWithoutResponseSchema (openapi_test.go), и этот список закрыт
// на дозапись: новая ручка обязана прийти со своим конвертом.

type healthzResp struct {
	Status string `json:"status"`
	// panel: embedded|placeholder — вкомпилирована ли панель в этот бинарь (#983).
	Panel string `json:"panel"`
}

type nodesResp struct {
	Nodes []store.Node `json:"nodes"`
}

type nodeResp struct {
	Node store.Node `json:"node"`
}

type serversResp struct {
	Servers []store.Server `json:"servers"`
}

type versionsResp struct {
	Versions []store.Version `json:"versions"`
}

type eventsResp struct {
	Events []store.Event `json:"events"`
}

type matchesResp struct {
	Matches []store.Match `json:"matches"`
}

type matchResp struct {
	Match store.Match `json:"match"`
}

type projectsResp struct {
	Projects []store.Project `json:"projects"`
}

type projectResp struct {
	Project store.Project `json:"project"`
}

type environmentsResp struct {
	Environments []store.Environment `json:"environments"`
}

type environmentResp struct {
	Environment store.Environment `json:"environment"`
}

type qosResp struct {
	QoS []store.QoSEndpoint `json:"qos"`
}

type alertRulesResp struct {
	Rules []alertRule `json:"rules"`
}

type activeAlertsResp struct {
	Alerts []activeAlert `json:"alerts"`
}

type alertHistoryResp struct {
	Alerts []alertEvent `json:"alerts"`
}

type alertMutesResp struct {
	Mutes []store.AlertMute `json:"mutes"`
}

type alertMuteResp struct {
	Mute store.AlertMute `json:"mute"`
}

type backupSettingsResp struct {
	Settings store.BackupSettings `json:"settings"`
}

type backupRunsResp struct {
	Runs []store.BackupRun `json:"runs"`
}

type backupStartedResp struct {
	Started bool `json:"started"`
}

type okResp struct {
	OK bool `json:"ok"`
}

type registriesResp struct {
	Registries []store.Registry `json:"registries"`
}

type registryResp struct {
	Registry store.Registry `json:"registry"`
}

type apiKeysResp struct {
	APIKeys []store.APIKey `json:"apikeys"`
}

type apiKeyResp struct {
	Key store.APIKey `json:"key"`
}

// apiKeyCreatedResp — единственный ответ, показывающий секрет; он же
// единственный раз, когда секрет вообще покидает мастер (apikeys.go).
type apiKeyCreatedResp struct {
	Key    store.APIKey `json:"key"`
	Secret string       `json:"secret"`
}

type projectUsageResp struct {
	Usage store.ProjectUsage `json:"usage"`
}

type environmentUsageResp struct {
	Usage store.EnvironmentUsage `json:"usage"`
}

type projectDeletedResp struct {
	Deleted store.DeleteProjectResult `json:"deleted"`
}

type environmentDeletedResp struct {
	Deleted store.DeleteEnvironmentResult `json:"deleted"`
}

type statsOverviewResp = stats.OverviewResponse

type statsCostResp = stats.CostResponse

// apiError — единственная форма тела ошибки во всём API: writeError пишет
// только её (server.go). Отдельный тип нужен, чтобы у `#/components/schemas/
// Error` в спеке был тот же источник, что у кода.
//
// ПОРЯДОК ПОЛЕЙ ЗДЕСЬ ЗНАЧИМ, и менять его нельзя. Раньше тело собиралось из
// map[string]string, а encoding/json сортирует ключи карты по алфавиту — то
// есть на проводе всегда было `{"detail":…,"error":…}`. У структуры же порядок
// полей и есть порядок ключей: объявление error первым молча поменяло бы байты
// каждого отказа API. Для JSON порядок ключей незначим, но пины, сравнивающие
// тело отказа целиком (alerts_binding_test.go, tenant_scope_test.go,
// sse_test.go), сравнивают именно байты — как и всякий клиент, сверяющий
// строку. Порядок сохранён алфавитным ровно затем, чтобы типизация осталась
// чистым рефактором, а не тихой сменой формата.
type apiError struct {
	// Detail — человекочитаемое пояснение; на нём нельзя строить логику.
	Detail string `json:"detail"`
	// Error — машинный код: bad_request, forbidden, not_found, conflict…
	Error string `json:"error"`
}
