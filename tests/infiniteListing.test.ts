import assert from "node:assert/strict";
import test from "node:test";
import {
  appendUniqueVideos,
  emptyInfiniteListingState,
  infiniteListingHasMore,
  infiniteListingKey,
  infiniteListingReducer,
  isListingExhausted,
  nextListingRequest,
  type InfiniteListingState,
} from "../src/lib/infiniteListing.ts";
import type { VideoItem } from "../src/types.ts";

const KEY = infiniteListingKey({ q: "", tag: "", sort: "hot", pageSize: 20 });

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
    requestedCount: 20,
    status: "ready",
    receivedAt: 1_000,
    ...overrides,
  };
}

test("the listing key normalizes whitespace so equivalent queries share one session", () => {
  assert.equal(
    infiniteListingKey({ q: " 猫 ", tag: " 剧情 ", sort: "latest", pageSize: 20 }),
    infiniteListingKey({ q: "猫", tag: "剧情", sort: "latest", pageSize: 20 })
  );
  assert.notEqual(
    infiniteListingKey({ q: "", tag: "", sort: "latest", pageSize: 20 }),
    infiniteListingKey({ q: "", tag: "", sort: "latest", pageSize: 14 })
  );
});

test("appending drops ids that are already on screen", () => {
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

test("a batch that overlaps the previous page is deduplicated on append", () => {
  const state = loaded();
  // 列表期间新入库一条视频，第二页整体后移一位，边界那条会重复返回。
  const next = infiniteListingReducer(state, {
    type: "load-success",
    requestID: 1,
    offset: 20,
    batchSize: 20,
    items: [...videos(19, 1), ...videos(20, 19)],
    total: 100,
    receivedAt: 2_000,
  });

  assert.equal(next.items.length, 39);
  assert.equal(new Set(next.items.map((item) => item.id)).size, 39);
  assert.equal(next.requestedCount, 40, "游标按请求条数推进，不受去重影响");
  assert.equal(next.exhausted, false);
  assert.equal(next.status, "ready");
});

test("loading starts as initial for an empty list and as load-more once items exist", () => {
  const first = infiniteListingReducer(
    emptyInfiniteListingState(KEY, 20),
    { type: "load-start", requestID: 1 }
  );
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
    offset: 20,
    batchSize: 20,
    items: videos(20, 20),
    total: 100,
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

test("a response whose offset no longer matches the cursor is discarded", () => {
  const state = loaded({ requestID: 3, requestedCount: 40, items: videos(0, 40) });

  const misaligned = infiniteListingReducer(state, {
    type: "load-success",
    requestID: 3,
    offset: 20,
    batchSize: 20,
    items: videos(20, 20),
    total: 100,
    receivedAt: 2_000,
  });

  assert.strictEqual(misaligned, state);
});

test("the list stops loading on a short page and on a reached total", () => {
  const shortPage = infiniteListingReducer(loaded(), {
    type: "load-success",
    requestID: 1,
    offset: 20,
    batchSize: 20,
    items: videos(20, 7),
    total: 100,
    receivedAt: 2_000,
  });
  assert.equal(shortPage.exhausted, true);
  assert.equal(infiniteListingHasMore(shortPage), false);
  assert.equal(nextListingRequest(shortPage), null);

  const reachedTotal = infiniteListingReducer(loaded({ total: 40 }), {
    type: "load-success",
    requestID: 1,
    offset: 20,
    batchSize: 20,
    items: videos(20, 20),
    total: 40,
    receivedAt: 2_000,
  });
  assert.equal(reachedTotal.exhausted, true);

  // total 只是查询时刻的计数，删除后会虚高；返回满页时不能提前收尾。
  assert.equal(
    isListingExhausted({
      received: 20,
      batchSize: 20,
      requestedCount: 40,
      total: 0,
    }),
    false
  );
});

test("a failed batch keeps the loaded items and stops auto-loading", () => {
  const failed = infiniteListingReducer(loaded({ requestID: 2, status: "loading-more" }), {
    type: "load-failure",
    requestID: 2,
    error: new Error("offline"),
  });

  assert.equal(failed.status, "error");
  assert.equal(failed.items.length, 20);
  assert.equal(failed.requestedCount, 20);
  assert.equal(infiniteListingHasMore(failed), false);
});

test("the next request continues on a page boundary derived from requested count", () => {
  assert.deepEqual(nextListingRequest(loaded()), { page: 2, size: 20 });
  assert.deepEqual(
    nextListingRequest(loaded({ requestedCount: 120 })),
    { page: 7, size: 20 },
    "恢复现场后的大请求也必须落在页边界上"
  );
  assert.equal(
    nextListingRequest(loaded({ requestedCount: 25 })),
    null,
    "偏移量不是页大小整数倍时无法用 page/size 表达"
  );
  assert.equal(nextListingRequest(loaded({ pageSize: 0 })), null);
  assert.deepEqual(
    nextListingRequest(emptyInfiniteListingState(KEY, 14)),
    { page: 1, size: 14 }
  );
});

test("changing the query resets accumulation and cached content hydrates as ready", () => {
  const reset = infiniteListingReducer(loaded(), {
    type: "reset",
    requestID: 9,
    key: "other",
    pageSize: 14,
  });
  assert.equal(reset.key, "other");
  assert.equal(reset.pageSize, 14);
  assert.equal(reset.items.length, 0);
  assert.equal(reset.requestedCount, 0);
  assert.equal(reset.exhausted, false);
  assert.equal(reset.status, "idle");

  const hydrated = infiniteListingReducer(emptyInfiniteListingState(KEY, 20), {
    type: "hydrate",
    requestID: 10,
    key: KEY,
    pageSize: 20,
    items: videos(0, 60),
    total: 100,
    requestedCount: 60,
    exhausted: false,
    receivedAt: 5_000,
  });
  assert.equal(hydrated.status, "ready");
  assert.equal(hydrated.items.length, 60);
  assert.equal(hydrated.requestedCount, 60);
  assert.equal(
    hydrated.receivedAt,
    5_000,
    "hydrate 保留原始响应时间，缓存不会自我续命"
  );

  const disabled = infiniteListingReducer(loaded(), {
    type: "disable",
    requestID: 11,
  });
  assert.equal(disabled.status, "idle");
  assert.equal(disabled.items.length, 0);
});
