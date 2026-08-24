import { useEffect } from "react";
import type { VideoItem } from "@/types";
import { scheduleVideoDetailPagePreload } from "@/lib/videoDetailRoute";
import { VideoCard } from "./VideoCard";

type Props = {
  videos: VideoItem[];
  loading?: boolean;
  refreshMode?: "blocking" | "background";
  compact?: boolean;
  emptyText?: string;
  eagerCount?: number;
  highPriorityCount?: number;
  skeletonCount?: number;
};

export function VideoGrid({
  videos,
  loading,
  refreshMode,
  compact,
  emptyText = "暂时没有视频",
  eagerCount = 0,
  highPriorityCount = 0,
  skeletonCount = 8,
}: Props) {
  useEffect(() => {
    scheduleVideoDetailPagePreload();
  }, []);

  const blockingRefresh = refreshMode === "blocking";
  const backgroundRefresh = refreshMode === "background";

  if (loading) {
    return (
      <div
        className={`video-grid-loading ${compact ? "is-compact" : ""}`}
        aria-busy="true"
        role="status"
        aria-label="正在加载视频列表"
      >
        {Array.from({ length: skeletonCount }).map((_, i) => (
          <div key={i} className="skeleton-card" aria-hidden="true">
            <span className="skeleton-card__thumb" />
            <span className="skeleton-card__title" />
            <span className="skeleton-card__meta" />
          </div>
        ))}
      </div>
    );
  }

  if (!videos || videos.length === 0) {
    return <div className="video-grid-empty">{emptyText}</div>;
  }

  return (
    <div
      className={`video-grid-region ${blockingRefresh ? "is-busy" : ""}`}
      aria-busy={blockingRefresh || backgroundRefresh || undefined}
    >
      <div className={`video-grid ${compact ? "is-compact" : ""}`}>
        {(videos ?? []).map((v, index) => (
          <VideoCard
            key={v.id}
            video={v}
            eager={index < eagerCount}
            highPriority={index < highPriorityCount}
          />
        ))}
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
