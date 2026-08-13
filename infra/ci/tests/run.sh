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
# Третий уровень роста форм — но НЕ последний: уровнем ниже лежат ключи внутри
# ОДНОЙ ЗАПИСИ длинной формы, и их закрывает ГЕЙТ 6 в конце этого файла
# (tracker #1115). Гейт 4 закрыл сразу два — ключи
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
# сломать сьюту оверлея в ОБОИХ её режимах (замерено на наивном варианте:
# ровно так и было).
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

# ─── ГЕЙТ 6: ключи ВНУТРИ ОДНОЙ ЗАПИСИ длинной формы (tracker #1115) ────────
# ЧЕТВЁРТЫЙ и самый глубокий уровень роста форм ПО КЛЮЧАМ. «И последний, что
# оставался открытым» тут стояло до #1119 — и было опровергнуто гейтом 7 ниже:
# под ключами лежат ещё и ЗНАЧЕНИЯ. Третий раз подряд уровень, объявленный
# последним, последним не оказался (#1107 так назвал третий, #1115 четвёртый),
# поэтому теперь тут не обещается исчерпание, а называется, что именно закрыто.
# Уровни выше (документ, сервис, определение тома) уже громкие: ключ,
# которого сторож не знает, роняет прогон. А ЗАПИСЬ под понятым ключом сторож
# читал ВЫБОРОЧНО — брал знакомые поля, остальные не смотрел вовсе. Замерено до
# перехода: `{type: bind, source: …, target: …, выдумка: 1}` давало RC=0 и
# «проверено маунтов: 1» — запись проезжала молча и зелёно.
#
# ЗАМЕР ПЕРЕД ВЫБОРОМ МЕХАНИЗМА, а не после (урок #1107, где допущение карточки
# опроверглось дважды): отвергает ли неизвестный ключ ЗАПИСИ сам compose?
# ОТВЕРГАЕТ, во всех пяти — `docker compose config` v2.32.4 client-side отвечает
# «services.db.volumes.0 Additional property выдумка is not allowed», RC=15, и
# так же на `devices.0`, `env_file.0`, `configs.0`, `secrets.0`. Значит набор
# КОНЕЧЕН и закрыт схемой, и белый список тут законен ровно по той же причине,
# что уровнем выше. Будь ответ обратным, список краснел бы на легальном.
#
# Записей ПЯТЬ, а не три, как называла карточка: ссылка `configs:`/`secrets:` у
# сервиса — тоже длинная запись со своим набором (`source, target, uid, gid,
# mode`), она нашлась замером, и без неё уровень остался бы дырявым.
#
# Цена промерена восемью настоящими рендерами (мастер; агент на боксе из одной и
# из двух нод — по два compose; мониторинг; оверлей спок и хаб): длинной формы
# не использует сегодня НИ ОДИН compose-шаблон роли, и вывод сторожа на всех
# восьми совпал ПОБАЙТНО — ни один код возврата не изменился. Ровно как не было
# в шаблонах и `devices:` в тот день, когда его объявили несуществующим, — так
# что кейс «законная длинная форма НЕ краснеет» ниже стоит не для симметрии.
echo
echo "── ключи внутри записи длинной формы: понятый набор закрыт, остальное роняет прогон"

# — МУТАЦИЯ, ради которой карточка: сегодня она давала RC=0 —
vol_case "запись тома: выдумка рядом с понятыми ключами" 1 "ключи записи: выдумка" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: /srv/fixture/pg.conf
	        target: /etc/pg.conf
	        выдумка: 1
YML
vol_case "запись devices: выдумка" 1 "ключи записи: выдумка" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    devices:
	      - source: /srv/fixture/pg.conf
	        target: /dev/x
	        выдумка: 1
YML
vol_case "запись env_file: выдумка" 1 "ключи записи: выдумка" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    env_file:
	      - path: /srv/fixture/pg.conf
	        выдумка: 1
YML
vol_case "ссылка configs: выдумка" 1 "ключи записи: выдумка" --allow-no-mounts <<-'YML'
	configs:
	  c1:
	    file: /srv/fixture/pg.conf
	services:
	  db:
	    image: postgres:16
	    configs:
	      - source: c1
	        выдумка: 1
YML
vol_case "ссылка secrets: выдумка" 1 "ключи записи: выдумка" --allow-no-mounts <<-'YML'
	secrets:
	  s1:
	    file: /srv/fixture/pg.conf
	services:
	  db:
	    image: postgres:16
	    secrets:
	      - source: s1
	        выдумка: 1
YML
# `required: false` — законный ПРОПУСК, и он обязан пропускать запись, а не
# ключи в ней: иначе выдумку унесло бы вместе с пропуском. Проверка стоит ДО
# раннего выхода именно поэтому.
vol_case "env_file required: false не прячет выдумку" 1 "ключи записи: выдумка" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    env_file:
	      - path: /srv/чужое/optional.env
	        required: false
	        выдумка: 1
