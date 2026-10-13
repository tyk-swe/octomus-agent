import { expect, test } from '@playwright/test';
import { Resource, type ResourceState } from '../src/lib/resource';
import { StateSession } from '../src/lib/stateSession';
import type { Snapshot } from '../src/lib/types';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((done, failed) => {
    resolve = done;
    reject = failed;
  });
  return { promise, resolve, reject };
}

// A coalescing resource whose every read the test resolves by hand, so a request's
// identity (which key was read, in which order) is directly observable.
//
// The invariant these tests hold it to: only the newest requested key may own the final
// fresh state. Retained older evidence may stay visible, but marked stale, and obsolete
// queued intent must never run afterwards and become authoritative.
function coalescing<T>() {
  const reads: { key: string; read: ReturnType<typeof deferred<T>> }[] = [];
  let current: ResourceState<T> = { value: null, loading: false, stale: false, error: '' };
  const resource = new Resource<T>(
    (key: string) => {
      const read = deferred<T>();
      reads.push({ key, read });
      return read.promise;
    },
    (state) => {
      current = state;
    },
    true
  );
  return {
    resource,
    reads,
    keys: () => reads.map((entry) => entry.key),
    state: () => current
  };
}

test('the newest requested key owns the final fresh state when an older key is requested again', async () => {
  const { resource, reads, keys, state } = coalescing<string>();
  const first = resource.refresh('A');
  expect(resource.refresh('B')).toBe(first);
  // A is requested again while its own read is pending: it is the newest request, so
  // the queued B is obsolete intent and must not become authoritative afterwards.
  expect(resource.refresh('A')).toBe(first);
  reads[0].read.resolve('A evidence');
  expect(await first).toBe(true);
  expect(keys()).toEqual(['A']);
  expect(state()).toMatchObject({ value: 'A evidence', loading: false, stale: false, error: '' });
});

test('coalescing keeps only the newest queued key across a longer sequence', async () => {
  const { resource, reads, keys, state } = coalescing<string>();
  const first = resource.refresh('A');
  void resource.refresh('B');
  void resource.refresh('A');
  void resource.refresh('C');
  reads[0].read.resolve('A evidence');
  expect(await first).toBe(true);
  expect(keys()).toEqual(['A', 'C']);
  reads[1].read.resolve('C evidence');
  expect(await resource.refresh('C')).toBe(true);
  expect(state()).toMatchObject({ value: 'C evidence', loading: false, stale: false, error: '' });

  // Repeated same-key requests share one read, and none of them outlive it.
  const repeated = resource.refresh('C');
  expect(resource.refresh('C')).toBe(repeated);
  expect(resource.refresh('C')).toBe(repeated);
  expect(keys()).toEqual(['A', 'C', 'C']);
  reads[2].read.resolve('C again');
  expect(await repeated).toBe(true);
  expect(state()).toMatchObject({ value: 'C again', stale: false, error: '' });

  // A repeat of the loading key supersedes a queued different key.
  const held = resource.refresh('D');
  void resource.refresh('E');
  expect(resource.refresh('D')).toBe(held);
  reads[3].read.resolve('D evidence');
  expect(await held).toBe(true);
  expect(keys()).toEqual(['A', 'C', 'C', 'D']);
  expect(state()).toMatchObject({ value: 'D evidence', loading: false, stale: false, error: '' });
});

test('a failed read for the newest key keeps retained evidence stale and never confirms a superseded key', async () => {
  const { resource, reads, keys, state } = coalescing<string>();
  const initial = resource.refresh('A');
  reads[0].read.resolve('A evidence');
  expect(await initial).toBe(true);

  const held = resource.refresh('A');
  void resource.refresh('B');
  expect(resource.refresh('A')).toBe(held);
  reads[1].read.reject(new Error('Confirming read failed'));
  expect(await held).toBe(false);
  expect(keys()).toEqual(['A', 'A']);
  expect(state()).toMatchObject({
    value: 'A evidence',
    loading: false,
    stale: true,
    error: 'Confirming read failed'
  });

  const retry = resource.refresh('A');
  reads[2].read.resolve('A confirmed');
  expect(await retry).toBe(true);
  expect(state()).toMatchObject({ value: 'A confirmed', stale: false, error: '' });
});

test('forced refresh, invalidate, cancel and dispose discard queued intent and ignore late responses', async () => {
  const { resource, reads, keys, state } = coalescing<string>();
  const initial = resource.refresh('A');
  reads[0].read.resolve('A evidence');
  expect(await initial).toBe(true);

  // A forced read replaces the pending read and the intent queued behind it.
  const first = resource.refresh('A');
  void resource.refresh('B');
  const forced = resource.refresh('C', true);
  expect(forced).not.toBe(first);
  reads[1].read.resolve('late A');
  expect(await first).toBe(false);
  reads[2].read.resolve('C evidence');
  expect(await forced).toBe(true);
  expect(keys()).toEqual(['A', 'A', 'C']);
  expect(state()).toMatchObject({ value: 'C evidence', loading: false, stale: false, error: '' });

  // Invalidate keeps retained content visible as stale and drops the queued key.
  const heldB = resource.refresh('B');
  void resource.refresh('D');
  resource.invalidate();
  expect(state()).toMatchObject({ value: 'C evidence', stale: true, loading: false });
  reads[3].read.resolve('late B');
  expect(await heldB).toBe(false);
  expect(state().value).toBe('C evidence');
  expect(keys()).toEqual(['A', 'A', 'C', 'B']);

  // Cancel abandons the read and the intent queued behind it.
  const heldE = resource.refresh('E');
  void resource.refresh('F');
  resource.cancel();
  expect(state().loading).toBe(false);
  reads[4].read.resolve('late E');
  expect(await heldE).toBe(false);
  expect(keys()).toEqual(['A', 'A', 'C', 'B', 'E']);

  // Dispose refuses new reads and ignores every response already in flight.
  const heldG = resource.refresh('G');
  void resource.refresh('H');
  resource.dispose();
  expect(await resource.refresh('I')).toBe(false);
  reads[5].read.resolve('late G');
  expect(await heldG).toBe(false);
  expect(keys()).toEqual(['A', 'A', 'C', 'B', 'E', 'G']);
  expect(state().value).toBe('C evidence');
});

