import assert from "node:assert/strict";
import test from "node:test";
import {
  createShortsSurfaceGestures,
  SHORTS_DOUBLE_TAP_MS,
} from "../src/shorts/slideGestures";
import { SHORTS_SYSTEM_GESTURE_TOP_PX } from "../src/shorts/gestureBoundary";

function createHarness(options?: { viewportTop?: number }) {
  let now = 1_000;
  let nextTimer = 0;
  const timers = new Map<number, { at: number; run: () => void }>();
  function eventTarget() {
    const listeners = new Map<string, Map<(event: any) => void, boolean>>();
    return {
      addEventListener(type: string, handler: (event: any) => void, options?: boolean | { capture?: boolean }) {
        if (!listeners.has(type)) listeners.set(type, new Map());
        listeners.get(type)!.set(handler, typeof options === "boolean" ? options : Boolean(options?.capture));
      },
      removeEventListener(type: string, handler: (event: any) => void) {
        listeners.get(type)?.delete(handler);
      },
      emit(type: string, event: object = {}, stopsBubbling = false) {
        for (const [handler, capture] of listeners.get(type) ?? []) {
          if (!stopsBubbling || capture) handler(event);
        }
      },
      get listenerCount() {
        return [...listeners.values()].reduce((sum, handlers) => sum + handlers.size, 0);
      },
    };
  }
  const surfaceTarget = { closest: () => null };
  const buttonTarget = { closest: () => ({ tagName: "BUTTON" }) };
  const outsideTarget = { closest: () => null };
  const surface = {
    ...eventTarget(),
    contains: (target: unknown) => target === surfaceTarget || target === buttonTarget,
    getBoundingClientRect: () => ({ left: 10, top: 20 }),
  };
  const video = {
    ...eventTarget(),
    paused: false,
    ended: false,
    playbackRate: 1,
    defaultPlaybackRate: 1,
    currentTime: 30,
    getBoundingClientRect: () => ({ width: 300 }),
  };
  const browser = {
    ...eventTarget(),
    visualViewport: { offsetTop: options?.viewportTop ?? 0 },
    setTimeout(run: () => void, delay: number) {
      const id = ++nextTimer;
      timers.set(id, { at: now + delay, run });
      return id;
    },
    clearTimeout(id: number) { timers.delete(id); },
  };
  const originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
  const originalPerformance = Object.getOwnPropertyDescriptor(globalThis, "performance");
  Object.defineProperty(globalThis, "window", { configurable: true, value: browser });
  Object.defineProperty(globalThis, "performance", { configurable: true, value: { now: () => now } });
  const state = {
    enabled: true,
    blocked: false,
    singles: 0,
    doubles: [] as Array<[number, number]>,
    resumes: 0,
    fast: false,
    seeking: false,
    seekPreviews: [] as number[],
    seekEnds: [] as number[],
    clearScreen: false,
    clearChanges: [] as boolean[],
    pinchScale: null as number | null,
  };
  const destroy = createShortsSurfaceGestures({
    surface: surface as unknown as HTMLElement,
    video: video as unknown as HTMLVideoElement,
    isEnabled: () => state.enabled,
    onSingleTap() { state.singles++; video.paused = !video.paused; },
    onDoubleTap(x, y) { state.doubles.push([x, y]); },
    shouldResumeImmediately: () => state.blocked,
    onImmediateResume() { state.resumes++; state.blocked = false; video.paused = false; },
    onFastChange(fast) { state.fast = fast; },
    getSeekDuration: () => 120,
    onSeekStart() { state.seeking = true; },
    onSeekPreview(time) { state.seekPreviews.push(time); },
    onSeekEnd(time) { state.seeking = false; state.seekEnds.push(time); },
    onClearScreenChange(clear) { state.clearScreen = clear; state.clearChanges.push(clear); },
    onPinchScale(scale) { state.pinchScale = scale; },
  });
  function pointer(type: string, x = 100, y = 200, extra: Record<string, unknown> = {}) {
    const event = {
      pointerId: 1, pointerType: "touch", isPrimary: true, button: 0,
      clientX: x, clientY: y, target: surfaceTarget, cancelable: true,
      preventDefault() {}, ...extra,
    };
    // window 的监听器在捕获阶段运行，控件停止冒泡也不会漏掉抬手。
    browser.emit(type, event, Boolean(extra.stopsBubbling));
    if (type === "pointerdown") surface.emit(type, event);
  }
  function advance(ms: number) {
    const end = now + ms;
    for (;;) {
      const next = [...timers].filter(([, timer]) => timer.at <= end)
        .sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      now = next[1].at;
      timers.delete(next[0]);
      next[1].run();
    }
    now = end;
  }
  return {
    state, video, surface, browser, buttonTarget, outsideTarget, pointer, advance, destroy,
    tap(x = 100, y = 200, extra: Record<string, unknown> = {}) {
      pointer("pointerdown", x, y, extra);
      advance(40);
      pointer("pointerup", x, y, extra);
    },
    click(detail = 1, pointerType = "touch") {
      surface.emit("click", { detail, pointerType, target: surfaceTarget });
    },
    restore() {
      destroy();
      if (originalWindow) Object.defineProperty(globalThis, "window", originalWindow);
      else delete (globalThis as { window?: unknown }).window;
      if (originalPerformance) Object.defineProperty(globalThis, "performance", originalPerformance);
      assert.equal(timers.size, 0);
      assert.equal(surface.listenerCount + browser.listenerCount + video.listenerCount, 0);
    },
  };
}

