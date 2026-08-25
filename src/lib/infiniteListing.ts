import type { VideoFeedCursor } from "@/data/videos";
import type { SortKey, VideoItem } from "@/types";

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
};

export type InfiniteListingState = {
  key: string;
  requestID: number;
  pageSize: number;
  items: VideoItem[];
  total: number;
  feedToken: string;
  /** 下一批在服务端快照中的位置；可能大于 items.length（快照项后来被删除）。 */
  requestedCount: number;
  exhausted: boolean;
  status: InfiniteListingStatus;
  error: Error | null;
  receivedAt: number;
};

export type InfiniteListingAction =
  | { type: "disable"; requestID: number; key: string; pageSize: number }
  | {
      type: "reset";
      requestID: number;
      key: string;
      pageSize: number;
      cursor?: VideoFeedCursor;
    }
  | {
      type: "hydrate";
      requestID: number;
      key: string;
      pageSize: number;
      items: VideoItem[];
      total: number;
      feedToken: string;
      requestedCount: number;
      exhausted: boolean;
      receivedAt: number;
    }
  | { type: "load-start"; requestID: number }
  | {
      type: "load-success";
      requestID: number;
      requestCursor: VideoFeedCursor;
      cursor: VideoFeedCursor;
      items: VideoItem[];
      total: number;
      exhausted: boolean;
      receivedAt: number;
    }
  | { type: "load-failure"; requestID: number; error: Error };

/** Batch size is a transport concern, not part of a logical result-set key. */
export function infiniteListingKey(query: InfiniteListingQuery): string {
  return JSON.stringify([query.q.trim(), query.tag.trim(), query.sort]);
}

export function infiniteListingCacheMatchesRestore(input: {
  cachedFeedToken: string;
  cachedRequestedCount: number;
  restoreFeedToken: string;
  restoreCount: number;
}): boolean {
  if (
    input.restoreFeedToken &&
    input.cachedFeedToken !== input.restoreFeedToken
  ) {
    return false;
  }
  if (!Number.isInteger(input.restoreCount) || input.restoreCount <= 0) {
    return true;
  }
  return input.cachedRequestedCount >= input.restoreCount;
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
    feedToken: "",
    requestedCount: 0,
    exhausted: false,
    status: "idle",
    error: null,
    receivedAt: 0,
  };
}

/** Defensive identity merge; a correctly formed snapshot already has unique IDs. */
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
  return fresh.length === 0 ? previous : [...previous, ...fresh];
}

export function infiniteListingReducer(
  state: InfiniteListingState,
  action: InfiniteListingAction
): InfiniteListingState {
  switch (action.type) {
    case "disable":
      return {
        ...emptyInfiniteListingState(action.key, action.pageSize),
        requestID: action.requestID,
      };
    case "reset":
      return {
        ...emptyInfiniteListingState(action.key, action.pageSize),
        requestID: action.requestID,
        feedToken: action.cursor?.feedToken ?? "",
        requestedCount: action.cursor?.position ?? 0,
      };
    case "hydrate":
      return {
        key: action.key,
        requestID: action.requestID,
        pageSize: action.pageSize,
        items: action.items,
        total: action.total,
        feedToken: action.feedToken,
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
      if (
        action.requestCursor.feedToken !== state.feedToken ||
        action.requestCursor.position !== state.requestedCount
      ) {
        return state;
      }
      const items = appendUniqueVideos(state.items, action.items);
      return {
        ...state,
        items,
        total: action.total,
        feedToken: action.cursor.feedToken,
        requestedCount: action.cursor.position,
        exhausted: action.exhausted,
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

export type InfiniteListingRequest = {
  cursor: VideoFeedCursor;
  size: number;
};

export function nextListingRequest(
  state: InfiniteListingState
): InfiniteListingRequest | null {
  if (state.exhausted) return null;
  if (!Number.isInteger(state.pageSize) || state.pageSize <= 0) return null;
  return {
    cursor: {
      feedToken: state.feedToken,
      position: state.requestedCount,
    },
    size: state.pageSize,
  };
}

export function infiniteListingHasMore(state: InfiniteListingState): boolean {
  return !state.exhausted && state.status !== "error";
}
