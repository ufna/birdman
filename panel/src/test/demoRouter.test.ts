// Демо-роутер (src/demo/router.ts) — единственная сетевая правда демо-режима.
// Тест держит два обещания: известная ручка отвечает 200 разбираемым телом, а
// НЕизвестная кричит. Молчаливая пустая карточка на скриншоте хуже красной
// ошибки при съёмке.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { demoFetch, installDemoFetch } from '../demo/router';
import { api } from '../lib/api';

async function get(path: string): Promise<{ status: number; body: unknown }> {
  const res = await demoFetch(path);
  const text = await res.text();
  return { status: res.status, body: text === '' ? undefined : JSON.parse(text) };
}

describe('demoFetch', () => {
  it('отдаёт admin-сессию: экран логина в демо не показывается', async () => {
    const { status, body } = await get('/v1/session');
    expect(status).toBe(200);
    expect(body).toMatchObject({ name: expect.any(String), scopes: expect.arrayContaining(['admin']) });
  });

  it('неизвестная ручка — 500 и громкий console.error', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { status } = await get('/v1/nope');
    expect(status).toBe(500);
    expect(spy).toHaveBeenCalledWith(expect.stringContaining('/v1/nope'));
    spy.mockRestore();
  });
});

describe('роутер обслуживает весь публичный REST панели', () => {
  beforeEach(() => {
    installDemoFetch();
  });

  it('списки не пусты и разбираются клиентом панели', async () => {
    expect((await api.listNodes({ project: 'nova-arena' })).length).toBe(12);
    expect((await api.listServers({ project: 'nova-arena' })).length).toBe(80);
    expect((await api.listVersions({ project: 'nova-arena' })).length).toBe(5);
    expect((await api.listMatches({ project: 'nova-arena', limit: 1000 })).length).toBeGreaterThan(140);
    expect((await api.listEvents(50)).length).toBe(50);
    expect((await api.listProjects()).length).toBe(2);
    expect((await api.listEnvironments('nova-arena')).length).toBe(2);
  });

  it('сужение по проекту и окружению работает серверно', async () => {
    expect((await api.listNodes({ project: 'dune-raiders' })).length).toBe(0);
    expect((await api.listVersions({ project: 'nova-arena', env: 'dev' })).length).toBe(1);
  });

  it('админские ручки отвечают формой, которую ждут экраны', async () => {
    expect((await api.alertsActive()).length).toBe(1);
    expect((await api.alertHistory({ limit: 100 })).length).toBeGreaterThan(1);
    expect((await api.listApiKeys()).some((k) => k.project === 'nova-arena' && k.env === 'prod')).toBe(true);
    expect((await api.listRegistries()).length).toBe(1);
    expect((await api.getBackupSettings()).s3_bucket).toBe('birdman-backups-demo');
    expect((await api.listBackupRuns(20)).some((r) => r.result === 'error')).toBe(true);
  });

  it('статистика отдаёт зерофилленные ряды за запрошенный период', async () => {
    const ov = await api.statsOverview(7);
    expect(ov.days).toBe(7);
    expect(ov.matches_per_day.points.length).toBe(7);
    expect(ov.version_distribution.reduce((a, v) => a + v.share, 0)).toBeCloseTo(1, 2);
    const cost = await api.statsCost(7);
    expect(cost.utilization.map((u) => u.region).sort()).toEqual(['ap-southeast', 'eu-central', 'us-east']);
  });
});
