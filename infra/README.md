# infra — ansible-обвязка birdman

Итерации 0–1: дев-нода «master + агент под ним» одной командой. Ansible — целевой путь и для прода (структура — `docs/specs/ops.md` §4).

```
infra/
  ansible.cfg                 # inventory/roles_path — запускать из infra/
  inventories/dev/hosts.local.yml # birdman-dev (общий дев-бокс; git-ignored, из hosts.example.yml)
  playbooks/dev-node.yml      # дев-нода: master (pg+бинарь+unit) → агент (демон)
  playbooks/monitoring.yml    # наблюдаемость + ops-бэкапы (итерация 4)
  playbooks/add-node.yml      # вторая+ нода: оверлей (хаб+спок) → агент (итерация 5)
  roles/birdman_master_dev/   # postgres в compose + master-бинарь под systemd
  roles/birdman_agent_dev/    # агент-демон + регистрация ноды (см. «Дев vs прод»)
  roles/birdman_monitoring_dev/  # VM+vmagent+vmalert+Grafana+alert-sink (compose) + pg-бэкапы
  roles/birdman_overlay/      # изолированный WireGuard-оверлей birdman (wireguard-go в контейнере)
```

## Запуск

```bash
# 1) собрать бинари (docker, Go на хосте не нужен)
(cd ../agent && ./build.sh)           # → agent/dist/birdman-agent
(cd ../master && ./build.sh)          # → master/dist/birdman-master

# 2) экспортировать GHCR-токен (classic PAT, скоуп read:packages).
#    Токен живёт ТОЛЬКО в env и в /etc/birdman/ghcr.token (0600 root) на тачке.
#    Никогда: в репо, в логах, в vars/host_vars.
export BIRDMAN_GHCR_TOKEN='<токен>'   # пробел перед командой = мимо shell history (zsh/bash с HIST_IGNORE_SPACE)

# 3) применить (из каталога infra/)
ansible-playbook playbooks/dev-node.yml
```

Плейбук идемпотентен (повторный прогон — `changed=0`); обновление бинаря/конфига = пересборка + повторный запуск (рестарты сервисов — по хэндлерам).

## Что делает `dev-node.yml`

Роль `birdman_master_dev` (первая — агент регистрируется в живом master):

| Объект | Что |
|---|---|
| `/opt/birdman/compose.yml` (0600) | postgres:16, порт ТОЛЬКО `127.0.0.1:5433` (5432 занят чужим продом), volume `birdman-pgdata` |
| `/etc/birdman/pg.pass` (0600) | пароль Postgres, генерируется при первом прогоне, живёт только на тачке |
| `/usr/local/bin/birdman-master` (0755) | статический бинарь из `master/dist/` — **выбранный путь деплоя**: проще сборки образа на тачке (не нужен исходник репо), логи/рестарт — штатный systemd; миграции master применяет сам при старте |
| `/etc/birdman/master.yaml` (0600) | dsn с паролем; listen ТОЛЬКО `127.0.0.1:8100` (REST) и `127.0.0.1:8444` (gRPC) — наружу не торчит, self-signed TLS в `/var/lib/birdman-master/certs` |
| `/etc/systemd/system/birdman-master.service` | enabled + started, `Restart=always` |
| `/etc/birdman/master-admin.key` (0600) | bootstrap admin-ключ, выхваченный из журнала первого старта (печатается один раз); нужен последующим задачам |

Роль `birdman_agent_dev`:

| Объект | Что |
|---|---|
| `/etc/birdman` (0755), `/var/lib/birdman`, `/var/log/birdman/servers` | каталоги агента |
| `/etc/tmpfiles.d/birdman.conf` | `/run/birdman/servers` через tmpfiles.d — переживает ребут |
| `/usr/local/bin/birdman-agent` (0755) | бинарь из `agent/dist/` |
| `/etc/birdman/agent.yaml` | region `dev`, слоты 8, порты `[20000, 20050]`, лимиты 2000m/1024MB, `master_addr 127.0.0.1:8444`, `node_token_file`, `tls_insecure: true` (dev) |
| `/etc/birdman/ghcr.token` (0600 root) | из env `BIRDMAN_GHCR_TOKEN` |
| `/etc/birdman/node.token` (0600) | одноразовая регистрация: `POST /v1/nodes` admin-ключом при первом прогоне → node_token только на тачке |
| UFW `20000:20050` tcp+udp, comment `birdman-dev` | аддитивно; чужие правила не трогаются |
| `/etc/systemd/system/birdman-agent.service` | `birdman-agent run`, enabled + started |

