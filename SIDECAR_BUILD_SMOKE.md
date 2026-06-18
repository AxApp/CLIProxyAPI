# GetTokens Sidecar Build Smoke

This smoke check proves the maintained `gettokens/sidecar` reference can compile a test-side sidecar binary with the current GetTokens management hooks linked into `cmd/server`.

Round26 explicitly separates deterministic source metadata from volatile build metadata. The smoke binary is not expected to be deterministic because `main.BuildDate` is set from the current UTC timestamp on every run. The goal is reproducible evidence, not a release-deterministic binary.

Final Completion Wave adds clean/dirty source-state comparison. A dirty primary smoke remains valid only as source-state evidence. When the current commit can be checked out cleanly, the script creates a temporary clean worktree under `/private/tmp`, runs a second clean comparison smoke for the same commit, and writes that clean result back into the primary latest manifest.

## Scope

The check is bounded to the CLIProxyAPI reference checkout:

- runs focused `internal/gettokenshooks` management route tests for Doctor diagnostics and Route Resilience actions;
- builds `./cmd/server` to `/private/tmp/gettokens-cliproxyapi-sidecar-smoke/cli-proxy-api-round26-smoke`;
- when the primary source is dirty and clean comparison is enabled, creates a temporary detached clean worktree for the same commit, builds `./cmd/server` to `/private/tmp/gettokens-cliproxyapi-sidecar-smoke/cli-proxy-api-round26-smoke-clean-comparison`, then removes the temporary worktree;
- runs the built binary with `-h` only, so the sidecar process does not start serving traffic;
- writes smoke output, reproducibility manifest, and Go build cache under `/private/tmp`.

It does not publish a release, copy into `build/bin/GetTokens.app`, replace `Contents/MacOS/cli-proxy-api`, edit `/Applications/GetTokens.app`, or touch formal GetTokens config/data directories.

## Command

From `docs-linhay/references/CLIProxyAPI`:

```bash
scripts/gettokens-sidecar-build-smoke.sh
node scripts/check-sidecar-smoke-manifest.mjs latest
```

Lightweight docs gate without rebuilding sidecar:

```bash
node scripts/check-sidecar-smoke-manifest.mjs fixture
```

Optional bounded output override:

```bash
GETTOKENS_SIDECAR_SMOKE_OUT=/private/tmp/gettokens-cliproxyapi-sidecar-smoke scripts/gettokens-sidecar-build-smoke.sh
```

The script refuses output paths outside `/private/tmp`. By default it also sets `GOPROXY=off`, so the smoke uses the local module cache and does not try to fetch dependencies from the network. Override it with `GETTOKENS_SIDECAR_SMOKE_GOPROXY` only when explicitly validating dependency download behavior.

Optional source override for clean comparison or an externally prepared checkout:

```bash
GETTOKENS_SIDECAR_SMOKE_SOURCE_DIR=/private/tmp/clean-cli-proxy-api scripts/gettokens-sidecar-build-smoke.sh
```

Set `GETTOKENS_SIDECAR_SMOKE_COMPARE_CLEAN=0` to skip the automatic clean comparison. If clean worktree creation or clean smoke execution fails, the primary manifest records `sourceStateComparison.cleanComparisonAvailable=false` and a `cleanUnavailableReason`; it must still remain test-only and non-release.

## Expected Evidence

Successful output includes:

- `go test ./internal/gettokenshooks` passing the focused management route registration tests;
- a built executable at `/private/tmp/gettokens-cliproxyapi-sidecar-smoke/cli-proxy-api-round26-smoke`;
- `cli-proxy-api-round26-smoke-help.txt`, proving the binary starts far enough to parse flags without serving;
- `cli-proxy-api-round26-smoke.sha256`, proving the exact test binary fingerprint for this run;
- `cli-proxy-api-round26-smoke-manifest.json`, proving deterministic source metadata, volatile build metadata, field classification, sha256, and release boundary as machine-readable evidence.
- when the primary source is dirty and clean comparison succeeds, `cli-proxy-api-round26-smoke-clean-comparison-manifest.json`, proving the same commit can also be smoke-built from a clean checkout.
- a checked-in stable fixture at `fixtures/sidecar-smoke/cli-proxy-api-round26-smoke-manifest.fixture.json`, so docs-check can validate manifest v2 schema and release boundary without rebuilding sidecar.

The JSON manifest must include:

