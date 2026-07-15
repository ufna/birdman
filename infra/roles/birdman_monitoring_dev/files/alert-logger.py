#!/usr/bin/env python3
# birdman alerts sink: постоянный webhook-приёмник ПОЗАДИ alertmanager
# (vmalert → alertmanager → этот sink), роль birdman_monitoring_dev. Принимает
# POST на ЛЮБОЙ путь, дописывает тело как ОДНУ JSON-строку (с меткой
# received_at) в файл и отвечает 200. Понимает и webhook-объект alertmanager
# (нормализует его в список "alerts"), и прямой список алертов в стиле vmalert.
# Слушает внутри контейнера 0.0.0.0:PORT, наружу — только на 127.0.0.1. Stdlib-only.
import json
import os
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG_PATH = os.environ.get("BIRDMAN_ALERTS_LOG", "/var/log/birdman/alerts.log")
LISTEN_HOST = os.environ.get("BIRDMAN_SINK_HOST", "0.0.0.0")
LISTEN_PORT = int(os.environ.get("BIRDMAN_SINK_PORT", "9094"))


class Handler(BaseHTTPRequestHandler):
    def _respond(self, code=200):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":"ok"}')

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0) or 0)
        raw = self.rfile.read(length) if length else b""
        rec = {
            "received_at": datetime.now(timezone.utc).isoformat(),
            "path": self.path,
        }
        try:
            body = json.loads(raw.decode("utf-8")) if raw else None
            # alertmanager шлёт webhook-объект {"alerts":[...], ...} — разворачиваем
            # его в список алертов; прямой список от vmalert кладём как есть.
            if isinstance(body, dict) and isinstance(body.get("alerts"), list):
                rec["alerts"] = body["alerts"]
            else:
                rec["alerts"] = body
        except Exception:
            rec["raw"] = raw.decode("utf-8", "replace")
        line = json.dumps(rec, ensure_ascii=False)
        with open(LOG_PATH, "a", encoding="utf-8") as fh:
            fh.write(line + "\n")
        self._respond(200)

    def do_GET(self):
        # health/liveness (используется docker healthcheck)
        self._respond(200)

    def log_message(self, *args):
        # тихо: сами алерты уходят в файл, container-логи не засоряем
        return


if __name__ == "__main__":
    ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), Handler).serve_forever()
