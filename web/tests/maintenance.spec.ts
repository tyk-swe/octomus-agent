import { expect } from '@playwright/test';
import {
  configurationFixture,
  login,
  now,
  openNavigation,
  patchState,
  runEvidence,
  taskEvidence,
  test
} from './synthetic';

test('maintenance delivery mode warns, disables features and saves limits and exclusions @responsive', async ({
  page,
  isMobile
}) => {
  const state = await configurationFixture(page);
  await login(page);
  await openNavigation(page, 'Configuration', !!isMobile);

  const mode = page.getByLabel('Delivery mode', { exact: true });
  await expect(mode).toHaveValue('standard');
  await expect(page.getByText(/Maintenance mode is opt-in/)).toHaveCount(0);
  await expect(page.getByRole('textbox', { name: /Manual-merge paths/ })).toHaveCount(0);
  const features = page.getByRole('checkbox', { name: 'features', exact: true });
  await expect(features).toBeChecked();
  await expect(features).toBeEnabled();

  await mode.selectOption('maintenance');
  await expect(page.getByText(/Maintenance mode is opt-in/)).toBeVisible();
  await expect(features).toBeChecked();
  await expect(features).toBeDisabled();
  await expect(page.getByText('Features stay in the saved category selection')).toBeVisible();

  const paths = page.getByRole('textbox', { name: /Manual-merge paths/ });
  await paths.fill('internal/deploy\nsecrets.txt\n\n');
  await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes).toHaveLength(1);
  expect(state.writes[0].config).toEqual({
    delivery_mode: 'maintenance',
    auto_merge_excluded_paths: ['internal/deploy', 'secrets.txt']
  });
  expect(state.saved!.delivery_mode).toBe('maintenance');
  expect(state.saved!.auto_merge_excluded_paths).toEqual(['internal/deploy', 'secrets.txt']);
  await expect(paths).toHaveValue('internal/deploy\nsecrets.txt');
  await expect(page.getByText(/Maintenance mode is opt-in/)).toBeVisible();
});

