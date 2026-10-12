import { expect, test } from '@playwright/test';
import { exactTime } from '../src/lib/format';
import { viewFromHash } from '../src/lib/navigation';

test.describe('viewFromHash', () => {
  test('maps the empty hash to the overview and known views to themselves', () => {
    expect(viewFromHash('')).toBe('overview');
    expect(viewFromHash('#')).toBe('overview');
    for (const id of ['overview', 'queue', 'proposals', 'prs', 'settings'])
      expect(viewFromHash('#' + id)).toBe(id);
  });
  test('ignores anchors that are not views', () => {
    for (const hash of ['#main-content', '#QUEUE', '#queue/x'])
      expect(viewFromHash(hash)).toBeNull();
  });
});

test.describe('exactTime', () => {
  test('returns no title for missing or invalid times', () => {
    expect(exactTime('')).toBe('');
    expect(exactTime('not a time')).toBe('');
  });
  test('renders the exact local date and time for a valid instant', () => {
    const title = exactTime('2026-10-12T15:04:05Z');
    expect(title).not.toBe('');
    expect(title).toContain('2026');
  });
});