- `deterministicSourceMetadata.commitShort`, `deterministicSourceMetadata.commitFull`, `deterministicSourceMetadata.branch`, `deterministicSourceMetadata.sourcePath`;
- `deterministicSourceMetadata.dirty`, `deterministicSourceMetadata.dirtyStatus`, `deterministicSourceMetadata.goModule`, and `deterministicSourceMetadata.sourceStateHash`;
- `volatileBuildMetadata.timestampUTC`, `volatileBuildMetadata.buildDateUTC`, `volatileBuildMetadata.binaryPath`, `volatileBuildMetadata.binarySha256`, `volatileBuildMetadata.sha256File`, `volatileBuildMetadata.helpLogPath`;
- `volatileBuildMetadata.commands.test`, `volatileBuildMetadata.commands.build`, `volatileBuildMetadata.commands.help`, `volatileBuildMetadata.commands.sha256`;
- `volatileBuildMetadata.environment.GOCACHE`, `volatileBuildMetadata.environment.GOPROXY`, `volatileBuildMetadata.environment.goVersion`;
- `reproducibilityBoundary.deterministicSourceFields`, `reproducibilityBoundary.volatileBuildFields`, `reproducibilityBoundary.binaryDeterministic: false`, and `reproducibilityBoundary.binaryVolatilityCauses`;
- `reproducibilityBoundary.binarySha256Volatile: true`, proving `volatileBuildMetadata.binarySha256` is run-local and not a fixed release hash;
- top-level `testOnly: true`, `notReleaseArtifact: true`, and `releasePipelineEligible: false`.
- `releaseBoundary.dirtyStatusEvidenceOnly: true`, proving dirty status is source-state evidence only and does not upgrade the smoke result into a release artifact.
- `sourceState.classification`, `sourceState.clean`, `sourceState.dirty`, `sourceState.artifactClass: volatile-test-binary`, and `sourceState.dirtyStatusEvidenceOnly: true`.
- `sourceStateComparison.cleanComparisonAvailable`; when true, the manifest must include `cleanManifestPath`, `cleanSourceStateHash`, `cleanBinarySha256`, and `sameCommit: true`; when false, it must include `cleanUnavailableReason`.

Validate the manifest schema with:

```bash
node scripts/check-sidecar-smoke-manifest.mjs latest
```

Or validate an explicit path:

```bash
node scripts/check-sidecar-smoke-manifest.mjs /private/tmp/gettokens-cliproxyapi-sidecar-smoke/cli-proxy-api-round26-smoke-manifest.json
```

The checker intentionally fails old Round25 manifests because they do not split deterministic source metadata from volatile build metadata.

The build metadata intentionally appends `+dirty` when the reference worktree has uncommitted changes. That is acceptable for smoke evidence because this check is for the current test-side reference, not a publishable sidecar artifact.

## Provenance Boundary

The manifest exists to make a dirty reference smoke auditable. It answers which source checkout produced the test binary, whether that checkout was dirty, which commands ran, which fields should be stable for a fixed source state, and which fields are expected to change across runs.

The manifest does not make a dirty binary releasable. A manifest with `deterministicSourceMetadata.dirty: true` or any `deterministicSourceMetadata.dirtyStatus` entries is explicitly test-only evidence. Do not copy that binary into `build/bin/GetTokens.app`, `Contents/MacOS/cli-proxy-api`, `/Applications/GetTokens.app`, or any release staging directory.

For Round26+ manifests, use `deterministicSourceMetadata.dirty` and `deterministicSourceMetadata.dirtyStatus` when checking source cleanliness. For Final Completion Wave manifests, prefer `sourceState.classification` for the top-level clean/dirty decision and `sourceStateComparison` for whether a clean checkout of the same commit was separately smoke-built. `releaseBoundary.dirtyStatusEvidenceOnly: true` means dirty fields only describe the audited source state; they do not make the smoke output releasable. `volatileBuildMetadata.binarySha256` is the fingerprint for one temporary smoke binary and is expected to change when `buildDateUTC` changes; `reproducibilityBoundary.binarySha256Volatile: true` records that boundary explicitly.

## Release Pipeline Eligibility

This smoke result may inform the release pipeline only after a separate release build proves all of the following:

- the maintained `gettokens/sidecar` fork is at a clean commit;
- the sidecar is rebuilt through the release-approved path, not reused from `/private/tmp`;
- generated release metadata records the clean source commit and artifact hash;
- Wails packaging, signing, notarization, and release checks pass for the intended architecture;
- no dirty smoke artifact is copied into an app bundle or published asset.
