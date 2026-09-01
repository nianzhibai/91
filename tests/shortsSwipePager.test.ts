import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  SHORTS_PAGER_FLICK_MS,
  SHORTS_PAGER_MAX_SETTLE_MS,
  SHORTS_PAGER_MIN_COMMIT_PX,
  SHORTS_PAGER_MIN_SETTLE_MS,
  SHORTS_PAGER_VELOCITY_WINDOW_MS,
  computeShortsPagerVelocity,
  findNearestShortsSlideIndex,
  measureOffsetWithinSlide,
  resolveShortsPagerSettleDuration,
  resolveShortsPagerTargetIndex,
  shortsPagerEase,
} from "../src/shorts/useShortsSwipePager";
import { shouldUseShortsTouchPager } from "../src/shorts/platform";

const VIEWPORT = 900;

function release(overrides: {
  deltaY: number;
  elapsedMs: number;
  anchorIndex?: number;
  slideCount?: number;
  viewportHeight?: number;
}) {
  return resolveShortsPagerTargetIndex({
    deltaY: overrides.deltaY,
    elapsedMs: overrides.elapsedMs,
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

test("settling matches the release speed so the slide keeps decelerating", () => {
  // easeOutCubic 起速 = 3 × 距离 / 时长；按抬手速度反解出来的时长满足这条式子
  const duration = resolveShortsPagerSettleDuration({
    remainingPx: 300,
    velocityPxPerMs: 3,
    viewportHeight: VIEWPORT,
  });
  assert.equal(duration, 300);
  assert.equal((3 * 300) / duration, 3);
});

test("settling stays inside a hand-tuned duration range", () => {
  // 甩得极快：时长压到下限，切屏干脆
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: 100,
      velocityPxPerMs: 20,
      viewportHeight: VIEWPORT,
    }),
    SHORTS_PAGER_MIN_SETTLE_MS
  );
  // 慢慢拖了大半屏才松手：按距离取时长，且不超过上限
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: VIEWPORT,
      velocityPxPerMs: 0,
      viewportHeight: VIEWPORT,
    }),
    SHORTS_PAGER_MAX_SETTLE_MS
  );
  // 速度低于阈值时不按速度反解（否则时长会趋于无穷再被夹到上限）
  const nearlyStill = resolveShortsPagerSettleDuration({
    remainingPx: 90,
    velocityPxPerMs: 0.01,
    viewportHeight: VIEWPORT,
  });
  assert.ok(nearlyStill > SHORTS_PAGER_MIN_SETTLE_MS);
  assert.ok(nearlyStill < SHORTS_PAGER_MAX_SETTLE_MS);
  // 抬手方向和落点方向相反时只取快慢，不取朝向
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: -300,
      velocityPxPerMs: -3,
      viewportHeight: VIEWPORT,
    }),
    300
  );
});

test("an already-settled position needs no animation at all", () => {
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: 0,
      velocityPxPerMs: 5,
      viewportHeight: VIEWPORT,
    }),
    0
  );
  assert.equal(
    resolveShortsPagerSettleDuration({
      remainingPx: 0.4,
      velocityPxPerMs: 0,
      viewportHeight: VIEWPORT,
    }),
    0
  );
  // 视口高度异常时按 1px 兜底，不产生除零
  assert.ok(
    Number.isFinite(
      resolveShortsPagerSettleDuration({
        remainingPx: 50,
        velocityPxPerMs: 0,
        viewportHeight: 0,
      })
    )
  );
});

