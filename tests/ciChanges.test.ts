import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test, { type TestContext } from "node:test";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";
import { classifyChanges, detectChanges } from "../.github/scripts/ci-changes.mjs";

test("documentation-only changes skip builds while deployment and unknown inputs run everything", () => {
  const all = { frontend: true, backend: true, images: true };
  assert.deepEqual(classifyChanges(["README.md", "backend/README.md", "docs/README.md", "docs/CRAWLER_PROTOCOL.md", "AGENTS.md"]),
    { frontend: false, backend: false, images: false });
  for (const path of ["install.sh", "docker-entrypoint.sh", "scripts/build-release.sh", "Dockerfile", ".github/workflows/ci.yml", ".github/scripts/release.mjs", "new-build-input"]) {
    assert.deepEqual(classifyChanges([path]), all, path);
  }
});

test("source changes cover cross-language assertions and rebuild images only for image inputs", () => {
  for (const path of ["package.json", "package-lock.json", ".nvmrc", ".npmrc", "sdk/src/index.ts", "examples/custom-frontend/src/main.ts"]) {
    assert.deepEqual(classifyChanges([path]), { frontend: true, backend: false, images: false }, path);
  }
  assert.deepEqual(classifyChanges(["backend/internal/api/admin.go"]), { frontend: true, backend: true, images: true });
  assert.deepEqual(classifyChanges(["backend/config.example.yaml"]), { frontend: true, backend: true, images: true });
  for (const path of ["backend/cmd/server/main_test.go", "backend/internal/api/testdata/fixture.json", "backend/integration/restore_preview_test.go"]) {
    assert.deepEqual(classifyChanges([path]), { frontend: true, backend: true, images: false }, path);
  }
  assert.deepEqual(classifyChanges(["tests/deploymentMigration.test.ts"]), { frontend: true, backend: false, images: false });
});

test("manual and unresolved change detection always run all checks", () => {
  const all = { frontend: true, backend: true, images: true };
  for (const env of [{ GITHUB_EVENT_NAME: "workflow_dispatch" }, { GITHUB_EVENT_NAME: "pull_request", GITHUB_EVENT_PATH: "/missing/ci-event.json" },
    { GITHUB_EVENT_NAME: "push" }]) {
    assert.deepEqual(detectChanges(env), all);
  }
});

