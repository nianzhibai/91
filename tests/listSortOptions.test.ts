import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const sortToolbarSource = readFileSync(
  new URL("../src/components/SortToolbar.tsx", import.meta.url),
  "utf8"
);
const listingPageSource = readFileSync(
  new URL("../src/pages/ListingPage.tsx", import.meta.url),
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
const homePageSource = readFileSync(
  new URL("../src/pages/HomePage.tsx", import.meta.url),
  "utf8"
);
const responsiveSource = readFileSync(
  new URL("../src/lib/responsive.ts", import.meta.url),
  "utf8"
);
const layoutCss = readFileSync(
  new URL("../src/styles/layout.css", import.meta.url),
  "utf8"
);
const typesSource = readFileSync(new URL("../src/types.ts", import.meta.url), "utf8");

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

test("list page sort toolbar only exposes active sort options", () => {
  assert.match(sortToolbarSource, /\{ key: "hot", label: "最热" \},\s*\{ key: "latest", label: "最新" \}/);
  assert.match(sortToolbarSource, /\{ key: "recent", label: "最近观看" \}/);
  assert.match(typesSource, /export type SortKey = "latest" \| "hot" \| "recent";/);
  assert.match(sortToolbarSource, /sortDisabled\?: boolean/);
  assert.match(sortToolbarSource, /disabled=\{sortDisabled\}/);
});

test("listing page keeps the public discovery layout and empty semantics", () => {
  assert.match(
    listingPageSource,
    /<SearchPanel[\s\S]*?variant="uiverse"[\s\S]*?placeholder=""[\s\S]*?className="search-panel--public search-panel--transparent"[\s\S]*?\/>/
  );
  assert.match(listingPageSource, /className="container page-section listing-discovery-section"/);
  assert.match(listingPageSource, /className="container page-section listing-primary-section"/);
  assert.match(listingPageSource, /variant=\{hasActiveFilter \? "no-results" : "empty"\}/);
  assert.match(listingPageSource, /text=\{hasActiveFilter \? "未查询到" : "当前库中没有视频"\}/);

  const discoverySection = ruleBody(layoutCss, ".listing-discovery-section");
  assert.match(discoverySection, /padding-bottom\s*:\s*var\(--space-2\)/);
  const listingEmptyState = ruleBody(layoutCss, ".admin-empty-state.listing-empty-state");
  assert.match(listingEmptyState, /min-height\s*:\s*clamp\(360px,\s*58vh,\s*620px\)/);
});

test("public listing query control state is restored from the URL", () => {
  assert.match(listingPageSource, /const sort = readListingSort\(params\)/);
  assert.match(listingPageSource, /withListingNavigation\(current, \{ sort: nextSort, page: 1 \}\)/);
  // 列表页改为无限滚动后没有页码，旧链接里的 page 参数会被清掉。
  assert.match(listingPageSource, /if \(!params\.has\("page"\) && !params\.has\("view"\)\) return;/);
  assert.match(
    listingPageSource,
    /setParams\(normalizeListingSearchParams, \{ replace: true \}\)/
  );
  assert.doesNotMatch(listingPageSource, /<Pagination/);
  assert.doesNotMatch(listingPageSource, /sessionStorage|localStorage/);
});

test("tag selection toggles through the shared listing query instead of rebuilding it", () => {
  assert.match(
    tagCloudSource,
    /const nextTag = activeTag === label \? null : label/
  );
  assert.match(
    tagCloudSource,
    /withListingNavigation\(params, \{ tag: nextTag, page: 1 \}\)/
  );
  assert.match(tagCloudSource, /to=\{buildTagHref\(tag\.label\)\}/);
  assert.doesNotMatch(
    tagCloudSource,
    /to=\{`\$\{linkBasePath\}\?tag=\$\{encodeURIComponent\(tag\.label\)\}`\}/
  );
});

test("list search updates the shared listing query instead of rebuilding it", () => {
  assert.match(searchPanelSource, /navigationPath = "\/list"/);
  assert.match(
    searchPanelSource,
    /withListingNavigation\(params, \{ q, page: 1 \}\)/
  );
  assert.match(
    searchPanelSource,
    /navigate\(query \? `\$\{navigationPath\}\?\$\{query\}` : navigationPath\)/
  );
  assert.doesNotMatch(searchPanelSource, /const sp = new URLSearchParams\(\)/);
});

test("public video lists use fourteen mobile and twenty desktop items per batch", () => {
  assert.match(responsiveSource, /export const MOBILE_VIDEO_PAGE_SIZE = 14;/);
  assert.match(listingPageSource, /const DESKTOP_PAGE_SIZE = 20;/);
  assert.match(listingPageSource, /const pageSize = isMobile \? MOBILE_VIDEO_PAGE_SIZE : DESKTOP_PAGE_SIZE;/);
  assert.match(
    listingPageSource,
    /listingFeedSource\(\{ q: keyword, tag, sort, pageSize \}\)/
  );
  assert.match(listingPageSource, /skeletonCount=\{pageSize\}/);
});

test("home filters use the shared snapshot-based infinite listing contract", () => {
  assert.match(homePageSource, /listingFeedSource\(\{[\s\S]*?q: activeSearchQuery,[\s\S]*?tag: activeTag,[\s\S]*?sort: searchSort/);
  assert.match(homePageSource, /useInfiniteListing\(activeFeedSource, \{/);
  assert.match(homePageSource, /useListingScrollRestore\(\{[\s\S]*?queryKey: activeFeedSource\.key/);
  assert.match(homePageSource, /<VirtualVideoGrid[\s\S]*?hasMore=\{homeFeed\.hasMore\}[\s\S]*?onLoadMore=\{homeFeed\.loadMore\}/);
  assert.match(homePageSource, /sortDisabled=\{homeFeed\.initialLoading\}/);
  assert.doesNotMatch(homePageSource, /useListingQuery|<Pagination|displayedSearchPage/);
});

test("sort tabs share the home tab design without view controls", () => {
  assert.match(sortToolbarSource, /className="content-tabs sort-toolbar"/);
  assert.match(sortToolbarSource, /className="content-tabs__tab"/);
  assert.match(sortToolbarSource, /role="tablist"/);
  assert.match(sortToolbarSource, /aria-selected=\{sort === option\.key\}/);
  assert.doesNotMatch(sortToolbarSource, /ViewMode|onViewChange|LayoutGrid|视图切换/);
  const tab = ruleBody(layoutCss, ".content-tabs__tab");
  assert.match(tab, /background\s*:\s*transparent/);
  assert.match(tab, /border\s*:\s*0/);
});
