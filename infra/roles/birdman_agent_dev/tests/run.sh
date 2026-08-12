#!/usr/bin/env bash
# Unit tests for the vector VRL transform of this role — LOCAL only, no host is
# touched and no playbook runs: the template is rendered with the role defaults
# into a temp dir and the transform is exercised by the real vector binary.
#
#   ./infra/roles/birdman_agent_dev/tests/run.sh
#
# Engine: `vector test` (the same image the role deploys, pinned by
# birdman_vector_image). Переписывать регекс на другом языке ради теста нельзя:
# проверять надо ту же машинку, что крутится на ноде, — от `parse_regex`
# требуется в том числе его СЕМАНТИКА ЧАСТИЧНОГО СОВПАДЕНИЯ, ради которой и
# нужен якорь `^`.
#
# Зачем эти тесты (tracker #1014, хвост #994): пара (project, env) вешается
# стрим-лейблами ЗДЕСЬ, из пути файла, и по ним master сужает запрос
# привязанного ключа. Снятие якоря `^/var/log/birdman/servers/` превращает
# плоский `/var/log/birdman/servers/legacy.log` в пару project=birdman,
# env=servers (оба — валидные слаги), и ключ, привязанный к ней, получает всю
# нелейблованную историю флота. До этих тестов такая мутация проходила CI
# зелёной: покрытия у шаблона не было никакого.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
role="$(dirname "$here")"
image="${BIRDMAN_VECTOR_IMAGE:-$(sed -n 's/^birdman_vector_image: *//p' "$role/defaults/main.yml")}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

python3 "$here/render_vector.py" "$role" "$work/vector.yaml"
cp "$here/vector_test.yaml" "$work/vector_test.yaml"

echo "vector: $image"
docker run --rm -v "$work:/w" "$image" test /w/vector.yaml /w/vector_test.yaml
