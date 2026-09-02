import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  SHORTS_PAGER_FLICK_MS,
  SHORTS_PAGER_COMMIT_SETTLE_MS,
  SHORTS_PAGER_FLICK_VELOCITY_PX_PER_MS,
  SHORTS_PAGER_MAX_BOUNCE_MS,
  SHORTS_PAGER_MIN_COMMIT_PX,
  SHORTS_PAGER_MIN_SETTLE_MS,
  SHORTS_PAGER_SETTLE_EASING,
  SHORTS_PAGER_VELOCITY_WINDOW_MS,
  computeShortsPagerVelocity,
  findNearestShortsSlideIndex,
  INITIAL_SHORTS_WHEEL_STATE,
  SHORTS_PAGER_WHEEL_GESTURE_GAP_MS,
  SHORTS_PAGER_WHEEL_STEP_PX,
  SHORTS_WHEEL_LINE_HEIGHT_PX,
  applyShortsPagerEdgeResistance,
  normalizeShortsWheelDelta,
  resolveShortsWheelStep,
  measureOffsetWithinSlide,
  parseTranslateY,
  resolveShortsPagerSettleDuration,
  resolveShortsPagerTargetIndex,
} from "../src/shorts/useShortsSwipePager";
import { shouldUseShortsSwipePager } from "../src/shorts/platform";

const VIEWPORT = 900;

function release(overrides: {
  deltaY: number;
  elapsedMs: number;
  velocityPxPerMs?: number;
  anchorIndex?: number;
  slideCount?: number;
  viewportHeight?: number;
}) {
  return resolveShortsPagerTargetIndex({
    deltaY: overrides.deltaY,
    elapsedMs: overrides.elapsedMs,
    // 默认按"慢拖到位"处理，速度通路单独测
    velocityPxPerMs: overrides.velocityPxPerMs ?? 0,
    viewportHeight: overrides.viewportHeight ?? VIEWPORT,
    anchorIndex: overrides.anchorIndex ?? 5,
    slideCount: overrides.slideCount ?? 20,
  });
}

// ---------------------------------------------------------------------------
// 切屏判定（对齐 zyronon/douyin utils/slide.ts 的三段式规则）
// ---------------------------------------------------------------------------

test("tiny swipes never switch videos, however fast they are", () => {
  // 距离不足门槛：再快也当误触，回弹到当前视频
  assert.equal(release({ deltaY: -19, elapsedMs: 10 }), 5);
  assert.equal(release({ deltaY: 19, elapsedMs: 10 }), 5);
  assert.equal(release({ deltaY: 0, elapsedMs: 1 }), 5);
  // 恰好等于门槛就不再算"太短"，此时按时间判定
  assert.equal(release({ deltaY: -SHORTS_PAGER_MIN_COMMIT_PX, elapsedMs: 10 }), 6);
});

test("swipes past one third of the screen switch even when they are slow", () => {
  const past = -(VIEWPORT / 3) - 1;
  assert.equal(release({ deltaY: past, elapsedMs: 5_000 }), 6);
  assert.equal(release({ deltaY: -past, elapsedMs: 5_000 }), 4);
  // 恰好等于 1/3 不算"够长"，退回按时间判定
  assert.equal(release({ deltaY: -VIEWPORT / 3, elapsedMs: 5_000 }), 5);
  assert.equal(release({ deltaY: -VIEWPORT / 3, elapsedMs: 100 }), 6);
});

test("mid-range swipes fall back to how fast the finger left the screen", () => {
  assert.equal(release({ deltaY: -60, elapsedMs: SHORTS_PAGER_FLICK_MS - 1 }), 6);
  assert.equal(release({ deltaY: 60, elapsedMs: SHORTS_PAGER_FLICK_MS - 1 }), 4);
  // 慢慢拖一小段再松手：回弹，防止误触
  assert.equal(release({ deltaY: -60, elapsedMs: SHORTS_PAGER_FLICK_MS }), 5);
  assert.equal(release({ deltaY: -60, elapsedMs: 900 }), 5);
});

