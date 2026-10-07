import { expect, test } from '@playwright/test';
import { CycleHistory, type CycleHistoryState } from '../src/lib/cycleHistory';
import type { CycleSummary, Page } from '../src/lib/types';

function cycle(number: number): CycleSummary {
  return {
    id: `cycle-${number}`,
    number,
    mode: 'execution',
    status: 'completed',
    started_at: '2026-09-10T00:00:00Z',
    completed_at: '2026-09-10T00:01:00Z',
    error: null,
    session_count: 0,
    decisions: {},
    lifecycle: {}
  };
}

function page(numbers: number[], cursor: number | null): Page<CycleSummary> {
  return { items: numbers.map(cycle), next_cursor: cursor, counts: {} };
}

function fixture(read: (path: string, signal: AbortSignal) => Promise<Page<CycleSummary>>) {
  let state: CycleHistoryState = { rows: [], cursor: null, loading: false, error: '' };
  const history = new CycleHistory((next) => (state = next), read);
  return { history, state: () => state };
}

test('history polls coalesce and canceled responses cannot overwrite the next visit', async () => {
  const requests: { signal: AbortSignal; resolve: (value: Page<CycleSummary>) => void }[] = [];
  const { history, state } = fixture(
    (_, signal) => new Promise((resolve) => requests.push({ signal, resolve }))
  );
  const first = history.refresh();
  for (let poll = 0; poll < 20; poll++) expect(history.refresh()).toBe(first);
  expect(requests).toHaveLength(1);
  history.cancel();
  expect(requests[0].signal.aborted).toBe(true);
  expect(state().loading).toBe(false);
  const next = history.refresh();
  requests[0].resolve(page([1], null));
  expect(await first).toBe(false);
  expect(state().rows).toEqual([]);
  expect(state().loading).toBe(true);
  requests[1].resolve(page([2, 1], null));
  expect(await next).toBe(true);
  expect(state().rows.map((row) => row.number)).toEqual([2, 1]);
  expect(state().loading).toBe(false);
  expect(state().error).toBe('');
});

test('polling keeps older pages and refreshes only the newest page and selected cycle', async () => {
  const reads: string[] = [];
  let head = page([6, 5, 4], 700);
  const { history, state } = fixture(async (path) => {
    reads.push(path);
    const query = new URL(path, 'http://fixture').searchParams;
    if (query.get('cycle') === 'cycle-2') {
      const result = page([2], null);
      result.items[0].lifecycle.archived_at = '2026-09-10T00:02:00Z';
      return result;
    }
    if (query.get('before') === '700') return page([3, 2, 1], null);
    return head;
  });
  await history.refresh();
  await history.older();
  head = page([7, 6, 5], 800);
  reads.length = 0;
  expect(await history.refresh('cycle-2')).toBe(true);
  expect(reads).toEqual(['/cycles?limit=100', '/cycles?limit=1&cycle=cycle-2']);
  expect(state().rows.map((row) => row.number)).toEqual([7, 6, 5, 4, 3, 2, 1]);
  expect(state().cursor).toBeNull();
  expect(state().rows.find((row) => row.number === 2)?.lifecycle.archived_at).toBeTruthy();
  reads.length = 0;
  await history.refresh('cycle-7');
  expect(reads).toEqual(['/cycles?limit=100']);
});

test('multiple new-history gaps are filled without dropping cached rows or skipping a cursor', async () => {
  const reads: (string | null)[] = [];
  let head = page([8, 7, 6], 600);
  const older = new Map([
    ['1800', page([17, 16, 15], 1500)],
    ['1500', page([14, 13, 12], 1200)],
    ['1200', page([11, 10, 9], 900)],
    ['900', page([8, 7, 6], 600)],
    ['600', page([5, 4, 3], 300)],
    ['300', page([2, 1], null)]
  ]);
  const { history, state } = fixture(async (path) => {
    const before = new URL(path, 'http://fixture').searchParams.get('before');
    reads.push(before);
    return before === null ? head : older.get(before)!;
  });
  await history.refresh();
  await history.older();
  head = page([14, 13, 12], 1200);
  await history.refresh();
  head = page([20, 19, 18], 1800);
  await history.refresh();
  expect(state().rows.map((row) => row.number)).toEqual([20, 19, 18, 14, 13, 12, 8, 7, 6, 5, 4, 3]);
  reads.length = 0;
  for (let i = 0; i < 5; i++) expect(await history.older()).toBe(true);
  expect(reads).toEqual(['1800', '1500', '1200', '900', '300']);
  expect(state().rows.map((row) => row.number)).toEqual(
    Array.from({ length: 20 }, (_, i) => 20 - i)
  );
  expect(state().cursor).toBeNull();
  expect(await history.older()).toBe(false);
});

