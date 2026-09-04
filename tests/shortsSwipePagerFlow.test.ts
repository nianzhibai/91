import assert from "node:assert/strict";
import test from "node:test";
import {
  SHORTS_PAGER_COMMIT_SETTLE_MS,
  SHORTS_PAGER_MAX_BOUNCE_MS,
  SHORTS_PAGER_SETTLE_FALLBACK_MS,
  createShortsSwipePager,
  parseTranslateY,
} from "../src/shorts/useShortsSwipePager";

// 事件序列级别的回归测试：判定公式已经在 shortsSwipePager.test.ts 里单独覆盖，
// 这里验证把它们串起来的状态机——跟手位移、方向让路、多指中断、点击是否被吞，
// 以及 transform → scrollTop 的落点提交在各种打断下还对不对。
//
// 坐标模型（与实现一致）：手势 / 动画进行中 scrollTop 不动，位移全部记在轨道
// 的 translateY 上；等效滚动位置恒为 scrollTop - translate。落点提交时把位移
// 写回 scrollTop 并清掉 transform。

const SLIDE_HEIGHT = 900;
const PRIMARY_POINTER = 1;
const SECOND_POINTER = 2;

type TouchPoint = { clientX: number; clientY: number };

type Harness = ReturnType<typeof createHarness>;
type HarnessOptions = {
  slideCount?: number;
  usesDocumentScroll?: boolean;
};

