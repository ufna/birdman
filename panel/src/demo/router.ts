// Демо-роутер: подменяет window.fetch и отвечает на публичный REST панели
// сгенерированным флотом. Живёт ТОЛЬКО во второй точке входа (demo.html) —
// в прод-бандл (index.html) не входит, см. гейт маркера в panel/build.sh.
import type { SessionInfo } from '../lib/api';
import { DEMO_MARKER } from './marker';

/** Ответ-JSON с проставленным Content-Type (панель читает text() → JSON.parse). */
export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const SESSION: SessionInfo = { name: `demo-admin (${DEMO_MARKER})`, scopes: ['admin'] };

/** Путь запроса без origin и query. */
function pathOf(input: RequestInfo | URL): string {
  const raw = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
  return new URL(raw, 'http://demo.invalid').pathname;
}

export async function demoFetch(input: RequestInfo | URL, _init?: RequestInit): Promise<Response> {
  const path = pathOf(input);
  if (path === '/v1/session') return jsonResponse(SESSION);
  console.error(`demo: необслуженная ручка ${path} — экран останется пустым`);
  return jsonResponse({ error: 'demo_unhandled', detail: path }, 500);
}

/** Ставит демо-fetch на window. Вызывается ДО бутстрапа приложения. */
export function installDemoFetch(): void {
  window.fetch = demoFetch as typeof window.fetch;
}
