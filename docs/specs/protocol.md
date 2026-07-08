# Спека: протоколы

> Два контракта: master↔agent (gRPC/mTLS) и liba↔agent (NDJSON/unix socket). Оба версионируются, оба меняются только аддитивно после заморозки (итерация 2).

## 1. master ↔ agent: gRPC bidi поверх mTLS

Агент **дозванивается сам** (outbound): тачки не требуют входящих admin-портов; master слушает `:8443`.

```proto
// proto/agentlink/v1/agentlink.proto
service AgentLink {
  rpc Session (stream AgentMsg) returns (stream MasterMsg);
}

message AgentMsg {
  oneof msg {
    Hello       hello = 1;   // первым сообщением в стриме
    Heartbeat   heartbeat = 2;
    ServerEvent event = 3;
    LogChunk    log = 4;     // ответ на TailLogs
    PullReport  pull = 5;    // прогресс/результат PrePull
    Ack         ack = 6;     // подтверждение команды master по cmd_id (уточнено в v0)
  }
}
message Hello      { string node_token=1; string hostname=2; string region=3;
                     int32 capacity_slots=4; string agent_version=5;
                     repeated ServerState servers=6; }   // восстановленная карта
message Heartbeat  { int64 ts_unix_ms=1; NodeStats node=2; repeated ServerState servers=3; }
message NodeStats  { float cpu=1; uint64 mem_used=2; uint64 mem_total=3;
                     uint64 disk_used=4; uint64 disk_total=5; float load1=6; }
message ServerState{ string server_id=1; string state=2; int32 players=3;
                     float tick_ms=4; string match_id=5; int32 port=6; string version=7; }
message ServerEvent{ string server_id=1; string kind=2; string detail=3; } // failed|oom|ready|match_start|match_end

message MasterMsg {
  oneof msg {
    StartServer  start = 1;
    StopServer   stop = 2;
    PrePull      prepull = 3;   // image_ref
    Drain        drain = 4;     // node-level: не создавать, доигрывать
    UpgradeAgent upgrade = 5;   // url, sha256, version
    TailLogs     tail = 6;      // server_id, follow
    Ack          ack = 7;
    AllocateServer allocate = 8;    // (добавлено в итерации 2, аддитивно)
    DrainServer  drain_server = 9;  // (добавлено в итерации 3, аддитивно)
  }
}
message StartServer { string server_id=1; string image_ref=2; map<string,string> env=3;
                      Limits limits=4; int32 port=5; string cmd_id=6; } // port=0 → агент выберет
message Limits      { int32 cpu_millis=1; int32 mem_mb=2; }
message StopServer  { string server_id=1; int32 grace_s=2; string cmd_id=3; }

// (уточнено в v0) поля остальных сообщений — канонический источник proto/agentlink/v1/agentlink.proto:
message Ack         { string cmd_id=1; }
message PrePull     { string cmd_id=1; string image_ref=2; }
message Drain       { string cmd_id=1; string reason=2; }
message UpgradeAgent{ string cmd_id=1; string url=2; string sha256=3; string version=4; }
message TailLogs    { string cmd_id=1; string server_id=2; bool follow=3; }
message LogChunk    { string cmd_id=1; string server_id=2; bytes data=3; bool eof=4; }
message PullReport  { string cmd_id=1; string image_ref=2; string status=3; string detail=4; } // pulling|pulled|failed

// (добавлено в итерации 2) доставка матча до дедика: master шлёт после КАЖДОЙ
// успешной аллокации (и REST /v1/allocate, и встроенный матчмейкер); агент
// пересылает в liba UDS-фреймом `allocated{match_id, players_expected}` (§2)
// и кэширует его для реплея при реконнекте liba. players_expected=0 = «не знаю»
// (внешний матчмейкер через REST его не сообщает).
message AllocateServer { string cmd_id=1; string server_id=2; string match_id=3; int32 players_expected=4; }

// (добавлено в итерации 3, аддитивно) per-server drain — reap deprecated-версий
// deploy-менеджером (master.md §5): агент переводит ОДИН дедик в draining и шлёт
// liba UDS-фрейм `drain{deadline_s, reason}` (§2; кэшируется и реплеится при
// реконнекте liba, как allocated). В отличие от StopServer сигналов нет: дедик
// доигрывает матч и выходит сам; deadline_s — внутриигровой дедлайн для liba.
// Node-level Drain (поле 4) остаётся отдельной командой вывода тачки.
message DrainServer { string cmd_id=1; string server_id=2; int32 deadline_s=3; string reason=4; }
```

