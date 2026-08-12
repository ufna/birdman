package httpapi

// Test-only hooks for the external httpapi_test package.

import (
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

// Ожидания реального времени на пути SSE — короткие на ВЕСЬ тестовый бинарь
// (tracker #1016). init() отрабатывает до TestMain, то есть до старта любого
// теста и любой горутины стрима, поэтому запись сюда одна и гонки с чтением из
// handleEventsStream быть не может; менять эти переменные ИЗ теста нельзя по
// той же причине. Обоснование подмены — у объявления в sse.go.
//
// Файл export_test.go компилируется только `go test`, так что в собранный
// birdman-master короткие значения не попадают ни при каких условиях: там
// остаются 1с/5с из sse.go.
func init() {
	ssePollInterval = 100 * time.Millisecond
	sseSettle = 2 * time.Second
}

// SSESettleForTest / SSEPollIntervalForTest отдают действующие значения внешнему
// пакету httpapi_test. Тест, которому нужно «читать дольше, чем ждёт курсор»,
// обязан считать свой срок ОТ них: вписанные числом секунды расклеятся при
// первой же смене подмены — и расклеятся молча, превратив проверку в
// тавтологию.
func SSESettleForTest() time.Duration       { return sseSettle }
func SSEPollIntervalForTest() time.Duration { return ssePollInterval }

func NewSessionStoreForTest(ttl time.Duration) *sessionStore { return newSessionStore(ttl) }

func (ss *sessionStore) CreateForTest(name string, scopes []string) (string, error) {
	return ss.create(store.APIKey{Name: name, Scopes: scopes})
}

func (ss *sessionStore) GetForTest(id string) (store.APIKey, bool) { return ss.get(id) }

// SessionBindingForTest exposes the binding half of the /v1/session response.
// Needed because the half-pair case (Project set, Env nil) cannot be produced
// through the store at all — CreateAPIKey and a CHECK both reject it — so the
// only way to test how the API describes such a key is to call the mapper.
func SessionBindingForTest(key store.APIKey) *sessionBinding { return sessionResponseFor(key).Binding }
