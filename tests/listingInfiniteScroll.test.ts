import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const listingPageSource = readFileSync(
  new URL("../src/pages/ListingPage.tsx", import.meta.url),
  "utf8"
);
const virtualGridSource = readFileSync(
  new URL("../src/components/VirtualVideoGrid.tsx", import.meta.url),
  "utf8"
);
const infiniteListingHookSource = readFileSync(
  new URL("../src/lib/useInfiniteListing.ts", import.meta.url),
  "utf8"
);
const scrollRestoreHookSource = readFileSync(
  new URL("../src/lib/useListingScrollRestore.ts", import.meta.url),
  "utf8"
);
const layoutCss = readFileSync(
  new URL("../src/styles/layout.css", import.meta.url),
  "utf8"
);
const videoCardCss = readFileSync(
  new URL("../src/styles/video-card.css", import.meta.url),
  "utf8"
);

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

test("the listing page renders its videos through the virtual grid", () => {
  assert.match(
    listingPageSource,
    /<VirtualVideoGrid\s+videos=\{items\}[\s\S]*?onRangeChange=\{handleRangeChange\}/
  );
  // 骨架屏之外不再整页渲染列表。
  assert.doesNotMatch(
    listingPageSource,
    /<VideoGrid\s+videos=\{items\}/
  );
});

test("scrolling near the end of the rendered window loads the next batch", () => {
  assert.match(
    listingPageSource,
    /shouldLoadMore\(\{[\s\S]*?endIndex: range\.endIndex,[\s\S]*?itemCount: items\.length,[\s\S]*?columns: range\.columns,[\s\S]*?hasMore,[\s\S]*?loading: loadingMore,[\s\S]*?prefetchRows: PREFETCH_ROWS,[\s\S]*?\}\)/
  );
  assert.match(listingPageSource, /loadMore\(\);/);
  assert.match(listingPageSource, /const PREFETCH_ROWS = 2;/);
});

