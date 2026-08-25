import { memo, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { Link, useLocation } from "react-router";
import type { PreviewState, VideoItem } from "@/types";
import { prefetchVideoDetail } from "@/data/videos";
import { previewController } from "@/lib/previewController";
import {
  shouldInterceptPreviewTap,
  shouldStartInstantPreview,
} from "@/lib/previewIntent";
import { useInViewport } from "@/lib/useInViewport";
import { preloadVideoDetailPage } from "@/lib/videoDetailRoute";
import { formatCount } from "@/lib/format";
import { isVideoReturnPath, routeToPath } from "@/lib/videoReturnPath";
import { PreviewVideo } from "./PreviewVideo";
import { VideoThumbnail } from "./VideoThumbnail";

type Props = {
  video: VideoItem;
  eager?: boolean;
  highPriority?: boolean;
};

const HOVER_DELAY_MS = 300;

function useIsActivePreview(videoID: string): boolean {
  return useSyncExternalStore(
    previewController.subscribe,
    () => previewController.getActiveId() === videoID,
    () => false
  );
}

export const VideoCard = memo(function VideoCard({
  video,
  eager = false,
  highPriority = false,
}: Props) {
  const [previewState, setPreviewState] = useState<PreviewState>("idle");
  const [shouldRenderPreview, setShouldRenderPreview] = useState(false);
  const [progress, setProgress] = useState(0); // 0~1
  const author = video.author.trim();
  const location = useLocation();
  const currentPath = routeToPath(location);
  const linkState = isVideoReturnPath(currentPath)
    ? { from: currentPath }
    : undefined;

  const rootRef = useRef<HTMLElement | null>(null);
  const hoverTimerRef = useRef<number | null>(null);
  const lastPointerTypeRef = useRef<string>("");
  const canHoverRef = useRef(true);
  const videoRef = useRef<HTMLVideoElement | null>(null);

  const previewIsActive = useIsActivePreview(video.id);
  const inView = useInViewport(rootRef);

  // 当全局活跃卡片不是自己时，立刻停止预览
  useEffect(() => {
    if (!previewIsActive && shouldRenderPreview) {
      cleanup();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [previewIsActive, video.id]);

  // 离开视口时停止预览
  useEffect(() => {
    if (!inView && shouldRenderPreview) {
      cleanup();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inView]);

  // 卸载时清理
  useEffect(() => {
    return () => {
      cleanup();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const media = window.matchMedia("(hover: hover) and (pointer: fine)");
    const update = () => {
      canHoverRef.current = media.matches;
    };
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);

  function cleanup() {
    if (hoverTimerRef.current) {
      window.clearTimeout(hoverTimerRef.current);
      hoverTimerRef.current = null;
    }

    const el = videoRef.current;
    if (el) {
      try {
        el.pause();
        el.removeAttribute("src");
        el.load();
      } catch {
        // noop
      }
    }

    setShouldRenderPreview(false);
    setPreviewState("idle");
    setProgress(0);

    if (previewController.getActiveId() === video.id) {
      previewController.setActiveId(null);
    }
  }

  function startPreviewIntent() {
    if (!inView) return;
    if (hoverTimerRef.current) return;
    setPreviewState("intent");

    hoverTimerRef.current = window.setTimeout(() => {
      hoverTimerRef.current = null;
      startPreviewNow({ requireInView: true });
    }, HOVER_DELAY_MS);
  }

  function startPreviewNow(options: { requireInView: boolean }) {
    if (options.requireInView && !inView) return;
    if (hoverTimerRef.current) {
      window.clearTimeout(hoverTimerRef.current);
      hoverTimerRef.current = null;
    }
    previewController.setActiveId(video.id);
    setShouldRenderPreview(true);
    setPreviewState("loading");
  }

  function stopPreview() {
    cleanup();
  }

  function handlePointerEnter(event: React.PointerEvent<HTMLElement>) {
    lastPointerTypeRef.current = event.pointerType;
    preloadVideoDetailPage();
    if (shouldStartInstantPreview({ pointerType: event.pointerType })) return;
    startPreviewIntent();
  }

  function handlePointerLeave(event: React.PointerEvent<HTMLElement>) {
    if (shouldStartInstantPreview({ pointerType: event.pointerType })) return;
    stopPreview();
  }

  function handlePointerDown(event: React.PointerEvent<HTMLElement>) {
    lastPointerTypeRef.current = event.pointerType;
    prepareDetailNavigation();
  }

  function prepareDetailNavigation() {
    preloadVideoDetailPage();
    void prefetchVideoDetail(video.id);
  }

  function handleFocus() {
    preloadVideoDetailPage();
    startPreviewIntent();
  }

  function handleClickCapture(event: React.MouseEvent<HTMLAnchorElement>) {
    const previewActive = previewIsActive && shouldRenderPreview;
    if (
      !shouldInterceptPreviewTap({
        pointerType: lastPointerTypeRef.current,
        canHover: canHoverRef.current,
        previewActive,
      })
    ) {
      prepareDetailNavigation();
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    startPreviewNow({ requireInView: false });
  }

  return (
    <article
      ref={rootRef as React.RefObject<HTMLElement>}
      className="video-card"
      onPointerEnter={handlePointerEnter}
      onPointerLeave={handlePointerLeave}
      onPointerDown={handlePointerDown}
      onFocus={handleFocus}
      onBlur={stopPreview}
    >
      <Link
        to={video.href}
        state={linkState}
        className="video-card__link"
        tabIndex={0}
        onClickCapture={handleClickCapture}
      >
        <div className="thumb-frame">
          <VideoThumbnail
            src={video.thumbnail}
            eager={eager}
            highPriority={highPriority}
          />

          {shouldRenderPreview && (
            <PreviewVideo
              ref={videoRef}
              src={video.previewSrc}
              state={previewState}
              onCanPlay={() => setPreviewState("playing")}
              onError={() => setPreviewState("error")}
              onTimeUpdate={(p) => setProgress(p)}
            />
          )}

          {previewState === "loading" && <span className="preview-loader" />}
          {previewState === "error" && (
            <span className="preview-error">预览加载失败</span>
          )}

          {/* 预览进度条（播放时显示在底部） */}
          {previewState === "playing" && (
            <div className="preview-progress" aria-hidden="true">
              <div
                className="preview-progress__bar"
                style={{ width: `${Math.min(100, progress * 100)}%` }}
              />
            </div>
          )}

          {/* hover 时右上角 "预览" 角标 */}
          {previewState === "playing" && (
            <span className="preview-tag" aria-hidden="true">
              预览
            </span>
          )}

          {(video.badges ?? []).length > 0 && (
            <div className="badge-row">
              {video.badges.map((badge) => (
                <span className="video-badge" key={badge}>
                  {badge}
                </span>
              ))}
            </div>
          )}

          {video.sourceLabel && previewState !== "playing" && (
            <span
              className="source-badge"
              data-kind={sourceKindFromLabel(video.sourceLabel)}
              title={`来源：${video.sourceLabel}`}
            >
              {video.sourceLabel}
            </span>
          )}

          <span className="duration">{video.duration}</span>
        </div>

        <h3 className="video-title" title={video.title}>
          {video.title}
        </h3>

        <div className="video-meta">
          {author && (
            <span className="video-meta__author" title={author}>
              {author}
            </span>
          )}
          <span className="video-meta__views">{formatCount(video.views)} 观看</span>
          <span className="video-meta__date">{video.publishedAt}</span>
        </div>
      </Link>
    </article>
  );
});

// 从后端返回的 sourceLabel 推断网盘类型（用于颜色标识）。
// 后端目前会下发中文名（"夸克网盘" / "115 网盘" / "PikPak" / "联通网盘" / "OneDrive"）
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
