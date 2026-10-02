import { expect, test } from '@playwright/test';
import { copyMessage, createCopyController } from '../src/lib/clipboard';

test.skip(({ isMobile }) => isMobile, 'Controlled clipboard feedback rules run once.');

function fixture() {
  let status = '';
  let sequence = 0;
  const updates: string[] = [];
  const writes: { value: string; complete: (success: boolean) => void }[] = [];
  type Timer = ReturnType<typeof setTimeout>;
  const timers = new Map<Timer, { callback: () => void; delay: number; active: boolean }>();
  const controller = createCopyController(
    (next) => {
      status = next;
      updates.push(next);
    },
    (value) => new Promise((complete) => writes.push({ value, complete })),
    {
      schedule(callback, delay) {
        const timer = ++sequence as unknown as Timer;
        timers.set(timer, { callback, delay, active: true });
        return timer;
      },
      clear(timer) {
        if (timer !== undefined && timers.has(timer)) timers.get(timer)!.active = false;
      }
    }
  );
  return {
    controller,
    writes,
    updates,
    timers,
    get status() {
      return status;
    },
    get activeTimers() {
      return [...timers.values()].filter((timer) => timer.active);
    }
  };
}

for (const olderSuccess of [false, true]) {
  for (const newerSuccess of [false, true]) {
    test(`older copy ${olderSuccess ? 'success' : 'failure'} cannot replace newer ${newerSuccess ? 'success' : 'failure'}`, async () => {
      const f = fixture();
      const older = f.controller.copy('old value', 'Earlier commit');
      const newer = f.controller.copy('new value', 'Latest commit');
      expect(f.writes.map((write) => write.value)).toEqual(['old value', 'new value']);
      f.writes[1].complete(newerSuccess);
      await newer;
      expect(f.status).toBe(copyMessage('Latest commit', newerSuccess));
      const updates = [...f.updates];
      f.writes[0].complete(olderSuccess);
      await older;
      expect(f.updates).toEqual(updates);
      expect(f.status).toBe(copyMessage('Latest commit', newerSuccess));
      expect(f.activeTimers).toHaveLength(1);
      expect(f.activeTimers[0].delay).toBe(4000);
      f.controller.dispose();
    });
  }
}

test('an earlier completion cannot start an expiry while the latest copy is still pending', async () => {
  const f = fixture();
  const older = f.controller.copy('old', 'Earlier commit');
  const newer = f.controller.copy('new', 'Latest commit');
  f.writes[0].complete(true);
  await older;
  expect(f.status).toBe('');
  expect(f.timers.size).toBe(0);
  f.writes[1].complete(true);
  await newer;
  expect(f.status).toBe('Latest commit copied.');
  expect(f.activeTimers).toHaveLength(1);
  f.controller.dispose();
});

test('a replacement clears old feedback and owns its full expiry even if a cancelled timer fires', async () => {
  const f = fixture();
  const first = f.controller.copy('first', 'First commit');
  f.writes[0].complete(true);
  await first;
  const oldTimer = f.activeTimers[0];
  const next = f.controller.copy('next', 'Next commit');
  expect(f.writes).toHaveLength(2);
  expect(f.status).toBe('');
  expect(oldTimer.active).toBe(false);
  oldTimer.callback();
  expect(f.status).toBe('');
  f.writes[1].complete(false);
  await next;
  expect(f.status).toBe(copyMessage('Next commit', false));
  oldTimer.callback();
  expect(f.status).toBe(copyMessage('Next commit', false));
  f.activeTimers[0].callback();
  expect(f.status).toBe('');
  f.controller.dispose();
});

test('repeated disposal prevents pending results and future calls from creating feedback or timers', async () => {
  const f = fixture();
  const older = f.controller.copy('old', 'Earlier commit');
  const newer = f.controller.copy('new', 'Latest commit');
  f.controller.dispose();
  f.controller.dispose();
  const updates = [...f.updates];
  f.writes[0].complete(true);
  f.writes[1].complete(false);
  await Promise.all([older, newer]);
  await f.controller.copy('ignored', 'Disposed commit');
  expect(f.updates).toEqual(updates);
  expect(f.writes).toHaveLength(2);
  expect(f.timers.size).toBe(0);
});

test('disposal invalidates an existing expiry callback even if it has already been queued', async () => {
  const f = fixture();
  const copy = f.controller.copy('value', 'Commit');
  f.writes[0].complete(true);
  await copy;
  const timer = f.activeTimers[0];
  f.controller.dispose();
  f.controller.dispose();
  const updates = [...f.updates];
  timer.callback();
  expect(f.activeTimers).toHaveLength(0);
  expect(f.updates).toEqual(updates);
});
