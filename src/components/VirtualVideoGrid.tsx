import {
  useCallback,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
  type Ref,
} from "react";
import { useWindowVirtualizer } from "@tanstack/react-virtual";
import {
  matchingVirtualGridSnapshot,
  shouldLoadMore,
  virtualGridColumns,
  virtualRowCount,
  virtualRowRange,
  type VirtualGridHandle,
  type VirtualGridSnapshot,
} from "@/lib/virtualGrid";
import { useRouteActivity } from "@/lib/routeActivity";
import {
  resolveVideoGridAnchor,
  type VideoGridAnchor,
} from "@/lib/videoGridAnchor";
import type { VideoItem } from "@/types";
import { VideoCard } from "./VideoCard";

/**
 * 虚拟滚动的视频网格：以"整行"为虚拟单元交给 @tanstack/react-virtual 的
 * window virtualizer 管理，只挂载可视区域附近的行，其余已加载行由占位容器
 * 顶出高度。未加载内容只保留固定尾行，滚动条随真实内容逐批增长。
 *
 * 每行本身仍是原来的 .video-grid，列数与列间距沿用既有 CSS；行高由
 * measureElement 实测，卡片高度随断点或标题变化都不需要额外配置。
 */

const DEFAULT_OVERSCAN_ROWS = 2;
const ESTIMATED_ROW_HEIGHT = 280;
const ESTIMATED_COMPACT_ROW_HEIGHT = 150;
const MOBILE_GRID_QUERY = "(max-width: 767px)";
const TABLET_GRID_QUERY = "(max-width: 991px)";
const TAIL_ROW_HEIGHT = 56;
const TAIL_ROW_KEY = "video-grid-tail";

type Props = {
  videos: VideoItem[];
  compact?: boolean;
  eagerCount?: number;
  highPriorityCount?: number;
  overscanRows?: number;
  refreshMode?: "blocking" | "background";
  hasMore?: boolean;
  loadingMore?: boolean;
  prefetchRows?: number;
  tailContent?: ReactNode;
  onLoadMore?: () => void;
  initialSnapshot?: VirtualGridSnapshot;
  snapshotRef?: Ref<VirtualGridHandle>;
};

function readResponsiveGridColumns(): number {
  if (typeof window === "undefined") return 4;
  return virtualGridColumns({
    compact: false,
    mobile: window.matchMedia(MOBILE_GRID_QUERY).matches,
    tablet: window.matchMedia(TABLET_GRID_QUERY).matches,
  });
}

function useResponsiveGridColumns(active: boolean): number {
  const [columns, setColumns] = useState(readResponsiveGridColumns);

  useLayoutEffect(() => {
    if (!active) return;
    const mobile = window.matchMedia(MOBILE_GRID_QUERY);
    const tablet = window.matchMedia(TABLET_GRID_QUERY);
    const update = () => setColumns(readResponsiveGridColumns());
    mobile.addEventListener("change", update);
    tablet.addEventListener("change", update);
    // Catch viewport changes that happened while the listing was inactive.
    update();
    return () => {
      mobile.removeEventListener("change", update);
      tablet.removeEventListener("change", update);
    };
  }, [active]);

  return columns;
}