test('failed history refresh preserves loaded rows and has independent retry state', async () => {
  let failing = false;
  const { history, state } = fixture(async () => {
    if (failing) throw new Error('Synthetic history outage');
    return page([2, 1], null);
  });
  await history.refresh();
  failing = true;
  expect(await history.refresh()).toBe(false);
  expect(state().rows.map((row) => row.number)).toEqual([2, 1]);
  expect(state().error).toBe('Synthetic history outage');
  expect(state().loading).toBe(false);
  failing = false;
  expect(await history.refresh()).toBe(true);
  expect(state().error).toBe('');
});

test('an uncached selected cycle is visible without skipping intervening history', async () => {
  const reads: string[] = [];
  const { history, state } = fixture(async (path) => {
    reads.push(path);
    const query = new URL(path, 'http://fixture').searchParams;
    if (query.get('cycle') === 'cycle-2') {
      const selected = page([2], null);
      selected.items[0].lifecycle.archived_at = '2026-09-10T00:02:00Z';
      return selected;
    }
    if (query.get('before') === '700') return page([6, 5, 4], 400);
    if (query.get('before') === '400') {
      const older = page([3, 2, 1], null);
      older.items[1].lifecycle.discarded_at = '2026-09-10T00:03:00Z';
      return older;
    }
    return page([9, 8, 7], 700);
  });
  expect(await history.refresh('cycle-2')).toBe(true);
  expect(state().rows.map((row) => row.number)).toEqual([9, 8, 7, 2]);
  expect(state().rows.at(-1)?.lifecycle.archived_at).toBeTruthy();
  expect(state().cursor).toBe(700);
  await history.older();
  expect(state().rows.map((row) => row.number)).toEqual([9, 8, 7, 6, 5, 4, 2]);
  expect(state().cursor).toBe(400);
  await history.older();
  expect(reads).toEqual([
    '/cycles?limit=100',
    '/cycles?limit=1&cycle=cycle-2',
    '/cycles?limit=100&before=700',
    '/cycles?limit=100&before=400'
  ]);
  expect(state().rows.map((row) => row.number)).toEqual([9, 8, 7, 6, 5, 4, 3, 2, 1]);
  expect(state().rows.filter((row) => row.number === 2)).toHaveLength(1);
  expect(state().rows.find((row) => row.number === 2)?.lifecycle).toEqual({
    discarded_at: '2026-09-10T00:03:00Z'
  });
  expect(state().cursor).toBeNull();
});

test('newest pages absorb individual lookups without duplicate or stale rows', async () => {
  let head = page([9, 8, 7], 700);
  let lookups = 0;
  const { history, state } = fixture(async (path) => {
    if (new URL(path, 'http://fixture').searchParams.has('cycle')) {
      lookups++;
      return page([6], null);
    }
    return head;
  });
  await history.refresh('cycle-6');
  expect(state().rows.map((row) => row.number)).toEqual([9, 8, 7, 6]);
  head = page([9, 8, 7, 6, 5, 4], 400);
  head.items[3].lifecycle.archived_at = '2026-09-10T00:02:00Z';
  await history.refresh('cycle-6');
  expect(lookups).toBe(1);
  expect(state().rows.map((row) => row.number)).toEqual([9, 8, 7, 6, 5, 4]);
  expect(state().rows.find((row) => row.number === 6)?.lifecycle.archived_at).toBeTruthy();
  expect(state().cursor).toBe(400);
});

test('an absent selected cycle removes its standalone lookup without changing pagination', async () => {
  let selected = page([2], null);
  const { history, state } = fixture(async (path) =>
    new URL(path, 'http://fixture').searchParams.has('cycle') ? selected : page([9, 8, 7], 700)
  );
  await history.refresh('cycle-2');
  expect(state().rows.map((row) => row.number)).toEqual([9, 8, 7, 2]);
  selected = page([], null);
  await history.refresh('cycle-2');
  expect(state().rows.map((row) => row.number)).toEqual([9, 8, 7]);
  expect(state().cursor).toBe(700);
});
