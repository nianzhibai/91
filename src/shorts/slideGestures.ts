import { clamp } from "./mediaBuffer";
import { isShortsSystemGestureStart } from "./gestureBoundary";

const SHORTS_SEEK_ACTIVATION_PX = 12;
const SHORTS_SEEK_DIRECTION_LOCK_RATIO = 1.2;
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

// 从抬手开始计时；第二次按下就锁定候选，避免等第二次抬手时单击已经执行。
export const SHORTS_DOUBLE_TAP_MS = 240;
const SHORTS_DOUBLE_TAP_DISTANCE_PX = 32;
const SHORTS_LONG_PRESS_MS = 400;
const SHORTS_PINCH_ACTIVATION_PX = 24;
const SHORTS_PINCH_ACTIVATION_RATIO = 0.12;
const INTERACTIVE_SELECTOR =
  'button, a, input, select, textarea, [role="button"], [contenteditable], [data-shorts-no-swipe], .shorts-slide__actions';

type SurfaceGestureHost = {
  surface: HTMLElement;
  video: HTMLVideoElement;
  isEnabled: () => boolean;
  onSingleTap: () => void;
  onDoubleTap: (x: number, y: number) => void;
  shouldResumeImmediately: () => boolean;
  onImmediateResume: () => void;
  onFastChange: (fast: boolean) => void;
  getSeekDuration: () => number;
  onSeekStart: () => void;
  onSeekPreview: (time: number) => void;
  onSeekEnd: (time: number) => void;
  onClearScreenChange: (clear: boolean) => void;
  /** null 表示手势结束，画面应回弹到原尺寸。 */
  onPinchScale: (scale: number | null) => void;
};

