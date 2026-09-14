# Security policy

Report vulnerabilities privately to **mail@mail.tyk.sh**. Include the affected
version or commit, reproduction steps, impact, and redacted evidence. Do not open
a public issue containing an exploit, access token, private repository content,
or raw runner transcripts. We target acknowledgement within **three business days**;
remediation and coordinated disclosure timing depend on the report.

## Supported versions

No public release is recorded yet. During preparation, report findings against
`main` with a commit ID. Once releases begin, security fixes target the latest
published release; older releases are unsupported and operators should upgrade.
This is a maintenance policy, not a guarantee of a fix within a particular time.

## Intended deployment

Octomus runs deliberately unsandboxed on a dedicated Linux VM for one operator.
Repository content can influence processes with the service user's privileges.
Keep the dashboard on loopback behind an SSH tunnel and restrict the GitHub
credential to the intended repository. The operator token grants full control.

Read the [threat model](docs/threat-model.md) before enabling work. Unexpected
publication, verification bypass, disclosure of secrets, and boundary failures
are appropriate private reports. Prompt injection is an acknowledged design risk;
report concrete incidents and any failure beyond the documented boundaries.
