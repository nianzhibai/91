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
const infiniteFeedStatusSource = readFileSync(
  new URL("../src/components/InfiniteFeedStatus.tsx", import.meta.url),
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
const videoCardSource = readFileSync(
  new URL("../src/components/VideoCard.tsx", import.meta.url),
  "utf8"
);
const searchPanelSource = readFileSync(
  new URL("../src/components/SearchPanel.tsx", import.meta.url),
  "utf8"
);
const tagCloudSource = readFileSync(
  new URL("../src/components/TagCloud.tsx", import.meta.url),
  "utf8"
);
const sortToolbarSource = readFileSync(
  new URL("../src/components/SortToolbar.tsx", import.meta.url),
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
    /<VirtualVideoGrid\s+videos=\{items\}[\s\S]*?onLoadMore=\{listing\.loadMore\}/
  );
  // 骨架屏之外不再整页渲染列表。
  assert.doesNotMatch(
    listingPageSource,
    /<VideoGrid\s+videos=\{items\}/
  );
});

test("scrolling near the end of the rendered window loads the next batch", () => {
  assert.match(
    virtualGridSource,
    /shouldLoadMore\(\{[\s\S]*?endIndex: Math\.min\(\(lastRow \+ 1\) \* columns, videos\.length\),[\s\S]*?itemCount: videos\.length,[\s\S]*?loading: loadingMore,[\s\S]*?prefetchRows,[\s\S]*?\}\)/
  );
  assert.match(virtualGridSource, /onLoadMore\(\);/);
  assert.match(listingPageSource, /onLoadMore=\{listing\.loadMore\}/);
  assert.match(listingPageSource, /const PREFETCH_ROWS = 2;/);
});

