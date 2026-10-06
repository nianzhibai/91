import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const videosPageSource = readFileSync(new URL("../src/admin/VideosPage.tsx", import.meta.url), "utf8");
const adminPaginationSource = readFileSync(
  new URL("../src/admin/AdminPagination.tsx", import.meta.url),
  "utf8"
);
const searchPanelSource = readFileSync(new URL("../src/components/SearchPanel.tsx", import.meta.url), "utf8");
const apiSource = readFileSync(new URL("../src/admin/api.ts", import.meta.url), "utf8");
const emptyVisualSource = readFileSync(new URL("../src/admin/AdminEmptyVisual.tsx", import.meta.url), "utf8");
const adminCss = readFileSync(new URL("../src/styles/admin.css", import.meta.url), "utf8");
const sharedStateCss = readFileSync(new URL("../src/styles/shared-state.css", import.meta.url), "utf8");
const filterAllIconSource = readFileSync(
  new URL("../src/components/icons/FilterAllIcon.tsx", import.meta.url),
  "utf8"
);

test("admin empty visual places the requested image above its text", () => {
  assert.match(emptyVisualSource, /import emptyImage from "@\/assets\/admin\/empty\.webp"/);
  assert.match(emptyVisualSource, /import noResultsImage from "@\/assets\/admin\/no-results\.webp"/);
  assert.match(emptyVisualSource, /variant === "no-results" \? noResultsImage : emptyImage/);
  assert.match(emptyVisualSource, /admin-empty-visual__media[\s\S]*?<img[\s\S]*?admin-empty-visual__text/);
});

