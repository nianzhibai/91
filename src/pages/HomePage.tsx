import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import { useLocation, useSearchParams } from "react-router";
import { AdminEmptyVisual } from "@/admin/AdminEmptyVisual";
import { AppShell } from "@/components/AppShell";
import { HomeFeedTabs } from "@/components/HomeFeedTabs";
import { ListingLoadError } from "@/components/ListingLoadError";
import { Pagination } from "@/components/Pagination";
import { PromoStrip } from "@/components/PromoStrip";
import { SearchPanel } from "@/components/SearchPanel";
import { SortToolbar } from "@/components/SortToolbar";
import { TagCloud } from "@/components/TagCloud";
import { VideoGrid } from "@/components/VideoGrid";
import {
  VirtualVideoGrid,
  type VirtualGridRange,
} from "@/components/VirtualVideoGrid";
import {
  homeLatestFeedSource,
  homeRecommendationFeedSource,
} from "@/lib/infiniteFeedSource";
import {
  readHomeFeed,
  readListingPage,
  readListingSort,
  readListingView,
  withHomeFeed,
  withListingNavigation,
  withListingPage,
  withListingView,
} from "@/lib/listingSearchParams";
import { MOBILE_VIDEO_PAGE_SIZE, useIsMobile } from "@/lib/responsive";
import { useInfiniteListing } from "@/lib/useInfiniteListing";
import { useListingQuery } from "@/lib/useListingQuery";
import {
  useListingRestoreTarget,
  useListingScrollRestore,
} from "@/lib/useListingScrollRestore";
import { shouldLoadMore } from "@/lib/virtualGrid";

const HOME_SEARCH_DESKTOP_PAGE_SIZE = 20;
const HOME_FEED_DESKTOP_BATCH_SIZE = 20;

// 距列表尾部还有两行时就续下一批，滚动到底之前数据已经在路上。
const PREFETCH_ROWS = 2;

const EMPTY_RANGE: VirtualGridRange = {
  startIndex: 0,
  endIndex: 0,
  columns: 1,
};

