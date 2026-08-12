package store

import "golang.org/x/crypto/bcrypt"

// РАБОЧИЙ ФАКТОР bcrypt — параметр КОНСТРУКТОРА стора, а не глобальная
// переменная и не настройка окружения.
//
// Зачем вообще ручка. `bcrypt.GenerateFromPassword` при `bcrypt.DefaultCost`
// под `-race` стоит ~0.7с за операцию (против ~11мс при `bcrypt.MinCost`), а
// интеграционный сьют master заводит порядка двух сотен ключей и нод за прогон
// — это была самая дорогая статья времени тестов, дороже и живых баз, и всех
// ожиданий реального времени (tracker #1060). Стоимость — рабочий фактор, а не
// поведение: ни один тест не утверждает ничего о ней, путь «хеш → БД → сверка»
// проходится целиком, отказ на неверном секрете остаётся отказом.
//
// Почему именно поле конструктора, а не то, что напрашивалось:
//
//   - env-переменная (`BIRDMAN_BCRYPT_COST`) дала бы рычаг СНАРУЖИ: хеширование
//     секретов запущенного мастера удешевлялось бы правкой окружения. Здесь
//     чтения окружения нет вообще — ни `os.Getenv`, ни поля в internal/config,
//     ни параметра DSN; удешевить хеширование можно только правкой исходника,
//     которую видно в ревью;
//   - пакетная `UseCheapHashingForTests()` — переключатель на ВЕСЬ ПРОЦЕСС:
//     сработал бы на всех уже созданных сторах, из любого случайно слинкованного
//     кода. Здесь стоимость приезжает ровно в точку конструирования ОДНОГО
//     `*Store` и после Open не меняется;
//   - `testing.Testing()` внутри стора не требует экспорта, но тянет пакет
//     `testing` в прод-сборку.
//
// Прод-путь (единственный `store.Open` в cmd/birdman-master) опций не передаёт
// и получает `bcrypt.DefaultCost`. Это закреплено двумя тестами в hashcost_
// internal_test.go: один читает cost ИЗ РЕАЛЬНО ПОЛУЧЕННОГО ХЕША, второй
// сканирует исходники и краснеет, если `WithHashCostForTests` появился хоть в
// одном не-тестовом файле вне этого файла и тестового бутстрапа testdb.

// Option настраивает Store в момент Open. Прод-вызов не передаёт ни одной.
type Option func(*Store)

// WithHashCostForTests задаёт рабочий фактор bcrypt для ЭТОГО стора.
//
// ТОЛЬКО ДЛЯ ТЕСТОВОГО БУТСТРАПА (master/internal/testdb): опция ослабляет
// хеширование секретов, и тест TestHashCostKnobStaysTestOnly уронит сьют, если
// её позовут из прод-кода. Значение вне [bcrypt.MinCost, bcrypt.MaxCost]
// игнорируется — стор останется на bcrypt.DefaultCost, а не свалится в «0 =
// без хеширования».
func WithHashCostForTests(cost int) Option {
	return func(s *Store) { s.bcryptCost = cost }
}

// hashCost — рабочий фактор этого стора. Невыставленное (нулевое) и заведомо
// негодное значение означает прод: bcrypt.DefaultCost.
func (s *Store) hashCost() int {
	if s.bcryptCost < bcrypt.MinCost || s.bcryptCost > bcrypt.MaxCost {
		return bcrypt.DefaultCost
	}
	return s.bcryptCost
}

// hashSecret — единственная точка хеширования секретов в сторе (ключи API и
// токены нод). Сверка (`bcrypt.CompareHashAndPassword`) остаётся как была:
// стоимость она читает из самого хеша, поэтому ключ, заведённый на одной
// стоимости, проверяется без всякого знания о ней.
func (s *Store) hashSecret(secret string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), s.hashCost())
	if err != nil {
		return "", err
	}
	return string(hash), nil
}
