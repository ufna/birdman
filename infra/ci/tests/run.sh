#!/usr/bin/env bash
# Repo-wide infra gates — LOCAL only, no host is touched and this repository is
# never mutated: every case builds a throwaway git repository in a temp dir,
# commits into it, and runs the checker there.
#
#   ./infra/ci/tests/run.sh
#
# THREE gates live here, all about invariants that belong to no single role:
#   1. the Needs-Ansible trailer (tracker #1062) — cases 1..15 below;
#   2. coverage of the mount watchdog (tracker #1089): every compose template of
#      every role must be checked by infra/ci/mounted_config_access.py from that
#      role's own test suite;
#   3. the volume FORMS that watchdog understands (tracker #1089) — the tail of
#      this file. Its parser dropped a legal form silently twice in a row (the
#      long syntax as a whole, then `type: bind` with a relative `source:`), and
#      both times the form was pinned by hand at review time and by nothing
#      afterwards. Hence a standing table: every form is either a mount or a
#      LOUD refusal, never a quiet skip.
#
# Why a gate needs tests of its own: a check that silently passes everything is
# worse than no check, because the case then LOOKS covered — the same failure
# mode as the dead buffer alerts (#960) and the missing scrape alert (#1061).
# Every case below therefore pins a REFUSAL as well as an acceptance.
#
# And the same rule applied to THIS FILE: a runner that dies mid-way must be
# RED, never a silent zero. That one bit us here for real (see the sentinel
# below), so it is pinned by cases too — the tail of this file mutates this
# very script and asserts the exit code both ways.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
self="$here/$(basename "${BASH_SOURCE[0]}")"
checker="$(dirname "$here")/needs-ansible-check.py"
root="$(cd "$here/../../.." && pwd)"

work="$(mktemp -d)"
# ЧАСОВОЙ, А НЕ ЧТЕНИЕ `$?`. Прогон, упавший ПОСРЕДИНЕ (например, на `set -u`),
# выходил с НУЛЁМ: суммарная строка «прошло/упало» до конца не доезжала, а
# вызвавший видел зелёное. Ровно то «выглядит покрытым», против чего написаны
# все три гейта ниже, — и обиднее всего в самом их раннере.
#
# НАСТОЯЩАЯ ПРИЧИНА, ЗАМЕРЕНА В ЛОБ И РОВНО В ЭТИХ ГРАНИЦАХ: bash 3.2
# (/bin/bash на macOS) при ОДНОВРЕМЕННО включённых `-e` и `-u` — то есть при
# `set -euo pipefail` в строке выше — не доносит до EXIT-trap'а код выхода по
# ОШИБКЕ РАСКРЫТИЯ. Воспроизводитель целиком, `set -e` в нём обязателен:
#
#     set -ue; trap 'echo "TRAP:$?"' EXIT; : "${nope}"      -> TRAP:0, наружу 0
#     set -u;  trap 'echo "TRAP:$?"' EXIT; : "${nope}"      -> TRAP:1, наружу 1
#
# Границы замерены, а не додуманы: под `-ue` так ведёт себя именно раскрытие
# (неопределённая переменная, `${arr[0]}` пустого массива, `${x:?}` — все 0), а
# обычное падение команды доезжает верно (`false` -> 1, несуществующая команда
# -> 127, явный `exit 3` -> 3). Поэтому НИ ОДИН trap, читающий `$?`, этот режим
# не восстанавливает: `st=$?` захватывает ноль и честно отдаёт его наружу.
# (Первая попытка чинить именно так — `st=$?; rm; exit "$st"` — была
# поведенчески НЕОТЛИЧИМА от голого `rm -rf`, замерено по всем восьми режимам;
# и диагноз «rm затирает код» был неверен: при голом `rm -rf` `set -e`,
# `exit N`, pipefail и честный красный финал все отдают ненулевой код.)
# Про bash 5 (то есть про CI) здесь замера НЕТ — этой оболочки на машине нет, а
# в вердикте по #1089 «на CI отдаёт 1 штатно» сказано ОГОВОРКОЙ, не замером;
# принимать это за измеренное не следует. Твёрдо известно лишь то, что дыру
# ловили локально, и что мерж-гейт на ней ни разу не покраснел.
#
# Инвариант часового версии bash не касается вовсе: до финала дошли — верим
# коду, не дошли — красное. Ненулевой код не трогается никогда, поэтому явный
# `exit N`, `set -e`, pipefail и честный красный финал приходят наружу как
# есть. Обе стороны пришпилены кейсами в конце файла.
reached_end=0
# Путь ЖИВОГО мутанта раннера роли (блок «часовой раннеров ролей» в конце
# файла). Мутант кладётся В ДЕРЕВО РОЛИ, поэтому обрыв прогона обязан унести
# его с собой — иначе следующий читатель найдёт в roles/ чужой огрызок.
# Инициализация ДО trap'а несущая: trap читает эту переменную, а под `set -u`
# неинициализированная уронила бы его в детях `trap_case`.
mutant=""
trap 'st=$?; rm -rf "$work"; [ -z "$mutant" ] || rm -f "$mutant"; if [ "$reached_end" != 1 ] && [ "$st" -eq 0 ]; then st=1; fi; exit "$st"' EXIT

fail=0
pass=0

# scaffold — a miniature of the real layout: two playbooks carrying different
# roles, so "you named the wrong playbook" is a case that can actually happen.
scaffold() {
	local repo="$1"
	mkdir -p "$repo"/infra/playbooks "$repo"/infra/roles/birdman_master_dev/defaults \
		"$repo"/infra/roles/birdman_monitoring_dev/templates \
		"$repo"/infra/roles/birdman_monitoring_dev/tests "$repo"/master
	cat >"$repo/infra/playbooks/dev-node.yml" <<-'YML'
		---
		- name: dev node
		  hosts: birdman_dev
		  roles:
		    - birdman_master_dev
		    - role: birdman_devdeploy
	YML
	cat >"$repo/infra/playbooks/monitoring.yml" <<-'YML'
		---
		- name: monitoring
		  hosts: birdman_dev
		  roles:
		    - birdman_monitoring_dev
	YML
	git -C "$repo" init -q
	git -C "$repo" config user.email t@example.com
	git -C "$repo" config user.name test
	git -C "$repo" add -A
	git -C "$repo" commit -qm "scaffold"
}

# case <name> <expected-exit> <file-to-touch> <commit message> [text the output must contain]
# The touch is a COMMENT line, not arbitrary junk: one of the files under test
# is a playbook, and the checker parses those — corrupting it would test the
# YAML error path instead of the trailer rule (that path has its own case).
case_run() {
	local name="$1" want="$2" file="$3" msg="$4" expect_text="${5:-}"
	local repo="$work/$(echo "$name" | tr -c 'a-zA-Z0-9' '_')"
	scaffold "$repo"
	mkdir -p "$(dirname "$repo/$file")"
	echo "# changed" >>"$repo/$file"
	git -C "$repo" add -A
	git -C "$repo" commit -qF - <<<"$msg"

	local out rc=0
	out="$(cd "$repo" && python3 "$checker" HEAD 2>&1)" || rc=$?
	if [ "$rc" != "$want" ]; then
		echo "FAIL $name: код возврата $rc, ожидался $want"
		echo "$out" | sed 's/^/      /'
		fail=$((fail + 1))
		return
	fi
	if [ -n "$expect_text" ] && ! printf '%s' "$out" | grep -qF "$expect_text"; then
		# Скобки обязательны: за именем стоит МНОГОБАЙТНАЯ кавычка, и без них
		# bash забирает её первый байт в имя переменной (unbound variable).
		echo "FAIL $name: в выводе нет «${expect_text}»"
		echo "$out" | sed 's/^/      /'
		fail=$((fail + 1))
		return
	fi
	echo "ok   $name"
	pass=$((pass + 1))
}