test("normal videos keep responsive capacities while blacklist pages contain twenty items", () => {
  assert.match(videosPageSource, /const DESKTOP_CURRENT_VIDEOS_PAGE_SIZE = 16;/);
  assert.match(videosPageSource, /const MOBILE_CURRENT_VIDEOS_PAGE_SIZE = 10;/);
  assert.match(videosPageSource, /const DESKTOP_BLACKLIST_PAGE_SIZE = 20;/);
  assert.match(videosPageSource, /const MOBILE_BLACKLIST_PAGE_SIZE = 20;/);
  assert.match(videosPageSource, /const VIDEOS_MOBILE_QUERY = "\(max-width: 640px\)";/);
  assert.match(videosPageSource, /window\.matchMedia\(VIDEOS_MOBILE_QUERY\)/);
  assert.match(
    videosPageSource,
    /function CurrentVideosTab[\s\S]*?const pageSize = useVideosPageSize\(\s*DESKTOP_CURRENT_VIDEOS_PAGE_SIZE,\s*MOBILE_CURRENT_VIDEOS_PAGE_SIZE\s*\);/
  );
  assert.match(
    videosPageSource,
    /function BlacklistTab[\s\S]*?const pageSize = useVideosPageSize\(\s*DESKTOP_BLACKLIST_PAGE_SIZE,\s*MOBILE_BLACKLIST_PAGE_SIZE\s*\);/
  );
  assert.match(
    videosPageSource,
    /function useVideosPageSize\(desktopPageSize: number, mobilePageSize: number\)/
  );
  assert.match(
    videosPageSource,
    /const activeListQueryKey = JSON\.stringify\(\[\s*page,\s*pageSize,\s*searchKeyword,\s*sourceDriveId,\s*sourceCrawlerId,\s*sourceKind,\s*appliedFilters,?\s*\]\);/
  );
  assert.match(videosPageSource, /api\.listVideos\(\{[\s\S]*?page,[\s\S]*?size: pageSize,[\s\S]*?keyword: searchKeyword,[\s\S]*?driveId: sourceDriveId,[\s\S]*?crawlerId: sourceCrawlerId,[\s\S]*?\.\.\.appliedFilters/);
});

test("normal videos keep source navigation separate from composable advanced filters", () => {
  const clearFiltersSource = videosPageSource.slice(
    videosPageSource.indexOf("function clearAdvancedFilters"),
    videosPageSource.indexOf("\n\n  return (", videosPageSource.indexOf("function clearAdvancedFilters"))
  );
  const sourceNavigationSource = videosPageSource.slice(
    videosPageSource.indexOf("function VideoSourceNavigation"),
    videosPageSource.indexOf("// ---------- 当前视频")
  );
  const advancedFilterSource = videosPageSource.slice(
    videosPageSource.indexOf("function AdvancedVideoFilters"),
    videosPageSource.indexOf("function SearchBox")
  );
  assert.match(videosPageSource, /className="admin-video-advanced-filters"/);
  assert.match(videosPageSource, /aria-haspopup="dialog"/);
  assert.match(videosPageSource, /<Modal[\s\S]*?open=\{advancedFiltersOpen\}[\s\S]*?title="筛选"/);
  assert.doesNotMatch(advancedFilterSource, /来源|driveId|crawlerId|VideoSourcePicker/);
  assert.doesNotMatch(videosPageSource, /function VideoSourcePicker|admin-video-source-picker/);
  assert.match(sourceNavigationSource, /role="group" aria-label="视频来源筛选"/);
  assert.match(sourceNavigationSource, /\{ key: "all", label: "全部", all: true \}/);
  assert.match(videosPageSource, /const sourceCatalogLoaded = sources\.some\(\(resource\) => resource\.ready\)/);
  assert.match(videosPageSource, /const hasLocalUploads = \(localResource\.data\?\.total \?\? 0\) > 0;/);
  assert.match(videosPageSource, /hasLocalUploads=\{hasLocalUploads\}/);
  assert.match(videosPageSource, /sourceCatalogLoaded=\{sourceCatalogLoaded\}/);
  assert.match(sourceNavigationSource, /if \(sourceCatalogLoaded\) \{[\s\S]*?sourceItems\.push\(/);
  assert.match(sourceNavigationSource, /\.\.\.\(hasLocalUploads[\s\S]*?label: "本地存储",[\s\S]*?upload: true,[\s\S]*?: \[\]\)/);
  assert.ok(
    sourceNavigationSource.indexOf("...drives") <
      sourceNavigationSource.indexOf('label: "本地存储"')
  );
  assert.ok(
    sourceNavigationSource.indexOf('label: "本地存储"') <
      sourceNavigationSource.indexOf("...crawlers")
  );
  assert.match(videosPageSource, /import \{ LocalStorageIcon \} from "@\/components\/icons\/LocalStorageIcon";/);
  assert.match(videosPageSource, /import \{ FilterAllIcon \} from "@\/components\/icons\/FilterAllIcon";/);
  assert.match(sourceNavigationSource, /<FilterAllIcon size=\{15\} className="admin-video-source-tab__glyph is-all" \/>/);
  assert.match(sourceNavigationSource, /<LocalStorageIcon size=\{15\} className="admin-video-source-tab__glyph is-upload" \/>/);
  assert.match(filterAllIconSource, /<rect x="3\.5" y="3\.5" width="5" height="5" rx="1\.4" \/>/);
  assert.match(filterAllIconSource, /<circle cx="12" cy="12" r="1\.6" fill="currentColor" stroke="none" \/>/);
  assert.match(sourceNavigationSource, /`drive:\$\{drive\.id\}`/);
  assert.match(sourceNavigationSource, /`crawler:\$\{crawler\.id\}`/);
  assert.match(sourceNavigationSource, /driveKindIconPath\(drive\.kind\)/);
  assert.match(videosPageSource, /import \{ SpiderIcon \} from "\.\/icons\/SpiderIcon";/);
  assert.match(sourceNavigationSource, /<SpiderIcon size=\{16\} className="admin-video-source-tab__glyph is-crawler" \/>/);
  assert.doesNotMatch(sourceNavigationSource, /admin-video-source-tab__icon is-(?:all|upload|crawler)/);
  assert.doesNotMatch(videosPageSource, /function SpiderIcon\(\)/);
  assert.match(videosPageSource, /const LOCAL_UPLOAD_SOURCE_ID = "local-upload";/);
  assert.match(adminCss, /\.admin-video-advanced-range__inputs\s*\{[^}]*grid-template-columns:\s*max-content auto max-content;[^}]*margin-top:\s*var\(--space-2\);/s);
  assert.match(adminCss, /input\[type="date"\]\s*\{[^}]*width:\s*136px;/s);
  assert.match(adminCss, /input\[type="number"\]\s*\{[^}]*width:\s*104px;/s);
  assert.doesNotMatch(adminCss, /admin-video-source-picker|admin-video-advanced-field--source/);
  assert.match(videosPageSource, /<legend>入库时间<\/legend>/);
  assert.match(videosPageSource, /<legend>视频时长\(分钟\)<\/legend>/);
  assert.doesNotMatch(videosPageSource, /admin-video-upload-source-options|uploadSource/);
  assert.equal(Array.from(advancedFilterSource.matchAll(/admin-video-advanced-range__placeholder/g)).length, 2);
  assert.equal(Array.from(advancedFilterSource.matchAll(/年\/月\/日/g)).length, 2);
  assert.match(videosPageSource, />\s*应用\s*<\/button>/);
  assert.doesNotMatch(videosPageSource, /应用筛选/);
  assert.doesNotMatch(advancedFilterSource, /<span>(开始|结束|最短|最长)<\/span>/);
  assert.doesNotMatch(advancedFilterSource, /入库日期包含开始和结束当天|视频时长按分钟计算/);
  assert.doesNotMatch(adminCss, /\.admin-video-advanced-filters__hint/);
  for (const label of ["入库开始日期", "入库截止日期", "视频最短时长（分钟）", "视频最长时长（分钟）"]) {
    assert.match(advancedFilterSource, new RegExp(`aria-label="${label}"`));
  }
  assert.match(videosPageSource, /type="number"[\s\S]*?value=\{value\.durationMinMinutes\}/);
  assert.match(videosPageSource, /type="number"[\s\S]*?value=\{value\.durationMaxMinutes\}/);
  assert.equal(Array.from(videosPageSource.matchAll(/type="date"/g)).length, 2);
  assert.match(
    advancedFilterSource,
    /value=\{value\.createdFrom\}[\s\S]*?max=\{earlierDateInputValue\(value\.createdTo, today\)\}/
  );
  assert.match(advancedFilterSource, /value=\{value\.createdTo\}[\s\S]*?max=\{today\}/);
  assert.match(videosPageSource, /function localDateInputValue\(date: Date\): string/);
  assert.match(videosPageSource, /show\("入库时间不能超过当天", "error"\)/);
  assert.equal(
    Array.from(
      videosPageSource.matchAll(/onClick=\{\(event\) => openNativeDatePicker\(event\.currentTarget\)\}/g)
    ).length,
    2
  );
  assert.match(videosPageSource, /function openNativeDatePicker\(input: HTMLInputElement\)[\s\S]*?input\.showPicker\(\)/);
  assert.doesNotMatch(videosPageSource, /视频时间|publishedFrom|publishedTo/);
  assert.match(videosPageSource, /useAdminResource\(api\.listDrives/);
  assert.match(videosPageSource, /useAdminResource\(api\.listCrawlers/);
  assert.match(videosPageSource, /api\.listVideos\(\{ driveId: LOCAL_UPLOAD_SOURCE_ID, page: 1, size: 1 \}, signal\)/);
  assert.match(videosPageSource, /useAdminResource\(api\.listTags/);
  assert.match(videosPageSource, /setAppliedFilters\(\{ \.\.\.draftFilters \}\)/);
  assert.match(clearFiltersSource, /setDraftFilters\(\{ \.\.\.EMPTY_VIDEO_FILTERS \}\)/);
  assert.doesNotMatch(clearFiltersSource, /setAppliedFilters|setPage|setAdvancedFiltersOpen/);
  assert.match(videosPageSource, /const activeAdvancedFilterCount = countVideoAdvancedFilters\(appliedFilters\);/);
  const advancedCountSource = videosPageSource.slice(
    videosPageSource.indexOf("function countVideoAdvancedFilters"),
    videosPageSource.indexOf("function dateRangeIsReversed")
  );
  assert.doesNotMatch(advancedCountSource, /driveId|crawlerId/);
  for (const key of ["driveId", "crawlerId", "createdFrom", "createdTo", "durationMinMinutes", "durationMaxMinutes"]) {
    assert.match(apiSource, new RegExp(`if \\(params\\.${key}\\) qs\\.set\\("${key}", params\\.${key}\\)`));
  }
  assert.doesNotMatch(apiSource, /publishedFrom|publishedTo/);
  assert.match(adminCss, /\.admin-modal\.admin-modal--video-filters\s*\{[^}]*width\s*:\s*min\(700px,\s*100%\)/s);
  assert.match(adminCss, /\.admin-video-advanced-filters__grid\s*\{[^}]*grid-template-columns\s*:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\)/s);
  assert.match(adminCss, /\.admin-modal--video-filters \.admin-video-advanced-range__inputs\s*\{[^}]*width:\s*min\(100%,\s*294px\);[^}]*margin-inline:\s*auto/s);
  assert.match(adminCss, /@media \(max-width: 520px\)[\s\S]*?\.admin-video-advanced-range__inputs\s*\{[^}]*margin-top:\s*8px/s);
  assert.match(adminCss, /@media \(max-width: 520px\)[\s\S]*?\.admin-video-advanced-range__inputs\.is-date-range\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1fr\) auto minmax\(0,\s*1fr\)/s);
  assert.match(adminCss, /@media \(max-width: 520px\)[\s\S]*?\.admin-video-advanced-range__inputs\.is-duration-range\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1fr\) auto minmax\(0,\s*1fr\)/s);
  assert.match(adminCss, /@media \(max-width: 520px\)[\s\S]*?\.admin-video-advanced-range input\[type="date"\],[\s\S]*?height:\s*40px;[^}]*border:\s*0;/s);
  assert.match(adminCss, /\.admin-modal--video-filters \.admin-modal__footer\s*\{[^}]*display:\s*grid;[^}]*grid-template-columns:\s*auto repeat\(2,\s*minmax\(0,\s*1fr\)\)/s);
  assert.match(videosPageSource, /className="admin-btn admin-video-advanced-clear"/);
});