type Harness = ReturnType<typeof createHarness>;
function withHarness(run: (h: Harness) => void, options?: { viewportTop?: number }) {
  const h = createHarness(options);
  try { run(h); } finally { h.restore(); }
}

test("a tap pauses without a browser click and duplicate clicks do not toggle again", () => withHarness(h => {
  h.tap();
  h.advance(SHORTS_DOUBLE_TAP_MS - 1);
  assert.equal(h.video.paused, false);
  h.advance(1);
  assert.equal(h.video.paused, true);
  h.click();
  h.click(0, "touch");
  h.advance(1_000);
  assert.equal(h.state.singles, 1);
}));

test("a second press reserves the double tap before the single deadline expires", () => withHarness(h => {
  h.tap();
  h.advance(220);
  h.pointer("pointerdown", 110, 205);
  h.advance(100);
  assert.equal(h.state.singles, 0);
  h.pointer("pointerup", 110, 205);
  h.advance(1_000);
  assert.deepEqual(h.state.doubles, [[100, 185]]);
  assert.equal(h.state.singles, 0);
  assert.equal(h.video.paused, false);
}));

test("a double tap on a user-paused video likes without starting playback", () => withHarness(h => {
  h.video.paused = true;
  h.tap(); h.advance(80); h.tap(); h.advance(500);
  assert.equal(h.state.doubles.length, 1);
  assert.equal(h.state.singles + h.state.resumes, 0);
  assert.equal(h.video.paused, true);
}));

test("distant taps and taps outside the time window remain separate singles", () => withHarness(h => {
  h.tap(); h.advance(30); h.tap(180, 200); h.advance(500);
  assert.equal(h.state.singles, 2);
  assert.equal(h.state.doubles.length, 0);
  h.tap(); h.advance(300); h.tap(); h.advance(500);
  assert.equal(h.state.singles, 4);
  assert.equal(h.state.doubles.length, 0);
}));

test("finger jitter stays a tap and final movement is checked even without move events", () => withHarness(h => {
  h.pointer("pointerdown");
  h.pointer("pointermove", 108, 206);
  h.pointer("pointerup", 109, 207);
  h.advance(300);
  assert.equal(h.state.singles, 1);
  h.pointer("pointerdown"); h.pointer("pointerup", 100, 240); h.advance(300);
  assert.equal(h.state.singles, 1);
}));

