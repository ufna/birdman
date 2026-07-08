import { afterEach, describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { en } from '../lib/locales/en';
import { ru } from '../lib/locales/ru';
import { EVENT_KINDS } from '../components/Badge';
import { I18nProvider, LANG_STORAGE_KEY, detectLang, resolveInitialLang, storedLang, useT } from '../lib/i18n';

/** Множество {placeholder}-токенов в шаблоне ('{a} {b}' → {a, b}). */
function placeholders(tpl: string): Set<string> {
  return new Set([...tpl.matchAll(/\{(\w+)\}/g)].map((m) => m[1]));
}

describe('detectLang', () => {
  it('ru-локаль → ru', () => {
    expect(detectLang({ language: 'ru-RU', languages: ['ru-RU'] })).toBe('ru');
    expect(detectLang({ language: 'ru', languages: [] as unknown as readonly string[] })).toBe('ru');
  });
  it('en-локаль → en', () => {
    expect(detectLang({ language: 'en-US', languages: ['en-US'] })).toBe('en');
  });
  it('незнакомая локаль → en (дефолт)', () => {
    expect(detectLang({ language: 'fr-FR', languages: ['fr-FR', 'de'] })).toBe('en');
    expect(detectLang({ language: '', languages: [] })).toBe('en');
  });
  it('уважает порядок предпочтений', () => {
    expect(detectLang({ language: 'en', languages: ['en-GB', 'ru-RU'] })).toBe('en');
    expect(detectLang({ language: 'ru', languages: ['ru-RU', 'en-US'] })).toBe('ru');
    expect(detectLang({ language: 'fr', languages: ['fr', 'ru', 'en'] })).toBe('ru');
  });
});

describe('resolveInitialLang / storedLang', () => {
  afterEach(() => {
    localStorage.clear();
  });
  it('явный выбор в localStorage перебивает автоопределение', () => {
    localStorage.setItem(LANG_STORAGE_KEY, 'ru');
    expect(storedLang()).toBe('ru');
    expect(resolveInitialLang()).toBe('ru');
    localStorage.setItem(LANG_STORAGE_KEY, 'en');
    expect(resolveInitialLang()).toBe('en');
  });
  it('без записи — авто (в jsdom navigator=en → en по умолчанию)', () => {
    expect(storedLang()).toBeNull();
    expect(resolveInitialLang()).toBe('en');
  });
  it('мусор в localStorage игнорируется', () => {
    localStorage.setItem(LANG_STORAGE_KEY, 'de');
    expect(storedLang()).toBeNull();
  });
});

describe('каталоги en/ru — паритет ключей (нет висячих)', () => {
  it('каждый ключ из en есть в ru и наоборот', () => {
    const enKeys = Object.keys(en).sort();
    const ruKeys = Object.keys(ru).sort();
    const missingInRu = enKeys.filter((k) => !(k in ru));
    const extraInRu = ruKeys.filter((k) => !(k in en));
    expect(missingInRu).toEqual([]);
    expect(extraInRu).toEqual([]);
    expect(ruKeys).toEqual(enKeys);
  });
  it('нет пустых переводов (в т.ч. из одних пробелов)', () => {
    for (const [k, v] of Object.entries(ru)) expect(v.trim(), `ru[${k}]`).not.toBe('');
    for (const [k, v] of Object.entries(en)) expect(v.trim(), `en[${k}]`).not.toBe('');
  });
  it('плейсхолдеры {x} совпадают между en и ru (перевод не теряет параметры)', () => {
    const mismatched = Object.keys(en).filter((k) => {
      const a = placeholders(en[k as keyof typeof en]);
      const b = placeholders((ru as Record<string, string>)[k]);
      return a.size !== b.size || [...a].some((p) => !b.has(p));
    });
    expect(mismatched, `ключи с рассинхроном плейсхолдеров: ${mismatched.join(', ')}`).toEqual([]);
  });
  it('у каждого вида события есть подпись event.<kind> в обоих каталогах', () => {
    const missing: string[] = [];
    for (const kind of EVENT_KINDS) {
      const key = `event.${kind}`;
      if (!(key in en)) missing.push(`en:${key}`);
      if (!(key in ru)) missing.push(`ru:${key}`);
    }
    expect(missing, `нет подписей для видов событий: ${missing.join(', ')}`).toEqual([]);
  });
});

// Пробник: даёт добраться до t()/tp() внутри провайдера.
function Probe() {
  const { t, tp } = useT();
  return (
    <div>
      <span data-testid="interp">{t('app.noScope', { name: 'boot' })}</span>
      <span data-testid="p1">{tp('fleet.nodesCount', 1)}</span>
      <span data-testid="p2">{tp('fleet.nodesCount', 2)}</span>
      <span data-testid="p5">{tp('fleet.nodesCount', 5)}</span>
    </div>
  );
}

describe('t / tp через провайдер', () => {
  it('en: интерполяция и англ. плюрализация (one/other)', () => {
    render(
      <I18nProvider initialLang="en">
        <Probe />
      </I18nProvider>,
    );
    expect(screen.getByTestId('interp').textContent).toContain('boot');
    expect(screen.getByTestId('p1').textContent).toBe('1 node');
    expect(screen.getByTestId('p2').textContent).toBe('2 nodes');
    expect(screen.getByTestId('p5').textContent).toBe('5 nodes');
  });
  it('ru: русская плюрализация one/few/many', () => {
    render(
      <I18nProvider initialLang="ru">
        <Probe />
      </I18nProvider>,
    );
    expect(screen.getByTestId('p1').textContent).toBe('1 тачка');
    expect(screen.getByTestId('p2').textContent).toBe('2 тачки');
    expect(screen.getByTestId('p5').textContent).toBe('5 тачек');
  });
});
