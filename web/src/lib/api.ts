import type {
  ControlResult,
  BaselineCheck,
  BaselineView,
  Backend,
  Config,
  DoctorResponse,
  Cycle,
  CycleMode,
  CycleSummary,
  Event,
  Model,
  Page,
  PRObservation,
  ProposalDetail,
  ProposalRow,
  RunEvidenceV1,
  SandboxSelfTest,
  SettingsView,
  Snapshot,
  Task,
  TaskRow
} from './types';
import type { ControlAction } from './setup';

let token = '';
let session = new AbortController();
let unauthorized: (() => void) | null = null;
export function setToken(value: string) {
  session.abort();
  session = new AbortController();
  token = value;
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
  const response = await fetch(`/api${path}`, {
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
  taskAction: (task: string, action: string) =>
    request<Acknowledgement>(`/tasks/${id(task)}/${id(action)}`, 'POST'),
  cycle: (cycle: string, signal?: AbortSignal) =>
    request<Cycle>(`/cycles/${id(cycle)}`, 'GET', undefined, signal),
  cycleAction: (cycle: string, action: 'archive' | 'discard') =>
    request<Acknowledgement>(`/cycles/${id(cycle)}/${action}`, 'POST'),
  proposal: (cycle: string, proposal: string, signal?: AbortSignal) =>
    request<ProposalDetail>(`/proposals/${id(cycle)}/${id(proposal)}`, 'GET', undefined, signal),
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
  catalog: (backend: Backend, binary: string) =>
    request<Model[]>('/model-catalog', 'POST', { backend, binary }),
  doctor: (mode: CycleMode) => request<DoctorResponse>(`/doctor?mode=${mode}`, 'POST')
};
