import { useCallback, useEffect, useRef, useState } from "react";
import {
  canRestoreScrollY,
  readListingScrollEntry,
  resolveReachableScrollY,
  resolveRestoreCount,
  resolveRestoreScrollY,
  writeListingScrollEntry,
  type ListingScrollStorage,
} from "@/lib/listingScrollRestore";

/**
 * 无限滚动列表的前进/后退现场恢复。历史条目的 key 是存储键，因此"后退"
 * 拿回的是那一条历史自己的进度，重新进入列表则是干净的新会话。
 *
 * 拆成两个 hook 是因为存在先后依赖：要补回多少条必须在数据层发起第一个
 * 请求之前就确定，而落盘进度又依赖数据层已经请求到的条数。
 */

// 内容还没渲染够时滚不到目标位置，按帧重试；超过上限就放弃，避免死循环。
const RESTORE_MAX_FRAMES = 90;

function sessionStorageOrNull(): ListingScrollStorage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

export type ListingRestoreTarget = {
  historyKey: string;
  count: number;
  scrollY: number;
};

/**
 * 解析当前历史条目要恢复的进度。在渲染期读取（而不是 effect）：数据层的
 * 首个请求就发生在这次渲染之后，晚一拍就会先打一个只有首屏的请求。
 */
export function useListingRestoreTarget(input: {
  historyKey: string;
  queryKey: string;
  pageSize: number;
}): ListingRestoreTarget {
  const targetRef = useRef<ListingRestoreTarget | null>(null);
  if (!targetRef.current || targetRef.current.historyKey !== input.historyKey) {
    const entry = readListingScrollEntry(
      sessionStorageOrNull(),
      input.historyKey
    );
    targetRef.current = {
      historyKey: input.historyKey,
      count: resolveRestoreCount({
        entry,
        queryKey: input.queryKey,
        pageSize: input.pageSize,
      }),
      scrollY: resolveRestoreScrollY(entry, input.queryKey),
    };
  }
  return targetRef.current;
}

export type UseListingScrollRestoreInput = {
  target: ListingRestoreTarget;
  queryKey: string;
  /** 当前已经请求过的条目数，作为下次恢复的进度。 */
  requestedCount: number;
  itemCount: number;
};

export function useListingScrollRestore({
  target,
  queryKey,
  requestedCount,
  itemCount,
}: UseListingScrollRestoreInput) {
  const historyKey = target.historyKey;
  const pendingScrollYRef = useRef(target.scrollY);
  const lastScrollYRef = useRef(target.scrollY);
  const restoredHistoryKeyRef = useRef(historyKey);
  const [restoring, setRestoring] = useState(target.scrollY > 0);

  if (restoredHistoryKeyRef.current !== historyKey) {
    restoredHistoryKeyRef.current = historyKey;
    pendingScrollYRef.current = target.scrollY;
    lastScrollYRef.current = target.scrollY;
  }

  useEffect(() => {
    const targetScrollY = pendingScrollYRef.current;
    if (targetScrollY <= 0) {
      setRestoring(false);
      return;
    }
    setRestoring(true);
    if (itemCount === 0) return;

    let frame = 0;
    let handle = 0;
    const finish = (restoredScrollY: number) => {
      pendingScrollYRef.current = 0;
      lastScrollYRef.current = restoredScrollY;
      setRestoring(false);
    };
    const attempt = () => {
      if (
        canRestoreScrollY({
          targetScrollY,
          documentHeight: document.documentElement.scrollHeight,
          viewportHeight: window.innerHeight,
        })
      ) {
        window.scrollTo(0, targetScrollY);
        finish(targetScrollY);
        return;
      }
      frame += 1;
      if (frame >= RESTORE_MAX_FRAMES) {
        // 保存的位置比恢复上限更深时，停在能到达的最远处而不是回到顶部。
        const reachable = resolveReachableScrollY({
          targetScrollY,
          documentHeight: document.documentElement.scrollHeight,
          viewportHeight: window.innerHeight,
        });
        window.scrollTo(0, reachable);
        finish(reachable);
        return;
      }
      handle = window.requestAnimationFrame(attempt);
    };
    handle = window.requestAnimationFrame(attempt);

    return () => window.cancelAnimationFrame(handle);
  }, [historyKey, itemCount]);

  const save = useCallback(
    (scrollY: number) => {
      // 还没恢复完就落盘，会把保存的位置覆盖成恢复前的 0。
      if (pendingScrollYRef.current > 0) return;
      if (requestedCount <= 0) return;
      writeListingScrollEntry(sessionStorageOrNull(), historyKey, {
        queryKey,
        requestedCount,
        scrollY: Math.max(0, Math.round(scrollY)),
      });
    },
    [historyKey, queryKey, requestedCount]
  );

  useEffect(() => {
    let ticking = false;
    let live = true;
    const handleScroll = () => {
      // 位置要在滚动事件里同步记下：卸载时列表 DOM 已经被详情页顶掉，
      // 那时再读 window.scrollY 拿到的是被浏览器压缩过的值。
      lastScrollYRef.current = Math.max(0, Math.round(window.scrollY));
      if (ticking) return;
      ticking = true;
      window.requestAnimationFrame(() => {
        ticking = false;
        if (!live) return;
        save(lastScrollYRef.current);
      });
    };
    const handlePageHide = () => save(window.scrollY);

    window.addEventListener("scroll", handleScroll, { passive: true });
    window.addEventListener("pagehide", handlePageHide);
    return () => {
      live = false;
      window.removeEventListener("scroll", handleScroll);
      window.removeEventListener("pagehide", handlePageHide);
      // 离开列表页（例如进详情页）时把最后的位置落盘，后退才能回到原处。
      save(lastScrollYRef.current);
    };
  }, [save]);

  return { restoring };
}
