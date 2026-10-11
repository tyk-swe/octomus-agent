import { expect, test } from '@playwright/test';
import { Resource } from '../src/lib/resource';
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
