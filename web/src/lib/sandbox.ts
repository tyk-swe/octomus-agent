import type { Tone } from './evidence';
import type { SandboxPosture } from './types';

export type SandboxVerdict = { label: string; tone: Tone; detail: string };

/** How isolated work is right now, in one line for the overview and the setup checklist. */
export function sandboxVerdict(sandbox: SandboxPosture): SandboxVerdict {
  if (sandbox.mode === 'off') {
    return {
      label: 'Unsandboxed',
      tone: 'failed',
      detail:
        'Agents and verification commands run with this service user’s permissions. Use only on a dedicated VM.'
    };
  }
  if (!sandbox.healthy) {
    return {
      label: 'Unavailable',
      tone: 'failed',
      detail: `${sandbox.error ?? 'The sandbox broker does not answer.'} No work starts until it is back.`
    };
  }
  const test = sandbox.self_test;
  if (!test) {
    return {
      label: 'Not yet proven',
      tone: 'blocked',
      detail: 'Run the self-test or Check connection to prove containment from inside a sandbox.'
    };
  }
  if (test.error !== null) {
    return {
      label: 'Self-test error',
      tone: 'failed',
      detail: test.error || 'The containment self-test did not complete.'
    };
  }
  const failed = test.checks.filter((check) => !check.passed);
  if (failed.length) {
    return {
      label: `${failed.length} check${failed.length === 1 ? '' : 's'} failed`,
      tone: 'failed',
      detail: failed.map((check) => check.label).join(' · ')
    };
  }
  if (!test.checks.length) {
    return {
      label: 'Self-test failed',
      tone: 'failed',
      detail: 'No containment checks were recorded. Run the self-test again to prove containment.'
    };
  }
  if (!test.passed) {
    return {
      label: 'Self-test failed',
      tone: 'failed',
      detail: 'The recorded self-test did not pass. Run it again to verify containment.'
    };
  }
  return {
    label: 'Contained',
    tone: 'clean',
    detail: `All ${test.checks.length} containment checks passed inside a real sandbox.`
  };
}

export function bytesLabel(bytes: number): string {
  const gib = bytes / 2 ** 30;
  return gib >= 1 ? `${Number(gib.toFixed(1))} GiB` : `${Math.round(bytes / 2 ** 20)} MiB`;
}

export function shortImage(id: string): string {
  return id.replace(/^sha256:/, '').slice(0, 12);
}
