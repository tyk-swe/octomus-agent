import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, mkdtempSync, writeFileSync, existsSync, rmSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve } from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { publicPayload, publicApproval } from '../showcase/contract.mjs';
import { parseUniqueJson } from '../scripts/public-json.mjs';

const example = JSON.parse(
  readFileSync(new URL('../showcase/synthetic.public.json', import.meta.url))
);
const fresh = () => structuredClone(example);
const hash = (bytes) => createHash('sha256').update(bytes).digest('hex');
const approval = (bytes) => ({
  approval_schema_version: 1,
  owner_reviewed: true,
  payload_sha256: hash(bytes),
  approval_reference: 'SYNTHETIC TEST ONLY — no owner approval granted'
});

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

test('projection retains all adverse evidence, zero/multiple matches and duplicate identities', () => {
  const p = fresh();
  p.evidence.proposals[2].linked_tasks[1].id = p.evidence.proposals[2].linked_tasks[0].id;
  assert.deepEqual(publicPayload(p, 'fixture'), p);
  assert.equal(p.evidence.proposals[1].linked_tasks.length, 0);
  assert.equal(p.evidence.proposals[2].linked_tasks.length, 2);
});
test('unknown free-form statuses and decisions remain verbatim; normative states are rejected', () => {
  const p = fresh();
  p.evidence.cycle.status = p.evidence.cycle.planning.status = 'future-status';
  p.evidence.proposals[1].final_decision = 'future-decision';
  p.evidence.cycle.planning.decisions = { accepted: 2, rejected: 1, 'future-decision': 1 };
  p.evidence.proposals[0].linked_tasks[0].sessions[0].status = 'future-session-status';
  assert.deepEqual(publicPayload(p, 'fixture'), p);
  for (const change of [
    (p) => (p.evidence.proposals[0].linked_tasks[0].status = 'merged'),
    (p) => (p.evidence.proposals[0].reviewer_verdicts[0].state = 'future-state'),
    (p) => (p.evidence.proposals[0].linked_tasks[0].required_commands.commands[0].state = 'green')
  ]) {
    const value = fresh();
    change(value);
    assert.throws(() => publicPayload(value, 'fixture'));
  }
});
test('malformed, unsupported and unallowlisted data fail closed', () => {
  for (const value of [null, [], {}, example.evidence, { ...fresh(), public_schema_version: 2 }])
    assert.throws(() => publicPayload(value, 'fixture'));
  for (const change of [
    (p) => (p.evidence.schema_version = 2),
    (p) => (p.evidence.review_required_before_sharing = false),
    (p) => (p.evidence.review_requirement = 'Approved!'),
    (p) => p.evidence.limitations.pop(),
    (p) => (p.evidence.proposals[0].prompt = 'private prompt'),
    (p) => (p.evidence.proposals[0].linked_tasks[0].sessions[0].transcript = 'private transcript'),
    (p) => (p.evidence.gaps = 'missing array'),
    (p) => (p.evidence.cycle.number = -1),
    (p) => delete p.evidence.proposals[0].reviewer_verdicts,
    (p) =>
      (p.evidence.proposals[0].linked_tasks[0].required_commands.commands[0].latest_success =
        'true')
  ]) {
    const value = fresh();
    change(value);
    assert.throws(() => publicPayload(value, 'fixture'));
  }
});
test('contradictory summary facts, joins, revision claims and reviewer states are rejected', () => {
  for (const change of [
    (p) => (p.evidence.cycle.planning.status = 'failed'),
    (p) => (p.evidence.cycle.planning.planning_finished = false),
    (p) => (p.evidence.cycle.planning.proposal_count = 0),
    (p) => (p.evidence.cycle.planning.decisions.accepted = 99),
    (p) => (p.evidence.cycle.planning.creates_execution_queue = false),
    (p) => p.evidence.proposals[0].reviewer_verdicts.reverse(),
    (p) => p.evidence.proposals[0].reviewer_verdicts.pop(),
    (p) => (p.evidence.proposals[0].reviewer_verdicts[0].decision = null),
    (p) => (p.evidence.proposals[0].linked_tasks[0].cycle_id = 'other-cycle'),
    (p) => (p.evidence.proposals[0].linked_tasks[0].proposal_id = 'other-proposal'),
    (p) => (p.evidence.proposals[0].linked_tasks[0].latest_review.rounds_recorded = 0),
    (p) => (p.evidence.proposals[0].linked_tasks[0].latest_review.latest = null),
    (p) => (p.evidence.proposals[0].linked_tasks[0].latest_review.latest.completed = false),
    (p) => (p.evidence.proposals[0].linked_tasks[0].latest_review.latest.summary_present = false),
    (p) => (p.evidence.proposals[0].linked_tasks[0].latest_review.latest.revision = 'other'),
    (p) => (p.evidence.proposals[0].linked_tasks[0].latest_review.clean_at_output_revision = false),
    (p) => (p.evidence.proposals[0].linked_tasks[0].required_commands.state = 'not_configured'),
    (p) =>
      (p.evidence.proposals[0].linked_tasks[0].required_commands.all_passed_at_output_revision = false),
    (p) =>
      (p.evidence.proposals[0].linked_tasks[0].required_commands.commands[0].latest_success = false),
    (p) =>
      (p.evidence.proposals[0].linked_tasks[0].required_commands.commands[0].results_recorded = 0),
    (p) =>
      (p.evidence.proposals[0].linked_tasks[0].required_commands.commands[0].latest_revision =
        'other')
  ]) {
    const value = fresh();
    change(value);
    assert.throws(() => publicPayload(value, 'fixture'));
  }
});
test('audit, missing output, no reviews, unconfigured and incomplete checks keep RunEvidenceV1 semantics', () => {
  const p = fresh();
  p.evidence.cycle.mode = 'audit';
  p.evidence.cycle.planning.creates_execution_queue = false;
  const t = p.evidence.proposals[0].linked_tasks[0];
  t.revisions.output = null;
  t.latest_review = {
    rounds_recorded: 0,
    latest: null,
    clean: false,
    clean_at_output_revision: false
  };
  t.required_commands = {
    state: 'not_configured',
    commands: [],
    all_passed_at_output_revision: false
  };
  assert.deepEqual(publicPayload(p, 'fixture'), p);
  t.required_commands = {
    state: 'recorded',
    all_passed_at_output_revision: false,
    commands: [
      {
        command: 'synthetic missing',
        state: 'no_result',
        results_recorded: 0,
        latest_success: null,
        latest_revision: null,
        latest_created_at: null,
        matches_output_revision: null
      },
      {
        command: 'synthetic stale',
        state: 'passed_at_other_revision',
        results_recorded: 1,
        latest_success: true,
        latest_revision: 'other',
        latest_created_at: 'synthetic date',
        matches_output_revision: null
      }
    ]
  };
  assert.deepEqual(publicPayload(p, 'fixture'), p);
});
test('approval binds exact bytes, including whitespace, and never self-approves', () => {
  const bytes = JSON.stringify(fresh());
  const a = approval(bytes);
  assert.deepEqual(publicApproval(a, hash(bytes)), a);
  for (const bad of [
    null,
    {},
    { ...a, owner_reviewed: false },
    { ...a, approval_reference: ' ' },
    { ...a, payload_sha256: '0'.repeat(64) }
  ])
    assert.throws(() => publicApproval(bad, hash(bytes)));
  assert.throws(() => publicApproval(a, hash(bytes + '\n')));
});
test('real CLI build modes, exact payload output, absent/mismatched approval and no fallback', () => {
  const dir = mkdtempSync(resolve(tmpdir(), 'octomus-showcase-test-'));
  const input = resolve(dir, 'public.json'),
    owner = resolve(dir, 'approval.json');
  const out = resolve('../dist/showcase');
  const run = (...args) =>
    spawnSync(process.execPath, ['scripts/build-showcase.mjs', ...args], { encoding: 'utf8' });
  try {
    writeFileSync(input, JSON.stringify(fresh()));
    let result = run('--mode', 'fixture', '--input', input);
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(readFileSync(resolve(out, 'public-run.json')), readFileSync(input));
    assert.ok(existsSync(resolve(out, 'index.html')));
    // The original bytes are downloadable, so even overwritten private members
    // must be rejected before a build. Exercise both plain and escaped names.
    for (const mode of ['fixture', 'recorded']) {
      const p = fresh();
      p.mode = mode;
      const clean = JSON.stringify(p);
      for (const source of [
        '{"evidence":{"transcript":"synthetic private text"},' + clean.slice(1),
        '{"\\u0065vidence":{"transcript":"synthetic private text"},' + clean.slice(1),
        clean.replace('"planning":{', '"planning":{"status":"synthetic hidden value",'),
        clean.replace('"requested_route":{', '"requested_route":{"model":"synthetic hidden value",')
      ]) {
        writeFileSync(input, source);
        // Even an exact synthetic approval must not authorize duplicate-key bytes.
        writeFileSync(owner, JSON.stringify(approval(source)));
        const args = ['--mode', mode, '--input', input];
        if (mode === 'recorded') args.push('--approval', owner);
        const rejected = run(...args);
        assert.notEqual(rejected.status, 0);
        assert.match(rejected.stderr, /Duplicate JSON object key/);
        assert.equal(existsSync(out), false);
      }
    }
    writeFileSync(input, JSON.stringify(fresh()));
    const badInputs = [
      [],
      ['--mode', 'recorded', '--input', input],
      ['--mode', 'fixture', '--input', dir],
      ['--mode', 'fixture', '--input', 'https://example.com/data.json'],
      ['--mode', 'fixture', '--input', input, '--approval', owner]
    ];
    for (const args of badInputs) {
      assert.notEqual(run(...args).status, 0);
      assert.equal(existsSync(out), false);
    }
    writeFileSync(input, '{malformed');
    assert.notEqual(run('--mode', 'fixture', '--input', input).status, 0);
    const p = fresh();
    p.mode = 'recorded';
    const bytes = JSON.stringify(p);
    writeFileSync(input, bytes);
    assert.notEqual(run('--mode', 'recorded', '--input', input).status, 0);
    writeFileSync(owner, JSON.stringify(approval(bytes + '\n')));
    assert.notEqual(run('--mode', 'recorded', '--input', input, '--approval', owner).status, 0);
    writeFileSync(owner, JSON.stringify(approval(bytes)));
    const validApproval = readFileSync(owner, 'utf8');
    for (const member of ['"owner_reviewed":false', '"\\u006fwner_reviewed":false']) {
      writeFileSync(owner, `{${member},${validApproval.slice(1)}`);
      const rejected = run('--mode', 'recorded', '--input', input, '--approval', owner);
      assert.notEqual(rejected.status, 0);
      assert.match(rejected.stderr, /Duplicate JSON object key/);
      assert.equal(existsSync(out), false);
    }
    writeFileSync(owner, validApproval);
    result = run('--mode', 'recorded', '--input', input, '--approval', owner);
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(readFileSync(resolve(out, 'public-run.json')), readFileSync(input));
    assert.deepEqual(readdirSync(out).sort(), [
      'approval.json',
      'assets',
      'index.html',
      'public-run.json'
    ]);
    assert.equal(
      readdirSync(resolve(out, 'assets')).some((name) => name.endsWith('.map')),
      false
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
    rmSync(out, { recursive: true, force: true });
  }
});
