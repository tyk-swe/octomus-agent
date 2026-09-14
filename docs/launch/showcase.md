# Explore a run: public static showcase

The showcase is a standalone Svelte/Vite build of recorded `RunEvidenceV1`
evidence. It requires no Rust service, account, token, model, database or live GitHub
request. It does not mount `RunEvidence.svelte`. It shares only the pure evidence/route
labels and `EvidenceFact` / `EvidenceText` presentation components with the dashboard.
No framework or runtime dependency was added.

## Exact local commands

From the repository root, using Node 22.12+ and the locked dependencies:

```sh
npm ci --prefix web
npm run showcase --prefix web -- --mode fixture --input showcase/synthetic.public.json
python3 -m http.server 4307 --bind 127.0.0.1 --directory dist
```

Open `http://127.0.0.1:4307/showcase/`. This is an ordinary static HTTP server. The
showcase also works at a static host's root or subdirectory; assets use relative URLs,
and proposal/task selection lives in the fragment. Refresh and browser history need no
server rewrite. `#proposal=2&task=0` selects the third proposal's first task *record*.
Indices preserve duplicate identities. Multiple task matches have no default selection;
an absent fragment selects the first proposal, and a sole task match is displayed.
Invalid selections remain explicitly unavailable, without substituting another record.

The command creates only `dist/showcase/`, separately from `web/build/` and the Rust
binary. It never copies `web/static`, an operator export, a repository directory, a state
database or a launch report. There is no default input or mode. Invalid invocations fail
with a nonzero exit and remove `dist/showcase/`, including any older build. This prevents
a stale output from being mistaken for the requested build. The normal dashboard build
continues to use its existing command and destination.

Input paths are resolved from the npm script's working directory (`web/` when using
`--prefix web`). Use absolute paths for owner-supplied files. Inputs must be explicit
local regular JSON files of at most 5 MB; URLs and directories are refused.
Both payload and approval JSON reject duplicate object keys at every depth, including
escape-equivalent names such as `"evidence"` and `"\u0065vidence"`. No overwritten member
may disappear from validation while remaining in the downloadable bytes. Repeated keys
in separate objects are allowed. This does not change the exact-byte hash binding.

## Public input contract

A deliberately prepared public JSON wrapper is required:

```json
{
  "public_schema_version": 1,
  "mode": "fixture",
  "evidence": { "...": "the complete allowlisted RunEvidenceV1 shape" }
}
```

The abbreviated `evidence` above is documentation, not a valid input. The complete
synthetic example is `web/showcase/synthetic.public.json`. The field contract is
[run-evidence.md](run-evidence.md); the explicit public allowlist and consistency gate
are `web/showcase/contract.mjs`. All required evidence keys, nulls, array entries,
reviewer slots, findings, command results, gaps and limitations must be preserved.
Only Route's `provider` and `variant` keys are optional, as in the TypeScript mirror.
Unknown keys at every object boundary are errors, including prompts, transcripts,
configuration, logs and command output. The gate constructs allowlisted objects; it
never silently strips a field, filters a record, upgrades a verdict or repairs evidence.
It consumes normalized exporter output; it does not rebuild the exporter or read state.

Prepare any recorded public payload privately and manually. Review **all** remaining
text, identities, revisions, repository names, commands and URLs for public disclosure.
Do not remove adverse evidence, gaps, failures, inconsistent records, duplicate verdicts
or ambiguous/multiple matches to make a run appear successful. Redact private identifying
text consistently while retaining its evidence meaning and references. If that cannot be
done faithfully, do not publish the run. This build cannot compare a public payload to
private source records and does not certify completeness or truth.

The operator-export review flag remains `true`, its original warning is required
verbatim and displayed unchanged, and all nine original limitations are mandatory.
The public wrapper does not turn the operator export into an automatically approved
artifact. There is no prepare/approve command that grants approval.

Normative schema states (reviewer state, check state, task status, route backend) reject
unknown values explicitly. The exporter's free-form cycle/session statuses and final decisions
remain verbatim, including unknown values, with neutral labels rather than an invented
successful interpretation. Contradictory normalized summaries (counts, reviewer slots,
review cleanliness, revision matches, configured checks and aggregate results) fail the
build. Gaps in the *saved records*, such as a rejected proposal with linked tasks or a
published task with missing review/PR evidence, remain displayable without being repaired.
Task joins must still match the supplied cycle and proposal identity.

