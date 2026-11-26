/** 桌面 wheel 输入按手势翻页，不把设备上报的距离直接映射成播放速度。 */
/** 小幅触控板轻扫可触发，微小抖动仍需过滤；鼠标一格通常远大于此值。 */
export const SHORTS_WHEEL_STEP_PX = 24;
export const SHORTS_WHEEL_GESTURE_GAP_MS = 200;
export const SHORTS_WHEEL_LINE_HEIGHT_PX = 16;
export const SHORTS_WHEEL_SETTLE_MS = 250;
export const SHORTS_WHEEL_SETTLE_EASING = "ease";

const RECENT_DELTA_SAMPLES = 3;
const RENEWAL_WINDOW_MS = 100;
const RENEWAL_MIN_DELTA_PX = 8;
const RENEWAL_GROWTH_RATIO = 1.5;
const RENEWAL_GROWTH_PX = 4;

/** 把像素、行和页三种 deltaMode 统一换算成像素。 */
export function normalizeShortsWheelDelta(input: {
  deltaY: number;
  deltaMode: number;
  viewportHeight: number;
}): number {
  if (!Number.isFinite(input.deltaY)) return 0;
  if (input.deltaMode === 1) return input.deltaY * SHORTS_WHEEL_LINE_HEIGHT_PX;
  if (input.deltaMode === 2) {
    return input.deltaY * Math.max(1, input.viewportHeight);
  }
  return input.deltaY;
}

export type ShortsWheelGestureState = {
  lastEventAt: number;
  recentDeltas: number[];
  direction: -1 | 0 | 1;
  accumulated: number;
  consumed: boolean;
  renewalStartedAt: number;
  renewalSamples: number;
};

export const INITIAL_SHORTS_WHEEL_STATE: ShortsWheelGestureState = {
  lastEventAt: Number.NEGATIVE_INFINITY,
  recentDeltas: [],
  direction: 0,
  accumulated: 0,
  consumed: false,
  renewalStartedAt: Number.NEGATIVE_INFINITY,
  renewalSamples: 0,
};

export function resolveShortsWheelStep(input: {
  state: ShortsWheelGestureState;
  deltaPx: number;
  now: number;
  canStep: boolean;
}): { state: ShortsWheelGestureState; step: -1 | 0 | 1 } {
  const { state, deltaPx, now, canStep } = input;
  if (!Number.isFinite(deltaPx) || deltaPx === 0) return { state, step: 0 };

  const direction = deltaPx > 0 ? 1 : -1;
  const newGesture = now - state.lastEventAt >= SHORTS_WHEEL_GESTURE_GAP_MS;
  const next: ShortsWheelGestureState = {
    lastEventAt: now,
    recentDeltas: newGesture
      ? [deltaPx]
      : [...state.recentDeltas.slice(1 - RECENT_DELTA_SAMPLES), deltaPx],
    direction: newGesture ? direction : state.direction,
    accumulated: newGesture ? 0 : state.accumulated,
    consumed: !newGesture && state.consumed,
    renewalStartedAt: newGesture ? Number.NEGATIVE_INFINITY : state.renewalStartedAt,
    renewalSamples: newGesture ? 0 : state.renewalSamples,
  };

  if (!canStep) {
    return {
      state: { ...next, direction, accumulated: 0, consumed: true, renewalSamples: 0 },
      step: 0,
    };
  }

  if (next.consumed) {
    if (direction !== next.direction) {
      next.accumulated =
        (Math.sign(next.accumulated) === direction ? next.accumulated : 0) + deltaPx;
      next.renewalSamples = 0;
    } else {
      const magnitude = Math.abs(deltaPx);
      const previousDelta = state.recentDeltas[state.recentDeltas.length - 1] ?? 0;
      const previousMagnitude = Math.abs(previousDelta);
      const recentPeak = Math.max(...state.recentDeltas.map((delta) => Math.abs(delta)));
      const continuingRenewal =
        next.renewalSamples > 0 &&
        now - next.renewalStartedAt <= RENEWAL_WINDOW_MS &&
        magnitude > previousMagnitude;
      if (continuingRenewal) {
        next.accumulated += deltaPx;
        next.renewalSamples += 1;
      } else if (
        magnitude >= RENEWAL_MIN_DELTA_PX &&
        Math.sign(previousDelta) === direction &&
        magnitude >= recentPeak * RENEWAL_GROWTH_RATIO &&
        magnitude - recentPeak >= RENEWAL_GROWTH_PX
      ) {
        next.accumulated = deltaPx;
        next.renewalStartedAt = now;
        next.renewalSamples = 1;
      } else {
        next.accumulated = 0;
        next.renewalSamples = 0;
      }
      if (next.renewalSamples < 2) return { state: next, step: 0 };
    }
  } else {
    next.accumulated =
      (Math.sign(next.accumulated) === direction ? next.accumulated : 0) + deltaPx;
  }

  if (Math.abs(next.accumulated) < SHORTS_WHEEL_STEP_PX) {
    return { state: next, step: 0 };
  }
  return {
    state: { ...next, direction, accumulated: 0, consumed: true, renewalSamples: 0 },
    step: next.accumulated > 0 ? 1 : -1,
  };
}
