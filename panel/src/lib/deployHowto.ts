// «Как залить билд»: чистые билдеры для карточки-инструкции на экране
// Деплоев (Task 1, docs/superpowers/plans/2026-07-09-deploys-howto.md).
// Ничего не рендерит — curl-строки и ctx собираются тут (не в JSX), чтобы
// (а) их было легко юнит-тестить точными строками и (б) держать guard
// no-hardcoded.test.ts счастливым: кириллицы в этом файле нет вообще (это
// код-примеры, не UI-текст — подписи вокруг них живут в DeployHowto.tsx
// через t()).
//
// Task 7 (registries v1 §5, docs/superpowers/specs/2026-07-09-registries-design.md)
// добавляет опциональную подстановку deploy-ключа в билдеры (HowtoKey) и два
// чистых хелпера для пикера/inline-создания на карточке: deployKeyOptions
// (фильтр активных deploy/admin ключей) и defaultDeployKeyName (deploy-YYYYMMDD).

import type { ApiKey } from './api';

/**
 * Контекст карточки: origin — адрес панели/master (за SSH-туннелем это
 * один и тот же хост, см. auth.go); project — слаг для подстановки в
 * команды; exampleImage — пример image_ref для шага 1 и тела registerVersionCurl.
 */
export interface HowtoCtx {
  origin: string;
  project: string;
  exampleImage: string;
}

/** Дефолтный пример образа — когда ни у одного проекта ещё нет версий.
 *  Намеренно без хардкода реальной org/repo (OSS/self-host). */
export const GENERIC_IMAGE_EXAMPLE = 'ghcr.io/<org>/<game>-server:1.2.3';

/** Плейсхолдер проекта — когда карточка не может подставить один
 *  конкретный слаг (0 или 2+ проектов на экране, см. buildHowtoCtx). */
const PROJECT_PLACEHOLDER = '<project>';

/**
 * Deploy-ключ, чьи данные подставляются в curl-команды карточки (Task 7 §5):
 * name всегда всплывает комментарием `# key: <name>` над командой; secret,
 * если известен (ключ создан прямо сейчас), заменяет плейсхолдер
 * $BIRDMAN_DEPLOY_KEY в Authorization. Без secret (выбран СУЩЕСТВУЮЩИЙ ключ)
 * плейсхолдер остаётся — секреты старых ключей невосстановимы by design
 * (bcrypt-хэш, показ ровно один раз при создании).
 */
export interface HowtoKey {
  name: string;
  secret?: string;
}

function keyCommentLines(key: HowtoKey | undefined): string[] {
  return key !== undefined ? [`# key: ${key.name}`] : [];
}

function bearerToken(key: HowtoKey | undefined): string {
  return key?.secret ?? '$BIRDMAN_DEPLOY_KEY';
}

/**
 * curl регистрации версии: POST {origin}/v1/versions (скоуп deploy), тело
 * {project, semver, image_ref, channel} — поля сверены с
 * master/internal/httpapi/handlers.go (createVersionRequest). Без key — только
 * плейсхолдер $BIRDMAN_DEPLOY_KEY, секретов в командах нет (поведение по
 * умолчанию не меняется — Task 7 добавляет key строго опционально). Формат
 * Authorization: Bearer сверен с master/internal/httpapi/auth.go.
 */
export function registerVersionCurl(ctx: HowtoCtx, semver: string, channel: 'staging' | 'prod', key?: HowtoKey): string {
  const body = JSON.stringify(
    { project: ctx.project, semver, image_ref: ctx.exampleImage, channel },
    null,
    2,
  );
  return [
    ...keyCommentLines(key),
    `curl -H "Authorization: Bearer ${bearerToken(key)}" \\`,
    '  -H "Content-Type: application/json" \\',
    `  -X POST ${ctx.origin}/v1/versions \\`,
    `  -d '${body}'`,
  ].join('\n');
}

/**
 * curl деплоя уже зарегистрированной версии: POST {origin}/v1/deploy, тело
 * {version_id} (см. panel/src/lib/api.ts — api.deploy). version_id — плейсхолдер:
 * реальный id смотрят в таблице версий этого же экрана (появится там после
 * регистрации). key — см. registerVersionCurl.
 */
export function deployCurl(ctx: HowtoCtx, key?: HowtoKey): string {
  return [
    ...keyCommentLines(key),
    `curl -H "Authorization: Bearer ${bearerToken(key)}" \\`,
    '  -H "Content-Type: application/json" \\',
    `  -X POST ${ctx.origin}/v1/deploy \\`,
    `  -d '{"version_id":"<version_id>"}'`,
  ].join('\n');
}

/**
 * ctx карточки из данных экрана: origin — как передал вызывающий (обычно
 * window.location.origin); project — реальный слаг, ЕСЛИ на экране ровно
 * один проект (карточка глобальная, не привязана к конкретному проекту),
 * иначе плейсхолдер <project>; exampleImage — image_ref активной версии
 * выбранного проекта, иначе первой попавшейся, иначе generic-заглушка.
 */
export function buildHowtoCtx(
  origin: string,
  projects: { project: string; versions: { state: string; image_ref: string }[] }[],
): HowtoCtx {
  if (projects.length !== 1) {
    return { origin, project: PROJECT_PLACEHOLDER, exampleImage: GENERIC_IMAGE_EXAMPLE };
  }
  const [{ project, versions }] = projects;
  const active = versions.find((v) => v.state === 'active') ?? versions[0];
  return { origin, project, exampleImage: active?.image_ref ?? GENERIC_IMAGE_EXAMPLE };
}

/**
 * Ключи, которые имеет смысл предлагать в пикере шага 2 (Task 7 §5): активные
 * (не отозванные) и способные аутентифицировать деплой-команды — скоуп
 * `deploy` или `admin` (admin включает deploy, master §6). Revoked-ключ в
 * комментарии команды был бы вводящим в заблуждение, а ключ без deploy/admin
 * не смог бы выполнить сами curl-команды.
 */
export function deployKeyOptions(keys: ApiKey[]): ApiKey[] {
  return keys.filter((k) => k.revoked_at === null && (k.scopes.includes('deploy') || k.scopes.includes('admin')));
}

/**
 * Дефолтное имя для только что создаваемого deploy-ключа: `deploy-YYYYMMDD`.
 * UTC-геттеры — значение детерминировано независимо от локальной TZ вызывающего
 * (важно для тестов и для консистентности с остальным UTC-соглашением панели).
 */
export function defaultDeployKeyName(now: Date = new Date()): string {
  const y = now.getUTCFullYear();
  const m = String(now.getUTCMonth() + 1).padStart(2, '0');
  const d = String(now.getUTCDate()).padStart(2, '0');
  return `deploy-${String(y)}${m}${d}`;
}
