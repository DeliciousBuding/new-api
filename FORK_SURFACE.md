# TokenDance Gateway Fork Surface
最后更新：2026-07-31 22:40

This branch is rebuilt from a fixed official NewAPI base and keeps the
TokenDance delta as a short, ordered topic stack. It intentionally preserves
the upstream project name, license, attribution, package paths, and metadata.

## Fixed Base

- Upstream: `QuantumNous/new-api`
- Base: `main` @ `9724ef1b248a436ea47270bb5b394a0fdb013a6c` (2026-07-31)
- Machine-readable pin: `UPSTREAM_BASE`

## Ordered Topic Stack

1. `fix(auth): restore TokenDance ID branding`
   - Owns only the built-in OIDC button/callback presentation, icon, and locale
     strings. It does not change the OIDC protocol or status API. OIDC display
     name follows upstream `oidc_display_name`.
2. `ci: publish TokenDance gateway candidate`
   - Builds the official Dockerfile on GitHub-hosted runners for this public
     source and publishes an immutable GHCR candidate.
   - Does not contain SSH, production credentials, compose files, or deployment.
3. `ci: run public fork automation on GitHub-hosted runners`
   - Keeps tag builds and weekly upstream checks off private infrastructure.
   - Excludes TokenDance tags from the upstream multi-platform binary release,
     so each TokenDance release has one intentional image build.
4. `ci: publish public source image separately`
5. `docs: clarify public image boundary`
6. `docs: describe public source mirror accurately`
7. `ci: fix upstream-check tag selection pipefail`
8. `feat(audit): record client source IP and expose cache rate in logs`
   - `RecordConsumeLog`/`RecordErrorLog` store the client IP by default via the
     new global option `LogRecordIpEnabled` (default true).
   - Frontend: admin IP column (masked in sensitive mode) on consume logs,
     cache-read ratio next to the cache figure, IP/Client rows in the consume
     log details dialog, and a "Record client IP addresses" switch in Log
     Maintenance settings.
   - No schema change: `logs.ip` already exists and `cache_tokens` is already
     carried in `other` JSON by upstream `GenerateTextOtherInfo`.
   - Follow-up commits in the same topic (replay in order):
     - `fix(audit): show cache read ratio in consume log details`
     - `feat(audit): record client profile from upstream trace headers`
       (client hint only; constrained multi-header matching, admin-only via
       `formatUserLogs` trimming, never used for auth/billing/routing)
     - `fix(audit): restrict Client row to admin view`
     - `fix(audit): use upstream input_tokens_total as cache ratio denominator`
     - `fix(audit): address external review findings`
       (client hint trust, self/token API trimming, ratio clamp to 100%,
       i18n translation namespace fix, sensitive-mode IP masking, user-level
       record_ip_log switch disabled with admin hint, unit tests)

### Not replayed

- `fix: treat admin search wildcards as literals` (2026-07-31 decision:
  follow upstream search semantics; this patch is dropped, not part of the
  topic stack).

## Updating

1. Fetch official tags and select an immutable tag and SHA.
2. Create a new branch directly from that SHA.
3. Replay the ordered topic commits. Resolve conflicts by reviewing upstream
   semantics; never use `merge -X ours` or bulk checkout of our side.
4. Update `UPSTREAM_BASE`, this file, and `VERSION`.
5. Run the focused tests, Go tests, frontend typecheck/build, and image build.
6. Publish a release tag only after the candidate digest and rollback digest are
   recorded. Production deployment remains an operator action behind the idle
   gate.

This clean stack is published as a public GitHub source mirror. The historical
TokenDance repository remains private because its old history contains operational
topology. Compose files, runtime pins, host details, and deployment logs remain
in the private deployment and server repositories. The GHCR image is public
because it contains only this public source; production still deploys by an
immutable digest recorded in the private repositories.
