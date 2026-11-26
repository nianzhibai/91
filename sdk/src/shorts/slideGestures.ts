import { clamp } from "./mediaBuffer.js";
import { isShortsSystemGestureStart } from "./gestureBoundary.js";
import { restoreShortsPlaybackRate } from "./playbackRate.js";

const SHORTS_SEEK_ACTIVATION_PX = 12;
const SHORTS_SEEK_DIRECTION_LOCK_RATIO = 1.2;
export type TouchSeekIntent = "pending" | "seek" | "vertical";

export function classifyTouchSeekIntent(dx: number, dy: number): TouchSeekIntent {
  const absX = Math.abs(dx);
  const absY = Math.abs(dy);
  if (absX < SHORTS_SEEK_ACTIVATION_PX && absY < SHORTS_SEEK_ACTIVATION_PX) {
    return "pending";
  }
  return absX < absY * SHORTS_SEEK_DIRECTION_LOCK_RATIO ? "vertical" : "seek";
}

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

export const SHORTS_DOUBLE_TAP_MS = 240;
const SHORTS_DOUBLE_TAP_DISTANCE_PX = 32;
const SHORTS_LONG_PRESS_MS = 400;
const SHORTS_PINCH_ACTIVATION_PX = 24;
const SHORTS_PINCH_ACTIVATION_RATIO = 0.12;
const INTERACTIVE_SELECTOR =
  'button, a, input, select, textarea, [role="button"], [contenteditable], [data-shorts-no-swipe], .shorts-slide__actions';

export type SurfaceGestureHost = {
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
  onPinchScale: (scale: number | null) => void;
};

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

  function endPress(seek: "commit" | "revert" = "commit") {
    clearHoldTimer();
    if (press?.mode === "fast") {
      restoreShortsPlaybackRate(video);
      host.onFastChange(false);
    } else if (press?.mode === "seek") {
      host.onSeekEnd(seek === "commit" ? press.targetTime : press.startTime);
    }
    press = null;
  }

  function cancel(seek: "commit" | "revert" = "commit") {
    cancelTap();
    endPress(seek);
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
    host.onPinchScale(clamp(1 + ratio * 0.35, 0.88, 1.12));
    if (!pinch.committed && Math.abs(delta) >= SHORTS_PINCH_ACTIVATION_PX &&
      Math.abs(ratio) >= SHORTS_PINCH_ACTIVATION_RATIO) {
      pinch.committed = true;
      host.onClearScreenChange(delta > 0);
    }
  }

  function reset() {
    cancel("revert");
    touches.clear();
    multiTouch = false;
  }

  function isSurfaceTarget(target: EventTarget | null) {
    const element = target as Element | null;
    return Boolean(element && surface.contains(element) && !element.closest(INTERACTIVE_SELECTOR));
  }

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
        restoreShortsPlaybackRate(video);
        host.onFastChange(false);
        press.mode = "pending";
      }
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
    if (pendingTap.resumed) host.onImmediateResume();
    tapTimer = window.setTimeout(finishTap, SHORTS_DOUBLE_TAP_MS);
  }

  function handleCancel(event: PointerEvent) {
    touches.delete(event.pointerId);
    if (multiTouch) {
      cancel("revert");
      if (touches.size === 0) multiTouch = false;
      return;
    }
    if (press?.id === event.pointerId) cancel("revert");
  }

  function handleClick(event: MouseEvent) {
    if (!isSurfaceTarget(event.target) || !host.isEnabled()) return;
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
