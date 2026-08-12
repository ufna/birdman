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

Что делает: берёт НАСТОЯЩИЙ рендер compose.yml (шаблон играется ansible'ом в
tests/render_rules.yml), достаёт из него bind-маунты «хостовый путь → сервис →
образ», сопоставляет каждый путь с той таской роли, которая его кладёт
(mode/owner/group, с раскрытием loop и подстановкой переменных из
defaults/main.yml), и требует доступности:

  · путь мирочитаем (o+r, каталог — o+rx) → uid образа не нужен вовсе;
  · иначе владелец файла обязан совпасть с НАСТОЯЩИМ uid образа.

uid добывается у самого образа (`docker run --rm --entrypoint id <образ> -u`),
а НЕ берётся из таблицы в тесте: таблица разъехалась бы с образом при первом же
его апгрейде — а апгрейд образа и есть второй способ наступить на те же грабли.
Проба нужна только для НЕмирочитаемых путей, поэтому стягивается один лёгкий
образ, а не весь стек.

Честная граница: групповые биты не учитываются. Список дополнительных групп
контейнера отсюда не виден (это `group_add` в compose), поэтому раскладка «0640
+ общая группа» будет отвергнута, хотя могла бы работать. Роль групп не
использует; если такая раскладка появится, сначала правится этот тест — молча
он её не пропустит.

Использование:
    mounted_config_access.py <отрендеренный compose.yml>