test("the listing page shows loading, end and tail-error states for infinite scroll", () => {
  assert.match(listingPageSource, /\{showTailError \? \(/);
  assert.match(
    listingPageSource,
    /<ListingLoadError hasContent onRetry=\{listing\.retry\} \/>/
  );
  assert.match(listingPageSource, /<InfiniteFeedStatus state="loading" \/>/);
  assert.match(listingPageSource, /listing\.exhausted \? \(\s*<InfiniteFeedStatus state="end" \/>/);
  assert.match(
    infiniteFeedStatusSource,
    /className="listing-infinite-status"[\s\S]*?role="status"[\s\S]*?aria-live="polite"[\s\S]*?正在加载更多/
  );
  assert.match(infiniteFeedStatusSource, /listing-infinite-status--end[\s\S]*?没有更多了/);

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
    /useWindowVirtualizer\(\{[\s\S]*?count: virtualRowCountWithTail,[\s\S]*?estimateSize,[\s\S]*?overscan: overscanRows,[\s\S]*?scrollMargin,[\s\S]*?directDomUpdates: true,/
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

test("the scrollbar grows with loaded content and keeps only a fixed tail row", () => {
  assert.match(
    virtualGridSource,
    /ref=\{virtualizer\.containerRef\}[\s\S]*?className="video-grid-virtual-canvas"/
  );
  assert.match(virtualGridSource, /const TAIL_ROW_HEIGHT = 56/);
  assert.match(virtualGridSource, /className="video-grid-virtual-tail"/);
  assert.match(
    virtualGridSource,
    /const virtualRowCountWithTail = loadedRowCount \+ \(hasTailRow \? 1 : 0\)/
  );
  assert.doesNotMatch(virtualGridSource, /totalCount|knownTotal|remainingRowCount|reserveTotalHeight/);
  assert.doesNotMatch(virtualGridSource, /style=\{\{ height: virtualizer\.getTotalSize\(\) \}\}/);

  const canvas = ruleBody(videoCardCss, ".video-grid-virtual-canvas");
  assert.match(canvas, /position\s*:\s*relative/);
  const row = ruleBody(videoCardCss, ".video-grid--virtual-row");
  assert.match(row, /position\s*:\s*absolute/);
  // 行间距做进行自身的下内边距，测到的行高才等于"行 + 间距"。
  assert.match(row, /padding-bottom\s*:\s*var\(--space-4\)/);
  const tail = ruleBody(videoCardCss, ".video-grid-virtual-tail");
  assert.match(tail, /position\s*:\s*absolute/);
});

test("layout measurement stays off the per-frame scroll path", () => {
  assert.match(virtualGridSource, /ref=\{virtualizer\.measureElement\}/);
  assert.match(virtualGridSource, /data-index=\{virtualRow\.index\}/);
  assert.match(virtualGridSource, /useState\(readResponsiveGridColumns\)/);
  assert.match(virtualGridSource, /const columns = compact \? 1 : responsiveColumns/);
  assert.match(virtualGridSource, /const nextMargin = rect\.top \+ window\.scrollY;/);
  assert.match(virtualGridSource, /new ResizeObserver\(\(\[entry\]\) =>/);
  assert.match(virtualGridSource, /observer\?\.disconnect\(\)/);
  assert.doesNotMatch(virtualGridSource, /window\.getComputedStyle|querySelector<HTMLElement>/);
  // 只有断点/视图变化清空测量缓存，追加视频不会重测全部旧行。
  assert.match(
    virtualGridSource,
    /if \(previousLayoutIdentityRef\.current === layoutIdentity\) return;[\s\S]*?virtualizer\.measure\(\);[\s\S]*?\}, \[layoutIdentity, virtualizer\]\)/
  );
});

test("scrolling rerenders only the virtual list and memoized cards", () => {
  assert.doesNotMatch(listingPageSource, /useState<VirtualGridRange>|setRange|onRangeChange/);
  assert.match(virtualGridSource, /directDomUpdates: true/);
  assert.match(videoCardSource, /export const VideoCard = memo\(function VideoCard/);
  assert.match(searchPanelSource, /export const SearchPanel = memo\(function SearchPanel/);
  assert.match(tagCloudSource, /export const TagCloud = memo\(function TagCloud/);
  assert.match(sortToolbarSource, /export const SortToolbar = memo\(function SortToolbar/);
});

test("the infinite listing hook keeps one in-flight batch per query", () => {
  assert.match(
    infiniteListingHookSource,
    /const INFINITE_LISTING_CACHE_TTL_MS = 60_000/
  );
  assert.match(infiniteListingHookSource, /controllerRef\.current\?\.abort\(\)/);
  assert.match(
    infiniteListingHookSource,
    /if \(controllerRef\.current && !controllerRef\.current\.signal\.aborted\) return;/
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
  // token 过期后重新建快照，并一次补回原游标附近的内容。
  assert.match(
    infiniteListingHookSource,
    /if \(request\.cursor\.feedToken && feed\.isExpiredError\(error\)\)/
  );
  assert.match(
    infiniteListingHookSource,
    /size: initialBatchSize\(restoreCount, feed\.batchSize\),/
  );
  assert.match(
    infiniteListingHookSource,
    /infiniteListingCacheMatchesRestore\(\{[\s\S]*?restoreFeedToken: restore\.feedToken,[\s\S]*?restoreCount: restore\.count,/
  );
});

test("browser history navigation restores both the loaded batches and the position", () => {
  assert.match(listingPageSource, /historyKey: location\.key/);
  assert.match(listingPageSource, /restoreCount: restoreTarget\.count/);
  assert.match(listingPageSource, /restoreFeedToken: restoreTarget\.feedToken/);
  assert.match(
    listingPageSource,
    /useListingScrollRestore\(\{[\s\S]*?target: restoreTarget,[\s\S]*?requestedCount: listing\.requestedCount,[\s\S]*?feedToken: listing\.feedToken,[\s\S]*?itemCount: listing\.items\.length,/
  );
  // 恢复条数必须在数据层发起首个请求之前解析，所以在渲染期读取。
  assert.match(
    scrollRestoreHookSource,
    /targetRef\.current\.historyKey !== input\.historyKey[\s\S]*?targetRef\.current\.queryKey !== input\.queryKey/
  );
  assert.match(scrollRestoreHookSource, /canRestoreScrollY\(\{/);
  assert.match(scrollRestoreHookSource, /window\.scrollTo\(0, targetScrollY\)/);
  assert.match(scrollRestoreHookSource, /if \(session\.pendingScrollY > 0 \|\| session\.requestedCount <= 0\) return;/);
  assert.match(
    scrollRestoreHookSource,
    /window\.addEventListener\("pagehide", handlePageHide\)/
  );
  // 卸载时列表 DOM 已被详情页顶掉，window.scrollY 会被压缩，只能用滚动时记下的值。
  const scrollHandler = scrollRestoreHookSource.match(
    /const handleScroll = \(\) => \{[\s\S]*?\n    \};/
  )?.[0] ?? "";
  assert.match(scrollHandler, /session\.lastScrollY = Math\.max\(0, Math\.round\(window\.scrollY\)\)/);
  assert.doesNotMatch(scrollHandler, /writeListingScrollEntry|sessionStorage|requestAnimationFrame|persist\(/);
  assert.match(scrollRestoreHookSource, /const handlePageHide = \(\) => \{[\s\S]*?persist\(\)/);
  assert.match(scrollRestoreHookSource, /return \(\) => \{[\s\S]*?persist\(\);/);
});
