#!/usr/bin/env bash
# Unit tests for the vmalert rules of this role — LOCAL only, no host is
# touched and no playbook runs: the template is rendered with the role defaults
# into a temp dir and the rules are evaluated against synthetic series.
#
#   ./infra/roles/birdman_monitoring_dev/tests/run.sh
#
# Engine: `promtool test rules` (Prometheus). The rules run under vmalert in
# production, and MetricsQL is a superset of PromQL — every expression in
# rules.yml.j2 is plain PromQL, and the semantics these tests pin down
# (an aggregation over an ABSENT series yields nothing, not 0; increase() over
# a series that never rose yields 0) are identical in both engines. A local
# promtool is used when present, otherwise the pinned container image; nothing
# is installed on the host either way (containerization rule).
#
# Why these tests exist: BufferEmptyReadyProd/NonProd were DEAD — the ready
# series simply disappears when ready hits 0, so `== 0` never held and the case
# only looked covered (tracker #960).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
role="$(dirname "$here")"
image="${BIRDMAN_PROMTOOL_IMAGE:-prom/prometheus:v3.10.0}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

python3 "$here/render_rules.py" "$role" "$work/rules.yml"
cp "$here/rules_test.yml" "$work/rules_test.yml"

if command -v promtool >/dev/null 2>&1; then
	echo "promtool: $(command -v promtool)"
	promtool check rules "$work/rules.yml"
	promtool test rules "$work/rules_test.yml"
else
	echo "promtool: none on PATH, using $image"
	docker run --rm -v "$work:/w" -w /w --entrypoint /bin/promtool "$image" check rules rules.yml
	docker run --rm -v "$work:/w" -w /w --entrypoint /bin/promtool "$image" test rules rules_test.yml
fi