test("admin video searches reuse the debounced home search component", () => {
  const searchBoxSource = videosPageSource.slice(
    videosPageSource.indexOf("function SearchBox"),
    videosPageSource.indexOf("function ErrorState")
  );

  assert.match(videosPageSource, /import \{ SearchPanel \} from "@\/components\/SearchPanel";/);
  assert.match(searchBoxSource, /<SearchPanel[\s\S]*?className="admin-videos-filter__search search-panel--transparent"[\s\S]*?value=\{keyword\}[\s\S]*?onSearch=\{onSearch\}[\s\S]*?variant="uiverse"/);
  assert.match(searchBoxSource, /placeholder=""/);
  assert.doesNotMatch(videosPageSource, /搜索标题 \/ 作者|搜索文件名/);
  assert.equal(
    Array.from(videosPageSource.matchAll(/<SearchBox keyword=\{searchKeyword\} onSearch=\{handleSearch\}/g)).length,
    2
  );
  assert.match(searchPanelSource, /const SEARCH_DEBOUNCE_MS = 500;/);
  assert.match(searchPanelSource, /window\.setTimeout\(\(\) => \{\s*commitSearch\(keyword\);/);
  assert.doesNotMatch(videosPageSource, /ADMIN_SEARCH_DEBOUNCE_MS/);
});

test("admin video pagination follows the compact CPA previous and next layout", () => {
  const paginationCalls = Array.from(
    videosPageSource.matchAll(
      /<AdminPagination\s+page=\{displayedPage \?\? page\}[\s\S]{0,240}?itemLabel="视频"[\s\S]{0,120}?pending=\{listQueryPending\}[\s\S]{0,120}?onPage=\{setPage\}\s*\/>/g
    )
  );
  assert.match(adminPaginationSource, /第 \{page\} \/ \{totalPages\} 页 · 共 \{total\} 个\{itemLabel\}/);
  assert.match(adminPaginationSource, /admin-table-pagination admin-list-pagination/);
  assert.equal(Array.from(adminPaginationSource.matchAll(/上一页|下一页/g)).length, 2);
  assert.doesNotMatch(adminPaginationSource, /首页|末页/);
  assert.match(adminPaginationSource, /disabled=\{pending \|\| page <= 1\}/);
  assert.match(adminPaginationSource, /disabled=\{pending \|\| page >= totalPages\}/);
  assert.doesNotMatch(videosPageSource, /每页 \{pageSize\} 个/);
  assert.doesNotMatch(videosPageSource, /<AdminPagination[^>]*pageSize=\{pageSize\}/);
  assert.equal(paginationCalls.length, 2);
  assert.match(adminCss, /\.admin-table-pagination\.admin-list-pagination\s*\{[^}]*gap:\s*14px;[^}]*margin-top:\s*0;[^}]*margin-bottom:\s*var\(--space-3\);/s);
  assert.match(adminCss, /\.admin-list-pagination__button\s*\{[^}]*padding:\s*8px 10px;[^}]*box-shadow:\s*none;/s);
  assert.match(adminCss, /\.admin-list-pagination__info\s*\{[^}]*font-family:\s*ui-monospace[^}]*font-size:\s*12px;[^}]*font-variant-numeric:\s*tabular-nums;[^}]*letter-spacing:\s*0\.02em;[^}]*white-space:\s*nowrap;/s);
});