function createHarness(options?: HarnessOptions) {
  const slideCount = options?.slideCount ?? 6;
  const usesDocumentScroll = options?.usesDocumentScroll ?? false;
  let documentScrollTop = 0;
  let clock = 1_000;
  let nextFrameId = 1;
  let nextTimerId = 1;
  const frames = new Map<number, (now: number) => void>();
  const timers = new Map<number, { at: number; run: () => void }>();
  const rootListeners = new Map<string, (event: unknown) => void>();
  const windowListeners = new Map<string, (event: unknown) => void>();
  const trackListeners = new Map<string, Set<(event: unknown) => void>>();

  /**
   * 动画进行中，内联 transform 记的是终点值，屏幕上是中间值。
   * override 非 null 时代表"transition 正在跑"，几何按它算。
   */
  let liveTranslateOverride: number | null = null;

  const trackStyle = {
    _transform: "",
    _transition: "",
    _willChange: "",
    get transform() {
      return this._transform;
    },
    set transform(value: string) {
      this._transform = value;
      transformWrites.push(value);
    },
    get transition() {
      return this._transition;
    },
    set transition(value: string) {
      this._transition = value;
      // transition 关掉时屏幕上立刻等于内联值，不再有中间态。
      if (value === "" || value === "none") liveTranslateOverride = null;
    },
    get willChange() {
      return this._willChange;
    },
    set willChange(value: string) {
      this._willChange = value;
    },
  };

  const inlineTranslate = () => parseTranslateY(trackStyle.transform);
  const visualTranslate = () => liveTranslateOverride ?? inlineTranslate();

  /** 每次写入 transform 的历史，用来验证 FLIP 的补偿值确实落到了样式上。 */
  const transformWrites: string[] = [];

  let styleFlushes = 0;

  const track = {
    style: trackStyle,
    getBoundingClientRect: () => {
      // 实现只在 FLIP 里读轨道自身的 rect，用来强制一次样式提交
      styleFlushes += 1;
      return { top: -readScrollTop() + visualTranslate() };
    },
    addEventListener: (type: string, handler: (event: unknown) => void) => {
      if (!trackListeners.has(type)) trackListeners.set(type, new Set());
      trackListeners.get(type)!.add(handler);
    },
    removeEventListener: (type: string, handler: (event: unknown) => void) => {
      trackListeners.get(type)?.delete(handler);
    },
  };

  const slides: Array<{
    dataset: { index: string };
    getBoundingClientRect: () => { top: number };
  }> = [];
  for (let index = 0; index < slideCount; index += 1) {
    const slide = {
      dataset: { index: String(index) },
      getBoundingClientRect: () => ({
        top:
          slides.indexOf(slide) * SLIDE_HEIGHT -
          readScrollTop() +
          visualTranslate(),
      }),
    };
    slides.push(slide);
  }

  const root = {
    scrollTop: 0,
    clientHeight: SLIDE_HEIGHT,
    setPointerCapture: () => undefined,
    releasePointerCapture: () => undefined,
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

  const readScrollTop = () =>
    usesDocumentScroll ? documentScrollTop : root.scrollTop;

  const originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
  const originalDocument = Object.getOwnPropertyDescriptor(globalThis, "document");
  const originalPerformance = Object.getOwnPropertyDescriptor(
    globalThis,
    "performance"
  );

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

  const fakeWindow = {
    innerHeight: SLIDE_HEIGHT,
    get scrollY() {
      return documentScrollTop;
    },
    scrollTo: (_left: number, top: number) => {
      documentScrollTop = top;
    },
    // 浏览器给的是矩阵形式，实现必须认得
    getComputedStyle: () => ({
      transform:
        liveTranslateOverride !== null
          ? `matrix(1, 0, 0, 1, 0, ${liveTranslateOverride})`
          : trackStyle.transform || "none",
    }),
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

  let anchorIndex = 0;
  const destroyPager = createShortsSwipePager({
    root: root as unknown as HTMLElement,
    track: track as unknown as HTMLElement,
    usesDocumentScroll,
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
        if (type === "pointermove") prevented.touchmove += 1;
        if (type === "pointerup") prevented.touchend += 1;
      },
      ...event,
    });
  }

  function point(x: number, y: number) {
    return { clientX: x, clientY: y };
  }

  return {
    root,
    track,
    slides,
    prevented,
    get clock() {
      return clock;
    },
    get pendingFrames() {
      return frames.size;
    },
    /** 内联 transform 上的位移（动画进行中是终点值）。 */
    get translate() {
      return inlineTranslate();
    },
    /** 屏幕上此刻的位移。 */
    get visualTranslate() {
      return visualTranslate();
    },
    get transitionSpec() {
      return trackStyle.transition;
    },
    get willChange() {
      return trackStyle.willChange;
    },
    get transformStyle() {
      return trackStyle.transform;
    },
    get transformWrites() {
      return [...transformWrites];
    },
    get styleFlushes() {
      return styleFlushes;
    },
    /** 落点动画是否在等 transitionend。 */
    get awaitingTransition() {
      return (trackListeners.get("transitionend")?.size ?? 0) > 0;
    },
    /** 从 `transform 300ms ...` 里取出时长。 */
    get settleDurationMs() {
      const match = /transform (\d+)ms/.exec(trackStyle.transition);
      return match ? Number(match[1]) : 0;
    },
    setAnchorIndex(index: number) {
      anchorIndex = index;
    },
    /** 模拟动画跑到中途：屏幕上的位移停在 px。 */
    setLiveTranslate(px: number) {
      liveTranslateOverride = px;
    },
    /** 模拟 transition 正常跑完。 */
    endTransition(propertyName = "transform") {
      liveTranslateOverride = null;
      const handlers = [...(trackListeners.get("transitionend") ?? [])];
      for (const handler of handlers) handler({ target: track, propertyName });
    },
    /** 模拟一个从子元素冒泡上来、或别的属性的 transitionend。 */
    fireForeignTransitionEnd(options: { fromChild?: boolean; property?: string }) {
      const handlers = [...(trackListeners.get("transitionend") ?? [])];
      for (const handler of handlers) {
        handler({
          target: options.fromChild ? slides[0] : track,
          propertyName: options.property ?? "transform",
        });
      }
    },
    /** 模拟长会话队列裁剪：砍掉队首 n 条，并按实现的方式重贴 scrollTop。 */
    trimLeading(count: number) {
      slides.splice(0, count);
      root.scrollHeight -= count * SLIDE_HEIGHT;
      root.scrollTop -= count * SLIDE_HEIGHT;
    },
    tick(ms: number) {
      clock += ms;
    },
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
    clickGuardActive() {
      return rootListeners.has("click");
    },
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
    /** 主指针按下。pointerType 默认 touch，测鼠标时显式传 mouse。 */
    pointerDown(
      x: number,
      y: number,
      options?: { target?: unknown; pointerType?: string; button?: number; id?: number }
    ) {
      fire("pointerdown", {
        pointerId: options?.id ?? PRIMARY_POINTER,
        pointerType: options?.pointerType ?? "touch",
        button: options?.button ?? 0,
        ...point(x, y),
        target: options?.target ?? null,
      });
    },
    pointerMove(x: number, y: number, id = PRIMARY_POINTER) {
      fire("pointermove", { pointerId: id, ...point(x, y) });
    },
    pointerUp(x: number, y: number, id = PRIMARY_POINTER) {
      fire("pointerup", { pointerId: id, ...point(x, y) });
    },
    pointerCancel(id = PRIMARY_POINTER) {
      fire("pointercancel", { pointerId: id });
    },
    /** 两根手指同时落下。 */
    pointerDownMulti(points: TouchPoint[]) {
      points.forEach((p, i) => {
        fire("pointerdown", {
          pointerId: PRIMARY_POINTER + i,
          pointerType: "touch",
          button: 0,
          clientX: p.clientX,
          clientY: p.clientY,
          target: null,
        });
      });
    },
    /** 容器发出一次 scroll 事件（外力挪动了滚动位置）。 */
    scroll() {
      const listeners = usesDocumentScroll ? windowListeners : rootListeners;
      const handler = listeners.get("scroll");
      assert.ok(handler, "scroll listener should be registered");
      handler({});
    },
    get scrollTop() {
      return readScrollTop();
    },
    setExternalScrollTop(value: number) {
      if (usesDocumentScroll) documentScrollTop = value;
      else root.scrollTop = value;
    },
    /** 一次 wheel 事件。deltaY 正 = 向下滚 = 看下一条。 */
    wheel(
      deltaY: number,
      options?: { deltaMode?: number; ctrlKey?: boolean; target?: unknown }
    ) {
      fire("wheel", {
        deltaY,
        deltaMode: options?.deltaMode ?? 0,
        ctrlKey: options?.ctrlKey ?? false,
        target: options?.target ?? null,
      });
    },
    /**
     * 拖动途中第二根手指落下。多指判定现在发生在 pointerdown 上（而不是
     * 像 touch 事件那样靠 touches.length），语义更直接。
     */
    secondFingerDownAt(points: TouchPoint[]) {
      const second = points[points.length - 1];
      fire("pointerdown", {
        pointerId: SECOND_POINTER,
        pointerType: "touch",
        button: 0,
        clientX: second.clientX,
        clientY: second.clientY,
        target: null,
      });
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
  options?: HarnessOptions
) {
  const harness = createHarness(options);
  try {
    run(harness);
  } finally {
    harness.destroy();
  }
}

/** 一次干净的快速上滑（切下一条）。 */
function flickToNext(h: Harness, fromY = 700) {
  h.pointerDown(200, fromY);
  h.tick(16);
  h.pointerMove(200, fromY - 30);
  h.tick(16);
  h.pointerMove(200, fromY - 60);
  h.pointerUp(200, fromY - 60);
}

// ---------------------------------------------------------------------------
// 跟手：位移记在轨道上，scrollTop 全程不动
// ---------------------------------------------------------------------------

test("the feed follows the finger one to one once the direction is settled", () => {
  withHarness((h) => {
    h.pointerDown(200, 700);
    // 未过方向判定阈值前不动，长按倍速 / 点击仍有机会成立
    h.tick(16);
    h.pointerMove(200, 692);
    assert.equal(h.translate, 0);
    assert.equal(h.prevented.touchmove, 0);

    // 判定成立的那一帧把阈值位移归零，画面不会突然跳 12px
    h.tick(16);
    h.pointerMove(200, 680);
    assert.equal(h.translate, 0);
    assert.equal(h.prevented.touchmove, 1);
    // 接管后才提升合成层，静止时不留
    assert.equal(h.willChange, "transform");

    // 之后逐像素跟手
    h.tick(16);
    h.pointerMove(200, 600);
    assert.equal(h.translate, -80);
    h.tick(16);
    h.pointerMove(200, 640);
    assert.equal(h.translate, -40);

    // 全程 scrollTop 不动：IntersectionObserver 之外的几何都还在原位
    assert.equal(h.root.scrollTop, 0);
  });
});

test("the drag runs on the compositor, never through scrollTop", () => {
  withHarness((h) => {
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, 600);
    assert.equal(h.transformStyle, "translate3d(0, -180px, 0)");
    assert.equal(h.transitionSpec, "none");
    assert.equal(h.root.scrollTop, 0);
  });
});

test("dragging never exposes a third video, however far the finger goes", () => {
  withHarness((h) => {
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, -5_000);
    // 相邻一屏之外只剩带阻尼的越界余量，第三条永远露不出来
    const overshoot = -SLIDE_HEIGHT - h.translate;
    assert.ok(overshoot > 0, "末端应当能推动一点，不是硬冻结");
    assert.ok(overshoot <= SLIDE_HEIGHT * 0.12 + 0.001, `overshoot ${overshoot}`);
  });
});

