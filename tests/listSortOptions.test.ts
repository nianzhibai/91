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
const listingQuerySource = readFileSync(
  new URL("../src/lib/useListingQuery.ts", import.meta.url),
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
const searchCss = readFileSync(
  new URL("../src/styles/search.css", import.meta.url),
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
  assert.match(listingPageSource, /const view = readListingView\(params\)/);
  assert.match(listingPageSource, /withListingNavigation\(params, \{ sort: nextSort, page: 1 \}\)/);
  assert.match(listingPageSource, /withListingView\(params, nextView\)/);
  // 列表页改为无限滚动后没有页码，旧链接里的 page 参数会被清掉。
  assert.match(listingPageSource, /if \(!params\.has\("page"\)\) return;/);
  assert.match(
    listingPageSource,
    /withListingPage\(current, 1\), \{ replace: true \}/
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

test("home search keeps the shared stale-while-revalidate query contract", () => {
  assert.match(homePageSource, /import \{ useListingQuery \} from "@\/lib\/useListingQuery"/);
  assert.match(homePageSource, /refreshMode=\{[\s\S]*?searchResult\.transitioning[\s\S]*?"blocking"[\s\S]*?searchResult\.revalidating[\s\S]*?"background"/);
  assert.match(homePageSource, /disabled=\{searchResult\.transitioning\}/);
  assert.match(homePageSource, /<ListingLoadError[\s\S]*?displayedPage=\{displayedSearchPage\}/);
  assert.match(homePageSource, /searchSnapshot\?\.query\.page \?\? searchPage/);
  assert.match(listingQuerySource, /const LISTING_CACHE_TTL_MS = 60_000/);
  assert.match(listingQuerySource, /const LISTING_CACHE_MAX_ENTRIES = 20/);
  assert.match(listingQuerySource, /function peekListingCache/);
  assert.match(listingQuerySource, /transitioning: refreshing && !snapshotMatchesRequest/);
  assert.match(listingQuerySource, /revalidating: refreshing && snapshotMatchesRequest/);
  assert.match(listingQuerySource, /return \(\) => controller\.abort\(\)/);
});

test("sort toolbar has no outer frame around its controls", () => {
  const toolbar = ruleBody(searchCss, ".sort-toolbar");
  const group = ruleBody(searchCss, ".sort-toolbar__group");

  assert.match(toolbar, /padding\s*:\s*0/);
  assert.doesNotMatch(toolbar, /background\s*:/);
  assert.doesNotMatch(toolbar, /border\s*:/);
  assert.match(group, /background\s*:\s*var\(--bg-sunken\)/);
  assert.match(group, /border\s*:\s*1px solid var\(--border-subtle\)/);
});
