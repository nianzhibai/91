import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useWindowVirtualizer } from "@tanstack/react-virtual";
import {
  shouldLoadMore,
  virtualGridColumns,
  virtualRowCount,
  virtualRowRange,
} from "@/lib/virtualGrid";
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
const ESTIMATED_ROW_HEIGHT = 260;
const ESTIMATED_COMPACT_ROW_HEIGHT = 120;
const MOBILE_GRID_QUERY = "(max-width: 640px)";
const TABLET_GRID_QUERY = "(max-width: 1024px)";
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
};

function readResponsiveGridColumns(): number {
  if (typeof window === "undefined") return 4;
  return virtualGridColumns({
    compact: false,
    mobile: window.matchMedia(MOBILE_GRID_QUERY).matches,
    tablet: window.matchMedia(TABLET_GRID_QUERY).matches,
  });
}

function useResponsiveGridColumns(): number {
  const [columns, setColumns] = useState(readResponsiveGridColumns);

  useEffect(() => {
    const mobile = window.matchMedia(MOBILE_GRID_QUERY);
    const tablet = window.matchMedia(TABLET_GRID_QUERY);
    const update = () => setColumns(readResponsiveGridColumns());
    mobile.addEventListener("change", update);
    tablet.addEventListener("change", update);
    return () => {
      mobile.removeEventListener("change", update);
      tablet.removeEventListener("change", update);
    };
  }, []);

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
}: Props) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const containerWidthRef = useRef(0);
  const responsiveColumns = useResponsiveGridColumns();
  const columns = compact ? 1 : responsiveColumns;
  // 列表容器距文档顶部的距离：window virtualizer 用它把窗口滚动换算成列表内偏移。
  const [scrollMargin, setScrollMargin] = useState(0);
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

  const virtualizer = useWindowVirtualizer({
    count: virtualRowCountWithTail,
    estimateSize,
    overscan: overscanRows,
    scrollMargin,
    getItemKey,
    directDomUpdates: true,
    directDomUpdatesMode: "transform",
  });

  const updateScrollMargin = useCallback(() => {
    const container = containerRef.current;
    if (!container) return;

    const rect = container.getBoundingClientRect();
    containerWidthRef.current = rect.width;
    const nextMargin = rect.top + window.scrollY;
    setScrollMargin((current) =>
      Math.abs(current - nextMargin) < 1 ? current : nextMargin
    );
  }, []);

  // 容器位置只在挂载和真实布局变化时读取，不跟着虚拟列表的每次渲染读取。
  useLayoutEffect(() => {
    updateScrollMargin();
  }, [updateScrollMargin]);

  useEffect(() => {
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
  }, [updateScrollMargin]);

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

  useEffect(() => {
    if (!onLoadMore || lastRow < 0) return;
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