test("a gesture never skips a video, and never runs off either end", () => {
  // 一次手势最多切一屏
  assert.equal(release({ deltaY: -VIEWPORT * 3, elapsedMs: 20 }), 6);
  // 首屏继续下拉 / 末屏继续上滑都只能回弹
  assert.equal(release({ deltaY: 400, elapsedMs: 20, anchorIndex: 0 }), 0);
  assert.equal(
    release({ deltaY: -400, elapsedMs: 20, anchorIndex: 19, slideCount: 20 }),
    19
  );
  // 队列还没有内容时不会算出负数下标
  assert.equal(release({ deltaY: -400, elapsedMs: 20, anchorIndex: 0, slideCount: 0 }), 0);
  assert.equal(release({ deltaY: -400, elapsedMs: 20, anchorIndex: 7, slideCount: 3 }), 2);
});

// ---------------------------------------------------------------------------
// 惯性收尾
// ---------------------------------------------------------------------------

test("mid-range swipes also commit on a fast flick, not just a short press", () => {
  // 先把手指搭在屏幕上停一会再甩：按压总时长早就超了，但这明显是一次甩动
  const fast = -SHORTS_PAGER_FLICK_VELOCITY_PX_PER_MS;
  assert.equal(release({ deltaY: -60, elapsedMs: 900, velocityPxPerMs: fast }), 6);
  assert.equal(release({ deltaY: 60, elapsedMs: 900, velocityPxPerMs: -fast }), 4);
  // 速度不够快，仍然回弹
  assert.equal(
    release({ deltaY: -60, elapsedMs: 900, velocityPxPerMs: fast * 0.99 }),
    5
  );
  // 速度方向和位移方向相反（滑过头又回带）：不认这条通路
  assert.equal(release({ deltaY: -60, elapsedMs: 900, velocityPxPerMs: -fast }), 5);
  // 距离仍然是硬门槛，再快也不能低于 20px
  assert.equal(release({ deltaY: -19, elapsedMs: 900, velocityPxPerMs: fast * 10 }), 5);
});

// ---------------------------------------------------------------------------
// 落点时长
// ---------------------------------------------------------------------------

test("switching to a neighbouring video always takes the same time", () => {
  // 「按抬手速度反解时长」在 FLIP 的几何下永远被夹到上限，是写了但从不生效
  // 的机制。切屏改成固定值——douyin/TikTok 也是固定时长。
  for (const remainingPx of [200, 600, 870, 1_500]) {
    assert.equal(
      resolveShortsPagerSettleDuration({
        remainingPx,
        viewportHeight: VIEWPORT,
        committed: true,
      }),
      SHORTS_PAGER_COMMIT_SETTLE_MS
    );
  }
  // 方向不影响时长
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: -870,
      viewportHeight: VIEWPORT,
      committed: true,
    }),
    SHORTS_PAGER_COMMIT_SETTLE_MS
  );
});

test("bouncing back scales with distance and is always quicker than a switch", () => {
  const near = resolveShortsPagerSettleDuration({
    remainingPx: 30,
    viewportHeight: VIEWPORT,
    committed: false,
  });
  const far = resolveShortsPagerSettleDuration({
    remainingPx: VIEWPORT / 2,
    viewportHeight: VIEWPORT,
    committed: false,
  });
  assert.ok(near >= SHORTS_PAGER_MIN_SETTLE_MS);
  assert.ok(near < far);
  assert.equal(far, SHORTS_PAGER_MAX_BOUNCE_MS);
  // 回弹不可能比切屏还慢，否则"没切成"读起来比"切成了"更拖沓
  assert.ok(far < SHORTS_PAGER_COMMIT_SETTLE_MS);
  // 超过半屏的回弹（边缘阻尼下可能出现）不会突破上限
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: VIEWPORT * 2,
      viewportHeight: VIEWPORT,
      committed: false,
    }),
    SHORTS_PAGER_MAX_BOUNCE_MS
  );
});