/** 双指接管时结束单指操作，直到所有手指抬起都不再识别轻点或横滑。 */
export function createShortsSurfaceGestures(host: SurfaceGestureHost) {
  const { surface, video } = host;
  const touches = new Map<number, { x: number; y: number; eligible: boolean }>();
  let multiTouch = false;
  let pinch: { startDistance: number; committed: boolean } | null = null;
  let press: {
    id: number;
    pointerType: string;
    x: number;
    y: number;
    at: number;
    startTime: number;
    width: number;
    targetTime: number;
    mode: "pending" | "fast" | "seek";
    secondTap: boolean;
  } | null = null;
  let pendingTap: {
    x: number;
    y: number;
    at: number;
    pointerType: string;
    resumed: boolean;
  } | null = null;
  let tapTimer: number | null = null;
  let holdTimer: number | null = null;

  function clearTapTimer() {
    if (tapTimer !== null) window.clearTimeout(tapTimer);
    tapTimer = null;
  }

  function cancelTap() {
    clearTapTimer();
    pendingTap = null;
  }

  function finishTap() {
    const tap = pendingTap;
    cancelTap();
    if (tap && !tap.resumed && host.isEnabled()) host.onSingleTap();
  }

  function clearHoldTimer() {
    if (holdTimer !== null) window.clearTimeout(holdTimer);
    holdTimer = null;
  }

  function endPress() {
    clearHoldTimer();
    if (press?.mode === "fast") {
      video.playbackRate = 1;
      host.onFastChange(false);
    } else if (press?.mode === "seek") {
      host.onSeekEnd(press.targetTime);
    }
    press = null;
  }

  function cancel() {
    cancelTap();
    endPress();
    endPinch();
  }

  function endPinch() {
    if (!pinch) return;
    pinch = null;
    host.onPinchScale(null);
  }

  function touchDistance() {
    const [first, second] = [...touches.values()];
    return Math.hypot(second.x - first.x, second.y - first.y);
  }

  function updatePinch() {
    if (!pinch) return;
    const distance = touchDistance();
    const delta = distance - pinch.startDistance;
    const ratio = delta / Math.max(1, pinch.startDistance);
    // 阻尼和幅度上限让它只是短暂反馈，不把清屏变成持续裁切的视频缩放。
    host.onPinchScale(clamp(1 + ratio * 0.35, 0.88, 1.12));
    if (!pinch.committed && Math.abs(delta) >= SHORTS_PINCH_ACTIVATION_PX &&
      Math.abs(ratio) >= SHORTS_PINCH_ACTIVATION_RATIO) {
      pinch.committed = true;
      host.onClearScreenChange(delta > 0);
    }
  }

  function reset() {
    cancel();
    touches.clear();
    multiTouch = false;
  }

  function isSurfaceTarget(target: EventTarget | null) {
    const element = target as Element | null;
    return Boolean(element && surface.contains(element) && !element.closest(INTERACTIVE_SELECTOR));
  }

  // 另一根手指、进度条或按钮接管输入时，挂起的单击不能随后改变播放状态。
  function handleGlobalDown(event: PointerEvent) {
    if (event.pointerType === "touch" && host.isEnabled()) {
      touches.set(event.pointerId, {
        x: event.clientX,
        y: event.clientY,
        eligible: isSurfaceTarget(event.target) && !isShortsSystemGestureStart(event),
      });
      if (touches.size > 1) {
        const canPinch = !multiTouch && touches.size === 2 &&
          [...touches.values()].every(touch => touch.eligible);
        multiTouch = true;
        cancel();
        if (canPinch) {
          pinch = { startDistance: touchDistance(), committed: false };
          host.onPinchScale(1);
        }
        return;
      }
    }
    if (
      isShortsSystemGestureStart(event) || !isSurfaceTarget(event.target) ||
      !event.isPrimary || (press && press.id !== event.pointerId)
    ) {
      cancel();
    }
  }

  function handleDown(event: PointerEvent) {
    if (
      !host.isEnabled() || multiTouch || !event.isPrimary || event.button !== 0 ||
      !isSurfaceTarget(event.target) || isShortsSystemGestureStart(event)
    ) return;
    endPress();
    const now = performance.now();
    const secondTap = Boolean(
      pendingTap && pendingTap.pointerType === event.pointerType &&
      now - pendingTap.at <= SHORTS_DOUBLE_TAP_MS &&
      Math.hypot(event.clientX - pendingTap.x, event.clientY - pendingTap.y) <= SHORTS_DOUBLE_TAP_DISTANCE_PX
    );
    if (secondTap) clearTapTimer();
    else finishTap();
    press = {
      id: event.pointerId,
      pointerType: event.pointerType,
      x: event.clientX,
      y: event.clientY,
      at: now,
      startTime: video.currentTime || 0,
      width: Math.max(1, video.getBoundingClientRect().width),
      targetTime: video.currentTime || 0,
      mode: "pending",
      secondTap,
    };
    holdTimer = window.setTimeout(() => {
      holdTimer = null;
      if (!press) return;
      cancelTap();
      // 暂停中的长按也不是轻点，但不启动倍速播放。
      if (!host.isEnabled() || video.paused || video.ended) {
        endPress();
        return;
      }
      press.mode = "fast";
      video.playbackRate = 2;
      host.onFastChange(true);
    }, SHORTS_LONG_PRESS_MS);
  }

  function handleMove(event: PointerEvent) {
    const touch = touches.get(event.pointerId);
    if (touch) {
      touch.x = event.clientX;
      touch.y = event.clientY;
    }
    if (multiTouch) {
      if (!host.isEnabled()) return cancel();
      if (pinch && touch) {
        if (event.cancelable) event.preventDefault();
        updatePinch();
      }
      return;
    }
    if (!press || event.pointerId !== press.id) return;
    if (!host.isEnabled()) return cancel();
    const dx = event.clientX - press.x;
    const dy = event.clientY - press.y;
    if (press.mode !== "seek") {
      const intent = classifyTouchSeekIntent(dx, dy);
      if (intent === "pending") return;
      clearHoldTimer();
      cancelTap();
      if (press.mode === "fast") {
        video.playbackRate = 1;
        host.onFastChange(false);
        press.mode = "pending";
      }
      // 竖滑留给翻页控制器，鼠标横拖也不能变成轻点。
      if (intent === "vertical" || event.pointerType === "mouse") return endPress();
      press.mode = "seek";
      host.onSeekStart();
    }
    if (event.cancelable) event.preventDefault();
    const duration = host.getSeekDuration();
    if (!duration) return;
    press.targetTime = computeTouchSeekTime({
      startTime: press.startTime,
      dx,
      width: press.width,
      duration,
    });
    host.onSeekPreview(press.targetTime);
  }

  function handleUp(event: PointerEvent) {
    if (multiTouch) {
      handleMove(event);
      touches.delete(event.pointerId);
      endPinch();
      if (touches.size === 0) multiTouch = false;
      return;
    }
    touches.delete(event.pointerId);
    if (!press || event.pointerId !== press.id) return;
    // 最终坐标也参与判定，避免漏发最后一次 move 时把拖动算成轻点。
    handleMove(event);
    const ended = press;
    endPress();
    if (!ended || ended.mode !== "pending" || !host.isEnabled()) return;
    if (performance.now() - ended.at >= SHORTS_LONG_PRESS_MS) return cancelTap();
    if (ended.secondTap && pendingTap) {
      cancelTap();
      const rect = surface.getBoundingClientRect();
      host.onDoubleTap(event.clientX - rect.left, event.clientY - rect.top);
      return;
    }
    pendingTap = {
      x: event.clientX,
      y: event.clientY,
      at: performance.now(),
      pointerType: event.pointerType,
      resumed: host.shouldResumeImmediately(),
    };
    // 仅自动播放被浏览器拒绝时直接恢复，保留 Safari 的用户激活权限。
    if (pendingTap.resumed) host.onImmediateResume();
    tapTimer = window.setTimeout(finishTap, SHORTS_DOUBLE_TAP_MS);
  }

  function handleCancel(event: PointerEvent) {
    touches.delete(event.pointerId);
    if (multiTouch) {
      cancel();
      if (touches.size === 0) multiTouch = false;
      return;
    }
    if (press?.id === event.pointerId) cancel();
  }

  function handleClick(event: MouseEvent) {
    if (!isSurfaceTarget(event.target) || !host.isEnabled()) return;
    // 触摸和鼠标都已经在 pointerup 处理；只为无指针的辅助技术激活保留 click。
    if (event.detail !== 0 || (event as PointerEvent).pointerType) return;
    cancel();
    host.onSingleTap();
  }

  function handlePause() {
    clearHoldTimer();
    if (press?.mode === "fast") endPress();
  }

  surface.addEventListener("pointerdown", handleDown);
  surface.addEventListener("click", handleClick);
  window.addEventListener("pointerdown", handleGlobalDown, true);
  // 进度条等控件会停止冒泡；捕获阶段仍须收到抬手，避免留下幽灵触点。
  window.addEventListener("pointermove", handleMove, { passive: false, capture: true });
  window.addEventListener("pointerup", handleUp, true);
  window.addEventListener("pointercancel", handleCancel, true);
  window.addEventListener("blur", reset);
  video.addEventListener("pause", handlePause);
  video.addEventListener("ended", handlePause);
  return () => {
    reset();
    surface.removeEventListener("pointerdown", handleDown);
    surface.removeEventListener("click", handleClick);
    window.removeEventListener("pointerdown", handleGlobalDown, true);
    window.removeEventListener("pointermove", handleMove, true);
    window.removeEventListener("pointerup", handleUp, true);
    window.removeEventListener("pointercancel", handleCancel, true);
    window.removeEventListener("blur", reset);
    video.removeEventListener("pause", handlePause);
    video.removeEventListener("ended", handlePause);
  };
}
