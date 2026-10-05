import assert from "node:assert/strict";
import test from "node:test";
import {
  canRestoreScrollY,
  clearListingScrollEntry,
  initializeListingScrollRestore,
  listingScrollStorageKey,
  parseListingScrollEntry,
  readListingScrollEntry,
  resolveReachableScrollY,
  resolveRestoreCount,
  resolveRestoreFeedToken,
  resolveRestoreScrollY,
  writeListingScrollEntry,
  type ListingScrollStorage,
} from "../src/lib/listingScrollRestore.ts";

const QUERY_KEY = 'listing:["","","hot"]';
const FEED_TOKEN = "snapshot-token";

function memoryStorage(): ListingScrollStorage & { map: Map<string, string> } {
  const map = new Map<string, string>();
  return {
    map,
    getItem: (key) => map.get(key) ?? null,
    setItem: (key, value) => void map.set(key, value),
    removeItem: (key) => void map.delete(key),
  };
}

const throwingStorage: ListingScrollStorage = {
  getItem() {
    throw new Error("storage disabled");
  },
  setItem() {
    throw new Error("storage disabled");
  },
  removeItem() {
    throw new Error("storage disabled");
  },
};

test("a saved entry round-trips through storage under its history key", () => {
  const storage = memoryStorage();
  storage.setItem("listing_scroll_v1:history-1", JSON.stringify({
    queryKey: QUERY_KEY,
    feedToken: FEED_TOKEN,
    requestedCount: 60,
    scrollY: 1_800,
  }));
  assert.equal(readListingScrollEntry(storage, "history-1"), null,
    "Measurements from retired layouts must not be restored");
  writeListingScrollEntry(storage, "history-1", {
    queryKey: QUERY_KEY,
    feedToken: FEED_TOKEN,
    requestedCount: 60,
    scrollY: 1_800,
  });

  assert.deepEqual(readListingScrollEntry(storage, "history-1"), {
    queryKey: QUERY_KEY,
    feedToken: FEED_TOKEN,
    requestedCount: 60,
    scrollY: 1_800,
  });
  assert.equal(
    storage.map.has(listingScrollStorageKey("history-1")),
    true,
    "每条历史记录各自存一份进度"
  );
  assert.equal(readListingScrollEntry(storage, "history-2"), null);

  clearListingScrollEntry(storage, "history-1");
  assert.equal(readListingScrollEntry(storage, "history-1"), null);
});

test("unusable storage degrades to no restoration instead of throwing", () => {
  assert.equal(readListingScrollEntry(throwingStorage, "history-1"), null);
  assert.doesNotThrow(() =>
    writeListingScrollEntry(throwingStorage, "history-1", {
      queryKey: QUERY_KEY,
      feedToken: FEED_TOKEN,
      requestedCount: 40,
      scrollY: 10,
    })
  );
  assert.doesNotThrow(() => clearListingScrollEntry(throwingStorage, "history-1"));
  assert.equal(readListingScrollEntry(null, "history-1"), null);
  assert.doesNotThrow(() =>
    writeListingScrollEntry(null, "history-1", {
      queryKey: QUERY_KEY,
      feedToken: FEED_TOKEN,
      requestedCount: 40,
      scrollY: 10,
    })
  );
});

test("malformed stored entries are rejected", () => {
  assert.equal(parseListingScrollEntry(null), null);
  assert.equal(parseListingScrollEntry("not json"), null);
  assert.equal(parseListingScrollEntry("null"), null);
  assert.equal(
    parseListingScrollEntry(JSON.stringify({ requestedCount: 40, scrollY: 10 })),
    null
  );
  assert.equal(
    parseListingScrollEntry(
      JSON.stringify({ queryKey: QUERY_KEY, requestedCount: 0, scrollY: 10 })
    ),
    null
  );
  assert.equal(
    parseListingScrollEntry(
      JSON.stringify({ queryKey: QUERY_KEY, requestedCount: 4.5, scrollY: 10 })
    ),
    null
  );
  assert.equal(
    parseListingScrollEntry(
      JSON.stringify({ queryKey: QUERY_KEY, requestedCount: 40, scrollY: -1 })
    ),
    null
  );
  assert.deepEqual(
    parseListingScrollEntry(
      JSON.stringify({ queryKey: QUERY_KEY, requestedCount: 40, scrollY: 0 })
    ),
    { queryKey: QUERY_KEY, feedToken: "", requestedCount: 40, scrollY: 0 }
  );
  assert.equal(
    parseListingScrollEntry(
      JSON.stringify({
        queryKey: QUERY_KEY,
        feedToken: "x".repeat(129),
        requestedCount: 40,
        scrollY: 0,
      })
    ),
    null
  );
});

