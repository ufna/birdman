// Guard против регрессий i18n. Проверок ДВЕ, и они ловят РАЗНОЕ.
//
// (A) КИРИЛЛИЦА в литералах — исходная проверка: любой русский текст мимо
//     каталога (t()/tp()) ловится в CI, а не глазами на ревью.
//
// (B) АНГЛИЙСКИЕ user-facing строки в JSX — добавлено в tracker #1012. Раньше
//     проверка была только (A), и это закрывало более ЛЁГКИЙ случай, оставляя
//     более тяжёлый: EN — каноничный каталог (en.ts — источник MessageKey),
//     поэтому захардкоженная английская строка выглядит на английском
//     интерфейсе идеально и обнаруживается только переключением на RU, то есть
//     на глаз и случайно. Ровно этот режим отказа чинили #996 и #1000.
//
//     Замер, на котором это построено (прогнан на дереве #1012, не выведен):
//     захардкоженные JSX-текст + aria-label + title, вписанные в
//     components/ui.tsx, проходили ВЕСЬ сьют — 480/480 passed, включая этот
//     файл. После (B) тот же ввод падает здесь.
//
//     Что (B) НЕ ловит, и это надо знать: строку, ВОЗВРАЩЁННУЮ из функции
//     (`return 'Master returned…'` в хелпере, который потом рендерят). Она не
//     JSX-узел, и отличить её от машинного кода/PromQL/URL по форме нельзя —
//     такую держат содержательные тесты поверхности, а не гвард. Именно она
//     была контрфактом карточки #1012: на сегодняшнем дереве её ловят три
//     теста metricMessage.test.tsx, но не этот файл.
//
// Что сканируем: panel/src/**/*.{ts,tsx}, КРОМЕ каталогов locales/ и test/ и
// файлов *.test.*. Комментарии (// и /* */) вырезаются — русские комментарии
// разрешены (код проекта английский, но пояснения по-русски — норма здесь).
// Строковые/шаблонные литералы сохраняются: кириллица в них = нарушение.
//
// Точечное исключение: строка с маркером `i18n-allow` пропускается — им помечен
// сам locale-слой форматтера (lib/format.ts), где русские подписи легитимны.
//
// Файлы читаем через import.meta.glob('?raw') (Vite-нативно, без node:fs — под
// tsconfig types:["vite/client"]).

import { describe, expect, it } from 'vitest';
import ts from 'typescript';

const CYRILLIC = /[Ѐ-ӿ]/;
const ALLOW_MARKER = 'i18n-allow';

// Каталоги (на любом уровне пути), где кириллица легитимна и не сканируется:
// lib/locales/{en,ru}.ts — сам каталог переводов; test/ — тесты.
//
// demo/ (фикстуры демо-режима) сюда СОЗНАТЕЛЬНО не входит: он под проверкой
// наравне с остальным кодом. Единственное, что там легитимно по-русски, —
// поле `description_ru` алерта, которое master отдаёт ровно таким; эти строки
// помечены маркером `i18n-allow` поштучно, а не спрятаны целым каталогом.
const SKIP_DIRS = new Set(['locales', 'test']);

// Все исходники src/ как сырой текст (ключи вида '../lib/format.ts').
const RAW = import.meta.glob('../**/*.{ts,tsx}', { query: '?raw', import: 'default', eager: true }) as Record<string, string>;

