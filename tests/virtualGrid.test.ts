import assert from "node:assert/strict";
import test from "node:test";
import {
  shouldLoadMore,
  virtualGridColumns,
  virtualRowCount,
  virtualRowRange,
} from "../src/lib/virtualGrid.ts";

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
    virtualGridColumns({ compact: false, mobile: false, tablet: false }),
    4
  );
  assert.equal(
    virtualGridColumns({ compact: false, mobile: false, tablet: true }),
    3
  );
  assert.equal(
    virtualGridColumns({ compact: false, mobile: true, tablet: true }),
    2
  );
  assert.equal(
    virtualGridColumns({ compact: true, mobile: false, tablet: false }),
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
