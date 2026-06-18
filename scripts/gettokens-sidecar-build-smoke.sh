#!/usr/bin/env bash
set -euo pipefail

SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
DEFAULT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="${GETTOKENS_SIDECAR_SMOKE_SOURCE_DIR:-$DEFAULT_ROOT}"
OUT_DIR="${GETTOKENS_SIDECAR_SMOKE_OUT:-/private/tmp/gettokens-cliproxyapi-sidecar-smoke}"
LABEL="${GETTOKENS_SIDECAR_SMOKE_LABEL:-latest}"
COMPARISON_ROLE="${GETTOKENS_SIDECAR_SMOKE_COMPARISON_ROLE:-primary}"

case "$OUT_DIR" in
  /private/tmp/*) ;;
  *)
    echo "refusing to write outside /private/tmp: $OUT_DIR" >&2
    exit 2
    ;;
esac

mkdir -p "$OUT_DIR"
cd "$ROOT"

export GOCACHE="${GETTOKENS_SIDECAR_SMOKE_GOCACHE:-$OUT_DIR/gocache}"
export GOPROXY="${GETTOKENS_SIDECAR_SMOKE_GOPROXY:-off}"
mkdir -p "$GOCACHE"

commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
commit_full="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
branch="$(git branch --show-current 2>/dev/null || echo detached)"
dirty_status="$(git status --porcelain=v1 --untracked-files=all 2>/dev/null || true)"
dirty_suffix=""
dirty="false"
if [ -n "$dirty_status" ]; then
  dirty_suffix="+dirty"
  dirty="true"
fi
build_date="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
if [ "$LABEL" = "latest" ]; then
  artifact_base="cli-proxy-api-round26-smoke"
else
  artifact_base="cli-proxy-api-round26-smoke-$LABEL"
fi
binary="$OUT_DIR/$artifact_base"
help_log="$OUT_DIR/$artifact_base-help.txt"
sha_file="$OUT_DIR/$artifact_base.sha256"
manifest="$OUT_DIR/$artifact_base-manifest.json"
version="round26-smoke"
go_module="$(awk '$1 == "module" { print $2; exit }' go.mod)"
go_version="$(go version)"
source_state_hash="$(printf "%s\n%s\n%s\n" "$commit_full" "$branch" "$dirty_status" | shasum -a 256 | awk '{print $1}')"
source_state_classification="clean-source"
source_state_clean="true"
if [ "$dirty" = "true" ]; then
  source_state_classification="dirty-source"
  source_state_clean="false"
fi
test_command="go test ./internal/gettokenshooks -run '^(TestDoctorDiagnosticsRegisteredOnGetTokensManagementRoutes|TestRouteResilienceActionRegisteredOnGetTokensManagementRoutes)$' -count=1"
build_command="go build -trimpath -ldflags \"-X main.Version=${version} -X main.Commit=${commit}${dirty_suffix} -X main.BuildDate=${build_date}\" -o \"$binary\" ./cmd/server"
help_command="\"$binary\" -h >\"$help_log\" 2>&1"
sha_command="shasum -a 256 \"$binary\""

echo "== gettokens management route focused tests =="
echo "GOCACHE=$GOCACHE"
echo "GOPROXY=$GOPROXY"
go test ./internal/gettokenshooks \
  -run '^(TestDoctorDiagnosticsRegisteredOnGetTokensManagementRoutes|TestRouteResilienceActionRegisteredOnGetTokensManagementRoutes)$' \
  -count=1

echo "== bounded sidecar reference build =="
rm -f "$binary" "$help_log" "$sha_file"
go build -trimpath \
  -ldflags "-X main.Version=${version} -X main.Commit=${commit}${dirty_suffix} -X main.BuildDate=${build_date}" \
  -o "$binary" \
  ./cmd/server

test -x "$binary"
"$binary" -h >"$help_log" 2>&1
sha256="$(shasum -a 256 "$binary" | awk '{print $1}')"
printf "%s  %s\n" "$sha256" "$binary" | tee "$sha_file"

export GETTOKENS_SMOKE_MANIFEST="$manifest"
export GETTOKENS_SMOKE_TIMESTAMP="$build_date"
export GETTOKENS_SMOKE_SOURCE_PATH="$ROOT"
export GETTOKENS_SMOKE_BRANCH="$branch"
export GETTOKENS_SMOKE_COMMIT_SHORT="$commit"
export GETTOKENS_SMOKE_COMMIT_FULL="$commit_full"
export GETTOKENS_SMOKE_DIRTY="$dirty"
export GETTOKENS_SMOKE_DIRTY_STATUS="$dirty_status"
export GETTOKENS_SMOKE_GO_MODULE="$go_module"
export GETTOKENS_SMOKE_SOURCE_STATE_HASH="$source_state_hash"
export GETTOKENS_SMOKE_SOURCE_STATE_CLASSIFICATION="$source_state_classification"
export GETTOKENS_SMOKE_SOURCE_STATE_CLEAN="$source_state_clean"
export GETTOKENS_SMOKE_COMPARISON_ROLE="$COMPARISON_ROLE"
export GETTOKENS_SMOKE_OUTPUT_DIR="$OUT_DIR"
export GETTOKENS_SMOKE_BINARY="$binary"
export GETTOKENS_SMOKE_HELP_LOG="$help_log"
export GETTOKENS_SMOKE_SHA_FILE="$sha_file"
export GETTOKENS_SMOKE_SHA256="$sha256"
export GETTOKENS_SMOKE_VERSION="$version"
export GETTOKENS_SMOKE_LDFLAGS_COMMIT="${commit}${dirty_suffix}"
export GETTOKENS_SMOKE_GOCACHE="$GOCACHE"
export GETTOKENS_SMOKE_GOPROXY="$GOPROXY"
export GETTOKENS_SMOKE_GO_VERSION="$go_version"
export GETTOKENS_SMOKE_TEST_COMMAND="$test_command"
export GETTOKENS_SMOKE_BUILD_COMMAND="$build_command"
export GETTOKENS_SMOKE_HELP_COMMAND="$help_command"
export GETTOKENS_SMOKE_SHA_COMMAND="$sha_command"

python3 - <<'PY'
import json
import os

manifest = os.environ["GETTOKENS_SMOKE_MANIFEST"]
dirty_status = os.environ.get("GETTOKENS_SMOKE_DIRTY_STATUS", "")
deterministic_fields = [
    "deterministicSourceMetadata.sourcePath",
    "deterministicSourceMetadata.branch",
    "deterministicSourceMetadata.commitShort",
    "deterministicSourceMetadata.commitFull",
    "deterministicSourceMetadata.dirty",
    "deterministicSourceMetadata.dirtyStatus",
    "deterministicSourceMetadata.goModule",
    "deterministicSourceMetadata.sourceStateHash",
]
volatile_fields = [
    "volatileBuildMetadata.timestampUTC",
    "volatileBuildMetadata.buildDateUTC",
    "volatileBuildMetadata.outputDir",
    "volatileBuildMetadata.binaryPath",
    "volatileBuildMetadata.binarySha256",
    "volatileBuildMetadata.sha256File",
    "volatileBuildMetadata.helpLogPath",
    "volatileBuildMetadata.version",
    "volatileBuildMetadata.ldflagsCommit",
    "volatileBuildMetadata.commands.test",
    "volatileBuildMetadata.commands.build",
    "volatileBuildMetadata.commands.help",
    "volatileBuildMetadata.commands.sha256",
    "volatileBuildMetadata.environment.GOCACHE",
    "volatileBuildMetadata.environment.GOPROXY",
    "volatileBuildMetadata.environment.goVersion",
]
data = {
    "manifestVersion": 2,
    "schema": "gettokens-sidecar-smoke-manifest.v2",
    "purpose": "GetTokens CLIProxyAPI sidecar rebuild smoke reproducibility evidence for test use only.",
    "testOnly": True,
    "notReleaseArtifact": True,
    "releasePipelineEligible": False,
    "deterministicSourceMetadata": {
        "sourcePath": os.environ["GETTOKENS_SMOKE_SOURCE_PATH"],
        "branch": os.environ["GETTOKENS_SMOKE_BRANCH"],
        "commitShort": os.environ["GETTOKENS_SMOKE_COMMIT_SHORT"],
        "commitFull": os.environ["GETTOKENS_SMOKE_COMMIT_FULL"],
        "dirty": os.environ["GETTOKENS_SMOKE_DIRTY"] == "true",
        "dirtyStatus": dirty_status.splitlines(),
        "goModule": os.environ["GETTOKENS_SMOKE_GO_MODULE"],
        "sourceStateHash": os.environ["GETTOKENS_SMOKE_SOURCE_STATE_HASH"],
    },
    "sourceState": {
        "classification": os.environ["GETTOKENS_SMOKE_SOURCE_STATE_CLASSIFICATION"],
        "clean": os.environ["GETTOKENS_SMOKE_SOURCE_STATE_CLEAN"] == "true",
        "dirty": os.environ["GETTOKENS_SMOKE_DIRTY"] == "true",
        "dirtyStatusEvidenceOnly": True,
        "artifactClass": "volatile-test-binary",
        "comparisonRole": os.environ["GETTOKENS_SMOKE_COMPARISON_ROLE"],
    },
    "volatileBuildMetadata": {
        "timestampUTC": os.environ["GETTOKENS_SMOKE_TIMESTAMP"],
        "buildDateUTC": os.environ["GETTOKENS_SMOKE_TIMESTAMP"],
        "outputDir": os.environ["GETTOKENS_SMOKE_OUTPUT_DIR"],
        "binaryPath": os.environ["GETTOKENS_SMOKE_BINARY"],
        "binarySha256": os.environ["GETTOKENS_SMOKE_SHA256"],
        "sha256File": os.environ["GETTOKENS_SMOKE_SHA_FILE"],
        "helpLogPath": os.environ["GETTOKENS_SMOKE_HELP_LOG"],
        "version": os.environ["GETTOKENS_SMOKE_VERSION"],
        "ldflagsCommit": os.environ["GETTOKENS_SMOKE_LDFLAGS_COMMIT"],
        "commands": {
            "test": os.environ["GETTOKENS_SMOKE_TEST_COMMAND"],
            "build": os.environ["GETTOKENS_SMOKE_BUILD_COMMAND"],
            "help": os.environ["GETTOKENS_SMOKE_HELP_COMMAND"],
            "sha256": os.environ["GETTOKENS_SMOKE_SHA_COMMAND"],
        },
        "environment": {
            "GOCACHE": os.environ["GETTOKENS_SMOKE_GOCACHE"],
            "GOPROXY": os.environ["GETTOKENS_SMOKE_GOPROXY"],
            "goVersion": os.environ["GETTOKENS_SMOKE_GO_VERSION"],
        },
    },
    "reproducibilityBoundary": {
        "deterministicSourceFields": deterministic_fields,
        "volatileBuildFields": volatile_fields,
        "binaryDeterministic": False,
        "binarySha256Volatile": True,
        "binaryVolatilityCauses": [
            "main.BuildDate is set from the current UTC timestamp for every smoke run",
            "binaryPath, sha256File, helpLogPath, outputDir, GOCACHE, and local Go toolchain/cache state are run-local evidence",
            "binarySha256 fingerprints this exact temporary smoke binary only and is expected to change when volatile build metadata changes",
        ],
        "deterministicClaim": "For a fixed source checkout state, deterministicSourceMetadata and sourceStateHash identify the audited source inputs. They do not claim the compiled binary is deterministic.",
    },
    "legacyRound25Mapping": {
        "source": "deterministicSourceMetadata",
        "artifact": "volatileBuildMetadata",
        "commands": "volatileBuildMetadata.commands",
        "environment": "volatileBuildMetadata.environment",
    },
    "releaseBoundary": {
        "mayCopyIntoAppBundle": False,
        "mayTouchApplicationsGetTokens": False,
        "mayPublish": False,
        "dirtyStatusEvidenceOnly": True,
        "requiredBeforeReleasePipeline": [
            "build from a clean maintained gettokens/sidecar commit",
            "record deterministic source metadata without dirtyStatus entries",
            "run the release pipeline sidecar rebuild path instead of this smoke script",
            "complete the release packaging and signing/notarization gates",
        ],
    },
}
if data["sourceState"]["dirty"]:
    data["sourceStateComparison"] = {
        "mode": "dirty-with-clean-comparison",
        "primaryRole": os.environ["GETTOKENS_SMOKE_COMPARISON_ROLE"],
        "cleanComparisonAvailable": False,
        "sameCommit": False,
        "result": "clean-comparison-not-attempted",
        "dirtyManifestPath": manifest,
        "dirtySourcePath": os.environ["GETTOKENS_SMOKE_SOURCE_PATH"],
        "dirtySourceStateHash": os.environ["GETTOKENS_SMOKE_SOURCE_STATE_HASH"],
        "dirtyBinarySha256": os.environ["GETTOKENS_SMOKE_SHA256"],
        "cleanUnavailableReason": "clean comparison has not run yet",
    }
else:
    data["sourceStateComparison"] = {
        "mode": "clean-comparison" if os.environ["GETTOKENS_SMOKE_COMPARISON_ROLE"] == "clean-comparison" else "clean-source-primary",
        "primaryRole": os.environ["GETTOKENS_SMOKE_COMPARISON_ROLE"],
        "cleanComparisonAvailable": True,
        "sameCommit": True,
        "result": "clean-source-recorded",
        "cleanManifestPath": manifest,
        "cleanSourcePath": os.environ["GETTOKENS_SMOKE_SOURCE_PATH"],
        "cleanSourceStateHash": os.environ["GETTOKENS_SMOKE_SOURCE_STATE_HASH"],
        "cleanBinarySha256": os.environ["GETTOKENS_SMOKE_SHA256"],
        "cleanCommitFull": os.environ["GETTOKENS_SMOKE_COMMIT_FULL"],
    }
with open(manifest, "w", encoding="utf-8") as handle:
    json.dump(data, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY

update_clean_comparison_unavailable() {
  local reason="$1"
  GETTOKENS_SMOKE_CLEAN_UNAVAILABLE_REASON="$reason" python3 - <<'PY'
import json
import os

manifest = os.environ["GETTOKENS_SMOKE_MANIFEST"]
with open(manifest, "r", encoding="utf-8") as handle:
    data = json.load(handle)
data["sourceStateComparison"].update({
    "cleanComparisonAvailable": False,
    "sameCommit": False,
    "result": "clean-comparison-unavailable",
    "cleanUnavailableReason": os.environ["GETTOKENS_SMOKE_CLEAN_UNAVAILABLE_REASON"],
})
with open(manifest, "w", encoding="utf-8") as handle:
    json.dump(data, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY
}

update_clean_comparison_available() {
  local clean_manifest="$1"
  GETTOKENS_SMOKE_CLEAN_MANIFEST="$clean_manifest" python3 - <<'PY'
import json
import os

manifest = os.environ["GETTOKENS_SMOKE_MANIFEST"]
clean_manifest = os.environ["GETTOKENS_SMOKE_CLEAN_MANIFEST"]
with open(manifest, "r", encoding="utf-8") as handle:
    data = json.load(handle)
with open(clean_manifest, "r", encoding="utf-8") as handle:
    clean = json.load(handle)
data["sourceStateComparison"].update({
    "cleanComparisonAvailable": True,
    "sameCommit": clean["deterministicSourceMetadata"]["commitFull"] == data["deterministicSourceMetadata"]["commitFull"],
    "result": "clean-source-recorded",
    "cleanManifestPath": clean_manifest,
    "cleanSourcePath": clean["deterministicSourceMetadata"]["sourcePath"],
    "cleanSourceStateHash": clean["deterministicSourceMetadata"]["sourceStateHash"],
    "cleanBinarySha256": clean["volatileBuildMetadata"]["binarySha256"],
    "cleanCommitFull": clean["deterministicSourceMetadata"]["commitFull"],
    "cleanClassification": clean["sourceState"]["classification"],
})
data["releaseBoundary"]["requiredBeforeReleasePipeline"] = [
    "use the clean comparison only as smoke evidence unless the release-approved rebuild path is also run",
    *data["releaseBoundary"]["requiredBeforeReleasePipeline"],
]
with open(manifest, "w", encoding="utf-8") as handle:
    json.dump(data, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY
}

maybe_run_clean_comparison() {
  [ "$dirty" = "true" ] || return 0
  [ "${GETTOKENS_SIDECAR_SMOKE_COMPARE_CLEAN:-1}" = "1" ] || return 0
  [ "${GETTOKENS_SIDECAR_SMOKE_INTERNAL:-0}" != "1" ] || return 0

  local tmp_parent tmp_src clean_log clean_manifest clean_run_log
  tmp_parent="$(mktemp -d /private/tmp/gettokens-cliproxy-clean.XXXXXX)"
  tmp_src="$tmp_parent/src"
  clean_log="$OUT_DIR/cli-proxy-api-round26-smoke-clean-worktree.log"
  clean_run_log="$OUT_DIR/cli-proxy-api-round26-smoke-clean-comparison-run.log"
  clean_manifest="$OUT_DIR/cli-proxy-api-round26-smoke-clean-comparison-manifest.json"

  echo "== clean source comparison =="
  set +e
  git -C "$ROOT" worktree add --detach "$tmp_src" "$commit_full" >"$clean_log" 2>&1
  local add_status=$?
  set -e
  if [ $add_status -ne 0 ]; then
    update_clean_comparison_unavailable "git worktree add failed; see $clean_log"
    rm -rf "$tmp_parent"
    return 0
  fi

  set +e
  GETTOKENS_SIDECAR_SMOKE_SOURCE_DIR="$tmp_src" \
    GETTOKENS_SIDECAR_SMOKE_OUT="$OUT_DIR" \
    GETTOKENS_SIDECAR_SMOKE_LABEL="clean-comparison" \
    GETTOKENS_SIDECAR_SMOKE_COMPARISON_ROLE="clean-comparison" \
    GETTOKENS_SIDECAR_SMOKE_COMPARE_CLEAN=0 \
    GETTOKENS_SIDECAR_SMOKE_INTERNAL=1 \
    "$SCRIPT_PATH" >"$clean_run_log" 2>&1
  local clean_status=$?
  set -e

  if [ $clean_status -ne 0 ]; then
    update_clean_comparison_unavailable "clean comparison smoke failed; see $clean_run_log"
  elif [ ! -f "$clean_manifest" ]; then
    update_clean_comparison_unavailable "clean comparison manifest was not produced at $clean_manifest"
  else
    update_clean_comparison_available "$clean_manifest"
    echo "clean comparison manifest: $clean_manifest"
  fi

  git -C "$ROOT" worktree remove "$tmp_src" >>"$clean_log" 2>&1 || true
  rm -rf "$tmp_parent"
}

maybe_run_clean_comparison

cat <<EOF
smoke ok
source: $ROOT
commit: ${commit}${dirty_suffix}
binary: $binary
help: $help_log
sha256: $sha_file
manifest: $manifest
testOnly: true
notReleaseArtifact: true
releasePipelineEligible: false
EOF
