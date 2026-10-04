import type { Verdict } from './evidence';
import { plural } from './format';
import type { SandboxPosture } from './types';

/** How isolated work is right now, in one line for the overview and the setup checklist. */
export function sandboxVerdict(sandbox: SandboxPosture): Verdict {
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
      label: `${plural(failed.length, 'check')} failed`,
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
