import type { Config, ModelCatalog, Route } from './types';

export function astraCatalog(binary: string, catalog?: ModelCatalog) {
  const blocked = (problem: string) => ({ problem, efforts: [] as string[] });
  if (!catalog) return blocked('Load Codex models for the current Codex executable first.');
  if (catalog.binary !== binary)
    return blocked('The Codex catalog is stale. Load Codex models for the current executable.');
  if (!catalog.loaded)
    return blocked('The Codex catalog failed to load. Load Codex models successfully to continue.');
  const model = catalog.models.find(
    (entry) => entry.backend === 'codex' && entry.provider == null && entry.model === 'gpt-6-astra'
  );
  if (!model) return blocked('The Codex catalog has no exact gpt-6-astra entry.');
  if (!model.available)
    return blocked(`gpt-6-astra is unavailable. ${model.unavailable_reason ?? ''}`.trim());
  if (
    !Array.isArray(model.efforts) ||
    !model.efforts.length ||
    model.efforts.some(
      (effort) =>
        typeof effort !== 'string' ||
        !effort ||
        effort.trim() !== effort ||
        new TextEncoder().encode(effort).length > 20 ||
        /[\p{Cc}]/u.test(effort)
    )
  )
    return blocked('gpt-6-astra has no compatible supported effort list in the Codex catalog.');
  return { problem: '', efforts: model.efforts };
}

export function applyAstraRehearsal(
  config: Config,
  catalog: ModelCatalog | undefined,
  effort: string
) {
  const availability = astraCatalog(config.codex_binary, catalog);
  if (availability.problem || !availability.efforts.includes(effort)) return null;
  const route = (): Route => ({ backend: 'codex', model: 'gpt-6-astra', effort });
  return {
    ...config,
    roles: {
      ...config.roles,
      orchestrator: route(),
      discovery: route(),
      proposal_reviewer: route(),
      code_reviewer: route()
    },
    tiers: { ...config.tiers, XS: route(), S: route(), M: route(), L: route(), XL: route() },
    repair_route: route(),
    discovery_agents: 9,
    execution_concurrency: 1,
    max_tasks_per_cycle: 1,
    cycle_interval_seconds: 21600
  };
}
