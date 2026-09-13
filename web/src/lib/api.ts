let token = '';
let session = new AbortController();
let unauthorized: (() => void) | null = null;
export function setToken(value: string) {
  session.abort();
  session = new AbortController();
  token = value;
}
/** The dashboard clears all private views when any current-session request gets a 401. */
export function onUnauthorized(handler: () => void) {
  unauthorized = handler;
  return () => {
    unauthorized = null;
  };
}
export class ApiError extends Error {
  constructor(
    message: string,
    public status: number
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
  const result = await response
    .json()
    .catch(() => ({ error: `Service returned ${response.status}` }));
  // Includes JSON parsing: an old session's response cannot populate a new session.
  requestSignal.throwIfAborted();
  if (response.status === 401) unauthorized?.();
  if (!response.ok) throw new ApiError(result.error ?? 'Request failed', response.status);
  return result as T;
}
export function relative(value: string) {
  const seconds = Math.max(0, (Date.now() - new Date(value).getTime()) / 1000);
  return seconds < 60
    ? 'just now'
    : seconds < 3600
      ? `${Math.floor(seconds / 60)}m ago`
      : seconds < 86400
        ? `${Math.floor(seconds / 3600)}h ago`
        : `${Math.floor(seconds / 86400)}d ago`;
}
export function safeUrl(value: string | null): string {
  try {
    const url = new URL(value ?? '');
    return url.protocol === 'https:' && url.hostname === 'github.com' ? url.href : '#';
  } catch {
    return '#';
  }
}
