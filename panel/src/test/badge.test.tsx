import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import { I18nProvider } from '../lib/i18n';
import {
  EVENT_KINDS,
  StateBadge,
  toneOfEventKind,
  toneOfMatchState,
  toneOfNodeState,
  toneOfServerState,
} from '../components/Badge';

const en = (ui: ReactElement) => render(<I18nProvider initialLang="en">{ui}</I18nProvider>);

describe('маппинг состояний в тона', () => {
  it('нода', () => {
    expect(toneOfNodeState('active')).toBe('good');
    expect(toneOfNodeState('draining')).toBe('warn');
    expect(toneOfNodeState('quarantine')).toBe('dead');
    expect(toneOfNodeState('dead')).toBe('neutral');
    expect(toneOfNodeState('???')).toBe('neutral');
  });
  it('дедик', () => {
    expect(toneOfServerState('ready')).toBe('good');
    expect(toneOfServerState('allocated')).toBe('accent');
    expect(toneOfServerState('creating')).toBe('warn');
    expect(toneOfServerState('failed')).toBe('dead');
    expect(toneOfServerState('reaped')).toBe('neutral');
  });
  it('матч', () => {
    expect(toneOfMatchState('running')).toBe('good');
    expect(toneOfMatchState('pending')).toBe('warn');
    expect(toneOfMatchState('finished')).toBe('neutral');
    expect(toneOfMatchState('aborted')).toBe('dead');
  });
  it('события', () => {
    expect(toneOfEventKind('crash_loop')).toBe('dead');
    expect(toneOfEventKind('allocation_failed')).toBe('warn');
    expect(toneOfEventKind('node_recovered')).toBe('good');
    expect(toneOfEventKind('fleet_updated')).toBe('accent');
    expect(toneOfEventKind('something_new')).toBe('neutral');
  });
});

describe('StateBadge', () => {
  it('показывает метку состояния и тон', () => {
    render(<StateBadge state="quarantine" tone="dead" />);
    const badge = screen.getByText('quarantine');
    expect(badge.dataset.tone).toBe('dead');
    expect(badge.className).toContain('text-dead');
  });
  it('у нейтрального тона — рамка вместо заливки', () => {
    render(<StateBadge state="reaped" tone="neutral" />);
    expect(screen.getByText('reaped').className).toContain('border-line');
  });
  it('domain=event: переведённая подпись + сырой код kind в title', () => {
    en(<StateBadge state="crash_loop" tone="dead" domain="event" />);
    const badge = screen.getByText('Crash loop');
    expect(badge.getAttribute('title')).toBe('crash_loop');
  });
  it('domain с неизвестным кодом → фолбэк на сам код', () => {
    en(<StateBadge state="totally_unknown_kind" tone="neutral" domain="event" />);
    expect(screen.getByText('totally_unknown_kind')).toBeTruthy();
  });
});

describe('EVENT_KINDS', () => {
  it('совпадает с набором тонов и без дублей', () => {
    expect(new Set(EVENT_KINDS).size).toBe(EVENT_KINDS.length);
    // Каждый вид имеет осмысленный тон (не падаем на неизвестном).
    for (const k of EVENT_KINDS) expect(typeof toneOfEventKind(k)).toBe('string');
  });
});
