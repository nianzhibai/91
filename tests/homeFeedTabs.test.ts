import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  readHomeFeed,
  withHomeFeed,
} from "../src/lib/listingSearchParams.ts";

const homePageSource = readFileSync(
  new URL("../src/pages/HomePage.tsx", import.meta.url),
  "utf8"
);
const tabsSource = readFileSync(
  new URL("../src/components/HomeFeedTabs.tsx", import.meta.url),
  "utf8"
);
const infiniteFeedStatusSource = readFileSync(
  new URL("../src/components/InfiniteFeedStatus.tsx", import.meta.url),
  "utf8"
);
const layoutCss = readFileSync(
  new URL("../src/styles/layout.css", import.meta.url),
  "utf8"
);

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

test("the active home tab lives in the URL so history can restore it", () => {
  assert.equal(readHomeFeed(new URLSearchParams("")), "recommend");
  assert.equal(readHomeFeed(new URLSearchParams("feed=latest")), "latest");
  assert.equal(
    readHomeFeed(new URLSearchParams("feed=whatever")),
    "recommend",
    "无法识别的值回落到默认 tab"
  );

  const latest = withHomeFeed(new URLSearchParams("q=猫"), "latest");
  assert.equal(latest.get("feed"), "latest");
  assert.equal(latest.get("q"), "猫", "切 tab 不能丢掉其它查询参数");
  assert.equal(
    withHomeFeed(latest, "recommend").get("feed"),
    null,
    "默认 tab 不写进 URL"
  );
});

test("the home tabs are an accessible tab list with the random feed first", () => {
  assert.match(
    tabsSource,
    /\{ key: "recommend", label: "随机推荐" \},\s*\{ key: "latest", label: "最新视频" \}/
  );
  assert.match(tabsSource, /role="tablist"/);
  assert.match(tabsSource, /role="tab"/);
  assert.match(tabsSource, /aria-selected=\{active\}/);
  assert.match(tabsSource, /className="content-tabs__tab home-feed-tabs__tab"/);

  const tabs = ruleBody(layoutCss, ".content-tabs");
  assert.match(tabs, /display\s*:\s*flex/);
  assert.match(tabs, /border-bottom\s*:\s*1px solid var\(--border-subtle\)/);
  const activeUnderline = ruleBody(layoutCss, '.content-tabs__tab[aria-selected="true"]::after');
  assert.match(activeUnderline, /background\s*:\s*var\(--accent-gradient\)/);
});

test("switching tabs replaces the history entry and restarts at the top", () => {
  assert.match(homePageSource, /const feed = readHomeFeed\(searchParams\)/);
  assert.match(
    homePageSource,
    /setSearchParams\(\(current\) => withHomeFeed\(current, nextFeed\), \{\s*replace: true,\s*\}\)/
  );
  assert.match(
    homePageSource,
    /if \(previousFeedKeyRef\.current === activeFeedSource\.key\) return;[\s\S]*?window\.scrollTo\(\{ top: 0, behavior: "auto" \}\)/
  );
});

test("each home tab scrolls infinitely through its own feed source", () => {
  assert.match(
    homePageSource,
    /feed === "latest"\s*\? homeLatestFeedSource\(batchSize\)\s*:\s*homeRecommendationFeedSource\(\)/
  );
  assert.match(
    homePageSource,
    /<VirtualVideoGrid[\s\S]*?videos=\{feedItems\}[\s\S]*?hasMore=\{homeFeed\.hasMore\}[\s\S]*?loadingMore=\{homeFeed\.loadingMore\}[\s\S]*?prefetchRows=\{PREFETCH_ROWS\}[\s\S]*?onLoadMore=\{homeFeed\.loadMore\}/
  );
  assert.doesNotMatch(homePageSource, /setRange|onRangeChange|shouldLoadMore/);
  assert.match(homePageSource, /<InfiniteFeedStatus state="loading" \/>/);
  assert.match(homePageSource, /<InfiniteFeedStatus state="end" \/>/);
  assert.match(infiniteFeedStatusSource, /正在加载更多/);
  assert.match(infiniteFeedStatusSource, /没有更多了/);
});

test("search, tag, and combined results use the same infinite listing", () => {
  assert.match(
    homePageSource,
    /const filterFeedSource = useMemo\([\s\S]*?listingFeedSource\(\{[\s\S]*?q: activeSearchQuery,[\s\S]*?tag: activeTag,[\s\S]*?sort: searchSort,[\s\S]*?pageSize: batchSize/
  );
  assert.match(
    homePageSource,
    /const activeFeedSource = hasActiveFilter \? filterFeedSource : feedSource;[\s\S]*?useInfiniteListing\(activeFeedSource,/
  );
  assert.match(
    homePageSource,
    /<VirtualVideoGrid[\s\S]*?videos=\{feedItems\}[\s\S]*?onLoadMore=\{homeFeed\.loadMore\}/
  );
  assert.match(homePageSource, /if \(!searchParams\.has\("page"\) && !searchParams\.has\("view"\)\) return;/);
  assert.doesNotMatch(homePageSource, /useListingQuery|<Pagination/);
});
