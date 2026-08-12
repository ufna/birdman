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
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
checker="$(dirname "$here")/needs-ansible-check.py"
root="$(cd "$here/../../.." && pwd)"

work="$(mktemp -d)"
# Код возврата сохраняется ЯВНО: под bash 3.2 (/bin/bash на macOS) EXIT-trap,
# который сам ничем не завершается, отдаёт наружу код своей последней команды —
# успешного `rm`. Прогон, УПАВШИЙ ПОСРЕДИНЕ (например, на `set -u`), выходил из-за
# этого с нулём: суммарная строка «прошло/упало» до конца не доезжала, а вызвавший
# видел зелёное. Ровно то «выглядит покрытым», против чего написаны все три гейта
# ниже, — и обиднее всего в самом их раннере. Честный красный финал (`[ fail -eq 0 ]`)
# бага не касался, поэтому не проявлялось.
trap 'st=$?; rm -rf "$work"; exit "$st"' EXIT

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

echo
echo "прошло: $pass, упало: $fail"
[ "$fail" -eq 0 ]