test("admin video pagination stores the active page in the URL", () => {
  assert.match(videosPageSource, /const page = readAdminVideosPage\(searchParams\);/);
  assert.match(videosPageSource, /return withAdminVideosPage\(prev, resolvedPage\);/);
  assert.match(videosPageSource, /function selectSource[\s\S]*?next\.delete\("tab"\);[\s\S]*?next\.delete\("page"\);/);
  assert.match(videosPageSource, /function openBlacklist[\s\S]*?next\.set\("tab", "blacklist"\);[\s\S]*?if \(activeView !== "blacklist"\) next\.delete\("page"\);/);
  assert.doesNotMatch(videosPageSource, /const \[page, setPage\] = useState\(1\);/);
});

test("blacklist navigation switches immediately while its initial list stays blank", () => {
  const openBlacklistSource = videosPageSource.slice(
    videosPageSource.indexOf("function openBlacklist"),
    videosPageSource.indexOf("\n\n  return (", videosPageSource.indexOf("function openBlacklist"))
  );
  const blacklistSource = videosPageSource.slice(
    videosPageSource.indexOf("function BlacklistTab"),
    videosPageSource.indexOf("// ---------- 共享小组件 ----------")
  );

  assert.match(openBlacklistSource, /setSearchParams\([\s\S]*?next\.set\("tab", "blacklist"\);/);
  assert.doesNotMatch(openBlacklistSource, /await|api\.listBlacklist|blacklistOpening/);
  assert.match(videosPageSource, /onClick=\{onOpenBlacklist\}/);
  assert.match(videosPageSource, /activeView === "blacklist"[\s\S]*?<BlacklistTab/);
  assert.match(blacklistSource, /api\.listBlacklist\(\{ page, size: pageSize, keyword: searchKeyword \}, signal\)/);
  assert.match(blacklistSource, /\{showInitialLoading \? null : loadError \? \(/);
  assert.doesNotMatch(blacklistSource, /AdminLoading|LoadingState/);
});

test("admin video pagination clamps stale deep links after totals load", () => {
  const currentSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频 ----------")
  );
  const blacklistSource = videosPageSource.slice(videosPageSource.indexOf("function BlacklistTab"));
  // Both tabs must wait for the active query, not just any previous request,
  // to settle before clamping a page restored from the URL.
  const clamp = /listQuerySettled\s*&&\s*page\s*>\s*totalPages[\s\S]{0,40}setPage\(totalPages\)/;
  assert.match(currentSource, clamp);
  assert.match(blacklistSource, clamp);
  assert.equal(
    Array.from(videosPageSource.matchAll(/const listQuerySettled = (?:videos|blacklist)\.ready && !(?:videos|blacklist)\.refreshing && !(?:videos|blacklist)\.failure;/g)).length,
    2
  );
});

test("video pagination does not create invisible rows on a short final page", () => {
  const currentSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频 ----------")
  );
  const blacklistSource = videosPageSource.slice(videosPageSource.indexOf("function BlacklistTab"));
  assert.equal(Array.from(videosPageSource.matchAll(/const showPagination = totalPages > 1;/g)).length, 2);
  assert.doesNotMatch(currentSource, /placeholderRows|admin-video-placeholder-row/);
  assert.doesNotMatch(blacklistSource, /placeholderRows|admin-video-placeholder-row/);
  assert.match(blacklistSource, /admin-table admin-table--static-rows admin-blacklist-table admin-videos-results__content/);
  assert.doesNotMatch(adminCss, /\.admin-video-placeholder-row/);
});

test("empty video tabs use the correct visual and distinguish search misses", () => {
  const currentSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频 ----------")
  );
  const blacklistSource = videosPageSource.slice(videosPageSource.indexOf("function BlacklistTab"));
  assert.match(
    currentSource,
    /const hasActiveSearch =\s*searchKeyword\.trim\(\)\.length > 0 \|\|\s*!!sourceDriveId \|\|\s*!!sourceCrawlerId \|\|\s*!!sourceKind \|\|\s*activeAdvancedFilterCount > 0;/
  );
  assert.match(blacklistSource, /const hasActiveSearch = searchKeyword\.trim\(\)\.length > 0;/);
  assert.doesNotMatch(currentSource, /hasVideoActions|批量选择/);
  assert.match(blacklistSource, /const hasBlacklistActions = list\.length > 0;/);
  assert.match(currentSource, /\{selectedIds\.size > 0 && \(\s*<div className="admin-videos-list-toolbar"/);
  assert.match(
    blacklistSource,
    /\{hasBlacklistActions && \(\s*<div[\s\S]*?className="admin-videos-filter__actions admin-blacklist-source-delete"[\s\S]*?删除全部/
  );
  assert.match(blacklistSource, /\{selectedIds\.size > 0 && \(\s*<div[\s\S]*?className="admin-videos-list-toolbar"/);
  assert.doesNotMatch(blacklistSource, /selectMode|批量选择|退出选择/);
  assert.match(currentSource, /admin-empty-state admin-empty-state--plain/);
  assert.match(blacklistSource, /admin-empty-state admin-empty-state--plain/);
  assert.match(currentSource, /variant=\{hasActiveSearch \? "no-results" : "empty"\}/);
  assert.match(blacklistSource, /variant=\{hasActiveSearch \? "no-results" : "empty"\}/);
  assert.match(currentSource, /hasActiveSearch \? "未查询到" : "当前库中没有视频"/);
  assert.match(blacklistSource, /hasActiveSearch \? "未查询到" : "暂无拉黑视频"/);
  assert.match(blacklistSource, /暂无拉黑视频/);
  assert.doesNotMatch(currentSource, /还没有视频。先在「网盘管理」里配置好盘并触发扫描，或调整搜索词。/);
  assert.doesNotMatch(blacklistSource, /黑名单为空/);
  assert.doesNotMatch(currentSource, /<Image size=\{48\}/);
  assert.doesNotMatch(blacklistSource, /<Ban size=\{48\}/);
  assert.match(
    sharedStateCss,
    /\.admin-empty-state--plain\s*\{[^}]*border\s*:\s*0;[^}]*background\s*:\s*transparent/s
  );
  assert.match(
    adminCss,
    /\.admin-videos-current,[\s\S]*?\.admin-videos-blacklist\s*\{[^}]*display\s*:\s*flex;[^}]*flex\s*:\s*1 1 auto;[^}]*flex-direction\s*:\s*column;[^}]*min-height\s*:\s*0/s
  );
  assert.match(videosPageSource, /className="admin-page admin-page--with-floating-actions admin-videos-page"/);
  assert.match(
    adminCss,
    /\.admin-videos-current > \.admin-empty-state--plain,[\s\S]*?\.admin-videos-blacklist > \.admin-empty-state--plain\s*\{[^}]*box-sizing\s*:\s*border-box;[^}]*flex\s*:\s*1 1 auto;[^}]*min-height\s*:\s*0;[^}]*padding\s*:\s*0 16px 96px/s
  );
  assert.doesNotMatch(adminCss, /translateY\(-48px\)/);
});

