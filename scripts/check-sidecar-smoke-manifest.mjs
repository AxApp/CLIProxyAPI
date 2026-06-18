#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const scriptDir = path.dirname(new URL(import.meta.url).pathname);
const repoRoot = path.resolve(scriptDir, "..");
const latestManifestPath =
  "/private/tmp/gettokens-cliproxyapi-sidecar-smoke/cli-proxy-api-round26-smoke-manifest.json";
const fixtureManifestPath = path.join(
  repoRoot,
  "fixtures",
  "sidecar-smoke",
  "cli-proxy-api-round26-smoke-manifest.fixture.json",
);

function resolveManifestTarget(argument) {
  if (!argument || argument === "fixture") {
    return { mode: "fixture", manifestPath: fixtureManifestPath };
  }
  if (argument === "latest") {
    return { mode: "latest", manifestPath: latestManifestPath };
  }
  return { mode: "path", manifestPath: path.resolve(argument) };
}

function fail(message) {
  console.error(`manifest check failed: ${message}`);
  process.exit(1);
}

function readJSON(filePath) {
  let text;
  try {
    text = fs.readFileSync(filePath, "utf8");
  } catch (error) {
    fail(`cannot read ${filePath}: ${error.message}`);
  }
  try {
    return JSON.parse(text);
  } catch (error) {
    fail(`invalid JSON in ${filePath}: ${error.message}`);
  }
}

function get(data, dottedPath) {
  return dottedPath.split(".").reduce((current, segment) => {
    if (current && Object.prototype.hasOwnProperty.call(current, segment)) {
      return current[segment];
    }
    return undefined;
  }, data);
}

function requirePath(data, dottedPath, predicate = (value) => value !== undefined) {
  const value = get(data, dottedPath);
  if (!predicate(value)) {
    fail(`missing or invalid field: ${dottedPath}`);
  }
  return value;
}

function requireBoolean(data, dottedPath, expected) {
  const value = requirePath(data, dottedPath, (candidate) => typeof candidate === "boolean");
  if (value !== expected) {
    fail(`${dottedPath} expected ${expected}, got ${value}`);
  }
}

const target = resolveManifestTarget(process.argv[2]);
const manifest = readJSON(target.manifestPath);

requirePath(manifest, "manifestVersion", (value) => value === 2);
requirePath(manifest, "schema", (value) => value === "gettokens-sidecar-smoke-manifest.v2");
requirePath(manifest, "purpose", (value) => typeof value === "string" && value.length > 0);
requireBoolean(manifest, "testOnly", true);
requireBoolean(manifest, "notReleaseArtifact", true);
requireBoolean(manifest, "releasePipelineEligible", false);

const deterministicFields = [
  "deterministicSourceMetadata.sourcePath",
  "deterministicSourceMetadata.branch",
  "deterministicSourceMetadata.commitShort",
  "deterministicSourceMetadata.commitFull",
  "deterministicSourceMetadata.dirty",
  "deterministicSourceMetadata.dirtyStatus",
  "deterministicSourceMetadata.goModule",
  "deterministicSourceMetadata.sourceStateHash",
];

for (const field of deterministicFields) {
  requirePath(manifest, field);
}

requirePath(manifest, "deterministicSourceMetadata.dirty", (value) => typeof value === "boolean");
requirePath(manifest, "deterministicSourceMetadata.dirtyStatus", Array.isArray);
requirePath(
  manifest,
  "deterministicSourceMetadata.sourceStateHash",
  (value) => typeof value === "string" && /^[a-f0-9]{64}$/.test(value),
);

const sourceState = {
  classification: requirePath(manifest, "sourceState.classification", (value) => {
    return value === "clean-source" || value === "dirty-source";
  }),
  clean: requirePath(manifest, "sourceState.clean", (value) => typeof value === "boolean"),
  dirty: requirePath(manifest, "sourceState.dirty", (value) => typeof value === "boolean"),
  artifactClass: requirePath(manifest, "sourceState.artifactClass", (value) => value === "volatile-test-binary"),
};
requireBoolean(manifest, "sourceState.dirtyStatusEvidenceOnly", true);
requirePath(manifest, "sourceState.comparisonRole", (value) => {
  return typeof value === "string" && value.length > 0;
});

if (sourceState.classification === "dirty-source") {
  if (sourceState.clean !== false || sourceState.dirty !== true) {
    fail("dirty-source classification must set clean=false and dirty=true");
  }
} else if (sourceState.clean !== true || sourceState.dirty !== false) {
  fail("clean-source classification must set clean=true and dirty=false");
}

const volatileFields = [
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
];

for (const field of volatileFields) {
  requirePath(manifest, field);
}

requirePath(
  manifest,
  "volatileBuildMetadata.binarySha256",
  (value) => typeof value === "string" && /^[a-f0-9]{64}$/.test(value),
);

const recordedFields = {
  deterministic: requirePath(manifest, "reproducibilityBoundary.deterministicSourceFields", Array.isArray),
  volatile: requirePath(manifest, "reproducibilityBoundary.volatileBuildFields", Array.isArray),
};

