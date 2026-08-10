# Спека: SDK (liba)

> Обязательная интеграция в дедик + клиентский матчмейкинг-модуль. UE-first. Итерация 2; контракт замораживается в её начале и дальше меняется только аддитивно.
>
> **Контракт v0 заморожен 08.07.2026** (заголовок `sdk/core/include/birdman/birdman.h`): изменения только аддитивные — новые методы, новые поля `Config` с безопасными дефолтами, новые поля событий; менять/удалять существующее нельзя до v2.

## 1. Состав

```
sdk/core/       — C++17 либа без UE-зависимостей: UDS-транспорт, NDJSON, стейт-машина, поток I/O
sdk/unreal/     — UE-плагин "Birdman": обёртки-сабсистемы над core, интеграция в GameMode
sdk/mockagent/  — Go CLI-мок агента для локальной отладки интеграции (§4)         (уточнено в v0)
sdk/example/    — референс-«игра» на core: UDP + полный lifecycle, цель smoke-теста (уточнено в v0)
```

Core отдельно — чтобы Unity/кастомные движки позже получили тот же контракт без переписывания. Core собирается без внешних зависимостей (POSIX + стандартная библиотека, JSON — крошечный внутренний).

## 2. Серверная часть (в дедике)

### Инициализация и no-op режим

Автоконфиг из env (`BIRDMAN_SOCKET`, `BIRDMAN_SERVER_ID`, `BIRDMAN_PORT`). **Если env отсутствует — no-op режим**: все вызовы безопасны и ничего не делают, `IsManaged() == false`, ноль сетевых операций. Это обязательное свойство: локальная разработка и PIE работают без агента и без ifdef'ов. Managed-режим ключуется только на `BIRDMAN_SOCKET`.

### Core C++ API (уточнено в v0 — фактический замороженный контракт)

Канон — заголовок `sdk/core/include/birdman/birdman.h`; здесь выжимка:

```cpp
namespace birdman {
  std::string SdkVersion();                    // "birdman-cpp/0.1.0" → hello.sdk_version
  enum class MatchResult { kCompleted, kAborted };
  struct AllocatedEvent { std::string match_id; int players_expected;   // 0 = не знаю
                          std::map<std::string,std::string> metadata; };// может быть пустым
  struct DrainEvent     { double deadline_seconds; std::string reason; };
  enum class CallbackMode { kDispatch /*дефолт*/, kPoll };
  struct Config {
    CallbackMode callback_mode;                          // см. «модель колбэков»
    std::function<void(const AllocatedEvent&)> on_allocated;
    std::function<void(const DrainEvent&)> on_drain_requested;
    std::string socket_path, server_id; int port;        // override'ы для тестов; пусто → env
  };
  class ServerLink {                            // один на процесс; все методы thread-safe
    bool Init();  bool Init(const Config&);     // возвращает IsManaged(); повторный Init — no-op
    bool IsManaged() const;
    void NotifyReady();                         // обязателен ≤30с от старта процесса
    void NotifyMatchStart();                    // match_id уже известен из on_allocated
    void NotifyMatchEnd(MatchResult);           // после него ready больше не реплеится (one-shot)
    void SetPlayerCount(int);
    void ReportMetric(const std::string&, double); // ≤1/с на имя, коалесится (последнее значение)
    int  PollCallbacks();                       // только kPoll: раз в тик игры; вернёт число событий
    void Shutdown();                            // идемпотентен; флаш ≤1с; вызывается и деструктором
    std::string ServerId() const; int Port() const; std::string MatchId() const;
  };
}
```

**Модель колбэков (решение v0):** по умолчанию `kDispatch` — `on_allocated`/`on_drain_requested` зовутся из внутреннего I/O-потока сразу по приходу фрейма (обработчик обязан быть thread-safe; из обработчика можно звать методы `ServerLink` — дедлока нет). Опциональный `kPoll` — события копятся во внутренней очереди, игра забирает их `PollCallbacks()` на своём тике (обработчики зовутся в потоке вызывающего; геймдев-friendly, ноль локов в коде игры — см. `sdk/example`). UE-плагин использует `kDispatch` + `AsyncTask(GameThread)`.

**Идемпотентность реплеев (уточнено в v0):** агент реплеит последний `allocated`/`drain` при каждом реконнекте — SDK дедуплицирует (`allocated` по `match_id`, `drain` по паре `deadline_s`+`reason`), игра видит каждое событие ровно один раз. Дедлайн реплеенного `drain` не пересчитывается.

### UE API (сабсистема)

```cpp
UCLASS()
class BIRDMAN_API UBirdmanServerSubsystem : public UGameInstanceSubsystem {
public:
  bool IsManaged() const;

  void NotifyReady();                          // обязателен ≤30с от старта процесса
  void NotifyMatchStart();                     // matchId уже известен из OnAllocated
  void NotifyMatchEnd(EBirdmanMatchResult Result);   // Completed | Aborted
  void SetPlayerCount(int32 Count);            // или автотрекинг, см. ниже
  void ReportMetric(FName Name, double Value); // кастомные метрики

  UPROPERTY(BlueprintAssignable) FBirdmanOnAllocated OnAllocated;
      // (FString MatchId, int32 PlayersExpected, const TMap<FString,FString>& Meta)
  UPROPERTY(BlueprintAssignable) FBirdmanOnDrain OnDrainRequested;
      // (float DeadlineSeconds, const FString& Reason) — доиграть текущий матч и НЕ начинать
      // новый (уточнено в v0: добавлен Reason — он есть в wire-фрейме и в core-событии)
};
```

