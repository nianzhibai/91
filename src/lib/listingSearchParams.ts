import type { SortKey } from "../types";

type ListingNavigationPatch = {
  q?: string | null;
  tag?: string | null;
  page?: number;
  sort?: SortKey;
};

export function readListingSort(params: URLSearchParams): SortKey {
  const sort = params.get("sort");
  switch (sort) {
    case "latest":
    case "recent":
    case "hot":
      return sort;
    default:
      return "hot";
  }
}

export function withListingSort(
  params: URLSearchParams,
  sort: SortKey
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (sort === "hot") {
    next.delete("sort");
  } else {
    next.set("sort", sort);
  }
  return next;
}

export function readListingPage(params: URLSearchParams): number {
  const raw = params.get("page");
  if (!raw || !/^\d+$/.test(raw)) return 1;
  const page = Number.parseInt(raw, 10);
  return Number.isSafeInteger(page) && page > 0 ? page : 1;
}

export function withListingPage(
  params: URLSearchParams,
  page: number
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (!Number.isSafeInteger(page) || page <= 1) {
    next.delete("page");
  } else {
    next.set("page", String(page));
  }
  return next;
}

export function normalizeListingSearchParams(params: URLSearchParams): URLSearchParams {
  const next = withListingPage(params, 1);
  next.delete("view");
  return next;
}

export type HomeFeedKey = "recommend" | "latest";

/** 首页推荐/最新两个 tab 记在 URL 里，前进后退才能回到原来那个 tab。 */
export function readHomeFeed(params: URLSearchParams): HomeFeedKey {
  return params.get("feed") === "latest" ? "latest" : "recommend";
}

export function withHomeFeed(
  params: URLSearchParams,
  feed: HomeFeedKey
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (feed === "latest") {
    next.set("feed", "latest");
  } else {
    next.delete("feed");
  }
  return next;
}

export function withListingNavigation(
  params: URLSearchParams,
  patch: ListingNavigationPatch
): URLSearchParams {
  let next = new URLSearchParams(params);
  if (patch.q !== undefined) next = withListingFilter(next, "q", patch.q);
  if (patch.tag !== undefined) next = withListingFilter(next, "tag", patch.tag);
  if (patch.sort !== undefined) next = withListingSort(next, patch.sort);
  if (patch.page !== undefined) next = withListingPage(next, patch.page);
  return next;
}

function withListingFilter(
  params: URLSearchParams,
  key: "q" | "tag",
  value: string | null
): URLSearchParams {
  const next = new URLSearchParams(params);
  const normalized = value?.trim() ?? "";
  if (normalized) {
    next.set(key, normalized);
  } else {
    next.delete(key);
  }
  return next;
}
