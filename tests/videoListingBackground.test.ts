import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import type { Location } from "react-router";
import {
  RouteActivityProvider,
  useRouteActivity,
} from "../src/lib/routeActivity.tsx";
import {
  continueVideoDetailNavigationState,
  createVideoDetailNavigationState,
  isVideoListingPath,
  readVideoListingBackground,
} from "../src/lib/videoListingBackground.ts";

const listingLocation: Location = {
  pathname: "/list",
  search: "?tag=剧情&sort=latest",
  hash: "",
  state: { source: "test" },
  key: "listing-history-key",
};

function ActivityProbe() {
  return createElement("span", null, String(useRouteActivity()));
}

test("retained routes expose whether their mounted tree is active", () => {
  assert.equal(
    renderToStaticMarkup(createElement(ActivityProbe)),
    "<span>true</span>"
  );
  assert.equal(
    renderToStaticMarkup(
      createElement(
        RouteActivityProvider,
        { active: false },
        createElement(ActivityProbe)
      )
    ),
    "<span>false</span>"
  );
});

test("video detail navigation retains one same-document listing location", () => {
  const state = createVideoDetailNavigationState(
    "/list?tag=剧情&sort=latest",
    listingLocation
  );

  assert.equal(state.from, "/list?tag=剧情&sort=latest");
  assert.deepEqual(readVideoListingBackground(state), listingLocation);

  const continued = continueVideoDetailNavigationState("/list", state);
  assert.deepEqual(
    readVideoListingBackground(continued),
    listingLocation,
    "recommendation and collection navigation keep the original listing"
  );
});

test("only home and list pages can become retained video backgrounds", () => {
  assert.equal(isVideoListingPath("/"), true);
  assert.equal(isVideoListingPath("/list"), true);
  assert.equal(isVideoListingPath("/list/"), true);
  assert.equal(isVideoListingPath("/shorts"), false);
  const unrelated: Location = {
    ...listingLocation,
    pathname: "/shorts",
    key: "shorts-history-key",
  };
  const state = createVideoDetailNavigationState("/shorts", unrelated);

  assert.deepEqual(state, { from: "/shorts" });
  assert.equal(readVideoListingBackground(state), null);
  assert.equal(readVideoListingBackground(null), null);
  assert.equal(readVideoListingBackground({ videoListingBackground: {} }), null);
});

test("history state from another document cannot retain a stale React tree", () => {
  const state = createVideoDetailNavigationState("/list", listingLocation);
  const retained = state.videoListingBackground;
  assert.ok(retained);

  const stale = {
    ...state,
    videoListingBackground: {
      ...retained,
      documentID: "previous-document",
    },
  };
  assert.equal(readVideoListingBackground(stale), null);
});

test("the app keeps the listing mounted behind an independently scrolling detail", () => {
  const app = readFileSync(new URL("../src/App.tsx", import.meta.url), "utf8");
  const card = readFileSync(
    new URL("../src/components/VideoCard.tsx", import.meta.url),
    "utf8"
  );
  const rail = readFileSync(
    new URL("../src/components/RecommendedRail.tsx", import.meta.url),
    "utf8"
  );
  const collection = readFileSync(
    new URL("../src/components/MobileVideoCollection.tsx", import.meta.url),
    "utf8"
  );
  const layout = readFileSync(
    new URL("../src/styles/layout.css", import.meta.url),
    "utf8"
  );
  const scrollRestore = readFileSync(
    new URL("../src/lib/useListingScrollRestore.ts", import.meta.url),
    "utf8"
  );
  const home = readFileSync(
    new URL("../src/pages/HomePage.tsx", import.meta.url),
    "utf8"
  );
  const listing = readFileSync(
    new URL("../src/pages/ListingPage.tsx", import.meta.url),
    "utf8"
  );

  assert.match(app, /readVideoListingBackground\(location\.state\)/);
  assert.doesNotMatch(app, /UNSAFE_LocationContext/);
  assert.match(
    app,
    /function RetainedListingSurface\([\s\S]*?<RouteActivityProvider active=\{active\}>[\s\S]*?<ListingRoutes location=\{location\} \/>/
  );
  assert.match(
    app,
    /function ListingRoutes[\s\S]*?<Routes location=\{location\}>/
  );
  assert.match(
    app,
    /const listingLocation = listingBackground \?\? activeListingLocation;[\s\S]*?<RetainedListingSurface[\s\S]*?active=\{listingBackground === null\}/
  );
  assert.match(app, /\{listingBackground && <VideoDetailForeground \/>\}/);
  assert.match(
    app,
    /function VideoDetailForeground\(\)[\s\S]*?useDocumentScrollLock\(true\)[\s\S]*?<PageScrollRootProvider/
  );
  assert.match(app, /document\.title = returnTitleRef\.current/);
  assert.match(app, /rootRef\.current\.inert = !active/);
  assert.match(app, /aria-hidden=\{!active \? true : undefined\}/);
  const inactiveListingStyle = layout.match(
    /\.app-primary-route\[aria-hidden="true"\]\s*\{([^}]+)\}/
  )?.[1];
  assert.ok(inactiveListingStyle);
  assert.match(inactiveListingStyle, /visibility:\s*hidden;/);
  assert.match(inactiveListingStyle, /opacity:\s*0;/);
  assert.match(inactiveListingStyle, /pointer-events:\s*none;/);
  // Removing layout would destroy the retained scroll surface and row geometry.
  assert.doesNotMatch(inactiveListingStyle, /display:\s*none|content-visibility:\s*hidden/);

  for (const page of [home, listing]) {
    assert.match(page, /const routeActive = useRouteActivity\(\)/);
    assert.match(page, /pausePagination: !routeActive/);
    assert.match(page, /active: routeActive/);
  }
  assert.match(scrollRestore, /if \(!active\) return;/);
  assert.match(
    scrollRestore,
    /window\.history\.scrollRestoration = "manual"[\s\S]*?window\.history\.scrollRestoration = "auto"/
  );

  assert.match(card, /createVideoDetailNavigationState\(currentPath, location\)/);
  assert.match(rail, /continueVideoDetailNavigationState\(returnPath, location\.state\)/);
  assert.match(
    collection,
    /continueVideoDetailNavigationState\(\s*returnPath,\s*location\.state\s*\)/
  );

  assert.match(
    layout,
    /\.video-detail-foreground\s*\{[^}]*position:\s*fixed;[^}]*overflow-y:\s*auto;/s
  );
  assert.match(layout, /scrollbar-gutter:\s*stable/);
  assert.match(
    layout,
    /background-color:\s*var\(--bg-page\)/
  );
});
