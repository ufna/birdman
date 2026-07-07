# Спека: SDK (liba)

> Обязательная интеграция в дедик + клиентский матчмейкинг-модуль. UE-first. Итерация 2; контракт замораживается в её начале и дальше меняется только аддитивно.

## 1. Состав

```
sdk/core/    — C++17 либа без UE-зависимостей: UDS-транспорт, NDJSON, стейт-машина, поток I/O
sdk/unreal/  — UE-плагин "Birdman": обёртки-сабсистемы над core, интеграция в GameMode
```

Core отдельно — чтобы Unity/кастомные движки позже получили тот же контракт без переписывания.

## 2. Серверная часть (в дедике)

### Инициализация и no-op режим

Автоконфиг из env (`BIRDMAN_SOCKET`, `BIRDMAN_SERVER_ID`, `BIRDMAN_PORT`). **Если env отсутствует — no-op режим**: все вызовы безопасны и ничего не делают, `IsManaged() == false`. Это обязательное свойство: локальная разработка и PIE работают без агента и без ifdef'ов.

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
      // (float DeadlineSeconds) — доиграть текущий матч и НЕ начинать новый
};
```

- **Автотрекинг игроков**: плагин подписывается на `AGameModeBase::PostLogin/Logout` и шлёт `players` сам; `SetPlayerCount` — для нестандартных случаев (переопределяет автотрекинг).
- **Tick-метрика**: плагин сам меряет среднее/`p95` время кадра сервера и шлёт `metric{tick_ms}` раз в 5с — интеграции не требует.
- Потоки: сокет-I/O в своём потоке; делегаты бросаются на game thread. Все методы thread-safe.
- Падение связи с агентом: буфер исходящих (кольцо 256 сообщений) + реконнект с бэкоффом; игра об этом не знает.

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

## 4. Мок для разработки

`sdk/core/mockagent` — крошечный бинарь: слушает UDS, печатает сообщения, по команде шлёт `allocated`/`drain`. Используется в тестах CI игры и при локальной отладке интеграции.

## 5. Дистрибуция и версии

- UE-плагин — git-сабмодуль/копия в репо игры (Fab — после OSS-анонса). Версия SDK — semver, в `hello.sdk_version`.
- Заморозка (конец итерации 2): сигнатуры выше — контракт; новые методы/делегаты добавлять можно, менять/удалять — нельзя до v2.

## 6. Acceptance (итерация 2)

- Наша игра: dev-сборка с плагином проходит цикл ready → allocated → match_start → players(N) → match_end → exit; master видит игроков live.
- PIE/локальный запуск без агента — ноль ошибок, ноль сетевых попыток (no-op).
- Убийство агента под живой либой → реконнект, матч не страдает.
- Клиент: QoS-замер + RequestMatch → коннект двух клиентов в один матч; сценарий `update_required` показывает правильный UI-кейс.
