export type CopyFeedback = {
  readonly status: string;
  copy: (value: string, label: string) => Promise<void>;
  dispose: () => void;
};

/** Copies to the clipboard and announces the outcome for four seconds; the latest copy wins. */
export function createCopyFeedback(): CopyFeedback {
  let status = $state('');
  let timer: ReturnType<typeof setTimeout> | undefined;
  let generation = 0;
  let disposed = false;
  return {
    get status() {
      return status;
    },
    async copy(value, label) {
      if (disposed) return;
      const current = ++generation;
      clearTimeout(timer);
      status = '';
      let copied = true;
      try {
        await navigator.clipboard.writeText(value);
      } catch {
        copied = false;
      }
      if (disposed || current !== generation) return;
      status = copied
        ? `${label} copied.`
        : `${label} could not be copied in this browser context.`;
      timer = setTimeout(() => {
        if (current === generation) status = '';
      }, 4000);
    },
    dispose() {
      disposed = true;
      generation++;
      clearTimeout(timer);
    }
  };
}