Это раскладка **одной** ноды на боксе (безымянный инстанс). Если нод на железе
несколько, у каждой следующей те же объекты — с суффиксом её имени; подробности
и таблица «пер-нодовое vs общее на хост» — в разделе «Несколько нод birdman на
одном боксе» ниже.

Проверка после прогона: `systemctl is-active birdman-master birdman-agent`, `curl -s localhost:8100/healthz` (на тачке), нода в `GET /v1/nodes` — `active` со свежим heartbeat.

Дальше жизнью дедиков управляет только master/agent-цикл (версии/флоты — через REST API master, см. `master/README.md`): ansible дедики не деплоит.

## Наблюдаемость + ops (`monitoring.yml`)

Итерация 4 (`docs/specs/ops.md` §1, §5). Запускать **после** `dev-node.yml`
(нужны живой `birdman-postgres` для бэкапов и цели скрейпа agent/master):

```bash
ansible-playbook playbooks/monitoring.yml
```

Роль `birdman_monitoring_dev` — отдельный compose-проект `birdman-monitoring`,
всё строго на `127.0.0.1` (наружу — только SSH-туннель):

| Сервис | Порт (127.0.0.1) | Что |
|---|---|---|
| VictoriaMetrics | 8428 | TSDB single-node, retention 30d, volume `birdman-vmdata` |
| vmagent | 8429 | скрейп 15s (host-network): agent :9101, master :8100, чужой node_exporter :4027 (read-only) → remote-write в VM |
| vmalert | 8880 | правила `ops.md §1`; query→VM, notify→sink/alertmanager |
| Grafana | 3000 | datasource VM + 2 дашборда provisioning'ом («Тачка», «birdman»); admin-пароль в `/etc/birdman/grafana_admin_password` (0600), volume `birdman-grafana` |
| alert-sink **или** alertmanager | 9094 / 9093 | по умолчанию logger-sink пишет `/var/log/birdman/alerts.log`; при `-e birdman_alert_discord_webhook=…` — реальный alertmanager с Discord |

Конфиги шаблонятся в `/etc/birdman/monitoring/…` и монтируются ro в контейнеры.

