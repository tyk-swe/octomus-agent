/**
 * Screenshot captures of the populated dashboard, Configuration, the run-evidence
 * panel and the task evidence panel at review viewports, using only SYNTHETIC fixtures.
 *
 * Captures land in `web/artifacts/captures/<label>/` (Git-ignored) so that a "before"
 * set survives Playwright's own `test-results` cleanup. Every file name carries the
 * `synthetic` marker: these images are not evidence of a real run.
 *
 * The test also asserts the layout contracts that screenshots alone cannot prove:
 * no page-level horizontal scrolling, no horizontal overflow inside the dialogs, and
 * no WCAG A/AA axe violations with each panel open.
 */
import { test, expect, type Page, type Route } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import {
  A,
  B,
  Z,
  command,
  login,
  minutesAgo,
  now,
  openNavigation,
  proposalEvidence,
  proposalRow,
  reviewRound,
  reviewer,
  runEvidence,
  serveProposals,
  taskEvidence
} from './synthetic';

// The test tsconfig carries no Node typings; only the environment is read here.
declare const process: { env: Record<string, string | undefined> };
const label = process.env.CAPTURE_LABEL ?? 'after';
/** Relative to the Playwright working directory (`web/`); screenshots create the path. */
const directory = `artifacts/captures/${label}`;
const viewports = [
  { name: '1440x1000', width: 1440, height: 1000 },
  { name: '1280x800', width: 1280, height: 800 },
  { name: '390x844', width: 390, height: 844 }
];

const route = { backend: 'codex' as const, model: 'gpt-6-astra', effort: 'medium' };
const CLEAN_ROUND = reviewRound({ created_at: minutesAgo(95) });

