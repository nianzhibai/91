import assert from "node:assert/strict";
import test from "node:test";
import { Virtualizer, type VirtualItem } from "@tanstack/react-virtual";
import {
  matchingVirtualGridSnapshot,
  parseVirtualGridSnapshot,
  shouldLoadMore,
  virtualGridColumns,
  virtualRowCount,
  virtualRowRange,
} from "../src/lib/virtualGrid.ts";
import { parseListingScrollEntry } from "../src/lib/listingScrollRestore.ts";

function measuredGrid(initialMeasurementsCache: VirtualItem[] = []) {
  return new Virtualizer({
    count: 80,
    getScrollElement: () => null,
    estimateSize: () => 260,
    getItemKey: (index) => `video-${index * 4}`,
    initialRect: { width: 1440, height: 900 },
    initialOffset: 7000,
    scrollMargin: 223,
    initialMeasurementsCache,
    scrollToFn: () => {},
    observeElementRect: () => {},
    observeElementOffset: () => {},
  });
}

test("a cold return restores the same visible rows and offsets from measured geometry", () => {
  const original = measuredGrid();
  original.getVirtualItems();
  for (let row = 0; row < 40; row += 1) {
    original.resizeItem(row, row % 3 === 0 ? 235 : 260);
  }
  const entry = parseListingScrollEntry(JSON.stringify({
    queryKey: "home:recommend",
    feedToken: "feed-1",
    requestedCount: 320,
    scrollY: 7000,
    grid: {
      viewportWidth: 1440,
      columns: 4,
      scrollMargin: 223,
      measurements: original.takeSnapshot(),
    },
  }));
  assert.ok(entry?.grid);
  const restored = measuredGrid(entry.grid.measurements);
  const visibleRows = (grid: ReturnType<typeof measuredGrid>) =>
    grid.getVirtualItems().filter(row => row.end > 7000 && row.start < 7900)
      .map(row => ({ key: row.key, top: row.start - 7000 }));

  assert.notDeepEqual(visibleRows(measuredGrid()), visibleRows(original),
    "scrollY alone loses the visible video when measured heights differ from estimates");
  assert.deepEqual(visibleRows(restored), visibleRows(original));
});

test("row measurements only apply to the viewport and grid layout that produced them", () => {
  const snapshot = {
    viewportWidth: 1440, columns: 4, scrollMargin: 223,
    measurements: [],
  };
  assert.equal(matchingVirtualGridSnapshot(snapshot, snapshot), snapshot);
  for (const layout of [
    { ...snapshot, viewportWidth: 1200 },
    { ...snapshot, columns: 2 },
  ]) {
    assert.equal(matchingVirtualGridSnapshot(snapshot, layout), null);
  }
});

test("malformed grid geometry cannot corrupt otherwise usable listing history", () => {
  assert.equal(parseVirtualGridSnapshot(null), null);
  assert.equal(parseVirtualGridSnapshot({}), null);
  const grid = {
    viewportWidth: 1440, columns: 4, scrollMargin: 223,
    measurements: [{ index: 0, key: "v1", start: 223, size: -20, end: 203, lane: 0 }],
  };
  assert.equal(parseVirtualGridSnapshot(grid), null);
  const entry = parseListingScrollEntry(JSON.stringify({
    queryKey: "home:recommend", feedToken: "feed-1", requestedCount: 40, scrollY: 1000, grid,
  }));
  assert.equal(entry?.scrollY, 1000);
  assert.equal(entry?.grid, undefined);
});

test("the flat video list is folded into whole rows", () => {
  assert.equal(virtualRowCount(0, 4), 0);
  assert.equal(virtualRowCount(8, 4), 2);
  assert.equal(virtualRowCount(9, 4), 3, "末行不满也要占一行");
  assert.equal(virtualRowCount(9, 1), 9);
});

test("row folding degrades to a single column instead of dividing by zero", () => {
  assert.equal(virtualRowCount(3, 0), 3);
  assert.equal(virtualRowCount(3, Number.NaN), 3);
  assert.deepEqual(virtualRowRange(1, 0, 3), { start: 1, end: 2 });
});

test("each row maps to its own slice of the list", () => {
  assert.deepEqual(virtualRowRange(0, 4, 10), { start: 0, end: 4 });
  assert.deepEqual(virtualRowRange(1, 4, 10), { start: 4, end: 8 });
  assert.deepEqual(
    virtualRowRange(2, 4, 10),
    { start: 8, end: 10 },
    "末行按实际条数收口，不能读到列表外"
  );
  assert.deepEqual(virtualRowRange(5, 4, 10), { start: 10, end: 10 });
  assert.deepEqual(virtualRowRange(-1, 4, 10), { start: 0, end: 0 });
  assert.deepEqual(virtualRowRange(0, 4, 0), { start: 0, end: 0 });
});

test("grid columns are known before the first browser paint", () => {
  assert.equal(
    virtualGridColumns({ mobile: false, tablet: false }),
    4
  );
  assert.equal(
    virtualGridColumns({ mobile: false, tablet: true }),
    3
  );
  assert.equal(
    virtualGridColumns({ mobile: true, tablet: true }),
    1
  );
});

test("the load-more trigger comes from the render window and stops at the end", () => {
  const base = {
    itemCount: 60,
    columns: 4,
    hasMore: true,
    loading: false,
    prefetchRows: 2,
  };

  assert.equal(shouldLoadMore({ ...base, endIndex: 40 }), false);
  assert.equal(shouldLoadMore({ ...base, endIndex: 52 }), true);
  assert.equal(shouldLoadMore({ ...base, endIndex: 60 }), true);
  assert.equal(
    shouldLoadMore({ ...base, endIndex: 60, loading: true }),
    false,
    "a request in flight must not be duplicated"
  );
  assert.equal(
    shouldLoadMore({ ...base, endIndex: 60, hasMore: false }),
    false,
    "an exhausted list must stop triggering loads"
  );
});
