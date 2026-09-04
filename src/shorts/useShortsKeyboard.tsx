import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Sparkles } from "lucide-react";
import { clamp } from "./mediaBuffer";

const SHORTS_KEYBOARD_SEEK_SECONDS = 5;
const SHORTS_KEYBOARD_FAST_PLAYBACK_DELAY_MS = 400;
// 浏览器失焦时可能收不到 keyup；左键最后一次重复事件后自动提交，避免目标悬空。
const SHORTS_KEYBOARD_SEEK_IDLE_COMMIT_MS = 1500;
const SHORTS_KEYBOARD_SEEK_RELEASE_HIDE_MS = 400;
const SHORTS_KEYBOARD_DOUBLE_SPACE_MS = 280;

type ShortsKeyboardSeekKey = "ArrowLeft" | "ArrowRight";

export type ShortsKeyboardSeekPreview = {
  videoIndex: number;
  currentTime: number;
  duration: number;
};

type ShortsKeyboardSeekTarget = ShortsKeyboardSeekPreview & {
  video: HTMLVideoElement;
};

export type ShortsKeyboardOptions = {
  /** 滚动容器：用于定位目标 slide 与当前屏的点赞按钮 */
  containerRef: React.RefObject<HTMLDivElement | null>;
  activeIndexRef: React.MutableRefObject<number>;
  itemsLengthRef: React.MutableRefObject<number>;
  /** 返回某索引当前生效的 video 元素（iOS 分支返回共享节点） */
  getVideoAtIndex: (index: number) => HTMLVideoElement | undefined;
  isVideoPausedByUser: (index: number) => boolean;
  setUserPausedForIndex: (index: number, isPaused: boolean) => void;
  onToggleMute: () => void;
  showHud: (text: string, icon?: ReactNode) => void;
  isWindowsShortsPlatform: boolean;
};

/**
 * 短视频页的键盘快捷键：
 * - ↑/↓ 切换上下视频，空格播放/暂停（双空格点赞），M 静音，L 点赞
 * - ← 按键重复时累计快退 5s/次，松开（或失焦/超时）后提交一次真实 seek
 * - → 短按快进 5s；按住 400ms 后以 2 倍速播放，松开恢复 1 倍速
 */
