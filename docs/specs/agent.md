# Спека: agent

> Один статический Go-бинарь `birdman-agent` на каждой тачке. Исполняет команды master, супервизирует дедики через containerd, мониторит тачку. Итерации 0–1, 3–4.

## 1. Обязанности / не-обязанности

**Делает:** pull/start/stop контейнеров (containerd), пул host-портов, супервизия и health дедиков, мост liba↔master, логи дедиков, метрики тачки и контейнеров, image GC, self-upgrade, UDP-echo для QoS.

**Не делает:** не решает *что* и *сколько* запускать (это reconcile master'а); не хранит durable-состояние (всё восстановимо из containerd-labels + master); не ходит в Postgres.

## 2. Состояние и рестарт

Агент — stateless поверх containerd. На старте: `containerd list` по namespace `birdman` → восстановить карту server_id→container (labels: `server_id`, `version`, `port`; уточнено в v0: плюс `state` и `match-id` — агент пишет их при переходах, чтобы после рестарта продолжить с ready/allocated, а не гонять readiness-grace заново; уточнено tracker #1008: плюс пара владельца `birdman/project`·`birdman/env`, чтобы пер-серверные метрики оставались размеченными и после рестарта агента, §9) → доложить master в Hello. Мёртвые контейнеры (умерли, пока агент не смотрел) — Event `failed` + cleanup. Контейнеры agent-рестарт **переживают** (containerd их держит) — рестарт/апгрейд агента не трогает живые матчи.

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

- stdout/stderr контейнера → `/var/log/birdman/servers/{project}/{env}/{server_id}.log`; ротация 100MB × 2 файла на дедик; после stop — gzip; ретенция **7 дней** (конфиг).
- (Уточнено, tracker #994.) **Путь несёт пару (project, env)** — она приезжает от master'а в существующем map-поле `StartServer.env` (`BIRDMAN_PROJECT`/`BIRDMAN_ENV`, ноль диффов proto) и валидируется агентом алфавитом слага перед тем, как стать каталогом. Смысл — разметить стрим в VictoriaLogs владельцем, чтобы master мог сузить запрос привязанного ключа (`master.md` §6); статический лейбл в конфиге шиппера для этого не годится, он рендерится по ХОСТУ, а окружение ноды меняется через API без перерендера (инвариант I6). Включается флагом `log_scope_dirs` (деф. **false** — бинарь агента апгрейдится сам, а конфиг vector'а кладёт ansible; включить разметку раньше шиппера значит перестать шипповать логи вовсе, поэтому флаг ставит та же ansible-роль, что и новый `vector.yaml`). Флаг выключен, пары нет или она не проходит алфавит (старый master, run-once) → прежний плоский путь `{server_id}.log`: лог пишется как раньше, просто остаётся без лейблов. **Пара не хранится в памяти агента** (после рестарта карта серверов восстанавливается из label'ов контейнера, пары там нет) — ротация, финализация, ретенция и tail находят каталог по файловой системе (`logrot.ServerDir`), поэтому live-tail работает и для дедика, пережившего рестарт агента, и для reaped. **Раскладку читает регекс vector'а, и он закреплён тестами (tracker #1014):** `infra/roles/birdman_agent_dev/tests/run.sh` — `vector test` настоящим бинарём по набору путей; якорь `^/var/log/birdman/servers/` несущий, без него плоский путь распадается на пару `birdman/servers`.
- `TailLogs{server_id, follow}` от master → стрим строк (для панели/CLI `GET /v1/servers/{id}/logs`).
- (Уточнено в v0, Логи v1 — реализовано, ветка `logs-v1`.) `vector` (контейнер, роль `birdman_agent_dev`, host-network) шипует эти же файлы в центральный VictoriaLogs (loki-push): читает `/var/log/birdman/servers/*/*/*.log` и — для дедиков, запущенных до разметки, — плоский `/var/log/birdman/servers/*.log`, то же самое, что и live-tail; node-local ротация/ретенция (выше) — независимая ручка от ретенции VL. История/поиск — `GET /v1/logs/query` через master (`master.md` §6, `ops.md` §1), панель: по всему флоту для глобального ключа, по своей паре для привязанного (запрос сужает master; строки без пары привязанному не отдаются).

## 6. Image GC и диск

- Watermark: диск >80% → удалить неиспользуемые образы (LRU), кроме версий в состоянии `active`/`prepulling`/`deprecated`.
- Диск >90% → отказ StartServer + событие `disk_full` (алерт).
- (environments v1 §6б) **`RemoveImage{image_ref}` — точечное снятие образа отключённой (`disabled`) версии**, которое шлёт master по at-least-once очереди (семантика обеих сторон — `protocol.md` §1). Обработка как у `PrePull`: диспатч возвращается сразу (штатный `Ack`, без расширения сообщения), работа — в горутине; синхронное удаление заблокировало бы recv-цикл команд. Ветки: образ под живым контейнером → лог «in use», пропуск (watermark-GC подберёт позже); образа нет → no-op (идемпотентность под реплеем) — проверка **точечным `ImageService().Get`** (есть/нет; `NotFound`→нет), а не полным листингом; иначе `DeleteImage` (SynchronousDelete). **Каждая ветка докладывает результат мастеру ровно одним `ImageReport{status}`** — `removed|absent|busy|error` (`detail` непуст только у `error`), как `PrePull` докладывает `PullReport`, через тот же at-least-once outbox. Это не дубль `Ack`: тот подтверждает лишь ПРИЁМ команды, и без отчёта мастер штамповал маркер «образ снят» вслепую, теряя пропущенные удаления навсегда (`master.md` §Окружения). Снятый ref выкидывается из protected-set watermark-GC — мёртвый ref не занимает слот защиты (LRU-cap, ниже). Образ `deprecated`-версии НЕ трогается (окно отката тёплое); промах самолечится `EnsureImage` на StartServer. Guard общих ссылок (тот же `image_ref` держит не-disabled версия того же `(project, env)`) живёт на мастере (`master.md` §Окружения) — сюда disabled-версия с ещё-живым ref не доходит.
- (environments v1 §6в) **Dual-fs watermark:** образы containerd живут под `containerd_root` (конфиг §10, деф. `/var/lib/containerd`), который может быть ОТДЕЛЬНЫМ маунтом от `data_dir`. Watermark берёт **max двух statfs** — `data_dir` и `containerd_root`: переполнение любой из ФС триггерит GC/отказ выше. Когда это один маунт — второй statfs равен первому, поведение прежнее (нет двойного счёта). Обе ФС экспортятся парой метрик `birdman_agent_containerd_disk_{used,total}_bytes` рядом с `birdman_agent_disk_*` (§9); vmalert поднимает по каждой свою пару DiskHigh (`ops.md` §1). **(y5):** на **одной** ФС пара `birdman_agent_containerd_disk_*` численно **дублирует** пару `birdman_agent_disk_*` (тот же statfs) — обе DiskHigh-записи загорятся на одном и том же диске. Это не двойной расход (решение GC берёт max, не сумму), а дубль наблюдаемости; аннотация правила различает, о какой ФС речь. На отдельном маунте пары расходятся и каждая стережёт свою ФС.

## 7. Self-upgrade

`UpgradeAgent{url, sha256, version}`: скачать во временный файл → проверить sha256 → `rename` поверх бинаря → systemd restart (контейнеры переживают, §2). Если после апгрейда агент не вышел на связь за 60с — master поднимает алерт `agent_upgrade_failed` (руками: ansible откат).

## 8. QoS-echo

UDP-echo на порту **19999**: отвечает исходным пакетом (≤64б). Клиенты меряют rtt до регионов (список отдаёт master `/v1/qos`).

- (Уточнено, tracker #1065.) Эхо — **ресурс ХОСТА, а не ноды**. На боксе, несущем несколько нод birdman, порт занят **ровно одним** агентом в каждый момент. Причина не в экономии портов: респондер безадресный — он не несёт ни идентичности ноды, ни проекта, — а меряется им сетевой путь **до хоста**, одинаковый для всех его нод. Вдобавок `GET /v1/qos` сужен по (project, env) и берёт `select distinct region, host(public_ip)`, то есть отдаёт **один** таргет на пару (регион, ip): пер-нодовые порты выдали бы клиенту N неразличимых по rtt целей и сломали бы инвариант «единственный внешне-открытый порт ноды» (`ops.md` §4).

- (Уточнено, tracker #1068.) **Владельца эха не назначают — за порт состязаются.** Все агенты бокса пишут в конфиг один и тот же `qos_echo_addr` (§10) и пробуют его занять; чей `bind` прошёл — тот и отвечает, проигравший повторяет попытку каждые 5с и подхватывает порт, как только тот освободится. Прежняя схема (владелец — первый инстанс, остальные `off`) уносила ping-таргет бокса в могилу вместе с владельцем: `GET /v1/qos` продолжал отдавать `{host: <ip бокса>, udp_port: 19999}` **соседнему** проекту, чьи агенты живы, а клиент, не получив эха, выбрасывал регион из ранжирования целиком (`mmcli measure` пропускает регион без единого подтверждённого эха). Взаимозаменяемость владельца законна ровно по причине из #1065: респондер безадресный, поэтому **кто именно** отвечает — ненаблюдаемо, а гонка старта безвредна. Инвариант, который из этого следует: таргет бокса отдаётся проекту, только пока у того есть нода со свежим (<30с) heartbeat, то есть пока жив хотя бы один агент бокса, — а значит эхо кем-то держится (окно перехвата ≤5с). Лежат все агенты бокса — все его ноды протухают по свежести, и бокс выпадает из `/v1/qos` сам. `qos_echo_addr: off` остаётся, но означает теперь другое: «этот агент не отвечает на QoS-пробы вовсе» (явный отказ, когда порт бокса принадлежит чему-то другому); роль падает, если так сказано всем нодам бокса.

## 9. Метрики

`localhost:9101/metrics` (Prometheus text): `birdman_agent_up`, `birdman_agent_servers{state}`, `birdman_server_players{server_id}`, `birdman_server_tick_ms`, per-container cpu/mem (из cgroups), диск/inode, длина пула портов. Скрейпит vmagent той же тачки (см. `ops.md`).

- (Уточнено, tracker #1065.) Порт — **пер-нодовый** (`metrics_addr`, §10): на боксе с несколькими нодами birdman у каждой свой листенер и свой джоб скрейпа (`birdman-agent`, `birdman-agent-<имя>`), а джобы выводятся из того же списка `birdman_agent_instances`, из которого роль раскладывает сами ноды. Общий порт означал бы, что вторая нода листенер не поднимет вовсе, а её `DiskHigh`/`TickDegraded` будут молчать — молчание, неотличимое от здоровья.

- (Уточнено, tracker #1008.) **Пер-серверные серии несут пару владельца** — `birdman_server_players`, `birdman_server_tick_ms`, `birdman_container_cpu_seconds_total`, `birdman_container_memory_bytes` эмитятся как `{server_id,project,env}`. Пара приезжает от master'а в `StartServer.env` (`BIRDMAN_PROJECT`/`BIRDMAN_ENV`, та же, что размечает путь лога, §5), валидируется алфавитом слага и чеканится в label'ы контейнера `birdman/project`·`birdman/env` — поэтому рестарт агента она переживает тем же `Restore`, что порт и состояние, а не хранением в памяти. Смысл: master сужает запрос привязанного к паре ключа по `extra_label` (`master.md` §6), и без лейблов на самой серии оператор не видит графиков **своих же** дедиков. Пара ставится **только целиком** — половина (`project` без `env`) под join'ом vmalert схлопывается на беспарную серию того же `server_id` и убивает правило TickDegraded целиком (`duplicate output timeseries`, замерено на VictoriaMetrics v1.102.1), поэтому половина значит «пары нет». Label пишется при СОЗДАНИИ контейнера и не дописывается задним числом: дедик, запущенный до апгрейда, остаётся беспарным до перекрутки (и невидим привязанному ключу, пока не истечёт ретенция VM), а не мигает между двумя идентичностями серии. Флага здесь нет намеренно, в отличие от логов: у метрик нет шиппера, чей конфиг кладёт ansible.
- Платформенные и нодовые серии (`birdman_agent_*`, диск, пул портов) пары не имеют **по существу** — это данные всего хоста, привязанному ключу их видеть не положено.

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
containerd_root: ""         # (environments v1) ФС образов, если ОТДЕЛЬНЫЙ маунт от
                            # data_dir; пусто → /var/lib/containerd (§6, dual-fs
                            # watermark: max statfs data_dir и этой ФС)
node_name: ""               # (#1065) имя ЭТОЙ ноды в мастере (колонка nodes.hostname);
                            # пусто → хостнейм ОС. Задаётся, когда на боксе НЕСКОЛЬКО
                            # нод: master строит пер-нодовые серии на этом значении,
                            # и два одинаковых имени схлопывают их в один лейблсет
containerd_namespace: ""    # (#1065) namespace containerd этой ноды; пусто → birdman.
                            # Граница Restore() и image-GC: в общем namespace агенты
                            # одного бокса усыновляли бы дедики и сносили образы друг
                            # у друга
metrics_addr: "127.0.0.1:9101"   # (§9) свой порт у каждой ноды бокса
qos_echo_addr: ":19999"     # (§8) одинаков у всех нод бокса — они состязаются за порт,
                            # держит его живой; `off` — этот агент не отвечает на пробы
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