test("the first video gives a damped pull instead of freezing solid", () => {
  withHarness((h) => {
    h.pointerDown(200, 100);
    h.tick(16);
    h.pointerMove(200, 120);
    h.tick(16);
    h.pointerMove(200, 5_000);
    // 手指拉了 4880px，画面只跟出去一屏的 12%——能动，但明确"到头了"
    assert.equal(h.translate, SLIDE_HEIGHT * 0.12);

    // 松手后弹回原位
    h.pointerUp(200, 5_000);
    h.endTransition();
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.translate, 0);
  });
});

// ---------------------------------------------------------------------------
// 松手判定与落点提交
// ---------------------------------------------------------------------------

test("a quick flick commits the position immediately, then animates into it", () => {
  withHarness((h) => {
    flickToNext(h);
    // 竖滑吞掉浏览器补发的合成 click，否则会被当成单击而暂停视频
    assert.equal(h.prevented.touchend, 1);

    // 关键：位置在松手当场就落到 scrollTop 上了，IntersectionObserver
    // 立刻能判出新的活跃屏，下一条视频不必等动画走完才起播
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);

    // 画面靠轨道上的反向补偿保持连续，再动画归零
    const writes = h.transformWrites;
    // 松手时手指已经把画面拉走 30px，补偿量 = 30 + 900
    assert.ok(
      writes.includes("translate3d(0, 870px, 0)"),
      `FLIP compensation missing: ${writes.join(" | ")}`
    );
    assert.equal(writes[writes.length - 1], "translate3d(0, 0px, 0)");
    assert.ok(h.awaitingTransition);
    assert.equal(h.willChange, "transform");
    assert.match(h.transitionSpec, /^transform \d+ms cubic-bezier/);

    h.endTransition();
    // 收尾只是摘掉合成层提示，位置早就定了
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    assert.equal(h.transformStyle, "");
    assert.equal(h.willChange, "");
    assert.equal(h.awaitingTransition, false);
  });
});