export function useShortsKeyboard(options: ShortsKeyboardOptions) {
  const [keyboardSeekPreview, setKeyboardSeekPreview] =
    useState<ShortsKeyboardSeekPreview | null>(null);
  const [keyboardFastPlaybackIndex, setKeyboardFastPlaybackIndex] =
    useState<number | null>(null);
  const keyboardSeekTargetRef = useRef<ShortsKeyboardSeekTarget | null>(null);
  const keyboardSeekHeldKeysRef = useRef<Set<ShortsKeyboardSeekKey>>(new Set());
  const keyboardSeekCommitTimerRef = useRef<number | null>(null);
  const keyboardSeekHideTimerRef = useRef<number | null>(null);
  const keyboardLikeHandlersRef = useRef<Map<number, () => void>>(new Map());
  const registerKeyboardLikeHandler = useCallback(
    (index: number, handler: (() => void) | null) => {
      if (handler) {
        keyboardLikeHandlersRef.current.set(index, handler);
      } else {
        keyboardLikeHandlersRef.current.delete(index);
      }
    },
    []
  );
  // 监听只绑定一次；回调经由这里读取每次渲染的最新配置。
  const optionsRef = useRef(options);
  optionsRef.current = options;

  // 键盘快捷键监听
  useEffect(() => {
    const { activeIndexRef, itemsLengthRef, containerRef } = optionsRef.current;
    const getCurrentVideoAtIndex = (videoIndex: number) =>
      optionsRef.current.getVideoAtIndex(videoIndex);
    const isVideoPausedByUser = (videoIndex: number) =>
      optionsRef.current.isVideoPausedByUser(videoIndex);
    const setUserPausedForIndex = (videoIndex: number, isPaused: boolean) =>
      optionsRef.current.setUserPausedForIndex(videoIndex, isPaused);
    const showHud = (text: string, icon?: ReactNode) =>
      optionsRef.current.showHud(text, icon);

    let pendingSpaceTimer: number | null = null;
    let pendingSpaceTarget: {
      videoIndex: number;
      video: HTMLVideoElement;
    } | null = null;
    let keyboardRightPressTimer: number | null = null;
    let keyboardRightPressTarget: {
      videoIndex: number;
      video: HTMLVideoElement;
      fastPlaybackActive: boolean;
    } | null = null;

    const clearKeyboardSeekCommitTimer = () => {
      if (keyboardSeekCommitTimerRef.current === null) return;
      window.clearTimeout(keyboardSeekCommitTimerRef.current);
      keyboardSeekCommitTimerRef.current = null;
    };

    const scheduleKeyboardSeekPreviewHide = (delay: number) => {
      if (keyboardSeekHideTimerRef.current !== null) {
        window.clearTimeout(keyboardSeekHideTimerRef.current);
      }
      keyboardSeekHideTimerRef.current = window.setTimeout(() => {
        keyboardSeekHideTimerRef.current = null;
        setKeyboardSeekPreview(null);
      }, delay);
    };

    const clearKeyboardSpaceTimer = () => {
      if (pendingSpaceTimer !== null) {
        window.clearTimeout(pendingSpaceTimer);
      }
      pendingSpaceTimer = null;
      pendingSpaceTarget = null;
    };

    const getActiveLikeButton = (videoIndex: number) =>
      containerRef.current?.querySelector<HTMLButtonElement>(
        `[data-index="${videoIndex}"] [data-shorts-like]`
      ) ?? null;

    const likeActiveVideo = (videoIndex: number) => {
      keyboardLikeHandlersRef.current.get(videoIndex)?.();
    };

    const toggleKeyboardPlayback = (target: {
      videoIndex: number;
      video: HTMLVideoElement;
    }) => {
      if (
        activeIndexRef.current !== target.videoIndex ||
        getCurrentVideoAtIndex(target.videoIndex) !== target.video
      ) {
        return;
      }

      const shouldResume =
        isVideoPausedByUser(target.videoIndex) ||
        (target.video.paused && target.video.readyState >= 3);
      if (shouldResume) {
        setUserPausedForIndex(target.videoIndex, false);
        target.video.play().catch(() => undefined);
      } else {
        setUserPausedForIndex(target.videoIndex, true);
        target.video.pause();
      }
    };

    const scheduleKeyboardSpaceToggle = (
      videoIndex: number,
      video: HTMLVideoElement
    ) => {
      pendingSpaceTarget = { videoIndex, video };
      pendingSpaceTimer = window.setTimeout(() => {
        const target = pendingSpaceTarget;
        pendingSpaceTimer = null;
        pendingSpaceTarget = null;
        if (target) toggleKeyboardPlayback(target);
      }, SHORTS_KEYBOARD_DOUBLE_SPACE_MS);
    };

    const discardKeyboardSeek = () => {
      clearKeyboardSeekCommitTimer();
      keyboardSeekTargetRef.current = null;
      keyboardSeekHeldKeysRef.current.clear();
    };

    const commitKeyboardSeek = () => {
      const target = keyboardSeekTargetRef.current;
      if (!target) return false;

      discardKeyboardSeek();
      const currentVideo = getCurrentVideoAtIndex(target.videoIndex);
      if (
        activeIndexRef.current === target.videoIndex &&
        currentVideo === target.video
      ) {
        const duration =
          Number.isFinite(target.video.duration) && target.video.duration > 0
            ? target.video.duration
            : target.duration;
        const nextTime = clamp(target.currentTime, 0, duration);
        try {
          // 左键连按期间只更新预览；松开后才执行这一次真实 seek。
          target.video.currentTime = nextTime;
        } catch {
          // ignore（部分 ready state 下设置会抛错）
        }
      }
      return true;
    };

    const finishKeyboardSeek = () => {
      if (!commitKeyboardSeek()) return;
      scheduleKeyboardSeekPreviewHide(SHORTS_KEYBOARD_SEEK_RELEASE_HIDE_MS);
    };

    const scheduleKeyboardSeekIdleCommit = () => {
      clearKeyboardSeekCommitTimer();
      keyboardSeekCommitTimerRef.current = window.setTimeout(() => {
        keyboardSeekCommitTimerRef.current = null;
        finishKeyboardSeek();
      }, SHORTS_KEYBOARD_SEEK_IDLE_COMMIT_MS);
    };

    const previewKeyboardSeek = (
      delta: number,
      key: ShortsKeyboardSeekKey
    ) => {
      const videoIndex = activeIndexRef.current;
      const activeVideo = getCurrentVideoAtIndex(videoIndex);
      const duration = activeVideo?.duration ?? 0;
      if (!activeVideo || !Number.isFinite(duration) || duration <= 0) return;

      if (keyboardSeekHideTimerRef.current !== null) {
        window.clearTimeout(keyboardSeekHideTimerRef.current);
        keyboardSeekHideTimerRef.current = null;
      }

      const pendingTarget = keyboardSeekTargetRef.current;
      const canContinuePendingTarget =
        pendingTarget?.videoIndex === videoIndex &&
        pendingTarget.video === activeVideo;
      if (pendingTarget && !canContinuePendingTarget) {
        discardKeyboardSeek();
      }

      keyboardSeekHeldKeysRef.current.add(key);
      const baseTime = canContinuePendingTarget
        ? pendingTarget.currentTime
        : activeVideo.currentTime;
      const currentTime = clamp(baseTime + delta, 0, duration);
      const nextTarget = { videoIndex, video: activeVideo, currentTime, duration };
      keyboardSeekTargetRef.current = nextTarget;
      setKeyboardSeekPreview({ videoIndex, currentTime, duration });
      if (!optionsRef.current.isWindowsShortsPlatform) {
        showHud(
          delta > 0 ? "+5秒" : "-5秒",
          <Sparkles size={16} />
        );
      }
      scheduleKeyboardSeekIdleCommit();
    };

    const clearKeyboardRightPressTimer = () => {
      if (keyboardRightPressTimer === null) return;
      window.clearTimeout(keyboardRightPressTimer);
      keyboardRightPressTimer = null;
    };

    const finishKeyboardRightPress = (seekOnShortPress: boolean) => {
      clearKeyboardRightPressTimer();
      const target = keyboardRightPressTarget;
      keyboardRightPressTarget = null;
      if (!target) return;

      if (target.fastPlaybackActive) {
        try {
          target.video.playbackRate = 1;
        } catch {
          // ignore
        }
        setKeyboardFastPlaybackIndex(null);
        return;
      }

      if (
        seekOnShortPress &&
        activeIndexRef.current === target.videoIndex &&
        getCurrentVideoAtIndex(target.videoIndex) === target.video
      ) {
        previewKeyboardSeek(
          SHORTS_KEYBOARD_SEEK_SECONDS,
          "ArrowRight"
        );
        keyboardSeekHeldKeysRef.current.delete("ArrowRight");
        finishKeyboardSeek();
      }
    };

    const startKeyboardRightPress = () => {
      if (keyboardRightPressTarget) return;
      finishKeyboardSeek();
      const videoIndex = activeIndexRef.current;
      const video = getCurrentVideoAtIndex(videoIndex);
      if (!video) return;

      keyboardRightPressTarget = {
        videoIndex,
        video,
        fastPlaybackActive: false,
      };
      keyboardRightPressTimer = window.setTimeout(() => {
        keyboardRightPressTimer = null;
        const target = keyboardRightPressTarget;
        if (
          !target ||
          activeIndexRef.current !== target.videoIndex ||
          getCurrentVideoAtIndex(target.videoIndex) !== target.video ||
          target.video.paused ||
          target.video.ended
        ) {
          return;
        }
        try {
          target.video.playbackRate = 2;
        } catch {
          return;
        }
        target.fastPlaybackActive = true;
        setKeyboardFastPlaybackIndex(target.videoIndex);
      }, SHORTS_KEYBOARD_FAST_PLAYBACK_DELAY_MS);
    };

    const handleKeyDown = (e: KeyboardEvent) => {
      const activeEl = document.activeElement;
      if (
        activeEl &&
        (activeEl.tagName === "INPUT" ||
          activeEl.tagName === "TEXTAREA" ||
          activeEl.tagName === "SELECT" ||
          (activeEl instanceof HTMLElement && activeEl.isContentEditable))
      ) {
        return;
      }

      if (e.key === "ArrowDown") {
        e.preventDefault();
        finishKeyboardRightPress(false);
        finishKeyboardSeek();
        const nextIdx = activeIndexRef.current + 1;
        if (nextIdx < itemsLengthRef.current) {
          const nextSlide = containerRef.current?.querySelector(`[data-index="${nextIdx}"]`);
          if (nextSlide) {
            nextSlide.scrollIntoView({ behavior: "smooth" });
          }
        }
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        finishKeyboardRightPress(false);
        finishKeyboardSeek();
        const prevIdx = activeIndexRef.current - 1;
        if (prevIdx >= 0) {
          const prevSlide = containerRef.current?.querySelector(`[data-index="${prevIdx}"]`);
          if (prevSlide) {
            prevSlide.scrollIntoView({ behavior: "smooth" });
          }
        }
      } else if (e.key === " ") {
        e.preventDefault();
        finishKeyboardRightPress(false);
        finishKeyboardSeek();
        if (e.repeat) return;
        const videoIndex = activeIndexRef.current;
        const activeVideo = getCurrentVideoAtIndex(videoIndex);
        if (!activeVideo) {
          clearKeyboardSpaceTimer();
          return;
        }

        if (
          pendingSpaceTimer !== null &&
          pendingSpaceTarget?.videoIndex === videoIndex &&
          pendingSpaceTarget.video === activeVideo
        ) {
          clearKeyboardSpaceTimer();
          likeActiveVideo(videoIndex);
          return;
        }

        clearKeyboardSpaceTimer();
        scheduleKeyboardSpaceToggle(videoIndex, activeVideo);
      } else if (e.key === "m" || e.key === "M") {
        e.preventDefault();
        finishKeyboardRightPress(false);
        finishKeyboardSeek();
        if (e.repeat) return;
        optionsRef.current.onToggleMute();
      } else if (e.key === "l" || e.key === "L") {
        e.preventDefault();
        finishKeyboardRightPress(false);
        finishKeyboardSeek();
        if (e.repeat) return;
        getActiveLikeButton(activeIndexRef.current)?.click();
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        if (e.repeat) return;
        startKeyboardRightPress();
      } else if (e.key === "ArrowLeft") {
        e.preventDefault();
        finishKeyboardRightPress(false);
        previewKeyboardSeek(
          -SHORTS_KEYBOARD_SEEK_SECONDS,
          "ArrowLeft"
        );
      }
    };

    const handleKeyUp = (e: KeyboardEvent) => {
      if (e.key === "ArrowRight") {
        if (!keyboardRightPressTarget) return;
        e.preventDefault();
        finishKeyboardRightPress(true);
        return;
      }
      if (e.key !== "ArrowLeft") return;
      if (!keyboardSeekTargetRef.current) return;

      e.preventDefault();
      keyboardSeekHeldKeysRef.current.delete(e.key);
      if (keyboardSeekHeldKeysRef.current.size === 0) finishKeyboardSeek();
    };

    const handleVisibilityChange = () => {
      if (!document.hidden) return;
      finishKeyboardRightPress(false);
      finishKeyboardSeek();
      clearKeyboardSpaceTimer();
    };

    const handleWindowBlur = () => {
      finishKeyboardRightPress(false);
      finishKeyboardSeek();
      clearKeyboardSpaceTimer();
    };

    window.addEventListener("keydown", handleKeyDown);
    window.addEventListener("keyup", handleKeyUp);
    window.addEventListener("blur", handleWindowBlur);
    document.addEventListener("visibilitychange", handleVisibilityChange);
    return () => {
      window.removeEventListener("keydown", handleKeyDown);
      window.removeEventListener("keyup", handleKeyUp);
      window.removeEventListener("blur", handleWindowBlur);
      document.removeEventListener("visibilitychange", handleVisibilityChange);
      clearKeyboardSpaceTimer();
      finishKeyboardRightPress(false);
      // 卸载时同步清掉累计 seek 的定时器与目标，避免悬空提交
      clearKeyboardSeekCommitTimer();
      if (keyboardSeekHideTimerRef.current !== null) {
        window.clearTimeout(keyboardSeekHideTimerRef.current);
        keyboardSeekHideTimerRef.current = null;
      }
      keyboardSeekTargetRef.current = null;
      keyboardSeekHeldKeysRef.current.clear();
    };
  }, []);

  return {
    keyboardSeekPreview,
    keyboardFastPlaybackIndex,
    registerKeyboardLikeHandler,
  };
}