test('maintenance limits bound the mergeable footprint and excluded paths stay in the saved draft', async ({
  page
}) => {
  const state = await configurationFixture(page, {
    saved: {
      delivery_mode: 'maintenance',
      auto_merge_max_lines: 800,
      auto_merge_max_files: 20,
      auto_merge_excluded_paths: ['internal/access/']
    }
  });
  await login(page);
  await openNavigation(page, 'Configuration', false);

  await expect(page.getByLabel('Delivery mode', { exact: true })).toHaveValue('maintenance');
  const paths = page.getByRole('textbox', { name: /Manual-merge paths/ });
  await expect(paths).toHaveValue('internal/access/');
  const lines = page.getByRole('spinbutton', { name: /^Auto-merge line limit/ });
  const files = page.getByRole('spinbutton', { name: /^Auto-merge file limit/ });
  await expect(lines).toHaveValue('800');
  await expect(files).toHaveValue('20');
  await lines.fill('250');
  await files.fill('7');
  await page.getByRole('button', { name: 'Save configuration' }).click();
  await expect(page.getByText('Configuration saved.', { exact: true })).toBeVisible();
  expect(state.writes.at(-1)!.config).toEqual({
    auto_merge_max_lines: 250,
    auto_merge_max_files: 7
  });

  await page.getByLabel('Delivery mode', { exact: true }).selectOption('standard');
  await expect(page.getByText(/Maintenance mode is opt-in/)).toHaveCount(0);
  await expect(paths).toHaveCount(0);
  await expect(page.getByRole('checkbox', { name: 'features', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Discard changes' }).click();
  await expect(page.getByLabel('Delivery mode', { exact: true })).toHaveValue('maintenance');
  await expect(paths).toHaveValue('internal/access/');
});

test('overview, notices and pull requests show automatic merge activity and evidence @responsive', async ({
  page,
  isMobile
}) => {
  await patchState(page, (snapshot) => {
    snapshot.delivery_mode = 'maintenance';
    snapshot.control.mode = 'run_once';
    snapshot.control.batch = { id: 'run-1', phase: 'merging', cycle_id: 'cycle-1' };
    snapshot.auto_merge = {
      active: true,
      counts: { waiting: 2, merging: 1, merged: 3, manual: 4, closed: 0, uncertain: 0 }
    };
  });
  await page.route('**/api/prs?*', async (route) => {
    const response = await route.fetch();
    const page = await response.json();
    const mergeEvidence = (status: string, extra: Record<string, unknown> = {}) => ({
      task_id: 'task-reviewed',
      head: 'b'.repeat(40),
      comparison_base: 'a'.repeat(40),
      head_branch: 'octomus/task-reviewed',
      base_branch: 'main',
      policy_revision: 'synthetic-policy',
      authorized: true,
      footprint: null,
      status,
      reason: '',
      observed_at: now,
      attempt_id: null,
      attempted_at: null,
      merge_commit: null,
      result_source: null,
      ...extra
    });
    for (const observation of page.items) {
      if (observation.pr.number === 12) {
        observation.auto_merge = mergeEvidence('waiting', {
          reason: 'Waiting for checks to complete on the reviewed head'
        });
      }
      if (observation.pr.number === 7) {
        observation.pr.state = 'merged';
        observation.auto_merge = mergeEvidence('merged', {
          reason: 'Squash merged by Octomus',
          merge_commit: 'c'.repeat(40),
          result_source: 'confirmed'
        });
      }
    }
    page.items.push({
      repository: 'fixture/project',
      pr: {
        number: 88,
        title: 'Uncertain merge acknowledgement',
        branch: 'octomus/uncertain',
        head: 'd'.repeat(40),
        base: 'main',
        url: 'https://github.com/fixture/project/pull/88',
        body: '',
        state: 'open',
        changed_lines: 3,
        created_at: now,
        owned: true,
        head_repository: 'fixture/project',
        base_repository: 'fixture/project'
      },
      observed_at: now,
      delivered_head: 'd'.repeat(40),
      external_head_movement: false,
      auto_merge: mergeEvidence('uncertain', {
        reason: 'The merge request outcome is unconfirmed',
        attempt_id: 'attempt-1',
        attempted_at: now
      })
    });
    await route.fulfill({ json: page });
  });
  await login(page);

  const stat = page.locator('#auto-merge-stat');
  await expect(stat).toContainText('Merge outcomes');
  await expect(stat).toContainText('03');
  await expect(stat).toContainText('3 waiting on checks or protections · checking now');
  await expect(page.getByLabel('Automatic merges')).toContainText('3 pull requests wait');
  await expect(page.getByLabel('Operating mode')).toContainText('Run once · settling merges');

  await openNavigation(page, 'Pull requests', !!isMobile);
  const delivered = page.getByRole('link', { name: /Explain the local development workflow/ });
  await expect(delivered).toContainText(
    'automatic merge: waiting — Waiting for checks to complete on the reviewed head'
  );
  await expect(page.getByRole('link', { name: /Record the first delivered change/ })).toContainText(
    'automatic merge: merged — squash merged by Octomus'
  );
  await expect(page.getByRole('link', { name: /Uncertain merge acknowledgement/ })).toContainText(
    'automatic merge: uncertain — The merge request outcome is unconfirmed'
  );
  await expect(page.getByRole('link', { name: /Adjust the retry backoff/ })).not.toContainText(
    'automatic merge'
  );
});

test('observed and refused merge outcomes keep actor provenance separate @responsive', async ({
  page,
  isMobile
}) => {
  await patchState(page, (snapshot) => {
    snapshot.delivery_mode = 'maintenance';
  });
  await page.route('**/api/prs?*', async (route) => {
    const response = await route.fetch();
    const page = await response.json();
    const base = (status: string, extra: Record<string, unknown> = {}) => ({
      task_id: 'task-reviewed',
      head: 'b'.repeat(40),
      comparison_base: 'a'.repeat(40),
      head_branch: 'octomus/task-reviewed',
      base_branch: 'main',
      policy_revision: 'synthetic-policy',
      authorized: true,
      footprint: null,
      status,
      reason: '',
      observed_at: now,
      attempt_id: null,
      attempted_at: null,
      merge_commit: null,
      result_source: null,
      ...extra
    });
    for (const observation of page.items) {
      if (observation.pr.number === 12) {
        observation.auto_merge = base('manual', {
          authorized: false,
          reason: 'Sensitive changes require a manual merge'
        });
      }
      if (observation.pr.number === 7) {
        observation.pr.state = 'merged';
        observation.auto_merge = base('merged', {
          reason: 'The pull request is merged on the remote',
          merge_commit: 'c'.repeat(40),
          result_source: 'observed'
        });
      }
    }
    await route.fulfill({ json: page });
  });
  await login(page);
  await openNavigation(page, 'Pull requests', !!isMobile);
  await expect(
    page.getByRole('link', { name: /Explain the local development workflow/ })
  ).toContainText('automatic merge: manual — Sensitive changes require a manual merge');
  await expect(page.getByRole('link', { name: /Record the first delivered change/ })).toContainText(
    'automatic merge: merged — merged on GitHub (actor not confirmed)'
  );
});

test('task detail and run evidence show the frozen maintenance assessment and footprint', async ({
  page
}) => {
  await page.route('**/api/tasks/task-reviewed', async (route) => {
    const response = await route.fetch();
    const task = await response.json();
    task.config.delivery_mode = 'maintenance';
    task.reviews[0].maintenance = {
      qualifies: true,
      manual_merge_required: false,
      reason: 'Fixes documented existing behavior without expanding scope.'
    };
    task.reviews[0].trusted_diff_complete = true;
    task.maintenance_footprint = {
      comparison_base: 'a'.repeat(40),
      revision: 'b'.repeat(40),
      changed_lines: 3,
      changed_files: 2,
      paths: ['README.md', 'docs/setup.md'],
      complete: true,
      manual_reasons: []
    };
    await route.fulfill({ json: task });
  });
  await login(page);
  await openNavigation(page, 'Task queue', false);
  await page.getByLabel('Search work').fill('documentation');
  await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText('maintenance', { exact: true })).toBeVisible();
  await expect(dialog.getByText('Maintenance footprint')).toBeVisible();
  await expect(dialog.getByText('3', { exact: true })).toBeVisible();
  await dialog.getByRole('tab', { name: /Reviews/ }).click();
  await expect(dialog.locator('.maintenance-assessment')).toContainText('Maintenance');
  await expect(dialog.locator('.maintenance-assessment')).toContainText(
    'Fixes documented existing behavior without expanding scope.'
  );
  await page.getByRole('button', { name: 'Close task details' }).click();
  await openNavigation(page, 'Overview', false);

  await page.route('**/api/cycles/cycle-1/evidence', async (route) => {
    await route.fulfill({
      json: runEvidence(
        {
          proposals: [
            {
              id: 'synthetic-proposal',
              title: 'Explain the local development workflow',
              target: 'main',
              tier: 'S',
              category: 'documentation',
              problem: 'Recorded problem',
              benefit: 'Recorded benefit',
              scope: 'Recorded scope',
              evidence: [],
              final_decision: 'accepted',
              final_reason: 'Recorded reason',
              reviewer_verdicts: [],
              linked_tasks: [
                taskEvidence('task-reviewed', {
                  delivery_mode: 'maintenance',
                  auto_merge: {
                    task_id: 'task-reviewed',
                    head: 'b'.repeat(40),
                    comparison_base: 'a'.repeat(40),
                    head_branch: 'octomus/task-reviewed',
                    base_branch: 'main',
                    policy_revision: 'synthetic-policy',
                    authorized: true,
                    footprint: null,
                    status: 'merged',
                    reason: 'Squash merged by Octomus',
                    observed_at: now,
                    attempt_id: 'attempt-1',
                    attempted_at: now,
                    merge_commit: 'c'.repeat(40),
                    result_source: 'confirmed'
                  },
                  maintenance_footprint: {
                    comparison_base: 'a'.repeat(40),
                    revision: 'b'.repeat(40),
                    changed_lines: 3,
                    changed_files: 2,
                    paths: ['README.md'],
                    complete: true,
                    manual_reasons: []
                  },
                  latest_review: {
                    rounds_recorded: 1,
                    clean: true,
                    clean_at_output_revision: true,
                    latest: {
                      session_id: 'review-session',
                      revision: 'b'.repeat(40),
                      comparison_base: 'a'.repeat(40),
                      created_at: '2026-01-01T00:00:00Z',
                      completed: true,
                      summary_present: true,
                      matches_output_revision: true,
                      findings: [],
                      maintenance: {
                        qualifies: true,
                        manual_merge_required: false,
                        reason: 'Documents existing behavior only.'
                      },
                      trusted_diff_complete: true
                    }
                  }
                })
              ],
              gaps: []
            }
          ]
        },
        { delivery_mode: 'maintenance' }
      )
    });
  });
  await page.getByRole('button', { name: 'Inspect run' }).click();
  const evidence = page.getByRole('dialog');
  await expect(evidence.getByText('maintenance', { exact: true }).first()).toBeVisible();
  await expect(evidence.getByText('Frozen footprint at the reviewed output')).toBeVisible();
  await expect(evidence.locator('.maintenance-assessment')).toContainText(
    'Documents existing behavior only.'
  );
  await expect(evidence.getByText(/Automatic merge outcome/)).toContainText(
    'Squash merged by Octomus'
  );
});