test("the listing page shows loading, end and tail-error states for infinite scroll", () => {
  assert.match(listingPageSource, /\{showTailError \? \(/);
  assert.match(
    listingPageSource,
    /<ListingLoadError hasContent onRetry=\{listing\.retry\} \/>/
  );
  assert.match(
    listingPageSource,
    /className="listing-infinite-status"[\s\S]*?role="status"[\s\S]*?aria-live="polite"[\s\S]*?正在加载更多/
  );
  assert.match(
    listingPageSource,
    /listing\.exhausted \? \([\s\S]*?listing-infinite-status--end[\s\S]*?没有更多了/
  );

  const status = ruleBody(layoutCss, ".listing-infinite-status");
  assert.match(status, /display\s*:\s*flex/);
  assert.match(status, /justify-content\s*:\s*center/);
});

test("the virtual grid renders whole rows through the window virtualizer", () => {
  assert.match(
    virtualGridSource,
    /import \{ useWindowVirtualizer \} from "@tanstack\/react-virtual"/
  );
  assert.match(
    virtualGridSource,
    /useWindowVirtualizer\(\{[\s\S]*?count: rowCount,[\s\S]*?estimateSize: \(\) => rowHeight,[\s\S]*?overscan: overscanRows,[\s\S]*?scrollMargin,/
  );
  assert.match(
    virtualGridSource,
    /const \{ start, end \} = virtualRowRange\(\s*virtualRow\.index,\s*columns,\s*videos\.length\s*\)/
  );
  // 卡片的加载优先级按列表里的绝对下标判断，而不是行内偏移。
  assert.match(virtualGridSource, /const index = start \+ offset;/);
  assert.match(virtualGridSource, /eager=\{index < eagerCount\}/);
  assert.match(virtualGridSource, /highPriority=\{index < highPriorityCount\}/);
});

test("the virtual canvas keeps the scrollbar as tall as the whole list", () => {
  assert.match(
    virtualGridSource,
    /className="video-grid-virtual-canvas"\s*style=\{\{ height: virtualizer\.getTotalSize\(\) \}\}/
  );
  assert.match(
    virtualGridSource,
    /transform: `translateY\(\$\{\s*virtualRow\.start - virtualizer\.options\.scrollMargin\s*\}px\)`/
  );

  const canvas = ruleBody(videoCardCss, ".video-grid-virtual-canvas");
  assert.match(canvas, /position\s*:\s*relative/);
  const row = ruleBody(videoCardCss, ".video-grid--virtual-row");
  assert.match(row, /position\s*:\s*absolute/);
  // 行间距做进行自身的下内边距，测到的行高才等于"行 + 间距"。
  assert.match(row, /padding-bottom\s*:\s*var\(--space-4\)/);
});

test("row heights and column counts are measured from the real layout", () => {
  assert.match(virtualGridSource, /ref=\{virtualizer\.measureElement\}/);
  assert.match(virtualGridSource, /data-index=\{virtualRow\.index\}/);
  assert.match(
    virtualGridSource,
    /virtualGridColumns\(window\.getComputedStyle\(row\)\)/
  );
  assert.match(virtualGridSource, /const nextMargin = rect\.top \+ window\.scrollY;/);
  assert.match(virtualGridSource, /new ResizeObserver\(measure\)/);
  assert.match(virtualGridSource, /observer\?\.disconnect\(\)/);
  assert.match(
    virtualGridSource,
    /window\.removeEventListener\("resize", measure\)/
  );
  // 列数变了每行内容都会重排，之前测到的行高必须作废。
  assert.match(
    virtualGridSource,
    /virtualizer\.measure\(\);\s*\}, \[columns, compact, virtualizer\]\)/
  );
});

test("the infinite listing hook keeps one in-flight batch per query", () => {
  assert.match(
    infiniteListingHookSource,
    /const INFINITE_LISTING_CACHE_TTL_MS = 60_000/
  );
  assert.match(infiniteListingHookSource, /controllerRef\.current\?\.abort\(\)/);
  assert.match(
    infiniteListingHookSource,
    /if \(current\.status === "initial-loading" \|\| current\.status === "loading-more"\) \{\s*return;\s*\}/
  );
  assert.match(
    infiniteListingHookSource,
    /if \(!batchOptions\.force && current\.status === "error"\) return;/
  );
  // 首屏失败整段重来，尾部失败只重试失败的那一批。
  assert.match(
    infiniteListingHookSource,
    /if \(stateRef\.current\.items\.length === 0\) \{\s*reload\(\);\s*return;\s*\}\s*requestBatch\(\{ force: true \}\);/
  );
  // 轮换 feed 的游标在服务端且不幂等，补回来的不是原来那批视频。
  assert.match(
    infiniteListingHookSource,
    /const restoreCount = sourceRef\.current\.supportsRestore\s*\? restoreCountRef\.current\s*:\s*0;/
  );
  assert.match(
    infiniteListingHookSource,
    /size: initialBatchSize\(restoreCount, batchSize\),/
  );
});

test("browser history navigation restores both the loaded batches and the position", () => {
  assert.match(listingPageSource, /historyKey: location\.key/);
  assert.match(listingPageSource, /restoreCount: restoreTarget\.count/);
  assert.match(
    listingPageSource,
    /useListingScrollRestore\(\{[\s\S]*?target: restoreTarget,[\s\S]*?requestedCount: listing\.requestedCount,[\s\S]*?itemCount: listing\.items\.length,/
  );
  // 恢复条数必须在数据层发起首个请求之前解析，所以在渲染期读取。
  assert.match(
    scrollRestoreHookSource,
    /if \(!targetRef\.current \|\| targetRef\.current\.historyKey !== input\.historyKey\)/
  );
  assert.match(scrollRestoreHookSource, /canRestoreScrollY\(\{/);
  assert.match(scrollRestoreHookSource, /window\.scrollTo\(0, targetScrollY\)/);
  assert.match(scrollRestoreHookSource, /if \(pendingScrollYRef\.current > 0\) return;/);
  assert.match(
    scrollRestoreHookSource,
    /window\.addEventListener\("pagehide", handlePageHide\)/
  );
  // 卸载时列表 DOM 已被详情页顶掉，window.scrollY 会被压缩，只能用滚动时记下的值。
  assert.match(
    scrollRestoreHookSource,
    /lastScrollYRef\.current = Math\.max\(0, Math\.round\(window\.scrollY\)\);/
  );
  assert.match(scrollRestoreHookSource, /save\(lastScrollYRef\.current\);/);
});
