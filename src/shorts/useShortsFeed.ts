import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { fetchShortsNext, ShortsFeedExpiredError } from "@/data/videos";
import {
  BATCH_SIZE,
  clearShortsFeedState,
  EMPTY_SHORTS_FEED,
  INITIAL_BATCH_SIZE,
  loadShortsFeedState,
  mergeShortsQueue,
  planShortsPrefetch,
  requestShortsBatch,
  saveShortsFeedState,
  type QueuedShortsItem,
  type ShortsFeedMode,
  type ShortsFeedState,
} from "./shortsFeed";

const SHORTS_FEED_SAVE_DELAY_MS = 500;

/**
 * 短视频队列：向服务端 token/cursor feed 拉取批次，维护页面内的视频
 * 队列、续播书签与预取节奏。activeIndex 是当前视口内的视频索引；
 * 队列因空库被丢弃时会调用 onQueueReset，让页面把 activeIndex 归零。
 */
export function useShortsFeed(
  activeIndex: number,
  onQueueReset: () => void,
  mode: ShortsFeedMode = "recommend"
) {
  // 已加入页面的视频队列（按出现顺序）
  const [items, setItems] = useState<QueuedShortsItem[]>([]);
  const [queueMode, setQueueMode] = useState(mode);
  const modeRef = useRef(mode);
  // 是否正在加载下一批，避免并发请求
  const [loading, setLoading] = useState(false);
  const loadingRef = useRef(false);
  const requestGenerationRef = useRef(0);
  const requestAbortRef = useRef<AbortController | null>(null);
  const hasLoadedBatchRef = useRef(false);
  // 后端报告"本轮已耗尽"，下次请求前会自动重置
  const [roundComplete, setRoundComplete] = useState(false);
  // 没有任何视频可放（库为空 / 全部隐藏）
  const [empty, setEmpty] = useState(false);
  // 请求失败和真实空库必须分开，不能再把断网误报为"没有视频"。
  const [loadError, setLoadError] = useState(false);
  const [initialFeedState] = useState(() => loadShortsFeedState(mode));
  // 指向已经取到队列尾部的位置；只在内存中预取，不直接写 localStorage。
  const requestFeedRef = useRef<ShortsFeedState>(initialFeedState);
  // 已写书签的最远逻辑队列位置。队首裁剪时 queueStartOffset 会前移，因此
  // 同一个视频的逻辑位置不变；这样旧/new commit 的 passive effect 即使相邻
  // 执行，也不会把已观看水位写回裁剪前的局部 index。
  const persistedFeedHighPositionRef = useRef(-1);
  const queueStartOffsetRef = useRef(0);
  const queueStartOffset = queueStartOffsetRef.current;
  const pendingPersistedFeedRef = useRef<{ feed: ShortsFeedState; mode: ShortsFeedMode } | null>(null);
  const persistedFeedWriteTimerRef = useRef<number | null>(null);
  const onQueueResetRef = useRef(onQueueReset);
  onQueueResetRef.current = onQueueReset;

  const cancelPendingPersistedFeed = useCallback(() => {
    if (persistedFeedWriteTimerRef.current !== null) {
      window.clearTimeout(persistedFeedWriteTimerRef.current);
      persistedFeedWriteTimerRef.current = null;
    }
    pendingPersistedFeedRef.current = null;
  }, []);

  const flushPersistedFeed = useCallback(() => {
    if (persistedFeedWriteTimerRef.current !== null) {
      window.clearTimeout(persistedFeedWriteTimerRef.current);
      persistedFeedWriteTimerRef.current = null;
    }
    const pending = pendingPersistedFeedRef.current;
    pendingPersistedFeedRef.current = null;
    if (pending) saveShortsFeedState(pending.feed, pending.mode);
  }, []);

  const schedulePersistedFeed = useCallback(
    (feed: ShortsFeedState) => {
      pendingPersistedFeedRef.current = { feed, mode };
      if (persistedFeedWriteTimerRef.current !== null) {
        window.clearTimeout(persistedFeedWriteTimerRef.current);
      }
      persistedFeedWriteTimerRef.current = window.setTimeout(
        flushPersistedFeed,
        SHORTS_FEED_SAVE_DELAY_MS
      );
    },
    [flushPersistedFeed, mode]
  );

  // 路由离开和页面进入后台时补写最后一个已观看游标，既避开切屏热路径，
  // 也不会因为 debounce 丢掉续播书签。
  useEffect(() => {
    window.addEventListener("pagehide", flushPersistedFeed);
    return () => {
      window.removeEventListener("pagehide", flushPersistedFeed);
      flushPersistedFeed();
    };
  }, [flushPersistedFeed]);

  const loadMore = useCallback(async () => {
    if (loadingRef.current) return;
    const generation = requestGenerationRef.current;
    const controller = new AbortController();
    requestAbortRef.current = controller;
    loadingRef.current = true;
    setLoading(true);
    setLoadError(false);
    try {
      const outcome = await requestShortsBatch({
        feed: requestFeedRef.current,
        count: hasLoadedBatchRef.current ? BATCH_SIZE : INITIAL_BATCH_SIZE,
        fetchNext: (token, cursor, count) =>
          fetchShortsNext(token, cursor, count, mode, { signal: controller.signal }),
        isFeedExpiredError: (error) => error instanceof ShortsFeedExpiredError,
        commitFeed: (feed, event) => {
          if (generation !== requestGenerationRef.current) return;
          requestFeedRef.current = feed;
          if (event === "expired") {
            cancelPendingPersistedFeed();
            clearShortsFeedState(mode);
          }
        },
      });
      if (generation !== requestGenerationRef.current) return;
      hasLoadedBatchRef.current = true;

      if (outcome.kind === "empty") {
        setEmpty(true);
        // 库在旧队列播放期间可能被清空。丢弃已经失效的队列并停止换轮，
        // 否则末条视频的预取 effect 会持续请求同一个空库。
        onQueueResetRef.current();
        setItems([]);
        persistedFeedHighPositionRef.current = -1;
        queueStartOffsetRef.current = 0;
        setRoundComplete(false);
        requestFeedRef.current = EMPTY_SHORTS_FEED;
        cancelPendingPersistedFeed();
        clearShortsFeedState(mode);
        return;
      }

      setEmpty(false);
      setItems((prev) => mergeShortsQueue(prev, outcome.response));
      setRoundComplete(outcome.response.roundComplete);
    } catch {
      if (generation === requestGenerationRef.current && !controller.signal.aborted) {
        setLoadError(true);
      }
    } finally {
      if (generation === requestGenerationRef.current) {
        requestAbortRef.current = null;
        loadingRef.current = false;
        setLoading(false);
      }
    }
  }, [cancelPendingPersistedFeed, mode]);

  const trimQueueBefore = useCallback((count: number) => {
    const removeCount = Math.max(0, Math.floor(count));
    if (removeCount === 0) return;
    setItems((previous) =>
      removeCount >= previous.length ? [] : previous.slice(removeCount)
    );
    queueStartOffsetRef.current += removeCount;
  }, []);

  // 更换的是队列，播放器和已获音频授权的 media element 继续存活。
  useLayoutEffect(() => {
    if (modeRef.current === mode) return;
    flushPersistedFeed();
    requestGenerationRef.current += 1;
    requestAbortRef.current?.abort();
    requestAbortRef.current = null;
    loadingRef.current = false;
    hasLoadedBatchRef.current = false;
    requestFeedRef.current = loadShortsFeedState(mode);
    persistedFeedHighPositionRef.current = -1;
    queueStartOffsetRef.current = 0;
    modeRef.current = mode;
    onQueueResetRef.current();
    setItems([]);
    setQueueMode(mode);
    setLoading(true);
    setEmpty(false);
    setLoadError(false);
    setRoundComplete(false);
  }, [flushPersistedFeed, mode]);

  // 首次加载
  useEffect(() => {
    void loadMore();
    return () => {
      requestGenerationRef.current += 1;
      requestAbortRef.current?.abort();
      requestAbortRef.current = null;
      loadingRef.current = false;
    };
  }, [loadMore]);

  // 只提交首次进入过的最远视频游标。预取不会跳过未观看条目；回滑也不会
  // 让书签倒退。刷新页面后从本次实际到达过的最远视频之后恢复。
  useEffect(() => {
    if (empty || queueMode !== mode) return;
    const active = items[activeIndex];
    if (!active) return;

    const activeQueuePosition = queueStartOffset + activeIndex;
    if (activeQueuePosition > persistedFeedHighPositionRef.current) {
      persistedFeedHighPositionRef.current = activeQueuePosition;
      schedulePersistedFeed({
        feedToken: active.feedToken,
        cursor: active.feedCursor,
      });
    }

    const plan = planShortsPrefetch({
      remainingAfterActive: items.length - 1 - activeIndex,
      loading,
      loadError,
      roundComplete,
    });
    if (plan === "none") return;
    if (plan === "new-round") {
      requestFeedRef.current = EMPTY_SHORTS_FEED;
      setRoundComplete(false);
    }
    void loadMore();
  }, [
    activeIndex,
    queueStartOffset,
    items,
    loading,
    loadError,
    empty,
    queueMode,
    mode,
    roundComplete,
    loadMore,
    schedulePersistedFeed,
  ]);

  return { items, loading, empty, loadError, loadMore, trimQueueBefore };
}
