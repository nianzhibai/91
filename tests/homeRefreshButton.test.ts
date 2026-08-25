import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const homePageSource = readFileSync(
  new URL("../src/pages/HomePage.tsx", import.meta.url),
  "utf8"
);
const tagCloudSource = readFileSync(
  new URL("../src/components/TagCloud.tsx", import.meta.url),
  "utf8"
);
const searchPanelSource = readFileSync(
  new URL("../src/components/SearchPanel.tsx", import.meta.url),
  "utf8"
);
const layoutCss = readFileSync(
  new URL("../src/styles/layout.css", import.meta.url),
  "utf8"
);
const searchCss = readFileSync(
  new URL("../src/styles/search.css", import.meta.url),
  "utf8"
);
const appShellSource = readFileSync(
  new URL("../src/components/AppShell.tsx", import.meta.url),
  "utf8"
);
const backToTopSource = readFileSync(
  new URL("../src/components/BackToTop.tsx", import.meta.url),
  "utf8"
);

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

test("home page refresh button shares back-to-top slot until back-to-top is visible", () => {
  assert.match(homePageSource, /import \{ RefreshCw \} from "lucide-react"/);
  // 刷新只对随机推荐有意义：最新视频是时间序，重抽一轮没有区别。
  assert.match(
    homePageSource,
    /const showRefresh = !hasActiveFilter && feed === "recommend";/
  );
  assert.match(homePageSource, /const refreshing = showRefresh && homeFeed\.initialLoading;/);
  assert.match(homePageSource, /\{showRefresh && \(/);
  assert.match(
    homePageSource,
    /const refreshHome = useCallback\(\(\) => \{\s*window\.scrollTo\(\{ top: 0, behavior: "auto" \}\);\s*reloadFeed\(\);/
  );
  assert.match(homePageSource, /const HOME_SEARCH_DESKTOP_PAGE_SIZE = 20;/);
  assert.match(homePageSource, /const HOME_FEED_DESKTOP_BATCH_SIZE = 20;/);
  assert.match(
    homePageSource,
    /const feedBatchSize = isMobile\s*\? MOBILE_VIDEO_PAGE_SIZE\s*:\s*HOME_FEED_DESKTOP_BATCH_SIZE;/
  );
  // 推荐流的取数和缓存都收敛到共享的无限滚动 hook 里。
  assert.doesNotMatch(homePageSource, /cachedRanking|cachedLatest/);
  assert.doesNotMatch(homePageSource, /fetchHomeVideos|fetchLatestHomeVideos/);
  assert.doesNotMatch(homePageSource, /LATEST_POOL_SIZE|HOME_LATEST_CURSOR_KEY/);
  assert.doesNotMatch(homePageSource, /fetchListing\(1,\s*96/);
  assert.doesNotMatch(homePageSource, /HOME_RECENT_KEY/);
  assert.doesNotMatch(homePageSource, /loadRecentHomeVideoIds/);
  assert.doesNotMatch(homePageSource, /rememberHomeVideos/);
  assert.match(homePageSource, /className=\{`home-refresh \$\{refreshing \? "is-refreshing" : ""\}`\}/);
  assert.match(homePageSource, /aria-label="刷新首页"/);
  assert.match(homePageSource, /<RefreshCw size=\{18\} \/>/);

  const refresh = ruleBody(layoutCss, ".home-refresh");
  const shiftedRefresh = ruleBody(layoutCss, ".app-shell.is-back-to-top-visible .home-refresh");
  const backToTop = ruleBody(layoutCss, ".back-to-top");
  assert.match(refresh, /position\s*:\s*fixed/);
  assert.match(refresh, /bottom\s*:\s*24px/);
  assert.match(backToTop, /bottom\s*:\s*24px/);
  assert.match(shiftedRefresh, /bottom\s*:\s*80px/);
  assert.match(refresh, /z-index\s*:\s*var\(--z-overlay\)/);
  assert.doesNotMatch(layoutCss, /\.home-refresh\.is-visible/);

  assert.match(appShellSource, /const \[backToTopVisible,\s*setBackToTopVisible\] = useState\(false\)/);
  assert.match(appShellSource, /backToTopVisible \? "is-back-to-top-visible" : ""/);
  assert.match(appShellSource, /<BackToTop onVisibilityChange=\{setBackToTopVisible\} \/>/);
  assert.match(backToTopSource, /onVisibilityChange\?: \(visible: boolean\) => void/);
  assert.match(backToTopSource, /onVisibilityChange\?\.\(nextVisible\)/);
});

test("home page keeps its recommendation state in the shared feed session", () => {
  // 返回首页时的续播、去重、中断过期请求都由 useInfiniteListing 负责，
  // 页面自己不再维护模块级缓存和请求版本号。
  assert.match(homePageSource, /const homeFeed = useInfiniteListing\(feedSource, \{/);
  assert.match(homePageSource, /enabled: !hasActiveFilter,/);
  assert.match(homePageSource, /restoreCount: restoreTarget\.count,/);
  assert.doesNotMatch(homePageSource, /rankingRequestVersion|latestRequestVersion|homeRequestVersion/);
  assert.doesNotMatch(homePageSource, /loadRanking|loadLatest|refreshLatest/);
  assert.doesNotMatch(homePageSource, /HOME_CACHE_TTL_MS|cacheIsFresh|cachedRankingAt|cachedLatestAt/);
  assert.doesNotMatch(homePageSource, /localStorage/);
});

test("home page reserves tag cloud space while tags load and uses one empty library state", () => {
  assert.match(tagCloudSource, /const visibleTags = useMemo/);
  assert.match(tagCloudSource, /typeof tag\.count !== "number" \|\| tag\.count > 0/);
  assert.match(tagCloudSource, /const initialTagsRef = useRef<TagItem\[\] \| null>\(readCachedTags\(\)\)/);
  assert.match(tagCloudSource, /const \[tags,\s*setTags\] = useState<TagItem\[\]>\(initialTagsRef\.current \?\? \[\]\)/);
  assert.match(tagCloudSource, /const \[loaded,\s*setLoaded\] = useState\(initialTagsRef\.current !== null\)/);
  assert.match(tagCloudSource, /if \(initialTagsRef\.current !== null\) return/);
  assert.match(tagCloudSource, /setLoaded\(true\)/);
  assert.match(tagCloudSource, /if \(loaded && visibleTags\.length === 0\) return null/);
  assert.match(tagCloudSource, /const loading = !loaded && visibleTags\.length === 0/);
  assert.match(tagCloudSource, /const TAG_PLACEHOLDER_COUNT = 16;/);
  assert.match(tagCloudSource, /type TagCloudProps = \{/);
  assert.match(tagCloudSource, /linkBasePath\?: string;/);
  assert.match(tagCloudSource, /onTagSelect\?: \(\) => void;/);
  assert.match(tagCloudSource, /export const TagCloud = memo\(function TagCloud\(\{[\s\S]*?linkBasePath = "\/list",[\s\S]*?onTagSelect,[\s\S]*?\}: TagCloudProps\)/);
  assert.match(tagCloudSource, /to=\{buildTagHref\(tag\.label\)\}/);
  assert.match(tagCloudSource, /onClick=\{onTagSelect\}/);
  assert.match(tagCloudSource, /const \[hasMoreRight, setHasMoreRight\] = useState\(false\)/);
  assert.match(tagCloudSource, /const remaining = slider\.scrollWidth - slider\.clientWidth - slider\.scrollLeft/);
  assert.match(tagCloudSource, /const nextHasMoreRight = remaining > 1/);
  assert.match(tagCloudSource, /slider\.addEventListener\("scroll", updateScrollOverflow, \{ passive: true \}\)/);
  assert.match(tagCloudSource, /new ResizeObserver\(updateScrollOverflow\)/);
  assert.match(tagCloudSource, /className=\{`tag-cloud-container\$\{loading \? " is-loading" : ""\}\$\{hasMoreRight \? " has-more-right" : ""\}`\}/);
  assert.match(tagCloudSource, /aria-busy=\{loading \? "true" : undefined\}/);
  assert.match(tagCloudSource, /Array\.from\(\{ length: TAG_PLACEHOLDER_COUNT \}/);
  assert.match(tagCloudSource, /tag-chip--placeholder/);
  assert.doesNotMatch(tagCloudSource, /setTimeout/);
  assert.match(tagCloudSource, /visibleTags\.map\(renderTag\)/);
  assert.doesNotMatch(tagCloudSource, /const row[12] = visibleTags\.filter/);
  assert.doesNotMatch(tagCloudSource, /\(\{tag\.count\}\)/);
  assert.doesNotMatch(tagCloudSource, /`\$\{tag\.count\} 个视频`/);

  const tagCloudContainer = ruleBody(searchCss, ".tag-cloud-container");
  const overflowingTagCloud = ruleBody(searchCss, ".tag-cloud-container.has-more-right");
  const loadingTagCloud = ruleBody(searchCss, ".tag-cloud-container.is-loading");
  const reservedTagCloud = ruleBody(searchCss, ".tag-cloud-container.is-reserved");
  const tagCloudRow = ruleBody(searchCss, ".tag-cloud__row");
  const tagChip = ruleBody(searchCss, ".tag-chip");
  const tagPlaceholder = ruleBody(searchCss, ".tag-chip--placeholder");
  assert.match(tagCloudContainer, /min-height\s*:\s*34px/);
  assert.match(tagCloudContainer, /mask-image\s*:\s*none/);
  assert.match(overflowingTagCloud, /mask-image\s*:\s*linear-gradient\(to right, black 0%, black 93%, transparent 100%\)/);
  assert.match(loadingTagCloud, /pointer-events\s*:\s*none/);
  assert.match(reservedTagCloud, /mask-image\s*:\s*none/);
  assert.match(tagCloudRow, /flex-wrap\s*:\s*nowrap/);
  assert.match(tagChip, /flex\s*:\s*0 0 auto/);
  assert.match(tagPlaceholder, /width\s*:\s*68px/);
  assert.match(tagPlaceholder, /animation\s*:\s*tag-chip-placeholder/);
  assert.match(searchCss, /\.tag-chip--placeholder:nth-child\(6n \+ 1\)/);
  assert.match(searchCss, /\.tag-chip--placeholder:nth-child\(6n\)/);

  const searchForm = ruleBody(searchCss, ".search-panel__form");
  const searchInput = ruleBody(searchCss, ".search-panel__input");
  const searchSubmit = ruleBody(searchCss, ".search-panel__submit");
  assert.match(searchPanelSource, /placeholder = "搜索视频标题或作者"/);
  assert.match(searchPanelSource, /placeholder=\{placeholder\}/);
  assert.doesNotMatch(searchPanelSource, /搜索视频标题或作者\.\.\./);
  assert.match(searchPanelSource, /const SEARCH_DEBOUNCE_MS = 500;/);
  assert.match(searchPanelSource, /window\.setTimeout\(\(\) => \{\s*commitSearch\(keyword\);/);
  assert.match(searchPanelSource, /onSearch\(q\);/);
  assert.match(searchPanelSource, /variant\?: "default" \| "uiverse";/);
  assert.match(searchPanelSource, /const isUiverse = variant === "uiverse";/);
  assert.match(searchPanelSource, /isUiverse \? " search-panel--uiverse" : ""/);
  assert.match(searchPanelSource, /aria-label="清空搜索"/);
  assert.match(searchPanelSource, /className=\{isUiverse \? "search-panel__uiverse-input" : "search-panel__input"\}/);
  assert.match(searchPanelSource, /className="search-panel__reset"/);
  assert.match(searchPanelSource, /className="search-panel__uiverse-submit" type="submit" aria-label="搜索"/);
  assert.match(searchPanelSource, /M7\.667 12\.667A5\.333 5\.333 0 107\.667 2a5\.333 5\.333 0 000 10\.667zM14\.334 14l-2\.9-2\.9/);
  assert.match(searchPanelSource, /className="search-panel__uiverse-submit"[\s\S]*?\{searchInput\}[\s\S]*?className="search-panel__reset"/);
  assert.doesNotMatch(searchPanelSource, /onChange=\{\(e\) => navigate/);
  assert.match(searchForm, /padding\s*:\s*4px/);
  assert.match(searchInput, /height\s*:\s*36px/);
  assert.match(searchSubmit, /height\s*:\s*36px/);

  const uiverseButton = ruleBody(searchCss, ".search-panel--uiverse button");
  const uiversePanel = ruleBody(searchCss, ".search-panel--uiverse");
  const publicUiversePanel = ruleBody(searchCss, ".search-panel--uiverse.search-panel--public");
  const transparentUiversePanel = ruleBody(searchCss, ".search-panel--uiverse.search-panel--transparent");
  const uiverseInput = ruleBody(searchCss, ".search-panel--uiverse .search-panel__uiverse-input");
  const uiverseFormBefore = ruleBody(searchCss, ".search-panel--uiverse::before");
  const focusedUiverseForm = ruleBody(searchCss, ".search-panel--uiverse:focus-within");
  const focusedUiverseFormBefore = ruleBody(searchCss, ".search-panel--uiverse:focus-within::before");
  const hiddenReset = ruleBody(searchCss, ".search-panel--uiverse .search-panel__reset");
  const visibleReset = ruleBody(searchCss, ".search-panel--uiverse .search-panel__uiverse-input:not(:placeholder-shown) ~ .search-panel__reset");
  assert.match(uiverseButton, /color\s*:\s*var\(--text-muted\)/);
  assert.match(uiversePanel, /--timing\s*:\s*0\.3s/);
  assert.match(uiversePanel, /--width-of-input\s*:\s*min\(100%,\s*360px\)/);
  assert.match(uiversePanel, /--height-of-input\s*:\s*40px/);
  assert.match(uiversePanel, /--input-bg\s*:\s*var\(--bg-surface\)/);
  assert.match(uiversePanel, /--border-color\s*:\s*var\(--accent\)/);
  assert.match(uiversePanel, /width\s*:\s*var\(--width-of-input\)/);
  assert.match(uiversePanel, /height\s*:\s*var\(--height-of-input\)/);
  assert.match(uiversePanel, /padding-inline\s*:\s*0\.8em/);
  assert.match(uiversePanel, /border-radius\s*:\s*var\(--border-radius\)/);
  assert.match(uiversePanel, /background\s*:\s*var\(--input-bg, var\(--bg-surface\)\)/);
  assert.doesNotMatch(uiversePanel, /\n\s*border\s*:|box-shadow|backdrop-filter|margin-inline/);
  assert.match(publicUiversePanel, /--width-of-input\s*:\s*100%/);
  assert.match(transparentUiversePanel, /--input-bg\s*:\s*transparent/);
  assert.match(transparentUiversePanel, /border\s*:\s*1px solid var\(--border-default\)/);
  assert.match(uiverseInput, /font-size\s*:\s*0\.9rem/);
  assert.match(uiverseInput, /padding-inline\s*:\s*0\.5em/);
  assert.match(uiverseInput, /padding-block\s*:\s*0\.7em/);
  assert.match(uiverseInput, /border\s*:\s*none/);
  assert.match(uiverseFormBefore, /transform\s*:\s*scaleX\(0\)/);
  assert.match(uiverseFormBefore, /transition\s*:\s*transform var\(--timing\) ease/);
  assert.match(focusedUiverseForm, /border-radius\s*:\s*var\(--after-border-radius\)/);
  assert.match(focusedUiverseFormBefore, /transform\s*:\s*scale\(1\)/);
  assert.match(hiddenReset, /opacity\s*:\s*0/);
  assert.match(hiddenReset, /visibility\s*:\s*hidden/);
  assert.match(visibleReset, /visibility\s*:\s*visible/);
  assert.match(searchCss, /\.search-panel--uiverse svg\s*\{[^}]*width\s*:\s*17px/);
  assert.doesNotMatch(searchCss, /\.search-panel--uiverse \.search-panel__form/);
  assert.doesNotMatch(searchCss, /\.search-panel--uiverse \.search-panel__reset\.is-visible/);
  assert.doesNotMatch(searchCss, /\.search-panel--uiverse \.search-panel__submit\s*\{/);

  assert.match(homePageSource, /import \{ AdminEmptyVisual \} from "@\/admin\/AdminEmptyVisual"/);
  assert.doesNotMatch(homePageSource, /const \[searchQuery, setSearchQuery\] = useState\(""\)/);
  assert.match(homePageSource, /const \[searchParams, setSearchParams\] = useSearchParams\(\)/);
  assert.match(homePageSource, /const activeSearchQuery = searchParams\.get\("q"\)\?\.trim\(\) \?\? ""/);
  assert.match(homePageSource, /const activeTag = searchParams\.get\("tag"\)\?\.trim\(\) \?\? ""/);
  assert.match(homePageSource, /const searchPage = readListingPage\(searchParams\)/);
  assert.match(homePageSource, /const searchSort = readListingSort\(searchParams\)/);
  assert.match(homePageSource, /const searchView = readListingView\(searchParams\)/);
  assert.doesNotMatch(homePageSource, /const handleSearch = useCallback/);
  assert.doesNotMatch(homePageSource, /next\.delete\("tag"\)/);
  assert.match(homePageSource, /<SearchPanel[\s\S]*?navigationPath="\/"[\s\S]*?variant="uiverse"[\s\S]*?placeholder=""[\s\S]*?\/>/);
  assert.match(homePageSource, /className="search-panel--public search-panel--transparent"/);
  assert.match(homePageSource, /const searchPageSize = isMobile\s*\? MOBILE_VIDEO_PAGE_SIZE\s*:\s*HOME_SEARCH_DESKTOP_PAGE_SIZE/);
  assert.match(homePageSource, /const searchResult = useListingQuery\([\s\S]*?q: activeSearchQuery,[\s\S]*?tag: activeTag,[\s\S]*?sort: searchSort,[\s\S]*?page: searchPage,[\s\S]*?pageSize: searchPageSize/);
  assert.match(homePageSource, /\{ enabled: hasActiveFilter \}/);
  assert.match(homePageSource, /withListingPage\(current, 1\)/);
  assert.match(homePageSource, /const hasActiveTag = activeTag\.length > 0/);
  assert.match(homePageSource, /const hasActiveFilter = hasActiveSearch \|\| hasActiveTag/);
  assert.doesNotMatch(homePageSource, /搜索结果：/);
  assert.match(homePageSource, /<SortToolbar[\s\S]*?sort=\{displayedSearchSort\}[\s\S]*?view=\{searchView\}/);
  assert.match(homePageSource, /withListingNavigation\(current, \{ sort: nextSort, page: 1 \}\)/);
  assert.match(homePageSource, /withListingView\(current, nextView\)/);
  assert.match(homePageSource, /compact=\{searchView === "compact"\}/);
  assert.match(homePageSource, /variant="no-results"[\s\S]*?text="未查询到"[\s\S]*?className="admin-empty-state admin-empty-state--plain home-empty-state"/);
  assert.match(homePageSource, /<Pagination[\s\S]*?page=\{displayedSearchPage\}[\s\S]*?pageSize=\{searchSnapshot\.query\.pageSize\}/);
  assert.match(homePageSource, /refreshMode=\{[\s\S]*?searchResult\.transitioning[\s\S]*?"blocking"[\s\S]*?searchResult\.revalidating[\s\S]*?"background"/);
  assert.match(homePageSource, /<ListingLoadError[\s\S]*?displayedPage=\{displayedSearchPage\}/);
  assert.match(homePageSource, /const feedHasContent = feedItems\.length > 0/);
  assert.match(
    homePageSource,
    /\{feedHasContent \|\| hasActiveFilter \? \([\s\S]*?<TagCloud linkBasePath="\/" \/>[\s\S]*?<div className="tag-cloud-container is-reserved" aria-hidden="true" \/>/
  );
  // 两个推荐列表改成一个 tab 栏，不再各占一个 section。
  assert.match(homePageSource, /<HomeFeedTabs\s+feed=\{feed\}/);
  assert.doesNotMatch(homePageSource, /<SectionHeader/);
  assert.doesNotMatch(homePageSource, /随机展示/);
  assert.doesNotMatch(homePageSource, /共 \$\{latest\.length\} 个/);
  assert.match(homePageSource, /className="container page-section home-discovery-section"/);
  assert.match(homePageSource, /className="container page-section home-primary-section"/);
  assert.match(homePageSource, /variant="empty"[\s\S]*?text="当前库中没有视频"[\s\S]*?className="admin-empty-state admin-empty-state--plain home-empty-state"/);
  assert.doesNotMatch(homePageSource, /className="home-empty"/);
  assert.doesNotMatch(homePageSource, /当前没有可播放视频/);

  const discoverySection = ruleBody(layoutCss, ".home-discovery-section");
  const primaryHeader = ruleBody(layoutCss, ".home-primary-section .section-header");
  assert.match(discoverySection, /padding-bottom\s*:\s*var\(--space-2\)/);
  assert.match(primaryHeader, /margin-top\s*:\s*var\(--space-2\)/);

  assert.doesNotMatch(layoutCss, /\.home-empty\s*\{/);
  const homeEmptyState = ruleBody(layoutCss, ".admin-empty-state.home-empty-state");
  assert.match(homeEmptyState, /min-height\s*:\s*clamp\(360px,\s*58vh,\s*620px\)/);
  assert.match(homeEmptyState, /padding\s*:\s*72px 16px 24px/);
});