Код возврата 0 — все маунты читаются, 1 — есть недоступные.
"""
from __future__ import annotations

import os
import re
import stat
import subprocess
import sys
from pathlib import Path

import yaml

VAR = re.compile(r"\{\{\s*([a-zA-Z_]\w*)\s*\}\}")
LAYING = {
    "ansible.builtin.template": "file",
    "ansible.builtin.copy": "file",
    "ansible.builtin.file": None,  # kind is decided by `state`
}
SKIP_PROBE_ENV = "BIRDMAN_SKIP_IMAGE_UID_PROBE"


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
        # Режим обязан быть СТРОКОЙ: голый 0600 в YAML — это десятичное 600
        # (ansible о таком предупреждает, но продолжает), т.е. права выйдут не
        # те, что написаны. Здесь это отказ, а не догадка.
        if not isinstance(self.mode, str) or not re.fullmatch(r"0?[0-7]{3,4}", self.mode):
            return None
        return int(self.mode, 8)


def substitute(value, variables: dict, item=None):
    """Раскрывает `{{ var }}` по defaults роли (и `{{ item }}` внутри loop)."""
    if not isinstance(value, str):
        return value
    prev = None
    out = value
    for _ in range(8):
        if out == prev:
            break
        prev = out

        def one(m):
            name = m.group(1)
            if name == "item" and item is not None:
                return str(item)
            v = variables.get(name)
            return str(v) if isinstance(v, (str, int, float)) else m.group(0)

        out = VAR.sub(one, out)
    return out


def laid_paths(role: Path, variables: dict) -> dict[str, Laid]:
    """Все пути, которые роль кладёт: путь → Laid. Читает ВСЕ tasks/*.yml роли."""
    laid: dict[str, Laid] = {}
    for tf in sorted((role / "tasks").glob("*.yml")):
        doc = yaml.safe_load(tf.read_text()) or []
        if not isinstance(doc, list):
            continue
        for task in doc:
            if not isinstance(task, dict):
                continue
            module = next((m for m in LAYING if m in task), None)
            if module is None:
                continue
            args = task[module] or {}
            if not isinstance(args, dict):
                continue
            dest = args.get("dest") or args.get("path")
            if not isinstance(dest, str):
                continue
            kind = LAYING[module]
            if kind is None:
                state = args.get("state", "file")
                if state == "absent":
                    continue
                kind = "dir" if state == "directory" else "file"
            loop = task.get("loop")
            items = loop if isinstance(loop, list) else [None]
            for it in items:
                if isinstance(it, dict):  # loop по словарям роль тут не использует
                    continue
                p = substitute(dest, variables, it)
                if "{{" in p or not p.startswith("/"):
                    continue
                laid[p.rstrip("/")] = Laid(
                    path=p.rstrip("/"),
                    kind=kind,
                    mode=substitute(args.get("mode"), variables, it),
                    owner=substitute(args.get("owner"), variables, it),
                    group=substitute(args.get("group"), variables, it),
                    task=str(task.get("name", "(без имени)")),
                    where=f"{tf.parent.name}/{tf.name}",
                )
    return laid


def compose_mounts(compose: Path) -> list[tuple[str, str, str]]:
    """[(сервис, образ, хостовый путь)] — только bind-маунты, именованные тома мимо."""
    doc = yaml.safe_load(compose.read_text()) or {}
    out = []
    for name, svc in (doc.get("services") or {}).items():
        image = svc.get("image", "")
        for vol in svc.get("volumes") or []:
            if not isinstance(vol, str):
                continue
            host = vol.split(":", 1)[0]
            if not host.startswith("/"):  # named volume
                continue
            out.append((name, image, host.rstrip("/")))
    return out


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


def image_uid(image: str, cache: dict[str, int | None]) -> tuple[int | None, str]:
    """Настоящий uid, под которым бежит образ. None + причина, если не добыть."""
    if image in cache:
        uid = cache[image]
        return uid, "" if uid is not None else "не удалось определить uid образа"
    probe = subprocess.run(
        ["docker", "run", "--rm", "--entrypoint", "id", image, "-u"],
        capture_output=True, text=True,
    )
    if probe.returncode == 0 and probe.stdout.strip().isdigit():
        cache[image] = int(probe.stdout.strip())
        return cache[image], ""
    # Образ без `id` (scratch-based): спрашиваем метаданные. Пустой USER = root.
    meta = subprocess.run(
        ["docker", "image", "inspect", "--format", "{{.Config.User}}", image],
        capture_output=True, text=True,
    )
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
    err = (probe.stderr or meta.stderr or "").strip().splitlines()
    return None, f"docker недоступен или образ не стягивается: {err[-1] if err else '?'}"


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
    if len(argv) != 2:
        sys.exit(__doc__)
    compose = Path(argv[1])
    role = Path(__file__).resolve().parent.parent
    variables = yaml.safe_load((role / "defaults" / "main.yml").read_text()) or {}
    laid = laid_paths(role, variables)
    mounts = compose_mounts(compose)
    if not mounts:
        print("в compose нет ни одного bind-маунта — глоб перестал совпадать?", file=sys.stderr)
        return 1

    skip_probe = os.environ.get(SKIP_PROBE_ENV) == "1"
    cache: dict[str, int | None] = {}
    problems: list[str] = []
    checked = 0

    for service, image, host in sorted(mounts):
        entry = laid.get(host)
        if entry is None:
            problems.append(
                f"{service}: монтирует {host}, но НИ ОДНА таска роли этот путь не кладёт"
                " — права на нём получатся случайными (docker создаст каталог от root)"
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
            print(f"ok   {service}: {host} — мирочитаем (uid образа не нужен)")
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
                    f"{service}: закрыто для «остальных» и не годится контейнеру, который"
                    " бежит не от root:\n      " + "\n      ".join(bad)
                )
            else:
                print(
                    f"~  {service}: {host} — владелец числовой и не root, НО с ОБРАЗОМ"
                    f" {image} он НЕ СВЕРЕН ({SKIP_PROBE_ENV}=1)"
                )
                checked += 1
            continue

        uid, why = image_uid(image, cache)
        if uid is None:
            problems.append(
                f"{service}: {host} закрыт для «остальных», а uid образа {image} не"
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
                f"{service} ({image}, uid={uid}) {what} {p}: {why}\n"
                f"    кладёт: «{e.task}» ({e.where}), owner={e.owner}"
                f" group={e.group} mode={e.mode}"
            )
        if not failed:
            print(f"ok   {service}: {host} — владелец = uid образа ({uid})")
            checked += 1

    print()
    if problems:
        print("КОНФИГ ПОЛОЖЕН ТАК, ЧТО КОНТЕЙНЕР ЕГО НЕ ПРОЧИТАЕТ.")
        print("Роль при этом отработает зелёно, а сервис будет падать на старте —")
        print("молча, до первого человека, заглянувшего в docker logs.\n")
        for p in problems:
            print(f"  · {p}\n")
        return 1
    print(f"проверено маунтов: {checked} — все читаются своими контейнерами")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