YML
# Опечатка в законном ключе записи — тот же класс, и раньше она проезжала
# зелёной: `sourse` не source, а маунта у записи как бы и нет.
vol_case "опечатка в ключе записи краснеет" 1 "ключи записи: sourse" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        sourse: /srv/fixture/secret.conf
	        target: /etc/pg.conf
YML
# УРОВНЕМ НИЖЕ ЗАПИСИ — её вложенные опции. Схема закрывает и их, поэтому они
# закрыты здесь же: оставить их значило бы, закрыв четвёртый остаток, тут же
# завести пятый.
vol_case "вложенные опции bind: выдумка" 1 "ключи опций bind: выдумка" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: /srv/fixture/pg.conf
	        target: /etc/pg.conf
	        bind:
	          выдумка: 1
YML
vol_case "вложенные опции volume: выдумка" 1 "ключи опций volume: выдумка" --allow-no-mounts <<-'YML'
	volumes:
	  v1: {}
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: volume
	        source: v1
	        target: /var/lib/x
	        volume:
	          выдумка: 1
YML
vol_case "вложенные опции не словарь" 1 "не словарь" --allow-no-mounts <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: /srv/fixture/pg.conf
	        target: /etc/pg.conf
	        bind: rprivate
YML

# — ЦЕНА РЕШЕНИЯ: законная длинная форма НЕ краснеет —
# Ровно тот compose, что ниже, `docker compose config` v2.32.4 принимает целиком
# (RC=0, замерено): здесь стоят ВСЕ промеренные ключи всех пяти записей и все
# вложенные опции. Покраснеть тут значило бы поменять тихую дыру на ложный
# отказ — то, чем кончился бы наивный вариант в #1107.
vol_case "все законные ключи записей не краснеют" 0 "проверено маунтов: 2" <<-'YML'
	configs:
	  c1:
	    file: /srv/fixture/pg.conf
	volumes:
	  v1: {}
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: /srv/fixture/pg.conf
	        target: /etc/pg.conf
	        read_only: true
	        consistency: consistent
	        bind:
	          propagation: rprivate
	          create_host_path: false
	          selinux: z
	          recursive: enabled
	      - type: volume
	        source: v1
	        target: /var/lib/x
	        volume:
	          nocopy: true
	          subpath: sub
	      - type: tmpfs
	        target: /tmp/x
	        tmpfs:
	          size: 1024
	          mode: 493
	    devices:
	      - source: /dev/net/tun
	        target: /dev/net/tun
	        permissions: rwm
	    configs:
	      - source: c1
	        target: /etc/c1
	        uid: "0"
	        gid: "0"
	        mode: 292
YML
# `x-…` ВНУТРИ записи — замер, а не симметрия с уровнями выше, и замер этот
# НЕсимметричен: compose принимает `x-birdman` внутри записи тома, devices и
# ссылок configs:/secrets: (RC=0), а внутри записи env_file ОТВЕРГАЕТ (RC=15).
# Правило у сторожа одно на все записи — покраснеть на форме, которую compose
# ПРИНИМАЕТ, значило бы дать ложный отказ на легальном.
vol_case "x- внутри записи не краснит, но и не прячет маунт" 1 "не годится контейнеру" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - type: bind
	        source: /srv/fixture/secret.conf
	        target: /etc/pg.conf
	        x-birdman: свой ключ
YML

# ─── ГЕЙТ 7: ЗНАЧЕНИЯ инертных ключей сервиса (tracker #1119, #1120) ────────
# Пятый уровень роста форм. Четыре уровня выше закрыты БЕЛЫМ
# СПИСКОМ КЛЮЧЕЙ, и это было законно ровно потому, что набор ключей закрыт
# схемой самого compose. Здесь не ключи, а ЗНАЧЕНИЯ трёх инертных ключей, и
# замер эти три РАЗДЕЛИЛ, а не уравнял — причём у каждого по-своему:
#
#   · `blkio_config` схема закрывает ЦЕЛИКОМ — и собственные ключи (их ровно
#     шесть), и ключи записи (`{path, rate}` у лимитов, `{path, weight}` у
#     веса). Значит к нему прикладывается та же машинка, что к уровню записей,
#     и он ЗАКРЫТ — вместе со значением `path`, которое поехало в ту же дверь,
#     что и узел из `devices:`;
#   · `security_opt` схема НЕ закрывает (`["выдумка:/путь"]` compose принимает,
#     RC=0), и белого списка тут нет до сих пор. Закрыта не форма, а ОДНА
#     измеримо путеносная строка — `seccomp:<путь>`, уехавшая в дверь 8
#     (tracker #1120): её разбирает разбор САМОГО docker, а не наш список.
#     Кейсы двери 8 — ниже, сразу за кейсом `logging`: критерий у неё общий с
#     дверью 6, но родом она отсюда, из значений инертного ключа;
#   · `logging.options` схема НЕ закрывает тоже (`options: {выдумка: /путь}` —
#     RC=0), и он оставлен СОЗНАТЕЛЬНО: путеносные опции РАЗНЫЕ у разных
#     драйверов, читает их ДЕМОН, и белый список тут пришлось бы держать нам —
#     расходился бы он с docker молча. Кейс ниже, чтобы решение было видно
#     исполняемым фактом, а не только словами в шапке.
echo
echo "── значения инертных ключей: blkio и seccomp закрыты, logging — решение"

