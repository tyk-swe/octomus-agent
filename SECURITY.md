# Security policy

Report vulnerabilities privately to **mail@mail.tyk.sh**. Include the affected
version or commit, reproduction steps, impact, and redacted evidence. Do not open
a public issue containing an exploit, access token, private repository content,
or raw runner transcripts. We aim to acknowledge reports within **three business
days**; remediation and coordinated disclosure timing depend on the report.

## Supported versions

No public release is recorded yet. During preparation, report findings against
`main` with a commit ID. Once releases begin, security fixes target the latest
published release; older releases are unsupported and operators should upgrade.
This is a maintenance policy, not a guarantee of a fix within a particular time.

## Intended deployment

Octomus is for one operator. The default Docker deployment runs every agent turn
and verification command in a sandbox container without GitHub credentials, behind
an egress allowlist; see [docs/sandbox.md](docs/sandbox.md). With `--sandbox off`
it runs deliberately unsandboxed on a dedicated Linux VM, where repository content
can influence processes with the service user's privileges. In both, keep the
dashboard on loopback behind an SSH tunnel and restrict the GitHub credential to
the intended repository. The operator token grants control of work and policy.

Read the [threat model](docs/threat-model.md) before enabling work. Unexpected
publication, verification bypass, disclosure of secrets, sandbox escapes, egress
allowlist bypasses and other boundary failures are appropriate private reports. Prompt injection is an acknowledged design risk;
report concrete incidents and any failure beyond the documented boundaries.
