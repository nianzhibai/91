import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { fetchListing } from "@/data/videos";
import {
  emptyInfiniteListingState,
  infiniteListingHasMore,
  infiniteListingKey,
  infiniteListingReducer,
  nextListingRequest,
  type InfiniteListingQuery,
  type InfiniteListingState,
} from "@/lib/infiniteListing";
import type { VideoItem } from "@/types";

/**
 * 无限滚动列表的数据层：只负责"按游标往后追加"和会话内的缓存，
 * 渲染窗口交给 VirtualVideoGrid，滚动现场交给 useListingScrollRestore。
 */

const INFINITE_LISTING_CACHE_TTL_MS = 60_000;
const INFINITE_LISTING_CACHE_MAX_ENTRIES = 8;

type CachedInfiniteListing = {
  key: string;
  items: VideoItem[];
  total: number;
  requestedCount: number;
  exhausted: boolean;
  receivedAt: number;
};

const infiniteListingCache = new Map<string, CachedInfiniteListing>();

function readInfiniteListingCache(key: string): CachedInfiniteListing | null {
  const cached = infiniteListingCache.get(key) ?? null;
  if (!cached) return null;
  infiniteListingCache.delete(key);
  infiniteListingCache.set(key, cached);
  return cached;
}

function writeInfiniteListingCache(entry: CachedInfiniteListing) {
  infiniteListingCache.delete(entry.key);
  infiniteListingCache.set(entry.key, entry);
  while (infiniteListingCache.size > INFINITE_LISTING_CACHE_MAX_ENTRIES) {
    const oldestKey = infiniteListingCache.keys().next().value as
      | string
      | undefined;
    if (!oldestKey) break;
    infiniteListingCache.delete(oldestKey);
  }
}

export function clearInfiniteListingCache() {
  infiniteListingCache.clear();
}

function cacheIsFresh(entry: CachedInfiniteListing, now: number): boolean {
  return now - entry.receivedAt < INFINITE_LISTING_CACHE_TTL_MS;
}

function normalizeQuery(query: InfiniteListingQuery): InfiniteListingQuery {
  return {
    q: query.q.trim(),
    tag: query.tag.trim(),
    sort: query.sort,
    pageSize:
      Number.isInteger(query.pageSize) && query.pageSize > 0
        ? query.pageSize
        : 1,
  };
}

/**
 * 恢复现场的首个请求要对齐页边界，否则后续 page/size 分页无法接着这个偏移量走。
 */
function initialBatchSize(restoreCount: number, pageSize: number): number {
  if (!Number.isInteger(restoreCount) || restoreCount <= pageSize) {
    return pageSize;
  }
  return Math.ceil(restoreCount / pageSize) * pageSize;
}

function errorValue(error: unknown): Error {
  return error instanceof Error ? error : new Error("视频列表加载失败");
}

function hydratedState(
  key: string,
  pageSize: number,
  cached: CachedInfiniteListing
): InfiniteListingState {
  return {
    key,
    requestID: 0,
    pageSize,
    items: cached.items,
    total: cached.total,
    requestedCount: cached.requestedCount,
    exhausted: cached.exhausted,
    status: "ready",
    error: null,
    receivedAt: cached.receivedAt,
  };
}

function initialState(
  key: string,
  query: InfiniteListingQuery,
  enabled: boolean
): InfiniteListingState {
  const base = emptyInfiniteListingState(key, query.pageSize);
  if (!enabled) return base;
  const cached = infiniteListingCache.get(key) ?? null;
  if (cached && cacheIsFresh(cached, Date.now())) {
    return hydratedState(key, query.pageSize, cached);
  }
  return { ...base, status: "initial-loading" };
}

export type UseInfiniteListingInput = InfiniteListingQuery & {
  enabled?: boolean;
  /** 后退回列表时要一次补回的条目数，0 表示普通首屏。 */
  restoreCount?: number;
};

