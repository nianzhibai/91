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

test("grid columns are read from the computed template and fall back to one", () => {
  const grid = (gridTemplateColumns: string) => ({
    display: "grid",
    gridTemplateColumns,
  });

  assert.equal(virtualGridColumns(grid("240px 240px 240px 240px")), 4);
  assert.equal(virtualGridColumns(grid("  180px   180px  ")), 2);
  assert.equal(virtualGridColumns(grid("none")), 1);
  assert.equal(virtualGridColumns(grid("")), 1);
  // compact 视图是 flex 列表：此时 grid-template-columns 仍是未解析的写法，
  // 按空格切会被误读成 3 列，进而把三张卡塞进同一个虚拟行。
  assert.equal(
    virtualGridColumns({
      display: "flex",
      gridTemplateColumns: "repeat(4, minmax(0, 1fr))",
    }),
    1
  );
  assert.equal(
    virtualGridColumns({
      display: "grid",
      gridTemplateColumns: "repeat(4, minmax(0, 1fr))",
    }),
    1,
    "轨道没被解析成具体宽度时不能猜列数"
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
