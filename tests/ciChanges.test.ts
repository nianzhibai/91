import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";
import { classifyChanges, detectChanges } from "../.github/scripts/ci-changes.mjs";

test("documentation-only changes skip builds while deployment and unknown inputs run everything", () => {
  const all = { frontend: true, backend: true, images: true };
  assert.deepEqual(classifyChanges(["README.md", "backend/README.md", "backend/docs/CRAWLER_PROTOCOL.md", "AGENTS.md"]),
    { frontend: false, backend: false, images: false });
  for (const path of ["install.sh", "docker-entrypoint.sh", "scripts/build-release.sh", "Dockerfile", ".github/workflows/ci.yml", ".github/scripts/release.mjs", "new-build-input"]) {
    assert.deepEqual(classifyChanges([path]), all, path);
  }
});

test("source changes cover cross-language assertions without rebuilding images for tests alone", () => {
  assert.deepEqual(classifyChanges(["src/main.tsx"]), { frontend: true, backend: false, images: true });
  assert.deepEqual(classifyChanges(["backend/internal/api/admin.go"]), { frontend: true, backend: true, images: true });
  assert.deepEqual(classifyChanges(["backend/config.example.yaml"]), { frontend: true, backend: true, images: true });
  assert.deepEqual(classifyChanges(["backend/cmd/server/main_test.go"]), { frontend: true, backend: true, images: false });
  assert.deepEqual(classifyChanges(["tests/deploymentMigration.test.ts"]), { frontend: true, backend: false, images: false });
});

test("release, manual and unresolved change detection always run all checks", () => {
  const all = { frontend: true, backend: true, images: true };
  for (const env of [{ FORCE_ALL: "true" }, { GITHUB_REF_TYPE: "tag" }, { GITHUB_EVENT_NAME: "workflow_dispatch" }, { GITHUB_EVENT_PATH: "/missing/ci-event.json" }]) {
    assert.deepEqual(detectChanges(env), all);
  }
});

test("renaming runtime source into documentation still runs checks for the removed source", (t) => {
  const directory = mkdtempSync(join(tmpdir(), "91-ci-diff-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const git = (...args: string[]) => {
    const result = spawnSync("git", args, { cwd: directory, encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  git("init", "-q");
  git("config", "user.name", "CI test");
  git("config", "user.email", "ci@example.invalid");
  writeFileSync(join(directory, "install.sh"), "installation source\n");
  git("add", "."); git("commit", "-qm", "Initial source");
  const before = git("rev-parse", "HEAD");
  git("mv", "install.sh", "README.md"); git("commit", "-qm", "Move source");
  const event = join(directory, "event.json");
  writeFileSync(event, JSON.stringify({ before }));
  const output = join(directory, "outputs");
  const result = spawnSync(process.execPath, [fileURLToPath(new URL("../.github/scripts/ci-changes.mjs", import.meta.url))], {
    cwd: directory, encoding: "utf8", env: { ...process.env, FORCE_ALL: "false", GITHUB_EVENT_NAME: "push", GITHUB_REF_TYPE: "branch",
      GITHUB_SHA: git("rev-parse", "HEAD"), GITHUB_EVENT_PATH: event, GITHUB_OUTPUT: output },
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(readFileSync(output, "utf8"), "frontend=true\nbackend=true\nimages=true\n");
});

test("Check rejects failed, canceled and unexplained skipped jobs", (t) => {
  const workflow = parse(readFileSync(new URL("../.github/workflows/checks.yml", import.meta.url), "utf8"));
  const directory = mkdtempSync(join(tmpdir(), "91-ci-check-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const run = (frontend: string, backend: string, affected = "true", changes = "success") => spawnSync("bash", ["-e", "-o", "pipefail", "-c", workflow.jobs.check.steps[0].run], {
    encoding: "utf8", env: { ...process.env, GITHUB_STEP_SUMMARY: join(directory, "summary"), RESULTS: JSON.stringify({
      changes: { result: changes, outputs: { frontend: affected, backend: affected } }, frontend: { result: frontend }, backend: { result: backend },
    }) },
  });
  assert.equal(run("success", "success").status, 0);
  assert.equal(run("skipped", "skipped", "false").status, 0);
  for (const status of ["failure", "cancelled", "skipped"]) {
    assert.notEqual(run(status, "success").status, 0, status);
    assert.notEqual(run("success", status).status, 0, status);
  }
  assert.notEqual(run("skipped", "skipped", "false", "failure").status, 0);
});

test("the branch-protection Check also rejects failed or unexpectedly skipped images", (t) => {
  const workflow = parse(readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8"));
  const directory = mkdtempSync(join(tmpdir(), "91-ci-images-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const run = (checks: string, images: string, affected = "true") => spawnSync("bash", ["-e", "-o", "pipefail", "-c", workflow.jobs.check.steps[0].run], {
    encoding: "utf8", env: { ...process.env, GITHUB_STEP_SUMMARY: join(directory, "summary"), RESULTS: JSON.stringify({
      checks: { result: checks, outputs: { images: affected } }, images: { result: images },
    }) },
  });
  assert.equal(run("success", "success").status, 0);
  assert.equal(run("success", "skipped", "false").status, 0);
  for (const status of ["failure", "cancelled", "skipped"]) {
    assert.notEqual(run(status, "success").status, 0, status);
    assert.notEqual(run("success", status).status, 0, status);
  }
});
