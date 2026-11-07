import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test, { type TestContext } from "node:test";
import { fileURLToPath } from "node:url";
import { parseVersion, promotionTags } from "../.github/scripts/release.mjs";

const sha = "a".repeat(40);
const publisher = fileURLToPath(new URL("../.github/scripts/release.mjs", import.meta.url));
type Call = { program: string; args: string[] };
type State = { calls: Call[]; release: any; releases: any[]; sums: string };

// All GitHub, Git and Docker commands stay local. State survives retries so tests
// exercise the same publication script and failure boundaries as the workflow.
const probe = `#!${process.execPath}
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const env = process.env;
const state = JSON.parse(fs.readFileSync(env.MOCK_STATE, 'utf8'));
const program = path.basename(process.argv[1]);
const args = process.argv.slice(2);
state.calls.push({program, args});
function finish(output = '', status = 0) {
  fs.writeFileSync(env.MOCK_STATE, JSON.stringify(state));
  if (output) (status ? process.stderr : process.stdout).write(output);
  process.exit(status);
}
if (program === 'git') {
  if (args[0] === 'ls-remote') finish((env.REMOTE_SHA || env.GITHUB_SHA) + '\\trefs/tags/' + env.RELEASE_TAG + '\\n');
  if (args[0] === 'tag') finish('Release notes\\n');
}
if (program === 'docker') {
  if (env.FAIL === (env.SOURCE_TAG ? 'channels' : 'images')) finish('registry unavailable', 1);
  finish();
}
if (program === 'gh') {
  if (args[0] === 'api') {
    if (env.FAIL === 'api') finish('gh: unavailable (HTTP 503)', 1);
    if (args.includes('--paginate')) finish(JSON.stringify([[...state.releases, state.release].filter(Boolean)]));
    if (!state.release) finish('gh: Not Found (HTTP 404)', 1);
    finish(JSON.stringify(state.release));
  }
  if (args[0] === 'release') {
    if (args[1] === 'create') {
      if (state.release) finish('release already exists', 1);
      state.release = {tag_name: args[2], draft: args.includes('--draft'), prerelease: args.includes('--prerelease'),
        target_commitish: args[args.indexOf('--target') + 1], assets: []};
      finish();
    }
    if (args[1] === 'upload') {
      if (env.FAIL === 'upload') finish('upload interrupted', 1);
      if (!state.release.draft) finish('cannot replace public release assets', 1);
      state.release.assets = args.filter(arg => arg.startsWith(env.RELEASE_DIR + '/')).map(file => {
        const bytes = fs.readFileSync(file);
        const name = path.basename(file);
        if (name === 'SHA256SUMS') state.sums = bytes.toString();
        return {name, state: 'uploaded', size: bytes.length, digest: 'sha256:' + crypto.createHash('sha256').update(bytes).digest('hex')};
      });
      if (env.FAIL === 'checksum') state.release.assets[0].digest = 'sha256:bad';
      finish();
    }
    if (args[1] === 'download') finish(state.sums);
    if (args[1] === 'edit') {
      if (args.includes('--draft=false')) state.release.draft = false;
      if (args.includes('--latest=true')) state.release.latest = true;
      finish();
    }
  }
}
finish('Unexpected command: ' + JSON.stringify({program,args}), 1);
`;

