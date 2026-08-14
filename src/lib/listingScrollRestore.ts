/**
 * 前进/后退时恢复无限滚动列表的现场。历史条目自己的 key 作为存储键，
 * 所以"后退回列表"能拿回当时的滚动位置，而"重新点进列表"是干净的新会话。
 */

export const LISTING_SCROLL_STORAGE_PREFIX = "listing_scroll_v1:";

/** 恢复现场时一次最多补回多少条，避免深滚后的返回打出一个超大请求。 */
export const MAX_RESTORE_ITEMS = 240;

export type ListingScrollEntry = {
  queryKey: string;
  /** 保存现场时已经请求过的条目数。 */
  requestedCount: number;
  scrollY: number;
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
      !Number.isInteger(parsed.requestedCount) ||
      parsed.requestedCount <= 0 ||
      !Number.isFinite(parsed.scrollY) ||
      parsed.scrollY < 0
    ) {
      return null;
    }
    return {
      queryKey: parsed.queryKey,
      requestedCount: parsed.requestedCount,
      scrollY: parsed.scrollY,
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
    return parseListingScrollEntry(storage.getItem(listingScrollStorageKey(historyKey)));
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
 * 恢复现场时首个请求要取多少条：把保存的进度向上取整到页大小的整数倍，
 * 这样后续分页游标仍然落在页边界上；超过上限就只补到上限。
 * 返回 0 表示按普通首屏加载。
 */
export function resolveRestoreCount(input: {
  entry: ListingScrollEntry | null;
  queryKey: string;
  pageSize: number;
  maxItems?: number;
}): number {
  const { entry, queryKey } = input;
  const pageSize =
    Number.isInteger(input.pageSize) && input.pageSize > 0 ? input.pageSize : 0;
  if (!entry || pageSize === 0) return 0;
  if (entry.queryKey !== queryKey) return 0;

  const maxItems = input.maxItems ?? MAX_RESTORE_ITEMS;
  const capped = Math.min(entry.requestedCount, Math.max(maxItems, pageSize));
  const rounded = Math.ceil(capped / pageSize) * pageSize;
  return rounded > pageSize ? rounded : 0;
}

export function resolveRestoreScrollY(
  entry: ListingScrollEntry | null,
  queryKey: string
): number {
  if (!entry || entry.queryKey !== queryKey) return 0;
  return entry.scrollY;
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
 * 保存的位置比恢复上限还深时（滚过 MAX_RESTORE_ITEMS 条之后返回），退而
 * 求其次滚到能到达的最远处：停在已恢复内容的末尾，比停在顶部更接近原位，
 * 继续往下滚也能无缝接上后续批次。
 */
export function resolveReachableScrollY(input: {
  targetScrollY: number;
  documentHeight: number;
  viewportHeight: number;
}): number {
  const maxScrollY = Math.max(0, input.documentHeight - input.viewportHeight);
  return Math.max(0, Math.min(input.targetScrollY, maxScrollY));
}
