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
  assert.match(tabsSource, /className=\{`home-feed-tabs__tab \$\{active \? "is-active" : ""\}`\}/);

  const tabs = ruleBody(layoutCss, ".home-feed-tabs");
  assert.match(tabs, /display\s*:\s*flex/);
  assert.match(tabs, /border-bottom\s*:\s*1px solid var\(--border-subtle\)/);
  const activeUnderline = ruleBody(layoutCss, ".home-feed-tabs__tab.is-active::after");
  assert.match(activeUnderline, /background\s*:\s*var\(--accent-gradient\)/);
});

test("switching tabs replaces the history entry and restarts at the top", () => {
  assert.match(homePageSource, /const feed = readHomeFeed\(searchParams\)/);
  assert.match(
    homePageSource,
    /setSearchParams\(withHomeFeed\(searchParams, nextFeed\), \{\s*replace: true,\s*\}\)/
  );
  assert.match(
    homePageSource,
    /if \(previousFeedKeyRef\.current === feedSource\.key\) return;[\s\S]*?window\.scrollTo\(\{ top: 0, behavior: "auto" \}\)/
  );
});

test("each home tab scrolls infinitely through its own feed source", () => {
  assert.match(
    homePageSource,
    /feed === "latest"\s*\? homeLatestFeedSource\(feedBatchSize\)\s*:\s*homeRecommendationFeedSource\(\)/
  );
  assert.match(
    homePageSource,
    /shouldLoadMore\(\{[\s\S]*?endIndex: range\.endIndex,[\s\S]*?itemCount: feedItems\.length,[\s\S]*?hasMore,[\s\S]*?loading: loadingMore,/
  );
  assert.match(homePageSource, /if \(hasActiveFilter\) return;\s*if \(\s*shouldLoadMore/);
  assert.match(homePageSource, /正在加载更多/);
  assert.match(homePageSource, /没有更多了/);
});

test("search and tag results keep their pagination", () => {
  assert.match(
    homePageSource,
    /const searchResult = useListingQuery\([\s\S]*?page: searchPage,[\s\S]*?pageSize: searchPageSize/
  );
  assert.match(homePageSource, /<Pagination[\s\S]*?page=\{displayedSearchPage\}/);
  // 搜索结果仍然分页，推荐流才是无限滚动。
  assert.match(
    homePageSource,
    /<VideoGrid\s+videos=\{searchItems\}/
  );
});