test("the FLIP start value is committed before the transition is armed", () => {
  withHarness((h) => {
    flickToNext(h);
    const writes = h.transformWrites;
    const start = writes.indexOf("translate3d(0, 870px, 0)");
    const end = writes.lastIndexOf("translate3d(0, 0px, 0)");
    // 起点必须先以无过渡的方式写下并强制提交，否则浏览器会把两次写入
    // 合并，直接跳到终点而根本不产生动画
    assert.ok(start >= 0 && end > start, writes.join(" | "));
    assert.ok(h.styleFlushes > 0, "start value must be flushed");
  });
});

test("a slow short drag bounces back to the current video", () => {
  withHarness((h) => {
    h.pointerDown(200, 700);
    h.tick(16);
    h.pointerMove(200, 680);
    h.tick(400);
    h.pointerMove(200, 660);
    assert.equal(h.translate, -20);
    h.pointerUp(200, 660);
    h.endTransition();
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.translate, 0);
  });
});

test("a slow but long drag still switches, exactly like Douyin", () => {
  withHarness((h) => {
    const past = SLIDE_HEIGHT / 3 + 40;
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(2_000);
    h.pointerMove(200, 800 - past);
    h.pointerUp(200, 800 - past);
    h.endTransition();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("swiping back down returns to the previous video", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT * 2;
    h.pointerDown(200, 200);
    h.tick(16);
    h.pointerMove(200, 230);
    h.tick(16);
    h.pointerMove(200, 300);
    h.pointerUp(200, 300);
    h.endTransition();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("the last video has nowhere to go and simply bounces back", () => {
  withHarness(
    (h) => {
      h.root.scrollTop = SLIDE_HEIGHT * 2;
      h.pointerDown(200, 800);
      h.tick(16);
      h.pointerMove(200, 780);
      h.tick(16);
      h.pointerMove(200, 400);
      h.pointerUp(200, 400);
      h.endTransition();
      assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
      assert.equal(h.translate, 0);
    },
    { slideCount: 3 }
  );
});

test("a lost transitionend cannot strand the feed between two videos", () => {
  withHarness((h) => {
    flickToNext(h);
    const duration = h.settleDurationMs;
    assert.ok(duration > 0);
    // 标签页切走 / 合成器丢事件：transitionend 永远不来。位置本来就已经
    // 落定，这里只验证兜底定时器把合成层提示也收干净了
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    h.advance(duration + SHORTS_PAGER_SETTLE_FALLBACK_MS);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    assert.equal(h.transformStyle, "");
    assert.equal(h.willChange, "");
    assert.equal(h.awaitingTransition, false);
  });
});

test("the fallback timer firing after a real transitionend changes nothing", () => {
  withHarness((h) => {
    flickToNext(h);
    const duration = h.settleDurationMs;
    h.endTransition();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    // 兜底定时器随后到期，不能把画面又推走一屏
    h.advance(duration + SHORTS_PAGER_SETTLE_FALLBACK_MS);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    assert.equal(h.transformStyle, "");
  });
});

test("a transitionend from a child or another property is ignored", () => {
  withHarness((h) => {
    flickToNext(h);
    // 子元素冒泡上来的、或别的属性的 transitionend 不能把动画提前掐掉——
    // 那会让画面在半路瞬移到落点
    h.fireForeignTransitionEnd({ fromChild: true });
    assert.ok(h.awaitingTransition, "child bubble must not end the motion");
    assert.equal(h.willChange, "transform");
    h.fireForeignTransitionEnd({ property: "opacity" });
    assert.ok(h.awaitingTransition, "other properties must not end the motion");

    h.endTransition();
    assert.equal(h.awaitingTransition, false);
    assert.equal(h.willChange, "");
  });
});

test("a switch always animates for the same fixed duration", () => {
  withHarness((h) => {
    // 慢慢拖过 1/3 屏再松手：切屏成立，时长是常数而不是按速度浮动
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(3_000);
    h.pointerMove(200, 400);
    h.pointerUp(200, 400);
    assert.equal(h.settleDurationMs, SHORTS_PAGER_COMMIT_SETTLE_MS);
    assert.match(h.transitionSpec, /cubic-bezier/);
  });
});

test("bouncing back is quicker than switching", () => {
  withHarness((h) => {
    h.pointerDown(200, 700);
    h.tick(16);
    h.pointerMove(200, 680);
    h.tick(600);
    h.pointerMove(200, 620);
    h.pointerUp(200, 620);
    // 没构成切屏 → 回弹，必须比切屏干脆
    assert.ok(h.settleDurationMs > 0);
    assert.ok(h.settleDurationMs <= SHORTS_PAGER_MAX_BOUNCE_MS);
    assert.ok(h.settleDurationMs < SHORTS_PAGER_COMMIT_SETTLE_MS);
    h.endTransition();
    assert.equal(h.root.scrollTop, 0);
  });
});

// ---------------------------------------------------------------------------
// 手势兼容
// ---------------------------------------------------------------------------

test("horizontal seeking on the video keeps the feed completely still", () => {
  withHarness((h) => {
    h.pointerDown(200, 700);
    h.tick(16);
    h.pointerMove(260, 706);
    h.tick(16);
    h.pointerMove(400, 712);
    assert.equal(h.translate, 0);
    assert.equal(h.root.scrollTop, 0);
    // 没有接管就不能挡掉默认行为，也不能吞掉这次点击
    assert.equal(h.prevented.touchmove, 0);
    h.pointerUp(400, 712);
    assert.equal(h.prevented.touchend, 0);
    assert.equal(h.awaitingTransition, false);
    // 轨道上不能留下任何合成层提示
    assert.equal(h.willChange, "");
  });
});

test("a plain tap stays a tap", () => {
  withHarness((h) => {
    h.pointerDown(200, 700);
    h.tick(80);
    h.pointerUp(200, 700);
    assert.equal(h.prevented.touchend, 0);
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.awaitingTransition, false);
  });
});

test("touches that start on the progress bar never page the feed", () => {
  withHarness((h) => {
    const progress = {
      closest: (selector: string) =>
        selector === "[data-shorts-no-swipe]" ? progress : null,
    };
    h.pointerDown(200, 700, { target: progress });
    h.tick(16);
    h.pointerMove(200, 400);
    assert.equal(h.translate, 0);
    h.pointerUp(200, 400);
    assert.equal(h.prevented.touchend, 0);
  });
});

test("a second finger hands the gesture back and snaps to the nearest video", () => {
  withHarness((h) => {
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, 200);
    assert.equal(h.translate, -580);

    h.secondFingerDownAt([
      { clientX: 200, clientY: 200 },
      { clientX: 260, clientY: 400 },
    ]);
    h.endTransition();
    // 等效位置 580 更靠近下一屏（900）而不是当前屏（0）
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);

    // 交出去之后的移动完全不再影响位移
    h.pointerMove(200, 100);
    assert.equal(h.translate, 0);
  });
});

test("a multi-touch start is ignored outright", () => {
  withHarness((h) => {
    h.pointerDownMulti([
      { clientX: 100, clientY: 700 },
      { clientX: 300, clientY: 700 },
    ]);
    h.tick(16);
    h.pointerMove(100, 300);
    assert.equal(h.translate, 0);
    assert.equal(h.root.scrollTop, 0);
  });
});

test("a cancelled touch settles instead of freezing between videos", () => {
  withHarness((h) => {
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, 500);
    assert.equal(h.translate, -280);
    h.pointerCancel();
    h.endTransition();
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.translate, 0);
  });
});