/** A deterministic six-proposal synthetic run that matches the browser-test database. */
function demoRun() {
  const published = taskEvidence('task-reviewed', {
    cycle_id: 'cycle-1',
    proposal_id: 'task-reviewed',
    branch: 'octomus/task-reviewed',
    created_at: minutesAgo(110),
    updated_at: minutesAgo(90),
    sessions: [
      {
        id: 'synthetic-executor-session',
        role: 'executor',
        status: 'completed',
        requested_route: route,
        started_at: minutesAgo(108)
      },
      {
        id: 'synthetic-review-session',
        role: 'code_reviewer',
        status: 'completed',
        requested_route: route,
        started_at: minutesAgo(97)
      }
    ],
    latest_review: {
      rounds_recorded: 1,
      latest: CLEAN_ROUND,
      clean: true,
      clean_at_output_revision: true
    },
    required_commands: {
      state: 'recorded',
      commands: [{ ...command('cargo test', 'passed'), latest_created_at: minutesAgo(92) }],
      all_passed_at_output_revision: true
    },
    pull_request: {
      number: 12,
      url: 'https://github.com/fixture/project/pull/12',
      source: 'recorded_task_reference'
    }
  });
  const queued = taskEvidence('task-active', {
    cycle_id: 'cycle-1',
    proposal_id: 'task-active',
    status: 'queued',
    branch: 'octomus/task-active',
    created_at: minutesAgo(110),
    updated_at: minutesAgo(110),
    revisions: { source: A, comparison_base: null, default_branch: 'main', output: null },
    sessions: [],
    latest_review: {
      rounds_recorded: 0,
      latest: null,
      clean: false,
      clean_at_output_revision: false
    },
    required_commands: {
      state: 'recorded',
      commands: [command('cargo test', 'no_result', null)],
      all_passed_at_output_revision: false
    },
    pull_request: null
  });
  const blocked = taskEvidence('task-blocked', {
    cycle_id: 'cycle-1',
    proposal_id: 'task-blocked',
    status: 'blocked',
    branch: 'octomus/task-blocked',
    attempts: 1,
    blocked_reason: 'verification_timeout',
    error_recorded: true,
    created_at: minutesAgo(110),
    updated_at: minutesAgo(40),
    latest_review: {
      rounds_recorded: 2,
      latest: reviewRound({
        session_id: 'synthetic-second-review-session',
        revision: Z,
        completed: false,
        summary_present: false,
        matches_output_revision: false,
        created_at: minutesAgo(45)
      }),
      clean: false,
      clean_at_output_revision: false
    },
    required_commands: {
      state: 'recorded',
      commands: [
        {
          ...command('cargo test --locked', 'passed_at_other_revision', Z),
          latest_created_at: minutesAgo(50)
        },
        {
          ...command(
            'npm ci --prefix web && npm run build --prefix web && npm run check --prefix web && npm run format:check --prefix web',
            'failed',
            B
          ),
          results_recorded: 2,
          latest_created_at: minutesAgo(42)
        },
        command('python3 tests/e2e.py', 'no_result', null)
      ],
      all_passed_at_output_revision: false
    },
    pull_request: null,
    gaps: ['An output revision is recorded without a clean latest review at that revision.']
  });
  return runEvidence(
    {
      limitations: [
        'Synthetic limitation recorded for browser tests.',
        'Planning completion is not task completion: a completed cycle records decisions, not delivered work.',
        'Deferred is not rejected.',
        'A recorded pull request describes delivery, not merge. Published is not merged.'
      ],
      proposals: [
        proposalEvidence('task-reviewed', {
          title: 'Explain the local development workflow',
          tier: 'S',
          category: 'documentation',
          problem:
            'Synthetic fixture: the contributor guide does not explain how to run the dashboard and the service together during local development.',
          benefit:
            'Synthetic fixture: a contributor can start both halves of the project and see a change working within a few minutes.',
          scope:
            'Synthetic fixture: document the existing commands only. No build, script or configuration changes.',
          evidence: [
            'docs/contributing.md: synthetic fixture reference',
            'web/package.json: synthetic fixture reference'
          ],
          final_reason:
            'Synthetic fixture: both reviewers found a concrete, bounded documentation gap with a verifiable outcome.',
          reviewer_verdicts: [
            reviewer('adversary-a', {
              reason:
                'Synthetic fixture verdict: the gap is real and the proposed scope stays within documentation.'
            }),
            reviewer('adversary-b', {
              reason:
                'Synthetic fixture verdict: low risk; the existing commands are already covered by the repository checks.'
            })
          ],
          linked_tasks: [published]
        }),
        proposalEvidence('task-active', {
          title: 'Complete the repository setup flow',
          tier: 'M',
          category: 'features',
          problem: 'Synthetic fixture: first-run configuration stops before the connection check.',
          benefit: 'Synthetic fixture: operators finish setup without leaving the dashboard.',
          scope: 'Synthetic fixture: dashboard and configuration validation only.',
          evidence: ['web/src/lib/Settings.svelte: synthetic fixture reference'],
          final_reason: 'Synthetic fixture: accepted as a bounded feature completion.',
          reviewer_verdicts: [
            reviewer('adversary-a', {
              reason: 'Synthetic fixture verdict: the gap blocks first use.'
            }),
            reviewer('adversary-b', {
              reason: 'Synthetic fixture verdict: scope is bounded and testable.'
            })
          ],
          linked_tasks: [queued]
        }),
        proposalEvidence('task-blocked', {
          title: 'Handle interrupted verification commands',
          tier: 'M',
          category: 'correctness',
          problem:
            'Synthetic fixture: a verification command that is interrupted leaves no recorded result.',
          benefit:
            'Synthetic fixture: interrupted checks are recorded as failures instead of silence.',
          scope: 'Synthetic fixture: verification recording only; no scheduling changes.',
          evidence: ['src/engine/execution.rs: synthetic fixture reference'],
          final_reason: 'Synthetic fixture: accepted; the failure mode is reproducible.',
          reviewer_verdicts: [
            reviewer('adversary-a', {
              reason: 'Synthetic fixture verdict: the missing record is observable.'
            }),
            reviewer('adversary-b', {
              reason: 'Synthetic fixture verdict: moderate risk, bounded by tests.'
            })
          ],
          linked_tasks: [blocked]
        }),
        proposalEvidence('synthetic-deferred', {
          title: 'Reduce duplicate startup log lines',
          tier: 'XS',
          category: 'maintainability',
          final_decision: 'deferred',
          final_reason:
            'Synthetic fixture: deferred until the logging format decision is recorded; not rejected.',
          reviewer_verdicts: [
            reviewer('adversary-a', {
              decision: 'deferred',
              reason: 'Synthetic fixture verdict: wait for the format decision.'
            }),
            reviewer('adversary-b', {
              decision: 'deferred',
              reason: 'Synthetic fixture verdict: no user-facing impact yet.'
            })
          ]
        }),
        proposalEvidence('synthetic-rejected', {
          title: 'Rewrite the scheduler around a work-stealing pool',
          tier: 'XL',
          category: 'performance',
          final_decision: 'rejected',
          final_reason: 'Synthetic fixture: rejected; no measured problem and an unbounded scope.',
          reviewer_verdicts: [
            reviewer('adversary-a', {
              decision: 'rejected',
              reason: 'Synthetic fixture verdict: no evidence of a bottleneck.'
            }),
            reviewer('adversary-b', {
              decision: 'rejected',
              reason: 'Synthetic fixture verdict: scope exceeds a single PR.'
            })
          ]
        }),
        proposalEvidence('synthetic-unattributed', {
          title: 'Document the release checksum verification',
          tier: 'S',
          category: 'documentation',
          final_reason: 'Synthetic fixture: accepted on the orchestrator record alone.',
          reviewer_verdicts: [
            reviewer('adversary-a', {
              state: 'missing',
              decision: null,
              reason: null,
              note: 'No saved assessment batch is attributed to adversary-a.'
            }),
            reviewer('adversary-b', {
              state: 'malformed',
              decision: null,
              reason: null,
              note: 'Batch 1 is malformed: saved batch does not contain a recorded assessment list.'
            })
          ],
          gaps: [
            'The proposal was accepted but no task is linked in this cycle; acceptance is not execution.'
          ]
        })
      ]
    },
    {
      id: 'cycle-1',
      number: 1,
      started_at: minutesAgo(120),
      completed_at: minutesAgo(112),
      planning: {
        status: 'completed',
        planning_finished: true,
        proposal_count: 6,
        decisions: { accepted: 4, deferred: 1, rejected: 1 },
        creates_execution_queue: true,
        error_recorded: false,
        reviewer_batches_saved: 2
      }
    }
  );
}

