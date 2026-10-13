import { expect, test } from '@playwright/test';
import {
  configurationDraft,
  configurationPatch,
  discardConfigurationDraft,
  draftCommands,
  draftDirty,
  replaceDraftField
} from '../src/lib/configurationDraft';
import type { Config, SettingsView, TransformedField } from '../src/lib/types';

function saved(config: Partial<Config>, transformed_fields: TransformedField[] = []): SettingsView {
  return {
    revision: 'saved-revision',
    transformed_fields,
    config: { verification_commands: [], auto_merge_excluded_paths: [], ...config } as Config
  };
}

test('unrelated edits preserve canonical multiline commands and untouched excluded paths', () => {
  const commands = ['  go test ./...  ', "printf 'first\\nsecond'\nprintf 'third'", ''];
  const excluded = ['  vendor/**  ', 'path\nwith newline'];
  const draft = configurationDraft(
    saved({
      repository: '/saved',
      verification_commands: commands,
      auto_merge_excluded_paths: excluded
    })
  );
  draft.values.repository = '/edited';
  expect(configurationPatch(draft)).toEqual({ repository: '/edited' });
  expect(draftCommands(draft)).toEqual(commands);
  draft.commands = '  go test ./internal/... \n\n npm test ';
  expect(configurationPatch(draft)).toEqual({
    repository: '/edited',
    verification_commands: ['go test ./internal/...', 'npm test']
  });
  const discarded = discardConfigurationDraft(draft);
  expect(draftDirty(discarded)).toBe(false);
  expect(draftCommands(discarded)).toEqual(commands);
  expect(discarded.values.auto_merge_excluded_paths).toEqual(excluded);
  expect(discarded.revision).toBe('saved-revision');
});

test('redacted and shortened fields cannot enter a patch before explicit replacement', () => {
  const draft = configurationDraft(
    saved(
      {
        repository: '[redacted]',
        verification_commands: ['shortened preview…'],
        runner_storage_paths: { codex: '[redacted]', opencode: '/canonical/opencode' }
      },
      [
        { field: 'repository', kinds: ['redacted'], paths: [['repository']] },
        {
          field: 'verification_commands',
          kinds: ['shortened'],
          paths: [['verification_commands', 0]]
        },
        {
          field: 'runner_storage_paths',
          kinds: ['redacted'],
          paths: [['runner_storage_paths', 'codex']]
        }
      ]
    )
  );
  draft.values.repository = '/accidental';
  draft.commands = 'accidental replacement';
  expect(configurationPatch(draft)).toEqual({});
  replaceDraftField(draft, 'repository');
  replaceDraftField(draft, 'verification_commands');
  replaceDraftField(draft, 'runner_storage_paths');
  expect(draft.values.repository).toBe('');
  expect(draft.commands).toBe('');
  draft.values.repository = '/explicit';
  draft.commands = 'new command';
  expect(configurationPatch(draft)).toEqual({
    repository: '/explicit',
    verification_commands: ['new command'],
    runner_storage_paths: { opencode: '/canonical/opencode' }
  });
  const local = discardConfigurationDraft(draft);
  expect(local.values.repository).toBe('[redacted]');
  expect(configurationPatch(local)).toEqual({});
  const reloaded = configurationDraft({
    ...saved({ repository: '/server changed' }),
    revision: 'new revision'
  });
  expect(reloaded.values.repository).toBe('/server changed');
  expect(reloaded.revision).toBe('new revision');
});
