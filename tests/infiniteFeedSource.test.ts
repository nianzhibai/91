import assert from "node:assert/strict";
import test from "node:test";
import {
  HOME_RECOMMENDATION_BATCH_SIZE,
  homeLatestFeedSource,
  homeRecommendationFeedSource,
  listingFeedSource,
} from "../src/lib/infiniteFeedSource.ts";

type RecordedRequest = { path: string; init?: RequestInit };

function stubFetch(
  t: { after: (fn: () => void) => void },
  respond: (path: string, init?: RequestInit) => unknown
) {
  const requested: RecordedRequest[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input);
    requested.push({ path, init });
    const response = await respond(path, init);
    if (response instanceof Response) return response;
    return new Response(JSON.stringify(response), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  return requested;
}

function feedResponse(overrides: Record<string, unknown> = {}) {
  return {
    items: [{ id: "v1" }],
    total: 90,
    feedToken: "snapshot-token",
    nextCursor: 20,
    exhausted: false,
    ...overrides,
  };
}

test("the listing feed creates a filtered snapshot instead of translating offsets to pages", async (t) => {
  const requested = stubFetch(t, () => feedResponse());
  const source = listingFeedSource({
    q: " 猫 ",
    tag: " 剧情 ",
    sort: "latest",
    pageSize: 20,
  });

  assert.equal(source.batchSize, 20);
  assert.equal(
    source.key,
    listingFeedSource({ q: "猫", tag: "剧情", sort: "latest", pageSize: 14 }).key,
    "响应式批次大小不能改变逻辑列表身份"
  );

  const batch = await source.fetchBatch(
    { cursor: { feedToken: "", position: 0 }, size: 20 },
    { signal: new AbortController().signal }
  );

  assert.deepEqual(batch, {
    items: [{ id: "v1" }],
    total: 90,
    cursor: { feedToken: "snapshot-token", position: 20 },
    exhausted: false,
  });
  assert.equal(requested.length, 1);
  assert.match(requested[0].path, /^\/api\/feed\?/);
  const query = new URLSearchParams(requested[0].path.split("?")[1]);
  assert.equal(query.get("kind"), "listing");
  assert.equal(query.get("cursor"), "0");
  assert.equal(query.get("count"), "20");
  assert.equal(query.get("q"), "猫");
  assert.equal(query.get("tag"), "剧情");
  assert.equal(query.get("sort"), "latest");
  assert.equal(query.has("page"), false);
});

test("later batches explicitly address the same token and cursor", async (t) => {
  const requested = stubFetch(t, () =>
    feedResponse({ items: [{ id: "v3" }], nextCursor: 60 })
  );
  const source = listingFeedSource({ q: "", tag: "", sort: "hot", pageSize: 20 });

  await source.fetchBatch(
    { cursor: { feedToken: "snapshot-token", position: 40 }, size: 20 },
    { signal: new AbortController().signal }
  );

  const query = new URLSearchParams(requested[0].path.split("?")[1]);
  assert.equal(query.get("feedToken"), "snapshot-token");
  assert.equal(query.get("cursor"), "40");
  assert.equal(query.get("count"), "20");
});

test("a feed completed by its first batch does not need a snapshot token", async (t) => {
  stubFetch(t, () =>
    feedResponse({
      items: [{ id: "only-result" }],
      total: 1,
      feedToken: "",
      nextCursor: 1,
      exhausted: true,
    })
  );
  const source = listingFeedSource({
    q: "only-result",
    tag: "",
    sort: "latest",
    pageSize: 20,
  });

  const batch = await source.fetchBatch(
    { cursor: { feedToken: "", position: 0 }, size: 20 },
    { signal: new AbortController().signal }
  );

  assert.equal(batch.exhausted, true);
  assert.deepEqual(batch.cursor, { feedToken: "", position: 1 });
});

test("a feed with another batch still requires a snapshot token", async (t) => {
  stubFetch(t, () => feedResponse({ feedToken: "" }));
  const source = listingFeedSource({
    q: "",
    tag: "",
    sort: "latest",
    pageSize: 20,
  });

  await assert.rejects(
    source.fetchBatch(
      { cursor: { feedToken: "", position: 0 }, size: 20 },
      { signal: new AbortController().signal }
    ),
    /Invalid \/api\/feed response/
  );
});

test("the random recommendation feed is a restorable shuffled snapshot", async (t) => {
  const requested = stubFetch(t, () =>
    feedResponse({ total: 300, nextCursor: 48 })
  );
  const source = homeRecommendationFeedSource();

  assert.equal(source.key, "home:recommend");
  assert.equal(source.batchSize, HOME_RECOMMENDATION_BATCH_SIZE);
  await source.fetchBatch(
    { cursor: { feedToken: "snapshot-token", position: 36 }, size: 12 },
    { signal: new AbortController().signal }
  );

  const query = new URLSearchParams(requested[0].path.split("?")[1]);
  assert.equal(query.get("kind"), "recommend");
  assert.equal(query.get("feedToken"), "snapshot-token");
  assert.equal(query.get("cursor"), "36");
  assert.equal(query.get("count"), "12");
  assert.doesNotMatch(requested[0].path, /\/api\/home/);
});

test("the home latest feed keeps its identity when its responsive batch changes", async (t) => {
  const requested = stubFetch(t, () =>
    feedResponse({ items: [], total: 300, nextCursor: 80 })
  );
  const source = homeLatestFeedSource(20);

  assert.equal(source.key, "home:latest");
  assert.equal(source.key, homeLatestFeedSource(14).key);
  assert.notEqual(
    source.key,
    listingFeedSource({ q: "", tag: "", sort: "latest", pageSize: 20 }).key,
    "首页和列表页各自累积，互不干扰"
  );

  await source.fetchBatch(
    { cursor: { feedToken: "snapshot-token", position: 60 }, size: 20 },
    { signal: new AbortController().signal }
  );
  const query = new URLSearchParams(requested[0].path.split("?")[1]);
  assert.equal(query.get("kind"), "latest");
  assert.equal(query.get("sort"), "latest");
});

test("an expired snapshot is exposed as a feed-specific error", async (t) => {
  stubFetch(t, () => new Response("expired", { status: 410 }));
  const source = homeLatestFeedSource(20);

  const error = await source
    .fetchBatch(
      { cursor: { feedToken: "expired-token", position: 20 }, size: 20 },
      { signal: new AbortController().signal }
    )
    .then(
      () => null,
      (reason) => reason
    );
  assert.ok(error instanceof Error);
  assert.equal(source.isExpiredError(error), true);
});

test("aborting a discarded feed request reaches the actual fetch", async (t) => {
  let fetchSignal: AbortSignal | undefined;
  stubFetch(t, (_path, init) => {
    fetchSignal = init?.signal ?? undefined;
    return new Promise((_resolve, reject) => {
      fetchSignal?.addEventListener("abort", () => reject(fetchSignal?.reason), {
        once: true,
      });
    });
  });
  const source = homeRecommendationFeedSource();
  const controller = new AbortController();
  const request = source.fetchBatch(
    { cursor: { feedToken: "", position: 0 }, size: 12 },
    { signal: controller.signal }
  );
  controller.abort(new DOMException("discarded", "AbortError"));

  await assert.rejects(request, { name: "AbortError" });
  assert.equal(fetchSignal?.aborted, true);
});
