import { createCopyController } from './clipboard';

export type CopyFeedback = {
  readonly status: string;
  copy: (value: string, label: string) => Promise<void>;
  dispose: () => void;
};

export function createCopyFeedback(): CopyFeedback {
  let status = $state('');
  return {
    get status() {
      return status;
    },
    ...createCopyController((value) => (status = value))
  };
}