- **Автотрекинг игроков**: плагин подписывается на `AGameModeBase::PostLogin/Logout` и шлёт `players` сам; `SetPlayerCount` — для нестандартных случаев (первый ручной вызов выключает автотрекинг — уточнено в v0).
- **Tick-метрика**: плагин сам меряет среднее время кадра сервера и шлёт `metric{tick_ms}` раз в 5с — интеграции не требует (`p95` — TODO, аддитивно).
- Потоки: сокет-I/O в своём потоке; делегаты бросаются на game thread. Все методы thread-safe.
- Падение связи с агентом: реконнект с бэкоффом (0.1с → cap 2с) навсегда; на каждом коннекте `hello` + реплей состояния (`ready` — если матч не завершён, `players`); событийные фреймы `match_start`/`match_end`, отправленные в разрыве, копятся в кольце исходящих (256, старые вытесняются) и доезжают после реконнекта; игра об этом не знает. Периодический `players` раз в 10с (keepalive протокола) SDK шлёт сам. (Уточнено в v0.)

### Контракт жизненного цикла (обязанности игры)

1. Вызвать `NotifyReady()` когда сервер готов принять матч (карта загружена, порты слушаются).
2. По `OnAllocated` — подготовиться к матчу (match_id в GameState), по коннекту игроков — `NotifyMatchStart()`.
3. По окончании — `NotifyMatchEnd()`; **после этого процесс должен завершиться сам** (`FGenericPlatformMisc::RequestExit`) — дедик одноразовый, слот пересоздаст reconcile.
4. По `OnDrainRequested` — не стартовать новые раунды; доиграть ≤ deadline.

## 3. Клиентская часть (матчмейкинг)

```cpp
UCLASS()
class BIRDMAN_API UBirdmanMatchmakingClient : public UGameInstanceSubsystem {
public:
  // 1) merit QoS: UDP-echo до регионов из GET /v1/qos (параллельно, 5 пакетов, медиана)
  void MeasureQos(FBirdmanOnQosComplete OnComplete);           // [{region, rtt_ms}]
  // 2) тикет + long-poll до результата
  void RequestMatch(const FBirdmanMatchRequest& Req,           // regions+rtt, client_version (авто из ProjectVersion)
                    FBirdmanOnMatchFound OnFound,              // (Host, Port, MatchId, JoinToken)
                    FBirdmanOnMatchFailed OnFailed);           // (EReason: Timeout|UpdateRequired|Cancelled|Error)
  void CancelMatch();
};
```

Поведение: ретраи сетевых ошибок с бэкоффом (тикет пересоздаётся — потеря тикетов при рестарте master невидима для игрока, растёт только время в очереди); `update_required` доводится до UI как отдельный кейс. Коннект к матчу — обычный `ClientTravel(host:port)` + `?join_token=` в options (если включён verify).

> **Кто держит `matchmaking`-ключ (граница доверия).** Сабсистема выше по спеке ходит в master напрямую (реализации пока нет — заголовок `BirdmanMatchmakingClient.h` помечен как черновик-заглушка, и это ограничение обязана учесть будущая реализация), а значит ключ со скоупом `matchmaking` уезжает в игровой клиент и становится публичным. Аутентификации игрока в birdman нет by design: `player_id` — непрозрачная строка, которой master доверяет, и в этой схеме она самопровозглашённая (любой игрок может встать в очередь под чужим id, прочитать по `ticket_id` чужой тикет и отменить его). Если `player_id` должен что-то значить, тикеты заводит **бэкенд игры**: он аутентифицирует игрока своим механизмом, держит ключ у себя и отдаёт клиенту готовый `{host, port, match_id, join_token}` — клиенту остаётся `ClientTravel`. Реконнект и выход игрока — тоже его зона (матчмейкер их не умеет: новый тикет = новый матч). Разбор целиком, с рецептом реконнекта, — `architecture.md`, «Модель доверия (trust boundaries)».

## 4. Мок для разработки

`sdk/mockagent` (уточнено в v0: отдельный Go-модуль, не под `core/`) — крошечный CLI: слушает UDS, печатает все фреймы liba, по stdin-командам шлёт `allocated`/`drain`/`ping` (`allocate <match_id> [n]` | `drain <sec> [reason]` | `ping`). Повторяет поведение настоящего агента: новое подключение заменяет старое, последний `allocated`/`drain` реплеится при реконнекте, keepalive-ping раз в 10с. Используется в тестах CI игры и при локальной отладке интеграции; smoke-тест `sdk/scripts/smoke.sh` гоняет против него `sdk/example`.

## 5. Дистрибуция и версии

- UE-плагин — git-сабмодуль/копия в репо игры (Fab — после OSS-анонса). Версия SDK — semver, в `hello.sdk_version` (`birdman-cpp/<semver>`).
- **Заморозка: контракт v0 заморожен 08.07.2026** (начало итерации 2, `birdman.h`): сигнатуры выше — контракт; новые методы/делегаты/поля с дефолтами добавлять можно, менять/удалять — нельзя до v2.

## 6. Acceptance (итерация 2)

- Наша игра: dev-сборка с плагином проходит цикл ready → allocated → match_start → players(N) → match_end → exit; master видит игроков live.
- PIE/локальный запуск без агента — ноль ошибок, ноль сетевых попыток (no-op).
- Убийство агента под живой либой → реконнект, матч не страдает.
- Клиент: QoS-замер + RequestMatch → коннект двух клиентов в один матч; сценарий `update_required` показывает правильный UI-кейс.
