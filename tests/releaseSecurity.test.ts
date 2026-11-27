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
const goMod = readFileSync(
  new URL("../backend/go.mod", import.meta.url),
  "utf8"
);
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
const auditWorkflow = readFileSync(new URL("../.github/workflows/audit.yml", import.meta.url), "utf8");
const releaseScript = readFileSync(
  new URL("../scripts/build-release.sh", import.meta.url),
  "utf8"
);

test("SDK verification and separate dependency audit keep checks reliable and builds verified", () => {
  assert.deepEqual(packageJson.allowScripts, {
    "esbuild@0.28.1": true,
    "fsevents@2.3.3": true,
  });
  assert.equal(packageJson.scripts?.audit, "npm audit --audit-level=info");
  assert.equal(
    packageJson.scripts?.check,
    "npm run lint && npm test"
  );
  assert.equal(
    packageJson.scripts?.verify,
    "npm run check && npm run build && npm run build:example"
  );
  const audit = parse(auditWorkflow).jobs;
  assert.ok(audit["npm-audit"].steps.some((step: { run?: string }) => step.run?.includes("npm run audit")));
  assert.match(
    releaseScript,
    /prepare-frontend\.sh/
  );
});

test("container and release builds use matching toolchains and run checks", () => {
  assert.match(nodeVersion, /^\d+\.\d+\.\d+$/);
  assert.equal(packageJson.engines?.node, `^${nodeVersion}`);
  assert.match(packageJson.packageManager ?? "", /^npm@\d+\.\d+\.\d+$/);
  assert.equal(packageJson.engines?.npm, `^${packageJson.packageManager!.slice(4)}`);

  const goModVersion = goMod.match(/^go\s+(\d+\.\d+\.\d+)\s*$/m)?.[1];
  assert.ok(goModVersion, "backend/go.mod must pin a Go version");
  assert.match(dockerfile, new RegExp(`^FROM\\s+--platform=\\$BUILDPLATFORM\\s+golang:${goModVersion.replaceAll(".", "\\.")}-bookworm\\s+AS\\s+backend\\s*$`, "m"));

  const frontendStage = dockerfile.match(/^FROM(?:\s+--platform=\S+)?\s+(\S+)\s+AS\s+frontend\s*$/im);
  assert.equal(frontendStage?.[1], "backend", "Frontend release verification uses the backend helper");
  assert.match(dockerfile, /^COPY frontend\.lock\.json /m);
  assert.match(checksWorkflow, /node-version-file: \.nvmrc/);

  const checks = parse(checksWorkflow).jobs;
  assert.ok(checks.frontend.steps.some((step: { run?: string }) => step.run?.includes("npm run verify")));
  assert.ok(checks.frontend.steps.some((step: { run?: string }) => step.run?.includes("shellcheck")));
  assert.ok(checks.backend.steps.some((step: { run?: string }) => step.run?.includes("go vet ./...") && step.run?.includes("go test -count=1 -timeout=15m ./...")));
  assert.deepEqual(checks.check.needs, ["frontend", "backend"]);

  // Images build alongside the checks, but nothing is published until both succeed.
  const ci = parse(ciWorkflow);
  assert.equal(ci.jobs.checks.needs, "changes");
  assert.equal(ci.jobs["frontend-bundle"].needs, "changes");
  assert.deepEqual(ci.jobs.images.needs, ["changes", "frontend-bundle"]);
  assert.deepEqual(ci.jobs.publish.needs, ["changes", "checks", "images"]);
  assert.equal(ci.concurrency["cancel-in-progress"], "${{ github.event_name == 'pull_request' }}", "Main runs must not be canceled while publishing");

  const release = parse(releaseWorkflow).jobs;
  assert.deepEqual(release.checks.with, { frontend: true, backend: true });
  assert.equal(release["frontend-bundle"].needs, "preflight");
  assert.equal(release["frontend-bundle"].with["retention-days"], 30);
  assert.deepEqual(release.images.needs, ["preflight", "frontend-bundle"]);
  assert.equal(release.images.with["retention-days"], 30);
  assert.deepEqual(release.packages.needs, ["preflight", "frontend-bundle"]);
  assert.ok(!release.packages.steps.some((step: { uses?: string }) => step.uses?.startsWith("actions/setup-node")), "Linux release packages do not need Node.js");
  assert.deepEqual(release.publish.needs, ["preflight", "checks", "images", "packages"]);
  assert.equal(release.preflight.steps.find((step: { id?: string }) => step.id === "version").env.RELEASE_BRANCH,
    "${{ github.event.repository.default_branch }}");
  assert.deepEqual(Object.keys(parse(dockerWorkflow).on), ["workflow_call"], "Image builds must run through a checked caller");
});