test("current videos keep their skeleton while blacklist initial loading stays invisible", () => {
  const currentSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频 ----------")
  );
  const blacklistSource = videosPageSource.slice(
    videosPageSource.indexOf("function BlacklistTab"),
    videosPageSource.indexOf("// ---------- 共享小组件 ----------")
  );
  assert.match(currentSource, /showInitialLoading \? \(\s*<VideoCardGridLoadingState \/>/);
  assert.match(blacklistSource, /\{showInitialLoading \? null : loadError \? \(/);
  assert.equal(
    Array.from(videosPageSource.matchAll(/const showInitialLoading = \(displayedPage === null \|\| (?:listItems|list)\.length === 0\) && !loadError && listQueryPending;/g)).length,
    2
  );
  assert.match(videosPageSource, /const CURRENT_VIDEO_SKELETON_CARD_COUNT = 6;/);
  assert.match(
    videosPageSource,
    /function VideoCardGridLoadingState\(\)[\s\S]*?className="admin-video-card-grid admin-video-card-grid--skeleton"[\s\S]*?role="status"[\s\S]*?aria-busy="true"[\s\S]*?Array\.from\(\{ length: CURRENT_VIDEO_SKELETON_CARD_COUNT \}[\s\S]*?className="admin-video-card-skeleton admin-card-skeleton-surface"/
  );
  assert.match(
    adminCss,
    /\.admin-video-card-skeleton\s*\{[^}]*height:\s*206px;[^}]*border-radius:\s*14px;/s
  );
  assert.match(
    adminCss,
    /\.admin-card-skeleton-surface\s*\{[^}]*linear-gradient\([^}]*animation:\s*admin-card-skeleton-shimmer 1\.5s ease-in-out infinite;/s
  );
  assert.doesNotMatch(blacklistSource, /LoadingState|AdminLoading/);
});

test("video pagination keeps settled results visible while the next page loads", () => {
  assert.equal(
    Array.from(videosPageSource.matchAll(/className=\{`admin-videos-results\$\{listQueryPending \? " is-page-loading" : ""\}`\}/g)).length,
    2
  );
  assert.equal(
    Array.from(videosPageSource.matchAll(/aria-busy=\{listQueryPending \|\| undefined\}/g)).length,
    2
  );
  assert.equal(
    Array.from(videosPageSource.matchAll(/const displayedPage = visibleResult\?\.page \?\? null;/g)).length,
    2
  );
  assert.equal(Array.from(videosPageSource.matchAll(/inert: ""/g)).length, 2);
  assert.equal(Array.from(videosPageSource.matchAll(/queryKey: activeListQueryKey/g)).length, 2);
  assert.match(adminCss, /\.admin-videos-results\s*\{[^}]*display:\s*flex;[^}]*flex-direction:\s*column;[^}]*gap:\s*14px;/s);
  assert.match(adminCss, /\.admin-videos-results\.is-page-loading \.admin-videos-results__content\s*\{[^}]*opacity:\s*0\.62;[^}]*pointer-events:\s*none;/s);
});

