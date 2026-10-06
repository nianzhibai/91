import type { ShortsFeedItem, ShortsNextResponse } from "@/data/videos";

// 只保存固定大小的服务端 feed 令牌和已实际看到的游标。
export const SHORTS_FEED_STORAGE_KEY = "shorts_feed_v2";

export const SHORTS_FEED_TABS = [
  { key: "latest", label: "最新" },
  { key: "hot", label: "最热" },
  { key: "recommend", label: "推荐" },
] as const;

export type ShortsFeedMode = (typeof SHORTS_FEED_TABS)[number]["key"];

export function readShortsFeedMode(params: URLSearchParams): ShortsFeedMode {
  const mode = params.get("feed");
  return mode === "latest" || mode === "hot" ? mode : "recommend";
}

export function withShortsFeedMode(
  params: URLSearchParams,
  mode: ShortsFeedMode
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (mode === "recommend") next.delete("feed");
  else next.set("feed", mode);
  return next;
}

function shortsFeedStorageKey(mode: ShortsFeedMode): string {
  return mode === "recommend" ? SHORTS_FEED_STORAGE_KEY : `${SHORTS_FEED_STORAGE_KEY}_${mode}`;
}

// 每次向后端取多少条续到队列尾。值不要太大避免一次返回过多浪费；
// 也不要太小导致频繁请求和滑动卡顿。
export const BATCH_SIZE = 5;
export const INITIAL_BATCH_SIZE = 2;

// 当队列里"还没看过的视频"少于这个数时，提前请求下一批。
export const PREFETCH_THRESHOLD = 2;
export const MAX_SHORTS_QUEUE_ITEMS = 60;
export const SHORTS_QUEUE_KEEP_BEHIND = 20;

export type ShortsFeedState = {
  feedToken: string;
  cursor: number;
};

export type QueuedShortsItem = ShortsFeedItem & {
  feedToken: string;
};

export const EMPTY_SHORTS_FEED: ShortsFeedState = { feedToken: "", cursor: 0 };

export function shortsQueueItemKey(item: {
  feedToken: string;
  feedCursor: number;
}) {
  return `${item.feedToken}:${item.feedCursor}`;
}

export function loadShortsFeedState(mode: ShortsFeedMode = "recommend"): ShortsFeedState {
  try {
    const raw = localStorage.getItem(shortsFeedStorageKey(mode));
    if (!raw) return EMPTY_SHORTS_FEED;
    const parsed = JSON.parse(raw);
    if (
      !parsed ||
      typeof parsed.feedToken !== "string" ||
      parsed.feedToken.length === 0 ||
      parsed.feedToken.length > 128 ||
      !Number.isInteger(parsed.cursor) ||
      parsed.cursor < 0
    ) {
      return EMPTY_SHORTS_FEED;
    }
    return { feedToken: parsed.feedToken, cursor: parsed.cursor };
  } catch {
    return EMPTY_SHORTS_FEED;
  }
}

export function saveShortsFeedState(feed: ShortsFeedState, mode: ShortsFeedMode = "recommend") {
  try {
    localStorage.setItem(shortsFeedStorageKey(mode), JSON.stringify(feed));
  } catch {
    // 隐私模式或存储不可用时只影响刷新后的续播，不影响当前 feed。
  }
}

export function clearShortsFeedState(mode: ShortsFeedMode = "recommend") {
  try {
    localStorage.removeItem(shortsFeedStorageKey(mode));
  } catch {
    // ignore
  }
}

/**
 * 把新一批响应追加到队列尾部，并按 feedToken:feedCursor 去重，
 * 保证重复请求（如 StrictMode 双跑）下幂等。
 */
export function mergeShortsQueue(
  prev: QueuedShortsItem[],
  resp: ShortsNextResponse
): QueuedShortsItem[] {
  const existing = new Set(prev.map(shortsQueueItemKey));
  const fresh = resp.items
    .map((item) => ({ ...item, feedToken: resp.feedToken }))
    .filter((item) => !existing.has(shortsQueueItemKey(item)));
  return [...prev, ...fresh];
}