test("long press resets speed and the very next tap works when no synthetic click arrives", () => withHarness(h => {
  h.pointer("pointerdown"); h.advance(410);
  assert.equal(h.video.playbackRate, 2);
  h.pointer("pointerup");
  assert.equal(h.video.playbackRate, 1);
  assert.equal(h.state.fast, false);
  h.tap(); h.advance(300);
  assert.equal(h.state.singles, 1);
  assert.equal(h.video.paused, true);
}));

test("releasing or canceling a long press restores the selected playback speed", () => withHarness(h => {
  h.video.defaultPlaybackRate = 1.5;
  h.video.playbackRate = 1.5;
  h.pointer("pointerdown"); h.advance(410);
  assert.equal(h.video.playbackRate, 2);
  h.pointer("pointerup");
  assert.equal(h.video.playbackRate, 1.5);
  h.pointer("pointerdown"); h.advance(410);
  h.pointer("pointercancel");
  assert.equal(h.video.playbackRate, 1.5);
  assert.equal(h.state.fast, false);
}));

test("holding a second press cancels the pending single and does not like", () => withHarness(h => {
  h.tap(); h.advance(100); h.pointer("pointerdown"); h.advance(500); h.pointer("pointerup");
  h.click(); h.advance(500);
  assert.equal(h.state.singles + h.state.doubles.length, 0);
  assert.equal(h.video.playbackRate, 1);
}));

test("a long press while paused does not start playback on release", () => withHarness(h => {
  h.video.paused = true;
  h.pointer("pointerdown"); h.advance(500); h.pointer("pointerup"); h.click(); h.advance(500);
  assert.equal(h.video.paused, true);
  assert.equal(h.video.playbackRate, 1);
  assert.equal(h.state.singles, 0);
}));

test("horizontal seeking commits the release position and the next tap still works", () => withHarness(h => {
  h.pointer("pointerdown"); h.pointer("pointermove", 130, 200);
  assert.equal(h.state.seeking, true);
  assert.equal(h.state.seekPreviews.at(-1), 42);
  h.pointer("pointerup", 160, 200);
  assert.deepEqual(h.state.seekEnds, [54]);
  assert.equal(h.state.seeking, false);
  h.click(); h.tap(); h.advance(300);
  assert.equal(h.state.singles, 1);
}));

test("vertical swipes cancel a pending tap, including when release is captured by the pager", () => withHarness(h => {
  h.tap(); h.advance(80); h.pointer("pointerdown");
  h.pointer("pointermove", 100, 240);
  h.pointer("pointerup", 100, 300, { target: h.outsideTarget });
  h.click(); h.advance(500);
  assert.equal(h.state.singles + h.state.doubles.length, 0);
  h.tap(); h.advance(300);
  assert.equal(h.state.singles, 1);
}));

test("pressing a control cancels a pending tap and does not become a surface gesture", () => withHarness(h => {
  h.tap(); h.advance(60);
  h.pointer("pointerdown", 100, 200, { target: h.buttonTarget });
  h.pointer("pointerup", 100, 200, { target: h.buttonTarget });
  h.advance(500);
  assert.equal(h.state.singles + h.state.doubles.length, 0);
  h.tap(); h.advance(300);
  assert.equal(h.state.singles, 1);
}));

test("multi-touch and pointer cancellation discard the whole tap sequence", () => withHarness(h => {
  h.tap(); h.advance(60); h.pointer("pointerdown");
  h.pointer("pointerdown", 130, 200, { pointerId: 2, isPrimary: false });
  h.pointer("pointerup", 130, 200, { pointerId: 2, isPrimary: false });
  h.pointer("pointerup"); h.advance(500);
  assert.equal(h.state.singles + h.state.doubles.length, 0);
  h.tap(); h.pointer("pointerdown"); h.pointer("pointercancel"); h.advance(500);
  assert.equal(h.state.singles + h.state.doubles.length, 0);
}));

