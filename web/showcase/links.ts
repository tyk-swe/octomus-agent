/** Only literal public GitHub PR URLs become links; rejected values remain visible text. */
export function publicPrUrl(value: string | null): string | null {
  return value &&
    value.trim() === value &&
    /^https:\/\/github\.com\/[A-Za-z0-9_-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9][0-9]*$/.test(value)
    ? value
    : null;
}