function demoProposalRows() {
  return demoRun().proposals.map((p, index) =>
    proposalRow(p.id, 'cycle-1', 1, {
      title: p.title,
      tier: p.tier,
      category: p.category,
      problem: p.problem,
      benefit: p.benefit,
      scope: p.scope,
      evidence: p.evidence,
      decision: p.final_decision,
      reason: p.final_reason,
      content_revision: index + 1
    })
  );
}

async function installDemo(page: Page) {
  const state = { evidence: 'ok' as 'ok' | 'fail' | 'missing' };
  await page.route('**/api/state', async (routeRequest: Route) => {
    const response = await routeRequest.fetch();
    const snapshot = await response.json();
    snapshot.sessions_today = 17;
    snapshot.configured = true;
    snapshot.audit_configured = true;
    snapshot.repository = 'fixture/project';
    if (snapshot.cycles[0]) {
      snapshot.cycles[0].decisions = { accepted: 4, rejected: 1, deferred: 1 };
      snapshot.cycles[0].started_at = minutesAgo(120);
      snapshot.cycles[0].completed_at = minutesAgo(112);
    }
    for (const task of snapshot.tasks) {
      task.created_at = minutesAgo(110);
      task.updated_at = task.status === 'blocked' ? minutesAgo(40) : minutesAgo(90);
    }
    snapshot.events = [
      {
        id: 6,
        at: minutesAgo(40),
        entity_id: 'task-blocked',
        kind: 'error',
        message: 'Synthetic fixture: verification timed out on task-blocked; workspace preserved.'
      },
      {
        id: 5,
        at: minutesAgo(90),
        entity_id: 'task-reviewed',
        kind: 'published',
        message: 'Synthetic fixture: published PR #12 for task-reviewed.'
      },
      {
        id: 4,
        at: minutesAgo(97),
        entity_id: 'task-reviewed',
        kind: 'review',
        message: 'Synthetic fixture: fresh review recorded zero findings for task-reviewed.'
      },
      {
        id: 3,
        at: minutesAgo(108),
        entity_id: 'task-reviewed',
        kind: 'execution',
        message: 'Synthetic fixture: executor session started for task-reviewed.'
      },
      {
        id: 2,
        at: minutesAgo(112),
        entity_id: 'cycle-1',
        kind: 'cycle',
        message: 'Synthetic fixture: planning completed with 4 accepted, 1 deferred, 1 rejected.'
      },
      {
        id: 1,
        at: minutesAgo(120),
        entity_id: 'cycle-1',
        kind: 'cycle',
        message: 'Synthetic fixture: execution cycle 1 started.'
      }
    ];
    await routeRequest.fulfill({ response, json: snapshot });
  });
  await serveProposals(page, demoProposalRows());
  await page.route('**/api/cycles/cycle-1/evidence', async (routeRequest: Route) => {
    if (state.evidence === 'missing')
      await routeRequest.fulfill({
        status: 404,
        json: { error: 'Synthetic retained cycle unavailable' }
      });
    else if (state.evidence === 'fail')
      await routeRequest.fulfill({ status: 503, json: { error: 'Service returned 503' } });
    else await routeRequest.fulfill({ json: demoRun() });
  });
  return state;
}

