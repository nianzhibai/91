import assert from "node:assert/strict";
import test from "node:test";
import {
  BATCH_SIZE,
  EMPTY_SHORTS_FEED,
  INITIAL_BATCH_SIZE,
  MAX_SHORTS_QUEUE_ITEMS,
  SHORTS_QUEUE_KEEP_BEHIND,
  SHORTS_FEED_STORAGE_KEY,
  clearShortsFeedState,
  getShortsQueueTrimCount,
  loadShortsFeedState,
  mergeShortsQueue,
  planShortsPrefetch,
  readShortsFeedMode,
  requestShortsBatch,
  saveShortsFeedState,
  withShortsFeedMode,
  type QueuedShortsItem,
  type ShortsFeedCommitEvent,
  type ShortsFeedState,
} from "../src/shorts/shortsFeed";
import type { ShortsNextResponse } from "../src/data/videos";

function withMemoryLocalStorage<T>(run: (store: Map<string, string>) => T): T {
  const store = new Map<string, string>();
  const stub = {
    getItem: (key: string) => (store.has(key) ? store.get(key)! : null),
    setItem: (key: string, value: string) => {
      store.set(key, String(value));
    },
    removeItem: (key: string) => {
      store.delete(key);
    },
  };
  const original = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  Object.defineProperty(globalThis, "localStorage", {
    value: stub,
    configurable: true,
    writable: true,
  });
  try {
    return run(store);
  } finally {
    if (original) {
      Object.defineProperty(globalThis, "localStorage", original);
    } else {
      delete (globalThis as { localStorage?: unknown }).localStorage;
    }
  }
}

function feedResponse(input: {
  feedToken: string;
  cursors?: number[];
  total?: number;
  nextCursor?: number;
  roundComplete?: boolean;
}): ShortsNextResponse {
  const cursors = input.cursors ?? [];
  return {
    items: cursors.map((feedCursor, index) => ({
      id: `${input.feedToken}-video-${index}`,
      feedCursor,
    })),
    total: input.total ?? 100,
    feedToken: input.feedToken,
    nextCursor: input.nextCursor ?? (cursors[cursors.length - 1] ?? 0),
    roundComplete: input.roundComplete ?? false,
  } as unknown as ShortsNextResponse;
}

test("shorts feed exposes a small initial batch and larger continuation batch", () => {
  assert.equal(INITIAL_BATCH_SIZE, 2);
  assert.equal(BATCH_SIZE, 5);
});

test("shorts mode defaults to recommendation and preserves other URL settings", () => {
  assert.equal(readShortsFeedMode(new URLSearchParams()), "recommend");
  assert.equal(readShortsFeedMode(new URLSearchParams("feed=unknown")), "recommend");
  for (const mode of ["latest", "hot", "recommend"] as const) {
    const params = withShortsFeedMode(new URLSearchParams("debug=1&feed=latest"), mode);
    assert.equal(readShortsFeedMode(params), mode);
    assert.equal(params.get("debug"), "1");
    assert.equal(params.get("feed"), mode === "recommend" ? null : mode);
  }
});

test("each shorts mode resumes and clears only its own bookmark", () => {
  withMemoryLocalStorage(() => {
    for (const mode of ["latest", "hot", "recommend"] as const) {
      saveShortsFeedState({ feedToken: `${mode}-token`, cursor: 7 }, mode);
    }
    for (const mode of ["latest", "hot", "recommend"] as const) {
      assert.deepEqual(loadShortsFeedState(mode), { feedToken: `${mode}-token`, cursor: 7 });
    }
    clearShortsFeedState("hot");
    assert.deepEqual(loadShortsFeedState("hot"), EMPTY_SHORTS_FEED);
    assert.equal(loadShortsFeedState("latest").feedToken, "latest-token");
    assert.equal(loadShortsFeedState().feedToken, "recommend-token");
  });
});