// ---------------------------------------------------------------------------
// 连续滑动 / 打断
// ---------------------------------------------------------------------------

test("grabbing a flying slide freezes it at the frame it is actually on", () => {
  withHarness((h) => {
    flickToNext(h, 800);
    // 补偿量从 870 动画归零；此刻画面走到一半，还剩 400
    h.setLiveTranslate(400);

    h.pointerDown(200, 500);
    // 内联值被改写成当前这一帧的位置，不会瞬移到终点
    assert.equal(h.translate, 400);
    assert.equal(h.transitionSpec, "none");
    assert.equal(h.awaitingTransition, false);

    // 接着再甩一把。等效位置 900-400=500 已经更靠近第 1 屏，
    // 所以这一下是从第 1 屏再前进一屏——连甩两次走两屏，不会卡住
    h.tick(16);
    h.pointerMove(200, 470);
    h.tick(16);
    h.pointerMove(200, 440);
    h.pointerUp(200, 440);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
    h.endTransition();
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
    assert.equal(h.transformStyle, "");
  });
});

test("tapping to stop a flying slide still lands on a video", () => {
  withHarness((h) => {
    flickToNext(h, 800);
    h.setLiveTranslate(400);

    h.pointerDown(200, 500);
    h.tick(40);
    h.pointerUp(200, 500);
    // 这一下只是"停住"，不该被当成单击去暂停视频
    assert.equal(h.prevented.touchend, 2);
    h.endTransition();
    // 等效位置 500 更靠近第 1 屏，就近吸附过去，不会卡在两屏之间
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    assert.equal(h.transformStyle, "");
  });
});