/** Captures the visible viewport, then the whole dialog with its internal scroll released. */
async function captureDialog(page: Page, width: number, height: number, name: string) {
  await page.screenshot({ path: `${directory}/${name}-viewport.png` });
  const style = await page.addStyleTag({
    content:
      '.task-dialog{height:auto!important;max-height:none!important;bottom:auto!important}.detail-content{overflow:visible!important}'
  });
  const total = await page.locator('dialog').evaluate((el) => el.scrollHeight);
  await page.setViewportSize({ width, height: Math.max(height, Math.min(total + 2, 16000)) });
  await page.screenshot({ path: `${directory}/${name}-full.png` });
  await page.setViewportSize({ width, height });
  await style.evaluate((el) => (el as HTMLElement).remove());
}

async function expectNoHorizontalOverflow(page: Page, scope: string) {
  const overflow = await page.evaluate(() => {
    const problems: string[] = [];
    if (document.documentElement.scrollWidth > innerWidth)
      problems.push(`page scrollWidth ${document.documentElement.scrollWidth} > ${innerWidth}`);
    for (const content of document.querySelectorAll('.detail-content, dialog'))
      if (content.scrollWidth > content.clientWidth + 1)
        problems.push(
          `${content.className || content.tagName} ${content.scrollWidth} > ${content.clientWidth}`
        );
    return problems;
  });
  expect.soft(overflow, scope).toEqual([]);
}

async function expectAccessible(page: Page, scope: string) {
  const result = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze();
  expect
    .soft(
      result.violations.map((v) => ({ rule: v.id, nodes: v.nodes.map((n) => n.target) })),
      scope
    )
    .toEqual([]);
}

test.skip(({ isMobile }) => !!isMobile, 'captures run once per viewport');