test("admin videos batch delete runs deletions sequentially", () => {
  assert.match(videosPageSource, /for \(const id of ids\) \{/);
  assert.match(videosPageSource, /const result = await api\.deleteVideo\(id, \{ deleteSource: batchDeleteSource \}\);/);
  assert.doesNotMatch(
    videosPageSource,
    /Promise\.allSettled\(\s*ids\.map\(\(id\) => api\.deleteVideo\(id(?:, [^)]+)?\)\)\s*\)/
  );
});

test("admin video selections persist across pages and can include the current page", () => {
  const currentSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频 ----------")
  );
  const refreshSource = currentSource.slice(
    currentSource.indexOf("async function refresh()"),
    currentSource.indexOf("async function refreshListOnly()")
  );

  assert.doesNotMatch(refreshSource, /setSelectedIds\(new Set\(\)\)/);
  assert.match(currentSource, /const selectPageVideos = \(\) => \{[\s\S]*?new Set\(current\)[\s\S]*?listItems\.forEach\(\(video\) => next\.add\(video\.id\)\)/);
  assert.match(currentSource, /listItems\.every\(\(video\) => selectedIds\.has\(video\.id\)\)/);
  assert.match(currentSource, /message=\{`确定要删除已选中的 \$\{selectedIds\.size\} 个视频吗？`\}/);
  assert.match(currentSource, /listItems\.every\(\(video\) => deletedIds\.has\(video\.id\)\)/);
  assert.doesNotMatch(currentSource, /success >= listItems\.length/);
});

