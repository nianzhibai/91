import { memo, useRef, useState } from "react";
import { Link, useLocation } from "react-router";
import { ThumbsDown, ThumbsUp } from "lucide-react";
import type { VideoItem } from "@/types";
import {
  prefetchVideoDetail,
  prefetchVideoRecommendations,
} from "@/data/videos";
import { useInViewport } from "@/lib/useInViewport";
import { useCardPreview } from "@/lib/useCardPreview";
import { useRouteActivity } from "@/lib/routeActivity";
import { preloadVideoDetailPage } from "@/lib/videoDetailRoute";
import { formatVideoDuration } from "@/lib/format";
import { isVideoReturnPath, routeToPath } from "@/lib/videoReturnPath";
import { createVideoDetailNavigationState } from "@/lib/videoListingBackground";
import { PreviewVideo } from "./PreviewVideo";
import { PreviewLoader } from "./PreviewLoader";
import { VideoThumbnail } from "./VideoThumbnail";

type Props = {
  video: VideoItem;
  eager?: boolean;
  highPriority?: boolean;
};

export const VideoCard = memo(function VideoCard({
  video,
  eager = false,
  highPriority = false,
}: Props) {
  const [titlePressed, setTitlePressed] = useState(false);
  const author = video.author?.trim() || "未知";
  const badges = video.badges ?? [];
  const hasOriginalBadge = badges.some((badge) => badge === "91" || badge === "原创");
  const location = useLocation();
  const currentPath = routeToPath(location);
  const linkState = isVideoReturnPath(currentPath)
    ? createVideoDetailNavigationState(currentPath, location)
    : undefined;

  const rootRef = useRef<HTMLElement | null>(null);
  const routeActive = useRouteActivity();
  const inView = useInViewport(rootRef);
  const {
    previewEnabled, previewState, shouldRenderPreview, showPreviewLoader,
    videoRef, startPreview, stopPreview, handlePreviewPlay, finishPreviewLoader,
  } = useCardPreview({ id: video.id, src: video.previewSrc, inView, active: routeActive });

  function handlePointerEnter(event: React.PointerEvent<HTMLElement>) {
    preloadVideoDetailPage();
    if (event.pointerType === "touch") return;
    startPreview();
  }

  function handlePointerLeave(event: React.PointerEvent<HTMLElement>) {
    if (event.pointerType === "touch") return;
    stopPreview();
  }

  function handlePointerDown(event: React.PointerEvent<HTMLElement>) {
    if (event.pointerType === "touch") return;
    prepareDetailNavigation();
  }

  function handleTouchStart() {
    preloadVideoDetailPage();
    startPreview();
  }

  function prepareDetailNavigation() {
    preloadVideoDetailPage();
    void prefetchVideoDetail(video.id);
  }

  function prepareConfirmedDetailNavigation() {
    prepareDetailNavigation();
    void prefetchVideoRecommendations(video.id);
  }

  function handleDetailNavigation() {
    stopPreview();
    prepareConfirmedDetailNavigation();
  }

  return (
    <article
      ref={rootRef as React.RefObject<HTMLElement>}
      className="video-card"
      data-preview-enabled={previewEnabled}
    >
      <Link
        to={video.href}
        state={linkState}
        className="video-card__link"
        aria-label={video.title}
        onPointerEnter={handlePointerEnter}
        onPointerLeave={handlePointerLeave}
        onPointerDown={handlePointerDown}
        onTouchStart={handleTouchStart}
        onFocus={preloadVideoDetailPage}
        onClick={handleDetailNavigation}
      >
        <div className="thumb-frame">
          <VideoThumbnail
            src={video.thumbnail}
            eager={eager}
            highPriority={highPriority}
          />

          {previewEnabled && shouldRenderPreview && (
            <PreviewVideo
              ref={videoRef}
              src={video.previewSrc}
              state={previewState}
              onPlay={handlePreviewPlay}
              onEnded={stopPreview}
              onError={stopPreview}
            />
          )}

          {previewEnabled && shouldRenderPreview && showPreviewLoader && (
            <PreviewLoader onFinish={finishPreviewLoader} />
          )}

          {badges.length > 0 && (
            <div className="badge-row">
              {badges.map((badge) => (
                <span className="video-badge" data-badge={badge} key={badge}>
                  {badge}
                </span>
              ))}
            </div>
          )}

          {video.sourceLabel && (
            <span
              className={`source-badge${hasOriginalBadge ? " source-badge--stacked" : ""}`}
              data-kind={sourceKindFromLabel(video.sourceLabel)}
              title={`来源：${video.sourceLabel}`}
            >
              <span className="source-badge__label">{video.sourceLabel}</span>
            </span>
          )}

          <span className="duration">{formatVideoDuration(video.duration)}</span>
        </div>
      </Link>

      <div className="video-card__body">
        <Link
          to={video.href}
          state={linkState}
          className="video-card__title-link"
          data-pressed={titlePressed || undefined}
          onPointerEnter={preloadVideoDetailPage}
          onPointerDown={() => {
            setTitlePressed(true);
            prepareDetailNavigation();
          }}
          onPointerUp={() => setTitlePressed(false)}
          onPointerCancel={() => setTitlePressed(false)}
          onPointerLeave={() => setTitlePressed(false)}
          onFocus={preloadVideoDetailPage}
          onClick={handleDetailNavigation}
        >
          <h3 className="video-title" title={video.title}>
            {video.title}
          </h3>
        </Link>

        <div className="video-meta">
          <span className="video-meta__date" title={video.publishedAt}>
            添加时间: {video.publishedAt}
          </span>
          <span className="video-meta__author" title={author}>
            作者: {author}
          </span>
          <div className="video-meta__row">
            <span className="video-meta__views">热度: {video.views}</span>
            <span>收藏: {video.favorites ?? 0}</span>
          </div>
          <div className="video-meta__row">
            <span>留言: {video.comments ?? 0}</span>
            <span className="video-meta__reaction" aria-label={`赞 ${video.likes ?? 0}`}>
              <ThumbsUp size={11} fill="currentColor" strokeWidth={1} aria-hidden="true" />
              {video.likes ?? 0}
            </span>
            <span className="video-meta__reaction" aria-label={`踩 ${video.dislikes ?? 0}`}>
              <ThumbsDown size={11} fill="currentColor" strokeWidth={1} aria-hidden="true" />
              {video.dislikes ?? 0}
            </span>
          </div>
        </div>
      </div>
    </article>
  );
});

// 从后端返回的 sourceLabel 推断网盘类型（用于颜色标识）。
// 后端目前会下发中文名（"夸克网盘" / "115网盘" / "PikPak" / "联通网盘" / "OneDrive"）
// 或英文 kind。两边都尝试匹配；都没匹配上时返回空字符串，CSS 会回落到默认色。
function sourceKindFromLabel(label: string): string {
  const value = label.toLowerCase();
  if (value.includes("夸克") || value.includes("quark")) return "quark";
  if (value.includes("115") || value.includes("p115")) return "p115";
  if (value.includes("123") || value.includes("p123")) return "p123";
  if (value.includes("pikpak")) return "pikpak";
  if (value.includes("沃盘") || value.includes("wopan") || value.includes("联通")) return "wopan";
  if (value.includes("光鸭") || value.includes("guangyapan") || value.includes("guangya")) return "guangyapan";
  if (value.includes("onedrive") || value.includes("one drive")) return "onedrive";
  if (value.includes("webdav") || value.includes("web dav")) return "webdav";
  if (value.includes("本地") || value.includes("localstorage") || value.includes("local storage")) return "localstorage";
  return "";
}
