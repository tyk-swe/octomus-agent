/** Parse JSON without allowing earlier object members to disappear from inspection. */
/** @param {string} source */
export function parseUniqueJson(source) {
  // Let the native parser own the JSON grammar. Then scan the original text, not the
  // parsed object: a reviver cannot see members overwritten by duplicate names.
  const value = JSON.parse(source);
  const objects = [];
  let stringToken;
  // Whole strings are tokens, so braces/colons and escaped quotes inside text cannot
  // affect object scope. Arrays introduce no member-name scope of their own.
  for (const [token] of source.matchAll(/"(?:[^"\\]|\\.)*"|[{}:]/g)) {
    if (token === '{') objects.push(new Set());
    else if (token === '}') objects.pop();
    else if (token === ':') {
      // JSON.parse above accepted the text, so a string key always sits
      // immediately before each colon and the object stack is never empty.
      // Decode escapes so "evidence" and "\\u0065vidence" are identical.
      const key = JSON.parse(/** @type {string} */ (stringToken));
      const names = /** @type {Set<string>} */ (objects.at(-1));
      if (names.has(key)) throw new Error('Duplicate JSON object key');
      names.add(key);
    } else stringToken = token;
  }
  return value;
}
