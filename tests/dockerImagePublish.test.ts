import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test, { type TestContext } from "node:test";
import { parse } from "yaml";

const workflow = parse(readFileSync(
  new URL("../.github/workflows/docker-build.yml", import.meta.url),
  "utf8"
));
const publishStep = workflow.jobs.publish.steps.find(
  (step: { run?: string }) => step.run?.includes("docker buildx imagetools create")
);
assert.ok(publishStep, "Expected a manifest publication step");

const image = "ghcr.io/example/91";
const digests = {
  amd64: `sha256:${"a".repeat(64)}`,
  arm64: `sha256:${"b".repeat(64)}`,
  "rockchip-arm64": `sha256:${"c".repeat(64)}`,
};

function publication(t: TestContext) {
  const directory = mkdtempSync(join(tmpdir(), "docker-publish-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const bin = join(directory, "bin");
  const argsFile = join(directory, "docker-args");
  mkdirSync(bin);
  // Capture the actual command arguments without accessing a registry.
  writeFileSync(join(bin, "docker"), `#!/bin/sh
printf '%s\\0' "$@" >> "$DOCKER_ARGS_FILE"
printf '\\n' >> "$DOCKER_ARGS_FILE"
`, { mode: 0o755 });

  function digestPath(arch: string) {
    return join(directory, "digests", `docker-digests-${arch}`, "digest.txt");
  }

  for (const [arch, digest] of Object.entries(digests)) {
    mkdirSync(join(directory, "digests", `docker-digests-${arch}`), { recursive: true });
    writeFileSync(digestPath(arch), `${digest}\n`);
  }

  return {
    digestPath,
    run(tags: string) {
      const result = spawnSync("bash", ["-e", "-o", "pipefail", "-c", publishStep.run], {
        encoding: "utf8",
        env: {
          ...process.env,
          PATH: `${bin}:${process.env.PATH ?? ""}`,
          RUNNER_TEMP: directory,
          DOCKER_ARGS_FILE: argsFile,
          IMAGE: image,
          TAGS: tags,
        },
      });
      assert.ifError(result.error);
      const calls = existsSync(argsFile)
        ? readFileSync(argsFile, "utf8").trimEnd().split("\n").map((line) => line.split("\0").slice(0, -1))
        : [];
      return { ...result, args: calls[0] ?? null, calls };
    },
  };
}

test("manifest publication combines both architectures under every requested tag", (t) => {
  const fixture = publication(t);
  const tags = ["v1.2.3", "1.2.3", "1.2", "sha-abcdef0", "latest", "stable"];
  const result = fixture.run(`\n${tags.map((tag) => `${image}:${tag}`).join("\n")}\n\n`);

  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.args, [
    "buildx", "imagetools", "create",
    ...tags.flatMap((tag) => ["--tag", `${image}:${tag}`]),
    `${image}@${digests.amd64}`,
    `${image}@${digests.arm64}`,
  ]);
  assert.deepEqual(result.calls[1], [
    "buildx", "imagetools", "create",
    ...tags.flatMap((tag) => ["--tag", `${image}:${tag}-rockchip`]),
    `${image}@${digests["rockchip-arm64"]}`,
  ]);
});

for (const arch of ["amd64", "arm64", "rockchip-arm64"]) {
  test(`manifest publication rejects a missing ${arch} image before updating tags`, (t) => {
    const fixture = publication(t);
    rmSync(fixture.digestPath(arch));
    const result = fixture.run(`${image}:main`);

    assert.notEqual(result.status, 0);
    assert.equal(result.args, null);
  });

  test(`manifest publication rejects a malformed ${arch} digest before updating tags`, (t) => {
    const fixture = publication(t);
    writeFileSync(fixture.digestPath(arch), "sha256:incomplete\n");
    const result = fixture.run(`${image}:main`);

    assert.notEqual(result.status, 0);
    assert.match(result.stderr, new RegExp(`Invalid image digest for ${arch}`));
    assert.equal(result.args, null);
  });
}

test("manifest publication requires at least one image tag", (t) => {
  const fixture = publication(t);
  const result = fixture.run("\n\n");

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /No image tags to publish/);
  assert.equal(result.args, null);
});

test("Docker builds isolate the Rockchip ARM64 runtime from standard images", () => {
  const builds = workflow.jobs.build.strategy.matrix.include;
  assert.deepEqual(builds.map(({ id, arch, target }: { id: string; arch: string; target: string }) =>
    [id, arch, target]), [
    ["amd64", "amd64", "runtime"],
    ["arm64", "arm64", "runtime"],
    ["rockchip-arm64", "arm64", "rockchip"],
  ]);
});
