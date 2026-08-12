#!/usr/bin/env bash
# Обёртка над mounted_config_access.py: выбор интерпретатора с PyYAML — в
# ОДНОМ месте, а не копией в раннере каждой роли (tracker #1089).
#
#   infra/ci/mounted-config-access.sh --role <каталог роли> [--vars <json>] \
#       [--allow-no-mounts] <отрендеренный compose>...
#
# Почему выбор вообще нужен: у CI PyYAML приезжает вместе с ansible-core в тот
# же python3, но у контрибьютора ansible может стоять из brew/pipx со своим
# интерпретатором — тогда берём его (последним кандидатом идёт shebang самого
# ansible-playbook). Иначе отказ выглядел бы как поломка теста, а не как
# отсутствие зависимости.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

pick_python() {
	local cand
	for cand in "${BIRDMAN_PYTHON:-}" python3 python; do
		[ -n "$cand" ] || continue
		command -v "$cand" >/dev/null 2>&1 || continue
		"$cand" -c 'import yaml' >/dev/null 2>&1 && { echo "$cand"; return 0; }
	done
	command -v ansible-playbook >/dev/null 2>&1 || return 1
	cand="$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||')"
	[ -x "$cand" ] && "$cand" -c 'import yaml' >/dev/null 2>&1 && { echo "$cand"; return 0; }
	return 1
}

py="$(pick_python)" || {
	echo "не нашёл python с PyYAML (python3 -m pip install pyyaml)" >&2
	exit 1
}
exec "$py" "$here/mounted_config_access.py" "$@"
