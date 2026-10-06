import { expect, test as base, type Page, type Route } from '@playwright/test';
import type {
  BaselineView,
  CommandResult,
  CommandState,
  Config,
  Model,
  ProposalEvidence,
  ProposalRow,
  ReviewRoundEvidence,
  ReviewerVerdict,
  RunEvidenceV1,
  SettingsView,
  Snapshot,
  TaskEvidence
} from '../src/lib/types';

// A route handler that races a client abort (navigation, polling churn) finds
// the request or its fetched response already disposed; the client is gone, so
// there is nothing to fulfill and the error is noise, not a test failure.
export const test = base.extend({
  page: async ({ page }, use) => {
    const register = page.route.bind(page);
    page.route = (url, handler, options) =>
      register(
        url,
        async (route, request) => {
          try {
            await handler(route, request);
          } catch (error) {
            if (!(error instanceof Error && error.message.includes('disposed'))) {
              throw error;
            }
          }
        },
        options
      );
    await use(page);
    await page.unrouteAll({ behavior: 'ignoreErrors' });
  }
});

export const token = 'browser-test-operator-token-32-characters';
export const SYNTHETIC = 'Synthetic browser-test verdict text. Not a real reviewer statement.';
export const now = new Date().toISOString();
export const A = 'a'.repeat(40);
export const B = 'b'.repeat(40);

export function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => (resolve = done));
  return { promise, resolve };
}

export async function login(page: Page) {
  await page.goto('/');
  await page.getByLabel('Operator access token').fill(token);
  await page.getByRole('button', { name: 'Open dashboard' }).click();
  await expect(page.getByRole('heading', { name: 'The bigger picture.' })).toBeVisible();
}

/**
 * Fires the dashboard's 4s polls now instead of waiting for them, then waits until the page has
 * applied the response to `path`. Install the page clock before the page loads; it keeps running,
 * so a poll this races still arrives on its own schedule.
 */
export async function nextPoll(page: Page, path = '**/api/state') {
  const response = page.waitForResponse(path);
  await page.clock.fastForward(4000);
  await (await response).finished();
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(resolve)));
}

export async function openNavigation(page: Page, name: string, mobile: boolean) {
  if (mobile) await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page.getByRole('navigation').getByRole('button', { name, exact: true }).click();
}

export async function openProposalEvidence(page: Page, index: number, mobile: boolean) {
  await openNavigation(page, 'Proposals', mobile);
  await page
    .locator('.proposal-card')
    .nth(index)
    .getByRole('button', { name: 'Inspect decision evidence' })
    .click();
}

export function trackWrites(page: Page) {
  const writes: { path: string; method: string }[] = [];
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname;
    if (path.startsWith('/api/') && request.method() !== 'GET')
      writes.push({ path, method: request.method() });
  });
  return writes;
}

/** Serves the service's own state with `patch` applied, on every poll. */
export async function patchState(page: Page, patch: (snapshot: Snapshot) => void) {
  await page.route('**/api/state', async (route) => {
    const response = await route.fetch();
    if (!response.ok()) {
      await route.fulfill({ response });
      return;
    }
    const snapshot: Snapshot = await response.json();
    patch(snapshot);
    await route.fulfill({ json: snapshot });
  });
}

/** Presents the service as started with --sandbox off, where runner executables are the host's own. */
export function unsandboxed(snapshot: Snapshot) {
  snapshot.sandbox = {
    mode: 'off',
    healthy: false,
    error: null,
    broker: null,
    egress: null,
    pinned_repository: null,
    self_test: null
  };
}

export const ASTRA: Model = {
  backend: 'codex',
  provider: null,
  provider_name: null,
  model: 'gpt-6-astra',
  display_name: 'Astra',
  efforts: ['medium', 'high'],
  variants: [],
  available: true,
  unavailable_reason: null
};

/**
 * A saved configuration the dashboard reads, saves, checks and baselines, on a paused idle
 * service. Saves conflict unless they name the current revision; `saveElsewhere` moves it.
 */