test("system-edge touches never pause, seek or enter long-press playback", () => {
  for (const viewportTop of [0, 40]) withHarness(h => {
    const startY = viewportTop + SHORTS_SYSTEM_GESTURE_TOP_PX;
    h.pointer("pointerdown", 100, startY);
    h.advance(500);
    assert.equal(h.video.playbackRate, 1);
    assert.equal(h.state.fast, false);
    h.pointer("pointermove", 250, 200);
    h.pointer("pointerup", 250, 200);
    h.advance(500);
    h.tap(100, startY);
    h.advance(300);
    assert.equal(h.state.singles, 0);
    assert.equal(h.video.paused, false);
    assert.deepEqual(h.state.seekPreviews, []);
    assert.deepEqual(h.state.seekEnds, []);

    h.tap();
    h.advance(300);
    assert.equal(h.state.singles, 1);
  }, { viewportTop });
});

test("starting a system-edge gesture cancels a pending video tap", () => withHarness(h => {
  h.tap();
  h.advance(60);
  h.pointer("pointerdown", 100, 10);
  h.advance(500);
  h.pointer("pointercancel", 100, 200);
  assert.equal(h.state.singles, 0);
  assert.equal(h.video.paused, false);
}));

test("a finger starting in the system-edge area cannot join a video pinch", () => withHarness(h => {
  h.pointer("pointerdown", 100, 10);
  h.pointer("pointerdown", 200, 200, { pointerId: 2, isPrimary: false });
  h.pointer("pointermove", 400, 200, { pointerId: 2, isPrimary: false });
  h.pointer("pointerup", 400, 200, { pointerId: 2, isPrimary: false });
  h.pointer("pointerup", 100, 200);
  assert.equal(h.state.pinchScale, null);
  assert.deepEqual(h.state.clearChanges, []);
}));

test("mouse taps at the top edge still control playback", () => withHarness(h => {
  h.tap(100, 10, { pointerType: "mouse" });
  h.advance(300);
  assert.equal(h.state.singles, 1);
}));

test("deactivation and disposal prevent delayed taps from affecting the next video", () => withHarness(h => {
  h.tap(); h.state.enabled = false; h.advance(500);
  assert.equal(h.state.singles, 0);
  h.state.enabled = true; h.tap(); h.destroy(); h.advance(500);
  assert.equal(h.state.singles, 0);
}));

test("disposal restores playback speed and removes all listeners and timers", () => withHarness(h => {
  h.pointer("pointerdown"); h.advance(410); h.destroy();
  assert.equal(h.video.playbackRate, 1);
  assert.equal(h.state.fast, false);
}));

test("autoplay recovery happens inside pointerup and double tap never toggles it back", () => withHarness(h => {
  h.state.blocked = true; h.video.paused = true;
  h.tap();
  assert.equal(h.state.resumes, 1);
  assert.equal(h.video.paused, false);
  h.advance(80); h.tap(); h.advance(500);
  assert.equal(h.state.doubles.length, 1);
  assert.equal(h.state.singles, 0);
}));

test("mouse double clicks use the same exclusive dispatch and drag cancels tapping", () => withHarness(h => {
  const mouse = { pointerType: "mouse" };
  h.tap(100, 200, mouse); h.click(1, "mouse");
  h.advance(80); h.tap(100, 200, mouse); h.click(2, "mouse"); h.advance(500);
  assert.equal(h.state.doubles.length, 1);
  assert.equal(h.state.singles, 0);
  h.pointer("pointerdown", 100, 200, mouse); h.pointer("pointermove", 150, 200, mouse);
  h.pointer("pointerup", 150, 200, mouse); h.click(1, "mouse"); h.advance(500);
  assert.equal(h.state.singles, 0);
  assert.equal(h.state.seekPreviews.length, 0);
}));

test("a third tap starts a fresh single after a double tap", () => withHarness(h => {
  h.tap(); h.advance(60); h.tap(); h.advance(60); h.tap(); h.advance(500);
  assert.equal(h.state.doubles.length, 1);
  assert.equal(h.state.singles, 1);
}));

test("assistive click activation still works without a pointer sequence", () => withHarness(h => {
  h.click(0, "");
  assert.equal(h.state.singles, 1);
  assert.equal(h.video.paused, true);
}));

