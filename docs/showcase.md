# Explore a run: public static showcase

The showcase is a standalone Svelte/Vite build of recorded `RunEvidenceV1`
evidence. It requires no Rust service, account, token, model, database or live GitHub
request. It does not mount `RunEvidence.svelte`. It shares only the pure evidence/route
labels and `EvidenceFact` / `EvidenceText` presentation components with the dashboard.
No framework or runtime dependency was added.

The [launch site](product-hunt.md) embeds a separate guided sample with four outcomes:
delivered, rejected, deferred, and blocked. To build only that sample, use
`npm run showcase --prefix web -- --mode fixture --input launch/sample.public.json`.
The original fixture below remains the regression example for duplicated, missing,
and inconsistent evidence. Both are synthetic and visibly labeled.

The explorer's numbered controls move keyboard focus through the idea, both reviewers,
the decision, and linked tasks. Raw cycle metadata is available in an expandable section;
gaps and adverse evidence remain visible. Navigation does not change the selected record.

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


## Preparing a recorded run

The fixture above is synthetic and needs no approval. A build from a real run does:
the owner reviews the payload and binds an approval to its exact bytes, and nothing in
this repository grants that approval on their behalf.

Validate and hash a candidate without building or serving it. The example reads one
explicit local file, writes nothing, and is exercised by `tests/evidence_snapshot.py`
with synthetic bytes only.

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

Supply the owner with the candidate, its exact-byte SHA-256 and a short provenance
summary. Never fabricate, upgrade or remove adverse evidence to make a run presentable.

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
```

The contract tests exercise explicit build modes, malformed and private input,
unknown statuses, hash-bound approvals and preservation of adverse records. Browser
tests serve the built files with no Rust process and cover navigation, missing and
multiple matches, hostile text and URLs, accessibility and network isolation. Both run
inside `make check` and `make test`, which overwrite `dist/showcase/` with synthetic
payloads — rerun your intended build afterwards.
