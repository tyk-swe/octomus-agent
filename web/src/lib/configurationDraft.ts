import { parseCommands } from './setup';
import type { Config, SettingsView, TransformedField } from './types';

// A settings response is a display snapshot. Only changed, losslessly represented
// fields or explicitly replaced previews may become an authoritative patch.
export type ConfigurationDraft = {
  revision: string;
  saved: Config;
  values: Config;
  transformed: TransformedField[];
  replaced: Record<string, boolean>;
  commands: string;
  excluded: string;
};

// Settings are JSON data. JSON cloning also accepts Svelte's reactive proxies,
// which structuredClone cannot copy during local discard.
function copy<T>(value: T): T {
  return JSON.parse(JSON.stringify(value));
}

export function configurationDraft(view: SettingsView): ConfigurationDraft {
  return {
    revision: view.revision,
    saved: copy(view.config),
    values: copy(view.config),
    transformed: copy(view.transformed_fields),
    replaced: {},
    commands: view.config.verification_commands.join('\n'),
    excluded: view.config.auto_merge_excluded_paths.join('\n')
  };
}

export function draftCommands(draft: ConfigurationDraft): string[] {
  return draft.commands !== draft.saved.verification_commands.join('\n') ||
    draft.replaced.verification_commands
    ? parseCommands(draft.commands)
    : draft.saved.verification_commands;
}

export function draftExcluded(draft: ConfigurationDraft): string[] {
  return draft.excluded !== draft.saved.auto_merge_excluded_paths.join('\n') ||
    draft.replaced.auto_merge_excluded_paths
    ? parseCommands(draft.excluded)
    : draft.saved.auto_merge_excluded_paths;
}

export function draftDirty(draft: ConfigurationDraft): boolean {
  return (
    JSON.stringify(draft.values) !== JSON.stringify(draft.saved) ||
    draft.commands !== draft.saved.verification_commands.join('\n') ||
    draft.excluded !== draft.saved.auto_merge_excluded_paths.join('\n') ||
    Object.values(draft.replaced).some(Boolean)
  );
}

export function configurationPatch(draft: ConfigurationDraft): Partial<Config> {
  const values: Config = {
    ...draft.values,
    verification_commands: draftCommands(draft),
    auto_merge_excluded_paths: draftExcluded(draft)
  };
  const locked = new Set(
    draft.transformed.filter((field) => !draft.replaced[field.field]).map((field) => field.field)
  );
  return Object.fromEntries(
    (Object.keys(values) as (keyof Config)[])
      .filter(
        (key) =>
          !locked.has(key) &&
          (draft.replaced[key] || JSON.stringify(values[key]) !== JSON.stringify(draft.saved[key]))
      )
      .map((key) => [key, values[key]])
  );
}

export function replaceDraftField(draft: ConfigurationDraft, field: string) {
  for (const path of draft.transformed.find((entry) => entry.field === field)?.paths ?? []) {
    let node: unknown = draft.values;
    for (const segment of path.slice(0, -1)) {
      node = (node as Record<string, unknown>)?.[segment as string];
      if (node == null) break;
    }
    const leaf = path.at(-1);
    if (leaf === undefined || node == null) continue;
    if (Array.isArray(node)) node[leaf as number] = '';
    else if (path[0] === 'runner_storage_paths')
      delete (node as Record<string, unknown>)[leaf as string];
    else (node as Record<string, unknown>)[leaf as string] = '';
  }
  // Category checkboxes cannot represent an empty selection left behind by a
  // cleared preview. Drop those slots while retaining visible saved choices.
  if (field === 'categories') draft.values.categories = draft.values.categories.filter(Boolean);
  if (field === 'verification_commands') draft.commands = '';
  if (field === 'auto_merge_excluded_paths') draft.excluded = '';
  draft.replaced[field] = true;
}

// Local discard restores this tab's saved snapshot; a server reload is a separate
// request and may accept a different revision.
export function discardConfigurationDraft(draft: ConfigurationDraft): ConfigurationDraft {
  return configurationDraft({
    config: draft.saved,
    revision: draft.revision,
    transformed_fields: draft.transformed
  });
}