Правила: каждое команда-сообщение несёт `cmd_id`, агент подтверждает `Ack{cmd_id}` (или Event с ошибкой) — master ретраит неподтверждённые при реконнекте, поэтому обработка команд на агенте идемпотентна по `cmd_id` (at-least-once). Поля protobuf только добавляем, номера не переиспользуем (`reserved`). (Уточнено в v0: `Ack` добавлен в `AgentMsg` — именно агент подтверждает команды; `MasterMsg.Ack` зарезервирован под подтверждения master'ом агентских сообщений.)

**Lease/карантин:** master считает тачку живой при heartbeat моложе **10с**; тишина → `quarantine` (аллокации исключены, сервера → failed после ещё 20с); возвращение heartbeat → сверка карты серверов и `active`. (Уточнено в v0: «сверка карты» = только Hello-репорты могут воскресить `failed`-сервер обратно в `ready/allocated/draining` — дедики переживают тишину линка, master был пессимистичен; событие `server_recovered`. Обычные heartbeat'ы терминальные состояния не трогают; `reaped` не воскресает никогда.)

**Auth (bootstrap → mTLS):** ansible кладёт в конфиг одноразовый `node_token` (создан master'ом при `POST /v1/nodes`). Первый коннект — TLS (server-auth) + Hello с токеном → master выдаёт клиентский сертификат (внутренняя CA, TTL 90 дней, авто-ротация за 14 дней до истечения) → дальше строго mTLS. Токен гасится после обмена.

> **(Уточнено в v0 — осознанное отступление, TODO итерации 2.)** master v0 не выдаёт клиентские сертификаты: агент аутентифицируется `node_token`'ом в Hello при **каждом** коннекте поверх TLS (server-auth; в dev — self-signed автогенерация при первом старте). Токен соответственно пока не одноразовый и не гасится; в БД хранится только его bcrypt-хэш (`nodes.token_hash`). Полный обмен «token → клиентский mTLS-серт» с внутренней CA — следующая итерация.

## 2. liba ↔ agent: NDJSON поверх unix socket

Per-server сокет: агент слушает `/run/birdman/servers/{id}/agent.sock`; в контейнер bind-mount'ится per-server **каталог** (ro) как `/birdman`, сокет виден как `/birdman/agent.sock`. (Уточнено в v0: каталог вместо файла — рестартовавший агент пересоздаёт сокет, и liba реконнектится к новому inode; connect(2) к 0666-сокету работает и на ro-mount.) **Identity = сокет**: токены не нужны, чужой дедик физически не видит чужой сокет. Кодировка: одна JSON-строка на сообщение, `\n`-терминатор, envelope:

```json
{"v":1,"type":"...","data":{...}}
```

**liba → agent:**

| type | data | когда |
|---|---|---|
| `hello` | `{sdk_version}` | сразу после коннекта |
| `ready` | `{}` | сервер готов принимать матч (обязателен ≤30с от старта) |
| `match_start` | `{match_id}` | матч начался |
| `players` | `{count}` | при каждом изменении (+периодически ≥1/10с) |
| `match_end` | `{match_id, result}` | `completed\|aborted` — после него агент шлёт Stop-готовность master'у |
| `metric` | `{name, value}` | tick_ms и кастомные, ≤1/с на имя |
| `log` | `{level, msg}` | опционально, структурированные события в лог дедика |

**agent → liba:**

| type | data | когда |
|---|---|---|
| `allocated` | `{match_id, players_expected, metadata}` | master выдал матч на этот дедик |
| `drain` | `{deadline_s, reason}` | доиграть и завершиться (deploy/drain тачки) |
| `verify_token` | `{player_id, token} → ответ liba {ok}` | v0 опционально (join_token) |
| `ping` | `{}` | keepalive; liba отвечает `pong` |

(Уточнено в v0: агент поле `allocated.metadata` пока не шлёт (`agent/internal/uds`) — liba обязана трактовать отсутствие как пустой словарь; поле займёт своё место аддитивно.)

Правила: неизвестный `type` игнорируется (forward-compat); реконнект liba допустим (агент реплеит последний `allocated`/`drain`, liba дедуплицирует реплеи — `allocated` по `match_id`, `drain` по значению); тишина liba >15с при allocated — `unhealthy`.

## 3. Клиент игры ↔ master: REST + long-poll/SSE

См. `master.md` §6. Транспорт: HTTPS/1.1+2, JSON; long-poll `?wait=25s` — базовый механизм (проще для UE HTTP-стека), SSE — для панели. Ошибки — RFC 7807 (`application/problem+json`). Rate-limit: 5 rps per player_id на matchmaking-эндпоинты.

## 4. Совместимость и заморозка

- Envelope `v` (UDS) и package-версия proto (`agentlink.v1`) — мажор контракта; в пределах v1 только аддитивные изменения.
- Заморозка контрактов — конец итерации 2 (вместе с SDK API): после неё breaking change = новый `v2` рядом со старым, старый живёт минимум одно окно деплоя всех агентов/либ.
- Матрица «кто с кем»: master поддерживает агентов на 1 минорную версию назад; агент поддерживает liba любой версии в пределах envelope v1.
