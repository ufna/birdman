#!/usr/bin/env python3
"""Require a `Needs-Ansible:` trailer on commits that change what ansible installs.

WHY (tracker #1062). The dev stand runs a pull deployer: every push to main is
auto-deployed, but a deploy is a BINARY — it does not run ansible. So a commit
that changes both the code and a role lands TORN: the code on the box is new,
the configuration is old, until a human runs the playbook by hand. That is not
a one-off, it is a class: any contract shared between code and role (an
address, a port, a file name, a config flag, a job name) does the same.

The measured instance: #1003 (`9a5db06`) moved the master's /metrics onto its
own listener and, in the same commit, pointed the vmagent scrape job at the new
port. The binary arrived; the role did not; the stand was blind to every master
metric for hours, and nothing said so.

This gate does NOT close the break — it closes its INVISIBILITY, which is the
cheap half. The loud half is the ScrapeTargetDown alert of #1061. What is left
here is a written record: after this, every landed commit that owes a playbook
run says so in its own message, so the owner's pending list is one query:

    git log --grep '^Needs-Ansible:' <last-run-sha>..main

Honest limit, stated so nobody mistakes this for a fix: the debt is RECORDED,
not DETECTED. A break that stops the scrape is caught in minutes by #1061; a
break that does not (the `log_scope_dirs` default of #994 — log scoping silently
dark) stays invisible until someone runs the playbook. Only teaching the
deployer to run ansible removes the class, and that is a decision about giving
it privileges on a shared box, not a decision for this script.

WHAT MAKES IT A GATE AND NOT A RITUAL: the trailer is CHECKED, not merely
counted. The playbook→roles map is parsed out of infra/playbooks/*.yml
themselves, so there is no table here to drift; naming a playbook that does not
carry the role you touched is refused, and the refusal names the one that does.

Usage:
    needs-ansible-check.py <commit>...        # check these commits
    needs-ansible-check.py --range A..B       # check every commit in the range
Exit code 0 — clean, 1 — at least one commit is missing or has a bad trailer.
"""
from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

TRAILER = "Needs-Ansible"

# Paths that ansible actually INSTALLS on a box. Everything else under infra/ is
# repository furniture: role unit tests never leave the checkout, this directory
# is the gate itself, and prose is prose.
EXCLUDED = (
    re.compile(r"^infra/ci/"),
    re.compile(r"^infra/roles/[^/]+/tests/"),
    re.compile(r"\.md$"),
)
ROLE_PATH = re.compile(r"^infra/roles/([^/]+)/")


def git(*args: str) -> str:
    return subprocess.run(
        ["git", *args], check=True, capture_output=True, text=True
    ).stdout


def installed_paths(paths: list[str]) -> list[str]:
    out = []
    for p in paths:
        if not p.startswith("infra/"):
            continue
        if any(rx.search(p) for rx in EXCLUDED):
            continue
        out.append(p)
    return out


def playbook_roles(root: Path) -> dict[str, set[str]]:
    """playbook file name -> the roles it applies, read from the playbook itself.

    Parsed rather than tabulated on purpose: a hand-kept mapping in this file
    would be one more thing to drift out of sync, which is the very defect the
    card is about. PyYAML ships with ansible and is installed by the workflow.
    """
    import yaml

    mapping: dict[str, set[str]] = {}
    for pb in sorted((root / "infra" / "playbooks").glob("*.yml")):
        try:
            doc = yaml.safe_load(pb.read_text()) or []
        except yaml.YAMLError as exc:
            # A traceback here would read as "the gate is broken" when what is
            # actually broken is the playbook — and a playbook that does not
            # parse is a finding, not an accident. Say so in one line.
            raise SystemExit(
                f"infra/playbooks/{pb.name} не парсится как YAML — почини плейбук:\n{exc}"
            ) from None
        roles: set[str] = set()
        for play in doc:
            if not isinstance(play, dict):
                continue
            for entry in play.get("roles") or []:
                if isinstance(entry, str):
                    roles.add(entry)
                elif isinstance(entry, dict) and "role" in entry:
                    roles.add(str(entry["role"]))
        mapping[pb.name] = roles
    return mapping


def trailers(message: str) -> list[str]:
    values = []
    for line in message.splitlines():
        m = re.match(rf"^\s*{TRAILER}\s*:\s*(.*)$", line, re.IGNORECASE)
        if m:
            values.append(m.group(1).strip())
    return values


