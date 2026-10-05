import { useCallback, useEffect, useMemo, useRef } from "react";
import { useLocation, useSearchParams } from "react-router";
import { AdminEmptyVisual } from "@/admin/AdminEmptyVisual";
import { AppShell } from "@/components/AppShell";
import { InfiniteFeedStatus } from "@/components/InfiniteFeedStatus";
import { ListingLoadError } from "@/components/ListingLoadError";
import { PromoStrip } from "@/components/PromoStrip";
import { SearchPanel } from "@/components/SearchPanel";
import { SortToolbar } from "@/components/SortToolbar";
import { TagCloud } from "@/components/TagCloud";
import { VideoGrid } from "@/components/VideoGrid";
import { VirtualVideoGrid } from "@/components/VirtualVideoGrid";
import { listingFeedSource } from "@/lib/infiniteFeedSource";
import {
  readListingSort,
  normalizeListingSearchParams,
  withListingNavigation,
} from "@/lib/listingSearchParams";
import { MOBILE_VIDEO_PAGE_SIZE, useIsMobile } from "@/lib/responsive";
import { useRouteActivity } from "@/lib/routeActivity";
import { useInfiniteListing } from "@/lib/useInfiniteListing";
import {
  useListingRestoreTarget,
  useListingScrollRestore,
} from "@/lib/useListingScrollRestore";
import type { SortKey } from "@/types";

const DESKTOP_PAGE_SIZE = 20;

// 距列表尾部还有两行时就续下一批，滚动到底之前数据已经在路上。
const PREFETCH_ROWS = 2;

export default function ListingPage() {
  const [params, setParams] = useSearchParams();
  const location = useLocation();
  const routeActive = useRouteActivity();
  const keyword = params.get("q") ?? "";
  const tag = params.get("tag") ?? "";
  const sort = readListingSort(params);
  const isMobile = useIsMobile();
  const pageSize = isMobile ? MOBILE_VIDEO_PAGE_SIZE : DESKTOP_PAGE_SIZE;
  const source = useMemo(
    () => listingFeedSource({ q: keyword, tag, sort, pageSize }),
    [keyword, tag, sort, pageSize]
  );
  const queryKey = source.key;

  const restoreTarget = useListingRestoreTarget({
    historyKey: location.key,
    queryKey,
    pageSize,
  });
  const listing = useInfiniteListing(source, {
    pausePagination: !routeActive,
    restoreCount: restoreTarget.count,
    restoreFeedToken: restoreTarget.feedToken,
  });
  const { gridRef, gridSnapshot } = useListingScrollRestore({
    target: restoreTarget,
    queryKey,
    requestedCount: listing.requestedCount,
    feedToken: listing.feedToken,
    itemCount: listing.items.length,
    active: routeActive,
  });

  const items = listing.items;
  const hasContent = items.length > 0;
  const showSkeleton = listing.initialLoading && !hasContent;
  const showEmptyError = listing.failed && !hasContent;
  const showTailError = listing.failed && hasContent;
  const hasActiveFilter = keyword.trim().length > 0 || tag.trim().length > 0;
  const eagerCount = isMobile ? 2 : 4;
  const previousQueryKeyRef = useRef(queryKey);

  useEffect(() => {
    document.title = keyword
      ? `搜索 "${keyword}"`
      : tag
      ? `标签 ${tag}`
      : "视频列表";
  }, [keyword, tag]);

  // 无限滚动只保留卡片网格，清理旧链接的页码和视图参数。
  useEffect(() => {
    if (!params.has("page") && !params.has("view")) return;
    setParams(normalizeListingSearchParams, { replace: true });
  }, [params, setParams]);

  // 换排序/换标签是一次全新的列表，回到顶部再开始累积。平滑滚动会被虚拟
  // 列表的行高补偿打断而停在半路，所以直接落到顶部。
  useEffect(() => {
    if (previousQueryKeyRef.current === queryKey) return;
    previousQueryKeyRef.current = queryKey;
    window.scrollTo({ top: 0, behavior: "auto" });
  }, [queryKey]);

  const handleSortChange = useCallback(
    (nextSort: SortKey) => {
      setParams(
        (current) =>
          withListingNavigation(current, { sort: nextSort, page: 1 }),
        { replace: true }
      );
    },
    [setParams]
  );

  const { loadingMore } = listing;

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
          sortDisabled={listing.initialLoading}
          onSortChange={handleSortChange}
        />

        {showSkeleton ? (
          <VideoGrid
            videos={[]}
            loading
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
              snapshotRef={gridRef}
              initialSnapshot={gridSnapshot}
              key={`${queryKey}:${listing.feedToken}`}
              eagerCount={eagerCount}
              highPriorityCount={1}
              hasMore={listing.hasMore}
              loadingMore={loadingMore}
              prefetchRows={PREFETCH_ROWS}
              tailContent={
                loadingMore ? <InfiniteFeedStatus state="loading" /> : undefined
              }
              onLoadMore={listing.loadMore}
            />

            {showTailError ? (
              <ListingLoadError hasContent onRetry={listing.retry} />
            ) : listing.exhausted ? (
              <InfiniteFeedStatus state="end" />
            ) : null}
          </>
        )}
      </div>
    </AppShell>
  );
}
