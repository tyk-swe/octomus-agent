import { copyMessage, copyText } from './clipboard';

/** The copy status line and its reset, shared by the task and run-evidence panels. */
export type CopyFeedback = {
  readonly status: string;
  copy: (value: string, label: string) => Promise<void>;
  dispose: () => void;
};

export function createCopyFeedback(): CopyFeedback {
  let status = $state('');
  let timer: ReturnType<typeof setTimeout> | undefined;
  return {
    get status() {
      return status;
    },
    async copy(value: string, label: string) {
      clearTimeout(timer);
      status = copyMessage(label, await copyText(value));
      timer = setTimeout(() => (status = ''), 4000);
    },
    dispose() {
      clearTimeout(timer);
    }
  };
}