// ---------------------------------------------------------------------------
// 与队列裁剪 / 视口变化共存
// ---------------------------------------------------------------------------

test("a queue trim mid-flight lands on the same video, in new coordinates", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT * 3;
    flickToNext(h, 800);
    assert.ok(h.awaitingTransition);

    // 裁掉队首 2 条：坐标系整体上移，画面位置不变
    h.trimLeading(2);
    h.endTransition();

    // 目标仍是原来那条视频，只是它现在的偏移少了 2 屏
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
    assert.equal(h.translate, 0);
  });
});

test("a queue trim mid-drag keeps the finger tracking continuous", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT * 3;
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, 700);
    assert.equal(h.translate, -80);

    h.trimLeading(2);
    h.tick(16);
    // 位移纯由手指决定，裁剪换坐标系不会把跟手带偏
    h.pointerMove(200, 680);
    assert.equal(h.translate, -100);
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

test("a resize mid-flight drops the stale target and realigns", () => {
  withHarness((h) => {
    h.setAnchorIndex(0);
    flickToNext(h, 800);
    h.setLiveTranslate(-400);
    h.resize();
    // 尺寸变了，按旧尺寸算的终点作废；当前屏被拉回吸附点
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.translate, 0);
    assert.equal(h.awaitingTransition, false);
  });
});

test("a resize during a drag leaves the finger in control", () => {
  withHarness((h) => {
    h.setAnchorIndex(0);
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, 600);
    assert.equal(h.translate, -180);
    h.resize();
    assert.equal(h.translate, -180);
    assert.equal(h.root.scrollTop, 0);
  });
});

// ---------------------------------------------------------------------------
// 合成 click 兜底
// ---------------------------------------------------------------------------

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
    h.pointerDown(200, 500);
    assert.equal(h.clickGuardActive(), false);
    assert.equal(h.fireClick(), false);
  });
});

test("once the feed has settled a tap is just a tap again", () => {
  withHarness((h) => {
    flickToNext(h);
    h.endTransition();
    h.advance(400);
    h.pointerDown(200, 500);
    h.tick(60);
    h.pointerUp(200, 500);
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
    h.pointerDown(200, 700);
    h.tick(16);
    h.pointerMove(280, 706);
    h.pointerUp(280, 706);
    assert.equal(h.clickGuardActive(), false);
  });
});