const secondFinger = { pointerId: 2, isPrimary: false };

test("spreading clears the screen with bounded enlargement, pinching restores with shrink feedback", () => withHarness(h => {
  h.pointer("pointerdown", 100, 200);
  h.pointer("pointerdown", 200, 200, secondFinger);
  h.pointer("pointermove", 230, 200, secondFinger);
  assert.equal(h.state.clearScreen, true);
  assert.ok(h.state.pinchScale! > 1);
  h.pointer("pointermove", 500, 200, secondFinger);
  assert.equal(h.state.pinchScale, 1.12);
  h.pointer("pointerup", 500, 200, secondFinger);
  assert.equal(h.state.pinchScale, null);
  h.pointer("pointerup", 100, 200);

  h.pointer("pointerdown", 100, 200);
  h.pointer("pointerdown", 300, 200, secondFinger);
  h.pointer("pointermove", 250, 200, secondFinger);
  assert.equal(h.state.clearScreen, false);
  assert.ok(h.state.pinchScale! < 1);
  h.pointer("pointermove", 110, 200, secondFinger);
  assert.equal(h.state.pinchScale, 0.88);
  h.pointer("pointerup", 110, 200, secondFinger);
  h.pointer("pointerup", 100, 200);
  h.advance(500);
  assert.deepEqual(h.state.clearChanges, [true, false]);
  assert.equal(h.state.pinchScale, null);
  assert.equal(h.video.paused, false);
  assert.equal(h.video.currentTime, 30);
  assert.equal(h.state.singles + h.state.doubles.length + h.state.seekPreviews.length, 0);
}));

test("small two-finger jitter does not change mode and each pinch commits only once", () => withHarness(h => {
  h.pointer("pointerdown", 100, 200);
  h.pointer("pointerdown", 200, 200, secondFinger);
  h.pointer("pointermove", 210, 205, secondFinger);
  h.pointer("pointerup", 210, 205, secondFinger);
  h.pointer("pointerup", 100, 200);
  assert.deepEqual(h.state.clearChanges, []);
  h.pointer("pointerdown", 100, 200);
  h.pointer("pointerdown", 200, 200, secondFinger);
  h.pointer("pointermove", 240, 200, secondFinger);
  h.pointer("pointermove", 160, 200, secondFinger);
  h.pointer("pointerup", 160, 200, secondFinger);
  h.pointer("pointerup", 100, 200);
  assert.deepEqual(h.state.clearChanges, [true]);
}));

test("vertical finger separation and final release coordinates also recognize a pinch", () => withHarness(h => {
  h.pointer("pointerdown", 100, 200);
  h.pointer("pointerdown", 100, 300, secondFinger);
  h.pointer("pointerup", 100, 340, secondFinger);
  h.pointer("pointerup", 100, 200);
  assert.deepEqual(h.state.clearChanges, [true]);
  assert.equal(h.state.pinchScale, null);
}));

test("pinch takes over pending taps, long press and seeking without triggering them again", () => withHarness(h => {
  h.tap(); h.advance(60);
  h.pointer("pointerdown", 100, 200);
  h.pointer("pointerdown", 200, 200, secondFinger);
  h.pointer("pointermove", 240, 200, secondFinger);
  h.advance(500);
  assert.equal(h.video.playbackRate, 1);
  h.pointer("pointerup", 240, 200, secondFinger);
  // 剩下的手指继续横拖也不能恢复快进或变成轻点。
  h.pointer("pointermove", 170, 200);
  h.pointer("pointerup", 170, 200);
  h.click(); h.advance(500);
  assert.equal(h.state.singles + h.state.doubles.length + h.state.seekPreviews.length, 0);

  h.pointer("pointerdown", 100, 200); h.advance(410);
  assert.equal(h.video.playbackRate, 2);
  h.pointer("pointerdown", 200, 200, secondFinger);
  assert.equal(h.video.playbackRate, 1);
  assert.equal(h.state.fast, false);
  h.pointer("pointerup", 200, 200, secondFinger);
  h.pointer("pointerup", 100, 200);

  h.pointer("pointerdown", 100, 200);
  h.pointer("pointermove", 130, 200);
  h.pointer("pointerdown", 230, 200, secondFinger);
  assert.equal(h.state.seeking, false);
  assert.deepEqual(h.state.seekEnds, [42]);
  h.pointer("pointermove", 270, 200, secondFinger);
  h.pointer("pointerup", 270, 200, secondFinger);
  h.pointer("pointerup", 130, 200);
  assert.equal(h.state.seekPreviews.length, 1);
  h.tap(); h.advance(300);
  assert.equal(h.state.singles, 1);
}));

