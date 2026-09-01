import assert from "node:assert/strict";
import test from "node:test";
import {
  SHORTS_PAGER_MAX_SETTLE_MS,
  createShortsSwipePager,
} from "../src/shorts/useShortsSwipePager";

// 事件序列级别的回归测试：判定公式已经在 shortsSwipePager.test.ts 里单独覆盖，
// 这里验证的是把它们串起来的状态机——跟手位移、方向让路、多指中断、点击是否
// 被吞、动画被队列裁剪平移之后还能不能正确落点。

const SLIDE_HEIGHT = 900;

type TouchPoint = { clientX: number; clientY: number };

type Harness = ReturnType<typeof createHarness>;

function createHarness(options?: { slideCount?: number }) {
  const slideCount = options?.slideCount ?? 6;
  let clock = 1_000;
  let nextFrameId = 1;
  let nextTimerId = 1;
  const frames = new Map<number, (now: number) => void>();
  const timers = new Map<number, { at: number; run: () => void }>();
  const rootListeners = new Map<string, (event: unknown) => void>();
  const windowListeners = new Map<string, (event: unknown) => void>();

  const root = {
    scrollTop: 0,
    clientHeight: SLIDE_HEIGHT,
    scrollHeight: slideCount * SLIDE_HEIGHT,
    getBoundingClientRect: () => ({ top: 0 }),
    querySelectorAll: () => slides,
    addEventListener: (type: string, handler: (event: unknown) => void) => {
      rootListeners.set(type, handler);
    },
    removeEventListener: (type: string) => {
      rootListeners.delete(type);
    },
  };

  const slides = Array.from({ length: slideCount }, (_, index) => ({
    dataset: { index: String(index) },
    getBoundingClientRect: () => ({
      top: index * SLIDE_HEIGHT - root.scrollTop,
    }),
  }));

  const originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
  const originalDocument = Object.getOwnPropertyDescriptor(
    globalThis,
    "document"
  );
  const originalPerformance = Object.getOwnPropertyDescriptor(
    globalThis,
    "performance"
  );

  const fakeWindow = {
    innerHeight: SLIDE_HEIGHT,
    scrollY: 0,
    scrollTo: () => undefined,
    requestAnimationFrame: (callback: (now: number) => void) => {
      const id = nextFrameId++;
      frames.set(id, callback);
      return id;
    },
    cancelAnimationFrame: (id: number) => {
      frames.delete(id);
    },
    setTimeout: (run: () => void, delay: number) => {
      const id = nextTimerId++;
      timers.set(id, { at: clock + delay, run });
      return id;
    },
    clearTimeout: (id: number) => {
      timers.delete(id);
    },
    addEventListener: (type: string, handler: (event: unknown) => void) => {
      windowListeners.set(type, handler);
    },
    removeEventListener: (type: string) => {
      windowListeners.delete(type);
    },
  };

  define("window", fakeWindow);
  define("document", { documentElement: { scrollHeight: root.scrollHeight } });
  define("performance", { now: () => clock });

  function define(name: string, value: unknown) {
    Object.defineProperty(globalThis, name, {
      value,
      configurable: true,
      writable: true,
    });
  }

  function restore(name: string, descriptor: PropertyDescriptor | undefined) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else delete (globalThis as Record<string, unknown>)[name];
  }

  let anchorIndex = 0;
  const destroyPager = createShortsSwipePager({
    root: root as unknown as HTMLElement,
    usesDocumentScroll: false,
    getAnchorSlide: () =>
      (slides[anchorIndex] ?? null) as unknown as HTMLElement | null,
  });

  const prevented = { touchmove: 0, touchend: 0 };

  function fire(type: string, event: Record<string, unknown>) {
    const handler = rootListeners.get(type);
    assert.ok(handler, `${type} listener should be registered`);
    handler({
      cancelable: true,
      preventDefault: () => {
        if (type === "touchmove") prevented.touchmove += 1;
        if (type === "touchend") prevented.touchend += 1;
      },
      ...event,
    });
  }

  function point(x: number, y: number): TouchPoint {
    return { clientX: x, clientY: y };
  }

  return {
    root,
    slides,
    prevented,
    get clock() {
      return clock;
    },
    get pendingFrames() {
      return frames.size;
    },
    setAnchorIndex(index: number) {
      anchorIndex = index;
    },
    tick(ms: number) {
      clock += ms;
    },
    /** 推进时钟并跑掉这一轮排队的帧与到期定时器。 */
    advance(ms: number) {
      clock += ms;
      const due = [...frames.entries()];
      frames.clear();
      for (const [, callback] of due) callback(clock);
      for (const [id, timer] of [...timers.entries()]) {
        if (timer.at <= clock) {
          timers.delete(id);
          timer.run();
        }
      }
    },
    /** 一直跑到落点动画结束。 */
    runSettle(maxFrames = 200) {
      let guard = 0;
      while (frames.size > 0 && guard < maxFrames) {
        this.advance(16);
        guard += 1;
      }
      assert.ok(guard < maxFrames, "settle animation should terminate");
    },
    touchStart(x: number, y: number, target?: unknown) {
      fire("touchstart", { touches: [point(x, y)], target: target ?? null });
    },
    touchStartMulti(points: TouchPoint[]) {
      fire("touchstart", { touches: points, target: null });
    },
    touchMove(x: number, y: number) {
      fire("touchmove", { touches: [point(x, y)] });
    },
    touchMoveMulti(points: TouchPoint[]) {
      fire("touchmove", { touches: points });
    },
    touchEnd(x: number, y: number) {
      fire("touchend", { changedTouches: [point(x, y)], touches: [] });
    },
    touchCancel() {
      fire("touchcancel", { touches: [] });
    },
    clickGuardActive() {
      return rootListeners.has("click");
    },
    /** 返回这次 click 有没有被吞掉（吞掉 = 不会传到 slide 的单击处理上）。 */
    fireClick() {
      const handler = rootListeners.get("click");
      if (!handler) return false;
      let swallowed = false;
      handler({
        stopPropagation: () => {
          swallowed = true;
        },
        preventDefault: () => undefined,
      });
      return swallowed;
    },
    resize() {
      const handler = windowListeners.get("resize");
      assert.ok(handler, "resize listener should be registered");
      handler({});
    },
    hasListeners() {
      return rootListeners.size > 0 || windowListeners.size > 0;
    },
    destroy() {
      destroyPager?.();
      restore("window", originalWindow);
      restore("document", originalDocument);
      restore("performance", originalPerformance);
    },
  };
}

