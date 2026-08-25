import assert from "node:assert/strict";
import test from "node:test";
import {
  appendUniqueVideos,
  emptyInfiniteListingState,
  infiniteListingCacheMatchesRestore,
  infiniteListingHasMore,
  infiniteListingKey,
  infiniteListingReducer,
  nextListingRequest,
  type InfiniteListingState,
} from "../src/lib/infiniteListing.ts";
import type { VideoItem } from "../src/types.ts";

const KEY = infiniteListingKey({ q: "", tag: "", sort: "hot" });
const FEED_TOKEN = "feed-token";

function videos(from: number, count: number): VideoItem[] {
  return Array.from(
    { length: count },
    (_, index) => ({ id: `video-${from + index}` } as VideoItem)
  );
}

function loaded(overrides: Partial<InfiniteListingState> = {}): InfiniteListingState {
  return {
    ...emptyInfiniteListingState(KEY, 20),
    requestID: 1,
    items: videos(0, 20),
    total: 100,
    feedToken: FEED_TOKEN,
    requestedCount: 20,
    status: "ready",
    receivedAt: 1_000,
    ...overrides,
  };
}

test("the listing key normalizes whitespace and contains only logical filters", () => {
  assert.equal(
    infiniteListingKey({ q: " 猫 ", tag: " 剧情 ", sort: "latest" }),
    infiniteListingKey({ q: "猫", tag: "剧情", sort: "latest" })
  );
  assert.notEqual(
    infiniteListingKey({ q: "", tag: "", sort: "latest" }),
    infiniteListingKey({ q: "", tag: "", sort: "hot" })
  );
});

test("history restoration only accepts a compatible snapshot cache", () => {
  assert.equal(
    infiniteListingCacheMatchesRestore({
      cachedFeedToken: "snapshot-a",
      cachedRequestedCount: 80,
      restoreFeedToken: "snapshot-a",
      restoreCount: 60,
    }),
    true
  );
  assert.equal(
    infiniteListingCacheMatchesRestore({
      cachedFeedToken: "snapshot-b",
      cachedRequestedCount: 80,
      restoreFeedToken: "snapshot-a",
      restoreCount: 60,
    }),
    false,
    "同一查询的新快照不能覆盖旧历史条目的内容"
  );
  assert.equal(
    infiniteListingCacheMatchesRestore({
      cachedFeedToken: "snapshot-a",
      cachedRequestedCount: 20,
      restoreFeedToken: "snapshot-a",
      restoreCount: 60,
    }),
    false,
    "缓存深度不足时必须按保存的 token 补回内容"
  );
  assert.equal(
    infiniteListingCacheMatchesRestore({
      cachedFeedToken: "snapshot-b",
      cachedRequestedCount: 20,
      restoreFeedToken: "",
      restoreCount: 0,
    }),
    true,
    "普通新会话仍可复用查询级缓存"
  );
});

test("appending defensively drops ids that are already on screen", () => {
  const previous = videos(0, 3);
  const merged = appendUniqueVideos(previous, [
    ...videos(2, 1),
    ...videos(3, 2),
  ]);

  assert.deepEqual(
    merged.map((item) => item.id),
    ["video-0", "video-1", "video-2", "video-3", "video-4"]
  );
  assert.strictEqual(
    appendUniqueVideos(previous, videos(0, 3)),
    previous,
    "an all-duplicate batch must not create a new array"
  );
  assert.strictEqual(appendUniqueVideos(previous, []), previous);
});

test("a successful batch adopts the server cursor independently of rendered item count", () => {
  const state = loaded();
  const next = infiniteListingReducer(state, {
    type: "load-success",
    requestID: 1,
    requestCursor: { feedToken: FEED_TOKEN, position: 20 },
    cursor: { feedToken: FEED_TOKEN, position: 43 },
    items: [...videos(19, 1), ...videos(20, 19)],
    total: 100,
    exhausted: false,
    receivedAt: 2_000,
  });

  assert.equal(next.items.length, 39);
  assert.equal(new Set(next.items.map((item) => item.id)).size, 39);
  assert.equal(
    next.requestedCount,
    43,
    "服务端可能跳过已隐藏的快照项，游标不能按收到或去重后的条数推算"
  );
  assert.equal(next.exhausted, false);
  assert.equal(next.status, "ready");
});

test("loading starts as initial for an empty list and as load-more once items exist", () => {
  const first = infiniteListingReducer(emptyInfiniteListingState(KEY, 20), {
    type: "load-start",
    requestID: 1,
  });
  assert.equal(first.status, "initial-loading");

  const more = infiniteListingReducer(loaded(), {
    type: "load-start",
    requestID: 2,
  });
  assert.equal(more.status, "loading-more");
  assert.equal(more.items.length, 20, "已加载内容在加载下一批时保持可见");
});

