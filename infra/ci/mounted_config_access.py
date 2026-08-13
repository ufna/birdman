#!/usr/bin/env python3
"""Каждый конфиг, который роль монтирует в контейнер, обязан этим контейнером ЧИТАТЬСЯ.

Зачем (tracker #1072). alertmanager.yml клался 0600 root:root, а
`prom/alertmanager:v0.27.0` бежит от nobody (uid 65534) — контейнер падал на
`open /etc/alertmanager/alertmanager.yml: permission denied` и стоял в
краш-лупе С РОЖДЕНИЯ: RestartCount 2380 за ~40 часов, ни одного успешного
старта. Звено vmalert → alertmanager → alert-sink не работало ни разу, история
алертов в панели пустая. Роль при этом отрабатывала ЗЕЛЁНО: она положила файл
ровно так, как написано, а «так, что потребитель его не прочитает» до этого
теста не проверял никто. Класс отказа — тот же, что у мёртвых правил (#960) и
у несторожимого скрейп-таргета (#1061): выглядит покрытым, не работает.

Почему НЕ в tests мониторинга (tracker #1089). Сторож родился роле-локальным и
смотрел один compose из четырёх: агентский, оверлейный и мастерский тем же
критерием не проверял никто. Дефекта в них на момент обобщения не было
(перепроверено), но и защиты от его появления — тоже, то есть ровно то
состояние, в котором alertmanager прожил 40 часов. Здесь лежит ОБЩИЙ КРИТЕРИЙ;
РЕНДЕР остаётся роле-локальным и делается настоящим ansible'ом в tests своей
роли — контекст рендера у ролей разный и выводится их же тасками (пути маунтов
агентского compose приезжают из birdman_instances, который нормализует
tasks/instances.yml), так что один generic-рендерер был бы второй правдой о
раскладке. Что новый compose-шаблон не остался без сторожа, следит гейт
покрытия в infra/ci/tests/run.sh.

Что делает: берёт НАСТОЯЩИЙ рендер compose-файлов роли, достаёт из них ВСЕ
пары «хостовый путь → сервис → образ», сопоставляет каждый путь с той таской
роли, которая его кладёт, и требует доступности:

  · путь мирочитаем (o+r, каталог — o+rx) → uid образа не нужен вовсе;
  · иначе владелец файла обязан совпасть с НАСТОЯЩИМ uid образа.

Тома читаются в ОБЕИХ легальных формах записи — короткой строкой
(`/etc/x:/etc/x:ro`) и длинной (`type: bind` / `source:` / `target:`), — а
запись, которую классифицировать нельзя, роняет прогон вместо тихого пропуска
(tracker #1089). Первая редакция сторожа брала только короткую форму и только
абсолютный путь, а всё остальное молча выбрасывала: конфиг 0600 root:root,
смонтированный длинным синтаксисом, проезжал мимо проверки, и compose мастера
при этом рапортовал «bind-маунтов нет вовсе, проверять нечего». Отказ был
ТИХИЙ И ЗЕЛЁНЫЙ — тот самый класс «выглядит покрытым», ради которого сторож и
писался.

Формы РАЗДЕЛЕНЫ по тому, как их классифицирует сам compose, и это тоже с
кровью: вторая редакция сторожа применяла «имя без слэша = именованный том» и
к длинной форме, где source сказан рядом с `type: bind`, — и запись `type:
bind` + `source: pg-tuning.conf` снова уезжала в «bind-маунтов нет» молча и
зелёно. Compose такую запись ПРИНИМАЕТ и разворачивает в настоящий bind
<каталог-проекта>/pg-tuning.conf. Теперь по имени классифицируется только
короткая запись (там так делает и compose), а в длинной решает type — который
docker и сам требует («type is required»). Проверять формы руками: раздел «формы
записи тома» в infra/ci/tests/run.sh.

ШЕСТЬ ДВЕРЕЙ: ПЯТЬ РАЗБИРАЮТСЯ, ЧЕТВЁРТАЯ ОТВЕРГАЕТСЯ (#1097, #1107). Перебор
форм в `services.*.volumes` был закрыт ровно настолько, насколько его кто-то
вспомнил, — и всё это время мимо сторожа шли ещё двери, которыми хостовый путь
попадает в контейнер. Дверь 1 — это вся сегодняшняя работа сторожа, она есть в
КАЖДОЙ роли. Двери 2-4 в ролях не встречались; ровно так же не встречался и
дефект #1072 в трёх composes из четырёх — за сутки до того, как встретился. А
двери 5 и 6 в ролях УЖЕ ЖИЛИ, и обе шли мимо сторожа зелёными:

  1. `services.*.volumes` c bind — разбирается здесь же (см. выше);
  2. ИМЕНОВАННЫЙ том, у которого верхнеуровневое определение несёт
     `driver_opts: {type: none, device: /srv/…, o: bind}`. У сервиса такая
     запись выглядит как `type: volume` и раньше СОЗНАТЕЛЬНО пропускалась как
     «хостовой стороны нет»; хостовый путь лежит через секцию, в которую никто
     не смотрел (tracker #1096);
  3. `configs:` / `secrets:` с `file: /srv/…`. Вне swarm compose БАЙНД-МОНТИРУЕТ
     этот файл в контейнер (а `uid`/`gid`/`mode` у записи вне swarm не работают),
     то есть права остаются ХОСТОВЫЕ — дословно #1072: 0600 root:root в
     контейнер от nobody. Сторож не видел этой двери вовсе;
  4. верхнеуровневый `include:` — документ втягивает ЦЕЛЫЕ сервисы из ДРУГОГО
     файла. Замерено на docker compose v2.32.4 (client-side, демон не нужен):
     включённый сервис приезжает в мердж вместе со своим bind'ом 0600 root:root,
     то есть дверь настоящая, а не теоретическая. Эта дверь НЕ разбирается, а
     ОТВЕРГАЕТСЯ: включаемый файл сторожу не показывают вовсе — его не видит и
     гейт покрытия в infra/ci/tests/run.sh (тот глобает `*compose*.j2`, а
     фрагмент зовётся как угодно), — так что «разобрать» тут значило бы завести
     свой резолвер путей включения и свой мердж поверх чужого;
  5. `devices:` — разбирается (`device_sources()`, tracker #1107). Хостовая
     сторона под `/dev/`, которую роль не кладёт, — сознательный ПРОПУСК с
     названной причиной: узлы создаёт ядро/udev, и вопрос «какая таска роли его
     кладёт» не имеет ответа по построению. Пропуск посчитан в отчёте, а не
     молчит. Всё прочее в `devices:` — настоящий хостовый путь и проверяется
     наравне с bind'ом: `devices: [/srv/…:/dev/x]` и есть протащенный в
     контейнер конфиг. ЭТА ДВЕРЬ В РОЛЯХ УЖЕ ОТКРЫТА: `birdman_overlay`
     монтирует `/dev/net/tun` в обоих режимах;
  6. `env_file:` / `label_file:` — разбираются (`host_tool_files()`), но ДРУГИМ
     критерием, и это замер, а не поблажка. Файл читает САМ COMPOSE на хосте от
     того, кто запускает `docker compose up` (у нас root), а контейнер его не
     открывает вовсе: содержимое приезжает переменными окружения. Замерено на
     v2.32.4 client-side — «env file /srv/… not found» и «label file /srv/…
     not found» валят прогон ещё на `config`. Значит критерий #1072 тут
     неприменим: роль мониторинга кладёт `grafana.env` 0600 root:root (таска
     «Render Grafana env_file»), и применить #1072 значило бы покраснеть на
     ней — замерено, наивный вариант так и делал. Что стенд с такой раскладкой
     живёт, отсюда НЕ проверяется и на этом вывод не висит: держит его
     механизм, а он промерен client-side. Осмысленный вопрос остаётся ровно
     один: кладёт ли путь таска роли. Длинная форма с `required: false` —
     пропуск: такой файл compose не требует (замерено). Длинная форма есть
     ТОЛЬКО у `env_file`: `label_file` в v2.32.4 — строка или список строк, и
     запись `{path: …}` compose отвергает («must be a string», RC=15).

Поэтому имя тома больше не значит «проверять нечего»: оно значит «решение
отложено до верхнеуровневой секции». Классифицируют определение —
`named_volume_host()` и `content_host()`.

РОСТ ФОРМ ГРОМКИЙ НА ТРЁХ УРОВНЯХ — И ТОЛЬКО НА НИХ. Перебирать формы бесконечно
нельзя, но можно сделать НЕперебранную форму красной. Перечислено то, что сторож
понимает, а всё остальное роняет прогон:

  · ключи САМОГО ДОКУМЕНТА (`document_keys()`): кроме `services`, `volumes`,
    `networks`, `configs`, `secrets`, `name`, `version` и расширений `x-`, любой
    верхнеуровневый ключ — отказ. Ровно этого уровня и не хватало: слова
    `include` в файле не было вовсе, то есть четвёртая дверь была не оставленной
    границей, а непросмотренной формой, и она проезжала ЗЕЛЁНОЙ. Расширения
    `x-` пропускаются не по доверию: якорь, объявленный под `x-`, раскрывает сам
    YAML-парсер ДО того, как сторож увидит документ, — сервис приезжает сюда уже
    со слитыми `volumes`, и маунт из такого якоря проверяется наравне с прочими
    (замерено, кейс в тестах);
  · ключи САМОГО СЕРВИСА (`service_keys()`, tracker #1107): кроме разобранных
    (`volumes`, `configs`, `secrets`, `devices`, `env_file`, `label_file`),
    отвергаемых (`volumes_from`, `extends`, `build`, `develop`,
    `credential_spec`), 77 поимённо перечисленных инертных и расширений `x-`,
    любой ключ сервиса — отказ. Это и был последний открытый уровень: сторож
    знал ровно пять ключей, а ключ вне пятёрки до него не доезжал ВОВСЕ, и
    `devices: [/srv/…:/dev/x]` рядом с честным bind'ом давал RC=0 и «проверено
    маунтов: 1»;
  · ключи ОПРЕДЕЛЕНИЯ тома, конфига и секрета (`definition()`): любой ключ вне
    списка понятых, как и чужой драйвер, и `external: true`, роняет прогон.

Это и есть разница между «список закрыт, пока кто-то помнит» и «список закрыт по
построению» — но ровно на этих трёх уровнях, и не шире.

ПОЧЕМУ БЕЛЫЙ СПИСОК КЛЮЧЕЙ СЕРВИСА ЗАКОНЕН, А НЕ САМОНАДЕЯН — ЗАМЕР, А НЕ
РАССУЖДЕНИЕ. Прошлая редакция этой шапки писала так: «у сервиса десятки
законных ключей …, поэтому „неизвестный ключ сервиса = отказ“ краснел бы на
живых composes ролей, а не на новых дверях, и требует своей карточки с честным
перебором, а не строчки здесь». Карточку она и просила — это она; а из двух её
звеньев замер подтвердил ПЕРВОЕ и опроверг ВТОРОЕ:

  · «десятки законных ключей» — ПРАВДА, их 88. Но набор при этом КОНЕЧЕН и
    закрыт схемой самого compose: `docker compose config`
    v2.32.4 на неизвестном ключе отвечает «services.db Additional property
    выдумка is not allowed» и выходит 15. Список здесь не вспомнен, а промерен
    по этой же схеме: 95 кандидатов, 89 приняты (88 именованных ключей плюс
    расширение `x-…`), 6 отвергнуты — пять из более поздней спеки (`provider`,
    `pull_refresh_after`, `uid_map`, `gid_map`, `use_api_socket`) и контрольная
    выдумка. Четыре списка сторожа вместе дают РОВНО эти 88: ни одного лишнего
    и ни одного пропущенного (сверено программно). Отвергнутые схемой сюда НЕ
    внесены — пин версии в том и состоит, что рост остаётся громким;
  · а вот «краснел бы на живых composes» — НЕВЕРНО, и цена оказалась нулевой:
    восемь настоящих рендеров репозитория (мастер; агент на боксе из одной и из
    двух нод — по два compose каждый; мониторинг; оверлей спок и хаб)
    используют 13 ключей сервиса из 88, и все 13 понятые. Вывод сторожа на этих
    рендерах до и после перехода отличается тремя ДОПИСАННЫМИ строками про
    ранее невидимые записи (`/dev/net/tun` у обоих режимов оверлея,
    `grafana.env` у мониторинга) и дописанной оговоркой в хвосте одной сводки;
    ни один код возврата не изменился.

Дорогим оказался не белый список, а КРИТЕРИЙ у новых дверей: наивный вариант
(«devices — обычный маунт», «env_file судить по uid образа») даёт ТРИ ложных
отказа на живых рендерах — мониторинг и оба режима оверлея. Замерено; поэтому у
двери 5 узел под `/dev/` пропускается, а у двери 6 критерий — происхождение, а
не права.

ГДЕ РОСТ НЕ ГРОМКИЙ, И ЭТО ЗАМЕРЕНО, А НЕ ЗАБЫТО. Остатков ЧЕТЫРЕ, и они
названы поимённо; первая редакция этого абзаца объявляла остаток ОДНИМ, и два
независимых прохода по ней нашли остальные три. Первые три — инертные ключи, у
которых хостовый путь есть, а класса #1072 нет, потому что читает его не
контейнер:

  · `logging`: `options: {syslog-address: "unix:///dev/log"}` — сокет открывает
    демон docker от root;
  · `security_opt`: `["seccomp:/путь/profile.json"]` — профиль читает сам
    docker/compose от root и подставляет его СОДЕРЖИМОЕ (`docker compose
    config` такую запись пропускает молча, RC=0);
  · `blkio_config`: `device_read_bps[].path`, `weight_device[].path` — это узлы
    блочных устройств, тот же класс, что пропускает дверь 5, только не
    посчитанный.

Отвергать эти ключи целиком дороже пользы: покраснел бы совершенно нормальный
`logging: json-file` + `max-size`, которого в ролях сегодня нет, но который
появится первым. Четвёртый остаток — УРОВНЕМ НИЖЕ ключей сервиса: ключи внутри
ОДНОЙ ЗАПИСИ длинной формы. Замерено на этом сторо́же: запись `{type: bind,
source: …, target: …, выдумка: 1}` даёт RC=0 и «проверено маунтов: 1» (сам
compose такую запись отвергает — «services.db.volumes.0 Additional property
выдумка is not allowed», RC=15, так что на бокс она не уедет). Промерены и
сами наборы, чтобы следующая карточка начинала с замера: запись тома —
`type, source, target, read_only, consistency, bind, volume, tmpfs`; запись
`devices` — `source, target, permissions`; запись `env_file` — `path, format,
required`. НИ ОДИН из непрочитанных сторожем ключей этих наборов хостового
пути сегодня не несёт (`bind`/`volume`/`tmpfs` — вложенные опции, `format` —
имя парсера), то есть дыры класса #1072 тут сегодня нет; открыт ровно РОСТ.
Заведена карточка #151 (1115). Так что про эти остатки сказано, как есть, и не
обещано большего.

Почему НЕ `docker compose config` (решение tracker #1097, замерено). Он и правда
авторитетнее ручного разбора для ПЕРВОЙ двери — сам разворачивает короткую и
длинную формы. Но:

  · класс он не закрывает. Вторую дверь `config` переносит в вывод как есть
    (у сервиса `type: volume`, device — в секции `volumes:`), третью — тоже:
    разбирать их всё равно руками, а это и есть основная работа;
  · он ЛОМАЕТ уже работающий громкий отказ. Замерено: `./conf` он разрешает в
    <каталог рендера>/conf, то есть сегодняшний верный диагноз («ОТНОСИТЕЛЬНЫЙ
    хостовый путь, пишите абсолютный») сменился бы уверенным враньём «ни одна
    таска роли этот путь не кладёт»;
  · он тянет docker+compose в КАЖДУЮ сьюту, включая мастерскую, которой docker
    не нужен по построению, и добавляет пин версии compose рядом с пинами
    образов.

Замерено также, что `config` ВЫБРАСЫВАЕТ верхнеуровневые volumes/configs/secrets,
на которые не ссылается ни один сервис. Для нас это верно по смыслу, но означает
ещё одну чужую логику под проверкой. Итого: ручной разбор оставлен, а работа
вложена в двери 2 и 3 и в громкий рост форм.

Таски роли обходятся ОТ tasks/main.yml по include_tasks/import_tasks — с
раскрытием loop и loop_control.loop_var, потому что часть путей роль кладёт
внутри пер-инстансного include (`{{ bmi.log_dir }}/servers` у агента), а не
плоским списком; внутрь block/rescue/always обход заходит (в роли агента там
живут и node_token 0600, и master-ca.pem). Переменные — defaults/main.yml роли
плюс `--vars` (json, который выплёвывает сам ansible-рендер: там лежат факты,
выведенные тасками роли, — их подстановка из шаблона по-другому не берётся).

uid добывается у самого образа (`docker run --rm --entrypoint id <образ> -u`),
а НЕ берётся из таблицы в тесте: таблица разъехалась бы с образом при первом же
его апгрейде — а апгрейд образа и есть второй способ наступить на те же грабли.
Проба нужна только для НЕмирочитаемых путей, поэтому стягивается один лёгкий
образ, а не весь стек.

Честные границы — обе про то, что проверка СТРОЖЕ реальности, а не слабее:

  · групповые биты не учитываются. Список дополнительных групп контейнера
    отсюда не виден (это `group_add` в compose), поэтому раскладка «0640 +
    общая группа» будет отвергнута, хотя могла бы работать;
  · контейнер, бегущий от root, по умолчанию несёт CAP_DAC_OVERRIDE и прочтёт
    хоть 0000 — здесь это НЕ моделируется намеренно. «Сегодня образ бежит от
    root» — не свойство, на которое можно опереться: ровно его смена и есть
    второй способ наступить на грабли #1072, а отказ на строгом критерии
    чинится одной правкой mode/owner;
  · ОТНОСИТЕЛЬНАЯ хостовая сторона маунта (`./conf:/etc/conf`) отвергается, а
    не разрешается. Она считается от каталога compose-файла НА БОКСЕ, а этот
    каталог отсюда не виден: угадывать его значило бы завести вторую правду о
    раскладке — ровно то, ради чего рендер оставлен роле-локальным. Пишите в
    compose абсолютный путь, тот же, что кладёт таска роли.

Ни одну из границ сторож не проходит молча: раскладка, которая в них попала,
будет отвергнута — и тогда сначала правится этот файл.

Использование:
    mounted_config_access.py --role <каталог роли> [--vars <json>]
                             [--allow-no-mounts] <отрендеренный compose>...
Код возврата 0 — все маунты читаются, 1 — есть недоступные.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import stat
import subprocess
import sys
from pathlib import Path
from typing import NamedTuple, NoReturn

import yaml

VAR = re.compile(r"\{\{\s*([a-zA-Z_]\w*(?:\.\w+)*)\s*\}\}")
# Имя ИМЕНОВАННОГО тома: по compose-спеке в нём не бывает слэша, поэтому любая
# хостовая сторона со слэшем — путь, а не имя тома (в т.ч. относительный).
# Применимо ТОЛЬКО к короткой записи: там источник классифицирует по имени сам
# compose. В длинной форме вид имени не значит ничего — там решает type.
NAMED_VOLUME = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_.-]*")
# Длинные формы, у которых хостовой стороны нет вовсе: проверять нечего.
# `volume` СЮДА НЕ ВХОДИТ (tracker #1097): за именем тома может стоять bind
# через driver_opts, и решает это верхнеуровневая секция volumes:, а не сервис.
HOSTLESS_TYPES = frozenset({"tmpfs", "npipe", "cluster", "image"})
# Ключи определения ВЕРХНЕУРОВНЕВОГО тома, которые сторож понимает. Всё, чего
# здесь нет, роняет прогон: перебрать все будущие формы нельзя, а сделать
# неперебранную форму красной — можно (tracker #1097).
VOLUME_DEF_KEYS = frozenset({"name", "labels", "driver", "driver_opts", "external"})
# То же для определения configs:/secrets:. `file` — хостовый путь (третья
# дверь), `environment`/`content` — содержимое берётся не с хоста.
CONTENT_DEF_KEYS = frozenset(
    {"name", "labels", "file", "environment", "content", "external", "template_driver"}
)
# Ключи, которыми сервис ПРИВОДИТ маунты со стороны, а не объявляет их у себя:
# `volumes_from` берёт тома другого сервиса (и подставляет под них СВОЙ образ,
# то есть свой uid), `extends` дотягивает описание из другого файла. Сторож ни
# то, ни другое не разворачивает — и молчать об этом не имеет права: маунт,
# приехавший чужим ключом, был бы не проверен и при этом зелён (tracker #1097).
IMPORTING_KEYS = ("volumes_from", "extends")
# Ключи сервиса, которые ВЕДУТ хостовый путь мимо всех разобранных дверей и
# которые сторож не разворачивает: `build` (context/dockerfile — плюс образ у
# такого сервиса не пин, и спрашивать uid не у чего), `develop` (watch[].path —
# compose синхронизирует хостовый каталог в контейнер), `credential_spec`
# (`file:`). Отказ, а не догадка (tracker #1107).
OPAQUE_PATH_KEYS = ("build", "develop", "credential_spec")
# Ключи сервиса, чей хостовый файл читает САМ COMPOSE на хосте (от того, кто
# запускает `docker compose up`, то есть от root), а НЕ контейнер. Критерий
# #1072 к ним неприменим — применим ДРУГОЙ: файла может не оказаться вовсе, и
# тогда прогон падает. Замерено на v2.32.4 client-side: «env file /srv/… not
# found», «label file /srv/… not found» — обе ещё на `config`, без демона.
HOST_TOOL_FILE_KEYS = ("env_file", "label_file")
# Хостовая сторона `devices:` — УЗЕЛ УСТРОЙСТВА, а узлы создаёт ядро/udev, и ни
# одна таска роли их не кладёт. Поэтому путь под этим префиксом, которого роль
# не кладёт, — сознательный пропуск (посчитанный, не молчаливый); всё прочее в
# `devices:` — настоящий хостовый путь и проверяется наравне с bind'ом.
DEVICE_NODE_PREFIX = "/dev/"
# Ключи САМОГО СЕРВИСА, которые не несут хостового пути ВОВСЕ (tracker #1107).
# Список не вспомнен, а ПРОМЕРЕН по схеме самого compose: 95 кандидатов прогнаны
# через `docker compose config` v2.32.4 (client-side, демон не нужен), 89
# приняты — 88 именованных ключей и расширение `x-…`. Именованные 88 разложены
# без остатка: 6 разбираются дверями, 5 отвергаются выше, 77 лежат здесь.
# Отвергнутые схемой (`provider`, `pull_refresh_after`, `uid_map`, `gid_map`,
# `use_api_socket` — они из более поздней спеки) сюда НЕ внесены сознательно:
# пин версии в том и состоит, что рост остаётся громким.
SERVICE_INERT_KEYS = frozenset({
    "annotations", "attach", "blkio_config", "cap_add", "cap_drop",
    "cgroup", "cgroup_parent", "command", "container_name", "cpu_count",
    "cpu_percent", "cpu_period", "cpu_quota", "cpu_rt_period",
    "cpu_rt_runtime", "cpu_shares", "cpus", "cpuset", "depends_on",
    "deploy", "device_cgroup_rules", "dns", "dns_opt", "dns_search",
    "domainname", "entrypoint", "environment", "expose", "external_links",
    "extra_hosts", "gpus", "group_add", "healthcheck", "hostname", "image",
    "init", "ipc", "isolation", "labels", "links", "logging",
    "mac_address", "mem_limit", "mem_reservation", "mem_swappiness",
    "memswap_limit", "network_mode", "networks", "oom_kill_disable",
    "oom_score_adj", "pid", "pids_limit", "platform", "ports",
    "post_start", "pre_stop", "privileged", "profiles", "pull_policy",
    "read_only", "restart", "runtime", "scale", "security_opt", "shm_size",
    "stdin_open", "stop_grace_period", "stop_signal", "storage_opt",
    "sysctls", "tmpfs", "tty", "ulimits", "user", "userns_mode", "uts",
    "working_dir",
})
# Ключи, которые сторож РАЗБИРАЕТ: они и составляют вместе с инертными и
# отвергаемыми полный понятый набор. Всё, чего нет ни в одном из четырёх, —
# отказ (service_keys()).
SERVICE_DOOR_KEYS = ("volumes", "configs", "secrets", "devices", *HOST_TOOL_FILE_KEYS)
# `x-…` на уровне сервиса compose тоже принимает (замерено: `x-birdman:` в
# сервисе проходит `docker compose config`), и пропускается он ровно по той же
# причине, что и на уровне документа: якоря раскрывает YAML-парсер до входа сюда.
SERVICE_EXTENSION = "x-"
# Ключи ВЕРХНЕУРОВНЕВОГО документа, которые сторож понимает. Всё, чего здесь
# нет, роняет прогон — тем же приёмом, что и неизвестный ключ определения тома
# (tracker #1097). `networks` и `name`/`version` перечислены как заведомо
# инертные: хостовый путь через них не заезжает.
DOC_KEYS = frozenset(
    {"services", "volumes", "networks", "configs", "secrets", "name", "version"}
)
# `x-…` — compose-неймспейс расширений, и он пропускается ЗАМЕРЕННО, а не по
# доверию: якоря из-под `x-` раскрывает сам YAML-парсер до входа сюда, так что
# смонтированное через `<<: *anchor` сторож видит наравне с написанным явно.
DOC_EXTENSION = "x-"
# Верхнеуровневый ключ, которым ДОКУМЕНТ приводит чужие сервисы целиком — с их
# маунтами. Тот же класс, что `extends` у сервиса, но этажом выше.
DOC_IMPORTING_KEYS = ("include",)
LAYING = {
    "ansible.builtin.template": "file",
    "ansible.builtin.copy": "file",
    "ansible.builtin.file": None,  # kind is decided by `state`
}
INCLUDING = ("ansible.builtin.include_tasks", "ansible.builtin.import_tasks")
SKIP_PROBE_ENV = "BIRDMAN_SKIP_IMAGE_UID_PROBE"

MISSING = object()


class Laid:
    """Один путь, который роль кладёт на бокс."""

    def __init__(self, path: str, kind: str, mode, owner, group, task: str, where: str):
        self.path = path
        self.kind = kind  # "file" | "dir"
        self.mode = mode  # str, как написано в таске
        self.owner = owner
        self.group = group
        self.task = task
        self.where = where

    def bits(self) -> int | None:
        # Режим обязан быть СТРОКОЙ. Голый 0600 PyYAML разберёт как
        # ВОСЬМЕРИЧНОЕ 384 (=0o600) — то есть «повезло»; настоящие грабли —
        # `mode: 644` без ведущего нуля: это десятичное 644 = 0o1204, и права
        # выйдут не те, что написаны (ansible о таком предупреждает, но
        # продолжает). Отличить «повезло» от «не повезло» по типу нельзя,
        # поэтому здесь отказ на ЛЮБОМ нестроковом режиме, а не догадка.
        if not isinstance(self.mode, str) or not re.fullmatch(r"0?[0-7]{3,4}", self.mode):
            return None
        return int(self.mode, 8)


def lookup(dotted: str, scope: dict):
    """`a.b.c` по scope; MISSING, если хоть одно звено не разрешилось."""
    parts = dotted.split(".")
    cur = scope.get(parts[0], MISSING)
    for p in parts[1:]:
        if cur is MISSING:
            return MISSING
        if isinstance(cur, dict) and p in cur:
            cur = cur[p]
        else:
            return MISSING
    return cur


def substitute(value, scope: dict):
    """Раскрывает `{{ var }}` и `{{ var.attr }}` по scope (defaults + loop-переменные)."""
    if not isinstance(value, str):
        return value
    prev = None
    out = value
    for _ in range(8):
        if out == prev:
            break
        prev = out

        def one(m):
            v = lookup(m.group(1), scope)
            return str(v) if isinstance(v, (str, int, float)) else m.group(0)

        out = VAR.sub(one, out)
    return out


def whole_expr(value: str, scope: dict):
    """Если строка — ровно один `{{ var }}`, вернуть САМО значение (список, dict)."""
    m = re.fullmatch(r"\s*\{\{\s*([a-zA-Z_]\w*(?:\.\w+)*)\s*\}\}\s*", value)
    return lookup(m.group(1), scope) if m else MISSING


def loop_of(task: dict, scope: dict):
    """[(loop_var, значение)] для таски. Без loop — одна итерация без привязки.

    Нераскрываемый loop (`{{ var }}`, которого нет в scope) даёт ПУСТОЙ список:
    таска не кладёт ни одного НАЗЫВАЕМОГО пути, и маунт на него потом честно
    упрётся в «ни одна таска роли этот путь не кладёт». Молча пропустить такую
    таску нельзя — это и была бы дыра, ради которой всё затевалось.
    """
    if "loop" not in task:
        return [(None, None)]
    var = (task.get("loop_control") or {}).get("loop_var", "item")
    loop = task["loop"]
    if isinstance(loop, str):
        resolved = whole_expr(loop, scope)
        loop = resolved if isinstance(resolved, list) else None
    if not isinstance(loop, list):
        return []
    return [(var, it) for it in loop]


def laid_paths(role: Path, variables: dict) -> dict[str, Laid]:
    """Все пути, которые роль кладёт: путь → Laid. Обход ОТ tasks/main.yml."""
    laid: dict[str, Laid] = {}
    tasks_dir = role / "tasks"

    def walk(tf: Path, scope: dict, stack: tuple[str, ...]) -> None:
        if tf.name in stack or not tf.is_file():
            return
        doc = yaml.safe_load(tf.read_text()) or []
        if not isinstance(doc, list):
            return
        walk_tasks(doc, scope, tf, stack)

    def walk_tasks(tasks: list, scope: dict, tf: Path, stack: tuple[str, ...]) -> None:
        for task in tasks:
            if not isinstance(task, dict):
                continue
            # block/rescue/always — обычный вложенный список тасок, и кладущие
            # таски внутри него роль исполняет наравне с плоскими: в роли агента
            # там лежат и node_token 0600, и master-ca.pem. Не заходить внутрь
            # значит объявить их «никем не положенными». Loop на самом block'е
            # ansible не поддерживает, раскрывать тут нечего.
            if "block" in task:
                inner = dict(scope)
                for k, v in (task.get("vars") or {}).items():
                    inner[k] = substitute(v, inner)
                for section in ("block", "rescue", "always"):
                    body = task.get(section)
                    if isinstance(body, list):
                        walk_tasks(body, inner, tf, stack)
                continue
            module = next(
                (m for m in (*LAYING, *INCLUDING) if m in task), None
            )
            if module is None:
                continue
            for var, item in loop_of(task, scope):
                inner = dict(scope)
                if var is not None:
                    inner[var] = item
                for k, v in (task.get("vars") or {}).items():
                    inner[k] = substitute(v, inner)
                if module in INCLUDING:
                    spec = task[module]
                    name = spec if isinstance(spec, str) else (spec or {}).get("file")
                    if isinstance(name, str):
                        walk(tasks_dir / substitute(name, inner), inner, (*stack, tf.name))
                    continue
                record(task, module, inner, tf)

    def record(task: dict, module: str, scope: dict, tf: Path) -> None:
        args = task[module] or {}
        if not isinstance(args, dict):
            return
        dest = args.get("dest") or args.get("path")
        if not isinstance(dest, str):
            return
        kind = LAYING[module]
        if kind is None:
            state = substitute(args.get("state", "file"), scope)
            if state == "absent":
                return
            kind = "dir" if state == "directory" else "file"
        p = substitute(dest, scope)
        if "{{" in p or not p.startswith("/"):
            return
        laid[p.rstrip("/")] = Laid(
            path=p.rstrip("/"),
            kind=kind,
            mode=substitute(args.get("mode"), scope),
            owner=substitute(args.get("owner"), scope),
            group=substitute(args.get("group"), scope),
            task=str(task.get("name", "(без имени)")),
            where=f"{tf.parent.name}/{tf.name}",
        )

    walk(tasks_dir / "main.yml", dict(variables), ())
    return laid


class Mount(NamedTuple):
    """Один хостовый путь, доехавший до контейнера: чем смонтирован и что."""

    where: str  # имя compose-файла
    service: str
    image: str
    host: str  # хостовая сторона, КАК ЗАПИСАНА в compose
    absolute: bool  # False — относительный путь: сопоставлять не с чем, см. main()
    door: str  # какой дверью пришёл — попадает в отчёт, чтобы её было видно
    # КТО этот путь читает, и от этого зависит критерий (tracker #1107):
    # "container" — контейнер, то есть дословно #1072 (uid образа против прав);
    # "compose"   — сам compose на хосте от root, и тогда права ни при чём, а
    #               осмысленный вопрос ровно один: кладёт ли путь таска роли.
    reader: str = "container"


def unclassifiable(where: str, what: str, why: str) -> NoReturn:
    """Отказ на форме, которую сторож не понял. Одна дверь — один текст.

    Молча пропущенная запись и есть та дыра, из-за которой конфиг 0600
    root:root проезжал мимо сторожа (tracker #1089/#1097): «не понял» обязано
    выглядеть как отказ, а не как «проверять нечего».
    """
    raise SystemExit(
        f"{where}: {what} — {why}. Сторож не берётся гадать, ведёт ли эта запись"
        " на хостовый путь: молча пропущенная дверь и есть та самая дыра"
        " «выглядит покрытым», ради которой он написан. Научите разбирать эту"
        " форму — infra/ci/mounted_config_access.py."
    )


class Source(NamedTuple):
    """Что монтирует запись тома: путь на хосте либо ИМЯ верхнеуровневого тома."""

    kind: str  # "path" | "named"
    value: str


def volume_source(vol, where: str, service: str) -> Source | None:
    """Источник тома, или None — если хостовой стороны нет ПО СПЕКЕ.

    None означает СОЗНАТЕЛЬНЫЙ пропуск (анонимный том, tmpfs): хостового пути
    у такой записи не существует. А вот ИМЯ тома больше не пропуск, а
    отложенное решение: за именем может стоять bind через driver_opts, и это
    вторая дверь (tracker #1096/#1097) — её разбирает named_volume_host().
    """

    def bail(why: str) -> NoReturn:
        unclassifiable(f"{where}:{service}", f"запись тома {vol!r}", why)

    if isinstance(vol, str):
        # Короткая форма. Одна только строка без двоеточия — АНОНИМНЫЙ том, и
        # путь в ней контейнерный, а не хостовый: принять его за хостовый значит
        # выдумать маунт, которого нет.
        if ":" not in vol:
            return None
        # Классификация по ИМЕНИ уместна ровно здесь и больше нигде: в короткой
        # записи compose сам решает по источнику — со слэшем путь, без слэша имя
        # тома (`- conf:/etc/conf` он трактует как ссылку на именованный том и
        # ругается «refers to undefined volume conf»).
        source = vol.split(":", 1)[0]
        if NAMED_VOLUME.fullmatch(source):
            return Source("named", source)
        return Source("path", source)
    if isinstance(vol, dict):
        # Длинная форма: {type, source, target, ...}. Легальна ровно так же, как
        # короткая, и первая редакция сторожа выбрасывала её целиком. Здесь всё
        # решает type, а НЕ вид source: угадывать по имени то, что уже сказано
        # словом, — это и была дыра второго круга (tracker #1089).
        vtype = vol.get("type")
        if not isinstance(vtype, str) or not vtype:
            # `type` в длинной форме обязателен, docker и сам такой compose не
            # берёт («services.db.volumes.0 type is required»), — значит, это не
            # «форма без type», а запись, которую мы не поняли.
            bail("длинная форма без type (docker: «type is required»)")
        if vtype in HOSTLESS_TYPES:
            return None
        if vtype == "volume":
            # Решение отложено: `source` — это ИМЯ, и что за ним стоит, знает
            # только верхнеуровневая секция volumes:. Без source том анонимный,
            # хостовой стороны у него нет.
            name = vol.get("source")
            if name is None:
                return None
            if not isinstance(name, str) or not name:
                bail("type: volume с нестроковым source")
            return Source("named", name)
        if vtype != "bind":
            bail(f"неизвестный type: {vtype!r}")
        source = vol.get("source")
        if not isinstance(source, str) or not source:
            # type: bind без строкового source — bind без хостовой стороны не
            # бывает, значит форма не понята.
            bail("type: bind без строкового source")
        # СКАЗАНО bind — значит источник хостовый, и прогонять его через имя
        # именованного тома нельзя: `source: pg-tuning.conf` под именованный том
        # ПОХОЖ, а разворачивает docker его в настоящий bind
        # <каталог-проекта>/pg-tuning.conf. Именно так конфиг, смонтированный
        # длинной формой с ОТНОСИТЕЛЬНЫМ source, уезжал в «bind-маунтов нет» —
        # то же тихое зелёное, ради которого сторож и писался. Относительность
        # ловит ниже общая громкая ветка, здесь её решать нечем.
        return Source("path", source)
    bail("не строка и не словарь")


def section(doc: dict, key: str) -> dict:
    """Верхнеуровневая секция compose как словарь (её может не быть вовсе)."""
    got = doc.get(key)
    return got if isinstance(got, dict) else {}


def definition(doc: dict, key: str, name: str, known: frozenset, tag: str, what: str):
    """Верхнеуровневое определение `name` из секции `key`, уже провалидированное.

    Ссылка на НЕОБЪЯВЛЕННОЕ имя — отказ, и это не придирка: docker такой compose
    сам не берёт («refers to undefined volume»). Ключ вне `known` — тоже отказ:
    так рост форм остаётся громким, а не проезжает зелёным (tracker #1097).
    """
    top = section(doc, key)
    if name not in top:
        unclassifiable(
            tag, f"{what} «{name}»",
            f"его нет в верхнеуровневой секции {key}: (docker и сам такой compose"
            " не берёт)",
        )
    spec = top[name]
    if spec is None:
        spec = {}
    if not isinstance(spec, dict):
        unclassifiable(tag, f"{what} «{name}»", "определение — не словарь")
    unknown = sorted(set(spec) - known)
    if unknown:
        unclassifiable(
            tag, f"{what} «{name}»",
            f"в определении ключи, которых сторож не знает: {', '.join(unknown)}",
        )
    if spec.get("external"):
        unclassifiable(
            tag, f"{what} «{name}»",
            "объявлен external — его содержимое описано НЕ здесь, а создать его"
            " могли и с хостовым путём (`docker volume create -o type=none -o"
            " device=/srv/… -o o=bind`); отсюда этого не видно",
        )
    return spec


def named_volume_host(doc: dict, name: str, tag: str) -> str | None:
    """ВТОРАЯ ДВЕРЬ: хостовый путь за именем тома, или None — если его нет.

    `driver_opts: {type: none, device: /srv/…, o: bind}` — это настоящий bind
    хостового каталога, но у сервиса он выглядит как `type: volume`, и раньше
    сторож пропускал его как «хостовой стороны нет» (tracker #1096).
    """
    spec = definition(doc, "volumes", name, VOLUME_DEF_KEYS, tag, "том")
    driver = spec.get("driver")
    opts = spec.get("driver_opts")
    if driver is not None and driver != "local":
        unclassifiable(
            tag, f"том «{name}»",
            f"драйвер «{driver}» — не local; чужой драйвер волен смонтировать что"
            " угодно, в том числе хостовый каталог",
        )
    if opts is None:
        return None  # обычный том под управлением docker: хостового пути нет
    if not isinstance(opts, dict):
        unclassifiable(tag, f"том «{name}»", "driver_opts — не словарь")
    otype = opts.get("type")
    if otype == "tmpfs":
        return None
    flags = {p.strip() for p in str(opts.get("o", "")).split(",")}
    if otype == "none" and "bind" in flags:
        device = opts.get("device")
        if not isinstance(device, str) or not device:
            unclassifiable(
                tag, f"том «{name}»", "bind через driver_opts без строкового device"
            )
        return device
    unclassifiable(
        tag, f"том «{name}»",
        f"driver_opts {opts!r} — это не «type: none + o: bind» (хостовый каталог)"
        " и не tmpfs",
    )


def content_host(doc: dict, kind: str, name: str, tag: str) -> str | None:
    """ТРЕТЬЯ ДВЕРЬ: хостовый путь за `configs:`/`secrets:`, или None.

    Вне swarm compose БАЙНД-МОНТИРУЕТ `file:` в контейнер, а `uid`/`gid`/`mode`
    у записи там не работают — права остаются хостовые. Это дословно #1072:
    0600 root:root в контейнер, бегущий от nobody (tracker #1097).
    """
    what = "конфиг" if kind == "configs" else "секрет"
    spec = definition(doc, kind, name, CONTENT_DEF_KEYS, tag, what)
    if "file" in spec:
        path = spec["file"]
        if not isinstance(path, str) or not path:
            unclassifiable(tag, f"{what} «{name}»", "file: не строка")
        return path
    # Содержимое не с хоста: compose берёт его из переменной окружения или из
    # самого compose-файла, и класть его таской роли некому.
    if "environment" in spec or "content" in spec:
        return None
    unclassifiable(
        tag, f"{what} «{name}»",
        "в определении нет ни file:, ни environment:, ни content: — откуда"
        " берётся содержимое, непонятно",
    )


def content_names(svc: dict, kind: str, tag: str):
    """Имена configs:/secrets:, на которые ссылается сервис (обе формы записи)."""
    for ref in svc.get(kind) or []:
        if isinstance(ref, str):
            yield ref
            continue
        if isinstance(ref, dict):
            src = ref.get("source")
            if not isinstance(src, str) or not src:
                unclassifiable(
                    tag, f"запись {kind} {ref!r}", "длинная форма без строкового source"
                )
            yield src
            continue
        unclassifiable(tag, f"запись {kind} {ref!r}", "не строка и не словарь")


def device_sources(svc: dict, tag: str):
    """ПЯТАЯ ДВЕРЬ: хостовые стороны `devices:` (обе формы, tracker #1107).

    Замерено на compose v2.32.4: короткая запись — `HOST[:CONTAINER[:PERMS]]`
    (без двоеточия хостовая сторона равна контейнерной, `config` разворачивает
    её в `source == target`), длинная — `{source, target, permissions}`.
    """
    for dev in svc.get("devices") or []:
        if isinstance(dev, str):
            if not dev:
                unclassifiable(tag, "запись devices ''", "пустая строка")
            yield dev.split(":", 1)[0]
            continue
        if isinstance(dev, dict):
            src = dev.get("source")
            if not isinstance(src, str) or not src:
                unclassifiable(
                    tag, f"запись devices {dev!r}", "длинная форма без строкового source"
                )
            yield src
            continue
        unclassifiable(tag, f"запись devices {dev!r}", "не строка и не словарь")


def host_tool_files(svc: dict, key: str, tag: str):
    """ШЕСТАЯ ДВЕРЬ: `env_file:`/`label_file:` — хостовый файл, который читает
    САМ COMPOSE (tracker #1107).

    Не маунт: содержимое приезжает в контейнер переменными окружения (или
    лейблами), а файл открывает `docker compose` на хосте от root — то есть
    критерий #1072 тут неприменим, и применить его значило бы покраснеть на
    живом рендере мониторинга (grafana.env лежит 0600 root:root и работает).
    Осмысленный вопрос другой: файл обязан существовать, иначе прогон падает
    ещё на `config` («env file … not found», замерено client-side). Длинная
    форма с `required: false` — сознательный пропуск: такой файл compose не
    требует (замерено там же).
    """
    spec = svc.get(key)
    if spec is None:
        return
    entries = spec if isinstance(spec, list) else [spec]
    for item in entries:
        if isinstance(item, str):
            if not item:
                unclassifiable(tag, f"запись {key} ''", "пустая строка")
            yield item
            continue
        if isinstance(item, dict):
            if item.get("required") is False:
                continue  # compose такой файл не требует — пропуск сознательный
            path = item.get("path")
            if not isinstance(path, str) or not path:
                unclassifiable(
                    tag, f"запись {key} {item!r}", "длинная форма без строкового path"
                )
            yield path
            continue
        unclassifiable(tag, f"запись {key} {item!r}", "не строка и не словарь")


def service_keys(svc: dict, tag: str) -> None:
    """ТРЕТИЙ УРОВЕНЬ РОСТА ФОРМ: ключи САМОГО СЕРВИСА (tracker #1107).

    Между двумя уже закрытыми уровнями — ключами документа и ключами
    определения тома — лежал третий, и он был открыт: сторож знал ровно пять
    ключей сервиса, а ключ вне пятёрки до него не доезжал вовсе. Замерено:
    `devices: [/srv/…:/dev/x]` рядом с честным bind'ом давал RC=0 и «проверено
    маунтов: 1» — уверенное «всё хорошо» при непроверенном хостовом пути.

    Почему белый список тут законен ровно так же, как на уровне документа:
    набор ключей сервиса КОНЕЧЕН и закрыт схемой самого compose. Замерено —
    `docker compose config` v2.32.4 на неизвестном ключе отвечает «services.db
    Additional property выдумка is not allowed» и выходит 15. Список ниже не
    вспомнен, а промерен по этой же схеме, и цена его промерена тоже: восемь
    настоящих рендеров репозитория (мастер; агент на боксе из одной и из двух
    нод; мониторинг; оверлей спок и хаб) используют 13 ключей, и все 13 понятые.
    """
    for key in OPAQUE_PATH_KEYS:
        if key in svc:
            unclassifiable(
                tag, f"сервис использует {key}:",
                "этим ключом в контейнер заезжает ХОСТОВЫЙ путь (context/dockerfile"
                " у build, watch[].path у develop, file: у credential_spec), а сторож"
                " его не разворачивает; у build к тому же образ не пин — спрашивать"
                " uid не у чего",
            )
    unknown = sorted(
        str(k)
        for k in svc
        if not (
            str(k).startswith(SERVICE_EXTENSION)
            or k in SERVICE_INERT_KEYS
            or k in SERVICE_DOOR_KEYS
            or k in IMPORTING_KEYS
        )
    )
    if unknown:
        unclassifiable(
            tag, f"ключи сервиса: {', '.join(unknown)}",
            "их нет среди понятых (список промерен по схеме docker compose"
            " v2.32.4), а ключ сервиса волен привести в контейнер хостовый путь —"
            " как это делают devices:, env_file: и build:",
        )


def document_keys(doc: dict, where: str) -> None:
    """ЧЕТВЁРТАЯ ДВЕРЬ и её класс: ключи САМОГО ДОКУМЕНТА (tracker #1097).

    `include:` втягивает в проект ЦЕЛЫЕ сервисы из другого файла — со всеми их
    маунтами. Замерено на docker compose v2.32.4: включённый сервис приезжает в
    мердж вместе со своим bind'ом 0600 root:root, а сторож на том же файле
    печатал «проверено маунтов: 1 — все читаются» и выходил нулём, потому что
    включаемого файла он не читает вовсе. Это ровно тот тихий зелёный, ради
    которого он написан, — и дыра была не в разборе, а УРОВНЕМ ВЫШЕ: неизвестный
    ключ определения тома прогон ронял, а неизвестный ключ документа — нет.
    """
    for key in DOC_IMPORTING_KEYS:
        if key in doc:
            unclassifiable(
                where, f"верхнеуровневый {key}:",
                "им документ втягивает ЦЕЛЫЕ сервисы из ДРУГОГО файла, вместе с их"
                " маунтами (замерено на compose v2.32.4: включённый сервис приезжает"
                " в мердж со своим bind'ом), а сторожу тот файл не показывают —"
                " его не видит и гейт покрытия, который глобает *compose*.j2."
                " Чинится в роли: пишите сервисы в самом compose-шаблоне",
            )
    unknown = sorted(
        str(k) for k in doc if not (str(k).startswith(DOC_EXTENSION) or k in DOC_KEYS)
    )
    if unknown:
        unclassifiable(
            where, f"верхнеуровневые ключи документа: {', '.join(unknown)}",
            "их нет среди понятых (services, volumes, networks, configs, secrets,"
            " name, version и расширения x-), а ключ документа волен привести в"
            " контейнер что угодно — как это делает include:",
        )


def compose_mounts(compose: Path) -> tuple[list[Mount], int]:
    """(хостовые маунты всеми дверями, сколько записей сознательно пропущено)."""
    doc = yaml.safe_load(compose.read_text())
    if isinstance(doc, dict):
        # ДО проверки на «есть ли сервисы»: документ из одного `include:` —
        # законный compose без секции services, и он обязан получить свой
        # диагноз, а не «рендер сломался».
        document_keys(doc, compose.name)
    if not isinstance(doc, dict) or not isinstance(doc.get("services"), dict) or not doc["services"]:
        raise SystemExit(
            f"{compose}: в отрендеренном compose нет ни одного сервиса — рендер сломался"
            " или файл не тот. Проверять тут нечего, и молчаливый успех здесь был бы"
            " ровно тем «выглядит покрытым», против которого этот сторож и написан."
        )
    out: list[Mount] = []
    hostless = 0
    for name, svc in doc["services"].items():
        svc = svc or {}
        image = svc.get("image", "")
        tag = f"{compose.name}:{name}"

        for key in IMPORTING_KEYS:
            if key in svc:
                unclassifiable(
                    tag, f"сервис использует {key}:",
                    "он приводит маунты, описанные НЕ в этом сервисе, а сторож их"
                    " не разворачивает; такой маунт остался бы непроверенным и"
                    " при этом зелёным — а под volumes_from ещё и подставляется"
                    " ДРУГОЙ образ, то есть другой uid",
                )
        # Всё, что не разобрано и не отвергнуто поимённо, роняет прогон здесь же:
        # уровень ключей сервиса закрыт по построению, а не по памяти (#1107).
        service_keys(svc, tag)

        def add(host: str, door: str, reader: str = "container") -> None:
            out.append(
                Mount(
                    compose.name, name, image, host.rstrip("/") or "/",
                    host.startswith("/"), door, reader,
                )
            )

        # Дверь 1: тома сервиса. Имя тома отправляется во вторую дверь.
        for vol in svc.get("volumes") or []:
            src = volume_source(vol, compose.name, name)
            if src is None:
                hostless += 1
                continue
            if src.kind == "named":
                host = named_volume_host(doc, src.value, tag)
                if host is None:
                    hostless += 1
                    continue
                add(host, f"том «{src.value}» (driver_opts bind)")
                continue
            add(src.value, "volumes")

        # Дверь 3: configs:/secrets: с file:.
        for kind in ("configs", "secrets"):
            for ref in content_names(svc, kind, tag):
                host = content_host(doc, kind, ref, tag)
                if host is None:
                    hostless += 1
                    continue
                add(host, f"{kind}: «{ref}»")

        # Дверь 5: devices: — узел устройства решается в main(), где видно, что
        # роль кладёт, а что нет.
        for dev in device_sources(svc, tag):
            add(dev, "devices")

        # Дверь 6: env_file:/label_file: — читает их compose на хосте, не контейнер.
        for key in HOST_TOOL_FILE_KEYS:
            for path in host_tool_files(svc, key, tag):
                add(path, f"{key}:", reader="compose")
    return out, hostless


def owner_uid(owner) -> int | None:
    """uid владельца — только если он задан ЧИСЛОМ."""
    if owner is None:
        return None
    s = str(owner)
    if s.isdigit():
        return int(s)
    if s == "root":
        return 0
    return None  # имя пользователя ХОСТА с uid внутри образа не сопоставимо


def docker(*args: str) -> subprocess.CompletedProcess:
    """docker с одной внятной строкой вместо traceback'а, если его нет вовсе.

    Отсутствие docker'а обязано выглядеть как отказ проверки, а не как поломка
    теста: сторож зовут раннеры трёх ролей, и стек-трейс из subprocess читается
    как «тест сломался», хотя сломано окружение.
    """
    try:
        return subprocess.run(["docker", *args], capture_output=True, text=True)
    except (FileNotFoundError, PermissionError) as exc:
        return subprocess.CompletedProcess(
            args=("docker", *args), returncode=127, stdout="",
            stderr=f"docker не запускается: {exc}",
        )


def image_uid(image: str, cache: dict[str, int | None]) -> tuple[int | None, str]:
    """Настоящий uid, под которым бежит образ. None + причина, если не добыть."""
    if image in cache:
        uid = cache[image]
        return uid, "" if uid is not None else "не удалось определить uid образа"
    probe = docker("run", "--rm", "--entrypoint", "id", image, "-u")
    if probe.returncode == 0 and probe.stdout.strip().isdigit():
        cache[image] = int(probe.stdout.strip())
        return cache[image], ""
    # Образ без `id` (scratch-based): спрашиваем метаданные. Пустой USER = root.
    meta = docker("image", "inspect", "--format", "{{.Config.User}}", image)
    if meta.returncode == 0:
        user = meta.stdout.strip().split(":")[0]
        if user == "":
            cache[image] = 0
            return 0, ""
        if user.isdigit():
            cache[image] = int(user)
            return cache[image], ""
        cache[image] = None
        return None, (
            f"образ объявляет пользователя «{user}», а `id -u` в нём не работает"
            " — uid не определить"
        )
    cache[image] = None
    # ПЕРВАЯ строка, а не последняя: docker кладёт причину первой, а подсказку
    # «See 'docker run --help'» — последней, и отчёт с одной подсказкой вместо
    # «Cannot connect to the Docker daemon» не даёт понять, что чинить.
    err = [ln for ln in (probe.stderr or meta.stderr or "").splitlines() if ln.strip()]
    return None, f"docker недоступен или образ не стягивается: {err[0].strip() if err else '?'}"


def world_access(entry: Laid, need_x: bool) -> bool:
    """Доступен «остальным» — тогда uid контейнера не нужен вовсе."""
    bits = entry.bits()
    if bits is None:
        return False
    need = stat.S_IROTH | (stat.S_IXOTH if need_x else 0)
    return bits & need == need


def readable(entry: Laid, uid: int, need_x: bool) -> tuple[bool, str]:
    bits = entry.bits()
    if bits is None:
        return False, f"режим «{entry.mode}» не строка вида \"0644\""
    if world_access(entry, need_x):
        return True, "мирочитаем"
    own = owner_uid(entry.owner)
    if own is None:
        return False, (
            f"владелец «{entry.owner}» задан не числом — сопоставить его с uid"
            " контейнера нельзя (имя на хосте и uid в образе — разные вселенные)"
        )
    if own != uid:
        return False, (
            f"режим {entry.mode} не даёт доступа «остальным», а владелец uid={own}"
            f" — не uid контейнера ({uid})"
        )
    user_need = stat.S_IRUSR | (stat.S_IXUSR if need_x else 0)
    if bits & user_need != user_need:
        return False, f"владелец совпал, но режим {entry.mode} не даёт доступа и ему"
    return True, f"владелец = uid контейнера ({uid})"


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(add_help=True, description=__doc__.splitlines()[0])
    ap.add_argument("--role", required=True, type=Path, help="каталог роли")
    ap.add_argument("--vars", type=Path, help="json с фактами, выведенными рендером роли")
    ap.add_argument(
        "--allow-no-mounts", action="store_true",
        help="у роли compose без bind-маунтов — законно (compose мастера: только именованный"
             " том). НЕ отмычка от непонятой записи: её сторож роняет до этой проверки",
    )
    ap.add_argument("compose", nargs="+", type=Path, help="отрендеренные compose-файлы")
    args = ap.parse_args(argv[1:])

    role: Path = args.role
    if not (role / "tasks" / "main.yml").is_file():
        raise SystemExit(f"{role}: не похоже на роль — нет tasks/main.yml")
    variables = yaml.safe_load((role / "defaults" / "main.yml").read_text()) or {}
    if args.vars:
        extra = json.loads(args.vars.read_text())
        if not isinstance(extra, dict):
            raise SystemExit(f"{args.vars}: ожидался json-объект переменных")
        variables.update(extra)
    laid = laid_paths(role, variables)

    mounts: list[Mount] = []
    hostless = 0
    for compose in args.compose:
        if not compose.is_file():
            raise SystemExit(f"{compose}: файла нет — рендер до него не дошёл?")
        found, ignored = compose_mounts(compose)
        mounts += found
        hostless += ignored
    # «Есть ли что проверять» решают ТОЛЬКО двери 1-3: устройство под /dev/ и
    # файл, который читает сам compose, bind-маунтом не являются, и зачесть их
    # сюда значило бы погасить этот громкий отказ чужой записью (tracker #1107).
    binds = [m for m in mounts if m.reader == "container" and m.door != "devices"]
    if not binds and not args.allow_no_mounts:
        print(
            f"{role.name}: ни в одном из {len(args.compose)} compose нет bind-маунтов —"
            " они уехали из compose или рендер отдал не то. Осознанно (только именованные"
            " тома): --allow-no-mounts",
            file=sys.stderr,
        )
        return 1

    skip_probe = os.environ.get(SKIP_PROBE_ENV) == "1"
    cache: dict[str, int | None] = {}
    problems: list[str] = []
    checked = 0
    nodes = 0  # узлы устройств: пропущены сознательно и ПОСЧИТАНЫ (#1107)
    host_read = 0  # файлы, которые читает сам compose на хосте (env_file/label_file)

    for where, service, image, host, absolute, door, reader in sorted(mounts):
        # Дверь названа в КАЖДОЙ строке отчёта: путь, пришедший через
        # `configs:` или через driver_opts, глазами в compose не находится по
        # самому пути — искать его надо в другой секции (tracker #1097).
        tag = f"{where}:{service}" + ("" if door == "volumes" else f" [{door}]")
        if not absolute:
            # Разобрать разобрали, а вот СОПОСТАВИТЬ не с чем: относительный путь
            # считается от каталога compose-файла на боксе, и знать его отсюда
            # неоткуда. Отказ громкий — тихо пропустить значит вернуть дыру.
            problems.append(
                f"{tag}: монтирует «{host}» — ОТНОСИТЕЛЬНЫЙ хостовый путь. Он"
                " разрешается от каталога compose-проекта НА БОКСЕ, а сторож"
                " сопоставляет маунты с тасками роли по абсолютному пути и этого"
                " каталога не знает; угадывать его — заводить вторую правду о"
                " раскладке.\n    Чинится в compose: пишите абсолютный путь, тот"
                " же, что кладёт таска роли."
            )
            continue
        entry = laid.get(host)
        if door == "devices" and entry is None and host.startswith(DEVICE_NODE_PREFIX):
            # Узел устройства: его создаёт ядро/udev, и «какая таска роли его
            # кладёт» — вопрос без ответа по построению, а не забытая проверка.
            # Пропуск сознательный, НАЗВАННЫЙ и посчитанный: `devices:` с путём
            # ВНЕ /dev/ (или с тем, который роль всё же кладёт) сюда не попадает
            # и проверяется наравне с bind'ом — это и есть протащенный в
            # контейнер хостовый файл, ради которого дверь открывали (#1107).
            print(f"~  {tag}: {host} — узел устройства (создаёт ядро, не таска роли)")
            nodes += 1
            continue
        if entry is None:
            problems.append(
                f"{tag}: монтирует {host}, но НИ ОДНА таска роли {role.name} этот путь не"
                " кладёт — права на нём получатся случайными (docker создаст каталог от root)"
                if reader == "container" else
                f"{tag}: читает {host}, но НИ ОДНА таска роли {role.name} этот путь не"
                " кладёт — `docker compose up` упрётся в «not found» ещё до старта"
                " контейнера (замерено на v2.32.4: это отказ уже на `config`)"
            )
            continue
        if reader == "compose":
            # Файл читает сам compose на хосте от root, а не контейнер: права
            # тут ни при чём (замерено — grafana.env лежит 0600 root:root и
            # работает), и вопрос был ровно один — кладёт ли его роль.
            print(f"ok   {tag}: {host} — читает сам compose на хосте (права ни при чём)")
            host_read += 1
            continue
        # Сам путь + все вышележащие каталоги, которые кладёт роль: режим файла
        # ни о чём, если в каталог по дороге не войти.
        targets = [(entry, entry.kind == "dir", host)]
        for parent in sorted(Path(host).parents, key=lambda p: len(str(p))):
            pe = laid.get(str(parent))
            if pe is not None and pe.kind == "dir":
                targets.append((pe, True, str(parent)))

        closed = [t for t in targets if not world_access(t[0], t[1])]
        if not closed:
            print(f"ok   {tag}: {host} — мирочитаем (uid образа не нужен)")
            checked += 1
            continue

        # uid образа нужен ТОЛЬКО здесь: мирочитаемый путь читает кто угодно,
        # и стягивать ради него образ незачем.
        if skip_probe:
            bad = [
                f"{p}: режим {e.mode}, владелец «{e.owner}»"
                for e, _, p in closed
                if owner_uid(e.owner) in (None, 0)
            ]
            if bad:
                problems.append(
                    f"{tag}: закрыто для «остальных» и не годится контейнеру, который"
                    " бежит не от root:\n      " + "\n      ".join(bad)
                )
            else:
                print(
                    f"~  {tag}: {host} — владелец числовой и не root, НО с ОБРАЗОМ"
                    f" {image} он НЕ СВЕРЕН ({SKIP_PROBE_ENV}=1)"
                )
                checked += 1
            continue

        uid, why = image_uid(image, cache)
        if uid is None:
            problems.append(
                f"{tag}: {host} закрыт для «остальных», а uid образа {image} не"
                f" добыть — {why}.\n    Проба: docker run --rm --entrypoint id {image} -u"
                f"\n    Осознанно без пробы: {SKIP_PROBE_ENV}=1 (проверка ослабнет"
                " до «владелец не root»)"
            )
            continue

        failed = False
        for e, need_x, p in targets:
            ok, why = readable(e, uid, need_x)
            if ok:
                continue
            failed = True
            what = "не пройдёт в каталог" if p != host else "не прочитает"
            problems.append(
                f"{tag} ({image}, uid={uid}) {what} {p}: {why}\n"
                f"    кладёт: «{e.task}» ({e.where}), owner={e.owner}"
                f" group={e.group} mode={e.mode}"
            )
        if not failed:
            print(f"ok   {tag}: {host} — владелец = uid образа ({uid})")
            checked += 1

    print()
    if not mounts:
        # «Маунтов нет» ≠ «не разобрал»: неклассифицируемая запись сюда не
        # доезжает вовсе — volume_host() роняет прогон раньше. Поэтому число
        # разобранных не-bind записей печатается: ноль при непустом compose —
        # повод смотреть на рендер, а не радоваться зелёному.
        print(
            f"{role.name}: bind-маунтов нет (--allow-no-mounts); разобрано записей"
            f" томов без хостовой стороны: {hostless} (именованные тома, tmpfs,"
            f" анонимные) — compose: {', '.join(c.name for c in args.compose)}"
        )
        return 0
    if problems:
        print(f"КОНФИГ РОЛИ {role.name} ПОЛОЖЕН ТАК, ЧТО КОНТЕЙНЕР ЕГО НЕ ПРОЧИТАЕТ.")
        print("Роль при этом отработает зелёно, а сервис будет падать на старте —")
        print("молча, до первого человека, заглянувшего в docker logs.\n")
        for p in problems:
            print(f"  · {p}\n")
        return 1
    # Двери 5 и 6 попадают в сводку ОТДЕЛЬНЫМИ числами, а не вливаются в
    # «проверено маунтов»: критерий у них другой, и слить их значило бы соврать
    # о том, что именно проверено (tracker #1107).
    extra = ""
    if host_read:
        extra += f"; файлов, читаемых самим compose: {host_read}"
    if nodes:
        extra += f"; узлов устройств пропущено: {nodes}"
    print(
        f"{role.name}: проверено маунтов: {checked}"
        f" (compose: {', '.join(c.name for c in args.compose)})"
        f" — все читаются своими контейнерами{extra}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