test("an already-settled position needs no animation at all", () => {
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: 0,
      viewportHeight: VIEWPORT,
      committed: true,
    }),
    0
  );
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: 0.4,
      viewportHeight: VIEWPORT,
      committed: false,
    }),
    0
  );
  // 视口高度异常时按 1px 兜底，不产生除零
  assert.ok(
    Number.isFinite(
      resolveShortsPagerSettleDuration({
        remainingPx: 50,
        viewportHeight: 0,
        committed: false,
      })
    )
  );
});

test("the settle curve keeps its hand-tuned shape", () => {
  // 缓动交给 CSS（合成器线程）。契约：起速要明显快过手指，终点斜率为 0。
  const control = /^cubic-bezier\(([\d.]+), ([\d.]+), ([\d.]+), ([\d.]+)\)$/.exec(
    SHORTS_PAGER_SETTLE_EASING
  );
  assert.ok(control, "easing should be an explicit cubic-bezier");
  const [x1, y1, , y2] = control.slice(1).map(Number);
  assert.ok(y1 / x1 > 2.5, "should leave the finger behind immediately");
  // 收尾必须停住，否则落点会显得"撞上去"
  assert.equal(y2, 1);
});

// ---------------------------------------------------------------------------
// 到头 / 到尾的阻尼
// ---------------------------------------------------------------------------

test("the ends give a little instead of freezing solid", () => {
  const bounds = { min: -900, max: 0, viewportHeight: VIEWPORT };
  // 范围内原样通过
  assert.equal(applyShortsPagerEdgeResistance({ value: -450, ...bounds }), -450);
  assert.equal(applyShortsPagerEdgeResistance({ value: 0, ...bounds }), 0);
  assert.equal(applyShortsPagerEdgeResistance({ value: -900, ...bounds }), -900);
  // 越界：能推动，但只有原位移的一小部分
  const pulled = applyShortsPagerEdgeResistance({ value: 100, ...bounds });
  assert.ok(pulled > 0 && pulled < 100);
  const pushed = applyShortsPagerEdgeResistance({ value: -1_000, ...bounds });
  assert.ok(pushed < -900 && pushed > -1_000);
  // 再怎么拽也有上限，画面不会被拉走大半屏
  const limit = VIEWPORT * 0.12;
  assert.equal(applyShortsPagerEdgeResistance({ value: 99_999, ...bounds }), limit);
  assert.equal(
    applyShortsPagerEdgeResistance({ value: -99_999, ...bounds }),
    -900 - limit
  );
  // 视口高度为 0 时退化成硬 clamp，不产生 NaN
  assert.equal(
    applyShortsPagerEdgeResistance({ value: 500, min: -900, max: 0, viewportHeight: 0 }),
    0
  );
});

test("the live track offset is readable from both inline and computed values", () => {
  // 我们自己写进去的内联值
  assert.equal(parseTranslateY("translate3d(0, -240px, 0)"), -240);
  assert.equal(parseTranslateY("translate3d(0px, 37.5px, 0px)"), 37.5);
  assert.equal(parseTranslateY("translateY(-12px)"), -12);
  // 动画进行中只有 computed 能拿到中间值，浏览器给的是矩阵
  assert.equal(parseTranslateY("matrix(1, 0, 0, 1, 0, -123.5)"), -123.5);
  assert.equal(
    parseTranslateY("matrix3d(1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, -88, 0, 1)"),
    -88
  );
  // 静止态与异常输入一律当没有位移，绝不能让 NaN 流进滚动位置
  assert.equal(parseTranslateY("none"), 0);
  assert.equal(parseTranslateY(""), 0);
  assert.equal(parseTranslateY(null), 0);
  assert.equal(parseTranslateY(undefined), 0);
  assert.equal(parseTranslateY("matrix(1, 0, 0, 1)"), 0);
  assert.equal(parseTranslateY("matrix(1, 0, 0, 1, 0, abc)"), 0);
  assert.equal(parseTranslateY("rotate(20deg)"), 0);
});

