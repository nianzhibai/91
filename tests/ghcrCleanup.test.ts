import assert from "node:assert/strict";
import test from "node:test";
import { collectReferencedDigests, isShaOnlyVersion, selectVersionsToDelete } from "../.github/scripts/cleanup-ghcr.mjs";

const NOW = Date.parse("2026-11-28T00:00:00Z");
const DAYS_AGO = (days: number) => new Date(NOW - days * 24 * 60 * 60 * 1000).toISOString();

function version(id: number, digest: string, tags: string[], daysAgo: number) {
  return { id, name: digest, created_at: DAYS_AGO(daysAgo), metadata: { container: { tags } } };
}

test("isShaOnlyVersion distinguishes sha-only tags from release and channel tags", () => {
  assert.equal(isShaOnlyVersion(version(1, "sha256:1", ["sha-abcdef1234"], 1)), true);
  assert.equal(isShaOnlyVersion(version(2, "sha256:2", ["sha-abcdef1234-rockchip"], 1)), true);
  assert.equal(isShaOnlyVersion(version(3, "sha256:3", ["main", "sha-abcdef1234"], 1)), false);
  assert.equal(isShaOnlyVersion(version(4, "sha256:4", ["v1.0.0"], 1)), false);
  assert.equal(isShaOnlyVersion(version(5, "sha256:5", [], 1)), false);
});

test("cleanup preserves referenced child manifests, release tags, and recent versions", () => {
  const versions = [
    version(1, "sha256:release-index", ["v1.0.0", "latest"], 90),
    version(2, "sha256:release-amd64", [], 90),
    version(3, "sha256:release-arm64", [], 90),
    version(4, "sha256:orphan-old", [], 30),
    version(5, "sha256:orphan-recent", [], 3),
    version(6, "sha256:sha-new-1", ["sha-1111"], 20),
    version(7, "sha256:sha-new-2", ["sha-2222"], 25),
    version(8, "sha256:sha-old-3", ["sha-3333"], 40),
    version(9, "sha256:sha-old-3-child", [], 40),
  ];

  const manifests: Record<string, string> = {
    "sha256:release-index": JSON.stringify({ manifests: [{ digest: "sha256:release-amd64" }, { digest: "sha256:release-arm64" }] }),
    "sha256:sha-new-1": JSON.stringify({ manifests: [] }),
    "sha256:sha-new-2": JSON.stringify({ manifests: [] }),
    "sha256:sha-old-3": JSON.stringify({ manifests: [{ digest: "sha256:sha-old-3-child" }] }),
  };

  const staleSha = selectVersionsToDelete(versions, {
    nowMs: NOW,
    keepDays: 14,
    keepShaVersions: 2,
    referencedDigests: new Set(versions.map((v) => v.name)),
  });
  assert.deepEqual(staleSha.map((v) => v.id), [8]);

  const referenced = collectReferencedDigests(versions, new Set(staleSha.map((v) => v.id)), (digest) => manifests[digest]);
  assert.equal(referenced.has("sha256:release-amd64"), true);
  assert.equal(referenced.has("sha256:release-arm64"), true);
  assert.equal(referenced.has("sha256:sha-old-3-child"), false);

  const targets = selectVersionsToDelete(versions, {
    nowMs: NOW,
    keepDays: 14,
    keepShaVersions: 2,
    referencedDigests: referenced,
  });
  assert.deepEqual(targets.map((v) => v.id).sort((a, b) => a - b), [4, 8, 9]);
});
