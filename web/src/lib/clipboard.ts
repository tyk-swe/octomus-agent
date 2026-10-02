export async function copyText(value: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
}

export function copyMessage(label: string, copied: boolean): string {
  return copied ? `${label} copied.` : `${label} could not be copied in this browser context.`;
}

type CopyTimers = {
  schedule: (callback: () => void, delay: number) => ReturnType<typeof setTimeout>;
  clear: (timer: ReturnType<typeof setTimeout> | undefined) => void;
};

const copyTimers: CopyTimers = {
  schedule: (callback, delay) => setTimeout(callback, delay),
  clear: (timer) => clearTimeout(timer)
};

export function createCopyController(
  update: (status: string) => void,
  write: (value: string) => Promise<boolean> = copyText,
  timers: CopyTimers = copyTimers
) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let generation = 0;
  let disposed = false;
  return {
    async copy(value: string, label: string) {
      if (disposed) return;
      const current = ++generation;
      timers.clear(timer);
      update('');
      const copied = await write(value);
      if (disposed || current !== generation) return;
      update(copyMessage(label, copied));
      timer = timers.schedule(() => {
        if (current === generation) update('');
      }, 4000);
    },
    dispose() {
      disposed = true;
      generation++;
      timers.clear(timer);
    }
  };
}