test("admin videos track preview regeneration after it is accepted", () => {
  assert.match(videosPageSource, /const REGEN_PREVIEW_STATUS = "generating";/);
  assert.match(videosPageSource, /const \[regenPreviewById, setRegenPreviewById\]/);
  assert.match(videosPageSource, /trackRegeneratingPreview\(\[v\]\)/);
  assert.doesNotMatch(videosPageSource, /data-label="预览视频"[\s\S]*?<PreviewStatus/);
  assert.match(videosPageSource, /onRegenPreview=\{\(\) => handleRegen\(editingVideo\)\}/);
  assert.match(videosPageSource, /className="admin-btn admin-video-preview-button"/);
  assert.match(videosPageSource, /void videos\.invalidate\(\)/);
});

test("admin videos keep generating status after page refresh", () => {
  assert.match(videosPageSource, /data\?\.items\?\.some\(\(video\) => video\.previewStatus === REGEN_PREVIEW_STATUS\)/);
  assert.match(
    videosPageSource,
    /intervalMs: \(data\) => trackedRegenCount > 0 \|\| data\?\.items\?\.some[\s\S]*?\? REGEN_PREVIEW_POLL_INTERVAL_MS : null/
  );
  assert.match(videosPageSource, /function isPreviewGenerating\(v: api\.AdminVideo\)/);
  assert.match(videosPageSource, /return !!regenPreviewById\[v\.id\] \|\| v\.previewStatus === REGEN_PREVIEW_STATUS;/);
  assert.match(videosPageSource, /previewGenerating=\{isPreviewGenerating\(editingVideo\)\}/);
  assert.match(videosPageSource, /disabled=\{saving \|\| previewBusy\}/);
});
