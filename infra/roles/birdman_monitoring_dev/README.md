# Development monitoring stack

The role owns Birdman's separate loopback-only VictoriaMetrics, logs, alerting and
Grafana stack on hetzner-dev. The fleet node_exporter belongs to hgdev-infra and
is scraped over its private address 10.20.4.10:4027; do not recreate that exporter.

Grafana is pinned independently from the central monitoring VM. Before a major
upgrade, back up its SQLite database and plugins, exercise migrations on a detached
copy with networking disabled, then verify the production datasource, dashboards
and Viewer access. Preserve the generated root-only administrator password.

Configuration files are individual Docker bind mounts. After replacing a mounted
file, restart the consumer so it receives the new inode. An HTTP reload alone can
continue reading the old file.
