# Changelog

Notable changes are recorded here using [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- One-shot audits with durable proposal decisions and no task dispatch.
- Embedded dashboard, Linux x86_64/aarch64 release packaging, checksum-verifying
  installer and Cargo package metadata/assets.
- Security policy, threat model, hardened systemd unit, failed-authentication
  backoff and warnings for non-loopback listeners.
- Dependency audits, distribution/audit coverage and community contribution files.
- Configurable repair routes, CLI version diagnostics, durable admission accounting
  and read-only usage reports for live commissioning preparation.
- Operator guidance: optional configuration text injected into grounding, discovery,
  adversarial review and consolidation prompts as authoritative operator policy.
- Outbound notifications: an optional webhook URL receives redacted JSON events for
  published/blocked tasks, failed cycles, audits and error pauses via `curl`.

### Changed

- Dashboard builds precede Rust builds; `--assets` is an explicit override.
- Operator checklist moved to `docs/operations.md`; README starts with installation
  and first-run guidance.

Publication remains pending; this is not a released version.
