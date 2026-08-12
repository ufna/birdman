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

Что делает: берёт НАСТОЯЩИЙ рендер compose-файлов роли, достаёт из них
bind-маунты «хостовый путь → сервис → образ», сопоставляет каждый путь с той
таской роли, которая его кладёт, и требует доступности:

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
HOSTLESS_TYPES = frozenset({"volume", "tmpfs", "npipe", "cluster", "image"})
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
    """Один bind-маунт: где записан, чем смонтирован, что монтирует."""

    where: str  # имя compose-файла
    service: str
    image: str
    host: str  # хостовая сторона, КАК ЗАПИСАНА в compose
    absolute: bool  # False — относительный путь: сопоставлять не с чем, см. main()


def volume_host(vol, where: str, service: str) -> str | None:
    """Хостовая сторона тома, или None — если тома на хосте нет вовсе.

    None означает СОЗНАТЕЛЬНЫЙ пропуск (именованный том, анонимный том, tmpfs):
    хостового пути у такой записи не существует, проверять нечего. А вот
    запись, которую классифицировать НЕЛЬЗЯ, роняет прогон: молча выброшенный
    том — это ровно та дыра, из-за которой конфиг 0600 root:root, записанный
    длинным синтаксисом, проезжал мимо сторожа (tracker #1089).
    """

    def unclassifiable(why: str) -> NoReturn:
        raise SystemExit(
            f"{where}:{service}: запись тома {vol!r} — {why}. Сторож не берётся"
            " гадать, bind это или именованный том: молча пропущенный том и есть"
            " та самая дыра «выглядит покрытым», ради которой он написан. Научите"
            " разбирать эту форму — infra/ci/mounted_config_access.py."
        )

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
        return None if NAMED_VOLUME.fullmatch(source) else source
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
            unclassifiable("длинная форма без type (docker: «type is required»)")
        if vtype in HOSTLESS_TYPES:
            return None
        if vtype != "bind":
            unclassifiable(f"неизвестный type: {vtype!r}")
        source = vol.get("source")
        if not isinstance(source, str) or not source:
            # type: bind без строкового source — bind без хостовой стороны не
            # бывает, значит форма не понята.
            unclassifiable("type: bind без строкового source")
        # СКАЗАНО bind — значит источник хостовый, и прогонять его через имя
        # именованного тома нельзя: `source: pg-tuning.conf` под именованный том
        # ПОХОЖ, а разворачивает docker его в настоящий bind
        # <каталог-проекта>/pg-tuning.conf. Именно так конфиг, смонтированный
        # длинной формой с ОТНОСИТЕЛЬНЫМ source, уезжал в «bind-маунтов нет» —
        # то же тихое зелёное, ради которого сторож и писался. Относительность
        # ловит ниже общая громкая ветка, здесь её решать нечем.
        return source
    unclassifiable("не строка и не словарь")


def compose_mounts(compose: Path) -> tuple[list[Mount], int]:
    """(bind-маунты, сколько записей сознательно пропущено как не-bind)."""
    doc = yaml.safe_load(compose.read_text())
    if not isinstance(doc, dict) or not isinstance(doc.get("services"), dict) or not doc["services"]:
        raise SystemExit(
            f"{compose}: в отрендеренном compose нет ни одного сервиса — рендер сломался"
            " или файл не тот. Проверять тут нечего, и молчаливый успех здесь был бы"
            " ровно тем «выглядит покрытым», против которого этот сторож и написан."
        )
    out: list[Mount] = []
    hostless = 0
    for name, svc in doc["services"].items():
        image = (svc or {}).get("image", "")
        for vol in (svc or {}).get("volumes") or []:
            host = volume_host(vol, compose.name, name)
            if host is None:
                hostless += 1
                continue
            out.append(
                Mount(compose.name, name, image, host.rstrip("/") or "/", host.startswith("/"))
            )
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
    if not mounts and not args.allow_no_mounts:
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

    for where, service, image, host, absolute in sorted(mounts):
        tag = f"{where}:{service}"
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
        if entry is None:
            problems.append(
                f"{tag}: монтирует {host}, но НИ ОДНА таска роли {role.name} этот путь не"
                " кладёт — права на нём получатся случайными (docker создаст каталог от root)"
            )
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
    print(
        f"{role.name}: проверено маунтов: {checked}"
        f" (compose: {', '.join(c.name for c in args.compose)})"
        " — все читаются своими контейнерами"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