function fixture(t: TestContext, tag = "v1.2.3", releases: any[] = []) {
  const directory = mkdtempSync(join(tmpdir(), "91-release-workflow-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const bin = join(directory, "bin");
  const releaseDir = join(directory, "release");
  mkdirSync(bin); mkdirSync(releaseDir);
  for (const program of ["git", "gh", "docker"]) writeFileSync(join(bin, program), probe, { mode: 0o755 });
  for (const arch of ["amd64", "arm64"]) writeFileSync(join(releaseDir, `91-linux-${arch}.tar.gz`), `archive for ${arch}`);
  for (const [index, arch] of ["amd64", "arm64", "rockchip-arm64"].entries()) {
    const path = join(directory, "digests", `docker-digests-${arch}`);
    mkdirSync(path, { recursive: true });
    writeFileSync(join(path, "digest.txt"), `sha256:${String(index).repeat(64)}\n`);
  }
  const stateFile = join(directory, "state.json");
  writeFileSync(stateFile, JSON.stringify({ calls: [], release: null, releases, sums: "" }));
  return {
    directory, releaseDir,
    state: (): State => JSON.parse(readFileSync(stateFile, "utf8")),
    run(overrides: Record<string, string> = {}) {
      return spawnSync(process.execPath, [publisher, "publish"], { encoding: "utf8", env: {
        ...process.env, PATH: `${bin}:${process.env.PATH}`, MOCK_STATE: stateFile,
        RELEASE_TAG: tag, RELEASE_DIR: releaseDir, DIGEST_DIR: join(directory, "digests"),
        GITHUB_REPOSITORY: "example/91", GITHUB_SHA: sha, IMAGE: "ghcr.io/example/91", GITHUB_STEP_SUMMARY: join(directory, "summary"),
        ...overrides,
      } });
    },
  };
}

function release(tag: string, options = {}) {
  return { tag_name: tag, draft: false, prerelease: false, ...options };
}

test("release tags distinguish stable and prerelease versions and reject ambiguous versions", () => {
  assert.equal(parseVersion("v1.2.3").prerelease, false);
  assert.equal(parseVersion("v1.2.3-rc.1").prerelease, true);
  for (const tag of ["v1", "v1.2", "v01.2.3", "v1.2.3-rc.01", "main", "v1.2.3+build"]) assert.throws(() => parseVersion(tag));
});

test("older releases cannot roll channels back, but may advance their own minor line", () => {
  assert.deepEqual(promotionTags(parseVersion("v1.2.3"), []), ["1.2", "1", "latest", "stable"]);
  assert.deepEqual(promotionTags(parseVersion("v1.2.3-rc.1"), []), []);
  assert.deepEqual(promotionTags(parseVersion("v1.2.3"), [release("v1.2.4")]), []);
  assert.deepEqual(promotionTags(parseVersion("v1.2.3"), [release("v1.3.0")]), ["1.2"]);
  assert.deepEqual(promotionTags(parseVersion("v1.2.10"), [release("v1.2.9"), release("v2.0.0", { draft: true }), release("v3.0.0-rc.1", { prerelease: true })]), ["1.2", "1", "latest", "stable"]);
});

test("release publishes verified draft assets and version images before shared channels", (t) => {
  const f = fixture(t);
  const result = f.run();
  assert.equal(result.status, 0, result.stderr);
  const { release: published, calls } = f.state();
  assert.equal(published.draft, false);
  assert.equal(published.latest, true);
  assert.equal(published.target_commitish, sha);
  assert.deepEqual(published.assets.map((asset: any) => asset.name), ["91-linux-amd64.tar.gz", "91-linux-arm64.tar.gz", "SHA256SUMS"]);
  const upload = calls.findIndex(call => call.args[1] === "upload");
  const firstImage = calls.findIndex(call => call.program === "docker");
  const publish = calls.findIndex(call => call.args.includes("--draft=false"));
  const promote = calls.findIndex(call => call.args.includes("ghcr.io/example/91:stable"));
  assert.ok(upload < firstImage && firstImage < publish && publish < promote);
  assert.ok(!calls.some(call => call.args.includes("delete")));
});

test("prereleases publish only version images and never change stable channels", (t) => {
  const f = fixture(t, "v1.2.3-rc.1");
  const result = f.run();
  assert.equal(result.status, 0, result.stderr);
  const { calls, release: published } = f.state();
  assert.equal(published.prerelease, true);
  assert.equal(calls.filter(call => call.program === "docker").length, 2);
  assert.ok(!calls.some(call => call.args.includes("--latest=true") || call.args.includes("ghcr.io/example/91:stable")));
});

for (const failure of ["upload", "checksum", "images"]) {
  test(`failed ${failure} keeps the release private and can resume without deletion`, (t) => {
    const f = fixture(t);
    assert.notEqual(f.run({ FAIL: failure }).status, 0);
    assert.equal(f.state().release.draft, true);
    assert.ok(!f.state().calls.some(call => call.args.includes("ghcr.io/example/91:stable")));
    if (failure !== "images") assert.equal(f.state().calls.filter(call => call.program === "docker").length, 0);
    const retry = f.run();
    assert.equal(retry.status, 0, retry.stderr);
    assert.equal(f.state().calls.filter(call => call.args[1] === "create").length, 1);
    assert.equal(f.state().release.draft, false);
  });
}

test("retry after channel failure reuses public assets and published images", (t) => {
  const f = fixture(t);
  assert.notEqual(f.run({ FAIL: "channels" }).status, 0);
  const before = f.state();
  assert.equal(before.release.draft, false);
  writeFileSync(join(f.releaseDir, "91-linux-amd64.tar.gz"), "different rebuilt bytes");
  const retry = f.run();
  assert.equal(retry.status, 0, retry.stderr);
  const after = f.state();
  assert.deepEqual(after.release.assets, before.release.assets);
  const calls = after.calls.slice(before.calls.length);
  assert.ok(!calls.some(call => ["upload", "create", "delete"].includes(call.args[1])));
  assert.ok(calls.filter(call => call.program === "docker").every(call => call.args.at(-1)?.includes(":v1.2.3")));
  assert.equal(after.release.latest, true);
});

test("missing archives, moved tags and API failures cannot create a release", (t) => {
  const missing = fixture(t);
  rmSync(join(missing.releaseDir, "91-linux-arm64.tar.gz"));
  assert.notEqual(missing.run().status, 0);
  assert.equal(missing.state().release, null);
  const moved = fixture(t);
  assert.notEqual(moved.run({ REMOTE_SHA: "b".repeat(40) }).status, 0);
  assert.equal(moved.state().calls.filter(call => call.program !== "git").length, 0);
  const unavailable = fixture(t);
  assert.notEqual(unavailable.run({ FAIL: "api" }).status, 0);
  assert.equal(unavailable.state().release, null);
});

test("publishing an older tag does not update latest or stable", (t) => {
  const f = fixture(t, "v1.2.3", [release("v1.2.4")]);
  const result = f.run();
  assert.equal(result.status, 0, result.stderr);
  assert.ok(!f.state().calls.some(call => call.args.includes("--latest=true") || call.args.includes("ghcr.io/example/91:stable")));
});