// A local repository and a fake `gh` reporting the last successful run on main.
function pushFixture(t: TestContext) {
  const root = mkdtempSync(join(tmpdir(), "91-ci-diff-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const directory = join(root, "repository");
  const bin = join(root, "bin");
  mkdirSync(directory); mkdirSync(bin);
  const git = (...args: string[]) => {
    const result = spawnSync("git", args, { cwd: directory, encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  git("init", "-q");
  git("config", "user.name", "CI test");
  git("config", "user.email", "ci@example.invalid");
  const commit = (path: string, content: string) => {
    mkdirSync(dirname(join(directory, path)), { recursive: true });
    writeFileSync(join(directory, path), content);
    git("add", "-A"); git("commit", "-qm", `Change ${path}`);
    return git("rev-parse", "HEAD");
  };
  writeFileSync(join(bin, "gh"), `#!/bin/sh\nprintf '%s\\n' "$*" >> "$GH_CALLS"\nprintf '{"workflow_runs":[{"head_sha":"%s"}]}' "$LAST_SUCCESS"\n`, { mode: 0o755 });
  const detect = (lastSuccess: string) => {
    const output = join(root, "outputs");
    rmSync(output, { force: true });
    const result = spawnSync(process.execPath, [fileURLToPath(new URL("../.github/scripts/ci-changes.mjs", import.meta.url))], {
      cwd: directory, encoding: "utf8", env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, GITHUB_EVENT_NAME: "push",
        GITHUB_REPOSITORY: "example/91", GITHUB_WORKFLOW_REF: "example/91/.github/workflows/ci.yml@refs/heads/main", GITHUB_REF_NAME: "main",
        GITHUB_SHA: git("rev-parse", "HEAD"), GITHUB_OUTPUT: output, LAST_SUCCESS: lastSuccess, GH_CALLS: join(root, "gh-calls") },
    });
    assert.equal(result.status, 0, result.stderr);
    return readFileSync(output, "utf8");
  };
  return { git, commit, detect, calls: () => readFileSync(join(root, "gh-calls"), "utf8") };
}

test("renaming runtime source into documentation still runs checks for the removed source", (t) => {
  const f = pushFixture(t);
  const published = f.commit("install.sh", "installation source\n");
  f.git("mv", "install.sh", "README.md"); f.git("commit", "-qm", "Move source");
  assert.equal(f.detect(published), "frontend=true\nbackend=true\nimages=true\n");
  assert.match(f.calls(), /actions\/workflows\/ci\.yml\/runs\?branch=main&event=push&status=success/);
});

test("pushes rebuild changes from replaced or failed runs since the last successful run", (t) => {
  const f = pushFixture(t);
  const published = f.commit("backend/main.go", "package main\n");
  f.commit("backend/main.go", "package main // changed\n");
  const docs = f.commit("README.md", "docs\n");
  assert.equal(f.detect(docs), "frontend=false\nbackend=false\nimages=false\n");
  assert.equal(f.detect(published), "frontend=true\nbackend=true\nimages=true\n");
});

test("pushes without a usable last successful run run everything", (t) => {
  const f = pushFixture(t);
  f.commit("README.md", "docs\n");
  const all = "frontend=true\nbackend=true\nimages=true\n";
  assert.equal(f.detect(""), all, "No successful run yet");
  f.git("checkout", "-q", "-b", "other");
  const unrelated = f.commit("README.md", "other docs\n");
  f.git("checkout", "-q", "-");
  f.commit("docs/guide.md", "guide\n");
  assert.equal(f.detect(unrelated), all, "Last success is not an ancestor");
});

test("Check rejects failed, canceled and unrequested skipped jobs", (t) => {
  const workflow = parse(readFileSync(new URL("../.github/workflows/checks.yml", import.meta.url), "utf8"));
  const directory = mkdtempSync(join(tmpdir(), "91-ci-check-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const run = (frontend: string, backend: string, requested = true) => spawnSync("bash", ["-e", "-o", "pipefail", "-c", workflow.jobs.check.steps[0].run], {
    encoding: "utf8", env: { ...process.env, GITHUB_STEP_SUMMARY: join(directory, "summary"),
      REQUESTED: JSON.stringify({ frontend: requested, backend: requested }),
      RESULTS: JSON.stringify({ frontend: { result: frontend }, backend: { result: backend } }) },
  });
  assert.equal(run("success", "success").status, 0);
  assert.equal(run("skipped", "skipped", false).status, 0);
  for (const status of ["failure", "cancelled", "skipped"]) {
    assert.notEqual(run(status, "success").status, 0, status);
    assert.notEqual(run("success", status).status, 0, status);
  }
});

test("the branch-protection Check rejects failed or unexpectedly skipped frontend preparation and images", (t) => {
  const workflow = parse(readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8"));
  const directory = mkdtempSync(join(tmpdir(), "91-ci-images-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const run = (checks: string, frontend: string, images: string, affected = "true", changes = "success") => spawnSync("bash", ["-e", "-o", "pipefail", "-c", workflow.jobs.check.steps[0].run], {
    encoding: "utf8", env: { ...process.env, GITHUB_STEP_SUMMARY: join(directory, "summary"), RESULTS: JSON.stringify({
      changes: { result: changes, outputs: { images: affected } }, checks: { result: checks }, frontend: { result: frontend }, images: { result: images },
    }) },
  });
  assert.equal(run("success", "success", "success").status, 0);
  assert.equal(run("success", "skipped", "skipped", "false").status, 0);
  for (const status of ["failure", "cancelled", "skipped"]) {
    assert.notEqual(run(status, "success", "success").status, 0, status);
    assert.notEqual(run("success", status, "success").status, 0, status);
    assert.notEqual(run("success", "success", status).status, 0, status);
    assert.notEqual(run("success", status, "skipped").status, 0, status);
  }
  assert.notEqual(run("success", "skipped", "skipped", "false", "failure").status, 0);
});
