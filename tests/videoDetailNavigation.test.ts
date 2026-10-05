import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  consumePrefetchedVideoDetail,
  consumePrefetchedVideoRecommendations,
  fetchVideoDetail,
  prefetchVideoDetail,
  prefetchVideoRecommendations,
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
    /function VideoDetailRouteElement\(\)[\s\S]*?<PageSuspense fallback=\{<VideoDetailRouteFallback \/>\}>[\s\S]*?<VideoDetailPage \/>/
  );
  assert.match(
    appSource,
    /path="\/video\/:id"[\s\S]*?element=\{<VideoDetailRouteElement \/>\}/
  );
  assert.match(
    appSource,
    /function VideoDetailRouteFallback\(\)[\s\S]*?<VideoDetailLoading isAdmin=\{isAdmin\} \/>/
  );
  assert.match(gridSource, /useEffect\(\(\) => \{\s*scheduleVideoDetailPagePreload\(\)/);
});

test("video cards start independent detail and recommendation requests for confirmed navigation", () => {
  assert.match(
    cardSource,
    /function prepareDetailNavigation\(\) \{\s*preloadVideoDetailPage\(\);\s*void prefetchVideoDetail\(video\.id\)/
  );
  assert.match(
    cardSource,
    /function handlePointerDown[\s\S]*?prepareDetailNavigation\(\)/
  );
  assert.match(
    cardSource,
    /function prepareConfirmedDetailNavigation\(\) \{\s*prepareDetailNavigation\(\);\s*void prefetchVideoRecommendations\(video\.id\)/
  );
  assert.match(
    cardSource,
    /function handleDetailNavigation\(\) \{\s*stopPreview\(\);\s*prepareConfirmedDetailNavigation\(\)/
  );
  assert.match(
    detailPageSource,
    /const prefetchedDetail =[\s\S]*?consumePrefetchedVideoDetail\(id\)[\s\S]*?const detailRequest = prefetchedDetail \?\? fetchVideoDetail\(id\);[\s\S]*?detailRequest[\s\S]*?\.then\(\(d\) =>[\s\S]*?setDetail\(stableDetail\);[\s\S]*?setLoading\(false\)/
  );
  assert.match(
    detailPageSource,
    /const prefetchedRecommendations =[\s\S]*?consumePrefetchedVideoRecommendations\(id\)[\s\S]*?const recommendationsRequest =[\s\S]*?fetchVideoRecommendations\(id, \{ signal: controller\.signal \}\)/
  );
  assert.doesNotMatch(detailPageSource, /Promise\.all\(\[detailRequest, fetchTags\(\)\]\)/);
  assert.doesNotMatch(
    detailPageSource,
    /Promise\.all\(\[[^\]]*detailRequest[^\]]*recommendationsRequest/
  );
  assert.match(
    detailPageSource,
    /if \(!isAdmin\) \{[\s\S]*?return;[\s\S]*?fetchTags\(\)/
  );
});

test("same-video overlay history does not restart detail navigation effects", () => {
  assert.match(
    detailPageSource,
    /const \[entryNavigationType\] = useState\(navigationType\)/
  );
  assert.match(
    detailPageSource,
    /\}, \[\s*detailLoadVersion,\s*entryNavigationType,\s*id,\s*initialSnapshot,\s*scrollRootRef,\s*\]\);/
  );
  assert.doesNotMatch(
    detailPageSource,
    /\[detailLoadVersion, id, initialSnapshot, navigationType\]/
  );
});

test("listing previews start on touch contact and leave clicks to the detail link", () => {
  assert.match(
    cardSource,
    /function handleTouchStart\(\)[\s\S]*?startPreview\(\)/
  );
  assert.match(
    cardSource,
    /useCardPreview\(\{ id: video\.id, src: video\.previewSrc, inView, active: routeActive \}\)/
  );
  assert.match(cardSource, /onTouchStart=\{handleTouchStart\}/);
  assert.match(cardSource, /onClick=\{handleDetailNavigation\}/);
  assert.doesNotMatch(cardSource, /setTimeout|TOUCH_PREVIEW_DELAY_MS|HOVER_DELAY_MS|onClickCapture|preventDefault/);
});

test("recommendation prefetch is shared and consumed by one navigation", async () => {
  const originalFetch = globalThis.fetch;
  const videoID = "recommendation-prefetch-navigation-test";
  let requestCount = 0;
  globalThis.fetch = async (input) => {
    requestCount += 1;
    assert.match(String(input), /\/api\/video\/recommendation-prefetch-navigation-test\/recommendations$/);
    return new Response(JSON.stringify([{ id: "recommended-video" }]), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  try {
    const first = prefetchVideoRecommendations(videoID);
    const second = prefetchVideoRecommendations(videoID);
    assert.strictEqual(second, first);
    assert.strictEqual(consumePrefetchedVideoRecommendations(videoID), first);
    assert.equal(consumePrefetchedVideoRecommendations(videoID), null);
    assert.equal((await first)[0]?.id, "recommended-video");
    assert.equal(requestCount, 1);
  } finally {
    globalThis.fetch = originalFetch;
  }
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

test("video detail distinguishes a missing resource from a service failure", async () => {
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = async () => new Response(null, { status: 404 });
    assert.equal(await fetchVideoDetail("missing-video"), null);

    globalThis.fetch = async () => new Response(null, { status: 503 });
    await assert.rejects(fetchVideoDetail("temporarily-unavailable"), /HTTP 503/);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
