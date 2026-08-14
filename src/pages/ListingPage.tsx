import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useSearchParams } from "react-router";
import { AdminEmptyVisual } from "@/admin/AdminEmptyVisual";
import { AppShell } from "@/components/AppShell";
import { ListingLoadError } from "@/components/ListingLoadError";
import { PromoStrip } from "@/components/PromoStrip";
import { SearchPanel } from "@/components/SearchPanel";
import { SortToolbar } from "@/components/SortToolbar";
import { TagCloud } from "@/components/TagCloud";
import { VideoGrid } from "@/components/VideoGrid";
import {
  VirtualVideoGrid,
  type VirtualGridRange,
} from "@/components/VirtualVideoGrid";
import { infiniteListingKey } from "@/lib/infiniteListing";
import {
  readListingSort,
  readListingView,
  withListingNavigation,
  withListingPage,
  withListingView,
} from "@/lib/listingSearchParams";
import { MOBILE_VIDEO_PAGE_SIZE, useIsMobile } from "@/lib/responsive";
import { useInfiniteListing } from "@/lib/useInfiniteListing";
import {
  useListingRestoreTarget,
  useListingScrollRestore,
} from "@/lib/useListingScrollRestore";
import { shouldLoadMore } from "@/lib/virtualGrid";

const DESKTOP_PAGE_SIZE = 20;

// 距列表尾部还有两行时就续下一批，滚动到底之前数据已经在路上。
const PREFETCH_ROWS = 2;

const EMPTY_RANGE: VirtualGridRange = {
  startIndex: 0,
  endIndex: 0,
  columns: 1,
};

export default function ListingPage() {
  const [params, setParams] = useSearchParams();
  const location = useLocation();
  const keyword = params.get("q") ?? "";
  const tag = params.get("tag") ?? "";
  const sort = readListingSort(params);
  const view = readListingView(params);
  const isMobile = useIsMobile();
  const pageSize = isMobile ? MOBILE_VIDEO_PAGE_SIZE : DESKTOP_PAGE_SIZE;
  const queryKey = infiniteListingKey({ q: keyword, tag, sort, pageSize });

  const restoreTarget = useListingRestoreTarget({
    historyKey: location.key,
    queryKey,
    pageSize,
  });
  const listing = useInfiniteListing({
    q: keyword,
    tag,
    sort,
    pageSize,
    restoreCount: restoreTarget.count,
  });
  useListingScrollRestore({
    target: restoreTarget,
    queryKey,
    requestedCount: listing.requestedCount,
    itemCount: listing.items.length,
  });

  const items = listing.items;
  const hasContent = items.length > 0;
  const showSkeleton = listing.initialLoading && !hasContent;
  const showEmptyError = listing.failed && !hasContent;
  const showTailError = listing.failed && hasContent;
  const hasActiveFilter = keyword.trim().length > 0 || tag.trim().length > 0;
  const eagerCount = isMobile ? 2 : 4;
  const [range, setRange] = useState<VirtualGridRange>(EMPTY_RANGE);
  const previousQueryKeyRef = useRef(queryKey);

  useEffect(() => {
    document.title = keyword
      ? `搜索 "${keyword}"`
      : tag
      ? `标签 ${tag}`
      : "视频列表";
  }, [keyword, tag]);

  // 无限滚动没有页码，旧链接里的 page 参数只会让 URL 与实际内容不符。
  useEffect(() => {
    if (!params.has("page")) return;
    setParams((current) => withListingPage(current, 1), { replace: true });
  }, [params, setParams]);

  // 换排序/换标签是一次全新的列表，回到顶部再开始累积。平滑滚动会被虚拟
  // 列表的行高补偿打断而停在半路，所以直接落到顶部。
  useEffect(() => {
    if (previousQueryKeyRef.current === queryKey) return;
    previousQueryKeyRef.current = queryKey;
    window.scrollTo({ top: 0, behavior: "auto" });
  }, [queryKey]);

  const handleRangeChange = useCallback((next: VirtualGridRange) => {
    setRange((current) =>
      current.startIndex === next.startIndex &&
      current.endIndex === next.endIndex &&
      current.columns === next.columns
        ? current
        : next
    );
  }, []);

  const { loadMore, loadingMore, hasMore } = listing;
  useEffect(() => {
    if (
      shouldLoadMore({
        endIndex: range.endIndex,
        itemCount: items.length,
        columns: range.columns,
        hasMore,
        loading: loadingMore,
        prefetchRows: PREFETCH_ROWS,
      })
    ) {
      loadMore();
    }
  }, [hasMore, items.length, loadMore, loadingMore, range]);

  return (
    <AppShell>
      <div className="container page-section listing-discovery-section">
        <PromoStrip />
        <SearchPanel
          variant="uiverse"
          placeholder=""
          className="search-panel--public search-panel--transparent"
        />
        <TagCloud />
      </div>

      <div className="container page-section listing-primary-section">
        <SortToolbar
          sort={sort}
          view={view}
          sortDisabled={listing.initialLoading}
          onSortChange={(nextSort) => {
            setParams(
              withListingNavigation(params, { sort: nextSort, page: 1 }),
              { replace: true }
            );
          }}
          onViewChange={(nextView) => {
            setParams(withListingView(params, nextView), { replace: true });
          }}
        />

        {showSkeleton ? (
          <VideoGrid
            videos={[]}
            loading
            compact={view === "compact"}
            skeletonCount={pageSize}
          />
        ) : showEmptyError ? (
          <ListingLoadError
            hasContent={false}
            onRetry={listing.retry}
            emptyClassName="admin-empty-state admin-empty-state--plain listing-empty-state"
          />
        ) : !hasContent ? (
          <AdminEmptyVisual
            variant={hasActiveFilter ? "no-results" : "empty"}
            text={hasActiveFilter ? "未查询到" : "当前库中没有视频"}
            className="admin-empty-state admin-empty-state--plain listing-empty-state"
          />
        ) : (
          <>
            <VirtualVideoGrid
              videos={items}
              compact={view === "compact"}
              eagerCount={eagerCount}
              highPriorityCount={1}
              onRangeChange={handleRangeChange}
            />

            {showTailError ? (
              <ListingLoadError hasContent onRetry={listing.retry} />
            ) : loadingMore ? (
              <div
                className="listing-infinite-status"
                role="status"
                aria-live="polite"
              >
                <span
                  className="video-grid-refresh-overlay__spinner"
                  aria-hidden="true"
                />
                <span>正在加载更多</span>
              </div>
            ) : listing.exhausted ? (
              <div className="listing-infinite-status listing-infinite-status--end">
                没有更多了
              </div>
            ) : null}
          </>
        )}
      </div>
    </AppShell>
  );
}
