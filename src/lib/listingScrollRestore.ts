import {
  parseVirtualGridSnapshot,
  type VirtualGridSnapshot,
} from "@/lib/virtualGrid";

/**
 * 前进/后退时恢复无限滚动列表的现场。历史条目自己的 key 作为存储键，
 * 所以"后退回列表"能拿回当时的滚动位置，而"重新点进列表"是干净的新会话。
 */

// v2 only stores measurements from the responsive card grid.
export const LISTING_SCROLL_STORAGE_PREFIX = "listing_scroll_v2:";

export type ListingScrollEntry = {
  queryKey: string;
  /** 服务端不可变快照；为空时恢复到同一查询的新快照。 */
  feedToken: string;
  /** 保存现场时已经请求过的条目数。 */
  requestedCount: number;
  scrollY: number;
  grid?: VirtualGridSnapshot;
};

export type ListingScrollStorage = {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
};

export function listingScrollStorageKey(historyKey: string): string {
  return `${LISTING_SCROLL_STORAGE_PREFIX}${historyKey}`;
}

export function parseListingScrollEntry(
  raw: string | null
): ListingScrollEntry | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw);
    if (
      !parsed ||
      typeof parsed.queryKey !== "string" ||
      (parsed.feedToken !== undefined && typeof parsed.feedToken !== "string") ||
      (typeof parsed.feedToken === "string" && parsed.feedToken.length > 128) ||
      !Number.isInteger(parsed.requestedCount) ||
      parsed.requestedCount <= 0 ||
      !Number.isFinite(parsed.scrollY) ||
      parsed.scrollY < 0
    ) {
      return null;
    }
    const grid = parseVirtualGridSnapshot(parsed.grid);
    return {
      queryKey: parsed.queryKey,
      feedToken: parsed.feedToken ?? "",
      requestedCount: parsed.requestedCount,
      scrollY: parsed.scrollY,
      ...(grid ? { grid } : {}),
    };
  } catch {
    return null;
  }
}

export function readListingScrollEntry(
  storage: ListingScrollStorage | null,
  historyKey: string
): ListingScrollEntry | null {
  if (!storage || !historyKey) return null;
  try {
    return parseListingScrollEntry(
      storage.getItem(listingScrollStorageKey(historyKey))
    );
  } catch {
    // 隐私模式下读写 sessionStorage 会抛错，恢复失败只是回到列表顶部。
    return null;
  }
}

export function writeListingScrollEntry(
  storage: ListingScrollStorage | null,
  historyKey: string,
  entry: ListingScrollEntry
): void {
  if (!storage || !historyKey) return;
  try {
    storage.setItem(
      listingScrollStorageKey(historyKey),
      JSON.stringify(entry)
    );
  } catch {
    // 同上：写不进去只影响返回时的位置恢复。
  }
}

export function clearListingScrollEntry(
  storage: ListingScrollStorage | null,
  historyKey: string
): void {
  if (!storage || !historyKey) return;
  try {
    storage.removeItem(listingScrollStorageKey(historyKey));
  } catch {
    // ignore
  }
}

/**
 * Run once before mounting the router. A reload refreshes only the entry being
 * opened; other history entries retain their snapshots even across documents.
 * A browser back/forward load should restore the entry being opened as well.
 */
export function initializeListingScrollRestore(
  browser: Pick<Window, "history" | "performance" | "sessionStorage">
): void {
  const navigation = browser.performance.getEntriesByType("navigation")[0] as
    | PerformanceNavigationTiming
    | undefined;
  if (navigation?.type === "back_forward") return;
  try {
    clearListingScrollEntry(
      browser.sessionStorage,
      browser.history.state?.key ?? "default"
    );
  } catch {
    // Accessing sessionStorage itself may be blocked by the browser.
  }
}

/**
 * 恢复历史条目已加载的完整进度。单次传输的大小限制由数据层分批处理，
 * 不能截断这里的总数，否则深滚后的原位置将不再存在。
 * 返回 0 表示按普通首屏加载。
 */
export function resolveRestoreCount(input: {
  entry: ListingScrollEntry | null;
  queryKey: string;
  pageSize: number;
}): number {
  const { entry, queryKey } = input;
  const pageSize =
    Number.isInteger(input.pageSize) && input.pageSize > 0 ? input.pageSize : 0;
  if (!entry || pageSize === 0) return 0;
  if (entry.queryKey !== queryKey) return 0;

  return entry.requestedCount > pageSize ? entry.requestedCount : 0;
}

export function resolveRestoreScrollY(
  entry: ListingScrollEntry | null,
  queryKey: string
): number {
  if (!entry || entry.queryKey !== queryKey) return 0;
  return entry.scrollY;
}

export function resolveRestoreFeedToken(
  entry: ListingScrollEntry | null,
  queryKey: string
): string {
  if (!entry || entry.queryKey !== queryKey) return "";
  return entry.feedToken;
}

/**
 * 内容还没长到目标位置时滚过去只会停在底部，之后再补的内容也不会把
 * 视口推回原位。所以恢复动作要等文档高度够了才执行。
 */
export function canRestoreScrollY(input: {
  targetScrollY: number;
  documentHeight: number;
  viewportHeight: number;
}): boolean {
  if (input.targetScrollY <= 0) return true;
  return input.documentHeight - input.viewportHeight >= input.targetScrollY;
}

/**
 * 快照过期、视频被删除等情况可能让列表变短，此时停在仍可到达的位置。
 */
export function resolveReachableScrollY(input: {
  targetScrollY: number;
  documentHeight: number;
  viewportHeight: number;
}): number {
  const maxScrollY = Math.max(0, input.documentHeight - input.viewportHeight);
  return Math.max(0, Math.min(input.targetScrollY, maxScrollY));
}
