/** Clipboard writes never throw into the UI; the caller announces the outcome as text. */
export async function copyText(value: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
}

export function copyMessage(label: string, copied: boolean): string {
  return copied ? `${label} copied.` : `${label} could not be copied in this browser context.`;
}