test("feed bookmark storage round-trips and rejects hostile payloads", () => {
  withMemoryLocalStorage((store) => {
    saveShortsFeedState({ feedToken: "abc123", cursor: 7 });
    assert.deepEqual(loadShortsFeedState(), { feedToken: "abc123", cursor: 7 });

    for (const raw of [
      "not json",
      JSON.stringify(null),
      JSON.stringify({ feedToken: "", cursor: 1 }),
      JSON.stringify({ feedToken: "x".repeat(129), cursor: 1 }),
      JSON.stringify({ feedToken: "abc", cursor: -1 }),
      JSON.stringify({ feedToken: "abc", cursor: 1.5 }),
      JSON.stringify({ feedToken: 42, cursor: 1 }),
    ]) {
      store.set(SHORTS_FEED_STORAGE_KEY, raw);
      assert.deepEqual(loadShortsFeedState(), EMPTY_SHORTS_FEED, raw);
    }

    store.set(SHORTS_FEED_STORAGE_KEY, JSON.stringify({ feedToken: "abc", cursor: 3 }));
    clearShortsFeedState();
    assert.equal(store.has(SHORTS_FEED_STORAGE_KEY), false);
  });
});

test("feed bookmark storage degrades quietly without localStorage", () => {
  // Node 环境没有 localStorage：读回空书签，写入/清除不抛错
  assert.deepEqual(loadShortsFeedState(), EMPTY_SHORTS_FEED);
  assert.doesNotThrow(() => saveShortsFeedState({ feedToken: "abc", cursor: 1 }));
  assert.doesNotThrow(() => clearShortsFeedState());
});

test("merge stamps the batch token and dedupes by token and cursor", () => {
  const prev = [
    { id: "a", feedCursor: 1, feedToken: "t1" },
    { id: "b", feedCursor: 2, feedToken: "t1" },
  ] as unknown as QueuedShortsItem[];

  const merged = mergeShortsQueue(
    prev,
    feedResponse({ feedToken: "t1", cursors: [2, 3] })
  );
  assert.deepEqual(
    merged.map((item) => `${item.feedToken}:${item.feedCursor}`),
    ["t1:1", "t1:2", "t1:3"]
  );
  // 原队列不被原地修改
  assert.equal(prev.length, 2);

  // 新一轮 feed 的相同游标不算重复：轮次由 token 区分
  const newRound = mergeShortsQueue(
    merged,
    feedResponse({ feedToken: "t2", cursors: [1] })
  );
  assert.deepEqual(
    newRound.map((item) => `${item.feedToken}:${item.feedCursor}`),
    ["t1:1", "t1:2", "t1:3", "t2:1"]
  );
});

test("prefetch plan waits for the queue end before starting a new round", () => {
  const base = { loading: false, loadError: false, roundComplete: false };

  // 未看数量充足时不动作；跌破阈值后请求下一批
  assert.equal(planShortsPrefetch({ ...base, remainingAfterActive: 2 }), "none");
  assert.equal(planShortsPrefetch({ ...base, remainingAfterActive: 1 }), "load");

  // 本轮已耗尽：最后一批仍在队列中就不换轮，滑到最后一条才开新 feed
  assert.equal(
    planShortsPrefetch({ ...base, remainingAfterActive: 1, roundComplete: true }),
    "none"
  );
  assert.equal(
    planShortsPrefetch({ ...base, remainingAfterActive: 0, roundComplete: true }),
    "new-round"
  );

  // 加载中 / 加载失败时都不再叠加请求
  assert.equal(
    planShortsPrefetch({ ...base, remainingAfterActive: 0, loading: true }),
    "none"
  );
  assert.equal(
    planShortsPrefetch({ ...base, remainingAfterActive: 0, loadError: true }),
    "none"
  );
});

test("long shorts sessions trim in batches while retaining back-scroll history", () => {
  assert.equal(getShortsQueueTrimCount(50, MAX_SHORTS_QUEUE_ITEMS), 0);
  assert.equal(
    getShortsQueueTrimCount(
      SHORTS_QUEUE_KEEP_BEHIND,
      MAX_SHORTS_QUEUE_ITEMS + 1
    ),
    0
  );
  assert.equal(
    getShortsQueueTrimCount(58, MAX_SHORTS_QUEUE_ITEMS + 1),
    58 - SHORTS_QUEUE_KEEP_BEHIND
  );
  const removed = getShortsQueueTrimCount(80, 85);
  assert.equal(80 - removed, SHORTS_QUEUE_KEEP_BEHIND);
});

class FakeExpiredError extends Error {}

type FetchCall = { feedToken: string; cursor: number; count: number };

