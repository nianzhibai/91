import { useEffect, useRef } from "react";
import { clamp } from "./mediaBuffer";
import { shouldUsePassiveShortsTouchMove } from "./platform";

const SHORTS_SEEK_ACTIVATION_PX = 12;
const SHORTS_SEEK_DIRECTION_LOCK_RATIO = 1.2;
// 网络视频不需要在每个 touch/pointer move 上都真实 seek。视觉进度仍按帧
// 更新，媒体管线最多约 12.5Hz 跳一次，松手再精确落到最终位置。
export const SHORTS_MEDIA_SEEK_INTERVAL_MS = 80;
// touchend / mouseup 之后浏览器还会补发 click。长按倍速和拖动进度已经
// 消费了这次手势，必须拦住这个合成 click，否则单击逻辑会把视频暂停。
const SHORTS_SYNTHETIC_CLICK_RESET_MS = 700;

type ShortsTouchSeekState = {
  startX: number;
  startY: number;
  startTime: number;
  videoWidth: number;
  mode: "seek" | null;
  targetTime: number;
};

export type TouchSeekIntent = "pending" | "seek" | "vertical";

/**
 * 手势方向判定：横纵位移都小于激活阈值时继续观望；横向没有明显
 * 大于纵向时交还给纵向滚动，否则进入相对快进/快退。
 */
export function classifyTouchSeekIntent(dx: number, dy: number): TouchSeekIntent {
  const absX = Math.abs(dx);
  const absY = Math.abs(dy);
  if (absX < SHORTS_SEEK_ACTIVATION_PX && absY < SHORTS_SEEK_ACTIVATION_PX) {
    return "pending";
  }
  return absX < absY * SHORTS_SEEK_DIRECTION_LOCK_RATIO ? "vertical" : "seek";
}

/** 相对快进：横向位移按视频宽度折算成时长偏移，并夹在 [0, duration]。 */
export function computeTouchSeekTime(input: {
  startTime: number;
  dx: number;
  width: number;
  duration: number;
}): number {
  return clamp(
    input.startTime + (input.dx / Math.max(1, input.width)) * input.duration,
    0,
    input.duration
  );
}

export type ShortsSlideGesturesOptions = {
  getVideoElement: () => HTMLVideoElement | null;
  shouldMount: boolean;
  /** 隐藏遮罩、播放失败等禁用交互的状态下不响应媒体手势 */
  disabled: boolean;
  /** 拖动中标记；媒体事件回调靠它忽略拖动期间的进度更新 */
  scrubbingRef: React.MutableRefObject<boolean>;
  setScrubbing: (scrubbing: boolean) => void;
  setFastActive: (fast: boolean) => void;
  setCurrentTime: (time: number) => void;
  getSeekDuration: (video: HTMLVideoElement | null) => number;
  /** 单击（280ms 内无第二击）：切换播放/暂停 */
  onSingleTap: () => void;
  /** 双击：点赞（相对 slide 左上角的坐标） */
  onDoubleTap: (x: number, y: number) => void;
  /** 自动播放被拒后的首次点击必须在原始 click 回调内直接恢复播放 */
  shouldResumeImmediately: () => boolean;
  onImmediateResume: () => void;
};

/**
 * 一屏短视频的手势输入：
 * - 长按 ≥400ms 进入 2 倍速，松手恢复（与详情页 VideoPlayer 行为一致）
 * - 横向滑动按当前播放点相对快进 / 快退，纵向滑动仍用于切换上下视频
 * - 单击切换播放 / 暂停，双击点赞；长按/拖动后的合成 click 被压制
 * - 底部进度条按 pointer capture 拖动
 */