vol_case "blkio: узел устройства — посчитанный пропуск" 0 "узлов устройств пропущено: 2" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    blkio_config:
	      device_read_bps:
	        - path: /dev/sda
	          rate: "12mb"
	      weight_device:
	        - path: /dev/sdb
	          weight: 300
YML
# Ровно та мутация, ради которой заведена карточка: конфиг 0600 root:root,
# протащенный в compose под инертным ключом. До #1119 давала RC=0 и «все
# читаются своими контейнерами».
vol_case "blkio: конфиг роли вместо узла — отказ" 1 "БЛОЧНОЕ УСТРОЙСТВО" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    blkio_config:
	      device_read_bps:
	        - path: /srv/fixture/secret.conf
	          rate: "12mb"
YML
vol_case "blkio: неизвестный собственный ключ" 1 "blkio_config, ключи: выдумка" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    blkio_config:
	      выдумка: 1
YML
vol_case "blkio: неизвестный ключ записи" 1 "ключи записи: выдумка" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    blkio_config:
	      device_read_bps:
	        - path: /dev/sda
	          rate: "12mb"
	          выдумка: 1
YML
# Перекрёстная подстановка: схема compose отвергает `weight` внутри лимита и
# `rate` внутри веса (замерено, RC=15), значит и сторож обязан. Кейс отдельный,
# потому что «выдумка» ловится и общим списком, а вот законный-но-не-здесь ключ
# поймает только РАЗДЕЛЬНЫЙ набор — то есть ровно то, что тут пришпилено.
vol_case "blkio: законный ключ не из своей записи" 1 "ключи записи: weight" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    blkio_config:
	      device_read_bps:
	        - path: /dev/sda
	          weight: 300
YML
vol_case "blkio: устройство не гасит «bind-маунтов нет»" 1 "нет bind-маунтов" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    blkio_config:
	      device_read_bps:
	        - path: /dev/sda
	          rate: "12mb"
YML
vol_case "blkio: weight скаляром — законно и зелено" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    blkio_config:
	      weight: 300
YML

# ── ОСТАВШИЙСЯ ОДИН: зелёное здесь — РЕШЕНИЕ, а не недосмотр ────────────────
# Кейс пришпиливает именно то, что НЕ закрыто, и это сделано намеренно: иначе
# следующий проход по шапке снова прочтёт «остаток» и заведёт очередную
# карточку, а прочитав — не сможет отличить решение от забывчивости. Закрывать
# `logging` отказом нельзя: имена опций схемой compose не закрыты (замерено —
# client-side RC=0), путеносные опции РАЗНЫЕ у разных драйверов, а скан значений
# на «похоже на путь» покраснел бы на совершенно легальном `syslog-address:
# unix:///dev/log`. Читает эти пути ДЕМОН от root, а не контейнер: класса #1072
# (контейнер не прочитал свой конфиг) тут не возникает.
# ЕСЛИ ЭТОТ КЕЙС КОГДА-НИБУДЬ ПОКРАСНЕЕТ — значит кто-то научил сторожа
# заходить внутрь значений `logging`; тогда правится не кейс, а шапка.
vol_case "logging: путь в options — зелено ОСОЗНАННО" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    logging:
	      driver: syslog
	      options:
	        syslog-address: "unix:///srv/fixture/secret.conf"
YML