// ---------------------------------------------------------------------------
// 抬手速度采样
// ---------------------------------------------------------------------------

test("release speed comes from the recent sample window, not the whole gesture", () => {
  // 采样不足两个点：无速度可言
  assert.equal(computeShortsPagerVelocity([]), 0);
  assert.equal(computeShortsPagerVelocity([{ y: 10, t: 0 }]), 0);
  // 两点直接求斜率
  assert.equal(
    computeShortsPagerVelocity([
      { y: 100, t: 0 },
      { y: 50, t: 25 },
    ]),
    -2
  );
  // 手势前段先停顿再甩：窗口外的点不能把速度拉平
  const start = 1_000;
  const velocity = computeShortsPagerVelocity([
    { y: 400, t: start },
    { y: 400, t: start + SHORTS_PAGER_VELOCITY_WINDOW_MS + 50 },
    { y: 200, t: start + SHORTS_PAGER_VELOCITY_WINDOW_MS + 150 },
  ]);
  assert.equal(velocity, -2);
  // 同一时间戳的重复采样不产生除零
  assert.equal(
    computeShortsPagerVelocity([
      { y: 10, t: 5 },
      { y: 90, t: 5 },
    ]),
    0
  );
});

// ---------------------------------------------------------------------------
// 锚点定位 / 裁剪偏移
// ---------------------------------------------------------------------------

test("the anchor slide is the one the viewport is closest to", () => {
  const tops = [0, 900, 1800, 2700];
  assert.equal(findNearestShortsSlideIndex(tops, 0), 0);
  assert.equal(findNearestShortsSlideIndex(tops, 449), 0);
  assert.equal(findNearestShortsSlideIndex(tops, 451), 1);
  assert.equal(findNearestShortsSlideIndex(tops, 2_700), 3);
  // 越过末屏（回弹中）仍然锚在末屏
  assert.equal(findNearestShortsSlideIndex(tops, 9_999), 3);
  // 正中间平手时取靠前的一条，结果稳定
  assert.equal(findNearestShortsSlideIndex(tops, 450), 0);
  // 队列还没有 slide
  assert.equal(findNearestShortsSlideIndex([], 100), -1);
});

test("queue trimming carries the in-flight offset into the new coordinates", () => {
  // 正好贴在吸附点上：没有偏移要还原
  assert.equal(
    measureOffsetWithinSlide({
      scrollTop: 1_800,
      slideTop: 1_800,
      viewportHeight: VIEWPORT,
    }),
    0
  );
  // 已经往下滑过锚点 360px，重贴时要原样还回来
  assert.equal(
    measureOffsetWithinSlide({
      scrollTop: 2_160,
      slideTop: 1_800,
      viewportHeight: VIEWPORT,
    }),
    360
  );
  assert.equal(
    measureOffsetWithinSlide({
      scrollTop: 1_600,
      slideTop: 1_800,
      viewportHeight: VIEWPORT,
    }),
    -200
  );
  // 几何异常时夹在一屏内，不会把滚动位置甩飞
  assert.equal(
    measureOffsetWithinSlide({
      scrollTop: 50_000,
      slideTop: 0,
      viewportHeight: VIEWPORT,
    }),
    VIEWPORT
  );
  assert.equal(
    measureOffsetWithinSlide({
      scrollTop: 0,
      slideTop: 50_000,
      viewportHeight: VIEWPORT,
    }),
    -VIEWPORT
  );
  assert.equal(
    measureOffsetWithinSlide({ scrollTop: 10, slideTop: 0, viewportHeight: 0 }),
    0
  );
});

