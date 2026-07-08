import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import {
  StateBadge,
  toneOfEventKind,
  toneOfMatchState,
  toneOfNodeState,
  toneOfServerState,
} from '../components/Badge';

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
});