for (const field of deterministicFields) {
  if (!recordedFields.deterministic.includes(field)) {
    fail(`deterministicSourceFields does not list ${field}`);
  }
}

for (const field of volatileFields) {
  if (!recordedFields.volatile.includes(field)) {
    fail(`volatileBuildFields does not list ${field}`);
  }
}

requireBoolean(manifest, "reproducibilityBoundary.binaryDeterministic", false);
requireBoolean(manifest, "reproducibilityBoundary.binarySha256Volatile", true);
requirePath(manifest, "reproducibilityBoundary.binaryVolatilityCauses", (value) => {
  return (
    Array.isArray(value) &&
    value.some((entry) => /BuildDate|timestamp/i.test(entry)) &&
    value.some((entry) => /binarySha256|sha256/i.test(entry))
  );
});
requireBoolean(manifest, "releaseBoundary.mayCopyIntoAppBundle", false);
requireBoolean(manifest, "releaseBoundary.mayTouchApplicationsGetTokens", false);
requireBoolean(manifest, "releaseBoundary.mayPublish", false);
requireBoolean(manifest, "releaseBoundary.dirtyStatusEvidenceOnly", true);
requirePath(manifest, "releaseBoundary.requiredBeforeReleasePipeline", (value) => {
  return Array.isArray(value) && value.length > 0;
});
requirePath(manifest, "releaseBoundary.requiredBeforeReleasePipeline", (value) => {
  return value.some((entry) => /clean/i.test(entry)) && value.some((entry) => /dirtyStatus/i.test(entry));
});

const comparison = {
  mode: requirePath(manifest, "sourceStateComparison.mode", (value) => {
    return ["dirty-with-clean-comparison", "clean-source-primary", "clean-comparison"].includes(value);
  }),
  cleanComparisonAvailable: requirePath(
    manifest,
    "sourceStateComparison.cleanComparisonAvailable",
    (value) => typeof value === "boolean",
  ),
  sameCommit: requirePath(manifest, "sourceStateComparison.sameCommit", (value) => typeof value === "boolean"),
  result: requirePath(manifest, "sourceStateComparison.result", (value) => {
    return typeof value === "string" && value.length > 0;
  }),
};

if (sourceState.classification === "dirty-source") {
  if (comparison.mode !== "dirty-with-clean-comparison") {
    fail("dirty-source manifest must use sourceStateComparison.mode=dirty-with-clean-comparison");
  }
  requirePath(
    manifest,
    "sourceStateComparison.dirtySourceStateHash",
    (value) => typeof value === "string" && /^[a-f0-9]{64}$/.test(value),
  );
  requirePath(
    manifest,
    "sourceStateComparison.dirtyBinarySha256",
    (value) => typeof value === "string" && /^[a-f0-9]{64}$/.test(value),
  );
}

if (comparison.cleanComparisonAvailable) {
  requireBoolean(manifest, "sourceStateComparison.sameCommit", true);
  requirePath(manifest, "sourceStateComparison.cleanManifestPath", (value) => {
    return typeof value === "string" && /clean-comparison-manifest\.json$|smoke-manifest\.json$/.test(value);
  });
  requirePath(manifest, "sourceStateComparison.cleanSourcePath", (value) => typeof value === "string" && value.length > 0);
  requirePath(
    manifest,
    "sourceStateComparison.cleanSourceStateHash",
    (value) => typeof value === "string" && /^[a-f0-9]{64}$/.test(value),
  );
  requirePath(
    manifest,
    "sourceStateComparison.cleanBinarySha256",
    (value) => typeof value === "string" && /^[a-f0-9]{64}$/.test(value),
  );
  requirePath(manifest, "sourceStateComparison.cleanCommitFull", (value) => {
    return typeof value === "string" && /^[a-f0-9]{40}$|^unknown$/.test(value);
  });
} else {
  requirePath(manifest, "sourceStateComparison.cleanUnavailableReason", (value) => {
    return typeof value === "string" && value.length > 0;
  });
}

console.log("manifest ok");
console.log(`mode=${target.mode}`);
console.log(`path=${path.resolve(target.manifestPath)}`);
console.log(`sha256=${manifest.volatileBuildMetadata.binarySha256}`);
console.log(`sourceStateHash=${manifest.deterministicSourceMetadata.sourceStateHash}`);
console.log(`dirty=${manifest.deterministicSourceMetadata.dirty}`);
console.log(`dirtyStatusEntries=${manifest.deterministicSourceMetadata.dirtyStatus.length}`);
console.log(`deterministicFields=${recordedFields.deterministic.length}`);
console.log(`volatileFields=${recordedFields.volatile.length}`);
console.log(`binarySha256Volatile=${manifest.reproducibilityBoundary.binarySha256Volatile}`);
console.log(`dirtyStatusEvidenceOnly=${manifest.releaseBoundary.dirtyStatusEvidenceOnly}`);
console.log(`sourceStateClassification=${sourceState.classification}`);
console.log(`artifactClass=${sourceState.artifactClass}`);
console.log(`cleanComparisonAvailable=${comparison.cleanComparisonAvailable}`);
console.log(`sourceStateComparisonMode=${comparison.mode}`);