# 1. THE DEFECT ITSELF: a role file changed, nothing said. This is #1003.
case_run "инфра-правка без трейлера отвергается" 1 \
	"infra/roles/birdman_monitoring_dev/templates/rules.yml.j2" \
	"fix(infra): правка роли" \
	"Needs-Ansible: monitoring.yml"

# 2. ...and the suggestion is derived from the playbooks, not from a table:
#    a master-role change must be told to run dev-node.yml.
case_run "подсказка берётся из playbook'а, несущего затронутую роль" 1 \
	"infra/roles/birdman_master_dev/defaults/main.yml" \
	"fix(infra): правка роли мастера" \
	"Needs-Ansible: dev-node.yml"

# 3. The happy path.
case_run "верный трейлер принимается" 0 \
	"infra/roles/birdman_monitoring_dev/templates/rules.yml.j2" \
	"fix(infra): правка роли

Needs-Ansible: monitoring.yml"

# 4. WHAT MAKES IT A GATE AND NOT A RITUAL: naming a playbook that does not
#    carry the role you touched is refused, and the refusal names the right one.
case_run "чужой playbook в трейлере отвергается" 1 \
	"infra/roles/birdman_master_dev/defaults/main.yml" \
	"fix(infra): правка роли мастера

Needs-Ansible: monitoring.yml" \
	"её несёт: dev-node.yml"

# 5. A playbook that does not exist at all.
case_run "несуществующий playbook отвергается" 1 \
	"infra/roles/birdman_monitoring_dev/templates/rules.yml.j2" \
	"fix(infra): правка роли

Needs-Ansible: nosuch.yml" \
	"несуществующие"

# 6. The escape hatch exists — with a reason.
case_run "none с причиной принимается" 0 \
	"infra/roles/birdman_monitoring_dev/templates/rules.yml.j2" \
	"fix(infra): правка комментария

Needs-Ansible: none — правка только в комментарии шаблона"

# 7. ...and without one it is an evasion, not a decision.
case_run "none без причины отвергается" 1 \
	"infra/roles/birdman_monitoring_dev/templates/rules.yml.j2" \
	"fix(infra): правка комментария

Needs-Ansible: none" \
	"без причины"

# 8. Role unit tests never leave the checkout — no debt, no nagging. A gate
#    that cries on every commit gets a rubber stamp, and then it protects
#    nothing.
case_run "правка tests/ роли трейлера не требует" 0 \
	"infra/roles/birdman_monitoring_dev/tests/rules_test.yml" \
	"test(infra): кейс в тестах роли"

# 9. Prose is prose.
case_run "правка .md трейлера не требует" 0 \
	"infra/README.md" \
	"docs(infra): README"

# 10. The gate itself is not installed on any box either.
case_run "правка infra/ci трейлера не требует" 0 \
	"infra/ci/needs-ansible-check.py" \
	"ci: правка самого гейта"

# 11. A pure code commit is none of this gate's business.
case_run "коммит без infra/ гейт не трогает" 0 \
	"master/main.go" \
	"feat(master): что-то в коде"

# 12. A playbook or inventory edit is not tied to a role, but it is still
#     installed state — it must declare too.
case_run "правка самого playbook'а тоже требует трейлера" 1 \
	"infra/playbooks/monitoring.yml" \
	"fix(infra): правка плейбука" \
	"нет трейлера"

# 13. THE TORN LANDING, exactly as it happened in #1003: code and role in one
#     commit. The binary auto-deploys, the role does not — the case the whole
#     card is about.
case_run "код+роль в одном коммите (случай #1003) требует трейлера" 1 \
	"infra/roles/birdman_monitoring_dev/templates/rules.yml.j2" \
	"fix(master+infra): порт метрик уехал в коде и в роли" \
	"трогает то, что ставит ansible"

# 14. A playbook that does not parse must produce ONE readable line, not a
#     traceback: a stack trace reads as "the gate is broken" when what is
#     broken is the playbook.
broken="$work/broken"
scaffold "$broken"
printf 'this is: not: a playbook\n' >>"$broken/infra/playbooks/monitoring.yml"
git -C "$broken" add -A
git -C "$broken" commit -qm "fix(infra): битый плейбук"
rc=0
out="$(cd "$broken" && python3 "$checker" HEAD 2>&1)" || rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -qF "не парсится как YAML" &&
	! printf '%s' "$out" | grep -qF "Traceback"; then
	echo "ok   битый плейбук: одна внятная строка, не traceback"
	pass=$((pass + 1))
else
	echo "FAIL битый плейбук: ожидалась внятная строка без traceback (код $rc)"
	echo "$out" | sed 's/^/      /'
	fail=$((fail + 1))
fi

# 15. A push carries several commits: each one that created a debt declares it,
#     so checking only the tip would let the middle of a push through silently.
range_repo="$work/range"
scaffold "$range_repo"
echo x >>"$range_repo/master/main.go"
git -C "$range_repo" add -A
git -C "$range_repo" commit -qm "feat(master): код"
base="$(git -C "$range_repo" rev-parse HEAD)"
echo x >>"$range_repo/infra/roles/birdman_monitoring_dev/templates/rules.yml.j2"
git -C "$range_repo" add -A
git -C "$range_repo" commit -qF - <<<"fix(infra): молчаливая правка роли"
echo x >>"$range_repo/master/main.go"
git -C "$range_repo" add -A
git -C "$range_repo" commit -qm "feat(master): ещё код"
rc=0
out="$(cd "$range_repo" && python3 "$checker" --range "$base..HEAD" 2>&1)" || rc=$?
if [ "$rc" = 1 ] && printf '%s' "$out" | grep -qF "молчаливая правка роли"; then
	echo "ok   диапазон: должник в СЕРЕДИНЕ пуша не проскакивает"
	pass=$((pass + 1))
else
	echo "FAIL диапазон: должник в середине пуша проскочил (код $rc)"
	echo "$out" | sed 's/^/      /'
	fail=$((fail + 1))
fi

