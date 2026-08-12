#!/usr/bin/env bash
# Тесты роли birdman_overlay — LOCAL only, ни один хост не трогается и плейбук
# не гоняется: шаблон compose рендерится настоящим ansible'ом во временный
# каталог, образ собирается тем же `docker build`, каким его собирает роль на
# боксе (tasks/prepare.yml), и никуда не публикуется.
#
#   ./infra/roles/birdman_overlay/tests/run.sh
#
# Что покрыто (tracker #1089): конфиг, который роль монтирует в контейнер,
# обязан этим контейнером ЧИТАТЬСЯ. Роль мониторинга положила alertmanager.yml
# 0600 root:root в контейнер, бегущий от nobody, — краш-луп с рождения, 2380
# рестартов за 40 часов, и роль при этом зелёная (#1072). Здесь монтируется
# /etc/birdman/overlay (0700 root) с приватным ключом WireGuard, а образ
# birdman-overlay:local собирается из alpine БЕЗ USER, то есть бежит от root, —
# сегодня раскладка сходится. Сторож нужен ровно затем, чтобы завтрашняя смена
# образа (или режима каталога) не приехала молча.
#
# Образ строится ВСЕГДА, а не берётся готовым: uid проверяемого контейнера
# добывается живой пробой у образа, и собранный из СЕГОДНЯШНЕГО Dockerfile
# роли — единственный, про который эта проба говорит правду. Тег тот же, что у
# роли (birdman_overlay_image): второй пин рядом с первым — это будущее
# расхождение. Контейнеров суите не нужно вовсе, так что имён и портов, за
# которые можно подраться с соседним прогоном, здесь нет.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
role="$(dirname "$here")"
repo="$(cd "$role/../../.." && pwd)"

# Тег берётся из ПИНА РОЛИ, а не пишется тут ещё раз. Хвостовой комментарий у
# этого ключа есть, и наивный `s/^ключ: *//p` утащил бы его в имя образа —
# `docker build -t` тогда падает на невалидном теге; поэтому режется ровно
# значение, до первого пробела/решётки.
image="$(sed -n 's/^birdman_overlay_image:[[:space:]]*"\{0,1\}\([^"#[:space:]][^"#[:space:]]*\).*/\1/p' "$role/defaults/main.yml")"
[ -n "$image" ] || { echo "не нашёл birdman_overlay_image в defaults" >&2; exit 1; }
case "$image" in
  # Скобки обязательны: за именем стоит МНОГОБАЙТНАЯ кавычка, и bash 3.2
  # (/bin/bash на macOS) забирает её первый байт в имя переменной — вместо
  # диагноза печаталось «image<байт>: unbound variable». Код тот же (1), но
  # отказ переставал объяснять, ЧТО не так с тегом. Замерено на битом теге.
  *[![:alnum:]:._/-]*) echo "тег образа выглядит битым: «${image}»" >&2; exit 1 ;;
esac

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

render() { # render <out-dir> <mode>
  mkdir -p "$1"
  ansible-playbook -i birdman-overlay-test, "$here/render_role.yml" \
    -e "birdman_out=$1" -e "birdman_overlay_test_mode=$2" >"$1/ansible.log" 2>&1 \
    || { cat "$1/ansible.log" >&2; return 1; }
}

# ── рендер обоих режимов ────────────────────────────────────────────────────
# Маунт у хаба и спока сегодня один и тот же, и проверяются оба именно поэтому:
# «сегодня одинаковые» — не свойство, а совпадение, и хаб-ветка шаблона (она
# под {% if %}) обязана попадать под тот же критерий.
echo "── render: spoke"
render "$work/spoke" spoke
grep -q 'BIRDMAN_WG_MODE: "spoke"' "$work/spoke/compose.yml" \
  || fail "спок отрендерился не споком"
! grep -q 'BIRDMAN_WG_FORWARDS' "$work/spoke/compose.yml" \
  || fail "у спока появились форвардеры — их несёт только хаб"

echo "── render: hub"
render "$work/hub" hub
grep -q 'BIRDMAN_WG_FORWARDS: "8444 9428 8428"' "$work/hub/compose.yml" \
  || fail "хаб потерял форвардеры control-plane"

# ── образ роли: тот же Dockerfile, тот же тег ────────────────────────────────
# После рендеров, а не до: рендер докера не требует, и при мёртвом демоне
# полезнее увидеть его результат, а отказ получить от того шага, который
# виноват.
echo "── build $image (role's own build context)"
docker build -q -t "$image" "$role/files" >"$work/build.log" 2>&1 \
  || { cat "$work/build.log" >&2; fail "образ роли не собрался — сторож маунтов не сможет спросить его uid"; }

# ── конфиг обязан читаться контейнером, в который смонтирован (#1072/#1089) ──
# BIRDMAN_SKIP_IMAGE_UID_PROBE=1 здесь НЕ отмычка: без пробы сторож слабеет до
# «владелец не root», а тут владелец root законно — потому что и контейнер
# root. Отсюда docker для этой сьюты обязателен, и его отсутствие обязано быть
# отказом. Так и есть: без пробы прогон покраснеет, а не позеленеет молча.
echo "── mounted config access"
for mode in spoke hub; do
  "$repo/infra/ci/mounted-config-access.sh" --role "$role" "$work/$mode/compose.yml"
done

echo "ALL OK"