export function VirtualVideoGrid({
  videos,
  compact,
  eagerCount = 0,
  highPriorityCount = 0,
  overscanRows = DEFAULT_OVERSCAN_ROWS,
  refreshMode,
  hasMore = false,
  loadingMore = false,
  prefetchRows = 2,
  tailContent,
  onLoadMore,
  initialSnapshot,
  snapshotRef,
}: Props) {
  const routeActive = useRouteActivity();
  const containerRef = useRef<HTMLDivElement | null>(null);
  const containerWidthRef = useRef(0);
  const responsiveColumns = useResponsiveGridColumns(routeActive);
  const columns = compact ? 1 : responsiveColumns;
  const restoredSnapshot = matchingVirtualGridSnapshot(initialSnapshot, {
    viewportWidth: typeof window === "undefined" ? 0 : window.innerWidth,
    columns,
    compact: !!compact,
  });
  // 列表容器距文档顶部的距离：window virtualizer 用它把窗口滚动换算成列表内偏移。
  const [scrollMargin, setScrollMargin] = useState(
    () => restoredSnapshot?.scrollMargin ?? 0
  );
  const loadedRowCount = virtualRowCount(videos.length, columns);
  // 未加载内容不提前撑高页面；只保留一个固定尾行承载加载反馈。
  const hasTailRow = hasMore;
  const virtualRowCountWithTail = loadedRowCount + (hasTailRow ? 1 : 0);
  const getItemKey = useCallback(
    (index: number) =>
      index === loadedRowCount && hasTailRow
        ? TAIL_ROW_KEY
        : videos[index * columns]?.id ?? index,
    [columns, hasTailRow, loadedRowCount, videos]
  );
  const estimateSize = useCallback(
    (index: number) =>
      index === loadedRowCount && hasTailRow
        ? TAIL_ROW_HEIGHT
        : compact
        ? ESTIMATED_COMPACT_ROW_HEIGHT
        : ESTIMATED_ROW_HEIGHT,
    [compact, hasTailRow, loadedRowCount]
  );
  const getScrollElement = useCallback(
    () =>
      routeActive && typeof document !== "undefined" ? window : null,
    [routeActive]
  );

  const virtualizer = useWindowVirtualizer({
    count: virtualRowCountWithTail,
    estimateSize,
    overscan: overscanRows,
    scrollMargin,
    getItemKey,
    getScrollElement,
    initialMeasurementsCache: restoredSnapshot?.measurements,
    directDomUpdates: true,
    directDomUpdatesMode: "transform",
  });

  useImperativeHandle(
    snapshotRef,
    () => ({
      takeSnapshot: () => ({
        viewportWidth: window.innerWidth,
        columns,
        compact: !!compact,
        scrollMargin,
        measurements: virtualizer.takeSnapshot(),
      }),
    }),
    [columns, compact, scrollMargin, virtualizer]
  );

  const updateScrollMargin = useCallback(() => {
    if (!routeActive) return;
    const container = containerRef.current;
    if (!container) return;

    const rect = container.getBoundingClientRect();
    containerWidthRef.current = rect.width;
    const nextMargin = rect.top + window.scrollY;
    setScrollMargin((current) =>
      Math.abs(current - nextMargin) < 1 ? current : nextMargin
    );
  }, [routeActive]);

  // 容器位置只在挂载和真实布局变化时读取，不跟着虚拟列表的每次渲染读取。
  useLayoutEffect(() => {
    updateScrollMargin();
  }, [updateScrollMargin]);

  useLayoutEffect(() => {
    if (!routeActive) return;
    const container = containerRef.current;
    if (!container) return;
    const observer =
      typeof ResizeObserver === "undefined"
        ? null
        : new ResizeObserver(([entry]) => {
            const nextWidth = entry?.contentRect.width ?? 0;
            if (
              nextWidth <= 0 ||
              Math.abs(containerWidthRef.current - nextWidth) < 1
            ) {
              return;
            }
            updateScrollMargin();
          });
    observer?.observe(container);
    window.addEventListener("resize", updateScrollMargin);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", updateScrollMargin);
    };
  }, [routeActive, updateScrollMargin]);

  // 只有断点或视图模式改变时，旧行的测量才真正失效。追加批次会保留旧缓存。
  const layoutIdentity = `${columns}:${compact ? "compact" : "grid"}`;
  const previousLayoutIdentityRef = useRef(layoutIdentity);
  useLayoutEffect(() => {
    if (previousLayoutIdentityRef.current === layoutIdentity) return;
    previousLayoutIdentityRef.current = layoutIdentity;
    virtualizer.measure();
  }, [layoutIdentity, virtualizer]);

  const virtualRows = virtualizer.getVirtualItems();
  const lastRow = virtualRows[virtualRows.length - 1]?.index ?? -1;

  const anchorRef = useRef<VideoGridAnchor | null>(null);
  useLayoutEffect(() => {
    if (!routeActive) return;
    const anchor = anchorRef.current;
    const target = anchor ? resolveVideoGridAnchor(anchor, videos) : null;
    let restoring = target !== null;
    let frame = 0;
    let attempts = 0;
    let stableFrames = 0;

    const capture = () => {
      if (restoring) return;
      const scrollY = window.scrollY;
      const row = virtualizer.getVirtualItems().find((item) => item.end > scrollY);
      anchorRef.current = row && scrollY >= scrollMargin && row.index < loadedRowCount
        ? { videos, index: row.index * columns, viewportOffset: row.start - scrollY }
        : null;
    };

    // The document is unlocked before this layout effect runs. Correct using
    // the rendered row after regrouping, including changes in measured height.
    const restore = () => {
      if (!target) return;
      const rowIndex = Math.floor(target.index / columns);
      const row = containerRef.current?.querySelector<HTMLElement>(
        `[data-index="${rowIndex}"]`
      );
      const top = row
        ? window.scrollY + row.getBoundingClientRect().top - target.viewportOffset
        : virtualizer.getOffsetForIndex(rowIndex, "start")?.[0];
      if (top !== undefined) {
        const reachable = Math.max(0, Math.min(
          top,
          document.documentElement.scrollHeight - window.innerHeight
        ));
        const aligned = Math.abs(window.scrollY - reachable) < 1;
        stableFrames = row && aligned ? stableFrames + 1 : 0;
        if (!aligned) window.scrollTo({ top: reachable, behavior: "auto" });
      }
      if (++attempts < 30 && stableFrames < 2) {
        frame = window.requestAnimationFrame(restore);
      } else {
        restoring = false;
        capture();
      }
    };

    if (target) restore();
    else capture();
    window.addEventListener("scroll", capture, { passive: true });
    return () => {
      window.cancelAnimationFrame(frame);
      window.removeEventListener("scroll", capture);
    };
  }, [routeActive, videos, columns, scrollMargin, loadedRowCount, virtualizer]);

  useEffect(() => {
    if (!routeActive || !onLoadMore || lastRow < 0) return;
    if (
      shouldLoadMore({
        endIndex: Math.min((lastRow + 1) * columns, videos.length),
        itemCount: videos.length,
        columns,
        hasMore,
        loading: loadingMore,
        prefetchRows,
      })
    ) {
      onLoadMore();
    }
  }, [
    columns,
    hasMore,
    lastRow,
    loadingMore,
    onLoadMore,
    prefetchRows,
    routeActive,
    videos.length,
  ]);

  const blockingRefresh = refreshMode === "blocking";
  const backgroundRefresh = refreshMode === "background";

  return (
    <div
      ref={containerRef}
      className={`video-grid-region ${blockingRefresh ? "is-busy" : ""}`}
      aria-busy={
        blockingRefresh || backgroundRefresh || loadingMore || undefined
      }
    >
      <div
        ref={virtualizer.containerRef}
        className="video-grid-virtual-canvas"
      >
        {virtualRows.map((virtualRow) => {
          if (virtualRow.index === loadedRowCount && hasTailRow) {
            return (
              <div
                key={virtualRow.key}
                data-index={virtualRow.index}
                ref={virtualizer.measureElement}
                className="video-grid-virtual-tail"
                style={{ height: TAIL_ROW_HEIGHT }}
              >
                {tailContent}
              </div>
            );
          }
          const { start, end } = virtualRowRange(
            virtualRow.index,
            columns,
            videos.length
          );
          return (
            <div
              key={virtualRow.key}
              data-index={virtualRow.index}
              ref={virtualizer.measureElement}
              className={`video-grid video-grid--virtual-row ${
                compact ? "is-compact" : ""
              }`}
            >
              {videos.slice(start, end).map((video, offset) => {
                const index = start + offset;
                return (
                  <VideoCard
                    key={video.id}
                    video={video}
                    eager={index < eagerCount}
                    highPriority={index < highPriorityCount}
                  />
                );
              })}
            </div>
          );
        })}
      </div>
      {blockingRefresh && (
        <div className="video-grid-refresh-overlay" aria-hidden="true" />
      )}
      {backgroundRefresh && (
        <div className="video-grid-background-status" role="status">
          <span
            className="video-grid-refresh-overlay__spinner"
            aria-hidden="true"
          />
          <span>正在同步</span>
        </div>
      )}
    </div>
  );
}