// ---------------------------------------------------------------------------
// 卸载
// ---------------------------------------------------------------------------

test("tearing down mid-flight leaves a settled position and a clean track", () => {
  const harness = createHarness();
  flickToNext(harness, 800);
  harness.setLiveTranslate(400);
  harness.destroy();
  // 位置在松手时就提交了，卸载只需要把 transform 收干净，不能留在 DOM 上
  assert.equal(harness.root.scrollTop, SLIDE_HEIGHT);
  assert.equal(harness.transformStyle, "");
  assert.equal(harness.willChange, "");
});

test("tearing down mid-drag writes the finger offset back to scrollTop", () => {
  const harness = createHarness();
  harness.pointerDown(200, 800);
  harness.tick(16);
  harness.pointerMove(200, 780);
  harness.tick(16);
  harness.pointerMove(200, 600);
  assert.equal(harness.translate, -180);
  // 手指还按着就被卸载：跟手位移必须落回 scrollTop，不能凭空丢掉
  harness.destroy();
  assert.equal(harness.root.scrollTop, 180);
  assert.equal(harness.transformStyle, "");
});

test("tearing down the page releases every listener", () => {
  const harness = createHarness();
  assert.ok(harness.hasListeners());
  harness.destroy();
  assert.equal(harness.hasListeners(), false);
  assert.equal(harness.awaitingTransition, false);
});

// ---------------------------------------------------------------------------
// 鼠标：桌面拖拽走的是同一条状态机
// ---------------------------------------------------------------------------

test("a mouse drag pages the feed exactly like a finger does", () => {
  withHarness((h) => {
    h.pointerDown(200, 700, { pointerType: "mouse" });
    h.tick(16);
    h.pointerMove(200, 670);
    h.tick(16);
    h.pointerMove(200, 640);
    // 跟手位移写在轨道上，和触摸一模一样
    assert.equal(h.translate, -30);
    h.pointerUp(200, 640);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    h.endTransition();
    assert.equal(h.transformStyle, "");
  });
});

test("only the left mouse button drags", () => {
  withHarness((h) => {
    // 右键 / 中键按下不该起手，否则右键菜单和中键粘贴会被当成滑动
    h.pointerDown(200, 700, { pointerType: "mouse", button: 2 });
    h.tick(16);
    h.pointerMove(200, 400);
    assert.equal(h.translate, 0);
    assert.equal(h.root.scrollTop, 0);
  });
});

test("a mouse click that never moves stays a click", () => {
  withHarness((h) => {
    h.pointerDown(200, 700, { pointerType: "mouse" });
    h.tick(60);
    h.pointerUp(200, 700);
    assert.equal(h.prevented.touchend, 0);
    assert.equal(h.clickGuardActive(), false);
    assert.equal(h.root.scrollTop, 0);
  });
});

// ---------------------------------------------------------------------------
// 滚轮：一次手势 = 一条视频，和手指同一条落点动画
// ---------------------------------------------------------------------------

