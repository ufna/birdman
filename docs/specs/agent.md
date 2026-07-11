# Спека: agent

> Один статический Go-бинарь `birdman-agent` на каждой тачке. Исполняет команды master, супервизирует дедики через containerd, мониторит тачку. Итерации 0–1, 3–4.

## 1. Обязанности / не-обязанности

**Делает:** pull/start/stop контейнеров (containerd), пул host-портов, супервизия и health дедиков, мост liba↔master, логи дедиков, метрики тачки и контейнеров, image GC, self-upgrade, UDP-echo для QoS.

**Не делает:** не решает *что* и *сколько* запускать (это reconcile master'а); не хранит durable-состояние (всё восстановимо из containerd-labels + master); не ходит в Postgres.

## 2. Состояние и рестарт

Агент — stateless поверх containerd. На старте: `containerd list` по namespace `birdman` → восстановить карту server_id→container (labels: `server_id`, `version`, `port`; уточнено в v0: плюс `state` и `match-id` — агент пишет их при переходах, чтобы после рестарта продолжить с ready/allocated, а не гонять readiness-grace заново) → доложить master в Hello. Мёртвые контейнеры (умерли, пока агент не смотрел) — Event `failed` + cleanup. Контейнеры agent-рестарт **переживают** (containerd их держит) — рестарт/апгрейд агента не трогает живые матчи.

(Уточнено в v1, mTLS.) Материал mTLS (`client.key`/`client.crt`/`ca.pem` в `tls_cert_dir`, каталог агента 0700) рестарт агента **переживает**: при действующем по сроку серте агент открывает mTLS-сессию сразу, без повторного `Enroll`. Полностью истёкший серт (нода лежала >90 дней) → mTLS-хендшейк невозможен → агент сам падает обратно в Enroll-by-token (токен на диске) и самовосстанавливается без ansible (`protocol.md` §Auth).

## 3. Запуск дедика

Команда `StartServer{server_id, image_ref, env, limits{cpu_millis, mem_mb}, port?}`:

1. Ensure image (обычно уже прогрет PrePull'ом; иначе pull с прогрессом в Event).
2. Выделить порт из пула (конфиг, деф. `20000–29999`); один порт TCP+UDP на дедик (v1).
3. Создать per-server unix socket `/run/birdman/servers/{server_id}/agent.sock` (слушает агент). (Уточнено в v0: сокет живёт в per-server каталоге, потому что монтируется каталог, а не файл — см. п. 4.)
4. `containerd create+start`: host network; bind mount: каталог `/run/birdman/servers/{server_id}` → `/birdman` (ro-каталог, rw-сокет 0666 — connect(2) к сокету работает и на ro-mount); env: `BIRDMAN_SOCKET=/birdman/agent.sock`, `BIRDMAN_PORT`, `BIRDMAN_SERVER_ID`, `BIRDMAN_REGION` + env из команды; cgroups-лимиты из `limits`; `oom_score_adj` дедикам > агенту. (Уточнено в v0: монтируется именно каталог — рестартовавший агент пересоздаёт сокет-файл, и liba реконнектится к новому inode; file-mount замораживал бы старый.)
5. Grace-период readiness: **30с** на `ready` от liba, иначе `failed` + stop.

(Уточнено в v0, Реестры v1.) Шаг 1 (Ensure image) резолвит pull-credential по host, распарсенному из `image_ref` настоящим reference-парсером (`github.com/distribution/reference`, не строковый сплит — те же правила нормализации, что у мастерской валидации `store.NormalizeRegistryHost`, `master.md` §1): цепочка «реестры от мастера (`SetRegistries`, `protocol.md` §1, точное совпадение host) → legacy `registry_auth` из `agent.yaml` (fallback, тоже host-scoped — см. §10) → анонимный pull». Credential выдаётся только при совпадении host — увод `image_ref` на чужой хост не получает наш токен (это и закрывает исходную дыру: обладатель deploy-ключа больше не может увести pull-токен, зарегистрировав версию с образом на чужом хосте); PrePull-путь использует тот же lookup, что и StartServer. `docker.io`/`index.docker.io` не поддерживаются в v1 (containerd резолвит их в `registry-1.docker.io` — точный host-match не сработал бы; master отклоняет такие host при регистрации реестра, `master.md` §6; агент, в свою очередь, **фейлит загрузку конфига**, если legacy `registry_auth.host` указывает на docker.io — мисконфиг не бутится молча). Битый legacy `token_file` (host совпал, но файл не читается) **фейлит pull**, не маскируется анонимным — так же, как и раньше, и как в `run-once` (§11). Наблюдаемость: перед каждым pull — advisory-лог `host=… source=master|legacy|anonymous` (никогда не токен) — «почему pull анонимный» дебажится по журналу агента без доступа к БД master'а.

Стейт-машина на агенте: `pulling → starting → ready → allocated → draining → stopped|failed`. Переходы `ready/allocated/draining` диктует master (allocated) и liba (ready, match_end); `failed` — exit-код ≠0, отсутствие `ready`, OOM.

`StopServer{server_id, grace_s}`: SIGTERM → grace (деф. 30с) → SIGKILL → delete container, освободить порт и сокет.

(Уточнено в v0, итерация 3.) `DrainServer{server_id, deadline_s, reason}` — per-server drain при reap deprecated-версии (deploy-менеджер, `master.md` §5): `ready|allocated → draining`, liba получает UDS-фрейм `drain{deadline_s, reason}` (кэшируется и реплеится при реконнекте liba); сигналов нет — дедик доигрывает матч и выходит сам (`match_end` → exit 0 → `stopped`, master делает `reaped`).

## 4. Health и heartbeat

- liba-heartbeat: `players`, `tick_ms`, match-события через UDS (см. `protocol.md`); тишина от liba >15с при state=allocated → пометить `unhealthy` в heartbeat (master решает).
- Процесс: exit-watch через containerd events; OOM-kill детектится и помечается в Event (панель покажет причину).
- Агрегированный heartbeat агента master'у — каждые **2с**: stats тачки (cpu, mem, disk, net, load) + список дедиков (id, state, players, tick_ms).

## 5. Логи дедиков

- stdout/stderr контейнера → `/var/log/birdman/servers/{server_id}.log`; ротация 100MB × 2 файла на дедик; после stop — gzip; ретенция **7 дней** (конфиг).
- `TailLogs{server_id, follow}` от master → стрим строк (для панели/CLI `GET /v1/servers/{id}/logs`).
- (Уточнено в v0, Логи v1 — реализовано, ветка `logs-v1`.) `vector` (контейнер, роль `birdman_agent_dev`, host-network) шипует эти же файлы в центральный VictoriaLogs (loki-push). **Агент не менялся** — vector лишь читает `/var/log/birdman/servers/*.log`, то же самое, что и live-tail; node-local ротация/ретенция (выше) — независимая ручка от ретенции VL. История/поиск по флоту — `GET /v1/logs/query` через master (`master.md` §6, `ops.md` §1), панель.

## 6. Image GC и диск

- Watermark: диск >80% → удалить неиспользуемые образы (LRU), кроме версий в состоянии `active`/`prepulling`/`deprecated`.
- Диск >90% → отказ StartServer + событие `disk_full` (алерт).

## 7. Self-upgrade

`UpgradeAgent{url, sha256, version}`: скачать во временный файл → проверить sha256 → `rename` поверх бинаря → systemd restart (контейнеры переживают, §2). Если после апгрейда агент не вышел на связь за 60с — master поднимает алерт `agent_upgrade_failed` (руками: ansible откат).

## 8. QoS-echo

UDP-echo на порту **19999**: отвечает исходным пакетом (≤64б). Клиенты меряют rtt до регионов (список отдаёт master `/v1/qos`).

## 9. Метрики

`localhost:9101/metrics` (Prometheus text): `birdman_agent_up`, `birdman_agent_servers{state}`, `birdman_server_players{server_id}`, `birdman_server_tick_ms`, per-container cpu/mem (из cgroups), диск/inode, длина пула портов. Скрейпит vmagent той же тачки (см. `ops.md`).

## 10. Конфиг `/etc/birdman/agent.yaml`

```yaml
master_addr: "master.birdman.internal:8443"
                            # итерация 5: удалённая нода через оверлей birdman — "10.77.0.1:8444"
                            # (не-loopback ⇒ tls_insecure невозможен, mTLS обязателен)
node_token: "…"            # bootstrap-токен, выдаётся при добавлении тачки (ansible)
node_token_file: /etc/birdman/node.token  # (уточнено в v0) альтернатива node_token:
                            # секрет в отдельном файле 0600 (как registry token),
                            # конфиг остаётся без секретов; inline-значение приоритетнее
region: "eu"
capacity_slots: 24          # обычно = физические ядра - резерв
port_range: [20000, 29999]
limits_default: { cpu_millis: 3500, mem_mb: 4096 }
log_dir: /var/log/birdman
data_dir: /var/lib/birdman
registry_auth:              # (уточнено в v0) pull приватного registry — bootstrap/
                            # fallback-путь (Реестры v1: основной путь — Админка →
                            # Реестры в панели, ниже)
  username: "ufna"
  token_file: /etc/birdman/ghcr.token   # токен только в файле — никогда в конфиге/коде
  host: "ghcr.io"           # (Реестры v1) host, к которому привязан этот креденшел —
                            # host-match, §3; опционально, деф. ghcr.io (единственный
                            # host, с которым говорил pre-Реестры-v1 фоллбэк) +
                            # WARN в лог один раз за процесс при срабатывании дефолта
tls_ca_file: /etc/birdman/master-ca.pem  # (уточнено в v1) bootstrap-траст: публичный
                            # CA-серт master'а, кладёт ansible; эффективный траст-пул =
                            # этот файл ∪ {tls_cert_dir}/ca.pem (оба — только публичные серты)
tls_cert_dir: ""            # (v1) каталог агента (0700, деф. {data_dir}/tls):
                            # client.key (0600, генерит агент), client.crt, ca.pem —
                            # материал mTLS, полученный при Enroll
tls_server_name: birdman-master  # (v1) SAN, проверяемый на серте master'а (DNS SAN
                            # его листа) — верификация IP-независима
tls_insecure: false         # (уточнено в v1) true — не проверять серт master; ТОЛЬКО dev
                            # и ТОЛЬКО при loopback master_addr — иначе ОШИБКА загрузки
                            # конфига (агент не стартует); см. ниже и protocol.md §Auth
```

(Уточнено в v0, Реестры v1.) Помимо `registry_auth` (файловый fallback выше), master раздаёт агенту полный набор registry-кредов по agentlink (`SetRegistries`, `protocol.md` §1) — эти креды агент держит **только в памяти** (никогда не пишет на диск, в конфиг не попадают) и получает заново при каждом Hello (после рестарта агента — из первого же снапшота от мастера, ещё до реплея pending-команд). Host-match и полная цепочка приоритетов — §3.

TLS: при первом коннекте агент обменивает `node_token` на клиентский сертификат unary-вызовом `Enroll` по server-auth TLS (ключ генерит сам, в CSR только публичная часть), сохраняет `client.key`/`client.crt`/`ca.pem` в `tls_cert_dir` атомарно (tmp+rename) и дальше ходит mTLS; при cert-сессии `node_token` в Hello **не шлёт**. Renewal (за 14 дней до истечения листа) — тем же `Enroll` поверх действующей mTLS-сессии + мягкий реконнект линка. (Уточнено в v1, mTLS — реализовано; см. `protocol.md` §Auth. `codes.Unimplemented` от старого master'а → WARN + token-auth Hello. `tls_insecure: true` легален **только** при loopback `master_addr` — агентская половина гейта итерации 5: закрывает и кражу токена, и спуфинг `UpgradeAgent`/`StartServer` по не-localhost линку; не-loopback + insecure → ошибка загрузки конфига, агент не стартует.)

## 11. Acceptance

- **Ит. 0**: `birdman-agent run-once --image ghcr.io/...` поднимает дедик на голой тачке; игрок коннектится по host:port; логи пишутся; ansible-плейбук ставит containerd+агента одной командой.
- **Ит. 1**: агент под master'ом: Start/Stop по командам, heartbeat 2с, восстановление карты после своего рестарта, failed при отсутствии ready за 30с.
- **Ит. 3**: PrePull всех тачек с прогрессом; при deploy старые дедики доигрывают.
- **Ит. 4**: tail логов через master; self-upgrade drain-aware; метрики в vmagent; UDP-echo отвечает.