# ── ДВЕРЬ 8: security_opt: seccomp — профиль читает ХОСТОВАЯ сторона ────────
# До #1120 этот ключ лежал среди инертных, а кейс ниже стоял ЗЕЛЁНЫМ с подписью
# «решение»: #1119 принимала его, не имея демона в прогоне, и честно записала
# это границей. Демона довели — и граница сдвинулась. Замерено на 27.5.1 /
# v2.32.4: `seccomp:/nope/absent.json` валит `up` ГРОМКО и ДО создания
# контейнера («opening seccomp profile … failed», RC=1, контейнеров ноль),
# валидный профиль — стартует, а до демона уезжает СОДЕРЖИМОЕ профиля, не путь
# (`docker inspect`: HostConfig.SecurityOpt = `seccomp={"defaultAction":…}`).
# То есть критерий двери 6 прикладывается ДОСЛОВНО: файл обязан существовать,
# вопрос ровно один — кладёт ли путь таска роли. ГЛАВНАЯ разница с дверью 6 —
# в моменте: там отказ на `config`, тут на `up` (единственной её не зовём —
# `config` ещё и относительное значение не разворачивает).
#
# ЭТО ТА САМАЯ МУТАЦИЯ, РАДИ КОТОРОЙ ЗАВЕДЕНА КАРТОЧКА: до #1120 первый кейс
# давал RC=0 и «проверено маунтов: 1 — все читаются».
vol_case "seccomp-профиль, которого роль не кладёт — отказ" 1 "НИ ОДНА таска роли" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "seccomp:/srv/чужое/profile.json"
YML
# Отказ обязан называть СВОЙ момент, а не чужой: у двери 6 «not found» видно на
# `config`, у этой — только на `up`. Соврать тут значило бы отправить читателя
# искать отказ там, где его не будет.
vol_case "отказ seccomp называет свой момент — up" 1 "до создания контейнера" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "seccomp:/srv/чужое/profile.json"
YML
# Обратная сторона мутации: путь, который роль КЛАДЁТ, обязан быть зелёным — и
# счётом двери 6, а не «проверено маунтов». Фикстура тут 0600 root:root, и это
# НЕ дефект: профиль открывает хостовая сторона выката, а до демона (и значит
# до контейнера) доезжает содержимое, а не путь — замерено `docker inspect`'ом.
# Что читатель у нас именно root — свойство раскладки, а не этого замера: роли
# зовут `docker compose up` под `become: true`; замер шёл не от root.
vol_case "seccomp-профиль, который роль кладёт — зелено" 0 "читаемых самим compose: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "seccomp:/srv/fixture/secret.conf"
YML
# ФОРМА С `=`: docker режет строку сначала по `=` и только потом по `:`
# (`opts.ParseSecurityOpts`; замерено — `seccomp=/nope/absent.json` даёт то же
# «opening seccomp profile … failed»). Разбор по одному двоеточию эту форму
# потерял бы МОЛЧА И ЗЕЛЁНО — ровно тот класс, ради которого сторож написан.
vol_case "seccomp через = ловится наравне с :" 1 "НИ ОДНА таска роли" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "seccomp=/srv/чужое/profile.json"
YML
# И ОБРАТНАЯ СТОРОНА ТОГО ЖЕ ПОРЯДКА РАЗДЕЛИТЕЛЕЙ: `=` ВНУТРИ пути делает
# строку НЕ профилем. ЗАМЕРЕНО ровно вот что: отказ приходит НЕ от чтения
# файла, а от демона, процитировавшего строку целиком («Error response from
# daemon: invalid --security-opt 2: "seccomp:/nope/ab=cd.json"») — значит
# профиль не читался. Модель «режет по первому `=`» это объясняет и сходится с
# парным замером (`seccomp=/nope/ab:cd.json` — наоборот, профиль), но сама она
# из вывода не наблюдаема. Сторож, разобравший эту строку как путь, покраснел
# бы там, где docker профиль не читает вовсе, — то есть дал бы ложный отказ.
vol_case "= внутри пути — не профиль, ложного отказа нет" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "seccomp:/srv/чужое/ab=cd.json"
YML
# ЛЕГАЛЬНОЕ, КОТОРОЕ КРАСНЕТЬ НЕ ИМЕЕТ ПРАВА. Все пять строк — не пути, и все
# пять замерены на живом демоне: контейнер с каждой из них стартует (RC=0).
# Ложный отказ на них был бы дороже самой двери: `security_opt` в ролях сегодня
# не используется вовсе, а появится он скорее всего именно так. `unconfined`
# стоит в ОБОИХ написаниях: разделителя у docker два, и пропуск «без профиля»
# обязан работать в каждом.
vol_case "легальные security_opt не путь и не краснеют" 0 "проверено маунтов: 1" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "no-new-privileges:true"
	      - "label:disable"
	      - "apparmor:docker-default"
	      - "seccomp:unconfined"
	      - "seccomp=unconfined"
YML
# ОТНОСИТЕЛЬНЫЙ профиль — та же граница, что у маунта: compose разрешает его от
# каталога COMPOSE-ПРОЕКТА на боксе (замерено: запуск из `/tmp` с `-f` на файл в
# другом каталоге — профиль нашёлся рядом с файлом, контейнер стартовал), а
# сторожу этот каталог неоткуда знать. Отказ громкий, диагноз общий с дверью 1.
vol_case "относительный seccomp-профиль — громкий отказ" 1 "ОТНОСИТЕЛЬНЫЙ хостовый путь" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "seccomp:./profile.json"
YML
# Нестроковая запись — отказ, а не тихий пропуск: `security_opt` схемой не
# закрыт, и «не понял» обязано выглядеть как отказ.
vol_case "security_opt нестрокой — громкий отказ" 1 "не строка" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - 123
YML
# `seccomp` БЕЗ пути — не форма, а поломка: docker на ней ПАДАЕТ ПАНИКОЙ
# (замерено, RC=2). Тихо пропустить значит приучиться пропускать записи
# security_opt без разбора — той же привычкой сторож терял двери 5 и 6.
vol_case "seccomp без пути — громкий отказ" 1 "без пути к профилю" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      - "seccomp:"
YML
# Форма записи: docker требует СПИСОК («must be a list», RC=15). Словарь —
# отказ, а не тихий пропуск.
vol_case "security_opt словарём — громкий отказ" 1 "не список" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    volumes:
	      - /srv/fixture/pg.conf:/etc/pg.conf:ro
	    security_opt:
	      seccomp: /srv/чужое/profile.json