function batchHarness(
  script: Array<ShortsNextResponse | Error>,
  feed: ShortsFeedState = EMPTY_SHORTS_FEED
) {
  const calls: FetchCall[] = [];
  const commits: Array<{ feed: ShortsFeedState; event: ShortsFeedCommitEvent }> = [];
  let step = 0;
  const run = requestShortsBatch({
    feed,
    count: 5,
    fetchNext: async (feedToken, cursor, count) => {
      calls.push({ feedToken, cursor, count });
      const next = script[step];
      step += 1;
      if (next instanceof Error) throw next;
      return next;
    },
    isFeedExpiredError: (error) => error instanceof FakeExpiredError,
    commitFeed: (nextFeed, event) => {
      commits.push({ feed: nextFeed, event });
    },
  });
  return { run, calls, commits };
}

test("batch request advances the cursor on success", async () => {
  const { run, calls, commits } = batchHarness(
    [feedResponse({ feedToken: "t1", cursors: [6, 7], nextCursor: 7 })],
    { feedToken: "t1", cursor: 5 }
  );
  const outcome = await run;

  assert.deepEqual(calls, [{ feedToken: "t1", cursor: 5, count: 5 }]);
  assert.deepEqual(commits, [
    { feed: { feedToken: "t1", cursor: 7 }, event: "advanced" },
  ]);
  assert.equal(outcome.kind, "batch");
  assert.equal(outcome.kind === "batch" && outcome.response.items.length, 2);
});

test("an expired token clears the bookmark and restarts a fresh round", async () => {
  const { run, calls, commits } = batchHarness(
    [
      new FakeExpiredError("expired"),
      feedResponse({ feedToken: "t2", cursors: [1], nextCursor: 1 }),
    ],
    { feedToken: "t1", cursor: 40 }
  );
  const outcome = await run;

  // 第二次请求不再携带失效令牌
  assert.deepEqual(calls, [
    { feedToken: "t1", cursor: 40, count: 5 },
    { feedToken: "", cursor: 0, count: 5 },
  ]);
  assert.deepEqual(
    commits.map((commit) => commit.event),
    ["expired", "advanced"]
  );
  assert.equal(outcome.kind, "batch");
});

test("an expired error without a token is a plain failure", async () => {
  const { run } = batchHarness([new FakeExpiredError("expired")]);
  await assert.rejects(run, FakeExpiredError);
});

test("network failures propagate instead of faking an empty library", async () => {
  const { run } = batchHarness(
    [new TypeError("fetch failed")],
    { feedToken: "t1", cursor: 3 }
  );
  await assert.rejects(run, TypeError);
});

test("a genuinely empty library reports empty without committing a cursor", async () => {
  const { run, commits } = batchHarness([
    feedResponse({ feedToken: "", cursors: [], total: 0 }),
  ]);
  assert.deepEqual(await run, { kind: "empty" });
  assert.deepEqual(commits, []);
});

test("a drained snapshot restarts instead of reporting an empty library", async () => {
  // 快照里剩下的视频全被删除/隐藏：items 空但 roundComplete，应换新快照
  const { run, calls, commits } = batchHarness(
    [
      feedResponse({ feedToken: "t1", cursors: [], nextCursor: 90, roundComplete: true }),
      feedResponse({ feedToken: "t2", cursors: [1], nextCursor: 1 }),
    ],
    { feedToken: "t1", cursor: 88 }
  );
  const outcome = await run;

  assert.deepEqual(calls[1], { feedToken: "", cursor: 0, count: 5 });
  assert.deepEqual(
    commits.map((commit) => commit.event),
    ["advanced", "snapshot-drained", "advanced"]
  );
  assert.equal(outcome.kind, "batch");
});

test("an empty batch before round completion is an error", async () => {
  const { run } = batchHarness([
    feedResponse({ feedToken: "t1", cursors: [], nextCursor: 10 }),
  ]);
  await assert.rejects(run, /no items before completion/);
});

test("recovery gives up after three attempts", async () => {
  const drained = () =>
    feedResponse({ feedToken: "t1", cursors: [], nextCursor: 90, roundComplete: true });
  const { run, calls } = batchHarness(
    [drained(), drained(), drained(), drained()],
    { feedToken: "t1", cursor: 88 }
  );
  await assert.rejects(run, /Unable to create a playable shorts feed/);
  assert.equal(calls.length, 3);
});
