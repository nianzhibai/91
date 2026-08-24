import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  consumePrefetchedVideoDetail,
  prefetchVideoDetail,
} from "../src/data/videos.ts";

const appSource = readFileSync(
  new URL("../src/App.tsx", import.meta.url),
  "utf8"
);
const routeSource = readFileSync(
  new URL("../src/lib/videoDetailRoute.ts", import.meta.url),
  "utf8"
);
const gridSource = readFileSync(
  new URL("../src/components/VideoGrid.tsx", import.meta.url),
  "utf8"
);
const cardSource = readFileSync(
  new URL("../src/components/VideoCard.tsx", import.meta.url),
  "utf8"
);
const detailPageSource = readFileSync(
  new URL("../src/pages/VideoDetailPage.tsx", import.meta.url),
  "utf8"
);

test("video detail route preloads once and always has a visible route fallback", () => {
  assert.match(routeSource, /let routeModulePromise:/);
  assert.match(routeSource, /routeModulePromise = import\("@\/pages\/VideoDetailPage"\)/);
  assert.match(routeSource, /window\.requestIdleCallback\(preload, \{ timeout: 1_500 \}\)/);
  assert.match(appSource, /const VideoDetailPage = lazy\(loadVideoDetailPage\)/);
  assert.match(
    appSource,
    /path="\/video\/:id"[\s\S]*?<PageSuspense fallback=\{<VideoDetailRouteFallback \/>\}>[\s\S]*?<VideoDetailPage \/>/
  );
  assert.match(
    appSource,
    /function VideoDetailRouteFallback\(\)[\s\S]*?<VideoDetailLoading isAdmin=\{isAdmin\} \/>/
  );
  assert.match(gridSource, /useEffect\(\(\) => \{\s*scheduleVideoDetailPagePreload\(\)/);
});

test("video cards start route and detail-data work before navigation commits", () => {
  assert.match(
    cardSource,
    /function prepareDetailNavigation\(\) \{\s*preloadVideoDetailPage\(\);\s*void prefetchVideoDetail\(video\.id\)/
  );
  assert.match(
    cardSource,
    /function handlePointerDown[\s\S]*?prepareDetailNavigation\(\)/
  );
  assert.match(
    detailPageSource,
    /const prefetchedDetail = consumePrefetchedVideoDetail\(id\)[\s\S]*?Promise\.all\(\[detailRequest, fetchTags\(\)\]\)/
  );
});

test("detail-data prefetch is shared and consumed by one navigation", async () => {
  const originalFetch = globalThis.fetch;
  const videoID = "prefetch-navigation-test";
  let requestCount = 0;
  globalThis.fetch = async () => {
    requestCount += 1;
    return new Response(JSON.stringify({ id: videoID, title: "预取测试" }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  try {
    const first = prefetchVideoDetail(videoID);
    const second = prefetchVideoDetail(videoID);
    assert.strictEqual(second, first);
    assert.strictEqual(consumePrefetchedVideoDetail(videoID), first);
    assert.equal(consumePrefetchedVideoDetail(videoID), null);
    assert.equal((await first)?.id, videoID);
    assert.equal(requestCount, 1);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
