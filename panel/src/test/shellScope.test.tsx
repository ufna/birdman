// Скоуп (проект + окружение) переехал из контента в сайдбар (слайс A).
// Проверяем контракт переезда, а не вёрстку: на экранах со скоупом блок есть,
// на экране без него — нет (иначе селектор врал бы, что на что-то влияет), а
// мобильный индикатор показывает текущий выбор, потому что на узком экране
// сам переключатель уехал в меню.

import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { Environment, ProjectInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { EnvContext } from '../lib/env';
import { ProjectContext } from '../lib/project';
import { ScopeIndicator, ScopePicker, pathUsesScope } from '../components/Shell';

const dev: Environment = {
  project: 'game',
  name: 'dev',
  production: false,
  auto_deploy: true,
  retention_keep: 20,
  created_at: '2026-07-01T00:00:00Z',
};

const projects: ProjectInfo[] = [
  { id: 'p1', slug: 'game', match_size: 2, created_at: '2026-07-01T00:00:00Z' },
  { id: 'p2', slug: 'other', match_size: 4, created_at: '2026-07-01T00:00:00Z' },
];

function withScope(node: React.ReactNode, env: string | null = 'dev') {
  const envValue = {
    environments: [dev],
    project: 'game',
    selected: env,
    setSelected: () => {},
    loading: false,
    reload: () => {},
  };
  const projectValue = {
    projects,
    selected: 'game',
    setSelected: () => {},
    loading: false,
    reload: () => {},
  };
  return render(
    <I18nProvider initialLang="en">
      <ProjectContext.Provider value={projectValue}>
        <EnvContext.Provider value={envValue}>{node}</EnvContext.Provider>
      </ProjectContext.Provider>
    </I18nProvider>,
  );
}

describe('скоуп панели: место и видимость', () => {
  it('pathUsesScope: Бекапы — единственный экран без скоупа', () => {
    expect(pathUsesScope('/backups')).toBe(false);
    // В Доступе скоуп НУЖЕН: к проекту и окружению привязываются API-ключи.
    expect(pathUsesScope('/access')).toBe(true);
    expect(pathUsesScope('/')).toBe(true);
    expect(pathUsesScope('/fleet')).toBe(true);
  });

  it('на экране со скоупом рендерятся оба переключателя', () => {
    withScope(<ScopePicker path="/fleet" />);
    expect(screen.getByRole('group', { name: /project/i })).toBeTruthy();
    expect(screen.getByRole('group', { name: /environment/i })).toBeTruthy();
  });

  it('на Бекапах блока нет вовсе', () => {
    withScope(<ScopePicker path="/backups" />);
    expect(screen.queryByRole('group', { name: /project/i })).toBeNull();
    expect(screen.queryByRole('group', { name: /environment/i })).toBeNull();
  });

  it('мобильный индикатор показывает проект и окружение', () => {
    const { container } = withScope(<ScopeIndicator path="/fleet" />);
    expect(container.textContent).toContain('game');
    expect(container.textContent).toContain('dev');
  });

  it('мобильный индикатор при «всех окружениях» показывает All, а не пустоту', () => {
    const { container } = withScope(<ScopeIndicator path="/fleet" />, null);
    expect(container.textContent).toContain('All');
  });

  it('мобильного индикатора нет на экране без скоупа', () => {
    const { container } = withScope(<ScopeIndicator path="/backups" />);
    expect(container.textContent).toBe('');
  });
});