YML
# Дверь 8 не bind-маунт: она НЕ гасит громкий отказ «bind-маунтов нет» — ровно
# как дверь 6. Слить их значило бы соврать о том, что именно проверено.
vol_case "seccomp не гасит «bind-маунтов нет»" 1 "нет bind-маунтов" <<-'YML'
	services:
	  db:
	    image: postgres:16
	    security_opt:
	      - "seccomp:/srv/fixture/secret.conf"
YML

# ── разбиение 88 ключей сервиса ПРИШПИЛЕНО, а не записано словами ───────────
# «Четыре списка сторожа вместе дают РОВНО эти 88, ни одного лишнего и ни
# одного пропущенного» шапка утверждает с #1107, и там честно сказано «сверено
# программно» — но сверено РАЗОВО, вне прогона: в тестах этого не было, то есть
# с тех пор ничто не мешало числу разъехаться. #1119 переложил `blkio_config` из инертных в
# разбираемые — то есть ровно та операция, которой это утверждение и ломается:
# ключ, потерянный при переносе, не краснит НИЧЕГО (он просто перестаёт быть
# понятым и начинает ронять прогон на живом compose), а ключ, продублированный
# в двух списках, не краснит вовсе. Здесь проверяется СВОЙСТВО, поэтому оно не
# протухает от следующего переноса.
echo
echo "── ключи сервиса: четыре списка дают ровно 88 без пересечений"
cat >"$work/keys88.py" <<-'PY'
	import importlib.util, sys
	spec = importlib.util.spec_from_file_location("guard", sys.argv[1])
	g = importlib.util.module_from_spec(spec)
	spec.loader.exec_module(g)
	doors = set(g.SERVICE_DOOR_KEYS)
	rejected = set(g.IMPORTING_KEYS) | set(g.OPAQUE_PATH_KEYS)
	inert = set(g.SERVICE_INERT_KEYS)
	overlap = (doors & rejected) | (doors & inert) | (rejected & inert)
	total = doors | rejected | inert
	if overlap:
	    sys.exit(f"списки пересекаются: {sorted(overlap)}")
	if len(total) != 88:
	    sys.exit(f"именованных ключей {len(total)}, а шапка обещает 88")
	print(f"{len(doors)}+{len(rejected)}+{len(inert)}={len(total)}")
PY
rc=0
out="$(python3 "$work/keys88.py" "$root/infra/ci/mounted_config_access.py" 2>&1)" || rc=$?
if [ "$rc" = 0 ] && printf '%s' "$out" | grep -qF "8+5+75=88"; then
	echo "ok   ключи сервиса: 8 дверей + 5 отвергаемых + 75 инертных = 88, пересечений нет"
	pass=$((pass + 1))
else
	echo "FAIL ключи сервиса: разбиение 88 разъехалось с шапкой (код $rc)"
	printf '%s\n' "$out" | sed 's/^/      /'
	fail=$((fail + 1))
fi

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