test("one wheel notch advances exactly one video", () => {
  withHarness((h) => {
    h.wheel(100);
    // 走的是和拖拽完全相同的 FLIP 落点：位置当场提交，再补一段动画
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    assert.equal(h.settleDurationMs, SHORTS_PAGER_COMMIT_SETTLE_MS);
    h.endTransition();
    assert.equal(h.transformStyle, "");
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("scrolling up with the wheel goes back one video", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT * 2;
    h.wheel(-100);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("a trackpad flick is one video, not fifteen", () => {
  withHarness((h) => {
    // 触控板一次轻扫：几十个小 delta，间隔十几毫秒，尾巴拖很久
    h.wheel(8);
    h.wheel(14);
    h.wheel(22); // 累计 44，越过阈值 → 切一屏
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);

    // 之后的惯性尾巴全部被同一次手势吞掉
    for (let i = 0; i < 40; i += 1) {
      h.tick(12);
      h.wheel(30);
    }
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT, "惯性尾巴不该继续翻页");
  });
});

test("a fresh wheel gesture after a pause advances again", () => {
  withHarness((h) => {
    h.wheel(100);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    h.endTransition();
    // 间隔拉开 = 新的一次手势
    h.tick(200);
    h.wheel(100);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
  });
});

test("a nudge too small to be deliberate does nothing", () => {
  withHarness((h) => {
    h.wheel(10);
    h.tick(10);
    h.wheel(12);
    assert.equal(h.root.scrollTop, 0);
    assert.equal(h.awaitingTransition, false);
  });
});

test("the wheel is line- and page-aware, not just pixel-aware", () => {
  withHarness((h) => {
    // Firefox 按行给 delta：3 行 ≈ 48px，够一格
    h.wheel(3, { deltaMode: 1 });
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
  withHarness((h) => {
    h.wheel(1, { deltaMode: 2 });
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

test("the wheel stops at the end of the queue", () => {
  withHarness(
    (h) => {
      h.root.scrollTop = SLIDE_HEIGHT * 2;
      h.wheel(100);
      assert.equal(h.root.scrollTop, SLIDE_HEIGHT * 2);
      assert.equal(h.awaitingTransition, false);
      // 队首同理
      h.root.scrollTop = 0;
      h.tick(500);
      h.wheel(-100);
      assert.equal(h.root.scrollTop, 0);
    },
    { slideCount: 3 }
  );
});

test("pinch zoom keeps its own wheel, everything else is taken over", () => {
  withHarness((h) => {
    // Ctrl+滚轮是浏览器缩放，不能抢
    h.wheel(300, { ctrlKey: true });
    assert.equal(h.root.scrollTop, 0);

    // 进度条上的滚轮**必须**被接管。data-shorts-no-swipe 是给拖拽用的
    // （进度条要吃掉整根手指），滚轮不该认它——漏掉一处 preventDefault，
    // 原生滚动就从那里溜出去，而吸附已经关了，画面会停在两条视频中间。
    const progress = {
      closest: (selector: string) =>
        selector === "[data-shorts-no-swipe]" ? progress : null,
    };
    h.tick(500);
    h.wheel(300, { target: progress });
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
  });
});

// ---------------------------------------------------------------------------
// 兜底不变量：静止时绝不停在两条视频之间
// ---------------------------------------------------------------------------

test("document scroll drift is observed and corrected on the viewport", () => {
  const harness = createHarness({ usesDocumentScroll: true });
  try {
    harness.setExternalScrollTop(SLIDE_HEIGHT + 137);
    harness.scroll();
    assert.equal(harness.scrollTop, SLIDE_HEIGHT + 137);

    harness.advance(200);
    assert.equal(harness.scrollTop, SLIDE_HEIGHT);
    harness.endTransition();
  } finally {
    harness.destroy();
  }
  // 清理也必须使用同一个宿主，否则每次重进短视频页都会泄漏一个监听器。
  assert.equal(harness.hasListeners(), false);
});

test("an unexplained scroll drift corrects itself once things go quiet", () => {
  withHarness((h) => {
    // 模拟浏览器滚动锚定 / 漏掉的原生滚动：位置被外力挪到了半路
    h.root.scrollTop = SLIDE_HEIGHT + 137;
    h.scroll();
    // 立刻不动手，先等滚动真正停下来
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT + 137);
    h.advance(200);
    // 就近吸附回去
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    h.endTransition();
    assert.equal(h.transformStyle, "");
  });
});

test("a couple of pixels of rounding is not worth a correction", () => {
  withHarness((h) => {
    h.root.scrollTop = SLIDE_HEIGHT + 1;
    h.scroll();
    h.advance(200);
    // 亚像素取整不该触发一次动画
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT + 1);
    assert.equal(h.awaitingTransition, false);
  });
});

test("the guard never fights an animation or a finger", () => {
  withHarness((h) => {
    // 落点动画进行中：scrollTop 本来就已经在目标上，守卫不能插手
    h.wheel(100);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);
    h.scroll();
    h.advance(200);
    assert.equal(h.root.scrollTop, SLIDE_HEIGHT);

    // 手指拖动中：位移记在 transform 上，scrollTop 不动，同样不能插手
    h.endTransition();
    h.tick(500);
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, 600);
    const dragged = h.translate;
    h.scroll();
    h.advance(200);
    assert.equal(h.translate, dragged);
  });
});

test("the wheel is ignored while a finger is still down", () => {
  withHarness((h) => {
    h.pointerDown(200, 800);
    h.tick(16);
    h.pointerMove(200, 780);
    h.tick(16);
    h.pointerMove(200, 700);
    const dragged = h.translate;
    h.wheel(300);
    // 手指还在拖，滚轮不能横插一脚改变落点
    assert.equal(h.translate, dragged);
    assert.equal(h.root.scrollTop, 0);
  });
});