test("stale responses cannot append to a newer request", () => {
  const state = loaded({ requestID: 5 });
  const lateSuccess = infiniteListingReducer(state, {
    type: "load-success",
    requestID: 4,
    requestCursor: { feedToken: FEED_TOKEN, position: 20 },
    cursor: { feedToken: FEED_TOKEN, position: 40 },
    items: videos(20, 20),
    total: 100,
    exhausted: false,
    receivedAt: 2_000,
  });
  const lateFailure = infiniteListingReducer(state, {
    type: "load-failure",
    requestID: 4,
    error: new Error("late"),
  });

  assert.strictEqual(lateSuccess, state);
  assert.strictEqual(lateFailure, state);
});

test("a response for a different snapshot position is discarded", () => {
  const state = loaded({ requestID: 3, requestedCount: 40, items: videos(0, 40) });
  const wrongPosition = infiniteListingReducer(state, {
    type: "load-success",
    requestID: 3,
    requestCursor: { feedToken: FEED_TOKEN, position: 20 },
    cursor: { feedToken: FEED_TOKEN, position: 40 },
    items: videos(20, 20),
    total: 100,
    exhausted: false,
    receivedAt: 2_000,
  });
  const wrongToken = infiniteListingReducer(state, {
    type: "load-success",
    requestID: 3,
    requestCursor: { feedToken: "other-feed", position: 40 },
    cursor: { feedToken: "other-feed", position: 60 },
    items: videos(40, 20),
    total: 100,
    exhausted: false,
    receivedAt: 2_000,
  });

  assert.strictEqual(wrongPosition, state);
  assert.strictEqual(wrongToken, state);
});

test("exhaustion comes from the snapshot server, not short-page guesses", () => {
  const skippedRows = infiniteListingReducer(loaded(), {
    type: "load-success",
    requestID: 1,
    requestCursor: { feedToken: FEED_TOKEN, position: 20 },
    cursor: { feedToken: FEED_TOKEN, position: 45 },
    items: videos(20, 7),
    total: 100,
    exhausted: false,
    receivedAt: 2_000,
  });
  assert.equal(skippedRows.exhausted, false);
  assert.equal(infiniteListingHasMore(skippedRows), true);

  const final = infiniteListingReducer(loaded(), {
    type: "load-success",
    requestID: 1,
    requestCursor: { feedToken: FEED_TOKEN, position: 20 },
    cursor: { feedToken: FEED_TOKEN, position: 100 },
    items: videos(20, 7),
    total: 100,
    exhausted: true,
    receivedAt: 2_000,
  });
  assert.equal(final.exhausted, true);
  assert.equal(infiniteListingHasMore(final), false);
  assert.equal(nextListingRequest(final), null);
});

test("a failed batch keeps loaded items and stops automatic loading", () => {
  const failed = infiniteListingReducer(
    loaded({ requestID: 2, status: "loading-more" }),
    { type: "load-failure", requestID: 2, error: new Error("offline") }
  );

  assert.equal(failed.status, "error");
  assert.equal(failed.items.length, 20);
  assert.equal(failed.requestedCount, 20);
  assert.equal(infiniteListingHasMore(failed), false);
});

test("the next request carries the exact token and cursor with the current batch size", () => {
  assert.deepEqual(nextListingRequest(loaded()), {
    cursor: { feedToken: FEED_TOKEN, position: 20 },
    size: 20,
  });
  assert.deepEqual(
    nextListingRequest(loaded({ pageSize: 14, requestedCount: 120 })),
    { cursor: { feedToken: FEED_TOKEN, position: 120 }, size: 14 }
  );
  assert.equal(nextListingRequest(loaded({ pageSize: 0 })), null);
  assert.equal(nextListingRequest(loaded({ exhausted: true })), null);
  assert.deepEqual(nextListingRequest(emptyInfiniteListingState(KEY, 14)), {
    cursor: { feedToken: "", position: 0 },
    size: 14,
  });
});

test("changing the query resets accumulation and cached content hydrates with its snapshot", () => {
  const reset = infiniteListingReducer(loaded(), {
    type: "reset",
    requestID: 9,
    key: "other",
    pageSize: 14,
  });
  assert.equal(reset.key, "other");
  assert.equal(reset.pageSize, 14);
  assert.equal(reset.items.length, 0);
  assert.equal(reset.feedToken, "");
  assert.equal(reset.requestedCount, 0);
  assert.equal(reset.exhausted, false);
  assert.equal(reset.status, "idle");

  const hydrated = infiniteListingReducer(emptyInfiniteListingState(KEY, 20), {
    type: "hydrate",
    requestID: 10,
    key: KEY,
    pageSize: 14,
    items: videos(0, 60),
    total: 100,
    feedToken: FEED_TOKEN,
    requestedCount: 63,
    exhausted: false,
    receivedAt: 5_000,
  });
  assert.equal(hydrated.status, "ready");
  assert.equal(hydrated.pageSize, 14);
  assert.equal(hydrated.items.length, 60);
  assert.equal(hydrated.feedToken, FEED_TOKEN);
  assert.equal(hydrated.requestedCount, 63);
  assert.equal(hydrated.receivedAt, 5_000);

  const disabled = infiniteListingReducer(loaded(), {
    type: "disable",
    requestID: 11,
    key: KEY,
    pageSize: 20,
  });
  assert.equal(disabled.status, "idle");
  assert.equal(disabled.items.length, 0);
});