/** Ключ glob → путь относительно src/ ('../lib/x.ts' → 'lib/x.ts'). */
function relToSrc(globKey: string): string {
  return globKey.replace(/^\.\.\//, '');
}

function isScanned(rel: string): boolean {
  if (/\.test\.tsx?$/.test(rel)) return false;
  return !rel.split('/').some((seg) => SKIP_DIRS.has(seg));
}

/** Узлы, чей текст пользователь может увидеть: литералы и JSX-текст. */
function isLiteralish(node: ts.Node): boolean {
  switch (node.kind) {
    case ts.SyntaxKind.StringLiteral:
    case ts.SyntaxKind.NoSubstitutionTemplateLiteral:
    case ts.SyntaxKind.TemplateHead:
    case ts.SyntaxKind.TemplateMiddle:
    case ts.SyntaxKind.TemplateTail:
    case ts.SyntaxKind.RegularExpressionLiteral:
    case ts.SyntaxKind.JsxText:
      return true;
    default:
      return false;
  }
}

/**
 * Номера строк, где кириллица стоит в ЛИТЕРАЛЕ. Разбор — типовым парсером
 * TypeScript, а не посимвольной машиной.
 *
 * Почему не машина (она тут была до 16.08): машина не знала про регулярки и
 * принимала кавычку ВНУТРИ них — `/server_id="([^"]+)"/` — за начало строки.
 * С этого места она считала литералом всё подряд до следующей кавычки где-то
 * ниже по файлу, и каждый русский КОММЕНТАРИЙ в этом промежутке становился
 * «нарушением». Разбор такого класса ошибок не делает: что литерал, а что
 * комментарий, решает грамматика языка.
 *
 * Комментарии литералами не являются и не проверяются — русские пояснения в
 * коде здесь норма (см. шапку файла).
 */
function findCyrillicLiterals(rel: string, raw: string): number[] {
  const src = ts.createSourceFile(rel, raw, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const lines = new Set<number>();
  const visit = (node: ts.Node): void => {
    if (isLiteralish(node)) {
      const start = node.getStart(src);
      const idx = raw.slice(start, node.getEnd()).search(CYRILLIC);
      // Указываем строку САМОЙ кириллицы, а не начало узла: в многострочном
      // шаблоне это разные строки, и сообщение должно вести туда, где текст.
      if (idx >= 0) lines.add(src.getLineAndCharacterOfPosition(start + idx).line + 1);
    }
    ts.forEachChild(node, visit);
  };
  visit(src);
  return [...lines].sort((a, b) => a - b);
}

// [путь относительно src/, сырой текст] для сканируемых файлов.
const scanned: [string, string][] = Object.entries(RAW)
  .map(([key, content]) => [relToSrc(key), content] as [string, string])
  .filter(([rel]) => isScanned(rel))
  .sort((a, b) => a[0].localeCompare(b[0]));
const files = scanned.map(([rel]) => rel);

/**
 * Атрибуты, чьё значение ВИДИТ или СЛЫШИТ пользователь. Список закрытый и
 * короткий нарочно: проверка обязана быть точной, а не широкой — расширять его
 * дешевле, чем разбирать ложные срабатывания и в итоге отключить проверку.
 */
const USER_FACING_ATTRS = new Set(['title', 'aria-label', 'placeholder', 'alt', 'aria-description']);

/**
 * Похоже ли на ПРОЗУ, а не на токен. Минимум два слова по две буквы: так
 * отсеиваются моно-разделители («—», «·»), односложные технические значения
 * («img», «none») и идентификаторы, но остаётся любая настоящая фраза.
 *
 * Порог осознанно занижает чувствительность: пропустить однословную подпись
 * дешевле, чем приучить читать этот гвард как источник ложных находок.
 */
function looksLikeProse(text: string): boolean {
  const words = text.trim().split(/\s+/).filter((w) => /[A-Za-z]{2}/.test(w));
  return words.length >= 2;
}

/** Захардкоженный user-facing текст в JSX: [строка, что нашли]. */
function findJsxProse(rel: string, raw: string): { line: number; what: string }[] {
  const src = ts.createSourceFile(rel, raw, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const found: { line: number; what: string }[] = [];
  const lineOf = (pos: number) => src.getLineAndCharacterOfPosition(pos).line + 1;

  const visit = (node: ts.Node): void => {
    // (1) Текст между тегами: `<span>Could not reach master</span>`. Он
    // рендерится ПО ОПРЕДЕЛЕНИЮ — догадываться о назначении не нужно.
    if (ts.isJsxText(node) && looksLikeProse(node.text)) {
      found.push({ line: lineOf(node.getStart(src)), what: node.text.trim() });
    }
    // (2) Атрибут из белого списка со СТРОКОВЫМ литералом. Значение в фигурных
    // скобках (`title={t('…')}`) сюда не попадает — это уже не литерал.
    if (ts.isJsxAttribute(node) && node.initializer !== undefined && ts.isStringLiteral(node.initializer)) {
      const name = node.name.getText(src);
      if (USER_FACING_ATTRS.has(name) && looksLikeProse(node.initializer.text)) {
        found.push({ line: lineOf(node.getStart(src)), what: `${name}="${node.initializer.text}"` });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(src);
  return found;
}

describe('нет захардкоженных user-facing строк (guard регрессий i18n)', () => {
  it('нашёл файлы для скана (санити — обход не пустой)', () => {
    expect(files.length).toBeGreaterThan(10);
    expect(files).toContain('App.tsx');
    expect(files).toContain('components/Badge.tsx');
    expect(files).toContain('screens/Events.tsx');
  });

  it('findCyrillicLiterals видит литералы, не трогает комментарии и не путается в регулярках (санити)', () => {
    // Строки пронумерованы вручную: 2–3 — блочный комментарий, 4 — нарушение.
    const probe = [
      '// это комментарий', // 1
      '/* блок', // 2
      '   коммент */', // 3
      "const s = 'привет';", // 4  ← единственное нарушение
      'const u = "http://x"; // ok', // 5
      'const re = /server_id="([^"]+)"/;', // 6
      '// комментарий ПОСЛЕ регулярки с кавычками', // 7
      'const t = `demo: строка ${x}`;', // 8  ← и это тоже
    ].join('\n');
    expect(findCyrillicLiterals('probe.ts', probe)).toEqual([4, 8]);
  });

  it('нарушение указывает ту строку, где кириллица, даже в многострочном шаблоне (санити)', () => {
    const probe = ['const t = `', 'ok', 'нельзя', '`;'].join('\n');
    expect(findCyrillicLiterals('probe.ts', probe)).toEqual([3]);
  });

  it('в исходниках panel/src нет кириллических литералов вне locales/format', () => {
    const violations: string[] = [];
    for (const [rel, raw] of scanned) {
      const rawLines = raw.split('\n');
      for (const line of findCyrillicLiterals(rel, raw)) {
        if (rawLines[line - 1]?.includes(ALLOW_MARKER)) continue;
        violations.push(`${rel}:${String(line)}: ${rawLines[line - 1]?.trim() ?? ''}`);
      }
    }
    expect(
      violations,
      `Захардкоженная кириллица — заведите ключ в locales/en.ts + ru.ts и берите через t():\n${violations.join('\n')}`,
    ).toEqual([]);
  });

  // --- (B) английские user-facing строки в JSX (tracker #1012) ---

  it('findJsxProse ловит текст узла и user-facing атрибут, но не трогает служебное (санити)', () => {
    const bad = findJsxProse(
      'probe.tsx',
      'export const A = () => (\n  <div aria-label="Retry loading the fleet" title="Something went wrong">\n    Could not reach the master node.\n  </div>\n);\n',
    );
    expect(bad.map((v) => v.what).sort()).toEqual([
      'Could not reach the master node.',
      'aria-label="Retry loading the fleet"',
      'title="Something went wrong"',
    ]);

    // А вот чего гвард обязан НЕ ловить, иначе его отключат первым же PR:
    const ok = findJsxProse(
      'probe.tsx',
      [
        'export const B = () => (',
        '  <div className="flex items-center gap-2" data-testid="node-row" role="img">',
        '    <span title={t(\'col.node\')} aria-label={t(\'metric.aria\')}>{name}</span>',
        '    <a href="https://example.com/docs/deploy">{t(\'nav.docs\')}</a>',
        '    <code>{\'sum(rate(birdman_matches_total[5m]))\'}</code>',
        '    <span>—</span>',
        '  </div>',
        ');',
      ].join('\n'),
    );
    expect(ok).toEqual([]);
  });

  it('в исходниках panel/src нет захардкоженного английского в JSX-тексте и user-facing атрибутах', () => {
    const violations: string[] = [];
    for (const [rel, raw] of scanned) {
      if (!rel.endsWith('.tsx')) continue;
      const rawLines = raw.split('\n');
      for (const hit of findJsxProse(rel, raw)) {
        if (rawLines[hit.line - 1]?.includes(ALLOW_MARKER)) continue;
        violations.push(`${rel}:${String(hit.line)}: ${hit.what}`);
      }
    }
    expect(
      violations,
      `Захардкоженный user-facing текст — заведите ключ в locales/en.ts + ru.ts и берите через t():\n${violations.join('\n')}`,
    ).toEqual([]);
  });
});
