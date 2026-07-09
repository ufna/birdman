import { describe, expect, it } from 'vitest';
import { escapeStreamValue, fleetSearchQuery, serverHistoryQuery } from '../lib/logsql';
import type { FleetFilter } from '../lib/logsql';

describe('escapeStreamValue', () => {
  it('экранирует обратный слэш и двойную кавычку (в этом порядке)', () => {
    expect(escapeStreamValue('a"b\\c')).toBe('a\\"b\\\\c');
  });
  it('обычный текст не меняет', () => {
    expect(escapeStreamValue('srv-1')).toBe('srv-1');
  });
});

describe('serverHistoryQuery', () => {
  it('только server_id → стрим-фильтр + суффикс сортировки', () => {
    expect(serverHistoryQuery('srv-1')).toBe('{server_id="srv-1"} | sort by (_time) desc');
  });
  it('server_id + текст → фильтр + фраза в кавычках + суффикс сортировки', () => {
    expect(serverHistoryQuery('srv-1', 'boom')).toBe('{server_id="srv-1"} "boom" | sort by (_time) desc');
  });
  it('экранирует и server_id, и текст', () => {
    expect(serverHistoryQuery('a"b\\c', 'x"y')).toBe('{server_id="a\\"b\\\\c"} "x\\"y" | sort by (_time) desc');
  });
  it('пустой/из пробелов текст — как будто текста нет', () => {
    expect(serverHistoryQuery('srv-1', '   ')).toBe('{server_id="srv-1"} | sort by (_time) desc');
    expect(serverHistoryQuery('srv-1', '')).toBe('{server_id="srv-1"} | sort by (_time) desc');
  });
});

describe('fleetSearchQuery', () => {
  it('совсем пусто → `*` + суффикс сортировки', () => {
    expect(fleetSearchQuery({})).toBe('* | sort by (_time) desc');
  });
  it('только текст → фраза в кавычках + суффикс сортировки', () => {
    expect(fleetSearchQuery({ text: 'boom' })).toBe('"boom" | sort by (_time) desc');
  });
  it('все стрим-поля → объединённый фильтр, без текста', () => {
    const f: FleetFilter = { region: 'eu', node: 'n1', serverId: 'srv-1' };
    expect(fleetSearchQuery(f)).toBe('{region="eu",node="n1",server_id="srv-1"} | sort by (_time) desc');
  });
  it('часть стрим-полей + текст → фильтр, затем фраза', () => {
    expect(fleetSearchQuery({ region: 'eu', text: 'boom' })).toBe('{region="eu"} "boom" | sort by (_time) desc');
  });
  it('экранирует значения полей и текст', () => {
    expect(fleetSearchQuery({ region: 'a"b', text: 'x\\y' })).toBe('{region="a\\"b"} "x\\\\y" | sort by (_time) desc');
  });
  it('пустые/из пробелов поля игнорируются, как незаданные', () => {
    expect(fleetSearchQuery({ region: '', node: '  ', text: '' })).toBe('* | sort by (_time) desc');
  });
});
