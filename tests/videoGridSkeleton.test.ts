import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const videoGridSource = readFileSync(
  new URL("../src/components/VideoGrid.tsx", import.meta.url),
  "utf8"
);
const videoCardCss = readFileSync(
  new URL("../src/styles/video-card.css", import.meta.url),
  "utf8"
);

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

test("video skeleton mirrors thumbnail, title, and metadata structure", () => {
  assert.match(videoGridSource, /className="skeleton-card__thumb"/);
  assert.match(videoGridSource, /className="skeleton-card__title"/);
  assert.match(videoGridSource, /className="skeleton-card__meta"/);
  assert.match(videoGridSource, /className=\{`video-grid-loading \$\{compact \? "is-compact" : ""\}`\}/);

  const skeleton = ruleBody(videoCardCss, ".skeleton-card");
  const pink = ruleBody(videoCardCss, ':root[data-theme="pink"] .skeleton-card');
  const sky = ruleBody(videoCardCss, ':root[data-theme="sky"] .skeleton-card');
  const thumb = ruleBody(videoCardCss, ".skeleton-card__thumb");

  assert.match(skeleton, /--skeleton-shimmer-base\s*:/);
  assert.match(thumb, /aspect-ratio\s*:\s*16 \/ 9/);
  assert.match(
    videoCardCss,
    /\.skeleton-card__title\s*\{\s*margin-top:[^}]*width:\s*100%;[^}]*height:\s*10px;[^}]*border-radius:\s*0/s
  );
  assert.match(
    videoCardCss,
    /\.skeleton-card__meta\s*\{\s*margin-top:\s*6px;[^}]*width:\s*100%;[^}]*height:\s*10px;[^}]*border-radius:\s*0/s
  );
  assert.match(pink, /--skeleton-shimmer-base\s*:\s*rgba\(255,\s*91,\s*138,\s*0\.12\)/);
  assert.match(sky, /--skeleton-shimmer-base\s*:\s*rgba\(60,\s*100,\s*170,\s*0\.13\)/);
  assert.match(videoCardCss, /\.video-grid-loading\.is-compact \.skeleton-card/);
});

test("background list revalidation stays interactive while transitions remain blocking", () => {
  assert.match(videoGridSource, /refreshMode\?: "blocking" \| "background"/);
  assert.match(videoGridSource, /const blockingRefresh = refreshMode === "blocking"/);
  assert.match(videoGridSource, /const backgroundRefresh = refreshMode === "background"/);
  assert.match(videoGridSource, /\{blockingRefresh && \([\s\S]*?video-grid-refresh-overlay/);
  assert.doesNotMatch(videoGridSource, /正在更新视频列表/);
  assert.doesNotMatch(videoGridSource, /video-grid-refresh-overlay__status/);
  assert.match(videoGridSource, /\{backgroundRefresh && \([\s\S]*?video-grid-background-status/);

  const blockedGrid = ruleBody(videoCardCss, ".video-grid-region.is-busy .video-grid");
  const backgroundStatus = ruleBody(videoCardCss, ".video-grid-background-status");
  assert.match(blockedGrid, /opacity\s*:\s*0\.48/);
  assert.match(backgroundStatus, /pointer-events\s*:\s*none/);
  assert.doesNotMatch(videoCardCss, /\.video-grid-region\.is-background[^}]*opacity/);
});
