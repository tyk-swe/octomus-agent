import type { RunEvidenceV1 } from './types';

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
    public checkedRevision?: string
  ) {
    super(message);
  }
}
export async function api<T>(
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
    const failure = result as { error?: string; checked_revision?: string } | null | undefined;
    throw new ApiError(
      failure?.error ??
        (parsed
          ? 'Request failed'
          : plainText(response, text) || `Service returned ${response.status}`),
      response.status,
      failure?.checked_revision
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
export function fetchEvidence(cycle: string, signal?: AbortSignal) {
  return api<RunEvidenceV1>(
    `/cycles/${encodeURIComponent(cycle)}/evidence`,
    'GET',
    undefined,
    signal
  );
}
