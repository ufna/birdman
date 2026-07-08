import { describe, expect, it } from 'vitest';
import { createSSEParser } from '../lib/sse';

describe('createSSEParser', () => {
  it('собирает фреймы из чанков, порезанных посреди строк', () => {
    const parse = createSSEParser();
    expect(parse('retry: 3000\n\nid: 4\nev')).toEqual([]);
    const frames = parse('ent: node_created\ndata: {"id":4,"kind":"node_created"}\n\n');
    expect(frames).toEqual([{ id: 4, kind: 'node_created', data: '{"id":4,"kind":"node_created"}' }]);
  });

  it('игнорирует keepalive-комментарии', () => {
    const parse = createSSEParser();
    expect(parse(': keepalive\n\n: keepalive\n\n')).toEqual([]);
  });

  it('несколько фреймов в одном чанке, в порядке прихода', () => {
    const parse = createSSEParser();
    const frames = parse(
      'id: 1\nevent: a\ndata: {"n":1}\n\nid: 2\nevent: b\ndata: {"n":2}\n\n',
    );
    expect(frames.map((f) => f.id)).toEqual([1, 2]);
    expect(frames.map((f) => f.kind)).toEqual(['a', 'b']);
  });

  it('многострочный data склеивается через \\n', () => {
    const parse = createSSEParser();
    const frames = parse('id: 7\nevent: x\ndata: line1\ndata: line2\n\n');
    expect(frames[0].data).toBe('line1\nline2');
  });

  it('фрейм без полей не эмитится', () => {
    const parse = createSSEParser();
    expect(parse('\n\n')).toEqual([]);
  });
});