export type ShortsPrefetchPlan = "none" | "load" | "new-round";

/**
 * 决定当前是否需要请求下一批：
 * - 未看数量仍然充足、或正在加载/加载失败时不动作；
 * - 本轮已耗尽时，最后一批仍在队列中就不提前换轮；真正滑到最后一条
 *   才丢弃旧令牌、开新一轮随机 feed。
 */
export function planShortsPrefetch(input: {
  remainingAfterActive: number;
  loading: boolean;
  loadError: boolean;
  roundComplete: boolean;
}): ShortsPrefetchPlan {
  if (
    input.remainingAfterActive >= PREFETCH_THRESHOLD ||
    input.loading ||
    input.loadError
  ) {
    return "none";
  }
  if (input.roundComplete) {
    return input.remainingAfterActive > 0 ? "none" : "new-round";
  }
  return "load";
}

/**
 * 长会话只在队列超限且已经留出足够回看历史时整批裁剪。返回从头部删除
 * 的条数；裁剪后当前项会稳定落在 SHORTS_QUEUE_KEEP_BEHIND。
 */
export function getShortsQueueTrimCount(
  activeIndex: number,
  itemCount: number
): number {
  if (
    itemCount <= MAX_SHORTS_QUEUE_ITEMS ||
    activeIndex <= SHORTS_QUEUE_KEEP_BEHIND
  ) {
    return 0;
  }
  return activeIndex - SHORTS_QUEUE_KEEP_BEHIND;
}

export type FetchShortsNextFn = (
  feedToken: string,
  cursor: number,
  count: number
) => Promise<ShortsNextResponse>;

/** 请求游标变更的原因：令牌失效重开 / 正常推进 / 快照被清空后换新一轮。 */
export type ShortsFeedCommitEvent = "expired" | "advanced" | "snapshot-drained";

export type ShortsBatchOutcome =
  | { kind: "empty" }
  | { kind: "batch"; response: ShortsNextResponse };

/**
 * 向后端 token/cursor feed 请求下一批视频。令牌因后端重启或超时失效时
 * 自动开新一轮；快照里剩下的视频全部被删除/隐藏时换新快照重试。只有
 * 真实空库才返回 empty。每次请求游标变化都先经 commitFeed 通知调用方，
 * 让内存中的续播游标与这里的恢复流程保持同步。
 */
export async function requestShortsBatch(options: {
  feed: ShortsFeedState;
  count: number;
  fetchNext: FetchShortsNextFn;
  isFeedExpiredError: (error: unknown) => boolean;
  commitFeed: (feed: ShortsFeedState, event: ShortsFeedCommitEvent) => void;
}): Promise<ShortsBatchOutcome> {
  let requestFeed = options.feed;
  for (let recoveryAttempt = 0; recoveryAttempt < 3; recoveryAttempt += 1) {
    let resp: ShortsNextResponse;
    try {
      resp = await options.fetchNext(
        requestFeed.feedToken,
        requestFeed.cursor,
        options.count
      );
    } catch (error) {
      if (options.isFeedExpiredError(error) && requestFeed.feedToken) {
        requestFeed = EMPTY_SHORTS_FEED;
        options.commitFeed(requestFeed, "expired");
        continue;
      }
      throw error;
    }

    if (resp.total === 0) {
      return { kind: "empty" };
    }

    requestFeed = {
      feedToken: resp.feedToken,
      cursor: resp.nextCursor,
    };
    options.commitFeed(requestFeed, "advanced");

    // A snapshot can become empty if its remaining videos were deleted or
    // hidden. Start a fresh snapshot instead of showing an empty-library lie.
    if (resp.items.length === 0 && resp.roundComplete) {
      requestFeed = EMPTY_SHORTS_FEED;
      options.commitFeed(requestFeed, "snapshot-drained");
      continue;
    }
    if (resp.items.length === 0) {
      throw new Error("Shorts feed returned no items before completion");
    }
    return { kind: "batch", response: resp };
  }
  throw new Error("Unable to create a playable shorts feed");
}
