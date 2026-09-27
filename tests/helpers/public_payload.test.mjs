// Unit tests for the private-payload gate's JSON parser. tests/evidence_snapshot.py
// runs the documented gate end to end against a real `--export-run` candidate.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseUniqueJson } from './public_payload.mjs';

test('JSON parsing rejects duplicate decoded keys at every object depth', () => {
  for (const source of [
    '{"evidence":{"transcript":"synthetic private text"},"evidence":{}}',
    '{"evidence":{},"\\u0065vidence":{}}',
    '{"nested":[{"key":1,"key":1}]}',
    '{"nested":{"deeper":{"key":null,"key":false}}}',
    '{"":0,"":1}',
    '{"__proto__":0,"__proto__":1}',
    '{"a\\\\b":0,"a\\u005cb":1}',
    '{"a\\\"b":0,"a\\u0022b":1}',
    '{"😀":0,"\\ud83d\\ude00":1}'
  ])
    assert.throws(() => parseUniqueJson(source), /Duplicate JSON object key/);
});

test('unique JSON keeps native syntax and values, without confusing text or sibling keys', () => {
  for (const value of [
    null,
    true,
    12.5,
    'text with "quotes", \\ escapes and {"key":1,"key":2}',
    [{ key: 1 }, { key: 2 }],
    { key: { key: 1 }, array: [{ key: 2 }], text: '{}[]: \\" : \\u0061' },
    { '': 1, constructor: 2, 'a"b': 3, 'a\\b': 4, '😀': 5 }
  ]) {
    const source = JSON.stringify(value, null, 2);
    assert.deepEqual(parseUniqueJson(source), JSON.parse(source));
  }
  const source = '{"__proto__":1,"a":0,"A":1,"é":2,"e\\u0301":3}';
  assert.deepEqual(parseUniqueJson(source), JSON.parse(source));
  for (const malformed of ['{"a":1,}', '{"a":1} {}', '{"a":"\\x61"}', '{"a":"unterminated}'])
    assert.throws(() => parseUniqueJson(malformed), SyntaxError);
});
