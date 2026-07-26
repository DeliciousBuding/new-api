# TokenDance Gateway Fork Surface
最后更新：2026-07-27 03:30

This branch is rebuilt from a fixed official NewAPI release and keeps the
TokenDance delta as a short, ordered topic stack. It intentionally preserves
the upstream project name, license, attribution, package paths, and metadata.

## Fixed Base

- Upstream: `QuantumNous/new-api`
- Tag: `v1.0.0-rc.22`
- Commit: `bc14c18f6024e79cba1c08d02cd007796e12d668`
- Machine-readable pin: `UPSTREAM_BASE`

## Ordered Topic Stack

1. `fix: treat admin search wildcards as literals`
   - Keeps administrative searches bounded and treats `%` and `_` as literals.
   - Includes the cross-database search contract test.
2. `fix(auth): restore TokenDance ID branding`
   - Owns only the built-in OIDC button/callback presentation, icon, and locale
     strings. It does not change the OIDC protocol or status API.
3. `ci: publish TokenDance gateway candidate`
   - Builds the official Dockerfile on GitHub-hosted runners for this public
     fork and publishes an immutable candidate to private GHCR.
   - Does not contain SSH, production credentials, compose files, or deployment.
4. `ci: run public fork automation on GitHub-hosted runners`
   - Keeps tag builds and weekly upstream checks off private infrastructure.
   - Excludes TokenDance tags from the upstream multi-platform binary release,
     so each TokenDance release has one intentional image build.

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

This clean stack is published as a public GitHub fork. The historical TokenDance
repository remains private because its old history contains operational
topology. Compose files, runtime pins, host details, and deployment logs remain
in the private deployment and server repositories. GHCR visibility is managed
separately and remains private.
