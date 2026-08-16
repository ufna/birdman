// Инварианты демо-флота. Экраны считают производные (игроков онлайн — суммой
// по allocated, версию флота — модой по региону), поэтому проверяем не «поля
// на месте», а ЧИСЛА, которые увидит зритель скриншота.
import { describe, expect, it } from 'vitest';
import { buildFleet, rng } from '../demo/fleet';

const NOW = Date.UTC(2026, 7, 16, 19, 40, 0);

describe('rng', () => {
  it('детерминирован при одном семени', () => {
    const a = rng(42),
      b = rng(42);
    expect([a(), a(), a()]).toEqual([b(), b(), b()]);
  });
});

describe('buildFleet', () => {
  const f = buildFleet(NOW);

  it('детерминирован при одном now', () => {
    expect(JSON.stringify(buildFleet(NOW))).toEqual(JSON.stringify(f));
  });

  it('оба проекта видны в селекторе, флот только у первого', () => {
    expect(f.projects.map((p) => p.slug)).toEqual(['nova-arena', 'dune-raiders']);
    expect(f.nodes.every((n) => n.project === 'nova-arena')).toBe(true);
  });

  it('состояния дедиков дают тёплый буфер и слив', () => {
    const by = (s: string) => f.servers.filter((x) => x.state === s).length;
    expect([by('allocated'), by('ready'), by('draining'), by('creating')]).toEqual([48, 24, 6, 2]);
  });

  it('игроков онлайн — как их посчитает Обзор (сумма по allocated)', () => {
    const online = f.servers.filter((s) => s.state === 'allocated').reduce((a, s) => a + s.players, 0);
    expect(online).toBeGreaterThan(500);
    expect(online).toBeLessThan(700);
    expect(f.servers.every((s) => s.state === 'allocated' || s.players === 0)).toBe(true);
  });

  it('мультиверсионность: две active-версии живут одновременно, третья сливается', () => {
    const live = f.servers.filter((s) => s.state !== 'reaped' && s.state !== 'failed');
    const semverOf = new Map(f.versions.map((v) => [v.id, v.semver]));
    const count = (sv: string) => live.filter((s) => semverOf.get(s.version_id) === sv).length;
    expect(count('1.15.0')).toBe(14);
    expect(count('1.14.3')).toBe(60);
    expect(count('1.14.2')).toBe(6);
    expect(f.versions.find((v) => v.semver === '1.14.2')?.state).toBe('deprecated');
  });

  it('ссылки замкнуты: у каждого дедика есть нода и версия, у матча — дедик', () => {
    const nodeIds = new Set(f.nodes.map((n) => n.id));
    const verIds = new Set(f.versions.map((v) => v.id));
    const srvIds = new Set(f.servers.map((s) => s.id));
    expect(f.servers.every((s) => nodeIds.has(s.node_id) && verIds.has(s.version_id))).toBe(true);
    expect(f.matches.every((m) => srvIds.has(m.server_id) && verIds.has(m.version_id))).toBe(true);
  });

  it('матчи за час наполняют спарклайн Обзора', () => {
    const hour = f.matches.filter((m) => NOW - new Date(m.created_at).getTime() < 3_600_000);
    expect(hour.length).toBeGreaterThan(100);
    expect(f.matches.filter((m) => m.state === 'running').length).toBe(48);
  });

  it('лента событий свежая и убывает по id', () => {
    expect(f.events.length).toBe(60);
    const ids = f.events.map((e) => e.id);
    expect([...ids].sort((a, b) => b - a)).toEqual(ids);
    expect(NOW - new Date(f.events[0].ts).getTime()).toBeLessThan(5 * 60_000);
  });

  it('ни одного маршрутизируемого IP — только TEST-NET-3', () => {
    expect(f.nodes.every((n) => n.public_ip.startsWith('203.0.113.'))).toBe(true);
  });
});
