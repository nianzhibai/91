import assert from "node:assert/strict";
import test from "node:test";
import {
  HOME_RECOMMENDATION_BATCH_SIZE,
  homeLatestFeedSource,
  homeRecommendationFeedSource,
  listingFeedSource,
  listingPageFromOffset,
} from "../src/lib/infiniteFeedSource.ts";

function stubFetch(
  t: { after: (fn: () => void) => void },
  respond: (path: string) => unknown
) {
  const requested: string[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const path = String(input);
    requested.push(path);
    return new Response(JSON.stringify(respond(path)), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  return requested;
}

test("a page/size offset only maps to a page on a batch boundary", () => {
  assert.equal(listingPageFromOffset(0, 20), 1);
  assert.equal(listingPageFromOffset(20, 20), 2);
  assert.equal(listingPageFromOffset(120, 20), 7);
  assert.equal(
    listingPageFromOffset(25, 20),
    null,
    "错位的偏移量用 page/size 表达不了，必须暴露出来而不是悄悄取整"
  );
  assert.equal(listingPageFromOffset(-20, 20), null);
  assert.equal(listingPageFromOffset(20, 0), null);
});

test("the listing feed asks the list endpoint for the matching page", async (t) => {
  const requested = stubFetch(t, () => ({ items: [{ id: "v1" }], total: 90 }));
  const source = listingFeedSource({
    q: "猫",
    tag: "剧情",
    sort: "latest",
    pageSize: 20,
  });

  assert.equal(source.batchSize, 20);
  assert.equal(source.supportsRestore, true);
  assert.equal(source.stopOnDuplicateBatch, undefined);

  const batch = await source.fetchBatch(
    { offset: 40, size: 20 },
    { signal: new AbortController().signal }
  );

  assert.deepEqual(batch, { items: [{ id: "v1" }], total: 90 });
  assert.equal(requested.length, 1);
  assert.match(requested[0], /^\/api\/list\?/);
  const query = new URLSearchParams(requested[0].split("?")[1]);
  assert.equal(query.get("page"), "3");
  assert.equal(query.get("size"), "20");
  assert.equal(query.get("q"), "猫");
  assert.equal(query.get("tag"), "剧情");
  assert.equal(query.get("sort"), "latest");
});

test("a misaligned listing offset fails loudly instead of silently skipping videos", async (t) => {
  const requested = stubFetch(t, () => ({ items: [], total: 0 }));
  const source = listingFeedSource({ q: "", tag: "", sort: "hot", pageSize: 20 });

  await assert.rejects(
    source.fetchBatch(
      { offset: 25, size: 20 },
      { signal: new AbortController().signal }
    ),
    /not aligned/
  );
  assert.equal(requested.length, 0);
});

test("the random recommendation feed rotates within the server's per-request cap", async (t) => {
  const requested = stubFetch(t, () => [{ id: "r1" }]);
  const source = homeRecommendationFeedSource();

  assert.equal(source.batchSize, HOME_RECOMMENDATION_BATCH_SIZE);
  // 轮换游标在服务端且不幂等，补不回原来那批视频。
  assert.equal(source.supportsRestore, false);
  assert.equal(source.stopOnDuplicateBatch, true);

  const batch = await source.fetchBatch(
    { offset: 240, size: 40 },
    { signal: new AbortController().signal }
  );

  assert.deepEqual(batch, { items: [{ id: "r1" }], total: 0 });
  assert.deepEqual(
    requested,
    [`/api/home?count=${HOME_RECOMMENDATION_BATCH_SIZE}`],
    "超过后端单次上限会被直接拒绝，这里必须先夹住"
  );
});

test("the home latest feed pages through the list endpoint, not the capped home rotation", async (t) => {
  const requested = stubFetch(t, () => ({ items: [], total: 300 }));
  const source = homeLatestFeedSource(20);

  assert.equal(source.key, "home:latest:20");
  assert.notEqual(
    source.key,
    listingFeedSource({ q: "", tag: "", sort: "latest", pageSize: 20 }).key,
    "首页和列表页各自累积，互不干扰"
  );
  assert.equal(source.supportsRestore, true);

  await source.fetchBatch(
    { offset: 60, size: 20 },
    { signal: new AbortController().signal }
  );

  // /api/home/latest 单次上限 12 条且只在最新 96 条里绕圈，撑不起无限滚动。
  assert.equal(requested.length, 1);
  assert.doesNotMatch(requested[0], /\/api\/home/);
  const query = new URLSearchParams(requested[0].split("?")[1]);
  assert.equal(query.get("page"), "4");
  assert.equal(query.get("sort"), "latest");
});
