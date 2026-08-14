import assert from "node:assert/strict";
import test from "node:test";
import {
  canRestoreScrollY,
  clearListingScrollEntry,
  listingScrollStorageKey,
  MAX_RESTORE_ITEMS,
  parseListingScrollEntry,
  readListingScrollEntry,
  resolveReachableScrollY,
  resolveRestoreCount,
  resolveRestoreScrollY,
  writeListingScrollEntry,
  type ListingScrollStorage,
} from "../src/lib/listingScrollRestore.ts";

const QUERY_KEY = '["","","hot",20]';

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
  writeListingScrollEntry(storage, "history-1", {
    queryKey: QUERY_KEY,
    requestedCount: 60,
    scrollY: 1_800,
  });

  assert.deepEqual(readListingScrollEntry(storage, "history-1"), {
    queryKey: QUERY_KEY,
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
      requestedCount: 40,
      scrollY: 10,
    })
  );
  assert.doesNotThrow(() => clearListingScrollEntry(throwingStorage, "history-1"));
  assert.equal(readListingScrollEntry(null, "history-1"), null);
  assert.doesNotThrow(() =>
    writeListingScrollEntry(null, "history-1", {
      queryKey: QUERY_KEY,
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
    { queryKey: QUERY_KEY, requestedCount: 40, scrollY: 0 }
  );
});

test("the restore request is rounded up to a page boundary and capped", () => {
  const entry = { queryKey: QUERY_KEY, requestedCount: 60, scrollY: 900 };

  assert.equal(resolveRestoreCount({ entry, queryKey: QUERY_KEY, pageSize: 20 }), 60);
  assert.equal(
    resolveRestoreCount({ entry, queryKey: QUERY_KEY, pageSize: 14 }),
    70,
    "换成手机页大小时向上取整，游标仍落在页边界"
  );
  assert.equal(
    resolveRestoreCount({
      entry: { ...entry, requestedCount: 5_000 },
      queryKey: QUERY_KEY,
      pageSize: 20,
    }),
    MAX_RESTORE_ITEMS,
    "深滚之后的返回不能打出一个无上限的大请求"
  );
  assert.equal(
    resolveRestoreCount({ entry: { ...entry, requestedCount: 20 }, queryKey: QUERY_KEY, pageSize: 20 }),
    0,
    "只看了首屏就按普通首屏加载"
  );
  assert.equal(
    resolveRestoreCount({ entry, queryKey: '["","","latest",20]', pageSize: 20 }),
    0,
    "排序变了就是另一个列表，不能沿用旧进度"
  );
  assert.equal(resolveRestoreCount({ entry: null, queryKey: QUERY_KEY, pageSize: 20 }), 0);
  assert.equal(resolveRestoreCount({ entry, queryKey: QUERY_KEY, pageSize: 0 }), 0);
});

test("the restore position only applies to the query it was saved for", () => {
  const entry = { queryKey: QUERY_KEY, requestedCount: 60, scrollY: 1_200 };
  assert.equal(resolveRestoreScrollY(entry, QUERY_KEY), 1_200);
  assert.equal(resolveRestoreScrollY(entry, '["","","latest",20]'), 0);
  assert.equal(resolveRestoreScrollY(null, QUERY_KEY), 0);
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

test("a position deeper than the restore cap falls back to the furthest reachable point", () => {
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