function withHarness(
  run: (harness: Harness) => void,
  options?: { slideCount?: number }
) {
  const harness = createHarness(options);
  try {
    run(harness);
  } finally {
    harness.destroy();
  }
}

// ---------------------------------------------------------------------------
// 跟手
// ---------------------------------------------------------------------------

test("the feed follows the finger one to one once the direction is settled", () => {
  withHarness((h) => {
    h.touchStart(200, 700);
    // 未过方向判定阈值前不动，长按倍速 / 点击仍有机会成立
    h.tick(16);
    h.touchMove(200, 692);
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.prevented.touchmove, 0);

    // 判定成立的那一帧把阈值位移归零，画面不会突然跳 12px
    h.tick(16);
    h.touchMove(200, 680);
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.prevented.touchmove, 1);

    // 之后逐像素跟手
    h.tick(16);
    h.touchMove(200, 600);
    assert.equal(h.root.scrollTop, 80);
    h.tick(16);
    h.touchMove(200, 640);
    assert.equal(h.root.scrollTop, 40);
  });
});

test("dragging never exposes a third video, however far the finger goes", () => {
  withHarness((h) => {
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, -5_000);
    // 最多停在下一屏，不会滑过它
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("the first video does not scroll above the top of the feed", () => {
  withHarness((h) => {
    h.touchStart(200, 100);
    h.tick(16);
    h.touchMove(200, 120);
    h.tick(16);
    h.touchMove(200, 5_000);
    assert.equal(h.root.scrollTop, 0);
  });
});

// ---------------------------------------------------------------------------
// 松手判定
// ---------------------------------------------------------------------------

test("a quick flick switches to the next video and settles onto it", () => {
  withHarness((h) => {
    h.touchStart(200, 700);
    h.tick(16);
    h.touchMove(200, 670);
    h.tick(16);
    h.touchMove(200, 640);
    h.tick(16);
    h.touchEnd(200, 640);
    // 竖滑吞掉浏览器补发的合成 click，否则会被当成单击而暂停视频
    assert.equal(h.prevented.touchend, 1);
    h.runSettle();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("a slow short drag bounces back to the current video", () => {
  withHarness((h) => {
    h.touchStart(200, 700);
    h.tick(16);
    h.touchMove(200, 680);
    h.tick(400);
    h.touchMove(200, 660);
    assert.ok(h.root.scrollTop > 0);
    h.touchEnd(200, 660);
    h.runSettle();
    assert.equal(h.root.scrollTop, 0);
  });
});

test("a slow but long drag still switches, exactly like Douyin", () => {
  withHarness((h) => {
    const past = SLIDE_HEIGHT / 3 + 40;
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(2_000);
    h.touchMove(200, 800 - past);
    h.touchEnd(200, 800 - past);
    h.runSettle();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("swiping back down returns to the previous video", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT * 2;
    h.touchStart(200, 200);
    h.tick(16);
    h.touchMove(200, 230);
    h.tick(16);
    h.touchMove(200, 300);
    h.touchEnd(200, 300);
    h.runSettle();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("the last video has nowhere to go and simply bounces back", () => {
  withHarness(
    (h) => {
      h.root.scrollTop = SLIDE_HEIGHT * 2;
      h.touchStart(200, 800);
      h.tick(16);
      h.touchMove(200, 780);
      h.tick(16);
      h.touchMove(200, 400);
      h.touchEnd(200, 400);
      h.runSettle();
      assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
    },
    { slideCount: 3 }
  );
});

// ---------------------------------------------------------------------------
// 手势兼容
// ---------------------------------------------------------------------------

test("horizontal seeking on the video keeps the feed completely still", () => {
  withHarness((h) => {
    h.touchStart(200, 700);
    h.tick(16);
    h.touchMove(260, 706);
    h.tick(16);
    h.touchMove(400, 712);
    assert.equal(h.root.scrollTop, 0);
    // 没有接管就不能挡掉默认行为，也不能吞掉这次点击
    assert.equal(h.prevented.touchmove, 0);
    h.touchEnd(400, 712);
    assert.equal(h.prevented.touchend, 0);
    assert.equal(h.pendingFrames, 0);
  });
});

test("a plain tap stays a tap", () => {
  withHarness((h) => {
    h.touchStart(200, 700);
    h.tick(80);
    h.touchEnd(200, 700);
    assert.equal(h.prevented.touchend, 0);
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.pendingFrames, 0);
  });
});

test("touches that start on the progress bar never page the feed", () => {
  withHarness((h) => {
    const progress = { closest: (selector: string) =>
      selector === "[data-shorts-no-swipe]" ? progress : null };
    h.touchStart(200, 700, progress);
    h.tick(16);
    h.touchMove(200, 400);
    assert.equal(h.root.scrollTop, 0);
    h.touchEnd(200, 400);
    assert.equal(h.prevented.touchend, 0);
  });
});

test("a second finger hands the gesture back and snaps to the nearest video", () => {
  withHarness((h) => {
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, 200);
    // 800→780 是方向判定那一段，被归零；之后的 580px 才是画面位移
    assert.equal(h.root.scrollTop, 580);

    h.touchMoveMulti([
      { clientX: 200, clientY: 200 },
      { clientX: 260, clientY: 400 },
    ]);
    h.runSettle();
    // 600 更靠近下一屏（900）而不是当前屏（0）
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);

    // 交出去之后的移动完全不再影响滚动
    h.touchMove(200, 100);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("a multi-touch start is ignored outright", () => {
  withHarness((h) => {
    h.touchStartMulti([
      { clientX: 100, clientY: 700 },
      { clientX: 300, clientY: 700 },
    ]);
    h.tick(16);
    h.touchMove(100, 300);
    assert.equal(h.root.scrollTop, 0);
  });
});

test("a cancelled touch settles instead of freezing between videos", () => {
  withHarness((h) => {
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, 500);
    assert.equal(h.root.scrollTop, 280);
    h.touchCancel();
    h.runSettle();
    assert.equal(h.root.scrollTop, 0);
  });
});

// ---------------------------------------------------------------------------
// 连续滑动 / 打断
// ---------------------------------------------------------------------------

test("grabbing a flying slide takes over from where it actually is", () => {
  withHarness((h) => {
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, 700);
    h.touchEnd(200, 700);
    // 只跑一帧，动画停在半路
    h.advance(16);
    const midFlight = h.root.scrollTop;
    assert.ok(midFlight > 0 && midFlight < SLIDE_HEIGHT);

    // 按住：动画立即停下，不再有排队的帧
    h.touchStart(200, 500);
    assert.equal(h.pendingFrames, 0);
    assert.equal(h.root.scrollTop, midFlight);

    // 接着再甩一把，仍然只前进一屏
    h.tick(16);
    h.touchMove(200, 470);
    h.tick(16);
    h.touchMove(200, 440);
    h.touchEnd(200, 440);
    h.runSettle();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("tapping to stop a flying slide still lands on a video", () => {
  withHarness((h) => {
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, 700);
    h.touchEnd(200, 700);
    h.advance(16);
    const midFlight = h.root.scrollTop;
    assert.ok(midFlight > 0 && midFlight < SLIDE_HEIGHT);

    h.touchStart(200, 500);
    h.tick(40);
    h.touchEnd(200, 500);
    // 这一下只是"停住"，不该被当成单击去暂停视频
    assert.equal(h.prevented.touchend, 2);
    h.runSettle();
    assert.ok(
      h.root.scrollTop === 0 || h.root.scrollTop === SLIDE_HEIGHT,
      "should land on a slide boundary"
    );
  });
});

test("settling never runs longer than the tuned ceiling", () => {
  withHarness((h) => {
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(3_000);
    h.touchMove(200, 400);
    h.touchEnd(200, 400);
    const startedAt = h.clock;
    h.runSettle();
    assert.ok(h.clock - startedAt <= SHORTS_PAGER_MAX_SETTLE_MS + 32);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

// ---------------------------------------------------------------------------
// 与队列裁剪 / 视口变化共存
// ---------------------------------------------------------------------------

test("a queue trim mid-flight shifts the animation instead of derailing it", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT * 3;
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, 700);
    h.touchEnd(200, 700);
    h.advance(16);

    // 裁掉队首 2 条：坐标系整体上移，画面位置不变
    const shift = SLIDE_HEIGHT * 2;
    h.root.scrollTop -= shift;
    h.runSettle();
    // 落点跟着一起平移，仍然停在原来那条视频的下一条上
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 4 - shift);
  });
});

test("a queue trim mid-drag keeps the finger tracking continuous", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT * 3;
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, 700);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 3 + 80);

    const shift = SLIDE_HEIGHT * 2;
    h.root.scrollTop -= shift;
    h.tick(16);
    // 手指再往上 20px：位置应当是"平移后的位置"再加 20，而不是跳回旧坐标
    h.touchMove(200, 680);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 3 + 100 - shift);
  });
});

test("a viewport resize realigns the active video", () => {
  withHarness((h) => {
    h.setAnchorIndex(2);
    h.root.scrollTop = SLIDE_HEIGHT * 2 + 37;
    h.resize();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);

    // 后续的补偿帧和定时器不会把位置再挪走
    h.advance(16);
    h.advance(300);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
  });
});

test("a resize during a drag leaves the finger in control", () => {
  withHarness((h) => {
    h.setAnchorIndex(0);
    h.touchStart(200, 800);
    h.tick(16);
    h.touchMove(200, 780);
    h.tick(16);
    h.touchMove(200, 600);
    assert.equal(h.root.scrollTop, 180);
    h.resize();
    assert.equal(h.root.scrollTop, 180);
  });
});

// ---------------------------------------------------------------------------
// 合成 click 兜底
// ---------------------------------------------------------------------------

function flickToNext(h: Harness) {
  h.touchStart(200, 700);
  h.tick(16);
  h.touchMove(200, 670);
  h.tick(16);
  h.touchMove(200, 640);
  h.touchEnd(200, 640);
}

test("the click a vertical swipe leaves behind never pauses the video", () => {
  withHarness((h) => {
    assert.equal(h.clickGuardActive(), false);
    flickToNext(h);
    assert.equal(h.clickGuardActive(), true);
    assert.equal(h.fireClick(), true);
    // 只吞一次，之后立刻恢复
    assert.equal(h.clickGuardActive(), false);
  });
});

test("a deliberate tap right after a swipe still reaches the video", () => {
  withHarness((h) => {
    flickToNext(h);
    assert.equal(h.clickGuardActive(), true);
    // 用户重新按下：守卫立即失效，这一下轻点的 click 能原样穿过去
    h.touchStart(200, 500);
    assert.equal(h.clickGuardActive(), false);
    assert.equal(h.fireClick(), false);
  });
});

test("once the feed has settled a tap is just a tap again", () => {
  withHarness((h) => {
    flickToNext(h);
    h.runSettle();
    h.touchStart(200, 500);
    h.tick(60);
    h.touchEnd(200, 500);
    // 只有那次竖滑挡过默认行为，这一下轻点原样放行
    assert.equal(h.prevented.touchend, 1);
    assert.equal(h.clickGuardActive(), false);
  });
});

test("the click guard expires on its own if no click ever arrives", () => {
  withHarness((h) => {
    flickToNext(h);
    assert.equal(h.clickGuardActive(), true);
    h.advance(400);
    assert.equal(h.clickGuardActive(), false);
  });
});

test("horizontal seeking leaves the click guard alone", () => {
  withHarness((h) => {
    h.touchStart(200, 700);
    h.tick(16);
    h.touchMove(280, 706);
    h.touchEnd(280, 706);
    assert.equal(h.clickGuardActive(), false);
  });
});

test("tearing down the page releases every listener", () => {
  const harness = createHarness();
  assert.ok(harness.hasListeners());
  harness.destroy();
  assert.equal(harness.hasListeners(), false);
});
