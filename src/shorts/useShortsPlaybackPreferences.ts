import { useEffect, useRef } from "react";
import { restoreShortsPlaybackRate } from "./playbackRate";

type Options = {
  getVideoElement: () => HTMLVideoElement | null;
  shouldMount: boolean;
  usesSharedVideo: boolean;
  isActive: boolean;
  canGoNext: boolean;
  autoAdvance: boolean;
  playbackRate: number;
  volume?: number;
  isTemporarilyAccelerated: boolean;
  onAdvance: () => void;
};

/** 桌面播放偏好不参与切源、缓冲或 iOS 受控循环的生命周期。 */
export function useShortsPlaybackPreferences(options: Options) {
  const latest = useRef(options);
  const hasControlledVolume = useRef(false);
  latest.current = options;
  const { getVideoElement, shouldMount, usesSharedVideo, playbackRate, volume,
    autoAdvance, canGoNext } = options;

  useEffect(() => {
    if (!shouldMount || usesSharedVideo) return;
    const video = getVideoElement();
    if (!video) return;
    video.defaultPlaybackRate = playbackRate;
    if (!latest.current.isTemporarilyAccelerated) restoreShortsPlaybackRate(video);
  }, [getVideoElement, shouldMount, usesSharedVideo, playbackRate]);

  useEffect(() => {
    if (!shouldMount || usesSharedVideo) return;
    // 手机首次播放由系统控制响度；从桌面缩回手机布局时恢复默认媒体音量。
    if (volume === undefined && !hasControlledVolume.current) return;
    const video = getVideoElement();
    if (!video) return;
    video.volume = volume ?? 1;
    hasControlledVolume.current = volume !== undefined;
  }, [getVideoElement, shouldMount, usesSharedVideo, volume]);

  useEffect(() => {
    if (!shouldMount || usesSharedVideo) return;
    const video = getVideoElement();
    if (!video) return;
    // 队列还在加载时继续循环当前视频，下一条就绪后再交给片尾翻页。
    video.loop = !autoAdvance || !canGoNext;
    const handleEnded = () => {
      const current = latest.current;
      if (current.isActive && current.autoAdvance && current.canGoNext &&
        !video.loop && video.ended && document.visibilityState !== "hidden") {
        current.onAdvance();
      }
    };
    video.addEventListener("ended", handleEnded);
    return () => video.removeEventListener("ended", handleEnded);
  }, [getVideoElement, shouldMount, usesSharedVideo, autoAdvance, canGoNext]);
}
