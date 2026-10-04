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
/** Decimal gigabytes, as the storage limits are configured. */
export function gb(bytes: number): string {
  return (bytes / 1e9).toFixed(2);
}
/** Binary units, as the sandbox memory limit is declared. */
export function bytesLabel(bytes: number): string {
  const gib = bytes / 2 ** 30;
  return gib >= 1 ? `${Number(gib.toFixed(1))} GiB` : `${Math.round(bytes / 2 ** 20)} MiB`;
}
/** The first twelve characters of a commit or image digest. */
export function shortHash(value: string): string {
  return value.replace(/^sha256:/, '').slice(0, 12);
}
export function plural(count: number, noun: string, pluralNoun = `${noun}s`): string {
  return `${count} ${count === 1 ? noun : pluralNoun}`;
}
