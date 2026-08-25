import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import type { VideoFeedCursor } from "@/data/videos";
import {
  emptyInfiniteListingState,
  infiniteListingCacheMatchesRestore,
  infiniteListingHasMore,
  infiniteListingReducer,
  nextListingRequest,
  type InfiniteListingState,
} from "@/lib/infiniteListing";
import type {
  InfiniteFeedRequest,
  InfiniteFeedSource,
} from "@/lib/infiniteFeedSource";
import type { VideoItem } from "@/types";

const INFINITE_LISTING_CACHE_TTL_MS = 60_000;
const INFINITE_LISTING_CACHE_MAX_ENTRIES = 8;
const MAX_INITIAL_BATCH_SIZE = 240;

const EMPTY_CURSOR: VideoFeedCursor = { feedToken: "", position: 0 };

type CachedInfiniteListing = {
  key: string;
  items: VideoItem[];
  total: number;
  feedToken: string;
  requestedCount: number;
  exhausted: boolean;
  receivedAt: number;
};

const infiniteListingCache = new Map<string, CachedInfiniteListing>();

function readInfiniteListingCache(
  key: string,
  restore: { feedToken: string; count: number }
): CachedInfiniteListing | null {
  const cached = infiniteListingCache.get(key) ?? null;
  if (!cached) return null;
  if (!cacheIsFresh(cached, Date.now())) {
    infiniteListingCache.delete(key);
    return null;
  }
  if (
    !infiniteListingCacheMatchesRestore({
      cachedFeedToken: cached.feedToken,
      cachedRequestedCount: cached.requestedCount,
      restoreFeedToken: restore.feedToken,
      restoreCount: restore.count,
    })
  ) {
    return null;
  }
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

function initialBatchSize(restoreCount: number, batchSize: number): number {
  const normalizedBatch = Math.max(1, Math.floor(batchSize));
  if (!Number.isInteger(restoreCount) || restoreCount <= normalizedBatch) {
    return normalizedBatch;
  }
  return Math.min(MAX_INITIAL_BATCH_SIZE, restoreCount);
}

function errorValue(error: unknown): Error {
  return error instanceof Error ? error : new Error("视频列表加载失败");
}

function initialState(
  source: InfiniteFeedSource,
  enabled: boolean,
  restore: { feedToken: string; count: number }
): InfiniteListingState {
  const base = emptyInfiniteListingState(source.key, source.batchSize);
  if (!enabled) return base;
  const cached = readInfiniteListingCache(source.key, restore);
  if (cached) {
    return {
      ...base,
      items: cached.items,
      total: cached.total,
      feedToken: cached.feedToken,
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
  restoreCount?: number;
  restoreFeedToken?: string;
};

export function useInfiniteListing(
  source: InfiniteFeedSource,
  options: UseInfiniteListingOptions = {}
) {
  const enabled = options.enabled ?? true;
  const key = source.key;
  const batchSize = source.batchSize;
  const [state, dispatch] = useReducer(infiniteListingReducer, undefined, () =>
    initialState(source, enabled, {
      feedToken: options.restoreFeedToken ?? "",
      count: options.restoreCount ?? 0,
    })
  );
  const [reloadVersion, setReloadVersion] = useState(0);

  const nextRequestIDRef = useRef(0);
  const controllerRef = useRef<AbortController | null>(null);
  const stateRef = useRef(state);
  const sourceRef = useRef(source);
  const enabledRef = useRef(enabled);
  const restoreCountRef = useRef(options.restoreCount ?? 0);
  const restoreFeedTokenRef = useRef(options.restoreFeedToken ?? "");
  const forceFreshRef = useRef(false);
  stateRef.current = state;
  sourceRef.current = source;
  enabledRef.current = enabled;
  restoreCountRef.current = options.restoreCount ?? 0;
  restoreFeedTokenRef.current = options.restoreFeedToken ?? "";

  const sendRequest = useCallback(
    function executeRequest(
      requestID: number,
      feed: InfiniteFeedSource,
      request: InfiniteFeedRequest
    ) {
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
            requestCursor: request.cursor,
            cursor: result.cursor,
            items: result.items ?? [],
            total: result.total ?? 0,
            exhausted: result.exhausted,
            receivedAt: Date.now(),
          });
        })
        .catch((error) => {
          if (controller.signal.aborted) return;
          if (request.cursor.feedToken && feed.isExpiredError(error)) {
            clearInfiniteListingCache(feed.key);
            if (controllerRef.current === controller) {
              controllerRef.current = null;
            }
            const restartID = ++nextRequestIDRef.current;
            const restoreCount = Math.max(
              request.cursor.position + request.size,
              stateRef.current.requestedCount
            );
            dispatch({
              type: "reset",
              requestID: restartID,
              key: feed.key,
              pageSize: feed.batchSize,
            });
            executeRequest(restartID, feed, {
              cursor: EMPTY_CURSOR,
              size: initialBatchSize(restoreCount, feed.batchSize),
            });
            return;
          }
          dispatch({ type: "load-failure", requestID, error: errorValue(error) });
        })
        .finally(() => {
          if (controllerRef.current === controller) {
            controllerRef.current = null;
          }
        });
    },
    []
  );

  useEffect(() => {
    const requestID = ++nextRequestIDRef.current;
    controllerRef.current?.abort();
    controllerRef.current = null;

    if (!enabled) {
      dispatch({ type: "disable", requestID, key, pageSize: batchSize });
      return;
    }

    const forceFresh = forceFreshRef.current;
    forceFreshRef.current = false;
    const restoreCount = restoreCountRef.current;
    const restoreFeedToken = restoreFeedTokenRef.current;
    const cached = forceFresh
      ? null
      : readInfiniteListingCache(key, {
          feedToken: restoreFeedToken,
          count: restoreCount,
        });
    if (cached) {
      dispatch({
        type: "hydrate",
        requestID,
        key,
        pageSize: batchSize,
        items: cached.items,
        total: cached.total,
        feedToken: cached.feedToken,
        requestedCount: cached.requestedCount,
        exhausted: cached.exhausted,
        receivedAt: cached.receivedAt,
      });
      return;
    }

    const restoreCursor: VideoFeedCursor = forceFresh
      ? EMPTY_CURSOR
      : { feedToken: restoreFeedToken, position: 0 };
    dispatch({
      type: "reset",
      requestID,
      key,
      pageSize: batchSize,
      cursor: restoreCursor,
    });
    sendRequest(requestID, sourceRef.current, {
      cursor: restoreCursor,
      size: initialBatchSize(restoreCount, batchSize),
    });

    return () => {
      controllerRef.current?.abort();
      controllerRef.current = null;
    };
  }, [batchSize, enabled, key, reloadVersion, sendRequest]);

  useEffect(() => {
    if (!enabled || state.key !== key) return;
    if (state.status !== "ready" || state.items.length === 0) return;
    writeInfiniteListingCache({
      key,
      items: state.items,
      total: state.total,
      feedToken: state.feedToken,
      requestedCount: state.requestedCount,
      exhausted: state.exhausted,
      receivedAt: state.receivedAt,
    });
  }, [enabled, key, state]);

  const requestBatch = useCallback(
    (batchOptions: { force?: boolean } = {}) => {
      if (!enabledRef.current) return;
      if (controllerRef.current && !controllerRef.current.signal.aborted) return;
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
    controllerRef.current?.abort();
    controllerRef.current = null;
    forceFreshRef.current = true;
    clearInfiniteListingCache(sourceRef.current.key);
    setReloadVersion((version) => version + 1);
  }, []);

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
    feedToken: matchesQuery ? state.feedToken : "",
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