## Preparing a real candidate privately

A launch narrative document is a historical comparison
source, not `RunEvidenceV1` input. Use only a specific offline snapshot or export the
owner has explicitly authorized; do not discover archives, reconstruct records from
prose, or launch a run without separate explicit authorization. Follow
[the snapshot procedure](run-evidence.md#exporting-from-a-copy-of-saved-state), keep the
authorized original untouched, and run `--export-run` on a private working copy. An
existing authorized export needs no database read. Raw input, intermediate exports,
candidate bytes and the private comparison/redaction notes stay outside Git and every
public build/serve root.

If the owner separately authorizes a new run, record that authorization and preserve the
new cycle/task IDs, baseline, decisions and actual outcome. Export its stopped, consistent
state through the same interface and label it as new-run evidence; it cannot recover
earlier records or establish their reported sequence. Carry that coverage limit into the
private review packet and the candidate's `limitations`. Public-sharing approval is still
required, and any remaining restriction on remote writes stays in force.

Compare the supplied fields with the narrative's reported references: cycle, task and
proposal identities, decision counts, positional reviewer verdicts, output and comparison
revisions, review identity and findings, PR references, interventions, repair, cost and
runtime identity. Record each as supported, missing or discrepant with a source/field
reference, and preserve every mismatch; never change saved facts to agree with a report.
Application revisions, intervention lineage and error text are outside `RunEvidenceV1`
and need separately authorized evidence; a rediscovery-shaped proposal ID is not proof of
an intervention, and requested routes are not runtime identity.

The public contract describes **one cycle**, not an entire rehearsal. Keep additional
authorized exports and the comparison in the private review packet. Do not splice earlier
attempts into a successful cycle, append an invented replay, or add wrapper fields. The
existing `limitations` array may carry a clearly labelled, owner-reviewed coverage
limitation such as "Coverage: this export represents one cycle; earlier attempts and
operator interventions are not included." Keep the nine original limitations verbatim and
never use the array to fabricate saved events. If the required context cannot be conveyed
faithfully within this projection, hold the candidate.

Manually prepare the public wrapper with `mode: "recorded"`. Redact only private text and
identifiers, consistently across their references. Keep record order, all entries, nulls,
gaps, adverse states and the original warning. Keep a private field-level record of each
redaction and any added coverage limitation. Do not attach raw logs, provenance paths,
billing material, transcripts or the review packet to the wrapper.

Before approval, validate and hash the candidate **without building or serving it**. From
the repository root, the following example invokes the showcase's existing parser and
allowlist, reads one explicit local file, and writes no files. It grants no approval and
does not compare the candidate to private source records. The fixture test exercises
this exact example with synthetic bytes only.

<!-- private-payload-check -->
```sh
node --input-type=module - /absolute/private/candidate.public.json <<'JS'
import { readFileSync, statSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { parseUniqueJson } from './web/scripts/public-json.mjs';
import { publicPayload } from './web/showcase/contract.mjs';

const input = process.argv[2];
const info = statSync(input);
if (!info.isFile() || info.size > 5_000_000)
  throw new Error('Input must be a local regular JSON file, at most 5 MB');
const bytes = readFileSync(input);
publicPayload(parseUniqueJson(bytes.toString('utf8')), 'recorded');
console.log('Candidate SHA-256: ' + createHash('sha256').update(bytes).digest('hex'));
console.log('Shape checked only; no build written and no sharing approval granted.');
JS
```

Supply the owner with the private candidate, its exact-byte SHA-256 and a concise
redaction/provenance summary: authorized source and capture method (private reference),
source/export hashes, cycle/task match results, redaction categories and preserved
adverse/missing evidence, scope limits and unresolved discrepancies. A candidate hash
is unavailable until real candidate bytes exist; a fixture hash is never a substitute.
Keep human usefulness assessment separate from permission to share this exact payload.

Only after the owner explicitly approves those bytes and supplies the public-safe
approval reference, use the recorded-mode command below. It stages the exact bytes in
`dist/showcase/public-run.json`. Compare that file byte-for-byte with the approved
candidate and check its hash. Any byte change, including whitespace or a new limitation,
invalidates approval. Record the staging result only after this gate and local staging
succeed; otherwise record the precise missing input, evidence or approval.
Staging is local preparation, not publication or deployment.

## Fixture and recorded modes

Fixture mode must be selected explicitly, the wrapper must say `"mode": "fixture"`,
and the prominent banner says **“Synthetic example — not a real run.”** Fixture mode
rejects approval files. The checked-in fixture includes delivery, deferral, rejection,
duplicate/malformed/missing reviewer evidence, failed checks, findings, zero matches and
multiple matches. It is not evidence of live operation.

Recorded mode requires a wrapper with `"mode": "recorded"` and the **exact owner-reviewed
public payload bytes**, plus a separate supplied approval JSON:

```json
{
  "approval_schema_version": 1,
  "owner_reviewed": true,
  "payload_sha256": "<64 lowercase hexadecimal SHA-256 characters>",
  "approval_reference": "<owner-supplied, public-safe review reference>"
}
```

The owner reviews the final public payload and supplies the approval reference bound to
its SHA-256. Do not generate or claim owner approval on the owner's behalf. The reference
is displayed as text, not fetched or linked. It must itself be suitable for public
sharing. The hash covers the **entire wrapper file's bytes**, including mode, whitespace,
newlines and encoding, before parsing/projection; reformatting requires renewed binding.
A hash binds reviewed bytes, not proof of truth or an independent signature. This is a
local build gate for a supplied owner attestation, not cryptographic identity verification.

With those two owner-supplied files already in hand:

```sh
sha256sum /absolute/private/reviewed-public-run.json
npm run showcase --prefix web -- --mode recorded --input /absolute/private/reviewed-public-run.json --approval /absolute/private/public-approval.json
python3 -m http.server 4307 --bind 127.0.0.1 --directory dist
```

Missing approval, an empty reference, a false attestation, a mismatched hash, a mode
mismatch, malformed JSON or an unsupported schema fails closed. There is no fixture
fallback. The build includes `public-run.json` with the exact reviewed bytes and, for
recorded mode only, `approval.json` containing the validated public approval fields.
The same payload is compiled into a local JavaScript asset; there is no runtime data fetch.
No source maps, raw operator artifacts, remote fonts, images or source reports are copied.

## Display and network boundaries

Visitors can inspect proposal text/evidence, both fixed reviewer slots, final rationale,
all linked task matches, source/comparison/output revisions, requested session routes,
the latest saved code review and findings, configured commands with their latest results,
and recorded PR references. Planning completion is distinct from delivery; deferred is
distinct from rejected; a recorded PR is neither current GitHub state nor a merge.
There is no full review history, command-output transcript or complete replay timeline
in this schema. The page states these limits instead of inventing them.

All supplied text uses escaped Svelte text rendering. There is no model-authored HTML,
Markdown execution, operator API import, authorization header, token storage, polling,
remote data URL option or automatic outbound request. The page's CSP blocks connections,
frames, images, objects, forms and nonlocal scripts. The only automatic HTTP requests are
its document and local JavaScript/CSS assets. Downloads and opening a PR are explicit
visitor actions. Clickable data URLs are limited to literal
`https://github.com/<owner>/<repo>/pull/<positive-number>` with no credentials, query,
fragment, whitespace or alternate host; unsafe/unsupported recorded URLs remain visible
as text. Links use `noopener noreferrer` and no-referrer policy. No PR state is fetched.

## Verification

```sh
npm run showcase:check --prefix web
npm run showcase:test --prefix web
make check
make test
```

The contract tests exercise explicit build modes, unsupported/malformed/private input,
unknown/inconsistent statuses, hash-bound approvals and preservation of adverse records.
Recorded-mode tests use explicitly synthetic approval attestations in temporary files;
they grant no real approval. Browser tests serve the built files under `/showcase/` on
Python's HTTP server, with no Rust process, and cover mobile/desktop navigation, missing
and multiple matches, review/check distinctions, hostile text/URLs, accessibility,
overflow and network isolation across the dashboard's ten-second polling interval.
The showcase checks are also included in `make check` and `make test`.

Tests overwrite/remove `dist/showcase/` and use synthetic payloads. After testing, rerun
your intended explicit build command to produce the final preview. No recorded public
build can be delivered until the owner supplies the reviewed payload and approval.