test("the settle curve starts fast and eases out", () => {
  assert.equal(shortsPagerEase(0), 0);
  assert.equal(shortsPagerEase(1), 1);
  // 定义域外夹紧，动画被拖长/回拨也不会越过端点
  assert.equal(shortsPagerEase(-1), 0);
  assert.equal(shortsPagerEase(2), 1);
  // 前半程走完大半路程才叫惯性减速
  assert.ok(shortsPagerEase(0.5) > 0.8);
  assert.ok(shortsPagerEase(0.25) > 0.5);
  let previous = -1;
  for (let step = 0; step <= 10; step += 1) {
    const value = shortsPagerEase(step / 10);
    assert.ok(value > previous);
    previous = value;
  }
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
  assert.equal(measureOffsetWithinSlide({ slideTop: 0, viewportHeight: VIEWPORT }), 0);
  // 切屏动画走到 40%：锚点已经被顶出去 360px，重贴时要原样还回来
  assert.equal(
    measureOffsetWithinSlide({ slideTop: -360, viewportHeight: VIEWPORT }),
    360
  );
  assert.equal(
    measureOffsetWithinSlide({ slideTop: 200, viewportHeight: VIEWPORT }),
    -200
  );
  // 几何异常时夹在一屏内，不会把滚动位置甩飞
  assert.equal(
    measureOffsetWithinSlide({ slideTop: -50_000, viewportHeight: VIEWPORT }),
    VIEWPORT
  );
  assert.equal(
    measureOffsetWithinSlide({ slideTop: 50_000, viewportHeight: VIEWPORT }),
    -VIEWPORT
  );
  assert.equal(measureOffsetWithinSlide({ slideTop: -10, viewportHeight: 0 }), 0);
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

test("the touch pager takes over only where touch is the primary input", () => {
  withWindow({ search: "", coarsePointer: true }, () => {
    assert.equal(shouldUseShortsTouchPager(false), true);
  });
  // 桌面 / 触控本仍然走原生 scroll-snap + 滚轮 + 方向键
  withWindow({ search: "", coarsePointer: false }, () => {
    assert.equal(shouldUseShortsTouchPager(false), false);
  });
  // 不支持 matchMedia 的环境不能抛错，按不启用处理
  withWindow({ search: "" }, () => {
    assert.equal(shouldUseShortsTouchPager(false), false);
  });
});

test("iPhone document scrolling keeps its native snapping by default", () => {
  // 文档滚动是为了让 Safari 工具栏随刷动收起，接管触摸会让它永远展开
  withWindow({ search: "", coarsePointer: true }, () => {
    assert.equal(shouldUseShortsTouchPager(true), false);
  });
  // 但可以显式强制开启做同机对照
  withWindow({ search: "?shortsPager=1", coarsePointer: true }, () => {
    assert.equal(shouldUseShortsTouchPager(true), true);
  });
});

test("the touch pager has an explicit escape hatch in both directions", () => {
  withWindow({ search: "?shortsPager=0", coarsePointer: true }, () => {
    assert.equal(shouldUseShortsTouchPager(false), false);
  });
  // 桌面上强制开启，便于在开发机上验证手势逻辑
  withWindow({ search: "?shortsPager=1", coarsePointer: false }, () => {
    assert.equal(shouldUseShortsTouchPager(false), true);
  });
  // 无关取值不改变默认判定
  withWindow({ search: "?shortsPager=yes", coarsePointer: false }, () => {
    assert.equal(shouldUseShortsTouchPager(false), false);
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
  const pagedRule = /^\.shorts-feed\.is-touch-paged \{[\s\S]*?\}/m.exec(shortsCss);
  assert.ok(pagedRule, ".shorts-feed.is-touch-paged rule should exist");
  // 手指位移完全由 JS 写进 scrollTop，不能再和浏览器抢同一根手指
  assert.match(pagedRule[0], /touch-action:\s*none/);
  // 吸附点会把程序化写入重新对齐，逐帧跟手和落点动画都会被它吃掉
  assert.match(pagedRule[0], /scroll-snap-type:\s*none/);
  assert.match(
    shortsCss,
    /\.shorts-feed\.is-touch-paged \.shorts-slide \{[\s\S]*?scroll-snap-align:\s*none/
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
  assert.match(shortsPageSource, /enabled:\s*useTouchPager/);
  assert.match(shortsPageSource, /containerRef,/);
  assert.match(shortsPageSource, /usesDocumentScroll:\s*useDocumentScroll/);
  assert.match(
    shortsPageSource,
    /className=\{`shorts-feed\$\{useTouchPager \? " is-touch-paged" : ""\}`\}/
  );
  // activeIndex 仍然只由 IntersectionObserver 决定，播放/预载链路不变
  assert.doesNotMatch(shortsPageSource, /pager[\s\S]{0,40}setActiveIndex/i);
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
  assert.match(
    shortsPageSource,
    /root\.scrollTop = Math\.max\(0, slide\.offsetTop \+ offsetWithinAnchor\)/
  );
});