# ─── ГЕЙТ 2: покрытие сторожем маунтов (tracker #1089) ───────────────────────
# Сторож «конфиг положен так, что контейнер его не прочитает» родился
# роле-локальным (#1072) и смотрел ОДИН compose из четырёх; composes агента,
# оверлея и мастера тем же критерием не проверял никто. Критерий с #1089 общий
# (infra/ci/mounted_config_access.py), но зовёт его раннер КАЖДОЙ роли — рендер
# роле-локальный по построению. Значит, новая роль с compose (или новый
# compose-шаблон в старой) может тихо остаться без сторожа: тесты зелёные,
# файла никто не смотрит. Это ровно тот отказ «выглядит покрытым», ради
# которого весь этот ряд карточек, — поэтому здесь он громкий.
#
# Честная граница: гейт проверяет СЦЕПКУ (шаблон назван в tests/ роли, раннер
# зовёт сторожа), а не то, что сторожу скормили именно этот рендер. Обмануть
# его можно, но не молча — только дописав имя шаблона в tests/ руками.
echo
echo "── покрытие: у каждого compose-шаблона роли есть сторож маунтов"
shopt -s nullglob
templates=("$root"/infra/roles/*/templates/*compose*.j2)
# else, а не проверка после цикла: под bash 3.2 (macOS у контрибьютора) `set -u`
# роняет обход ПУСТОГО массива с «unbound variable», и отказ выглядел бы как
# поломка теста вместо внятного «глоб перестал совпадать».
if [ ${#templates[@]} -eq 0 ]; then
	echo "FAIL покрытие: глоб infra/roles/*/templates/*compose*.j2 не совпал ни с чем — раскладка ролей уехала"
	fail=$((fail + 1))
else
	for tpl in "${templates[@]}"; do
		rel="${tpl#"$root"/}"
		role_dir="${tpl%/templates/*}"
		role_name="$(basename "$role_dir")"
		base="$(basename "$tpl" .j2)"
		runner="$role_dir/tests/run.sh"
		if [ ! -x "$runner" ]; then
			echo "FAIL покрытие: $rel — у роли $role_name нет исполняемого tests/run.sh"
			fail=$((fail + 1))
			continue
		fi
		if ! grep -qF 'mounted-config-access.sh' "$runner"; then
			echo "FAIL покрытие: $rel — раннер роли $role_name не зовёт сторожа маунтов"
			fail=$((fail + 1))
			continue
		fi
		# Имя шаблона обязано встречаться в tests/ роли — иначе его никто не
		# рендерит и сторожу нечего показывать. Граница слева не даёт
		# `compose.yml` совпасть внутри `vmagent-compose.yml`: иначе один
		# покрытый шаблон «покрывал» бы забытый соседний.
		if ! grep -rqE "(^|[^-[:alnum:]._])${base//./\\.}" "$role_dir/tests/"; then
			echo "FAIL покрытие: $rel — ни один файл tests/ роли $role_name его не называет: шаблон не рендерится, значит и не проверяется"
			fail=$((fail + 1))
			continue
		fi
		echo "ok   покрытие: $rel"
		pass=$((pass + 1))
	done
fi

# ─── ГЕЙТ 3: формы записи тома, которые сторож обязан понимать (#1089) ───────
# Гейт 2 следит, что сторожа ЗОВУТ. Этот — что позванный сторож видит маунт.
# Дважды подряд он терял легальную форму МОЛЧА И ЗЕЛЁНО: сначала длинную запись
# целиком, потом `type: bind` с относительным `source:` (compose такую запись
# принимает и разворачивает в bind <каталог-проекта>/pg-tuning.conf, а сторож
# классифицировал источник по имени и записывал его в именованные тома). Оба
# раза форму перебирали руками на ревью — то есть список форм был закрыт ровно
# настолько, насколько его кто-то вспомнил. Здесь он записан.
#
# Правило таблицы: у КАЖДОЙ формы исход ровно один из двух — она либо становится
# маунтом, либо роняет прогон с внятной строкой. Третьего («пропущена как
# не-bind») удостоены только формы, у которых хостовой стороны нет ПО СПЕКЕ, и
# они названы поимённо. Docker тут не нужен: uid образа спрашивают только у
# немирочитаемых путей, а BIRDMAN_SKIP_IMAGE_UID_PROBE=1 закрывает и этот случай.
echo
echo "── формы записи тома: маунт или громкий отказ, но не тихий пропуск"
vol_role="$work/volforms/role"
mkdir -p "$vol_role/tasks" "$vol_role/defaults"
cat >"$vol_role/tasks/main.yml" <<-'YML'
	---
	- name: каталог конфигов
	  ansible.builtin.file:
	    path: /srv/fixture
	    state: directory
	    owner: root
	    mode: "0755"
	- name: конфиг, который монтируют
	  ansible.builtin.template:
	    src: pg.conf.j2
	    dest: /srv/fixture/pg.conf
	    owner: root
	    mode: "0644"
	- name: конфиг, закрытый ото всех, кроме root
	  ansible.builtin.template:
	    src: secret.conf.j2
	    dest: /srv/fixture/secret.conf
	    owner: root
	    mode: "0600"
YML
: >"$vol_role/defaults/main.yml"

# vol_case <имя> <ожидаемый код> <текст, который обязан быть в выводе> [флаг]
# Тело compose приходит с stdin (heredoc у вызова).
vol_case() {
	local name="$1" want="$2" expect="$3" flag="${4:-}"
	local file="$work/volforms/$(echo "$name" | tr -c 'a-zA-Z0-9' '_').yml"
	cat >"$file"
	local out rc=0
	out="$(BIRDMAN_SKIP_IMAGE_UID_PROBE=1 "$root/infra/ci/mounted-config-access.sh" \
		--role "$vol_role" ${flag:+"$flag"} "$file" 2>&1)" || rc=$?
	if [ "$rc" != "$want" ] || ! printf '%s' "$out" | grep -qF "$expect"; then
		# ${expect} в скобках не для красоты: bash 3.2 (/bin/bash macOS) съедает
		# в имя переменной первый байт следующей за ней «»» и падает с
		# «expect?: unbound variable» вместо отчёта о непройденном кейсе.
		echo "FAIL форма: $name — код $rc (ожидался $want), искали «${expect}»"
		printf '%s\n' "$out" | sed 's/^/      /'
		fail=$((fail + 1))
		return
	fi
	echo "ok   форма: $name"
	pass=$((pass + 1))
}

# — формы, которые ОБЯЗАНЫ стать маунтом —
vol_case "короткая, абсолютный путь" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
vol_case "длинная, абсолютный путь" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: /srv/fixture/pg.conf
	        target: /etc/pg.conf
	        read_only: true
YML
vol_case "смешанный compose: именованный том не съедает bind" 0 "проверено маунтов: 2" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - pgdata:/var/lib/postgresql/data
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	  web:
	    image: nginx:1.27
	    volumes:
	      - type: bind
	        source: /srv/fixture/pg.conf
	        target: /etc/nginx/pg.conf
	volumes:
	  pgdata: {}
YML
# Приёмка не должна быть пустой: маунт на путь, которого роль не кладёт, —
# главный смысл сторожа, и он обязан быть красным.
vol_case "маунт на путь, который роль не кладёт" 1 "НИ ОДНА таска роли" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/чужое/pg.conf:/etc/pg.conf:ro
YML
vol_case "0600 root в контейнер не от root" 1 "не годится контейнеру" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/secret.conf:/etc/pg.conf:ro
YML

# — ОТНОСИТЕЛЬНЫЙ источник: разбирается, но отвергается ГРОМКО (честная граница).
#   Первая строка — дефект второго круга: `type: bind` + source без слэша давал
#   RC=0 и «bind-маунтов нет вовсе».
vol_case "длинная, source без слэша (дефект #1089)" 1 "ОТНОСИТЕЛЬНЫЙ хостовый путь" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: pg-tuning.conf
	        target: /etc/postgresql/pg-tuning.conf
YML
vol_case "длинная, source ./x" 1 "ОТНОСИТЕЛЬНЫЙ хостовый путь" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: ./pg.conf
	        target: /etc/pg.conf
YML
vol_case "короткая, ./x" 1 "ОТНОСИТЕЛЬНЫЙ хостовый путь" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - ./pg.conf:/etc/pg.conf:ro
YML
vol_case "короткая, ../x" 1 "ОТНОСИТЕЛЬНЫЙ хостовый путь" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - ../pg.conf:/etc/pg.conf:ro
YML
vol_case "короткая, ~/x" 1 "ОТНОСИТЕЛЬНЫЙ хостовый путь" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - ~/pg.conf:/etc/pg.conf:ro
YML

# — формы БЕЗ хостовой стороны по спеке: сознательный пропуск, и он ПОСЧИТАН —
vol_case "именованный том (короткая)" 0 "без хостовой стороны: 1" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - pgdata:/var/lib/postgresql/data
	volumes:
	  pgdata: {}
YML
vol_case "type: volume" 0 "без хостовой стороны: 1" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: volume
	        source: pgdata
	        target: /var/lib/postgresql/data
	volumes:
	  pgdata: {}
YML
vol_case "type: tmpfs" 0 "без хостовой стороны: 1" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: tmpfs
	        target: /run
YML
vol_case "анонимный том (строка без двоеточия)" 0 "без хостовой стороны: 1" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /var/lib/postgresql/data
YML

# — формы, которые понять НЕЛЬЗЯ: прогон падает, а не «маунтов нет» —
vol_case "длинная без type" 1 "type is required" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - source: /srv/fixture/pg.conf
	        target: /etc/pg.conf
YML
vol_case "type: bind без source" 1 "не берётся гадать" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        target: /etc/pg.conf
YML
vol_case "source не строка" 1 "не берётся гадать" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: 1234
	        target: /etc/pg.conf
YML
vol_case "чужой type" 1 "неизвестный type" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: выдумка
	        source: /srv/fixture/pg.conf
	        target: /etc/pg.conf
YML
vol_case "запись-список" 1 "не строка и не словарь" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - [/srv/fixture/pg.conf, /etc/pg.conf]
YML
vol_case "compose без сервисов" 1 "рендер сломался" --allow-no-mounts <<-'YML'
	services: {}
YML

# ─── ГЕЙТ 4: ОСТАЛЬНЫЕ ДВЕРИ хостового пути в контейнер (tracker #1097) ─────
# Гейт 3 перебирает формы ОДНОЙ двери — `services.*.volumes`. Но хостовый путь
# попадает в контейнер ещё тремя, и ни одной сторож не видел ВОВСЕ:
#
#   · именованный том, чьё верхнеуровневое определение несёт
#     `driver_opts: {type: none, device: /srv/…, o: bind}` — у сервиса это
#     выглядит как `type: volume`, и запись сознательно пропускалась (#1096);
#   · `configs:`/`secrets:` с `file:` — вне swarm compose байнд-монтирует файл
#     в контейнер с ХОСТОВЫМИ правами (uid/gid/mode там не работают), то есть
#     это дословно #1072;
#   · верхнеуровневый `include:` — документ втягивает ЦЕЛЫЕ чужие сервисы с их
#     маунтами (найдено на ревью первого круга, см. ниже свой блок).
#
# Замерено на каждой: до перехода прогон давал RC=0 и «bind-маунтов нет» (у
# `include` — «проверено маунтов: 1 — все читаются») — ровно то тихое зелёное, в
# котором alertmanager прожил 40 часов. Поэтому у каждой двери здесь ДВА кейса —
# красный (иначе дверь не закрыта) и зелёный (иначе «закрыто» могло бы значить
# «сломано и всегда красное»).
echo
echo "── остальные двери: driver_opts-bind, configs/secrets и include под сторожем"

# — ДВЕРЬ 2: bind, спрятанный в верхнеуровневом определении тома —
vol_case "driver_opts bind: 0600 root в контейнер не от root (#1096)" 1 "не годится контейнеру" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
	volumes:
	  conf:
	    driver_opts:
	      type: none
	      device: /srv/fixture/secret.conf
	      o: bind
YML
vol_case "driver_opts bind виден как настоящий маунт" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
	volumes:
	  conf:
	    driver_opts:
	      type: none
	      device: /srv/fixture/pg.conf
	      o: bind
YML
vol_case "driver_opts bind с относительным device" 1 "ОТНОСИТЕЛЬНЫЙ хостовый путь" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
	volumes:
	  conf:
	    driver_opts:
	      type: none
	      device: ./pg.conf
	      o: bind
YML
# Обычный том под docker'ом обязан остаться пропуском — иначе «закрытая дверь»
# сломала бы все настоящие composes ролей (у мастера том именно такой).
vol_case "именованный том с name: остаётся без хостовой стороны" 0 "без хостовой стороны: 1" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - pgdata:/var/lib/postgresql/data
	volumes:
	  pgdata:
	    name: birdman-pgdata
YML

# — РОСТ ФОРМ ГРОМКИЙ: всё, что не разобрано, роняет прогон, а не зеленеет —
vol_case "ссылка на необъявленный том" 1 "нет в верхнеуровневой секции volumes" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
YML
vol_case "том с чужим драйвером" 1 "не local" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
	volumes:
	  conf:
	    driver: rexray
YML
vol_case "том external" 1 "объявлен external" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
	volumes:
	  conf:
	    external: true
YML
vol_case "driver_opts не про bind (nfs)" 1 "не берётся гадать" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
	volumes:
	  conf:
	    driver_opts:
	      type: nfs
	      device: ":/exported"
	      o: addr=10.0.0.1
YML
vol_case "неизвестный ключ в определении тома" 1 "ключи, которых сторож не знает" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - conf:/etc/pg
	volumes:
	  conf:
	    выдумка: 1
YML

# — ДВЕРЬ 3: configs:/secrets: с file: —
vol_case "configs file: 0600 root в контейнер не от root (#1072)" 1 "не годится контейнеру" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    configs:
	      - source: conf
	        target: /etc/pg.conf
	configs:
	  conf:
	    file: /srv/fixture/secret.conf
YML
vol_case "secrets file: 0600 root в контейнер не от root" 1 "не годится контейнеру" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    secrets:
	      - tok
	secrets:
	  tok:
	    file: /srv/fixture/secret.conf
YML
vol_case "configs file: виден как настоящий маунт" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    configs:
	      - conf
	configs:
	  conf:
	    file: /srv/fixture/pg.conf
YML
vol_case "configs на путь, который роль не кладёт" 1 "НИ ОДНА таска роли" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    configs:
	      - conf
	configs:
	  conf:
	    file: /srv/чужое/pg.conf
YML
# Содержимое не с хоста — законный пропуск, и он ПОСЧИТАН.
vol_case "config из environment: хостовой стороны нет" 0 "без хостовой стороны: 1" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    configs:
	      - conf
	configs:
	  conf:
	    environment: PG_CONF
YML
vol_case "ссылка на необъявленный config" 1 "нет в верхнеуровневой секции configs" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    configs:
	      - conf
YML
vol_case "неизвестный ключ в определении секрета" 1 "ключи, которых сторож не знает" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    secrets:
	      - tok
	secrets:
	  tok:
	    выдумка: 1
YML

# — маунты, ПРИВЕДЁННЫЕ со стороны: сторож их не разворачивает и молчать не смеет —
# `volumes_from` особенно подл: тома он берёт у чужого сервиса, а образ (значит,
# и uid) подставляет свой — то есть даже проверенный у соседа путь здесь снова
# ничего не гарантирует.
vol_case "volumes_from не проезжает молча" 1 "не разворачивает" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	  side:
	    image: busybox:1.36
	    volumes_from:
	      - db
YML
vol_case "extends не проезжает молча" 1 "не разворачивает" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    extends:
	      file: base.yml
	      service: base
YML

# — ДВЕРЬ 4: сам ДОКУМЕНТ приводит чужие сервисы (tracker #1097, второй круг) —
# `extends` (сервис тянет описание из чужого файла) был закрыт, а `include`
# (документ тянет ЦЕЛЫЕ сервисы из чужого файла) — нет: слова `include` в
# сторожe не было вовсе, то есть это была не оставленная граница, а
# непросмотренная форма. Замерено на ревью и перемерено здесь: `docker compose
# config` v2.32.4 (client-side, демон не нужен) МЕРДЖИТ включённый сервис вместе
# с его bind'ом 0600 root:root, а сторож на том же файле печатал «проверено
# маунтов: 1 — все читаются своими контейнерами» и выходил НУЛЁМ. Включаемый
# фрагмент не спасает и гейт покрытия: он глобает `*compose*.j2`, а фрагмент
# зовётся как угодно.
#
# Лечится уровнем выше самой формы: неизвестный ключ ДОКУМЕНТА роняет прогон так
# же, как неизвестный ключ определения тома. Поэтому кейсов четыре — не только
# про `include`, но и про цену этого решения: законные верхнеуровневые ключи
# краснеть НЕ должны, иначе лекарство хуже болезни.
cat >"$work/volforms/included-fragment.yml" <<-'YML'
	# фрагмент, который втягивает include: сторожу его не показывают вовсе
	services:
	  am:
	    image: prom/alertmanager:v0.27.0
	    volumes:
	      - /srv/fixture/secret.conf:/etc/am.yml:ro
YML
vol_case "include: не проезжает молча (0600 root в чужом файле)" 1 "верхнеуровневый include:" <<-'YML'
	include:
	  - included-fragment.yml
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
vol_case "неизвестный верхнеуровневый ключ документа" 1 "верхнеуровневые ключи документа" <<-'YML'
	выдумка: 1
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
# Цена решения, кейс 1: ровно те верхнеуровневые ключи, что есть в НАСТОЯЩИХ
# composes ролей (name/services/volumes) плюс инертные networks/configs/secrets.
# Покраснеть тут значило бы сломать все пять сьют разом.
vol_case "законные верхнеуровневые ключи не краснеют" 0 "проверено маунтов: 1" <<-'YML'
	name: birdman-fixture
	version: "3.9"
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	networks:
	  default:
	    name: fixture_default
	volumes: {}
	configs: {}
	secrets: {}
YML
# Цена решения, кейс 2: `x-…` пропускается — и это НЕ дыра. Якорь из-под `x-`
# раскрывает сам YAML-парсер до входа в сторожа, поэтому смонтированный через
# `<<: *anchor` конфиг 0600 root:root обязан быть ПОЙМАН, а не пропущен вместе с
# ключом. Кейс проверяет обе половины сразу: ключ не краснит, а маунт из-под
# него краснит.
vol_case "x-расширение не краснит, но и не прячет маунт" 1 "не годится контейнеру" <<-'YML'
	x-common: &common
	  volumes:
	    - /srv/fixture/secret.conf:/etc/pg.conf:ro
	services:
	  db:
	    image: postgres:16
	    <<: *common
YML

# ─── ГЕЙТ 5: ключи САМОГО СЕРВИСА (tracker #1107) ───────────────────────────
# Третий и последний уровень роста форм. Гейт 4 закрыл сразу два — ключи
# ДОКУМЕНТА и ключи ОПРЕДЕЛЕНИЯ тома (оба пришли с #1097); между ними оставался
# уровень, на котором сторож знал ровно пять ключей сервиса, а ключ вне пятёрки
# до него не доезжал ВОВСЕ. Замерено до перехода: `devices: [/srv/…:/dev/x]` рядом с
# честным bind'ом давал RC=0 и «проверено маунтов: 1» — уверенное «всё хорошо»
# при непроверенном хостовом пути.
#
# Закрыт белым списком, и это законно ровно потому же, почему на уровне
# документа: набор конечен и закрыт схемой САМОГО compose. Замерено на v2.32.4
# (client-side, демон не нужен): на неизвестном ключе `docker compose config`
# отвечает «services.db Additional property выдумка is not allowed» и выходит
# 15. Список в сторо́же промерен по этой же схеме: 95 кандидатов, 89 приняты (88
# именованных ключей и расширение `x-…`), и четыре его списка дают ровно эти 88.
#
# Цена промерена тоже, и она НУЛЕВАЯ: восемь настоящих рендеров репозитория
# (мастер; агент на боксе из одной и из двух нод — по два compose; мониторинг;
# оверлей спок и хаб) используют 13 ключей сервиса, все понятые, и вывод
# сторожа на них до и после перехода отличается только ДОПИСАННЫМИ строками про
# ранее невидимые записи. Кейсы ниже пришпиливают ровно ту половину этой цены,
# которую можно пришпилить отсюда: что `/dev/net/tun` и `env_file` 0600
# root:root НЕ краснеют. Вторая половина замерена вручную и здесь только
# записана: наивный вариант («devices — обычный маунт», «env_file судить
# критерием #1072») давал на живых рендерах ТРИ ложных отказа — мониторинг и
# оба режима оверлея.
echo
echo "── ключи сервиса: понятый набор закрыт, остальное роняет прогон"

# — МУТАЦИЯ, ради которой карточка: devices протаскивает хостовый файл —
vol_case "devices: 0600 root в контейнер не от root (#1107)" 1 "не годится контейнеру" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    devices:
	      - /srv/fixture/secret.conf:/dev/x
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
vol_case "devices: длинная форма протаскивает то же самое" 1 "не годится контейнеру" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    devices:
	      - source: /srv/fixture/secret.conf
	        target: /dev/x
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
vol_case "devices: путь, который роль не кладёт" 1 "НИ ОДНА таска роли" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    devices:
	      - /srv/чужое/thing:/dev/x
YML
# ЦЕНА ЭТОЙ ДВЕРИ, кейс 1: настоящая запись роли birdman_overlay. Узел под
# /dev/ создаёт ядро, таской роли он не кладётся и класться не может —
# пропуск сознательный, НАЗВАННЫЙ и посчитанный. Покраснеть тут значило бы
# сломать обе сьюты оверлея (замерено на наивном варианте: ровно так и было).
vol_case "devices: /dev/net/tun остаётся узлом устройства" 0 "узлов устройств пропущено: 1" <<-'YML'
	services:
	  overlay:
	    image: birdman/overlay:dev
	    devices:
	      - /dev/net/tun:/dev/net/tun
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML

# И обратная сторона того же: узел устройства НЕ засчитывается за bind-маунт.
# Иначе compose без единого маунта, но с `devices:`, гасил бы громкий отказ
# «bind-маунтов нет — они уехали из compose или рендер отдал не то» чужой
# записью, и рендер, потерявший все маунты, проезжал бы зелёным.
vol_case "устройство не гасит отказ «bind-маунтов нет»" 1 "нет bind-маунтов" <<-'YML'
	services:
	  overlay:
	    image: birdman/overlay:dev
	    devices:
	      - /dev/net/tun:/dev/net/tun
YML

# — ДВЕРЬ 6: env_file/label_file — их читает САМ COMPOSE на хосте —
# Критерий тут ДРУГОЙ, и это не поблажка, а замер: compose открывает файл сам
# (v2.32.4 падает «env file … not found» ещё на `config`, без демона), а
# контейнер его не открывает вовсе. Живой мониторинг ровно такой: grafana.env
# лежит 0600 root:root и РАБОТАЕТ — судить его критерием #1072 значило бы
# покраснеть на работающей раскладке.
vol_case "env_file на путь, который роль не кладёт" 1 "НИ ОДНА таска роли" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    env_file:
	      - /srv/чужое/grafana.env
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
vol_case "label_file на путь, который роль не кладёт" 1 "НИ ОДНА таска роли" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    label_file: /srv/чужое/labels
YML
# ЦЕНА ЭТОЙ ДВЕРИ: 0600 root:root у env_file — НЕ дефект, и кейс стоит здесь
# именно потому, что выглядит он как дефект #1072 дословно.
vol_case "env_file 0600 root — не дефект, но и не молчание" 0 "читаемых самим compose: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    env_file:
	      - /srv/fixture/secret.conf
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
# `required: false` — compose такой файл не требует (замерено): пропуск.
vol_case "env_file required: false не требует пути" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    env_file:
	      - path: /srv/чужое/optional.env
	        required: false
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML

# — ключи, которые ведут хостовый путь и НЕ разворачиваются: отказ —
vol_case "build не проезжает молча" 1 "ХОСТОВЫЙ путь" --allow-no-mounts <<-'YML'
	services:
	  db:
	    build:
	      context: /srv/fixture
YML
vol_case "develop watch не проезжает молча" 1 "ХОСТОВЫЙ путь" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    develop:
	      watch:
	        - path: /srv/fixture
	          action: sync
	          target: /etc/pg
YML

# — САМ УРОВЕНЬ: неизвестный ключ сервиса роняет прогон —
vol_case "неизвестный ключ сервиса" 1 "ключи сервиса: выдумка" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    выдумка: 1
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
# Опечатка в законном ключе — тот же класс, и раньше она проезжала зелёной.
vol_case "опечатка в законном ключе краснеет" 1 "ключи сервиса: volumez" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumez:
	      - /srv/fixture/secret.conf:/etc/pg.conf:ro
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
YML
# ЦЕНА РЕШЕНИЯ: ровно те 13 ключей, что стоят в НАСТОЯЩИХ composes ролей
# (замерено по восьми рендерам). Покраснеть тут значило бы сломать все пять
# сьют разом — то есть лекарство хуже болезни.
vol_case "13 ключей живых composes не краснеют" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    container_name: birdman-db
	    command: ["postgres"]
	    environment:
	      PGDATA: /var/lib/postgresql/data
	    ports:
	      - "127.0.0.1:5433:5432"
	    depends_on:
	      - side
	    restart: unless-stopped
	    healthcheck:
	      test: ["CMD-SHELL", "pg_isready"]
	    cap_add: [NET_ADMIN]
	    devices:
	      - /dev/net/tun:/dev/net/tun
	    env_file:
	      - /srv/fixture/secret.conf
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	  side:
	    image: busybox:1.36
	    network_mode: host
YML
# `x-…` у сервиса compose принимает (замерено), и пропускается он по той же
# причине, что и на уровне документа: якорь раскрывает YAML-парсер до сторожа,
# поэтому маунт из-под него обязан быть ПОЙМАН, а не пропущен вместе с ключом.
vol_case "x- у сервиса не краснит, но и не прячет маунт" 1 "не годится контейнеру" <<-'YML'
	x-common: &common
	  - /srv/fixture/secret.conf:/etc/pg.conf:ro
	services:
	  db:
	    image: postgres:16
	    x-birdman: свой ключ
	    volumes: *common
YML

# ─── САМ РАННЕР: упавший посредине обязан быть красным (tracker #1089) ──────
# Гейт, который умеет молча зеленеть, не гейт вовсе — и однажды именно этим
# кончилось: раннер умер на `set -u` посреди новой таблицы форм и отрапортовал
# успех. Часовой сверху это чинит; здесь он ПРИШПИЛЕН, потому что предыдущая
# «починка» была проверена гипотезой, а не лекарством, и мутация на её
# инварианте оставалась зелёной целый круг.
#
# Мутируется НАСТОЯЩИЙ этот файл, а не его копия-реплика: смерть вписывается
# строкой сразу за trap'ом, дальше прогон запускается как отдельный процесс.
# Рекурсии нет с двух сторон: ребёнок умирает задолго до этого блока, и блок в
# нём всё равно выключен через BIRDMAN_TRAP_SELFTEST.
#
# Кейса ДВА — на два способа соврать, которые кейсом ловятся: не покраснеть,
# когда должен, и придавить чужой ненулевой код своей единицей. Способов, если
# честно, ТРИ: часовой, который не взвели вовсе (`reached_end=1` не выставлен),
# краснит ЧЕСТНЫЙ УСПЕХ. Третий кейсом не закрыт сознательно — он ловится
# кодом возврата САМОГО ЭТОГО ПРОГОНА (сводка «упало: 0» при rc=1), и написать
# на него кейс изнутри того же файла нельзя: соврал бы ровно тот механизм,
# которым кейс проверяют. Проверено мутацией руками, не автоматом.
if [ -z "${BIRDMAN_TRAP_SELFTEST:-}" ]; then
	echo
	echo "── сам раннер: смерть посредине обязана быть красной"

	# trap_case <имя> <строка, вписываемая сразу за trap'ом> <ожидаемый код>
	# Ожидаемый код — либо точное число, либо `!0` («любой ненулевой»). Разница
	# принципиальная и не косметическая: КАКИМ именно кодом оболочка выходит по
	# `set -u`, зависит от её версии (bash 3.2 без trap'а даёт 1, про bash 5
	# здесь замера нет — его на машине разработчика нет), а гоняется этот файл
	# и локально, и на CI. Пришпилить там число значило бы пришпилить версию
	# bash. Инвариант же версии не касается: до финала не дошли — не ноль.
	# Явный `exit N` — наоборот, число точное: его выбирает не оболочка.
	trap_case() {
		local name="$1" inject="$2" want="$3"
		local dir="$work/trapself/$(echo "$name" | tr -c 'a-zA-Z0-9' '_')"
		mkdir -p "$dir"
		# Сторожим ВСТАВКИ, а не присутствие иглы в файле (tracker #1099).
		# Прежний сторож `grep -qF "$inject" "$dir/run.sh"` сработать не мог
		# НИКОГДА: `$inject` — это аргумент, и обе иглы стоят дословно в
		# строках вызова `trap_case` ниже по этому же файлу, а копия содержит
		# весь файл целиком; grep находил иглу и при НУЛЕ вставок. Считает
		# теперь сам awk — сколько раз совпал ЕГО регексп: ровно одна вставка
		# норма, ноль (регексп разошёлся со строкой trap'а — то, от чего блок
		# и защищает) и больше одной (вписали не туда) — отказ.
		local ins_rc=0
		awk -v ins="$inject" -v cnt="$dir/injected" '
			{print}
			/^trap .*EXIT$/ {print ins; n++}
			END {print n + 0 > cnt; exit (n == 1 ? 0 : 1)}
		' "$self" >"$dir/run.sh" || ins_rc=$?
		chmod +x "$dir/run.sh"
		if [ "$ins_rc" != 0 ]; then
			echo "FAIL раннер: $name — смерть вписана $(cat "$dir/injected" 2>/dev/null || echo '?') раз(а), нужна ровно одна: awk-регексп и строка trap'а разошлись"
			fail=$((fail + 1))
			return
		fi
		local rc=0
		BIRDMAN_TRAP_SELFTEST=1 "$dir/run.sh" >"$dir/out" 2>&1 || rc=$?
		# Ненулевого кода МАЛО для кейса `!0`. Ребёнок лежит в темпе, его
		# `checker`/`root` указывают в никуда, поэтому НЕ умерев он даёт
		# честный красный финал от всех своих упавших базовых кейсов — тот же
		# ненулевой код, но не про то. Смерть вписана сразу за trap'ом, то
		# есть задолго до первого кейса: доехавшая до сводки строка означает,
		# что ребёнок пережил вставку.
		if grep -q '^прошло:' "$dir/out"; then
			echo "FAIL раннер: $name — ребёнок доехал до своей сводки, вписанная смерть его не убила"
			tail -5 "$dir/out" | sed 's/^/      /'
			fail=$((fail + 1))
			return
		fi
		local ok=1
		if [ "$want" = '!0' ]; then
			[ "$rc" != 0 ] || ok=0
		else
			[ "$rc" = "$want" ] || ok=0
		fi
		if [ "$ok" != 1 ]; then
			echo "FAIL раннер: $name — код $rc, ожидался $want"
			tail -5 "$dir/out" | sed 's/^/      /'
			fail=$((fail + 1))
			return
		fi
		echo "ok   раннер: $name (код $rc)"
		pass=$((pass + 1))
	}

	# Тот самый режим: bash 3.2 отдаёт trap'у `$?` == 0, и раннер зеленел.
	trap_case "смерть по set -u не выходит с нулём" ': "${nope_selftest}"' '!0'
	# Обратная сторона: часовой поднимает ТОЛЬКО ноль. Чужой ненулевой код
	# обязан дойти наружу неизменным, иначе «красное» перестанет что-то значить.
	trap_case "явный exit 7 посредине остаётся семёркой" 'exit 7' 7
fi

# ─── ЧАСОВОЙ РАННЕРОВ РОЛЕЙ: тоже пришпилен, а не «снят руками» (tracker #1103)
# Тот же часовой стоит в раннерах ролей с #1098, но до этого блока его не
# сторожил НИКТО: правильность была снята мутацией руками один раз и после
# этого не пересчитывалась никогда. Класс ровно тот же, ради которого
# заводились #1089 и #1098: лекарство, которое ничем не охраняется, — это
# лекарство до первой правки. Ломается оно в ДВЕ РАЗНЫЕ стороны, и кейсы ниже
# держат обе (обе замерены мутацией по всем четырём раннерам):
#   * вернуть голый `trap 'rm -rf "$work"' EXIT` — и раннер снова начнёт молча
#     ЗЕЛЕНЕТЬ на смерти посредине (замер: код 0 там, где ждали ненулевой);
#   * снять `reached_end=1` — и он, наоборот, начнёт КРАСНИТЬ честный успех
#     (замер: код 1 там, где ждали 0), потому что часовой не снимается никогда.
#
# ПОЧЕМУ МУТАНТ КЛАДЁТСЯ В ДЕРЕВО РОЛИ, А НЕ В ТЕМП. Раннер роли резолвит себя
# от собственного пути и читает файлы роли:
#
#     role="$(dirname "$here")"; repo="$(cd "$role/../../.." && pwd)"
#     image="$(sed -n '…' "$role/defaults/main.yml")"
#
# Из темпа это разъезжается — и разъезжается ТИХО, что и есть грабли #1099
# (сторож, проверяющий не то, что думает). Замерено в лоб, и замер ставился в
# положение, которое кейс ОБЯЗАН заметить: копия в темп со вписанной смертью и
# СНЕСЁННЫМ ЧАСОВЫМ (голый `trap 'rm -rf "$work"' EXIT`). monitoring_dev,
# overlay и agent_dev в нём всё равно отдают RC=1 — они умирают на `sed` по
# `defaults/main.yml` роли ДО того, как trap будет установлен. Кейс ждёт ровно
# ненулевого кода, значит на всех троих он позеленел бы, не заметив, что
# часового нет вовсе; guard по `ALL OK` ниже тут не помощник — из темпа финала
# нет и подавно. Слепота именно у троих, и это делает ошибку особенно
# незаметной: master_dev файлов роли не читает, из темпа доезжает до trap'а и
# честно разделяет 1 с часовым и 0 без него, то есть на ОДНОМ раннере приём
# работает и выглядит рабочим. Поэтому копия
# кладётся РЯДОМ с оригиналом, скрытым именем с pid'ом (в дереве работают
# несколько сессий разом, фиксированное имя они бы делили), а сам оригинал не
# трогается ни на байт — его правит только человек. Мутант сносится сразу после
# кейса, а на случай обрыва прогона — EXIT-trap'ом гейта через `$mutant`
# (замерено сигналом по живому прогону, с ловлей ЖИВОГО мутанта опросом: TERM
# → rc=143 и HUP → rc=129, мутантов в дереве не осталось ни разу. INT и QUIT
# этим способом ЗАМЕРИТЬ НЕ ВЫШЛО, и это не мелочь: асинхронная команда в
# неинтерактивном bash получает их как SIG_IGN, `trap - INT` в ребёнке
# диспозицию не возвращает — прогон досчитывает до конца, rc=0, и «мутантов 0»
# означало бы штатный финал, а не уборку по обрыву. Сигналы, при которых bash
# до EXIT-trap'а не доходит вовсе, огрызок оставляют — SIGKILL по определению;
# на них страховка в .gitignore, и она не про конкретный сигнал).
#
# ПОЧЕМУ ЭТО НЕ ТРЕБУЕТ ЖИВОГО DOCKER'А. Сами раннеры без демона краснеют:
# замерено при мёртвом `docker info` — monitoring_dev RC=125, overlay RC=1,
# agent_dev RC=125 (master_dev докера не просит вовсе и даёт ALL OK). До
# докерных шагов ТЕЛА не доходит ни один кейс: два убивают мутанта строкой
# сразу за trap'ом, третий вырезает тело целиком, а голова раннера до trap'а —
# это резолв путей, `mktemp -d` и разбор `defaults/main.yml` (у master_dev нет
# и того), ни докера, ни сети там нет. Но «docker не зовётся вовсе» было бы
# неправдой, и это замерено стабом в PATH: за прогон он зовётся ТРИЖДЫ, всегда
# как `docker rm -f …` из EXIT-trap'а agent_dev (по разу на каждый его кейс).
# Блоку это безразлично, потому что там `>/dev/null 2>&1 || true`: со стабом,
# возвращающим 1, гейт остаётся зелёным. Замер сходится с главным: весь блок
# зелёный именно при мёртвом демоне.
if [ -z "${BIRDMAN_TRAP_SELFTEST:-}" ]; then
	echo
	echo "── часовой раннеров ролей: пришпилен кейсами, а не памятью"

	# role_case <раннер> <имя> <режим> <ожидаемый код>
	# Режим — либо строка, вписываемая сразу за trap'ом, либо `splice`
	# («вырезать тело»). Ожидаемый код — точное число либо `!0`; разница та же,
	# что у trap_case выше: каким кодом оболочка выходит по `set -u`, зависит
	# от её версии, а явный `exit N` выбирает не оболочка.
	role_case() {
		local runner="$1" name="$2" mode="$3" want="$4"
		local rel="${runner#"$root"/}"
		local anchors="$work/role_anchors" arc=0
		# НЕ local: EXIT-trap гейта убирает по этой переменной недоеденного
		# мутанта, если прогон оборвут прямо здесь.
		mutant="$(dirname "$runner")/.run.sh.mutant.$$"
		if [ "$mode" = splice ]; then
			# Голова по строку trap'а включительно + хвост от снятия часового.
			# Тело (в нём и живут докерные шаги) выброшено, поэтому мутант
			# обязан пройти честный финал и выйти нулём.
			awk -v cnt="$anchors" '
				/^trap .*EXIT$/ { print; t++; skip = 1; next }
				/^# Часовой снимается ровно здесь/ { skip = 0; e++ }
				skip { next }
				{ print }
				END { print t + 0, e + 0 > cnt; exit (t == 1 && e == 1 ? 0 : 1) }
			' "$runner" >"$mutant" || arc=$?
		else
			# Сторожим ВСТАВКИ, а не присутствие иглы в файле (#1099): считает
			# сам awk, ровно одна вставка — норма.
			awk -v ins="$mode" -v cnt="$anchors" '
				{ print }
				/^trap .*EXIT$/ { print ins; t++ }
				END { print t + 0, 1 > cnt; exit (t == 1 ? 0 : 1) }
			' "$runner" >"$mutant" || arc=$?
		fi
		if [ "$arc" != 0 ]; then
			echo "FAIL часовой $rel: $name — якоря (trap / снятие часового) совпали «$(cat "$anchors" 2>/dev/null || echo '?')» раз(а), нужно по одному"
			rm -f "$mutant"
			mutant=""
			fail=$((fail + 1))
			return
		fi
		chmod +x "$mutant"
		local rc=0
		"$mutant" >"$work/role_out" 2>&1 || rc=$?
		rm -f "$mutant"
		mutant=""
		# `ALL OK` — финал раннера роли, и он читается в ОБЕ стороны. У смертей
		# он обязан ОТСУТСТВОВАТЬ: ненулевой код сам по себе кейса не
		# доказывает — мутант мог упасть где угодно ниже и не от вставки.
		# У выреза тела — обязан присутствовать: иначе ноль пришёл бы не с
		# финала.
		local saw_end=0 want_end=0
		grep -q '^ALL OK$' "$work/role_out" && saw_end=1
		[ "$mode" = splice ] && want_end=1
		if [ "$saw_end" != "$want_end" ]; then
			if [ "$want_end" = 1 ]; then
				echo "FAIL часовой $rel: $name — мутант не доехал до ALL OK (код $rc)"
			else
				echo "FAIL часовой $rel: $name — мутант доехал до ALL OK, вписанная смерть его не убила"
			fi
			tail -5 "$work/role_out" | sed 's/^/      /'
			fail=$((fail + 1))
			return
		fi
		local ok=1
		if [ "$want" = '!0' ]; then
			[ "$rc" != 0 ] || ok=0
		else
			[ "$rc" = "$want" ] || ok=0
		fi
		if [ "$ok" != 1 ]; then
			echo "FAIL часовой $rel: $name — код $rc, ожидался $want"
			tail -5 "$work/role_out" | sed 's/^/      /'
			fail=$((fail + 1))
			return
		fi
		echo "ok   часовой $rel: $name (код $rc)"
		pass=$((pass + 1))
	}

	pinned=0
	for runner in "$root"/infra/roles/*/tests/run.sh; do
		[ -f "$runner" ] || continue
		# Область действия ВЫЧИСЛЯЕТСЯ, а не перечисляется: под критерий
		# попадает раннер с той самой ТРОЙКОЙ из замера #1098 — `-e` и `-u`
		# ВМЕСТЕ плюс EXIT-trap. Именно она даёт ноль на ошибке раскрытия;
		# новый раннер такой формы попадёт под кейсы сам, вместо того чтобы
		# приехать без часового молча. `birdman_devdeploy` (`set -uo` без `-e`
		# и без trap'а) отсеивается здесь по ДВУМ независимым причинам и не
		# трогается вовсе: добавить ему `-e` без часового значило бы СОЗДАТЬ
		# дефект, а не закрыть.
		flags="$(sed -n 's/^set -\([a-zA-Z]*\).*/\1/p' "$runner" | head -1)"
		case "$flags" in *e*) ;; *) continue ;; esac
		case "$flags" in *u*) ;; *) continue ;; esac
		grep -q '^trap .*EXIT$' "$runner" || continue
		pinned=$((pinned + 1))
		# 1. Тот самый режим, ради которого часовой и заведён.
		role_case "$runner" "смерть по set -u не выходит с нулём" ': "${nope_selftest}"' '!0'
		# 2. Обратная сторона: часовой поднимает ТОЛЬКО ноль, чужой ненулевой
		#    код обязан дойти наружу неизменным.
		role_case "$runner" "явный exit 7 посредине остаётся семёркой" 'exit 7' 7
		# 3. Третий способ соврать — часовой, которого не СНИМАЮТ: он красит
		#    честный успех. В самом этом файле такой кейс написать нельзя (соврал
		#    бы ровно тот механизм, которым кейс проверяют), а для чужого раннера
		#    можно и нужно: снятое `reached_end=1` роняет мутанта в единицу.
		role_case "$runner" "честный финал остаётся зелёным" splice 0
	done
	# ЧИСЛО РАННЕРОВ ПРИШПИЛЕНО, А НЕ ПРОСТО НАПЕЧАТАНО, и это не педантизм:
	# фильтр выше СИНТАКСИЧЕСКИЙ (первая строка `set -<флаги>` и трап без
	# хвоста), поэтому та же семантика, записанная иначе, из выборки ВЫПАДАЕТ.
	# Замерено на живом раннере: `set -euo pipefail`, разбитое на две строки
	# `set -eo pipefail` + `set -u` (флаги те же), уводит master_dev из выборки
	# — и гейт остаётся ЗЕЛЁНЫМ, тихо посчитав троих вместо четверых (73/0).
	# Это ровно то «выглядит покрытым», против чего написан весь блок, поэтому
	# убыль обязана краснить. Прибавка законна (новый раннер той же формы),
	# убыль — нет: либо у раннера сняли часового, либо его запись перестала
	# попадать под фильтр. Порог правит человек, осознанно.
	pinned_floor=4
	if [ "$pinned" -lt "$pinned_floor" ]; then
		echo "FAIL часовой раннеров ролей: под критерий (-e + -u + EXIT-trap) попало $pinned раннер(ов) из $pinned_floor. Либо у раннера сняли часового, либо его set/trap записан формой, которой фильтр не видит (например, флаги разнесены на две строки). Правка порога — последнее дело, а не первое: сперва убедись, что часовой на месте у всех"
		fail=$((fail + 1))
	else
		echo "ok   часовой раннеров ролей: под часовым $pinned раннер(ов), порог $pinned_floor"
		pass=$((pass + 1))
	fi
fi

echo
echo "прошло: $pass, упало: $fail"
# Часовой снимается ровно здесь: всё, что ниже, — это и есть финал.
reached_end=1
[ "$fail" -eq 0 ]
