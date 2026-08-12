#!/usr/bin/env python3
"""Render vector.yaml.j2 with the role defaults — no ansible run, no host touched.

Usage: render_vector.py <role-dir> <output.yaml>

Хостовые переменные (inventory_hostname) ansible подставляет из инвентаря, и
на них разметка стрима НЕ опирается — они едут в лейблы node/region как есть,
поэтому здесь достаточно фиксированных значений. Всё остальное берётся из
defaults/main.yml роли: проверять надо ровно тот конфиг, который оператор
получает до любых override'ов. jinja2 + PyYAML приезжают вместе с ansible.
"""
import sys

import yaml
from jinja2 import Environment, FileSystemLoader

if len(sys.argv) != 3:
    sys.exit(__doc__)
role, out = sys.argv[1], sys.argv[2]

variables = yaml.safe_load(open(f"{role}/defaults/main.yml"))
variables["ansible_managed"] = "Ansible managed (rendered by tests/render_vector.py)"
variables["inventory_hostname"] = "birdman-test-node"

env = Environment(loader=FileSystemLoader(f"{role}/templates"), keep_trailing_newline=True)
text = env.get_template("vector.yaml.j2").render(**variables)
with open(out, "w") as fh:
    fh.write(text)

# Шаблон, отрендерившийся в невалидный YAML, — это уже провал, до того как
# vector о нём узнает.
doc = yaml.safe_load(text)
transforms = list(doc.get("transforms", {}))
print(f"rendered {out}: transforms {transforms}, sinks {list(doc.get('sinks', {}))}")
