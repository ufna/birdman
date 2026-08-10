#!/usr/bin/env python3
"""Render rules.yml.j2 with the role defaults — no ansible run, no host touched.

Usage: render_rules.py <role-dir> <output.yml>

Only the role's defaults/main.yml is used as the variable set: the alert rules
must be valid with the shipped thresholds, which is exactly what an operator
gets before overriding anything. jinja2 + PyYAML ship with ansible, so this
needs nothing extra installed.
"""
import sys

import yaml
from jinja2 import Environment, FileSystemLoader

if len(sys.argv) != 3:
    sys.exit(__doc__)
role, out = sys.argv[1], sys.argv[2]

variables = yaml.safe_load(open(f"{role}/defaults/main.yml"))
variables["ansible_managed"] = "Ansible managed (rendered by tests/render_rules.py)"

env = Environment(loader=FileSystemLoader(f"{role}/templates"), keep_trailing_newline=True)
text = env.get_template("rules.yml.j2").render(**variables)
with open(out, "w") as fh:
    fh.write(text)

# Parse what we produced: a template that renders to invalid YAML is a failure
# on its own, before promtool ever sees it.
doc = yaml.safe_load(text)
rules = [r for g in doc["groups"] for r in g["rules"]]
print(f"rendered {out}: {len(doc['groups'])} groups, {len(rules)} rules")
