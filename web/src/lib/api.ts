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
    public status: number,
    /** Canonical configuration revision the checked result applies to. */
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
  let result: unknown;
  let parsed = true;
  try {
    result = JSON.parse(await response.text());
  } catch {
    // An unreadable body, such as a proxy's HTML page; it is never returned as data.
    parsed = false;
  }
  // Includes the body read: an old session's response cannot populate a new session.
  requestSignal.throwIfAborted();
  if (response.status === 401) unauthorized?.();
  if (!response.ok) {
    const failure = result as { error?: string; checked_revision?: string } | null | undefined;
    throw new ApiError(
      failure?.error ?? (parsed ? 'Request failed' : `Service returned ${response.status}`),
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
export function relative(value: string) {
  const seconds = Math.max(0, (Date.now() - new Date(value).getTime()) / 1000);
  if (!Number.isFinite(seconds)) return '';
  return seconds < 60
    ? 'just now'
    : seconds < 3600
      ? `${Math.floor(seconds / 60)}m ago`
      : seconds < 86400
        ? `${Math.floor(seconds / 3600)}h ago`
        : `${Math.floor(seconds / 86400)}d ago`;
}
/** Gigabytes with the two decimals every storage figure is shown with. */
export function gb(bytes: number): string {
  return (bytes / 1e9).toFixed(2);
}
/** Wall-clock time of a dashboard refresh, e.g. `14:03`. */
export function clockTime(date = new Date()): string {
  return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}
export function safeUrl(value: string | null): string {
  try {
    const url = new URL(value ?? '');
    return url.protocol === 'https:' && url.hostname === 'github.com' ? url.href : '#';
  } catch {
    return '#';
  }
}