export async function configurationFixture(
  page: Page,
  options: {
    unconfigured?: boolean;
    snapshot?: (snapshot: Snapshot) => void;
    saved?: Partial<Config>;
    catalog?: Model[];
  } = {}
) {
  const state = {
    saved: null as Config | null,
    revision: 'synthetic-revision-0',
    reads: 0,
    writes: [] as { expected_revision: string; config: Record<string, unknown> }[],
    catalogs: [] as { backend: string; binary: string }[],
    checks: [] as string[],
    failLoad: false,
    failSave: false,
    failCheck: false,
    loadGate: null as ReturnType<typeof deferred> | null,
    baselineView: {
      check: null,
      eligible: true,
      reason: null,
      config_matches: null,
      config_revision: null,
      revision_status: 'unknown',
      default_observation: null,
      caveat: 'Synthetic baseline caveat'
    } as BaselineView,
    baselines: [] as unknown[],
    saveElsewhere(config: Partial<Config>) {
      Object.assign(this.saved!, config);
      this.revision = `synthetic-revision-${this.writes.length}-elsewhere`;
    }
  };
  const view = (): SettingsView => ({
    config: structuredClone(state.saved!),
    revision: state.revision,
    transformed_fields: []
  });
  await patchState(page, (snapshot) => {
    snapshot.control.paused = true;
    snapshot.control.mode = 'paused';
    snapshot.active_tasks = 0;
    snapshot.cycle_active = false;
    snapshot.active_cycle_mode = null;
    options.snapshot?.(snapshot);
  });
  await page.route('**/api/config', async (route) => {
    if (route.request().method() === 'PUT') {
      const body = route.request().postDataJSON() as {
        expected_revision: string;
        config: Record<string, unknown>;
      };
      state.writes.push(body);
      if (state.failSave) {
        await route.fulfill({ status: 409, json: { error: 'Synthetic save conflict' } });
        return;
      }
      if (body.expected_revision !== state.revision) {
        await route.fulfill({
          status: 409,
          json: { error: 'Synthetic save conflict; reload settings and check the current values.' }
        });
        return;
      }
      Object.assign(state.saved!, body.config);
      state.revision = `synthetic-revision-${state.writes.length}`;
      await route.fulfill({ json: view() });
      return;
    }
    state.reads++;
    if (!state.saved) {
      const initial = ((await (await route.fetch()).json()) as SettingsView).config;
      const model = options.unconfigured
        ? { backend: 'codex' as const, model: '', effort: '' }
        : { backend: 'codex' as const, model: 'gpt-6-astra', effort: 'medium' };
      state.saved = {
        ...initial,
        repository: options.unconfigured ? '' : '/fixture/repository',
        github_repo: options.unconfigured ? '' : 'fixture/project',
        default_branch: 'fixture-main',
        codex_binary: '/fixture/codex',
        opencode_binary: '/fixture/opencode',
        verification_commands: options.unconfigured ? [] : ['fixture saved test'],
        roles: Object.fromEntries(Object.keys(initial.roles).map((key) => [key, { ...model }])),
        tiers: Object.fromEntries(Object.keys(initial.tiers).map((key) => [key, { ...model }])),
        repair_route: { ...model },
        ...options.saved
      };
    }
    await state.loadGate?.promise;
    await route.fulfill(
      state.failLoad
        ? { status: 503, json: { error: 'Synthetic configuration unavailable' } }
        : { json: view() }
    );
  });
  await page.route('**/api/model-catalog', async (route) => {
    const request = route.request().postDataJSON() as { backend: string; binary: string };
    state.catalogs.push(request);
    await route.fulfill({
      json: (options.catalog ?? [ASTRA]).filter((model) => model.backend === request.backend)
    });
  });
  await page.route('**/api/baseline-checks/latest', async (route) => {
    await route.fulfill({ json: state.baselineView });
  });
  await page.route('**/api/baseline-checks/*/cancel', async (route) => {
    state.baselines.push({ cancel: route.request().url() });
    if (state.baselineView.check) {
      state.baselineView = {
        ...state.baselineView,
        check: {
          ...state.baselineView.check,
          status: 'cancelled',
          completed_at: new Date().toISOString()
        }
      };
    }
    await route.fulfill({ json: { ok: true } });
  });
  await page.route('**/api/baseline-checks', async (route) => {
    const body = route.request().postDataJSON() as { expected_revision: string };
    state.baselines.push(body);
    if (body.expected_revision !== state.revision) {
      await route.fulfill({ status: 409, json: { error: 'Synthetic baseline conflict' } });
      return;
    }
    state.baselineView = {
      ...state.baselineView,
      config_revision: state.revision,
      check: {
        id: 'synthetic-check',
        status: 'running',
        config: structuredClone(state.saved!),
        config_fingerprint: state.revision,
        revision: null,
        started_at: new Date().toISOString(),
        completed_at: null,
        commands: [],
        error: null,
        workspace_removed: false,
        cleanup_error: null
      }
    };
    await route.fulfill({ status: 202, json: state.baselineView.check });
  });
  await page.route('**/api/doctor?*', async (route) => {
    state.checks.push(new URL(route.request().url()).searchParams.get('mode')!);
    const checked_config = structuredClone(state.saved);
    const checked_revision = state.revision;
    await route.fulfill(
      state.failCheck
        ? {
            status: 400,
            json: { error: 'Synthetic connection check failed', checked_config, checked_revision }
          }
        : {
            json: {
              message: 'Synthetic saved configuration checked',
              checked_config,
              checked_revision
            }
          }
    );
  });
  return state;
}

export function reviewer(slot: string, over: Partial<ReviewerVerdict> = {}): ReviewerVerdict {
  return {
    reviewer: slot,
    state: 'recorded',
    decision: 'accepted',
    reason: `${SYNTHETIC} (${slot})`,
    note: null,
    ...over
  };
}

export function reviewRound(over: Partial<ReviewRoundEvidence> = {}): ReviewRoundEvidence {
  return {
    session_id: 'synthetic-review-session',
    revision: B,
    comparison_base: A,
    created_at: now,
    completed: true,
    summary_present: true,
    matches_output_revision: true,
    findings: [],
    ...over
  };
}

