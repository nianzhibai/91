import assert from "node:assert/strict";
import test from "node:test";

import {
  readListingPage,
  readListingSort,
  normalizeListingSearchParams,
  withListingNavigation,
  withListingPage,
  withListingSort,
} from "../src/lib/listingSearchParams.ts";

test("listing sort is restored from the URL after returning from a video", () => {
  assert.equal(readListingSort(new URLSearchParams("sort=latest")), "latest");
  assert.equal(readListingSort(new URLSearchParams("sort=recent")), "recent");
  assert.equal(readListingSort(new URLSearchParams("sort=hot")), "hot");
  assert.equal(readListingSort(new URLSearchParams("sort=unknown")), "hot");
  assert.equal(readListingSort(new URLSearchParams()), "hot");
});

test("listing page URL state uses stable defaults", () => {
  assert.equal(readListingPage(new URLSearchParams("page=3")), 3);
  assert.equal(readListingPage(new URLSearchParams("page=0")), 1);
  assert.equal(readListingPage(new URLSearchParams("page=3.5")), 1);
  assert.equal(readListingPage(new URLSearchParams("page=invalid")), 1);

  const original = new URLSearchParams("q=舞蹈&tag=推荐");
  const deepPage = withListingPage(original, 4);
  assert.equal(deepPage.get("page"), "4");
  assert.equal(withListingPage(deepPage, 1).get("page"), null);
});

test("listing navigation applies page and sort atomically", () => {
  const original = new URLSearchParams("q=舞蹈&page=8");
  const next = withListingNavigation(original, {
    page: 1,
    sort: "latest",
  });

  assert.equal(next.get("q"), "舞蹈");
  assert.equal(next.get("page"), null);
  assert.equal(next.get("sort"), "latest");
  assert.equal(original.get("page"), "8");
});

test("tag navigation preserves the current keyword and sort while resetting the page", () => {
  const original = new URLSearchParams(
    "q=舞蹈&tag=旧标签&sort=recent&page=8"
  );
  const next = withListingNavigation(original, {
    tag: "新标签",
    page: 1,
  });

  assert.equal(next.get("q"), "舞蹈");
  assert.equal(next.get("tag"), "新标签");
  assert.equal(next.get("sort"), "recent");
  assert.equal(next.get("page"), null);
  assert.equal(original.get("tag"), "旧标签");
  assert.equal(original.get("page"), "8");

  const cleared = withListingNavigation(next, { tag: null, page: 1 });
  assert.equal(cleared.get("q"), "舞蹈");
  assert.equal(cleared.get("tag"), null);
  assert.equal(cleared.get("sort"), "recent");
  assert.equal(cleared.get("page"), null);
});

test("search navigation preserves the current tag and sort while resetting the page", () => {
  const original = new URLSearchParams(
    "q=旧关键字&tag=推荐&sort=recent&page=8"
  );
  const next = withListingNavigation(original, {
    q: "新关键字",
    page: 1,
  });

  assert.equal(next.get("q"), "新关键字");
  assert.equal(next.get("tag"), "推荐");
  assert.equal(next.get("sort"), "recent");
  assert.equal(next.get("page"), null);

  const cleared = withListingNavigation(next, { q: null, page: 1 });
  assert.equal(cleared.get("q"), null);
  assert.equal(cleared.get("tag"), "推荐");
  assert.equal(cleared.get("sort"), "recent");
});

test("normalizing a listing URL removes retired controls and preserves filters", () => {
  const original = new URLSearchParams("q=舞蹈&tag=推荐&sort=recent&feed=latest&page=8&view=compact");
  const normalized = normalizeListingSearchParams(original);
  assert.equal(normalized.toString(), new URLSearchParams("q=舞蹈&tag=推荐&sort=recent&feed=latest").toString());
  assert.equal(original.get("page"), "8");
  assert.equal(original.get("view"), "compact");
});

test("listing sort URL updates preserve filters and omit the default", () => {
  const original = new URLSearchParams("q=舞蹈&tag=推荐");

  const latest = withListingSort(original, "latest");
  assert.equal(latest.get("q"), "舞蹈");
  assert.equal(latest.get("tag"), "推荐");
  assert.equal(latest.get("sort"), "latest");
  assert.equal(original.get("sort"), null, "the current location must not be mutated");

  const hot = withListingSort(latest, "hot");
  assert.equal(hot.get("q"), "舞蹈");
  assert.equal(hot.get("tag"), "推荐");
  assert.equal(hot.get("sort"), null);
});