export function useShortsSlideGestures(options: ShortsSlideGesturesOptions) {
  // 单击和双击的延迟分发：第一次点击挂在定时器里，
  // 280ms 内有第二次就当双击点赞，否则当单击 toggle play
  const clickTimerRef = useRef<number | null>(null);
  const lastClickAtRef = useRef(0);
  const suppressNextClickRef = useRef(false);
  const suppressNextClickResetTimerRef = useRef<number | null>(null);
  // 拖动开始时是否在播：用于拖完后判断要不要 resume
  const wasPlayingRef = useRef(true);
  const optionsRef = useRef(options);
  optionsRef.current = options;
  const previewFrameRef = useRef<number | null>(null);
  const pendingPreviewTimeRef = useRef<number | null>(null);
  const mediaSeekTimerRef = useRef<number | null>(null);
  const pendingMediaSeekRef = useRef<{
    video: HTMLVideoElement;
    time: number;
  } | null>(null);
  const lastMediaSeekAtRef = useRef(-Infinity);
  const progressTargetTimeRef = useRef<number | null>(null);

  function writeMediaSeek(
    video: HTMLVideoElement,
    time: number,
    exact: boolean
  ) {
    try {
      if (!exact && typeof video.fastSeek === "function") {
        video.fastSeek(time);
      } else {
        video.currentTime = time;
      }
      lastMediaSeekAtRef.current = performance.now();
    } catch {
      // 部分 ready state 下 seek 会抛错；后续 move / 最终提交仍会重试。
    }
  }

  function scheduleProgressPreview(time: number) {
    pendingPreviewTimeRef.current = time;
    if (previewFrameRef.current !== null) return;
    previewFrameRef.current = window.requestAnimationFrame(() => {
      previewFrameRef.current = null;
      const pending = pendingPreviewTimeRef.current;
      pendingPreviewTimeRef.current = null;
      if (pending !== null) optionsRef.current.setCurrentTime(pending);
    });
  }

  function flushProgressPreview(time: number) {
    if (previewFrameRef.current !== null) {
      window.cancelAnimationFrame(previewFrameRef.current);
      previewFrameRef.current = null;
    }
    pendingPreviewTimeRef.current = null;
    optionsRef.current.setCurrentTime(time);
  }

  function scheduleMediaSeek(video: HTMLVideoElement, time: number) {
    pendingMediaSeekRef.current = { video, time };
    if (mediaSeekTimerRef.current !== null) return;

    const elapsed = performance.now() - lastMediaSeekAtRef.current;
    const delay = Math.max(0, SHORTS_MEDIA_SEEK_INTERVAL_MS - elapsed);
    if (delay === 0) {
      pendingMediaSeekRef.current = null;
      writeMediaSeek(video, time, false);
      return;
    }

    mediaSeekTimerRef.current = window.setTimeout(() => {
      mediaSeekTimerRef.current = null;
      const pending = pendingMediaSeekRef.current;
      pendingMediaSeekRef.current = null;
      if (pending) writeMediaSeek(pending.video, pending.time, false);
    }, delay);
  }

  function flushMediaSeek(video: HTMLVideoElement, time: number) {
    if (mediaSeekTimerRef.current !== null) {
      window.clearTimeout(mediaSeekTimerRef.current);
      mediaSeekTimerRef.current = null;
    }
    pendingMediaSeekRef.current = null;
    writeMediaSeek(video, time, true);
  }

  function cancelScheduledSeeks() {
    if (previewFrameRef.current !== null) {
      window.cancelAnimationFrame(previewFrameRef.current);
      previewFrameRef.current = null;
    }
    pendingPreviewTimeRef.current = null;
    if (mediaSeekTimerRef.current !== null) {
      window.clearTimeout(mediaSeekTimerRef.current);
      mediaSeekTimerRef.current = null;
    }
    pendingMediaSeekRef.current = null;
    progressTargetTimeRef.current = null;
  }

  function clearClickTimer() {
    if (clickTimerRef.current !== null) {
      window.clearTimeout(clickTimerRef.current);
      clickTimerRef.current = null;
    }
  }

  function clearSuppressNextClickResetTimer() {
    if (suppressNextClickResetTimerRef.current === null) return;
    window.clearTimeout(suppressNextClickResetTimerRef.current);
    suppressNextClickResetTimerRef.current = null;
  }

  function suppressNextSyntheticClick() {
    clearClickTimer();
    clearSuppressNextClickResetTimer();
    suppressNextClickRef.current = true;
    // 少数 WebKit 版本在长按后不会生成 click，届时自动失效，避免误吞掉
    // 用户之后真正的一次轻点。
    suppressNextClickResetTimerRef.current = window.setTimeout(() => {
      suppressNextClickRef.current = false;
      suppressNextClickResetTimerRef.current = null;
    }, SHORTS_SYNTHETIC_CLICK_RESET_MS);
  }

  // 长按 2 倍速：直接绑原生事件
  useEffect(() => {
    const video = optionsRef.current.getVideoElement();
    if (!video) return;
    const passiveTouchMove = shouldUsePassiveShortsTouchMove();
    const { scrubbingRef, setScrubbing, setFastActive } = optionsRef.current;
    let timer: number | null = null;
    let active = false;
    let touchSeekState: ShortsTouchSeekState | null = null;

    const clearTimer = () => {
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
    };
    const start = () => {
      if (optionsRef.current.disabled) return;
      if (video.paused || video.ended) return;
      clearTimer();
      timer = window.setTimeout(() => {
        timer = null;
        if (optionsRef.current.disabled) return;
        if (video.paused || video.ended) return;
        video.playbackRate = 2;
        active = true;
        setFastActive(true);
      }, 400);
    };
    const end = () => {
      clearTimer();
      if (active) {
        active = false;
        video.playbackRate = 1;
        setFastActive(false);
      }
    };

    const resetTouchSeek = () => {
      if (touchSeekState?.mode === "seek") {
        scrubbingRef.current = false;
        setScrubbing(false);
      }
      touchSeekState = null;
    };

    const cancelFastForTouchSeek = () => {
      clearTimer();
      if (active) {
        active = false;
        video.playbackRate = 1;
        setFastActive(false);
      }
    };

    const handleTouchStart = (event: TouchEvent) => {
      if (optionsRef.current.disabled) return;
      if (event.touches.length !== 1) return;
      const touch = event.touches[0];
      touchSeekState = {
        startX: touch.clientX,
        startY: touch.clientY,
        startTime: video.currentTime || 0,
        videoWidth: Math.max(1, video.getBoundingClientRect().width),
        mode: null,
        targetTime: video.currentTime || 0,
      };
      start();
    };

    const handleTouchMove = (event: TouchEvent) => {
      if (optionsRef.current.disabled) {
        resetTouchSeek();
        end();
        return;
      }
      if (!touchSeekState) return;
      if (event.touches.length !== 1) {
        resetTouchSeek();
        end();
        return;
      }

      const touch = event.touches[0];
      const dx = touch.clientX - touchSeekState.startX;
      const dy = touch.clientY - touchSeekState.startY;

      if (!touchSeekState.mode) {
        const intent = classifyTouchSeekIntent(dx, dy);
        if (intent === "pending") return;
        cancelFastForTouchSeek();
        if (intent === "vertical") {
          touchSeekState = null;
          return;
        }
        touchSeekState.mode = "seek";
        scrubbingRef.current = true;
        setScrubbing(true);
        clearClickTimer();
      }

      if (!passiveTouchMove) event.preventDefault();
      const seekDuration = optionsRef.current.getSeekDuration(video);
      if (!seekDuration) return;
      const next = computeTouchSeekTime({
        startTime: touchSeekState.startTime,
        dx,
        width: touchSeekState.videoWidth,
        duration: seekDuration,
      });
      touchSeekState.targetTime = next;
      scheduleProgressPreview(next);
      scheduleMediaSeek(video, next);
    };

    const handleTouchEnd = (event: TouchEvent) => {
      const wasSeeking = touchSeekState?.mode === "seek";
      const wasFastPress = active;
      if (wasSeeking) {
        event.preventDefault();
        const targetTime = touchSeekState?.targetTime;
        if (targetTime !== undefined) {
          flushProgressPreview(targetTime);
          flushMediaSeek(video, targetTime);
        }
      }
      if (wasSeeking || wasFastPress) {
        suppressNextSyntheticClick();
      }
      resetTouchSeek();
      end();
    };

    const handleTouchCancel = () => {
      if (touchSeekState?.mode === "seek") {
        flushProgressPreview(touchSeekState.targetTime);
        flushMediaSeek(video, touchSeekState.targetTime);
      }
      resetTouchSeek();
      end();
    };

    // 桌面现在也能用鼠标拖拽翻页，"按住不动 400ms = 2 倍速"因此必须能被
    // 拖动取消：否则慢一点的拖拽会顺手把视频切成 2 倍速。触摸那侧本来就有
    // 同样的取消（cancelFastForTouchSeek），这里补上鼠标的。
    let mouseDownAt: { x: number; y: number } | null = null;

    const handleMouseDown = (event: MouseEvent) => {
      if (event.button !== 0) return;
      mouseDownAt = { x: event.clientX, y: event.clientY };
      start();
    };

    const handleMouseMove = (event: MouseEvent) => {
      if (!mouseDownAt) return;
      const dx = event.clientX - mouseDownAt.x;
      const dy = event.clientY - mouseDownAt.y;
      // 与横向 seek 共用同一套方向判定的阈值：过了阈值就不再算"按住不动"。
      if (classifyTouchSeekIntent(dx, dy) === "pending") return;
      mouseDownAt = null;
      cancelFastForTouchSeek();
    };

    const handleMouseUp = (event: MouseEvent) => {
      if (event.button !== 0) return;
      mouseDownAt = null;
      const wasFastPress = active;
      if (wasFastPress) suppressNextSyntheticClick();
      end();
    };

    const handleMouseLeave = () => {
      mouseDownAt = null;
      end();
    };

    video.addEventListener("touchstart", handleTouchStart, { passive: true });
    video.addEventListener("touchmove", handleTouchMove, {
      passive: passiveTouchMove,
    });
    video.addEventListener("touchend", handleTouchEnd);
    video.addEventListener("touchcancel", handleTouchCancel);
    video.addEventListener("mousedown", handleMouseDown);
    video.addEventListener("mousemove", handleMouseMove);
    video.addEventListener("mouseup", handleMouseUp);
    video.addEventListener("mouseleave", handleMouseLeave);
    video.addEventListener("pause", end);
    video.addEventListener("ended", end);

    return () => {
      clearTimer();
      cancelScheduledSeeks();
      resetTouchSeek();
      video.removeEventListener("touchstart", handleTouchStart);
      video.removeEventListener("touchmove", handleTouchMove);
      video.removeEventListener("touchend", handleTouchEnd);
      video.removeEventListener("touchcancel", handleTouchCancel);
      video.removeEventListener("mousedown", handleMouseDown);
      video.removeEventListener("mousemove", handleMouseMove);
      video.removeEventListener("mouseup", handleMouseUp);
      video.removeEventListener("mouseleave", handleMouseLeave);
      video.removeEventListener("pause", end);
      video.removeEventListener("ended", end);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [options.getVideoElement, options.shouldMount]);

  // 组件卸载时清理定时器
  useEffect(() => {
    return () => {
      clearClickTimer();
      clearSuppressNextClickResetTimer();
      cancelScheduledSeeks();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  /**
   * 单击 / 双击分发：
   * - 第一次点击：挂一个 280ms 定时器，到时如果还没第二次点击就单击处理
   * - 第二次点击（280ms 内）：清掉定时器，当作双击点赞，不切换播放状态
   */
  function handleSlideClick(e: React.MouseEvent<HTMLElement>) {
    // 隐藏状态下不处理点击
    if (options.disabled) return;
    if (suppressNextClickRef.current) {
      suppressNextClickRef.current = false;
      clearSuppressNextClickResetTimer();
      clearClickTimer();
      e.preventDefault();
      e.stopPropagation();
      return;
    }

    const now = Date.now();
    const delta = now - lastClickAtRef.current;
    lastClickAtRef.current = now;

    // 双击命中
    if (delta < 280 && clickTimerRef.current !== null) {
      clearClickTimer();
      // 在双击位置弹心形动画
      const rect = e.currentTarget.getBoundingClientRect();
      options.onDoubleTap(e.clientX - rect.left, e.clientY - rect.top);
      return;
    }

    // Safari 的有声播放权限按 media element 授予。自动播放被拒后，
    // 用户第一次点击必须在这个原始 click 回调内直接恢复播放，
    // 不能等 280ms 的单/双击定时器，否则会丢失用户激活。
    if (options.shouldResumeImmediately()) {
      clearClickTimer();
      clickTimerRef.current = window.setTimeout(() => {
        clickTimerRef.current = null;
      }, 280);
      options.onImmediateResume();
      return;
    }

    // 单击挂起，等是否有第二次
    clearClickTimer();
    clickTimerRef.current = window.setTimeout(() => {
      clickTimerRef.current = null;
      options.onSingleTap();
    }, 280);
  }

  // ---- 进度条拖动 ----
  // 触摸进度条时：暂停 → 跟随手指更新 currentTime → 松手 resume
  function handleProgressPointerDown(e: React.PointerEvent<HTMLDivElement>) {
    e.preventDefault();
    e.stopPropagation();
    if (options.disabled) return;
    const video = options.getVideoElement();
    const seekDuration = options.getSeekDuration(video);
    if (!video || !seekDuration) return;
    try {
      (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    } catch {
      // ignore
    }
    wasPlayingRef.current = !video.paused;
    if (!video.paused) {
      try {
        video.pause();
      } catch {
        // ignore
      }
    }
    options.scrubbingRef.current = true;
    options.setScrubbing(true);
    progressTargetTimeRef.current = null;
    applyProgressFromEvent(e, seekDuration);
  }
  function handleProgressPointerMove(e: React.PointerEvent<HTMLDivElement>) {
    if (!options.scrubbingRef.current) return;
    e.preventDefault();
    e.stopPropagation();
    applyProgressFromEvent(e);
  }
  function handleProgressPointerEnd(e: React.PointerEvent<HTMLDivElement>) {
    if (!options.scrubbingRef.current) return;
    e.preventDefault();
    e.stopPropagation();
    try {
      (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
    } catch {
      // ignore
    }
    const video = options.getVideoElement();
    const targetTime = progressTargetTimeRef.current;
    if (video && targetTime !== null) {
      flushProgressPreview(targetTime);
      flushMediaSeek(video, targetTime);
    }
    progressTargetTimeRef.current = null;
    options.scrubbingRef.current = false;
    options.setScrubbing(false);
    if (video && wasPlayingRef.current) {
      video.play().catch(() => undefined);
    }
  }
  function applyProgressFromEvent(
    e: React.PointerEvent<HTMLDivElement>,
    knownDuration?: number
  ) {
    const video = options.getVideoElement();
    const seekDuration = knownDuration ?? options.getSeekDuration(video);
    if (!video || !seekDuration) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const ratio = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    const next = ratio * seekDuration;
    progressTargetTimeRef.current = next;
    scheduleProgressPreview(next);
    scheduleMediaSeek(video, next);
  }

  return {
    handleSlideClick,
    handleProgressPointerDown,
    handleProgressPointerMove,
    handleProgressPointerEnd,
  };
}
