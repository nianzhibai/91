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
    /const isRandomRecommendationFeed =\s*!hasActiveFilter && feed === "recommend";/
  );
  assert.match(homePageSource, /const showRefresh = isRandomRecommendationFeed;/);
  assert.match(homePageSource, /const refreshing = showRefresh && homeFeed\.initialLoading;/);
  assert.match(homePageSource, /\{showRefresh && \(/);
  assert.match(
    homePageSource,
    /const refreshHome = useCallback\(\(\) => \{\s*window\.scrollTo\(\{ top: 0, behavior: "auto" \}\);\s*reloadFeed\(\);/
  );
  assert.match(homePageSource, /const HOME_DESKTOP_BATCH_SIZE = 20;/);
  assert.match(
    homePageSource,
    /const batchSize = isMobile\s*\? MOBILE_VIDEO_PAGE_SIZE\s*:\s*HOME_DESKTOP_BATCH_SIZE;/
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

test("home page keeps every active result set in the shared feed session", () => {
  // 返回首页时的续播、去重、中断过期请求都由 useInfiniteListing 负责，
  // 页面自己不再维护模块级缓存和请求版本号。
  assert.match(homePageSource, /const activeFeedSource = hasActiveFilter \? filterFeedSource : feedSource;/);
  assert.match(homePageSource, /const homeFeed = useInfiniteListing\(activeFeedSource, \{/);
  assert.match(homePageSource, /restoreCount: restoreTarget\.count,/);
  assert.match(homePageSource, /queryKey: activeFeedSource\.key,[\s\S]*?requestedCount: homeFeed\.requestedCount/);
  assert.doesNotMatch(homePageSource, /rankingRequestVersion|latestRequestVersion|homeRequestVersion/);
  assert.doesNotMatch(homePageSource, /loadRanking|loadLatest|refreshLatest/);
  assert.doesNotMatch(homePageSource, /HOME_CACHE_TTL_MS|cacheIsFresh|cachedRankingAt|cachedLatestAt/);
  assert.doesNotMatch(homePageSource, /localStorage/);
});

test("home and list pages load the shared tag cloud independently from video results", () => {
  assert.match(tagCloudSource, /const visibleTags = useMemo/);
  assert.match(tagCloudSource, /typeof tag\.count !== "number" \|\| tag\.count > 0/);
  assert.match(tagCloudSource, /const initialTagsRef = useRef<TagItem\[\] \| null>\(readCachedTags\(\)\)/);
  assert.match(tagCloudSource, /const \[tags,\s*setTags\] = useState<TagItem\[\]>\(initialTagsRef\.current \?\? \[\]\)/);
  assert.match(tagCloudSource, /type TagCloudStatus = "loading" \| "ready" \| "error"/);
  assert.match(tagCloudSource, /const \[retryVersion, setRetryVersion\] = useState\(0\)/);
  assert.match(tagCloudSource, /if \(initialTagsRef\.current !== null && retryVersion === 0\) return/);
  assert.match(tagCloudSource, /setStatus\("ready"\)/);
  assert.match(tagCloudSource, /setStatus\("error"\)/);
  assert.match(tagCloudSource, /if \(status === "loading"\) return null/);
  assert.match(tagCloudSource, /if \(status === "ready" && visibleTags\.length === 0\) return null/);
  assert.match(tagCloudSource, /const failed = status === "error" && visibleTags\.length === 0/);
  assert.match(tagCloudSource, /标签加载失败/);
  assert.match(tagCloudSource, /重新加载/);
  assert.match(tagCloudSource, /setRetryVersion\(\(current\) => current \+ 1\)/);
  assert.match(tagCloudSource, /type TagCloudProps = \{/);
  assert.match(tagCloudSource, /linkBasePath\?: string;/);
  assert.match(tagCloudSource, /onTagSelect\?: \(\) => void;/);
  assert.match(tagCloudSource, /export const TagCloud = memo\(function TagCloud\(\{[\s\S]*?linkBasePath = "\/list",[\s\S]*?onTagSelect,[\s\S]*?\}: TagCloudProps\)/);
  assert.match(tagCloudSource, /to=\{buildTagHref\(tag\.label\)\}/);
  assert.match(tagCloudSource, /onClick=\{onTagSelect\}/);
  assert.match(tagCloudSource, /<nav[\s\S]*?aria-label="热门标签"/);
  assert.doesNotMatch(tagCloudSource, /热搜榜|tag-cloud__title/);
  assert.match(tagCloudSource, /aria-current=\{activeTag === tag\.label \? "true" : undefined\}/);
  assert.doesNotMatch(tagCloudSource, /addEventListener|ResizeObserver/);
  assert.doesNotMatch(tagCloudSource, /aria-busy|TAG_PLACEHOLDER_COUNT|tag-cloud__placeholder/);
  assert.doesNotMatch(tagCloudSource, /setTimeout/);
  assert.match(tagCloudSource, /visibleTags\.map\(renderTag\)/);
  assert.doesNotMatch(tagCloudSource, /const row[12] = visibleTags\.filter/);
  assert.doesNotMatch(tagCloudSource, /\(\{tag\.count\}\)/);
  assert.doesNotMatch(tagCloudSource, /`\$\{tag\.count\} 个视频`/);

  const tagCloudContainer = ruleBody(searchCss, ".tag-cloud-container");
  const tagCloudError = ruleBody(searchCss, ".tag-cloud__error");
  const tagCloudRetry = ruleBody(searchCss, ".tag-cloud__retry");
  const tagCloudLink = ruleBody(searchCss, ".tag-cloud__link");
  assert.match(tagCloudContainer, /min-height\s*:\s*24px/);
  assert.doesNotMatch(tagCloudContainer, /mask-image|overflow/);
  assert.match(tagCloudError, /flex-wrap\s*:\s*wrap/);
  assert.match(tagCloudRetry, /color\s*:\s*var\(--accent\)/);
  assert.match(tagCloudLink, /overflow-wrap\s*:\s*anywhere/);
  assert.doesNotMatch(tagCloudLink, /background\s*:|border\s*:/);
  assert.doesNotMatch(searchCss, /tag-cloud__placeholder|\.tag-cloud-container\[aria-busy/);

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
  assert.match(homePageSource, /const searchSort = readListingSort\(searchParams\)/);
  assert.doesNotMatch(homePageSource, /const handleSearch = useCallback/);
  assert.doesNotMatch(homePageSource, /next\.delete\("tag"\)/);
  assert.match(homePageSource, /<SearchPanel[\s\S]*?navigationPath="\/"[\s\S]*?variant="uiverse"[\s\S]*?placeholder=""[\s\S]*?\/>/);
  assert.match(homePageSource, /className="search-panel--public search-panel--transparent"/);
  assert.match(homePageSource, /const batchSize = isMobile\s*\? MOBILE_VIDEO_PAGE_SIZE\s*:\s*HOME_DESKTOP_BATCH_SIZE/);
  assert.match(homePageSource, /listingFeedSource\(\{[\s\S]*?q: activeSearchQuery,[\s\S]*?tag: activeTag,[\s\S]*?sort: searchSort,[\s\S]*?pageSize: batchSize/);
  assert.doesNotMatch(homePageSource, /useListingQuery|<Pagination/);
  assert.match(homePageSource, /setSearchParams\(normalizeListingSearchParams, \{ replace: true \}\)/);
  assert.match(homePageSource, /const hasActiveTag = activeTag\.length > 0/);
  assert.match(homePageSource, /const hasActiveFilter = hasActiveSearch \|\| hasActiveTag/);
  assert.doesNotMatch(homePageSource, /搜索结果：/);
  assert.match(homePageSource, /<SortToolbar[\s\S]*?sort=\{searchSort\}/);
  assert.match(homePageSource, /withListingNavigation\(current, \{ sort: nextSort, page: 1 \}\)/);
  assert.doesNotMatch(homePageSource, /ViewMode|readListingView|withListingView|compact=/);
  assert.match(homePageSource, /variant=\{hasActiveFilter \? "no-results" : "empty"\}[\s\S]*?text=\{hasActiveFilter \? "未查询到" : "当前库中没有视频"\}/);
  assert.match(homePageSource, /<VirtualVideoGrid[\s\S]*?onLoadMore=\{homeFeed\.loadMore\}/);
  assert.match(homePageSource, /const feedHasContent = feedItems\.length > 0/);
  assert.match(homePageSource, /<TagCloud linkBasePath="\/" \/>/);
  assert.doesNotMatch(homePageSource, /is-reserved/);
  // 两个推荐列表改成一个 tab 栏，不再各占一个 section。
  assert.match(homePageSource, /<HomeFeedTabs\s+feed=\{feed\}/);
  assert.doesNotMatch(homePageSource, /<SectionHeader/);
  assert.doesNotMatch(homePageSource, /随机展示/);
  assert.doesNotMatch(homePageSource, /共 \$\{latest\.length\} 个/);
  assert.match(homePageSource, /className="container page-section home-discovery-section"/);
  assert.match(homePageSource, /className="container page-section home-primary-section"/);
  assert.match(homePageSource, /className="admin-empty-state admin-empty-state--plain home-empty-state"/);
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