test("pinches beginning on controls or outside the slide never change viewing mode", () => withHarness(h => {
  for (const target of [h.buttonTarget, h.outsideTarget]) {
    for (const controlFirst of [true, false]) {
      h.pointer("pointerdown", 100, 200, controlFirst ? { target } : {});
      h.pointer("pointerdown", 200, 200, { ...secondFinger, ...(controlFirst ? {} : { target }) });
      h.pointer("pointermove", 260, 200, secondFinger);
      h.pointer("pointerup", 260, 200, secondFinger);
      h.pointer("pointerup", 100, 200);
    }
  }
  h.advance(500);
  assert.deepEqual(h.state.clearChanges, []);
  assert.equal(h.state.pinchScale, null);
  assert.equal(h.state.singles, 0);
}));

test("controls stopping pointerup or pointercancel propagation do not leave stale touches", () => {
  for (const release of ["pointerup", "pointercancel"]) withHarness(h => {
    h.pointer("pointerdown", 100, 700, { target: h.buttonTarget });
    h.pointer(release, 100, 700, { target: h.buttonTarget, stopsBubbling: true });
    h.pointer("pointerdown", 100, 200);
    h.pointer("pointerdown", 200, 200, secondFinger);
    h.pointer("pointermove", 240, 200, secondFinger);
    h.pointer("pointerup", 240, 200, secondFinger);
    h.pointer("pointerup", 100, 200);
    assert.deepEqual(h.state.clearChanges, [true]);
    assert.equal(h.state.pinchScale, null);
  });
});

test("a third finger cancels pinch feedback until all fingers lift", () => withHarness(h => {
  h.pointer("pointerdown", 100, 200);
  h.pointer("pointerdown", 200, 200, secondFinger);
  h.pointer("pointermove", 210, 200, secondFinger);
  h.pointer("pointerdown", 300, 200, { pointerId: 3, isPrimary: false });
  assert.equal(h.state.pinchScale, null);
  h.pointer("pointerup", 300, 200, { pointerId: 3, isPrimary: false });
  h.pointer("pointermove", 270, 200, secondFinger);
  h.pointer("pointerup", 270, 200, secondFinger);
  h.pointer("pointerup", 100, 200);
  assert.deepEqual(h.state.clearChanges, []);
  h.tap(); h.advance(300);
  assert.equal(h.state.singles, 1);
}));

test("cancellation, deactivation, blur and disposal release pinch feedback without committing", () => {
  for (const end of ["cancel", "disable", "blur", "destroy"]) withHarness(h => {
    h.pointer("pointerdown", 100, 200);
    h.pointer("pointerdown", 200, 200, secondFinger);
    h.pointer("pointermove", 210, 200, secondFinger);
    if (end === "cancel") h.pointer("pointercancel", 270, 200, secondFinger);
    if (end === "disable") { h.state.enabled = false; h.pointer("pointermove", 270, 200, secondFinger); }
    if (end === "blur") h.browser.emit("blur");
    if (end === "destroy") h.destroy();
    assert.equal(h.state.pinchScale, null);
    assert.deepEqual(h.state.clearChanges, []);
    h.pointer("pointerup", 270, 200, secondFinger);
    h.pointer("pointerup", 100, 200);
    h.advance(500);
    assert.equal(h.state.singles, 0);
  });
});