# ─── ГЕЙТ «СГЕНЕРИРОВАННОЕ СОВПАДАЕТ С ИСХОДНИКОМ» (proto/, tracker #1110)
#
# `proto/agentlink/v1/*.pb.go` ЗАКОММИЧЕНЫ, а генерируются локально через buf.
# До #1110 ни один workflow генерацию не запускал и не диффил (grep по
# `.github/workflows/*` на `buf|protoc|protobuf|go generate|generate` давал
# единственное попадание — комментарий про `BufferEmptyReady` в `infra.yml`),
# так что разъезд между `.proto` и `.pb.go` заметить было нечем.
#
# ЧТО ИМЕННО ЗДЕСЬ ПРИШПИЛЕНО, И ГДЕ ГРАНИЦА. Кейсы гоняют
# `infra/ci/proto-generated-check.sh` — ПРОВЕРЯЮЩУЮ половину гейта, ту, что
# отвечает «разошлось ли дерево под proto/ после регенерации». Саму генерацию
# (`docker build` + `buf generate`) они не запускают: докер ей нужен, а этому
# файлу — нет, и перевод «правка .proto без регенерации» → «грязное дерево»
# делает не скрипт, а workflow (он удаляет `*.pb.go` ПЕРЕД генерацией). Поэтому
# кейсы перебирают СОСТОЯНИЯ ДЕРЕВА, а не сорта правок: у каждого состояния
# исход ровно один из двух, и оба конца пришпилены.
#
# Ложное срабатывание тут дороже отсутствия гейта — гейт, краснеющий на ровном
# месте, научат обходить. Поэтому ЗЕЛЁНЫХ кейсов два, и второй не формальность:
# грязь ВНЕ proto/ этот гейт не касается вовсе.
if [ -z "${BIRDMAN_TRAP_SELFTEST:-}" ]; then
	echo
	echo "── гейт «сгенерированное совпадает с исходником» (proto/)"

	protocheck="$(dirname "$here")/proto-generated-check.sh"
	if [ ! -x "$protocheck" ]; then
		echo "FAIL гейт proto: $protocheck не найден или не исполняем"
		fail=$((fail + 1))
	else
		# proto_scaffold <repo> — миниатюра раскладки: `.proto`, два его
		# сгенерированных зеркала и один файл ВНЕ proto/ (нужен кейсу про грязь
		# снаружи — без него ту сторону проверить не на чем).
		proto_scaffold() {
			local repo="$1"
			mkdir -p "$repo/proto/agentlink/v1"
			printf 'syntax = "proto3";\npackage agentlink.v1;\n' \
				>"$repo/proto/agentlink/v1/agentlink.proto"
			printf '// Code generated by protoc-gen-go. DO NOT EDIT.\npackage agentlinkv1\n' \
				>"$repo/proto/agentlink/v1/agentlink.pb.go"
			printf '// Code generated by protoc-gen-go-grpc. DO NOT EDIT.\npackage agentlinkv1\n' \
				>"$repo/proto/agentlink/v1/agentlink_grpc.pb.go"
			echo "# birdman" >"$repo/README.md"
			git -C "$repo" init -q
			git -C "$repo" config user.email t@example.com
			git -C "$repo" config user.name test
			git -C "$repo" add -A
			git -C "$repo" commit -qm "scaffold"
		}

		# proto_case <имя> <ожидаемый код> <мутация: shell в корне репо> [текст в выводе]
		# Текст в выводе — не украшение: гейт, который краснеет НЕ НАЗЫВАЯ
		# разошедшийся путь, отправляет следующего читателя искать вслепую.
		proto_case() {
			local name="$1" want="$2" mutate="$3" expect_text="${4:-}"
			local repo="$work/protogate/$(echo "$name" | tr -c 'a-zA-Z0-9' '_')"
			mkdir -p "$repo"
			proto_scaffold "$repo"
			( cd "$repo" && eval "$mutate" )
			local out rc=0
			out="$("$protocheck" "$repo" 2>&1)" || rc=$?
			if [ "$rc" != "$want" ]; then
				echo "FAIL гейт proto: $name — код $rc, ожидался $want"
				printf '%s\n' "$out" | sed 's/^/      /'
				fail=$((fail + 1))
				return
			fi
			if [ -n "$expect_text" ] && ! printf '%s' "$out" | grep -qF "$expect_text"; then
				# Скобки обязательны: за именем стоит многобайтная кавычка.
				echo "FAIL гейт proto: $name — в выводе нет «${expect_text}»"
				printf '%s\n' "$out" | sed 's/^/      /'
				fail=$((fail + 1))
				return
			fi
			echo "ok   гейт proto: $name (код $rc)"
			pass=$((pass + 1))
		}

		# ЗЕЛЁНОЕ 1 — обратная проверка: регенерация воспроизвела ровно то, что
		# лежит в git. Замерено на настоящем дереве #1110: пинованный тулчейн
		# (buf 1.55.1, protoc-gen-go v1.36.6, protoc-gen-go-grpc v1.5.1) выдал
		# оба закоммиченных файла ПОБАЙТОВО.
		proto_case "чистое дерево остаётся зелёным" 0 ':'
		# ЗЕЛЁНОЕ 2 — страховка от ложного красного: грязь ВНЕ proto/ не дело
		# этого гейта. Пропади в скрипте `-- proto`, и красить начала бы любая
		# посторонняя правка в дереве.
		proto_case "грязь вне proto/ не краснит" 0 'echo dirty >>README.md'
		# КРАСНОЕ 1, главный класс — хенд-эдит в `.pb.go`. Тот же след
		# оставляет и правка `.proto`, которую регенерация перенесла в зеркало.
		proto_case "правка .pb.go краснит" 1 \
			'echo "// hand edit" >>proto/agentlink/v1/agentlink.pb.go' \
			'agentlink.pb.go'
		# КРАСНОЕ 2 — новый `.proto`, чей `.pb.go` забыли закоммитить. `git diff`
		# такого не видит ВОВСЕ, потому в скрипте `git status --porcelain -uall`.
		proto_case "незакоммиченный сгенерированный файл краснит" 1 \
			'printf "package agentlinkv1\n" >proto/agentlink/v1/extra.pb.go' \
			'extra.pb.go'
		# КРАСНОЕ 3 — генерация не произвела ничего и вышла нулём. Workflow
		# удаляет `*.pb.go` ПЕРЕД генерацией ровно затем, чтобы этот случай
		# приехал сюда удалением, а не чистым деревом (то есть красным, а не
		# «молча зелёным»).
		proto_case "пропавший сгенерированный файл краснит" 1 \
			'rm -f proto/agentlink/v1/agentlink.pb.go' \
			'agentlink.pb.go'
		# КРАСНОЕ 4 — приёмка не бывает пустой: нет ни одного трекнутого
		# `*.pb.go`, значит проверять нечего и «зелёное» не значило бы ничего.
		proto_case "пустая приёмка не проходит молча" 1 \
			'git rm -q proto/agentlink/v1/agentlink.pb.go proto/agentlink/v1/agentlink_grpc.pb.go && git commit -qm drop' \
			'ни одного трекнутого'
	fi
