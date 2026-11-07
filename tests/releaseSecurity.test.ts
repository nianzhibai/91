import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { parse } from "yaml";

const packageJson = JSON.parse(
  readFileSync(new URL("../package.json", import.meta.url), "utf8")
) as {
  allowScripts?: Record<string, boolean>;
  engines?: Record<string, string>;
  packageManager?: string;
  scripts?: Record<string, string>;
};
const nodeVersion = readFileSync(
  new URL("../.nvmrc", import.meta.url),
  "utf8"
).trim();
const dockerfile = readFileSync(
  new URL("../Dockerfile", import.meta.url),
  "utf8"
);
const dockerWorkflow = readFileSync(
  new URL("../.github/workflows/docker-build.yml", import.meta.url),
  "utf8"
);
const releaseWorkflow = readFileSync(
  new URL("../.github/workflows/release.yml", import.meta.url),
  "utf8"
);
const checksWorkflow = readFileSync(new URL("../.github/workflows/checks.yml", import.meta.url), "utf8");
const ciWorkflow = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
const releaseScript = readFileSync(
  new URL("../scripts/build-release.sh", import.meta.url),
  "utf8"
);

test("frontend verification rejects every npm audit finding before release", () => {
  assert.deepEqual(packageJson.allowScripts, {
    "esbuild@0.28.1": true,
    "fsevents@2.3.3": true,
  });
  assert.equal(packageJson.scripts?.audit, "npm audit --audit-level=info");
  assert.equal(
    packageJson.scripts?.check,
    "npm run audit && npm run lint && npm test"
  );
  assert.equal(packageJson.scripts?.verify, "npm run check && npm run build");
  assert.match(
    releaseScript,
    /npm --prefix "\$ROOT_DIR" run verify/
  );
});

test("container and release builds use supported Node and run checks", () => {
  assert.match(nodeVersion, /^\d+\.\d+\.\d+$/);
  assert.equal(packageJson.engines?.node, `^${nodeVersion}`);
  assert.match(packageJson.packageManager ?? "", /^npm@\d+\.\d+\.\d+$/);
  assert.equal(packageJson.engines?.npm, `^${packageJson.packageManager!.slice(4)}`);
  const frontendStage = dockerfile.match(/^FROM(?:\s+--platform=\S+)?\s+(\S+)\s+AS\s+frontend\s*$/im);
  assert.equal(frontendStage?.[1], `node:${nodeVersion}-slim`, "Frontend image must use the pinned Node version");
  assert.match(dockerfile, /^COPY package\.json package-lock\.json \.npmrc \.\/$/m);
  assert.match(checksWorkflow, /node-version-file: \.nvmrc/);
  assert.match(releaseWorkflow, /node-version-file: \.nvmrc/);
  const checks = parse(checksWorkflow).jobs;
  assert.ok(checks.frontend.steps.some((step: { run?: string }) => step.run?.includes("npm run check")));
  assert.ok(checks.backend.steps.some((step: { run?: string }) => step.run?.includes("go test -count=1 -timeout=15m ./...")));
  assert.deepEqual(checks.check.needs, ["changes", "frontend", "backend"]);
  const ci = parse(ciWorkflow).jobs;
  assert.equal(ci.images.needs, "checks");
  assert.deepEqual(ci.publish.needs, ["checks", "images"]);
  const release = parse(releaseWorkflow).jobs;
  assert.equal(release.checks.with["force-all"], true);
  assert.deepEqual(release.images.needs, ["preflight", "checks"]);
  assert.deepEqual(release.packages.needs, ["preflight", "checks"]);
  assert.deepEqual(release.publish.needs, ["preflight", "checks", "images", "packages"]);
  assert.deepEqual(Object.keys(parse(dockerWorkflow).on), ["workflow_call"], "Image builds must run through a checked caller");
});