def check_commit(sha: str, root: Path, pb_roles: dict[str, set[str]]) -> list[str]:
    """Return a list of problems; empty means the commit is fine."""
    # --no-commit-id keeps merges quiet: a merge changes nothing by itself.
    paths = git(
        "show", "--pretty=format:", "--name-only", "--no-renames", sha
    ).split()
    touched = installed_paths(paths)
    if not touched:
        return []

    roles = {m.group(1) for p in touched if (m := ROLE_PATH.match(p))}
    # A playbook or inventory edit is not tied to a role; ask for any playbook.
    bare_infra = [p for p in touched if not ROLE_PATH.match(p)]

    suggestion = sorted(
        {name for name, rs in pb_roles.items() if rs & roles}
    ) or sorted(pb_roles)
    subject = git("log", "-1", "--format=%s", sha).strip()
    head = f"{sha[:12]} {subject}\n    трогает то, что ставит ansible: {', '.join(touched)}"

    values = trailers(git("log", "-1", "--format=%B", sha))
    if not values:
        return [
            f"{head}\n"
            f"    нет трейлера {TRAILER}. Допиши в тело коммита:\n"
            f"        {TRAILER}: {', '.join(suggestion)}\n"
            f"    либо, если прогон не нужен, объясни почему:\n"
            f"        {TRAILER}: none — <причина>"
        ]

    problems = []
    for value in values:
        if not value:
            problems.append(f"{head}\n    трейлер {TRAILER} пустой")
            continue
        if value.split()[0].lower().strip(",;—-") == "none":
            reason = value[len(value.split()[0]):].strip(" ,;—-")
            if not reason:
                problems.append(
                    f"{head}\n    «{TRAILER}: none» без причины. Причина обязательна —"
                    f" иначе это отписка, а не решение:\n"
                    f"        {TRAILER}: none — <почему прогон не нужен>"
                )
            continue

        named = [n.strip() for n in re.split(r"[,\s]+", value) if n.strip()]
        unknown = [n for n in named if n not in pb_roles]
        if unknown:
            problems.append(
                f"{head}\n    в трейлере названы несуществующие playbook'и:"
                f" {', '.join(unknown)}\n"
                f"    есть только: {', '.join(sorted(pb_roles))}"
            )
            continue
        covered = set().union(*(pb_roles[n] for n in named)) if named else set()
        missing = sorted(roles - covered)
        if missing:
            for role in missing:
                carriers = sorted(n for n, rs in pb_roles.items() if role in rs)
                problems.append(
                    f"{head}\n    роль {role} тронута, но НИ ОДИН из названных"
                    f" playbook'ов её не несёт.\n"
                    f"    её несёт: {', '.join(carriers) or '(ни один — роль не подключена к playbook)'}"
                )
        if bare_infra and not named:
            problems.append(f"{head}\n    трейлер не назвал ни одного playbook'а")
    return problems


def main(argv: list[str]) -> int:
    root = Path(git("rev-parse", "--show-toplevel").strip())
    args = argv[1:]
    if not args:
        sys.exit(__doc__)

    if args[0] == "--range":
        if len(args) != 2:
            sys.exit(__doc__)
        shas = git("rev-list", "--no-merges", args[1]).split()
    else:
        shas = args

    if not shas:
        print("нечего проверять: в диапазоне нет коммитов")
        return 0

    pb_roles = playbook_roles(root)
    problems = []
    for sha in shas:
        problems += check_commit(sha, root, pb_roles)

    if not problems:
        print(f"проверено коммитов: {len(shas)} — долгов по ansible не заявлено или заявлены верно")
        return 0

    print("КОММИТ МЕНЯЕТ ТО, ЧТО СТАВИТ ANSIBLE, НО НЕ ГОВОРИТ ОБ ЭТОМ.")
    print("Деплоер дев-стенда везёт БИНАРЬ и ansible не гоняет, поэтому такая")
    print("правка приземляется наполовину: код на боксе новый, конфиг старый.")
    print("Трейлер — не формальность: по нему владелец собирает список долгов")
    print(f"одной командой `git log --grep '^{TRAILER}:' <sha последнего прогона>..main`.\n")
    for p in problems:
        print(f"  · {p}\n")
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
