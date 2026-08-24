import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

function source(path: string): string {
  return readFileSync(new URL(path, import.meta.url), "utf8");
}

const publicSources = [
  source("../src/types.ts"),
  source("../src/components/VideoCard.tsx"),
  source("../src/components/VideoMetaHeader.tsx"),
  source("../src/components/RecommendedRail.tsx"),
  source("../src/styles/video-card.css"),
  source("../src/styles/video-detail.css"),
];
const adminSources = [
  source("../src/admin/api.ts"),
  source("../src/admin/VideosPage.tsx"),
];

test("public video UI has no retired quality or HD badge design", () => {
  for (const contents of publicSources) {
    assert.doesNotMatch(contents, /\bquality\b|vd-rail__hd|video-badge\.is-hd/);
  }
});

test("admin video metadata no longer transports or displays quality", () => {
  for (const contents of adminSources) {
    assert.doesNotMatch(contents, /\bquality\b/);
  }
});