export default function HomePage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const location = useLocation();
  const activeSearchQuery = searchParams.get("q")?.trim() ?? "";
  const activeTag = searchParams.get("tag")?.trim() ?? "";
  const hasActiveSearch = activeSearchQuery.length > 0;
  const hasActiveTag = activeTag.length > 0;
  const hasActiveFilter = hasActiveSearch || hasActiveTag;
  const searchPage = readListingPage(searchParams);
  const searchSort = readListingSort(searchParams);
  const searchView = readListingView(searchParams);
  const feed = readHomeFeed(searchParams);
  const isMobile = useIsMobile();
  const eagerCount = isMobile ? 2 : 4;

  // 搜索和标签结果仍然分页：这类结果常常需要定位到具体某一页。
  const searchPageSize = isMobile
    ? MOBILE_VIDEO_PAGE_SIZE
    : HOME_SEARCH_DESKTOP_PAGE_SIZE;
  const searchResult = useListingQuery(
    {
      q: activeSearchQuery,
      tag: activeTag,
      sort: searchSort,
      page: searchPage,
      pageSize: searchPageSize,
    },
    { enabled: hasActiveFilter }
  );
  const searchSnapshot = searchResult.snapshot;
  const searchItems = searchSnapshot?.items ?? [];
  const searchHasContent = searchItems.length > 0;
  const searchShowSkeleton =
    searchResult.initialLoading ||
    (searchResult.transitioning && !searchHasContent);
  const searchShowContentError =
    searchResult.phase === "error" && searchHasContent;
  const searchShowEmptyError =
    searchResult.phase === "error" && !searchHasContent;
  const previousSearchPageSizeRef = useRef(searchPageSize);
  const searchScrollOnCommitRef = useRef(false);

  // 两个推荐 tab 都是无限滚动。随机推荐走服务端轮换 feed，最新视频走真正
  // 分页的列表接口。
  const feedBatchSize = isMobile
    ? MOBILE_VIDEO_PAGE_SIZE
    : HOME_FEED_DESKTOP_BATCH_SIZE;
  const feedSource = useMemo(
    () =>
      feed === "latest"
        ? homeLatestFeedSource(feedBatchSize)
        : homeRecommendationFeedSource(),
    [feed, feedBatchSize]
  );
  const restoreTarget = useListingRestoreTarget({
    historyKey: location.key,
    queryKey: feedSource.key,
    pageSize: feedSource.batchSize,
  });
  const homeFeed = useInfiniteListing(feedSource, {
    enabled: !hasActiveFilter,
    restoreCount: restoreTarget.count,
  });
  useListingScrollRestore({
    target: restoreTarget,
    queryKey: feedSource.key,
    requestedCount: hasActiveFilter ? 0 : homeFeed.requestedCount,
    itemCount: homeFeed.items.length,
  });

  const feedItems = homeFeed.items;
  const feedHasContent = feedItems.length > 0;
  const [range, setRange] = useState<VirtualGridRange>(EMPTY_RANGE);
  const previousFeedKeyRef = useRef(feedSource.key);

  useEffect(() => {
    document.title = activeSearchQuery
      ? `搜索 "${activeSearchQuery}"`
      : activeTag
      ? `标签 ${activeTag}`
      : "首页";
  }, [activeSearchQuery, activeTag]);

  useEffect(() => {
    if (previousSearchPageSizeRef.current === searchPageSize) return;
    previousSearchPageSizeRef.current = searchPageSize;
    if (!hasActiveFilter || searchPage === 1) return;
    setSearchParams((current) => withListingPage(current, 1), { replace: true });
  }, [hasActiveFilter, searchPage, searchPageSize, setSearchParams]);

  useEffect(() => {
    if (
      !searchScrollOnCommitRef.current ||
      searchSnapshot?.key !== searchResult.key
    ) {
      return;
    }
    searchScrollOnCommitRef.current = false;
    window.scrollTo({ top: 0, behavior: "smooth" });
  }, [searchResult.key, searchSnapshot?.key]);

  // 换 tab 是一次全新的列表，回到顶部再开始累积。平滑滚动会被虚拟列表的
  // 行高补偿打断而停在半路，所以直接落到顶部。
  useEffect(() => {
    if (previousFeedKeyRef.current === feedSource.key) return;
    previousFeedKeyRef.current = feedSource.key;
    window.scrollTo({ top: 0, behavior: "auto" });
  }, [feedSource.key]);

  const handleRangeChange = useCallback((next: VirtualGridRange) => {
    setRange((current) =>
      current.startIndex === next.startIndex &&
      current.endIndex === next.endIndex &&
      current.columns === next.columns
        ? current
        : next
    );
  }, []);

  const { loadMore, loadingMore, hasMore } = homeFeed;
  useEffect(() => {
    if (hasActiveFilter) return;
    if (
      shouldLoadMore({
        endIndex: range.endIndex,
        itemCount: feedItems.length,
        columns: range.columns,
        hasMore,
        loading: loadingMore,
        prefetchRows: PREFETCH_ROWS,
      })
    ) {
      loadMore();
    }
  }, [
    feedItems.length,
    hasActiveFilter,
    hasMore,
    loadMore,
    loadingMore,
    range,
  ]);

  const reloadFeed = homeFeed.reload;
  const refreshHome = useCallback(() => {
    window.scrollTo({ top: 0, behavior: "auto" });
    reloadFeed();
  }, [reloadFeed]);

  const displayedSearchSort =
    searchResult.phase === "error" && searchSnapshot
      ? searchSnapshot.query.sort
      : searchSort;
  const displayedSearchPage = searchSnapshot?.query.page ?? searchPage;
  const showRefresh = !hasActiveFilter && feed === "recommend";
  const refreshing = showRefresh && homeFeed.initialLoading;

  return (
    <AppShell mobileAutoHideNav>
      <div className="container page-section home-discovery-section">
        <PromoStrip />
        <SearchPanel
          navigationPath="/"
          variant="uiverse"
          placeholder=""
          className="search-panel--public search-panel--transparent"
        />
        {feedHasContent || hasActiveFilter ? (
          <TagCloud linkBasePath="/" />
        ) : (
          <div className="tag-cloud-container is-reserved" aria-hidden="true" />
        )}
      </div>

      {hasActiveFilter ? (
        <div className="container page-section home-primary-section">
          <SortToolbar
            sort={displayedSearchSort}
            view={searchView}
            sortDisabled={searchResult.initialLoading || searchResult.transitioning}
            onSortChange={(nextSort) => {
              searchScrollOnCommitRef.current = true;
              setSearchParams(
                withListingNavigation(searchParams, {
                  sort: nextSort,
                  page: 1,
                }),
                { replace: true }
              );
            }}
            onViewChange={(nextView) => {
              setSearchParams(withListingView(searchParams, nextView), {
                replace: true,
              });
            }}
          />

          {searchShowSkeleton ? (
            <VideoGrid
              videos={[]}
              loading
              compact={searchView === "compact"}
              skeletonCount={searchPageSize}
            />
          ) : searchShowEmptyError ? (
            <ListingLoadError
              hasContent={false}
              onRetry={searchResult.retry}
              emptyClassName="admin-empty-state admin-empty-state--plain home-empty-state"
            />
          ) : searchSnapshot && searchItems.length === 0 ? (
            <AdminEmptyVisual
              variant="no-results"
              text="未查询到"
              className="admin-empty-state admin-empty-state--plain home-empty-state"
            />
          ) : (
            <>
              {searchShowContentError && (
                <ListingLoadError
                  hasContent
                  displayedPage={displayedSearchPage}
                  onRetry={searchResult.retry}
                />
              )}
              <VideoGrid
                videos={searchItems}
                compact={searchView === "compact"}
                refreshMode={
                  searchResult.transitioning
                    ? "blocking"
                    : searchResult.revalidating
                    ? "background"
                    : undefined
                }
                eagerCount={eagerCount}
                highPriorityCount={1}
              />
            </>
          )}

          {searchSnapshot && (
            <Pagination
              page={displayedSearchPage}
              pageSize={searchSnapshot.query.pageSize}
              total={searchSnapshot.total}
              disabled={searchResult.transitioning}
              pendingPage={searchResult.transitioning ? searchPage : undefined}
              onChange={(nextPage) => {
                searchScrollOnCommitRef.current = true;
                setSearchParams(withListingPage(searchParams, nextPage));
              }}
            />
          )}
        </div>
      ) : (
        <div className="container page-section home-primary-section">
          <HomeFeedTabs
            feed={feed}
            onChange={(nextFeed) => {
              setSearchParams(withHomeFeed(searchParams, nextFeed), {
                replace: true,
              });
            }}
          />

          {homeFeed.initialLoading && !feedHasContent ? (
            <VideoGrid videos={[]} loading skeletonCount={feedBatchSize} />
          ) : homeFeed.failed && !feedHasContent ? (
            <ListingLoadError
              hasContent={false}
              onRetry={homeFeed.retry}
              emptyClassName="admin-empty-state admin-empty-state--plain home-empty-state"
            />
          ) : !feedHasContent ? (
            <AdminEmptyVisual
              variant="empty"
              text="当前库中没有视频"
              className="admin-empty-state admin-empty-state--plain home-empty-state"
            />
          ) : (
            <>
              <VirtualVideoGrid
                videos={feedItems}
                eagerCount={eagerCount}
                highPriorityCount={1}
                onRangeChange={handleRangeChange}
              />

              {homeFeed.failed ? (
                <ListingLoadError hasContent onRetry={homeFeed.retry} />
              ) : homeFeed.loadingMore ? (
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
              ) : homeFeed.exhausted ? (
                <div className="listing-infinite-status listing-infinite-status--end">
                  没有更多了
                </div>
              ) : null}
            </>
          )}
        </div>
      )}

      {showRefresh && (
        <button
          type="button"
          className={`home-refresh ${refreshing ? "is-refreshing" : ""}`}
          onClick={refreshHome}
          disabled={refreshing}
          aria-label="刷新首页"
          title="刷新首页"
        >
          <RefreshCw size={18} />
        </button>
      )}
    </AppShell>
  );
}