test("history restore keeps the complete cursor count even for deep histories", () => {
  const entry = {
    queryKey: QUERY_KEY,
    feedToken: FEED_TOKEN,
    requestedCount: 60,
    scrollY: 900,
  };

  assert.equal(
    resolveRestoreCount({
      entry,
      queryKey: QUERY_KEY,
      pageSize: 20,
    }),
    60
  );
  assert.equal(
    resolveRestoreCount({
      entry,
      queryKey: QUERY_KEY,
      pageSize: 14,
    }),
    60,
    "显式 cursor 不依赖响应式批次的页边界"
  );
  assert.equal(
    resolveRestoreCount({
      entry: { ...entry, requestedCount: 5_000 },
      queryKey: QUERY_KEY,
      pageSize: 20,
    }),
    5_000,
    "分批恢复完整进度，不能截断深滚位置所需的数据"
  );
  assert.equal(
    resolveRestoreCount({
      entry: { ...entry, requestedCount: 20 },
      queryKey: QUERY_KEY,
      pageSize: 20,
    }),
    0,
    "只看了首屏就按普通首屏加载"
  );
  assert.equal(
    resolveRestoreCount({
      entry,
      queryKey: 'listing:["","","latest"]',
      pageSize: 20,
    }),
    0,
    "排序变了就是另一个列表，不能沿用旧进度"
  );
  assert.equal(
    resolveRestoreCount({
      entry: null,
      queryKey: QUERY_KEY,
      pageSize: 20,
    }),
    0
  );
  assert.equal(
    resolveRestoreCount({
      entry,
      queryKey: QUERY_KEY,
      pageSize: 0,
    }),
    0
  );
});

test("the restore position only applies to the query it was saved for", () => {
  const entry = {
    queryKey: QUERY_KEY,
    feedToken: FEED_TOKEN,
    requestedCount: 60,
    scrollY: 1_200,
  };
  assert.equal(resolveRestoreScrollY(entry, QUERY_KEY), 1_200);
  assert.equal(resolveRestoreFeedToken(entry, QUERY_KEY), FEED_TOKEN);
  assert.equal(
    resolveRestoreScrollY(
      entry,
      'listing:["","","latest"]'
    ),
    0
  );
  assert.equal(resolveRestoreFeedToken(entry, 'listing:["","","latest"]'), "");
  assert.equal(resolveRestoreScrollY(null, QUERY_KEY), 0);
  assert.equal(resolveRestoreFeedToken(null, QUERY_KEY), "");
});

function browserDocument(
  storage: ListingScrollStorage,
  historyKey: string | undefined,
  navigationType: string = "reload"
) {
  return {
    sessionStorage: storage,
    history: { state: historyKey ? { key: historyKey } : null },
    performance: { getEntriesByType: () => [{ type: navigationType }] },
  } as unknown as Parameters<typeof initializeListingScrollRestore>[0];
}

const savedEntries = [
  { historyKey: "default", queryKey: "home:recommend" },
  { historyKey: "latest-history", queryKey: "home:latest" },
  { historyKey: "filtered-home-history", queryKey: 'listing:["cat","","hot"]' },
  { historyKey: "list-history", queryKey: QUERY_KEY },
];

function savedHistory() {
  const storage = memoryStorage();
  for (const { historyKey, queryKey } of savedEntries) {
    writeListingScrollEntry(storage, historyKey, {
      queryKey,
      feedToken: `${historyKey}-snapshot`,
      requestedCount: 60,
      scrollY: 1_200,
    });
  }
  return storage;
}

