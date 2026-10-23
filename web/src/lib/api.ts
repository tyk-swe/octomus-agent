import type {
  ControlAction,
  ControlResult,
  BaselineCheck,
  BaselineView,
  Backend,
  Config,
  DoctorResponse,
  Cycle,
  CycleAction,
  CycleMode,
  CycleSummary,
  Event,
  Model,
  Page,
  PRObservation,
  ProposalDetail,
  ProposalRow,
  RunEvidenceV1,
  RepositoriesView,
  RequestsView,
  Insights,
  OperatorRequest,
  RequestAction,
  SandboxSelfTest,
  SettingsView,
  Snapshot,
  Task,
  TaskAction,
  TaskRow
} from './types';

let token = '';
let repository = '';
let session = new AbortController();
let unauthorized: (() => void) | null = null;
export function setToken(value: string) {
  session.abort();
  session = new AbortController();
  token = value;
}
// The selected repository scopes every request except the fleet-level ones
// (the repository list itself and the instance ceiling). Empty selects the
// instance's primary repository, matching the service default.
export function setRepository(value: string) {
  repository = value;
}
export function getRepository() {
  return repository;
}
function scoped(path: string): string {
  if (!repository || path.startsWith('/repositories') || path.startsWith('/instance')) return path;
  return `${path}${path.includes('?') ? '&' : '?'}repository=${encodeURIComponent(repository)}`;
}
export function onUnauthorized(handler: () => void) {
  unauthorized = handler;
  return () => {
    unauthorized = null;
  };
}
export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public checkedRevision?: string,
    public code?: string
  ) {
    super(message);
  }
}
async function request<T>(
  path: string,
  method = 'GET',
  body?: unknown,
  signal?: AbortSignal
): Promise<T> {
  const requestSignal = AbortSignal.any([
    session.signal,
    AbortSignal.timeout(90000),
    ...(signal ? [signal] : [])
  ]);
  const response = await fetch(`/api${scoped(path)}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    ...(method !== 'GET' ? { body: JSON.stringify(body ?? {}) } : {}),
    signal: requestSignal
  });
  let text = '';
  let result: unknown;
  let parsed = true;
  try {
    text = await response.text();
    result = JSON.parse(text);
  } catch {
    parsed = false;
  }
  requestSignal.throwIfAborted();
  if (response.status === 401) unauthorized?.();
  if (!response.ok) {
    const failure = result as
      { error?: string; checked_revision?: string; code?: string } | null | undefined;
    throw new ApiError(
      failure?.error ??
        (parsed
          ? 'Request failed'
          : plainText(response, text) || `Service returned ${response.status}`),
      response.status,
      failure?.checked_revision,
      failure?.code
    );
  }
  if (!parsed)
    throw new ApiError(
      `Service returned an unreadable response (${response.status})`,
      response.status
    );
  return result as T;
}
function plainText(response: Response, text: string): string {
  if (!/^text\/plain\b/i.test(response.headers.get('Content-Type') ?? '')) return '';
  const message = text.trim();
  return message.length > 500 ? `${message.slice(0, 500)}…` : message;
}
const id = encodeURIComponent;
const query = (parameters?: URLSearchParams) => (parameters ? `?${parameters}` : '');
type Acknowledgement = { ok: boolean };

// Each operation owns its request and response pairing. The unchecked transport
// assertion is private; components cannot select a response type for any URL.
export const client = {
  state: (signal?: AbortSignal) => request<Snapshot>('/state', 'GET', undefined, signal),
  repositories: (signal?: AbortSignal) =>
    request<RepositoriesView>('/repositories', 'GET', undefined, signal),
  addRepository: (githubRepo: string) =>
    request<RepositoriesView>('/repositories', 'POST', { github_repo: githubRepo }),
  saveInstance: (maxSessionsPerDay: number) =>
    request<RepositoriesView>('/instance', 'PUT', { max_sessions_per_day: maxSessionsPerDay }),
  tasks: (parameters?: URLSearchParams, signal?: AbortSignal) =>
    request<Page<TaskRow>>(`/tasks${query(parameters)}`, 'GET', undefined, signal),
  cycles: (parameters?: URLSearchParams, signal?: AbortSignal) =>
    request<Page<CycleSummary>>(`/cycles${query(parameters)}`, 'GET', undefined, signal),
  proposals: (parameters?: URLSearchParams, signal?: AbortSignal) =>
    request<Page<ProposalRow>>(`/proposals${query(parameters)}`, 'GET', undefined, signal),
  prs: (parameters?: URLSearchParams, signal?: AbortSignal) =>
    request<Page<PRObservation>>(`/prs${query(parameters)}`, 'GET', undefined, signal),
  task: (task: string, signal?: AbortSignal) =>
    request<Task>(`/tasks/${id(task)}`, 'GET', undefined, signal),
  taskAction: (task: string, action: TaskAction) =>
    request<Acknowledgement>(`/tasks/${id(task)}/${id(action)}`, 'POST'),
  cycle: (cycle: string, signal?: AbortSignal) =>
    request<Cycle>(`/cycles/${id(cycle)}`, 'GET', undefined, signal),
  cycleAction: (cycle: string, action: CycleAction) =>
    request<Acknowledgement>(`/cycles/${id(cycle)}/${action}`, 'POST'),
  proposal: (cycle: string, proposal: string, signal?: AbortSignal) =>
    request<ProposalDetail>(`/proposals/${id(cycle)}/${id(proposal)}`, 'GET', undefined, signal),
  requests: (signal?: AbortSignal) => request<RequestsView>('/requests', 'GET', undefined, signal),
  createRequest: (body: { title: string; description: string; target: string }) =>
    request<OperatorRequest>('/requests', 'POST', body),
  requestAction: (requestID: string, action: RequestAction) =>
    request<Acknowledgement>(`/requests/${id(requestID)}/${id(action)}`, 'POST'),
  insights: (days: number, signal?: AbortSignal) =>
    request<Insights>(`/insights?days=${days}`, 'GET', undefined, signal),
  evidence: (cycle: string, signal?: AbortSignal) =>
    request<RunEvidenceV1>(`/cycles/${id(cycle)}/evidence`, 'GET', undefined, signal),
  events: (entity: string, signal?: AbortSignal) =>
    request<Event[]>(`/events?entity=${id(entity)}`, 'GET', undefined, signal),
  settings: (signal?: AbortSignal) => request<SettingsView>('/config', 'GET', undefined, signal),
  saveSettings: (revision: string, config: Partial<Config>) =>
    request<SettingsView>('/config', 'PUT', { expected_revision: revision, config }),
  baseline: (signal?: AbortSignal) =>
    request<BaselineView>('/baseline-checks/latest', 'GET', undefined, signal),
  startBaseline: (revision: string) =>
    request<BaselineCheck>('/baseline-checks', 'POST', { expected_revision: revision }),
  cancelBaseline: (check: string) =>
    request<Acknowledgement>(`/baseline-checks/${id(check)}/cancel`, 'POST'),
  control: (action: ControlAction) => request<ControlResult>(`/control/${action}`, 'POST'),
  selfTest: () => request<SandboxSelfTest>('/sandbox/self-test', 'POST'),
  catalog: (backend: Backend, binary: string, signal?: AbortSignal) =>
    request<Model[]>('/model-catalog', 'POST', { backend, binary }, signal),
  doctor: (mode: CycleMode, signal?: AbortSignal) =>
    request<DoctorResponse>(`/doctor?mode=${mode}`, 'POST', undefined, signal)
};