test('a pending keyed replacement marks retained content stale until it succeeds', async () => {
  const reads: ReturnType<typeof deferred<string>>[] = [];
  let current = { value: null as string | null, loading: false, stale: false, error: '' };
  const resource = new Resource(
    (_key: string) => {
      const read = deferred<string>();
      reads.push(read);
      return read.promise;
    },
    (state) => {
      current = state;
    }
  );
  const initial = resource.refresh('revision-1');
  expect(current).toMatchObject({ value: null, loading: true, stale: false });
  reads[0].resolve('revision-1 evidence');
  await initial;
  expect(current).toMatchObject({ value: 'revision-1 evidence', loading: false, stale: false });

  const replacement = resource.refresh('revision-2');
  expect(current).toMatchObject({ value: 'revision-1 evidence', loading: true, stale: true });
  expect(resource.refresh('revision-2')).toBe(replacement);
  expect(reads).toHaveLength(2);
  expect(current.stale).toBe(true);
  reads[1].resolve('revision-2 evidence');
  await replacement;
  expect(current).toMatchObject({ value: 'revision-2 evidence', loading: false, stale: false });
});

test('resource rejects abandoned reads and keeps failed confirming reads stale and retryable', async () => {
  const reads: ReturnType<typeof deferred<string>>[] = [];
  let current = { value: null as string | null, loading: false, stale: false, error: '' };
  const resource = new Resource(
    (_key: string, _signal) => {
      const read = deferred<string>();
      reads.push(read);
      return read.promise;
    },
    (state) => {
      current = state;
    }
  );
  const old = resource.refresh('first');
  const fresh = resource.refresh('second');
  reads[0].resolve('abandoned');
  expect(await old).toBe(false);
  reads[1].resolve('current');
  expect(await fresh).toBe(true);
  expect(current.value).toBe('current');
  resource.invalidate();
  const confirmation = resource.refresh('second', true);
  reads[2].reject(new Error('Confirming read failed'));
  expect(await confirmation).toBe(false);
  expect(current).toMatchObject({
    value: 'current',
    stale: true,
    loading: false,
    error: 'Confirming read failed'
  });
  const retry = resource.refresh('second');
  reads[3].resolve('confirmed');
  expect(await retry).toBe(true);
  expect(current).toMatchObject({ value: 'confirmed', stale: false, error: '' });
  const disposedRead = resource.refresh('third');
  resource.dispose();
  reads[4].resolve('after disposal');
  expect(await disposedRead).toBe(false);
  expect(current.value).toBe('confirmed');
});

test('slow evidence coalesces newer revisions without preventing other resources from progressing', async () => {
  const reads: { key: string; read: ReturnType<typeof deferred<string>> }[] = [];
  let evidence = '';
  let stale = false;
  const resource = new Resource(
    (key: string, _signal) => {
      const read = deferred<string>();
      reads.push({ key, read });
      return read.promise;
    },
    (state) => {
      evidence = state.value ?? '';
      stale = state.stale;
    },
    true
  );
  const first = resource.refresh('old');
  void resource.refresh('intermediate');
  void resource.refresh('newest');
  let independent = '';
  const activity = new Resource(
    async (_key: string) => 'current activity',
    (state) => {
      independent = state.value ?? '';
    }
  );
  await activity.refresh('activity');
  expect(independent).toBe('current activity');
  expect(reads.map((entry) => entry.key)).toEqual(['old']);
  reads[0].read.resolve('retained');
  await first;
  expect(evidence).toBe('retained');
  expect(stale).toBe(true);
  expect(reads.map((entry) => entry.key)).toEqual(['old', 'newest']);
  reads[1].read.resolve('current');
  await resource.refresh('newest');
  expect(evidence).toBe('current');
  expect(stale).toBe(false);
});

test('state session cannot confirm an accepted mutation with a preceding poll or old login', async () => {
  const reads: ReturnType<typeof deferred<Snapshot>>[] = [];
  const accepted: string[] = [];
  const errors: string[] = [];
  let pending = false;
  const session = new StateSession(
    (_signal) => {
      const read = deferred<Snapshot>();
      reads.push(read);
      return read.promise;
    },
    (snapshot) => accepted.push(snapshot.status),
    (error) => errors.push(error.message),
    (value) => {
      pending = value;
    }
  );
  const poll = session.refresh();
  session.invalidate();
  const confirmation = session.refresh();
  expect(confirmation).toBe(poll);
  reads[0].resolve({ status: 'before mutation' } as Snapshot);
  await Promise.resolve();
  reads[1].reject(new Error('Confirmation unavailable'));
  await confirmation;
  expect(accepted).toEqual([]);
  expect(errors).toEqual(['Confirmation unavailable']);
  expect(pending).toBe(true);
  const retry = session.refresh();
  reads[2].resolve({ status: 'confirmed' } as Snapshot);
  await retry;
  expect(pending).toBe(false);
  const priorLogin = session.refresh();
  session.reset();
  const currentLogin = session.refresh();
  reads[3].resolve({ status: 'prior login' } as Snapshot);
  await priorLogin;
  reads[4].resolve({ status: 'current login' } as Snapshot);
  await currentLogin;
  expect(accepted).toEqual(['confirmed', 'current login']);
});