export function command(
  name: string,
  state: CommandState,
  revision: string | null = B
): CommandResult {
  return {
    command: name,
    state,
    results_recorded: state === 'no_result' ? 0 : 1,
    latest_success: state === 'no_result' ? null : state !== 'failed',
    latest_revision: revision,
    latest_created_at: state === 'no_result' ? null : now,
    matches_output_revision: state === 'no_result' ? null : revision === B
  };
}

export function taskEvidence(id: string, over: Partial<TaskEvidence> = {}): TaskEvidence {
  return {
    id,
    cycle_id: 'synthetic-cycle',
    proposal_id: 'synthetic-proposal',
    status: 'published',
    branch: `tyk/${id}`,
    attempts: 0,
    blocked_reason: null,
    error_recorded: false,
    created_at: now,
    updated_at: now,
    revisions: { source: A, comparison_base: A, default_branch: 'main', output: B },
    sessions: [
      {
        id: 'synthetic-executor-session',
        role: 'executor',
        status: 'completed',
        requested_route: { backend: 'codex', model: 'gpt-6-astra', effort: 'medium' },
        started_at: now
      }
    ],
    latest_review: {
      rounds_recorded: 1,
      latest: reviewRound(),
      clean: true,
      clean_at_output_revision: true
    },
    required_commands: {
      state: 'recorded',
      commands: [command('go test ./...', 'passed')],
      all_passed_at_output_revision: true
    },
    pull_request: {
      number: 77,
      url: 'https://github.com/fixture/project/pull/77',
      source: 'recorded_task_reference'
    },
    gaps: [],
    ...over
  };
}

export function proposalEvidence(
  id: string,
  over: Partial<ProposalEvidence> = {}
): ProposalEvidence {
  return {
    id,
    title: `Synthetic proposal ${id}`,
    target: 'main',
    tier: 'S',
    category: 'correctness',
    problem: 'Synthetic recorded problem statement for browser tests.',
    benefit: 'Synthetic recorded benefit statement for browser tests.',
    scope: 'Synthetic recorded scope statement for browser tests.',
    evidence: ['internal/example/example.go: synthetic fixture reference'],
    final_decision: 'accepted',
    final_reason: 'Synthetic recorded final rationale for browser tests.',
    reviewer_verdicts: [reviewer('adversary-a'), reviewer('adversary-b')],
    linked_tasks: [],
    gaps: [],
    ...over
  };
}

export function runEvidence(
  over: Partial<RunEvidenceV1> = {},
  cycle: Partial<RunEvidenceV1['cycle']> = {}
): RunEvidenceV1 {
  return {
    schema_version: 1,
    generated_at: now,
    kind: 'recorded_review_check_evidence',
    review_required_before_sharing: true,
    review_requirement:
      'Requires review before sharing. This is a private operator export of saved records, not a public-safe or publication-approved artifact.',
    limitations: ['Synthetic limitation recorded for browser tests.', 'Deferred is not rejected.'],
    cycle: {
      id: 'synthetic-cycle',
      number: 7,
      mode: 'execution',
      status: 'completed',
      started_at: now,
      completed_at: now,
      repository: 'fixture/project',
      grounding_revision: A,
      planning: {
        status: 'completed',
        planning_finished: true,
        proposal_count: 1,
        decisions: { accepted: 1 },
        creates_execution_queue: true,
        error_recorded: false,
        reviewer_batches_saved: 2
      },
      ...cycle
    },
    proposals: [],
    gaps: [],
    ...over
  };
}

export function proposalRow(
  id: string,
  cycleId: string,
  cycleNumber: number,
  over: Partial<ProposalRow> = {}
): ProposalRow {
  return {
    id,
    cycle: cycleNumber,
    cycle_id: cycleId,
    mode: 'execution',
    content_revision: 1,
    title: `Synthetic proposal ${id}`,
    problem: 'Synthetic recorded problem statement for browser tests.',
    benefit: 'Synthetic recorded benefit statement for browser tests.',
    scope: 'Synthetic recorded scope statement for browser tests.',
    target: 'main',
    tier: 'S',
    category: 'correctness',
    evidence: ['internal/example/example.go: synthetic fixture reference'],
    dependencies: [],
    prompt: 'Synthetic execution prompt for browser tests.',
    decision: 'accepted',
    reason: 'Synthetic recorded decision reason for browser tests.',
    problem_key: '',
    relevant_paths: [],
    reconsiders: [],
    ...over
  };
}

export async function serveProposals(page: Page, rows: ProposalRow[]) {
  await page.route('**/api/proposals?*', async (route: Route) => {
    const status = new URL(route.request().url()).searchParams.get('status') ?? 'all';
    const items = status === 'all' ? rows : rows.filter((row) => row.decision === status);
    const counts: Record<string, number> = { all: rows.length };
    for (const row of rows) counts[row.decision] = (counts[row.decision] ?? 0) + 1;
    await route.fulfill({ json: { items, next_cursor: null, counts } });
  });
}