for (const viewport of viewports) {
  test(`synthetic captures at ${viewport.name}`, async ({ page }) => {
    test.setTimeout(120000);
    const mobile = viewport.width < 700;
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(e.message));
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    const demo = await installDemo(page);
    const prefix = `synthetic-${viewport.name}`;

    // Populated overview.
    await login(page);
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${directory}/${prefix}-overview.png`, fullPage: true });
    await expectNoHorizontalOverflow(page, 'overview');
    await expectAccessible(page, 'overview');

    // Keyboard focus on the overview's run action.
    await page.keyboard.press('Tab');
    await page.getByRole('button', { name: 'Inspect run' }).focus();
    await page.screenshot({ path: `${directory}/${prefix}-overview-focus.png` });

    // Proposal-review evidence for the published proposal.
    await page.getByRole('button', { name: 'Inspect run' }).click();
    const picker = page.getByLabel('Proposal', { exact: true });
    await picker.selectOption('task-reviewed');
    await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
    await page.waitForTimeout(200);
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-evidence-published`);
    await expectNoHorizontalOverflow(page, 'evidence published');
    await expectAccessible(page, 'evidence published');

    // Missing reviewer evidence, no linked task.
    await picker.selectOption('synthetic-unattributed');
    await expect(page.getByText('Malformed batch', { exact: true })).toBeVisible();
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-evidence-missing`);
    await expectNoHorizontalOverflow(page, 'evidence missing');

    // Mismatched check revisions, failed command, incomplete review, blocked task.
    await picker.selectOption('task-blocked');
    await expect(page.getByText('Passed at another revision', { exact: true })).toBeVisible();
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-evidence-mismatch`);
    await expectNoHorizontalOverflow(page, 'evidence mismatch');
    await expectAccessible(page, 'evidence mismatch');

    // Stale: the next poll fails, retained records stay labelled.
    demo.evidence = 'fail';
    await expect(page.getByText('Retained · stale')).toBeVisible({ timeout: 15000 });
    await page.screenshot({ path: `${directory}/${prefix}-evidence-stale-viewport.png` });
    demo.evidence = 'ok';
    await page.getByRole('button', { name: 'Close run evidence' }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);

    // Task evidence for the published task.
    await openNavigation(page, 'Task queue', mobile);
    await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
    await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
    await page.waitForTimeout(200);
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-task-evidence`);
    await expectNoHorizontalOverflow(page, 'task evidence');
    await expectAccessible(page, 'task evidence');
    await page.getByRole('tab', { name: 'Verification' }).click();
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-task-verification`);
    await page.getByRole('button', { name: 'Close task details' }).click();

    // Task evidence errors: retained missing cycle, explicit retry focus, then recovery.
    demo.evidence = 'missing';
    await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
    await expect(
      page.getByText('Synthetic retained cycle unavailable', { exact: false })
    ).toBeVisible();
    const retryEvidence = page.getByRole('button', { name: 'Retry evidence' });
    await retryEvidence.focus();
    await page.keyboard.press('Shift+Tab');
    await page.keyboard.press('Tab');
    await expect(retryEvidence).toBeFocused();
    await expect(retryEvidence).toHaveCSS('outline-style', 'solid');
    if (mobile) {
      const bounds = await retryEvidence.boundingBox();
      expect(bounds!.width).toBeGreaterThanOrEqual(44);
      expect(bounds!.height).toBeGreaterThanOrEqual(44);
    }
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-task-missing-focus`);
    await expectNoHorizontalOverflow(page, 'task missing');
    await expectAccessible(page, 'task missing');
    demo.evidence = 'ok';
    await page.getByRole('button', { name: 'Retry evidence' }).click();
    await expect(page.getByText('Clean at the output commit', { exact: true })).toBeVisible();
    await page.route('**/api/tasks/task-reviewed', async (request) => {
      const task = await (await request.fetch()).json();
      await request.fulfill({ json: { ...task, updated_at: 'synthetic-changed-revision' } });
    });
    demo.evidence = 'fail';
    await expect(page.getByText('Retained · stale')).toBeVisible({ timeout: 10000 });
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-task-stale`);
    await expectNoHorizontalOverflow(page, 'task stale');
    await page.getByRole('button', { name: 'Close task details' }).click();
    await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
    await expect(page.getByText('Service returned 503', { exact: false })).toBeVisible();
    await captureDialog(page, viewport.width, viewport.height, `${prefix}-task-error`);
    await expectNoHorizontalOverflow(page, 'task error');
    await page.getByRole('button', { name: 'Close task details' }).click();

    // Failed initial request: the panel must explain the failure rather than show nothing.
    demo.evidence = 'fail';
    await openNavigation(page, 'Overview', mobile);
    await page.getByRole('button', { name: 'Inspect run' }).click();
    await expect(page.getByRole('dialog')).toContainText(/503/);
    await page.screenshot({ path: `${directory}/${prefix}-evidence-failed-viewport.png` });
    await page.getByRole('button', { name: 'Close run evidence' }).click();
    demo.evidence = 'ok';

    // Configuration draft, including mobile feedback and a synthetic runner catalog.
    await page.route('**/api/config', async (request) => {
      expect(request.request().method()).toBe('GET');
      const response = await request.fetch();
      const config = await response.json();
      const model = { backend: 'codex', model: 'gpt-6-astra', effort: 'medium' };
      config.repository = '/srv/synthetic/project';
      config.github_repo = 'fixture/project';
      config.branch_prefix = 'tyk/';
      config.codex_binary = 'codex';
      config.opencode_binary = 'opencode';
      config.verification_commands = ['cargo test'];
      config.roles = Object.fromEntries(Object.keys(config.roles).map((key) => [key, model]));
      config.repair_route = model;
      await request.fulfill({ json: config });
    });
    await page.route('**/api/model-catalog', (request) =>
      request.fulfill({
        json: [
          {
            backend: 'codex',
            provider: null,
            provider_name: null,
            model: 'gpt-6-astra',
            display_name: 'Astra',
            efforts: ['low', 'medium', 'high'],
            variants: [],
            available: true,
            unavailable_reason: null
          }
        ]
      })
    );
    await openNavigation(page, 'Configuration', mobile);
    await page.getByRole('button', { name: 'Load Codex models' }).click();
    await page
      .getByRole('textbox', { name: /^Verification commands/ })
      .fill('cargo test\nnpm run check --prefix web');
    await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({ path: `${directory}/${prefix}-configuration-viewport.png` });
    await page.screenshot({
      path: `${directory}/${prefix}-configuration-full.png`,
      fullPage: true
    });
    await expectNoHorizontalOverflow(page, 'configuration');
    await expectAccessible(page, 'configuration');
    if (mobile) {
      const smallTargets = await page
        .locator(
          'button:visible, input:visible, select:visible, textarea:visible, .checkbox:visible'
        )
        .evaluateAll((elements) =>
          elements
            .filter((element) => !element.matches('[type="checkbox"]'))
            .filter((element) => {
              const box = element.getBoundingClientRect();
              return box.height < 44 || box.width < 44;
            })
            .map((element) => element.textContent?.trim() || element.getAttribute('aria-label'))
        );
      expect(smallTargets, '44px mobile controls').toEqual([]);
      expect(
        await page
          .locator('input:visible:not([type="checkbox"]), textarea:visible, select:visible')
          .evaluateAll((elements) =>
            elements.every((element) => parseFloat(getComputedStyle(element).fontSize) >= 16)
          )
      ).toBe(true);
    }
    await page.emulateMedia({ reducedMotion: 'reduce' });
    if (mobile) await page.getByRole('button', { name: 'Toggle navigation' }).click();
    const motion = await page
      .locator('.sidebar')
      .evaluate((element) => parseFloat(getComputedStyle(element).transitionDuration));
    expect(motion).toBeLessThanOrEqual(0.00001);
    await page.screenshot({ path: `${directory}/${prefix}-configuration-reduced-motion.png` });

    expect(errors).toEqual([]);
    void now;
  });
}

test('unconfigured first-run overview at 1440x1000', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.route('**/api/state', async (routeRequest: Route) => {
    const response = await routeRequest.fetch();
    const snapshot = await response.json();
    snapshot.configured = false;
    snapshot.repository = '';
    snapshot.tasks = [];
    snapshot.attention_tasks = [];
    snapshot.cycles = [];
    snapshot.events = [];
    snapshot.counts = {};
    snapshot.active_tasks = 0;
    snapshot.storage = null;
    await routeRequest.fulfill({ response, json: snapshot });
  });
  await login(page);
  await page.waitForTimeout(300);
  await page.screenshot({
    path: `${directory}/synthetic-1440x1000-unconfigured.png`,
    fullPage: true
  });
  await expectNoHorizontalOverflow(page, 'unconfigured overview');
  await expectAccessible(page, 'unconfigured overview');
});