// ---------------------------------------------------------------------------
// 启用条件
// ---------------------------------------------------------------------------

function withWindow<T>(
  options: { search: string; coarsePointer?: boolean },
  run: () => T
): T {
  const original = Object.getOwnPropertyDescriptor(globalThis, "window");
  Object.defineProperty(globalThis, "window", {
    value: {
      location: { search: options.search },
      matchMedia:
        options.coarsePointer === undefined
          ? undefined
          : (query: string) => ({
              matches:
                options.coarsePointer === true &&
                query === "(hover: none) and (pointer: coarse)",
            }),
    },
    configurable: true,
    writable: true,
  });
  try {
    return run();
  } finally {
    if (original) Object.defineProperty(globalThis, "window", original);
    else delete (globalThis as { window?: unknown }).window;
  }
}

test("the gesture controller is mounted on every device, mouse included", () => {
  // 控制器用 pointer 事件，同一套代码同时吃手指和鼠标——桌面拖拽是白送的。
  // 参考实现 zyronon/douyin 全仓只绑 pointer 事件，没有"移动端一套桌面一套"。
  withWindow({ search: "", coarsePointer: true }, () => {
    assert.equal(shouldUseShortsSwipePager(), true);
  });
  withWindow({ search: "", coarsePointer: false }, () => {
    assert.equal(shouldUseShortsSwipePager(), true);
  });
  // 不支持 matchMedia 的环境不能抛错
  withWindow({ search: "" }, () => {
    assert.equal(shouldUseShortsSwipePager(), true);
  });
  // 唯一的关闭方式是显式回退开关
  withWindow({ search: "?shortsPager=0", coarsePointer: true }, () => {
    assert.equal(shouldUseShortsSwipePager(), false);
  });
});

test("the pager has an explicit escape hatch in both directions", () => {
  withWindow({ search: "?shortsPager=0", coarsePointer: true }, () => {
    assert.equal(shouldUseShortsSwipePager(), false);
  });
  // 桌面同样接管：滚轮和拖拽都走我们自己的判定与落点
  withWindow({ search: "?shortsPager=1", coarsePointer: false }, () => {
    assert.equal(shouldUseShortsSwipePager(), true);
  });
  // 无关取值不改变默认判定（默认就是开）
  withWindow({ search: "?shortsPager=yes", coarsePointer: false }, () => {
    assert.equal(shouldUseShortsSwipePager(), true);
  });
});

// ---------------------------------------------------------------------------
// 接线：样式与页面
// ---------------------------------------------------------------------------

const shortsCss = readFileSync(
  new URL("../src/styles/shorts.css", import.meta.url),
  "utf8"
);
const shortsPageSource = readFileSync(
  new URL("../src/pages/ShortsPage.tsx", import.meta.url),
  "utf8"
);