Правила алертов проверяются **локально, без выката** — `roles/birdman_monitoring_dev/tests/run.sh`:
рендерит `rules.yml.j2` и `scrape.yml.j2` настоящим ansible'ом (тем же
`tasks/scrape_jobs.yml`, что и на боксе) во временный каталог и гоняет по нему
`promtool test rules` (локальный бинарь, иначе контейнер `prom/prometheus`).
Оттуда же берётся регрессия на мёртвый `BufferEmptyReady` (tracker #960):
агрегация по отсутствующей серии даёт пусто, а не 0, поэтому master эмитит
явные нули ready-серий — правила без них не могли загореться вовсе. И там же —
бокс с ДВУМЯ нодами birdman (#1065): джоб второй ноды обязан приехать и в
`scrape.yml`, и в цепочку `absent_over_time` правила `ScrapeTargetMissing`, из
одного и того же списка.

Бэкапы Postgres исполняет сам master (Backups v1, настройка — в панели);
restore-drill — `/usr/local/bin/birdman-pg-restore-test` (роль также сносит
legacy-таймер `birdman-pg-backup`).

UFW `19999/udp` (QoS echo) открывает роль `birdman_agent_dev` (единственный
внешне-открытый порт ноды), при прогоне `dev-node.yml`.

## Несколько нод birdman на одном боксе (tracker #1065)

Нода принадлежит **ровно одному проекту** — планировщик берёт кандидатов под
запуск только среди нод своего проекта, — поэтому **новому проекту нужна своя
нода**. На уже стоящем железе она заводится вторым инстансом роли
`birdman_agent_dev`: один мощный дев-сервер тянет ряд проектов со слабой
нагрузкой, ноды разные, с разным конфигом, и шарят один айпи.

Список нод бокса — host-переменная `birdman_agent_instances` (шаблон и пример
— `inventories/dev/hosts.example.yml`). Пустое имя = легаси-нода: юнит
`birdman-agent`, `/etc/birdman/agent.yaml`, `/var/lib/birdman`, порты
20000–20050, метрики 9101 — всё как до параметризации, поэтому прогон по живым
нодам ничего не переименовывает и не регистрирует заново.

| Что | Пер-нодовое | Общее на хост |
|---|---|---|
| systemd-юнит | `birdman-agent<-имя>` | — |
| конфиг / токен | `/etc/birdman/agent<-имя>.yaml`, `node<-имя>.token` | `/etc/birdman` (master-CA, ghcr-токен) |
| каталоги | `/var/lib/birdman<-имя>`, `/var/log/birdman<-имя>`, `/run/birdman<-имя>/servers` | — |
| containerd | namespace `birdman<-имя>` | сам демон и диск под ним |
| порты | `port_range` дедиков, `metrics_port` | UDP-эхо QoS `19999` |
| наблюдаемость | джоб скрейпа на ноду | один vector, один нодовый vmagent |
| имя в мастере | `<hostname>-<имя>` (колонка `nodes.hostname`) | публичный IP |

Почему каждое из пер-нодовых — именно пер-нодовое, а не «удобнее так»:

* **containerd-namespace.** `Restore()` усыновляет любой контейнер namespace'а с
  лейблом `birdman/server-id`, а image-GC чистит образы namespace'а целиком: в
  общем namespace перезапустившийся агент забрал бы дедики соседа и отчитался
  бы ими за свою ноду.
* **`log_dir`.** Фоновая уборка агента обходит всё дерево `{log_dir}/servers` и
  гзипует любой `.log`, которого нет в её списке живых серверов, — на общем
  каталоге это порча ЖИВЫХ логов соседа (шим пишет в удалённый инод).
* **`port_range`.** Пул портов живёт в памяти агента и о соседе не знает: на
  общем диапазоне оба выдадут дедикам один порт, и второй bind упадёт уже в
  игре.
* **Имя ноды.** Master строит пер-нодовые серии на `nodes.hostname`
  (`birdman_node_heartbeat_age_seconds{node,region}`,
  `birdman_node_cert_expiry_timestamp_seconds{node}`); два одинаковых имени
  схлопывают их в один лейблсет, и `/metrics` мастера отдаёт ошибку сбора
  **целиком**, а не только по этим сериям. Мало передать имя в `POST /v1/nodes`
  — `HelloSync` переписывает колонку из `Hello.hostname` на каждом реконнекте,
  поэтому роль пинует его и в `agent.yaml` (`node_name`).
* **QoS-эхо — наоборот, общее.** Респондер безадресный (байт-зеркало ≤64б), rtt
  есть свойство сетевого пути до **хоста**, а `GET /v1/qos` и так отдаёт один
  таргет на пару (регион, ip). Владельца ему **не назначают** (#1068): за порт
  состязаются все агенты бокса, держит его тот, кто жив, проигравший
  перезахватывает его за ≤5с. Назначенный конфигом владелец гасил бы ping-таргет
  бокса вместе с собой — в том числе у СОСЕДНЕГО проекта, чью ноду мастер
  продолжает отдавать, пока её собственный агент жив.

Роль падает **до записи на диск**, если ноды бокса делят имя, порт, каталог,
токен или namespace, или если за эхо не состязается ни одна нода. Проверяется
локально, без хоста: `roles/birdman_agent_dev/tests/run.sh`.

## Вторая+ нода (`add-node.yml`, итерация 5)

Спека: `docs/specs/ops.md` (add-node/overlay). Нода
подключается к master по **собственному изолированному оверлею birdman**
(WireGuard hub-and-spoke `10.77.0.0/24`, UDP 51827): userspace `wireguard-go`
в контейнере `birdman-overlay` (host-network, NET_ADMIN, /dev/net/tun; образ
собирается на боксе, kernel-модуль не используется). Хаб (master-бокс) несёт
socat-форвардеры `10.77.0.1:{8444,9428,8428} → 127.0.0.1` — master/VL/VM не
меняют ни бинда, ни конфига. По оверлею — только control-plane (agentlink,
логи, метрики); игровой UDP и QoS — напрямую на публичный IP ноды.

```sh
(cd ../agent && ./build.sh)              # бинарь агента для новой ноды
ansible-playbook playbooks/add-node.yml  # идемпотентно; хаб + все ноды группы
```

Добавить следующую ноду = host-блок в `inventories/dev/hosts.local.yml` (регион,
`birdman_overlay_ip` из 10.77.0.0/24, `birdman_master_api_host: birdman-dev`,
`birdman_registry_legacy: false`) + прогон. Регистрация ноды (`POST
/v1/nodes`, `GET /v1/ca`) выполняется `delegate_to` master-бокса — admin-ключ
его не покидает; node_token/CA приезжают на ноду copy-тасками. Агент ноды
набирает `10.77.0.1:8444` (не-loopback ⇒ конфиг-гейт агента требует mTLS) и
заходит Enroll-by-token с первого коннекта.

С итерации 5.2 нода несёт **vmagent-сайдкар** (`birdman-node-vmagent`, гейт
`birdman_node_vmagent: true` в host-блоке): скрейпит своего агента
(`127.0.0.1:9101`) и пушит серии с лейблами `node`/`region` в центральный VM
через оверлей (`10.77.0.1:8428`) — DiskHigh/TickDegraded видят ноды. На
дев-боксе гейт выключен (его агента скрейпит центральный vmagent напрямую).
Сайдкар тоже **один на бокс**: если нод на железе несколько (#1065), он несёт
по джобу на ноду — свой порт метрик, свои лейблы `node`/`region`.

⚠️ Роль агента — та же дев-роль `birdman_agent_dev`, генерализованная на
удалённые ноды (не прод-суита ops.md §4: без hardening/vault/node_exporter).
⚠️ Инвариант: пока на хабе живёт форвардер — `agentlink_auth: mtls` only.
Снос оверлея (на КАЖДОМ боксе оверлея): `docker compose -f
/opt/birdman/overlay/compose.yml down` + удалить `/etc/birdman/overlay`,
`/opt/birdman/overlay` и образ (`docker rmi birdman-overlay:local`); UFW-правила
оверлея — только на хабе: 4 правила `birdman-dev` (51827/udp и `in on birdman-wg0`).

## Дев vs прод

- **Дев-роли** рассчитаны на общий бокс: containerd/докер уже стоят и не трогаются, никакого hardening, наружу — только UDP/TCP-порты дедиков (master строго на localhost), UFW-правила только добавляются. Всё аддитивно и легко сносится. Роль агента генерализована на удалённые ноды (итерация 5, add-node.yml) — но остаётся дев-ролью.
- **Прод (позже, по `docs/specs/ops.md` §4)**: `inventories/production/hosts.yml` (группы master/nodes_*), роли `base` (sshd, nftables, sysctl), `containerd`, `node_exporter`/`vmagent`, `birdman_agent` (node_token из vault), `birdman_master` (реальные TLS-серты вместо `tls_insecure`), `postgres`, `victoria`; вход — `add-node.yml` («тачка одной командой»). Дев-роли в прод не переиспользуем.

## Секреты

- Все секреты живут только на тачке в файлах 0600: `pg.pass`, `master-admin.key`, `node.token`, `ghcr.token` (+ dsn внутри `master.yaml`). В репо, фактах и логах ansible их нет — таски с ними `no_log`.
- GHCR-токен — только env при запуске (`lookup('env', 'BIRDMAN_GHCR_TOKEN')`). Перед коммитами: `grep -rn "ghp[_]\|bmk[_]\|bnt[_]" --exclude-dir=.git .` → ничего, кроме фейковых фикстур в тестах.
- Прод-секреты — ansible-vault (ops.md §4), появятся с прод-ролями.

## Авто-выкат дев-стенда (`birdman_devdeploy`)

Роль ставит на master-бокс pull-деплоер: `birdman-devdeploy.timer` раз в 60с
забирает дев-сборку из GHCR-пакета `ghcr.io/ufna/birdman-dev:dev` (его собирает
`.github/workflows/dev-build.yml` из ветки `main`), сверяет sha256, ставит
мастера, ждёт `/healthz` и при провале возвращает `birdman-master.prev`. Затем —
агенты окружения `dev`: адресно по `node_id`, канарейкой (сначала агент самого
master-бокса), по одной.

Дев-сборки **не публикуются релизами**: релиз — обещание пользователям, а здесь
чистый дев-стейт, поэтому Releases и теги репозитория остаются чистыми. Пакет
публичный, бокс тянет его анонимным pull-токеном реестра — учётных данных на
боксе не появляется. Целостность даёт сам реестр: каждый файл лежит отдельным
блобом, digest блоба и есть его sha256, поэтому отдельные `.sha256` не нужны.
Агенту, который качает свой бинарь сам и заголовков не умеет, деплоер отдаёт
подписанную ссылку реестра (она живёт минуты и резолвится перед каждой нодой).

Выкат идёт **pull**, а не push из CI: в публичном репозитории нет ни одного
секрета с доступом к инфраструктуре, наружу ничего не открывается.

```bash
# посмотреть, что выкачено сейчас
ssh master-box 'sudo cat /var/lib/birdman-master/deployed.json'
# журнал тиков
ssh master-box 'sudo journalctl -u birdman-devdeploy -n 50'
# выключить (переживает прогон ansible)
ssh master-box 'sudo touch /etc/birdman/devdeploy.disabled'
# или только на текущую сессию
ssh master-box 'sudo systemctl stop birdman-devdeploy.timer'
```

⚠️ Пока деплоер включён, **бинарями мастера И агента владеет он**: роли
`birdman_master_dev` и `birdman_agent_dev` ставят свой бинарь только при
bootstrap (когда его ещё нет), а прогон по живому боксу его пропускает с
заметкой в выводе. Ручная замена бинаря живёт до следующего тика — сначала
выключи деплоер.

У агента это не про лишний рестарт (#1069). Строка версии зашита в бинарь
через `-X main.version`, едет в Hello агента, и мастер сверяет её с тем, что
запросил в `POST /v1/agent-upgrade`; в CI она задаётся явно
(`VERSION="dev-${GITHUB_SHA::7}"`), а `agent/build.sh` на машине разработчика
берёт `git describe` — то есть локальная сборка ТОЙ ЖЕ ревизии несёт другую
строку (проверено: размер байт-в-байт, sha256 разные). Прогон роли без гарда
не только откатывал бы агента на чужую сборку, но и заставлял бы watchdog
слать ложные `agent_upgrade_failed`. Заодно из этого следует приятное:
добавить ноду на живой бокс можно **без локальной сборки агента** — до гарда
такой прогон падал бы уже на `src`.

Кого роль агента считает владельцем — вопрос про ОКРУЖЕНИЕ, а не про плей
(#1070). Деплоер живёт на master-боксе, а цели берёт из
`GET /v1/nodes?env=dev`, фильтруя по окружению и состоянию, а не по тому, на
каком боксе нода. Поэтому роль спрашивает мастер-бокс флота
(`birdman_master_api_host` — тот же хост, у которого она берёт admin-ключ и
CA): владелец-деплоер ⟺ на мастере лежит `/usr/local/bin/birdman-devdeploy` и
НЕ лежит `/etc/birdman/devdeploy.disabled`. Отсюда гард одинаково работает в
`dev-node.yml` и в `add-node.yml` по удалённой ноде, а self-host, где деплоера
нет вовсе, продолжает ставить бинарь ролью — и никаких новых обязательных
переменных в инвентаре для этого не нужно.

До #1070 ответ приезжал только флагом `birdman_devdeploy_enabled`, а он доезжал
до роли агента лишь там, где в плее есть роль деплоера, — то есть в
`dev-node.yml`. В `add-node.yml` её нет, и прогон по УДАЛЁННОЙ ноде откатывал
её агента на локальную сборку.

Хочешь заменить бинарь агента руками — положи на мастер-боксе
`/etc/birdman/devdeploy.disabled` (переживает прогон ansible; его же роль
деплоера кладёт при `birdman_devdeploy_enabled: false`) либо прогони плейбук с
`-e birdman_devdeploy_enabled=false`: явный флаг всегда выигрывает у пробы.

⚠️ **Правило expand/contract** (`docs/specs/ops.md` §2): миграции в цикле
`main` только добавляют. Авто-откат возвращает бинарь, но не откатывает
миграции — удаление или переименование в том же цикле сделает откат
невозможным.

Тесты деплоера (заглушки curl/systemctl, без боксов и без сети):

```bash
./infra/roles/birdman_devdeploy/tests/run.sh
```

### ⚠️ Деплоер везёт БИНАРЬ и ansible НЕ гоняет — трейлер `Needs-Ansible` (#1062)

Отсюда следует то, обо что уже спотыкались: коммит, меняющий и код, и роль,
приземляется **наполовину** — код на боксе новый, конфигурация старая, пока
плейбук не прогонят руками. Замеренный случай — `9a5db06` (#1003): `/metrics`
мастера уехал на свой листенер в коде И в роли одним коммитом, бинарь приехал,
роль нет, и стенд простоял слепым по метрикам мастера часами. Класс, а не
случай: так ведёт себя любой контракт, живущий в двух местах сразу (адрес,
порт, имя файла, дефолт флага, имя джоба — например `log_scope_dirs` из #994).

Поэтому **коммит, меняющий то, что ansible ставит, обязан нести трейлер**:

```
Needs-Ansible: monitoring.yml
Needs-Ansible: dev-node.yml, monitoring.yml
Needs-Ansible: none — правка только в комментарии шаблона
```

Проверяет `.github/workflows/infra.yml` (скрипт `infra/ci/needs-ansible-check.py`),
и это **гейт, а не ритуал**: карта «playbook → роли» парсится из самих
`infra/playbooks/*.yml`, поэтому названный playbook обязан существовать и
реально нести затронутую роль — назвал `monitoring.yml`, тронув
`birdman_master_dev`, получишь отказ с указанием верного. Трейлера не требуют
`*.md`, `infra/roles/*/tests/**` и `infra/ci/**` — они на бокс не ставятся.

Смысл трейлера — **список долгов одной командой**:

```bash
# что я ещё не прогнал с прошлого раза
git log --grep '^Needs-Ansible:' <sha последнего прогона>..main
```

Тесты гейта (одноразовые git-репозитории во временном каталоге, бокс не
трогается):

```bash
./infra/ci/tests/run.sh
```

Честная граница: трейлер делает долг **записанным, а не обнаруженным**. Разрыв,
который ломает скрейп, ловит `ScrapeTargetDown` (#1061) в пределах минут;
разрыв, который скрейп не ломает, остаётся невидимым до прогона плейбука.
Убрать класс целиком может только деплоер, умеющий догонять роль, — а это
ansible-права на общем боксе, то есть решение владельца.

## Конфиг, смонтированный в контейнер, обязан им читаться (tracker #1072/#1089)

`alertmanager.yml` клался `0600 root:root`, а `prom/alertmanager:v0.27.0` бежит
от `nobody` (uid 65534): контейнер падал на `permission denied` и стоял в
краш-лупе **с рождения** — 2380 рестартов за 40 часов, ни одного успешного
старта, доставка алертов не работала ни разу. Роль при этом отрабатывала
зелёно: она положила файл ровно так, как написано, а «так, что потребитель его
не прочитает» не проверял никто.

Теперь проверяет `infra/ci/mounted_config_access.py`. Он берёт **настоящий
рендер** compose-файлов роли, сопоставляет каждый bind-маунт с кладущей его
таской (обход от `tasks/main.yml` по `include_tasks`, с раскрытием `loop` и
`loop_var`) и требует: путь либо мирочитаем, либо принадлежит uid'у образа.
Uid **добывается у самого образа** (`docker run --entrypoint id`), а не берётся
из таблицы: таблица разъехалась бы с образом при первом же апгрейде — а апгрейд
образа и есть второй способ наступить на те же грабли.

Критерий один на репозиторий, **рендер роле-локальный**: контекст рендера у
ролей разный и выводится их же тасками (пути маунтов агентского compose
приезжают из `birdman_instances`, который нормализует `tasks/instances.yml`),
так что один generic-рендерер был бы второй правдой о раскладке. Зовут сторожа
раннеры ролей:

```bash
./infra/roles/birdman_agent_dev/tests/run.sh        # compose.yml + vmagent-compose.yml, оба бокса
./infra/roles/birdman_master_dev/tests/run.sh       # только именованный том → --allow-no-mounts
./infra/roles/birdman_monitoring_dev/tests/run.sh
./infra/roles/birdman_overlay/tests/run.sh          # hub + spoke, образ роли собирается тут же
```

Что новый compose-шаблон не остался без сторожа, следит гейт покрытия в
`./infra/ci/tests/run.sh`: роль с `templates/*compose*.j2` обязана иметь
`tests/run.sh`, зовущий сторожа, и называть в своих `tests/` каждый свой
шаблон. Роль без покрытия = красный прогон, а не молчание.

Честные границы (обе — в сторону строгости): групповые биты не учитываются
(`group_add` контейнера отсюда не виден), а контейнер от root, который по
умолчанию несёт `CAP_DAC_OVERRIDE` и прочтёт хоть `0000`, здесь всё равно
обязан пройти общий критерий — «сегодня образ бежит от root» не то свойство,
на которое стоит опираться. Раскладка, попавшая в границу, будет отвергнута:
сначала правится сторож, молча она не проедет.