export function useInfiniteListing(input: UseInfiniteListingInput) {
  const enabled = input.enabled ?? true;
  const query = useMemo(
    () => normalizeQuery(input),
    [input.q, input.tag, input.sort, input.pageSize]
  );
  const key = infiniteListingKey(query);
  const [state, dispatch] = useReducer(infiniteListingReducer, undefined, () =>
    initialState(key, query, enabled)
  );
  const [retryVersion, setRetryVersion] = useState(0);

  const nextRequestIDRef = useRef(0);
  const controllerRef = useRef<AbortController | null>(null);
  const stateRef = useRef(state);
  const queryRef = useRef(query);
  const enabledRef = useRef(enabled);
  const restoreCountRef = useRef(input.restoreCount ?? 0);
  stateRef.current = state;
  queryRef.current = query;
  enabledRef.current = enabled;
  restoreCountRef.current = input.restoreCount ?? 0;

  const sendRequest = useCallback(
    (
      requestID: number,
      requestQuery: InfiniteListingQuery,
      request: { page: number; size: number },
      offset: number
    ) => {
      const controller = new AbortController();
      controllerRef.current = controller;
      dispatch({ type: "load-start", requestID });
      fetchListing(
        request.page,
        request.size,
        { q: requestQuery.q, tag: requestQuery.tag, sort: requestQuery.sort },
        { signal: controller.signal }
      )
        .then((result) => {
          if (controller.signal.aborted) return;
          dispatch({
            type: "load-success",
            requestID,
            offset,
            batchSize: request.size,
            items: result.items ?? [],
            total: result.total ?? 0,
            receivedAt: Date.now(),
          });
        })
        .catch((error) => {
          if (controller.signal.aborted) return;
          dispatch({ type: "load-failure", requestID, error: errorValue(error) });
        });
    },
    []
  );

  useEffect(() => {
    const requestID = ++nextRequestIDRef.current;
    controllerRef.current?.abort();
    controllerRef.current = null;

    if (!enabled) {
      dispatch({ type: "disable", requestID });
      return;
    }

    const cached = readInfiniteListingCache(key);
    if (cached && cacheIsFresh(cached, Date.now())) {
      dispatch({
        type: "hydrate",
        requestID,
        key,
        pageSize: query.pageSize,
        items: cached.items,
        total: cached.total,
        requestedCount: cached.requestedCount,
        exhausted: cached.exhausted,
        receivedAt: cached.receivedAt,
      });
      return;
    }

    dispatch({ type: "reset", requestID, key, pageSize: query.pageSize });
    sendRequest(
      requestID,
      query,
      { page: 1, size: initialBatchSize(restoreCountRef.current, query.pageSize) },
      0
    );

    return () => {
      controllerRef.current?.abort();
      controllerRef.current = null;
    };
  }, [enabled, key, query, retryVersion, sendRequest]);

  // 会话内缓存以真实响应时间为准：hydrate 回来的状态不会自我续命。
  useEffect(() => {
    if (!enabled || state.key !== key) return;
    if (state.status !== "ready" || state.items.length === 0) return;
    writeInfiniteListingCache({
      key,
      items: state.items,
      total: state.total,
      requestedCount: state.requestedCount,
      exhausted: state.exhausted,
      receivedAt: state.receivedAt,
    });
  }, [enabled, key, state]);

  const requestBatch = useCallback(
    (options: { force?: boolean } = {}) => {
      if (!enabledRef.current) return;
      const current = stateRef.current;
      if (current.status === "initial-loading" || current.status === "loading-more") {
        return;
      }
      if (!options.force && current.status === "error") return;
      const request = nextListingRequest(current);
      if (!request) return;
      sendRequest(
        ++nextRequestIDRef.current,
        queryRef.current,
        request,
        current.requestedCount
      );
    },
    [sendRequest]
  );

  const loadMore = useCallback(() => requestBatch(), [requestBatch]);

  // 首屏失败要整段重来，尾部失败只重试失败的那一批，已加载内容保持不动。
  const retry = useCallback(() => {
    if (stateRef.current.items.length === 0) {
      setRetryVersion((version) => version + 1);
      return;
    }
    requestBatch({ force: true });
  }, [requestBatch]);

  const matchesQuery = state.key === key;
  const items = matchesQuery ? state.items : [];
  const initialLoading =
    enabled && (!matchesQuery || (state.status === "initial-loading" && items.length === 0));

  return {
    items,
    total: matchesQuery ? state.total : 0,
    status: state.status,
    error: state.error,
    initialLoading,
    loadingMore: matchesQuery && state.status === "loading-more",
    failed: matchesQuery && state.status === "error",
    exhausted: matchesQuery && state.exhausted,
    hasMore: matchesQuery && infiniteListingHasMore(state),
    requestedCount: matchesQuery ? state.requestedCount : 0,
    loadMore,
    retry,
  };
}
