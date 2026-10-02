import { expect, type Page } from '@playwright/test';
import { login, openNavigation, test } from './synthetic';

type ClipboardHarness = {
  copies: { value: string; finish: (success: boolean) => void }[];
};

async function controlledClipboard(page: Page) {
  await page.addInitScript(() => {
    const harness: ClipboardHarness = { copies: [] };
    (window as unknown as { clipboardHarness: ClipboardHarness }).clipboardHarness = harness;
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText(value: string) {
          return new Promise<void>((resolve, reject) => {
            harness.copies.push({
              value,
              finish(success) {
                if (success) resolve();
                else reject(new DOMException('Synthetic clipboard failure', 'NotAllowedError'));
              }
            });
          });
        }
      }
    });
  });
}

async function finishCopy(page: Page, index: number, success: boolean) {
  await page.evaluate(
    ({ index, success }) => {
      (window as unknown as { clipboardHarness: ClipboardHarness }).clipboardHarness.copies[
        index
      ].finish(success);
    },
    { index, success }
  );
}

const panels = [
  {
    kind: 'run',
    older: 'Copy cycle ID',
    newer: 'Copy full grounding revision',
    success: 'Grounding revision copied.',
    failure: 'Grounding revision could not be copied in this browser context.',
    fallback: /^Private operator export of saved records\.$/,
    close: 'Close run evidence'
  },
  {
    kind: 'task',
    older: 'Copy full source revision',
    newer: 'Copy full output commit',
    success: 'Output commit copied.',
    failure: 'Output commit could not be copied in this browser context.',
    fallback: /^Created /,
    close: 'Close task details'
  }
];

async function openPanel(page: Page, kind: string, mobile: boolean) {
  if (kind === 'run') await page.getByRole('button', { name: 'Inspect run', exact: true }).click();
  else {
    await openNavigation(page, 'Task queue', mobile);
    await page.getByRole('button', { name: /Explain the local development workflow/ }).click();
  }
  await expect(page.getByRole('dialog')).toBeVisible();
}

for (const panel of panels) {
  test(`${panel.kind} copy feedback belongs to the latest request for its full expiry`, async ({
    page,
    isMobile
  }) => {
    await controlledClipboard(page);
    await page.clock.install();
    await login(page);
    await openPanel(page, panel.kind, !!isMobile);
    await expect(page.getByRole('button', { name: panel.newer, exact: true })).toBeVisible();
    await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
    const feedback = page.locator('.dialog-footer .muted');
    await page.getByRole('button', { name: panel.older, exact: true }).click();
    await page.getByRole('button', { name: panel.newer, exact: true }).click();
    await expect(feedback).toHaveText(panel.fallback);
    expect(
      await page.evaluate(
        () =>
          (window as unknown as { clipboardHarness: ClipboardHarness }).clipboardHarness.copies
            .length
      )
    ).toBe(2);
    await finishCopy(page, 1, true);
    await expect(feedback).toHaveText(panel.success);
    await finishCopy(page, 0, false);
    await expect(feedback).toHaveText(panel.success);
    await page.clock.runFor(3999);
    await expect(feedback).toHaveText(panel.success);
    await page.clock.runFor(1);
    await expect(feedback).toHaveText(panel.fallback);
    await page.getByRole('button', { name: panel.newer, exact: true }).click();
    await finishCopy(page, 2, false);
    await expect(feedback).toHaveText(panel.failure);
  });

  test(`${panel.kind} pending copy is discarded when its panel closes and reopens`, async ({
    page,
    isMobile
  }) => {
    const errors: string[] = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await controlledClipboard(page);
    await login(page);
    await openPanel(page, panel.kind, !!isMobile);
    await page.getByRole('button', { name: panel.older, exact: true }).click();
    await page.getByRole('button', { name: panel.close }).click();
    await finishCopy(page, 0, false);
    await openPanel(page, panel.kind, !!isMobile);
    const feedback = page.locator('.dialog-footer .muted');
    await expect(feedback).toHaveText(panel.fallback);
    await page.getByRole('button', { name: panel.newer, exact: true }).click();
    await finishCopy(page, 1, true);
    await expect(feedback).toHaveText(panel.success);
    expect(errors).toEqual([]);
  });
}
