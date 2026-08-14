import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { useWindowVirtualizer } from "@tanstack/react-virtual";
import {
  virtualGridColumns,
  virtualRowCount,
  virtualRowRange,
} from "@/lib/virtualGrid";
import type { VideoItem } from "@/types";
import { VideoCard } from "./VideoCard";

/**
 * 虚拟滚动的视频网格：以"整行"为虚拟单元交给 @tanstack/react-virtual 的
 * window virtualizer 管理，只挂载可视区域附近的行，其余行由占位容器的
 * 总高度顶出来，滚动条因此与完整列表等高。
 *
 * 每行本身仍是原来的 .video-grid，列数与列间距沿用既有 CSS；行高由
 * measureElement 实测，卡片高度随断点或标题变化都不需要额外配置。
 */

const DEFAULT_OVERSCAN_ROWS = 2;
const ESTIMATED_ROW_HEIGHT = 260;
const ESTIMATED_COMPACT_ROW_HEIGHT = 120;

export type VirtualGridRange = {
  startIndex: number;
  endIndex: number;
  columns: number;
};

type Props = {
  videos: VideoItem[];
  compact?: boolean;
  eagerCount?: number;
  highPriorityCount?: number;
  overscanRows?: number;
  refreshMode?: "blocking" | "background";
  onRangeChange?: (range: VirtualGridRange) => void;
};

export function VirtualVideoGrid({
  videos,
  compact,
  eagerCount = 0,
  highPriorityCount = 0,
  overscanRows = DEFAULT_OVERSCAN_ROWS,
  refreshMode,
  onRangeChange,
}: Props) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [columns, setColumns] = useState(1);
  // 列表容器距文档顶部的距离：window virtualizer 用它把窗口滚动换算成列表内偏移。
  const [scrollMargin, setScrollMargin] = useState(0);
  const [rowHeight, setRowHeight] = useState(
    compact ? ESTIMATED_COMPACT_ROW_HEIGHT : ESTIMATED_ROW_HEIGHT
  );
  const rowCount = virtualRowCount(videos.length, columns);

  const virtualizer = useWindowVirtualizer({
    count: rowCount,
    estimateSize: () => rowHeight,
    overscan: overscanRows,
    scrollMargin,
    getItemKey: (index) => videos[index * columns]?.id ?? index,
  });

  const measure = useCallback(() => {
    const container = containerRef.current;
    if (!container) return;

    const rect = container.getBoundingClientRect();
    const nextMargin = rect.top + window.scrollY;
    setScrollMargin((current) =>
      Math.abs(current - nextMargin) < 1 ? current : nextMargin
    );

    const row = container.querySelector<HTMLElement>(".video-grid--virtual-row");
    if (!row) return;
    const nextColumns = virtualGridColumns(window.getComputedStyle(row));
    setColumns((current) => (current === nextColumns ? current : nextColumns));
    const nextRowHeight = row.getBoundingClientRect().height;
    setRowHeight((current) =>
      nextRowHeight > 0 && Math.abs(current - nextRowHeight) >= 1
        ? nextRowHeight
        : current
    );
  }, []);

  // 布局阶段就要量出列数：首帧按单列渲染，浏览器绘制之前会被纠正过来。
  useLayoutEffect(() => {
    measure();
  });

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    const observer =
      typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    observer?.observe(container);
    window.addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, [measure]);

  // 列数变化会重排每一行的内容，之前测到的行高全部失效。
  useEffect(() => {
    virtualizer.measure();
  }, [columns, compact, virtualizer]);

  const virtualRows = virtualizer.getVirtualItems();
  const firstRow = virtualRows[0]?.index ?? 0;
  const lastRow = virtualRows[virtualRows.length - 1]?.index ?? -1;

  useEffect(() => {
    if (lastRow < 0) return;
    onRangeChange?.({
      startIndex: firstRow * columns,
      endIndex: Math.min((lastRow + 1) * columns, videos.length),
      columns,
    });
  }, [columns, firstRow, lastRow, onRangeChange, videos.length]);

  const blockingRefresh = refreshMode === "blocking";
  const backgroundRefresh = refreshMode === "background";

  return (
    <div
      ref={containerRef}
      className={`video-grid-region ${blockingRefresh ? "is-busy" : ""}`}
      aria-busy={blockingRefresh || backgroundRefresh || undefined}
    >
      <div
        className="video-grid-virtual-canvas"
        style={{ height: virtualizer.getTotalSize() }}
      >
        {virtualRows.map((virtualRow) => {
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
              style={{
                transform: `translateY(${
                  virtualRow.start - virtualizer.options.scrollMargin
                }px)`,
              }}
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
          <span className="video-grid-refresh-overlay__spinner" aria-hidden="true" />
          <span>正在同步</span>
        </div>
      )}
    </div>
  );
}
