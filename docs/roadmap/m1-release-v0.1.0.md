# M1 — Release v0.1.0

[Roadmap](README.md) · **Depends on:** owner gates · **Enables:**
[M2](m2-state-upgrades.md), [M4](m4-v0.2.0.md) · **Target:** Fri 2026-10-16

## Why

The repository has no tags, and the release workflow (`.github/workflows/release.yml`)
has never run. [Releasing](../releasing.md) says public releases "remain pending"
and image publication "has not run yet: the first release will exercise it".
`install.sh` fails with "No published stable release found" until a stable
release exists, so every operator builds from source. Arm64 has only ever been
tested under emulation.

## Deliverable

A public `v0.1.0` GitHub release with x86_64 and aarch64 archives and
`SHA256SUMS`, signed `octomus-agent` and `octomus-sandbox` images on GHCR, and
documentation that describes the release as published.

## Owner gates

These are owner decisions, not engineering work. Record each as it clears; while
any is open, the milestone is `BLOCKED` on it.

1. Employer and open-source clearance, and name clearance.
2. The LICENSE copyright holder and whether a NOTICE file is needed. AGENTS.md
   defers both until the owner supplies cleared facts.
3. Release-tag protection, so the worker identity cannot trigger a release by
   pushing a `v*` tag ([releasing](../releasing.md)).
4. Owner-written v0.1.0 release notes for the manual workflow run.
5. Public visibility for both GHCR packages.

## Work

1. **Fix two problems in `release.yml`.**
   - The `images` job pushes version-tagged images before `publish` runs, so a
     failed publish leaves public images for a release that does not exist.
     Change the ordering or tagging so a failed run leaves no version tag behind.
   - The `images` job checks out with `actions/checkout@v4`; every other job uses
     `@v7`.
2. **Date the changelog.** Turn `[Unreleased]` in `CHANGELOG.md` into `[0.1.0]`
   with the release date, and drop its "Binary releases remain pending" preamble.
3. **Cut the release.** Run `make check`, `make test`, `make audit` and
   `make package` on the release commit, then push `v0.1.0`. `VERSION` already
   reads `0.1.0`, which the workflow's tag check requires.
4. **Check what shipped.**
   - `install.sh` on fresh Ubuntu 24.04 hosts, one x86_64 and one **native**
     aarch64.
   - `cosign verify` on both images, with the identity in
     [releasing](../releasing.md).
   - The compose stack with `OCTOMUS_IMAGE` and `OCTOMUS_SANDBOX_IMAGE` set to the
     release images passes **Check connection**, which includes the containment
     self-test.
5. **Retire the "pending" statements** once the release exists:
   - `docs/releasing.md` (opening paragraph and "Image publication has not run
     yet");
   - `docs/index.md` ("Public image and binary distribution is pending");
   - `docs/getting-started.md` (the opening paragraph and "Pending release
     options");
   - `docs/deployment.md` ("Public release installation remains pending");
   - `SECURITY.md` ("No public release is recorded yet");
   - `CHANGELOG.md` ("Image publication has not run yet").

   Cost statements stay as they are; live cost measurement is outside this
   roadmap.

## Acceptance criteria

1. The tag equals `v` plus the contents of `VERSION`, and the release workflow run
   is green.
2. Both archives and `SHA256SUMS` download from the release page, and the
   checksums match.
3. `install.sh` installs on x86_64 and native aarch64, and the installed binary's
   `--version` prints `0.1.0`.
4. `cosign verify` passes for both images.
5. A compose stack on the release images passes **Check connection**, including
   the containment self-test.
6. A release run that fails at `publish` leaves no version-tagged image, shown by
   the workflow change and its review.
7. No document still describes the release or image publication as pending.

## Verification

- Local: `make check`, `make test`, `make audit`, `make package`, and
  `python3 tests/distribution.py --package dist/<archive>` for each archive.
- Release: the workflow run URL, the release URL and the image digests.
- Install checks: the host, architecture and `--version` output for each.
- `grep -rnw -e pending -e "has not run" README.md SECURITY.md CHANGELOG.md docs/*.md`
  leaves only the cost, notification-queue and rediscovery mentions.

## Progress record

```text
Milestone: M1
Status: TODO
Revision: Not started.
Delivered output: None.
Commands and results: Not run.
Unrun checks and blockers: All checks unrun; owner gates 1–5 open.
Next: M4's release after M1 is DONE.
```
