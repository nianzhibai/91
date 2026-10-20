import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { setVideoFavorite } from "../src/data/videos.ts";
import { SHORTS_FEED_TABS, readShortsFeedMode } from "../src/shorts/shortsFeed.ts";

const shortsPageSource = readFileSync(
  new URL("../src/pages/ShortsPage.tsx", import.meta.url),
  "utf8"
);
const shortsChromeSource = readFileSync(
  new URL("../src/shorts/ShortsDesktopChrome.tsx", import.meta.url),
  "utf8"
);
const shortsCssSource = readFileSync(
  new URL("../src/styles/shorts.css", import.meta.url),
  "utf8"
);
const videosDataSource = readFileSync(
  new URL("../src/data/videos.ts", import.meta.url),
  "utf8"
);

test("the favorites section sits directly below the hot section", () => {
  const keys = SHORTS_FEED_TABS.map((tab) => tab.key);
  assert.deepEqual(keys, ["latest", "hot", "favorites", "recommend"]);

  const hotIndex = SHORTS_FEED_TABS.findIndex((tab) => tab.key === "hot");
  assert.equal(SHORTS_FEED_TABS[hotIndex + 1].key, "favorites");
  assert.equal(SHORTS_FEED_TABS[hotIndex + 1].label, "收藏");
});

test("the favorites section round-trips through the feed URL parameter", () => {
  assert.equal(readShortsFeedMode(new URLSearchParams("feed=favorites")), "favorites");
  // 桌面侧栏只把推荐提前，其余栏目沿用声明顺序，所以收藏仍紧跟最热。
  assert.match(
    shortsChromeSource,
    /\.\.\.SHORTS_FEED_TABS\.filter\(\(tab\) => tab\.key !== "recommend"\)/
  );
  assert.match(shortsChromeSource, /favorites: Bookmark,/);
});

test("favorite toggle posts and deletes the account favorite endpoint", async (t) => {
  const calls: { url: string; method: string }[] = [];
  const fetchMock = t.mock.method(
    globalThis,
    "fetch",
    async (input: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: String(input), method: String(init?.method ?? "GET") });
      return Response.json({ favorited: init?.method === "POST" });
    }
  );
  try {
    assert.equal(await setVideoFavorite("video/1", true), true);
    assert.equal(await setVideoFavorite("video/1", false), false);
    assert.deepEqual(calls, [
      { url: "/api/video/video%2F1/favorite", method: "POST" },
      { url: "/api/video/video%2F1/favorite", method: "DELETE" },
    ]);
  } finally {
    fetchMock.mock.restore();
  }
});

test("favorite toggle rejects a malformed server response", async (t) => {
  const fetchMock = t.mock.method(globalThis, "fetch", async () => Response.json({ ok: true }));
  try {
    await assert.rejects(setVideoFavorite("video-1", true), /Invalid video favorite response/);
  } finally {
    fetchMock.mock.restore();
  }
});

test("the shorts favorite button renders directly below the like button", () => {
  // 收藏按钮必须排在点赞之后、分享之前，用户才能在最热/推荐里顺手收藏。
  assert.match(shortsPageSource, /data-shorts-like[\s\S]*?data-shorts-favorite/);
  assert.match(shortsPageSource, /data-shorts-favorite[\s\S]*?aria-label="分享"/);
  assert.match(
    shortsPageSource,
    /className=\{`shorts-slide__action \$\{isFavorited \? "is-favorited" : ""\}`\}/
  );
  assert.match(shortsPageSource, /aria-pressed=\{isFavorited\}/);
  assert.match(shortsCssSource, /\.shorts-slide__action\.is-favorited \{/);
});

test("favorite state comes from the server item and rolls back on failure", () => {
  assert.match(videosDataSource, /favorited\?: boolean;/);
  assert.match(
    shortsPageSource,
    /const \[isFavorited, setIsFavorited\] = useState\(item\.favorited \?\? false\);/
  );
  assert.match(
    shortsPageSource,
    /useEffect\(\(\) => \{\s*setIsFavorited\(item\.favorited \?\? false\);\s*\}, \[item\.id, item\.favorited\]\);/
  );
  // 服务端回执是唯一事实来源：null 表示失败，UI 回到相反状态。
  assert.match(shortsPageSource, /setIsFavorited\(favorited \?\? !willFavorite\);/);
  assert.match(
    shortsPageSource,
    /onFavoriteToggle: \(videoId: string, favorite: boolean\) => Promise<boolean \| null>;/
  );
  assert.match(shortsPageSource, /onFavoriteToggle=\{handleFavoriteToggle\}/);
});

test("the empty favorites section says so instead of claiming an empty library", () => {
  assert.match(shortsPageSource, /favorites: "还没有收藏的视频"/);
  assert.match(shortsPageSource, /text=\{SHORTS_EMPTY_TEXT\[mode\]\}/);
});