function assertHistoryRestores(storage: ListingScrollStorage, historyKey: string, queryKey: string) {
  const entry = readListingScrollEntry(storage, historyKey);
  assert.equal(resolveRestoreCount({ entry, queryKey, pageSize: 20 }), 60);
  assert.equal(resolveRestoreFeedToken(entry, queryKey), `${historyKey}-snapshot`);
  assert.equal(resolveRestoreScrollY(entry, queryKey), 1_200);
}

test("reloading a video preserves every previous listing's snapshot, progress and scroll", () => {
  const storage = savedHistory();
  initializeListingScrollRestore(browserDocument(storage, "video-history"));
  // Repeated reloads and navigation to another video must preserve the same history.
  initializeListingScrollRestore(browserDocument(storage, "video-history"));
  initializeListingScrollRestore(browserDocument(storage, "next-video-history"));

  for (const { historyKey, queryKey } of savedEntries) {
    assertHistoryRestores(storage, historyKey, queryKey);
  }
  assert.equal(readListingScrollEntry(storage, "new-list-history"), null);
});

test("reloading a listing starts fresh only for the entry being reloaded", () => {
  for (const reloaded of savedEntries) {
    const storage = savedHistory();
    initializeListingScrollRestore(browserDocument(
      storage,
      reloaded.historyKey === "default" ? undefined : reloaded.historyKey
    ));

    const entry = readListingScrollEntry(storage, reloaded.historyKey);
    assert.equal(resolveRestoreCount({ entry, queryKey: reloaded.queryKey, pageSize: 20 }), 0);
    assert.equal(resolveRestoreFeedToken(entry, reloaded.queryKey), "");
    assert.equal(resolveRestoreScrollY(entry, reloaded.queryKey), 0);
    for (const other of savedEntries) {
      if (other.historyKey !== reloaded.historyKey) {
        assertHistoryRestores(storage, other.historyKey, other.queryKey);
      }
    }
  }
});

test("a browser back/forward document load restores its active listing too", () => {
  const storage = savedHistory();
  initializeListingScrollRestore(browserDocument(storage, "list-history", "back_forward"));
  assertHistoryRestores(storage, "list-history", QUERY_KEY);
});

test("a new document navigation clears a copied current entry without clearing other history", () => {
  const storage = savedHistory();
  initializeListingScrollRestore(browserDocument(storage, "default", "navigate"));
  assert.equal(readListingScrollEntry(storage, "default"), null);
  assertHistoryRestores(storage, "list-history", QUERY_KEY);
});

test("initialization tolerates missing navigation timing and blocked storage", () => {
  const storage = savedHistory();
  const browser = browserDocument(storage, "list-history");
  browser.performance.getEntriesByType = () => [];
  initializeListingScrollRestore(browser);
  assert.equal(readListingScrollEntry(storage, "list-history"), null);
  assert.doesNotThrow(() => initializeListingScrollRestore(browserDocument(throwingStorage, "list-history")));
  Object.defineProperty(browser, "sessionStorage", {
    get() { throw new Error("storage access disabled"); },
  });
  assert.doesNotThrow(() => initializeListingScrollRestore(browser));
});

test("restoring waits until the document is tall enough to reach the position", () => {
  assert.equal(
    canRestoreScrollY({
      targetScrollY: 2_000,
      documentHeight: 1_500,
      viewportHeight: 800,
    }),
    false
  );
  assert.equal(
    canRestoreScrollY({
      targetScrollY: 2_000,
      documentHeight: 2_800,
      viewportHeight: 800,
    }),
    true
  );
  assert.equal(
    canRestoreScrollY({
      targetScrollY: 0,
      documentHeight: 0,
      viewportHeight: 800,
    }),
    true
  );
});

test("a position beyond a shortened list falls back to the furthest reachable point", () => {
  assert.equal(
    resolveReachableScrollY({
      targetScrollY: 9_000,
      documentHeight: 4_000,
      viewportHeight: 800,
    }),
    3_200,
    "停在已恢复内容的末尾，而不是回到顶部"
  );
  assert.equal(
    resolveReachableScrollY({
      targetScrollY: 1_000,
      documentHeight: 4_000,
      viewportHeight: 800,
    }),
    1_000
  );
  assert.equal(
    resolveReachableScrollY({
      targetScrollY: 500,
      documentHeight: 600,
      viewportHeight: 800,
    }),
    0
  );
});
