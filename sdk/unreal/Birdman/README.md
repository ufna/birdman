# Birdman — UE-плагин

> **⚠️ DRAFT: этот плагин ещё ни разу не компилировался против реального движка.**
> Первая сборка — при интеграции в игру (итерация 2, «~0.5 инженера команды
> игры»). Код написан по памяти UE5 API поверх замороженного контракта
> `sdk/core` и покрытого тестами транспорта; ожидаемые правки — имена
> инклюдов/делегатов UE, не семантика.

Тонкая обёртка над `sdk/core` (см. `../../core/include/birdman/birdman.h` —
замороженный контракт v0, и `docs/specs/sdk.md`):

- **`UBirdmanServerSubsystem`** — серверная часть в дедике: `IsManaged()`,
  `NotifyReady/MatchStart/MatchEnd`, `SetPlayerCount`, `ReportMetric`,
  делегаты `OnAllocated` / `OnDrainRequested` (бросаются на game thread через
  `AsyncTask`). Автотрекинг игроков по `FGameModeEvents` PostLogin/Logout
  (первый ручной `SetPlayerCount` выключает автотрекинг), автометрика
  `tick_ms` раз в 5с. В PIE/клиенте/без агента — безопасный no-op.
- **`UBirdmanMatchmakingClient`** — клиентский матчмейкинг: пока только
  заголовок с TODO-планом (HTTP-слой UE — итерация интеграции с игрой).

## Подключение в игру

1. Репо birdman — сабмодулем (или vendored-копией) в репо игры; плагин
   линкуется/копируется в `Plugins/Birdman` → на `sdk/unreal/Birdman`.
   Если копируете плагин отдельно от репо — поправьте путь к core в
   `Source/Birdman/Birdman.Build.cs` (константа `SdkRoot`).
2. Соберите core под целевую платформу сервера (см. ниже) — Build.cs ждёт
   статическую либу в `sdk/core/build-ue/libbirdman_core.a`.
3. Включите плагин в `.uproject` и добавьте `"Birdman"` в
   `PublicDependencyModuleNames` модуля игры (если зовёте из C++).
4. В GameMode: `NotifyReady()` после загрузки карты; подписка на
   `OnAllocated`/`OnDrainRequested`; `NotifyMatchEnd()` + `RequestExit` по
   концу матча. Референс потока — `sdk/example/main.cpp`.

## Сборка core под UE (`build-ue`)

Linux-сервер собирается clang'ом из UE-тулчейна, чтобы ABI (libc++) совпал с
движком:

```sh
# пример для Linux x86_64; путь к тулчейну — из установки UE
UE_CLANG=~/UnrealToolchains/v22_clang-16.0.6-centos7/x86_64-unknown-linux-gnu/bin/clang++
cmake -S sdk -B sdk/core/build-ue \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_CXX_COMPILER="$UE_CLANG" \
  -DCMAKE_CXX_FLAGS="-stdlib=libc++ -fvisibility=hidden"
cmake --build sdk/core/build-ue --target birdman_core -j
# либа: sdk/core/build-ue/core/libbirdman_core.a → положить/слинковать в sdk/core/build-ue/
```

> TODO при первой интеграции: зафиксировать точный путь тулчейна и добавить
> скрипт `sdk/scripts/build-ue-core.sh`; проверить, не потребуется ли
> `-nostdinc++` + инклюды libc++ из UE (зависит от версии движка).

На платформах без core-либы (Windows-редактор и т.п.) Build.cs выставляет
`BIRDMAN_WITH_CORE=0` — сабсистема компилируется в no-op, игра работает как
обычно.

## Что проверить при первой сборке (чеклист интеграции)

- [ ] компиляция UHT-типов (`TMap` в динамическом делегате `FBirdmanOnAllocated`);
- [ ] `FGameModeEvents::GameModePostLoginEvent/GameModeLogoutEvent` — сигнатуры;
- [ ] линковка `libbirdman_core.a` (libc++ vs libstdc++);
- [ ] PIE: ноль ошибок, ноль сетевых попыток (`IsManaged() == false`);
- [ ] цикл на дев-стенде против `sdk/mockagent`, затем против настоящего агента.

Контракт SDK v0 заморожен 08.07.2026 (см. `docs/specs/sdk.md` §5): сигнатуры
выше меняются только аддитивно.