fi

# ─── ГЕЙТ «НЕТ ТРЕКНУТЫХ ИСПОЛНЯЕМЫХ АРТЕФАКТОВ» (tracker #1112) ─────────────
#
# 12.08 коммитом `bebbbac` (про allocate-metadata, к бинарю отношения не
# имевшим) в индекс случайным `git add` уехал `sdk/mockagent/mockagent` — 4.2 МБ
# ELF из `go build` без `-o`, запущенного прямо в каталоге модуля. Потребителей
# у файла не было НИ ОДНОГО (smoke.sh, README и CMakeLists собирают mockagent
# заново во временный каталог), но он уже разошёлся с репозиторием: go1.26.5
# при пине 1.24, `CGO_ENABLED=1` при каноне `CGO_ENABLED=0`, внутри —
# абсолютный путь другого чекаута владельца. Сверял его с исходником никто:
# единственным шагом CI про mockagent был `go vet ./...`. Тот же класс
# «выглядит покрытым», что закрывала #1110.
#
# ЧТО ИМЕННО ЗДЕСЬ ПРИШПИЛЕНО. Кейсы гоняют `infra/ci/no-tracked-binaries.sh`
# на одноразовых репозиториях и перебирают СОСТОЯНИЯ ДЕРЕВА. Ложное
# срабатывание тут дороже отсутствия гейта — краснеющий на ровном месте гейт
# научат обходить, — поэтому зелёных кейсов столько же, сколько красных, и
# каждый закрывает СВОЙ класс ложного красного: бит `755` на скрипте (таких в
# дереве 18), трекнутый НЕисполняемый бинарь (два PNG в `docs/images/`) и
# НЕтрекнутый бинарь в дереве (он есть у всякого, кто собирал локально).
#
# Фикстуры бинарных форматов — ЗАГЛУШКИ ИЗ ОДНОЙ МАГИИ, а не настоящие файлы:
# чекер читает ровно первые 8 байт, и восьми байт ему довольно.
if [ -z "${BIRDMAN_TRAP_SELFTEST:-}" ]; then
	echo
	echo "── гейт «нет трекнутых исполняемых артефактов»"

	bincheck="$(dirname "$here")/no-tracked-binaries.sh"
	if [ ! -x "$bincheck" ]; then
		echo "FAIL гейт бинарей: $bincheck не найден или не исполняем"
		fail=$((fail + 1))
	else
		# bin_scaffold <repo> — намеренно ПУСТАЯ миниатюра: только README и
		# .gitignore. Всё, что кейс проверяет, кейс же и кладёт — иначе
		# непонятно, на что именно он ответил.
		bin_scaffold() {
			local repo="$1"
			mkdir -p "$repo"
			echo "# birdman" >"$repo/README.md"
			printf 'build/\n' >"$repo/.gitignore"
			git -C "$repo" init -q
			git -C "$repo" config user.email t@example.com
			git -C "$repo" config user.name test
			git -C "$repo" add -A
			git -C "$repo" commit -qm "scaffold"
		}

		# bin_case <имя> <ожидаемый код> <мутация: shell в корне репо> [текст в выводе]
		# Текст в выводе — не украшение: гейт, который краснеет НЕ НАЗЫВАЯ
		# путь, отправляет следующего читателя искать вслепую.
		bin_case() {
			local name="$1" want="$2" mutate="$3" expect_text="${4:-}"
			local repo="$work/bingate/$(echo "$name" | tr -c 'a-zA-Z0-9' '_')"
			mkdir -p "$repo"
			bin_scaffold "$repo"
			(cd "$repo" && eval "$mutate")
			local out rc=0
			out="$("$bincheck" "$repo" 2>&1)" || rc=$?
			if [ "$rc" != "$want" ]; then
				echo "FAIL гейт бинарей: $name — код $rc, ожидался $want"
				printf '%s\n' "$out" | sed 's/^/      /'
				fail=$((fail + 1))
				return
			fi
			if [ -n "$expect_text" ] && ! printf '%s' "$out" | grep -qF "$expect_text"; then
				# Скобки обязательны: за именем стоит многобайтная кавычка.
				echo "FAIL гейт бинарей: $name — в выводе нет «${expect_text}»"
				printf '%s\n' "$out" | sed 's/^/      /'
				fail=$((fail + 1))
				return
			fi
			echo "ok   гейт бинарей: $name (код $rc)"
			pass=$((pass + 1))
		}

		# ЗЕЛЁНОЕ 1 — базовая линия: обычное дерево не краснит.
		bin_case "чистое дерево остаётся зелёным" 0 ':'
		# ЗЕЛЁНОЕ 2, ГЛАВНЫЙ ложный красный — БИТ РЕЖИМА НЕ СИГНАЛ. В настоящем
		# дереве трекнутых файлов с `100755` девятнадцать, и восемнадцать из них
		# обычные скрипты. Сломайся здесь сигнал на режим — гейт покраснел бы на
		# всех восемнадцати и был бы обойдён в первый же день.
		bin_case "скрипт с битом 755 не краснит" 0 \
			'printf "#!/bin/sh\nexit 0\n" >tool.sh && chmod 755 tool.sh && git add -A && git commit -qm tool'
		# ЗЕЛЁНОЕ 3 — трекнутый бинарь, который НЕ исполняемый формат: в репо
		# это два PNG в docs/images/. Гейт про исполняемые артефакты, не про
		# бинарники вообще.
		bin_case "трекнутый PNG не краснит" 0 \
			'mkdir -p docs/images && printf "\211PNG\r\n\032\n" >docs/images/panel.png && git add -A && git commit -qm png'
		# ЗЕЛЁНОЕ 4 — НЕтрекнутый бинарь в дереве. Он есть у каждого, кто собирал
		# локально; покрасней гейт на нём — красное стало бы нормой и потеряло
		# бы смысл. Ровно то, ради чего в скрипте `git ls-files`, а не обход ФС.
		bin_case "нетрекнутый ELF в дереве не краснит" 0 \
			'mkdir -p sdk/mockagent && printf "\177ELF\002\001\001\001" >sdk/mockagent/mockagent'
		# КРАСНОЕ 1 — САМ ДЕФЕКТ #1112, один в один.
		bin_case "трекнутый ELF краснит" 1 \
			'mkdir -p sdk/mockagent && printf "\177ELF\002\001\001\001" >sdk/mockagent/mockagent && git add -Af && git commit -qm oops' \
			'sdk/mockagent/mockagent'
		# КРАСНОЕ 2 — формат хоста, с которого чаще всего и коммитят.
		bin_case "трекнутый Mach-O краснит" 1 \
			'printf "\317\372\355\376\014\000\000\001" >helper && git add -Af && git commit -qm oops' \
			'Mach-O'
		# КРАСНОЕ 3, НЕОЧЕВИДНОЕ И САМОЕ ЦЕННОЕ — .gitignore ТРЕКНУТЫЙ ФАЙЛ НЕ
		# ЗАЩИЩАЕТ. Правило игнора действует только на нетрекнутые пути, так что
		# «добавили в .gitignore» без `git rm --cached` оставляет бинарь в
		# индексе, а `git status` при этом ЧИСТ (замерено на настоящем дереве
		# #1112). Без этого кейса лечение можно было бы сделать наполовину и
		# считать закрытым.
		bin_case "трекнутый ELF краснит даже будучи в .gitignore" 1 \
			'mkdir -p sdk/mockagent && printf "\177ELF\002\001\001\001" >sdk/mockagent/mockagent && git add -Af sdk/mockagent/mockagent && printf "sdk/mockagent/mockagent\n" >>.gitignore && git add -A && git commit -qm oops' \
			'sdk/mockagent/mockagent'
		# КРАСНОЕ 4 — приёмка не бывает пустой: ноль трекнутых файлов, значит
		# проверять нечего и «зелёное» не значило бы ничего.
		bin_case "пустая приёмка не проходит молча" 2 \
			'git rm -q -r . && git commit -qm drop' \
			'НИ ОДНОГО трекнутого файла'
		# КРАСНОЕ 5 — не git-репозиторий вовсе: громкий отказ, а не тихий ноль.
		# Мимо bin_case: тот по построению заводит репозиторий.
		notrepo="$work/bingate-notrepo"
		mkdir -p "$notrepo"
		rc=0
		out="$("$bincheck" "$notrepo" 2>&1)" || rc=$?
		if [ "$rc" = 2 ] && printf '%s' "$out" | grep -qF "не git-репозиторий"; then
			echo "ok   гейт бинарей: не-git-каталог отвергается (код $rc)"
			pass=$((pass + 1))
		else
			echo "FAIL гейт бинарей: не-git-каталог — код $rc, ожидался 2"
			printf '%s\n' "$out" | sed 's/^/      /'
			fail=$((fail + 1))
		fi
	fi
fi

echo
echo "прошло: $pass, упало: $fail"
# Часовой снимается ровно здесь: всё, что ниже, — это и есть финал.
reached_end=1
[ "$fail" -eq 0 ]
