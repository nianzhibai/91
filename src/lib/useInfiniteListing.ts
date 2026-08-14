import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import {
  emptyInfiniteListingState,
  infiniteListingHasMore,
  infiniteListingReducer,
  nextListingRequest,
  type InfiniteListingState,
} from "@/lib/infiniteListing";
import type { InfiniteFeedSource } from "@/lib/infiniteFeedSource";
import type { VideoItem } from "@/types";

/**
 * 无限滚动的数据层：按 feed source 描述的方式一批批往后取，负责累积、去重、
 * 中断过期请求和会话内缓存。渲染窗口交给 VirtualVideoGrid，滚动现场交给
 * useListingScrollRestore。
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

export function clearInfiniteListingCache(key?: string) {
  if (key === undefined) {
    infiniteListingCache.clear();
    return;
  }
  infiniteListingCache.delete(key);
}

function cacheIsFresh(entry: CachedInfiniteListing, now: number): boolean {
  return now - entry.receivedAt < INFINITE_LISTING_CACHE_TTL_MS;
}

/**
 * 恢复现场的首个请求要对齐批边界，否则后续游标接不上（page/size 接口尤其
 * 如此）。不支持恢复的 feed 一律按普通首屏来。
 */
function initialBatchSize(restoreCount: number, batchSize: number): number {
  if (!Number.isInteger(restoreCount) || restoreCount <= batchSize) {
    return batchSize;
  }
  return Math.ceil(restoreCount / batchSize) * batchSize;
}

function errorValue(error: unknown): Error {
  return error instanceof Error ? error : new Error("视频列表加载失败");
}

function initialState(
  source: InfiniteFeedSource,
  enabled: boolean
): InfiniteListingState {
  const base = emptyInfiniteListingState(source.key, source.batchSize);
  if (!enabled) return base;
  const cached = infiniteListingCache.get(source.key) ?? null;
  if (cached && cacheIsFresh(cached, Date.now())) {
    return {
      ...base,
      items: cached.items,
      total: cached.total,
      requestedCount: cached.requestedCount,
      exhausted: cached.exhausted,
      status: "ready",
      receivedAt: cached.receivedAt,
    };
  }
  return { ...base, status: "initial-loading" };
}

export type UseInfiniteListingOptions = {
  enabled?: boolean;
  /** 后退回列表时要一次补回的条目数，0 表示普通首屏。 */
  restoreCount?: number;
};

export function useInfiniteListing(
  source: InfiniteFeedSource,
  options: UseInfiniteListingOptions = {}
) {
  const enabled = options.enabled ?? true;
  const key = source.key;
  const batchSize = source.batchSize;
  const [state, dispatch] = useReducer(infiniteListingReducer, undefined, () =>
    initialState(source, enabled)
  );
  const [reloadVersion, setReloadVersion] = useState(0);

  const nextRequestIDRef = useRef(0);
  const controllerRef = useRef<AbortController | null>(null);
  const stateRef = useRef(state);
  const sourceRef = useRef(source);
  const enabledRef = useRef(enabled);
  const restoreCountRef = useRef(options.restoreCount ?? 0);
  stateRef.current = state;
  sourceRef.current = source;
  enabledRef.current = enabled;
  restoreCountRef.current = options.restoreCount ?? 0;

  const sendRequest = useCallback(
    (
      requestID: number,
      feed: InfiniteFeedSource,
      request: { offset: number; size: number }
    ) => {
      const controller = new AbortController();
      controllerRef.current = controller;
      dispatch({ type: "load-start", requestID });
      feed
        .fetchBatch(request, { signal: controller.signal })
        .then((result) => {
          if (controller.signal.aborted) return;
          dispatch({
            type: "load-success",
            requestID,
            offset: request.offset,
            batchSize: request.size,
            items: result.items ?? [],
            total: result.total ?? 0,
            receivedAt: Date.now(),
            stopOnDuplicateBatch: feed.stopOnDuplicateBatch,
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
        pageSize: batchSize,
        items: cached.items,
        total: cached.total,
        requestedCount: cached.requestedCount,
        exhausted: cached.exhausted,
        receivedAt: cached.receivedAt,
      });
      return;
    }

    dispatch({ type: "reset", requestID, key, pageSize: batchSize });
    const restoreCount = sourceRef.current.supportsRestore
      ? restoreCountRef.current
      : 0;
    sendRequest(requestID, sourceRef.current, {
      offset: 0,
      size: initialBatchSize(restoreCount, batchSize),
    });

    return () => {
      controllerRef.current?.abort();
      controllerRef.current = null;
    };
  }, [batchSize, enabled, key, reloadVersion, sendRequest]);

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
    (batchOptions: { force?: boolean } = {}) => {
      if (!enabledRef.current) return;
      const current = stateRef.current;
      if (current.status === "initial-loading" || current.status === "loading-more") {
        return;
      }
      if (!batchOptions.force && current.status === "error") return;
      const request = nextListingRequest(current);
      if (!request) return;
      sendRequest(++nextRequestIDRef.current, sourceRef.current, request);
    },
    [sendRequest]
  );

  const loadMore = useCallback(() => requestBatch(), [requestBatch]);

  const reload = useCallback(() => {
    clearInfiniteListingCache(sourceRef.current.key);
    setReloadVersion((version) => version + 1);
  }, []);

  // 首屏失败要整段重来，尾部失败只重试失败的那一批，已加载内容保持不动。
  const retry = useCallback(() => {
    if (stateRef.current.items.length === 0) {
      reload();
      return;
    }
    requestBatch({ force: true });
  }, [reload, requestBatch]);

  const matchesQuery = state.key === key;
  const items = matchesQuery ? state.items : [];
  const initialLoading =
    enabled &&
    (!matchesQuery || (state.status === "initial-loading" && items.length === 0));

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
    reload,
    retry,
  };
}