test("taking over the gesture also turns off the browser's own scrolling", () => {
  const pagedRule = /^\.shorts-feed\.is-pager-driven \{[\s\S]*?\}/m.exec(shortsCss);
  assert.ok(pagedRule, ".shorts-feed.is-pager-driven rule should exist");
  // 手指位移完全由 JS 写进 scrollTop，不能再和浏览器抢同一根手指
  assert.match(pagedRule[0], /touch-action:\s*none/);
  // 吸附点会把程序化写入重新对齐，逐帧跟手和落点动画都会被它吃掉
  assert.match(pagedRule[0], /scroll-snap-type:\s*none/);
  assert.match(
    shortsCss,
    /\.shorts-feed\.is-pager-driven \.shorts-slide \{[\s\S]*?scroll-snap-align:\s*none/
  );
});

test("desktop keeps the native snapping path untouched", () => {
  // 行首锚定：文档滚动模式的覆盖规则里也含有 `.shorts-feed {` 这段文本
  const baseRule = /^\.shorts-feed \{[\s\S]*?\}/m.exec(shortsCss);
  assert.ok(baseRule, ".shorts-feed rule should exist");
  assert.match(baseRule[0], /scroll-snap-type:\s*y mandatory/);
  assert.doesNotMatch(baseRule[0], /touch-action:\s*none/);
  const slideRule = /^\.shorts-slide \{[\s\S]*?\}/m.exec(shortsCss);
  assert.ok(slideRule, ".shorts-slide rule should exist");
  assert.match(slideRule[0], /scroll-snap-align:\s*start/);
});

test("the shorts page wires the pager to the same scroll container", () => {
  assert.match(shortsPageSource, /useShortsSwipePager\(\{/);
  assert.match(shortsPageSource, /enabled:\s*usePagerGestures/);
  assert.match(shortsPageSource, /containerRef,/);
  assert.match(shortsPageSource, /trackRef,/);
  assert.match(shortsPageSource, /usesDocumentScroll:\s*useDocumentScroll/);
  assert.match(
    shortsPageSource,
    /className=\{`shorts-feed\$\{usePagerGestures \? " is-pager-driven" : ""\}`\}/
  );
  // activeIndex 仍然只由 IntersectionObserver 决定，播放/预载链路不变
  assert.doesNotMatch(shortsPageSource, /pager[\s\S]{0,40}setActiveIndex/i);
});

test("every slide lives inside the transform track", () => {
  // 位移写在轨道上而不是逐条 slide 上：一次合成层，切屏动画走合成器线程
  assert.match(
    shortsPageSource,
    /<div className="shorts-feed__track" ref=\{trackRef\}>/
  );
  const feedBlock =
    /<div\s+className=\{`shorts-feed[\s\S]*?<div className="shorts-feed__track"[\s\S]*?items\.map\(/.exec(
      shortsPageSource
    );
  assert.ok(feedBlock, "slides should be rendered inside the track");
});

test("the track carries no layer hint while the feed is at rest", () => {
  // 常驻 transform / will-change 会在每个 <video> 祖先上留合成层，
  // 本页在 iOS 合成路径上踩过坑，静止态必须是干净的普通块
  const trackRule = /^\.shorts-feed__track \{[\s\S]*?\}/m.exec(shortsCss);
  assert.ok(trackRule, ".shorts-feed__track rule should exist");
  assert.match(trackRule[0], /transform:\s*none/);
  assert.match(trackRule[0], /will-change:\s*auto/);
  assert.match(trackRule[0], /position:\s*static/);
  assert.doesNotMatch(trackRule[0], /translate3d/);
});

test("the progress bar keeps the whole finger to itself", () => {
  assert.match(shortsPageSource, /data-shorts-no-swipe=""/);
  const progressBlock = /className=\{`shorts-slide__progress [\s\S]*?onPointerDown/.exec(
    shortsPageSource
  );
  assert.ok(progressBlock, "progress bar block should exist");
  assert.match(progressBlock[0], /data-shorts-no-swipe=""/);
});

test("queue trimming re-anchors without discarding the in-flight offset", () => {
  assert.match(shortsPageSource, /offsetWithinAnchor: measureOffsetWithinActiveSlide\(/);
  // 量和还原必须用同一个参照系（轨道），否则动画进行中的 translateY 会被
  // 重复计入一次，重贴时画面跳一整屏
  assert.match(
    shortsPageSource,
    /readShortsSlideTopWithinTrack\(slide, track\) \+ pending\.offsetWithinAnchor/
  );
  assert.match(shortsPageSource, /slideTop: readShortsSlideTopWithinTrack\(slide, track\)/);
  assert.doesNotMatch(
    shortsPageSource,
    /slideTop:[\s\S]{0,80}getBoundingClientRect\(\)\.top - base/
  );
});

// ---------------------------------------------------------------------------
// 滚轮：一次手势 = 一条视频
// ---------------------------------------------------------------------------

test("wheel deltas are normalised across the three delta modes", () => {
  const viewportHeight = VIEWPORT;
  // 0 = 像素（Chrome 一格约 100）
  assert.equal(normalizeShortsWheelDelta({ deltaY: 100, deltaMode: 0, viewportHeight }), 100);
  // 1 = 行（Firefox 一格 3 行）
  assert.equal(
    normalizeShortsWheelDelta({ deltaY: 3, deltaMode: 1, viewportHeight }),
    3 * SHORTS_WHEEL_LINE_HEIGHT_PX
  );
  // 2 = 页
  assert.equal(
    normalizeShortsWheelDelta({ deltaY: 1, deltaMode: 2, viewportHeight }),
    VIEWPORT
  );
  // 方向保留
  assert.ok(normalizeShortsWheelDelta({ deltaY: -3, deltaMode: 1, viewportHeight }) < 0);
  // 视口高度异常时按 1px 兜底；脏输入不产生 NaN
  assert.equal(normalizeShortsWheelDelta({ deltaY: 2, deltaMode: 2, viewportHeight: 0 }), 2);
  assert.equal(normalizeShortsWheelDelta({ deltaY: NaN, deltaMode: 0, viewportHeight }), 0);
});

function wheelRun(events: Array<{ delta: number; at: number }>) {
  let state = INITIAL_SHORTS_WHEEL_STATE;
  const steps: number[] = [];
  for (const event of events) {
    const decision = resolveShortsWheelStep({
      state,
      deltaPx: event.delta,
      now: event.at,
    });
    state = decision.state;
    if (decision.step !== 0) steps.push(decision.step);
  }
  return steps;
}

test("small deltas accumulate until they add up to a deliberate scroll", () => {
  // 单个事件不够，累计够了才切
  assert.deepEqual(wheelRun([{ delta: 15, at: 0 }, { delta: 15, at: 10 }]), []);
  assert.deepEqual(
    wheelRun([
      { delta: 15, at: 0 },
      { delta: 15, at: 10 },
      { delta: 15, at: 20 },
    ]),
    [1]
  );
  // 一格鼠标滚轮本身就够
  assert.deepEqual(wheelRun([{ delta: SHORTS_PAGER_WHEEL_STEP_PX, at: 0 }]), [1]);
  assert.deepEqual(wheelRun([{ delta: -SHORTS_PAGER_WHEEL_STEP_PX, at: 0 }]), [-1]);
});

test("one continuous wheel gesture only ever moves one video", () => {
  // 触控板一次轻扫：连发大量事件、间隔十几毫秒、尾巴很长
  const flick = Array.from({ length: 60 }, (_, i) => ({ delta: 30, at: i * 12 }));
  assert.deepEqual(wheelRun(flick), [1], "惯性尾巴不该继续翻页");
});

test("a pause between wheel events starts a new gesture", () => {
  const gap = SHORTS_PAGER_WHEEL_GESTURE_GAP_MS;
  assert.deepEqual(
    wheelRun([
      { delta: 100, at: 0 },
      { delta: 100, at: gap - 1 }, // 仍属同一次手势
      { delta: 100, at: gap - 1 + gap }, // 间隔够了 → 新手势
    ]),
    [1, 1]
  );
  // 累计量也跟着手势重置：跨手势的零碎位移不该攒成一步
  assert.deepEqual(
    wheelRun([
      { delta: 30, at: 0 },
      { delta: 30, at: 1_000 },
      { delta: 30, at: 2_000 },
    ]),
    []
  );
});

test("reversing direction mid-tail does not double back", () => {
  // 同一次手势内先向下越过阈值，之后无论来什么都不再产生动作
  assert.deepEqual(
    wheelRun([
      { delta: 100, at: 0 },
      { delta: -100, at: 10 },
      { delta: -100, at: 20 },
    ]),
    [1]
  );
});
