import type { SortKey, VideoItem } from "@/types";

/**
 * 无限滚动列表的纯状态层。分页游标按"已请求条目数"推进而不是按"已渲染条目数"，
 * 否则后端去重/隐藏行会让偏移量漂移，越翻越错位。
 */

export type InfiniteListingStatus =
  | "idle"
  | "initial-loading"
  | "ready"
  | "loading-more"
  | "error";

export type InfiniteListingQuery = {
  q: string;
  tag: string;
  sort: SortKey;
  pageSize: number;
};

export type InfiniteListingState = {
  key: string;
  requestID: number;
  pageSize: number;
  items: VideoItem[];
  total: number;
  /** 已经向后端请求过的条目数，等于下一批的偏移量。 */
  requestedCount: number;
  /** 后端已经没有更多数据，停止再触发加载。 */
  exhausted: boolean;
  status: InfiniteListingStatus;
  error: Error | null;
  /** 最近一次真实响应的时间，缓存新鲜度以它为准。 */
  receivedAt: number;
};

export type InfiniteListingAction =
  | { type: "disable"; requestID: number }
  | { type: "reset"; requestID: number; key: string; pageSize: number }
  | {
      type: "hydrate";
      requestID: number;
      key: string;
      pageSize: number;
      items: VideoItem[];
      total: number;
      requestedCount: number;
      exhausted: boolean;
      receivedAt: number;
    }
  | { type: "load-start"; requestID: number }
  | {
      type: "load-success";
      requestID: number;
      /** 本批请求时的偏移量，用于丢弃与当前游标不衔接的响应。 */
      offset: number;
      batchSize: number;
      items: VideoItem[];
      total: number;
      receivedAt: number;
      /** 服务端轮换 feed 没有终点，整批都是重复内容时就收尾。 */
      stopOnDuplicateBatch?: boolean;
    }
  | { type: "load-failure"; requestID: number; error: Error };

export function infiniteListingKey(query: InfiniteListingQuery): string {
  return JSON.stringify([
    query.q.trim(),
    query.tag.trim(),
    query.sort,
    Number.isInteger(query.pageSize) && query.pageSize > 0 ? query.pageSize : 1,
  ]);
}

export function emptyInfiniteListingState(
  key: string,
  pageSize: number
): InfiniteListingState {
  return {
    key,
    requestID: 0,
    pageSize,
    items: [],
    total: 0,
    requestedCount: 0,
    exhausted: false,
    status: "idle",
    error: null,
    receivedAt: 0,
  };
}

/**
 * 分页边界会随新入库/删除的视频移动，同一条视频可能出现在相邻两页里。
 * 追加时按 id 去重，既修掉重复卡片，也让 StrictMode 的重复请求幂等。
 */
export function appendUniqueVideos(
  previous: VideoItem[],
  incoming: VideoItem[]
): VideoItem[] {
  if (incoming.length === 0) return previous;
  const seen = new Set(previous.map((item) => item.id));
  const fresh = incoming.filter((item) => {
    if (!item || seen.has(item.id)) return false;
    seen.add(item.id);
    return true;
  });
  if (fresh.length === 0) return previous;
  return [...previous, ...fresh];
}

export function infiniteListingReducer(
  state: InfiniteListingState,
  action: InfiniteListingAction
): InfiniteListingState {
  switch (action.type) {
    case "disable":
      return {
        ...emptyInfiniteListingState(state.key, state.pageSize),
        requestID: action.requestID,
      };
    case "reset":
      return {
        ...emptyInfiniteListingState(action.key, action.pageSize),
        requestID: action.requestID,
      };
    case "hydrate":
      return {
        key: action.key,
        requestID: action.requestID,
        pageSize: action.pageSize,
        items: action.items,
        total: action.total,
        requestedCount: action.requestedCount,
        exhausted: action.exhausted,
        status: "ready",
        error: null,
        receivedAt: action.receivedAt,
      };
    case "load-start":
      return {
        ...state,
        requestID: action.requestID,
        status: state.items.length > 0 ? "loading-more" : "initial-loading",
        error: null,
      };
    case "load-success": {
      if (action.requestID !== state.requestID) return state;
      // 游标不衔接说明这批响应属于已经作废的加载序列。
      if (action.offset !== state.requestedCount) return state;
      const items = appendUniqueVideos(state.items, action.items);
      const requestedCount = action.offset + action.batchSize;
      const total = action.total > 0 ? action.total : state.total;
      return {
        ...state,
        items,
        total,
        requestedCount,
        exhausted: isListingExhausted({
          received: action.items.length,
          added: items.length - state.items.length,
          batchSize: action.batchSize,
          requestedCount,
          total: action.total,
          stopOnDuplicateBatch: action.stopOnDuplicateBatch,
        }),
        status: "ready",
        error: null,
        receivedAt: action.receivedAt,
      };
    }
    case "load-failure":
      if (action.requestID !== state.requestID) return state;
      return { ...state, status: "error", error: action.error };
  }
}

/**
 * 两个终止条件缺一不可：total 只是查询时刻的计数，删除/隐藏后会虚高，
 * 只靠它会在尾部反复请求空页；只靠"返回不足一页"则会漏掉整除的边界。
 */
export function isListingExhausted(input: {
  received: number;
  /** 去重后真正新增的条数。 */
  added: number;
  batchSize: number;
  requestedCount: number;
  total: number;
  /** 轮换 feed 没有 total，也永远返回满批，只能靠"整批都重复"收尾。 */
  stopOnDuplicateBatch?: boolean;
}): boolean {
  if (input.received < input.batchSize) return true;
  if (input.stopOnDuplicateBatch && input.added === 0) return true;
  return input.total > 0 && input.requestedCount >= input.total;
}

export type InfiniteListingRequest = { offset: number; size: number };

/** 下一批要取的区间；具体怎么翻页由各个 feed source 自己决定。 */
export function nextListingRequest(
  state: InfiniteListingState
): InfiniteListingRequest | null {
  if (state.exhausted) return null;
  const size = state.pageSize;
  if (!Number.isInteger(size) || size <= 0) return null;
  return { offset: state.requestedCount, size };
}

export function infiniteListingHasMore(state: InfiniteListingState): boolean {
  return !state.exhausted && state.status !== "error";
}
