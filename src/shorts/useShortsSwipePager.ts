import { useEffect, useRef } from "react";
import { clamp } from "./mediaBuffer";
import { classifyTouchSeekIntent } from "./useShortsSlideGestures";

/**
 * 移动端上下翻页手势控制器。
 *
 * 原生 `scroll-snap-type: y mandatory` 把"滑多远才算切屏"完全交给了浏览器：
 * 各家实现不一致，慢速大幅滑动经常被判回弹，快速轻扫又可能不吸附，落点动画
 * 的时长和缓动也不可控——这就是移动端"手感不好"的根源。
 *
 * 这里参考 zyronon/douyin 的 `utils/slide.ts` 把判定收回自己手里：
 * - 按下到抬手全程 1:1 跟手（不跟随浏览器的滚动预测）
 * - 抬手时按"距离 + 时长"判定切屏还是回弹，规则固定、跨浏览器一致
 * - 落点用 easeOutCubic 收尾，时长按抬手速度反解，读起来是惯性减速而非跳变
 *
 * 与 douyin 不同的是，这里不引入 transform 轨道：位移仍然写在原有滚动容器的
 * `scrollTop`（文档滚动模式写 `window.scrollY`）上。页面其余部分——
 * IntersectionObserver 判活跃屏、长会话队列裁剪重贴、键盘 `scrollIntoView`、
 * 隐藏视频后跳下一条——全都建立在同一套 scrollTop / offsetTop 几何上，换成
 * transform 轨道会一次性推翻它们。
 */

/** 位移不足这个值一律当误触，回弹到当前视频。 */
export const SHORTS_PAGER_MIN_COMMIT_PX = 20;
/** 位移超过一屏的这个比例，无论快慢都切屏。 */
export const SHORTS_PAGER_COMMIT_RATIO = 1 / 3;
/** 中间地带（位移够但不大）看抬手快慢：短于这个时长算轻扫，切屏。 */
export const SHORTS_PAGER_FLICK_MS = 150;
/** 落点动画时长区间；douyin 用的是固定 300ms，这里按速度在区间内浮动。 */
export const SHORTS_PAGER_MIN_SETTLE_MS = 180;
export const SHORTS_PAGER_MAX_SETTLE_MS = 380;
/** 抬手速度取这个时间窗内的平均值，避免最后一帧抖动主导结果。 */
export const SHORTS_PAGER_VELOCITY_WINDOW_MS = 100;
/** 低于这个速度就不按速度反解时长，直接按剩余距离取。 */
const SHORTS_PAGER_MIN_VELOCITY_PX_PER_MS = 0.05;
/**
 * 判定"滚动位置被外力挪过"的阈值。自己写进去的值再读回来只会有亚像素级
 * 取整误差；队列裁剪重贴一次至少挪掉好几屏，两者相差好几个数量级。
 */
const SHORTS_PAGER_EXTERNAL_SCROLL_EPSILON_PX = 4;
/**
 * 竖滑之后吞掉合成 click 的时间窗。合成 click 紧跟在同一次 touchend 之后
 * 到达，这个窗口只要覆盖住它即可；下一次按下也会立刻解除，用户真正的轻点
 * 不会被误伤。
 */
const SHORTS_PAGER_CLICK_GUARD_MS = 300;
/** 起点标记：滑动手势不接管这个子树内按下的触摸（如底部进度条）。 */
export const SHORTS_NO_SWIPE_ATTRIBUTE = "data-shorts-no-swipe";

export type ShortsPagerSample = { y: number; t: number };

/**
 * 抬手速度（px/ms，向下为正）。取最近 SHORTS_PAGER_VELOCITY_WINDOW_MS 内
 * 最早的一个采样点与最后一个采样点做差：只用最后两帧会被单帧抖动放大，
 * 用整段手势又会把中途的停顿算进来。
 */
export function computeShortsPagerVelocity(samples: ShortsPagerSample[]): number {
  if (samples.length < 2) return 0;
  const last = samples[samples.length - 1];
  let first = samples[0];
  for (const sample of samples) {
    if (last.t - sample.t <= SHORTS_PAGER_VELOCITY_WINDOW_MS) {
      first = sample;
      break;
    }
  }
  const elapsed = last.t - first.t;
  if (elapsed <= 0) return 0;
  return (last.y - first.y) / elapsed;
}

export type ShortsPagerRelease = {
  /** 纵向位移，向上滑（看下一条）为负。 */
  deltaY: number;
  /** 按下到抬手的毫秒数。 */
  elapsedMs: number;
  /** 一屏高度。 */
  viewportHeight: number;
  /** 手势开始时贴合的那一屏。 */
  anchorIndex: number;
  /** 当前队列里的 slide 总数。 */
  slideCount: number;
};

/**
 * 松手后应该停在哪一屏；返回值仍等于 anchorIndex 表示回弹。
 *
 * 判定顺序照搬 douyin：先用距离把两头的情况定死（太短必不通过、超过 1/3 屏
 * 必通过），只有中间地带才看抬手快慢。目标恒为 anchorIndex ± 1——一次手势
 * 最多切一屏，快速连滑靠多次手势叠加，不会一口气跳过中间的视频。
 */
export function resolveShortsPagerTargetIndex(input: ShortsPagerRelease): number {
  const { deltaY, elapsedMs, viewportHeight, anchorIndex, slideCount } = input;
  const lastIndex = Math.max(0, slideCount - 1);
  const distance = Math.abs(deltaY);

  let gapTime = elapsedMs;
  if (distance < SHORTS_PAGER_MIN_COMMIT_PX) {
    gapTime = Number.POSITIVE_INFINITY;
  } else if (distance > viewportHeight * SHORTS_PAGER_COMMIT_RATIO) {
    gapTime = 0;
  }
  if (gapTime >= SHORTS_PAGER_FLICK_MS) return clamp(anchorIndex, 0, lastIndex);

  const next = deltaY < 0 ? anchorIndex + 1 : anchorIndex - 1;
  // 到头/到尾时 clamp 会把目标压回 anchorIndex，等价于回弹。
  return clamp(next, 0, lastIndex);
}

/**
 * 落点动画时长。easeOutCubic 的起始速度是 3 × 距离 / 时长，按抬手速度反解
 * 时长，动画第一帧就能接上手指离开时的速度，看起来是"继续减速滑过去"而不是
 * 松手后重新起步——这正是需求里的惯性滚动。
 *
 * 速度过低（慢慢拖到位再松手）时反解出来的时长会趋于无穷，改按剩余距离取：
 * 拖得越远收尾越长，短距离回弹不会拖泥带水。两条路径最后都夹在区间内。
 *
 * `velocityPxPerMs` 只取模：抬手瞬间的方向偶尔会和落点方向相反（滑过头又
 * 回带一点），那时用的是它的快慢而不是朝向。
 */
export function resolveShortsPagerSettleDuration(input: {
  remainingPx: number;
  velocityPxPerMs: number;
  viewportHeight: number;
}): number {
  const remaining = Math.abs(input.remainingPx);
  if (remaining < 1) return 0;
  const speed = Math.abs(input.velocityPxPerMs);
  const ratio = clamp(remaining / Math.max(1, input.viewportHeight), 0, 1);
  const duration =
    speed > SHORTS_PAGER_MIN_VELOCITY_PX_PER_MS
      ? (3 * remaining) / speed
      : SHORTS_PAGER_MIN_SETTLE_MS +
        (SHORTS_PAGER_MAX_SETTLE_MS - SHORTS_PAGER_MIN_SETTLE_MS) * ratio;
  return clamp(
    duration,
    SHORTS_PAGER_MIN_SETTLE_MS,
    SHORTS_PAGER_MAX_SETTLE_MS
  );
}

/** easeOutCubic：入场快、收尾慢，和惯性减速同形。 */
export function shortsPagerEase(t: number): number {
  const clamped = clamp(t, 0, 1);
  return 1 - (1 - clamped) * (1 - clamped) * (1 - clamped);
}

/**
 * 锚点 slide 内的滚动偏移：0 表示正好贴在吸附点上，正值表示已经往下滑过了
 * 一部分。长会话队列裁剪要用它把"裁剪前的画面位置"原样搬到新坐标系里。
 * 夹在 ±一屏之内，异常几何（尺寸未就绪、节点已脱离布局）不会把滚动甩飞。
 */
export function measureOffsetWithinSlide(input: {
  /** slide 顶端相对滚动容器上沿的距离，向下为正。 */
  slideTop: number;
  viewportHeight: number;
}): number {
  const limit = Math.max(0, input.viewportHeight);
  // `0 - x` 而不是 `-x`：贴合吸附点时前者是 +0，后者是 -0，会污染调用方的比较。
  return clamp(0 - input.slideTop, -limit, limit);
}

/** 贴合度最高的那一屏；用于手势起点和落点的锚定。 */
export function findNearestShortsSlideIndex(
  slideTops: number[],
  scrollTop: number
): number {
  if (slideTops.length === 0) return -1;
  let bestIndex = 0;
  let bestDistance = Math.abs(slideTops[0] - scrollTop);
  for (let index = 1; index < slideTops.length; index += 1) {
    const distance = Math.abs(slideTops[index] - scrollTop);
    if (distance < bestDistance) {
      bestDistance = distance;
      bestIndex = index;
    }
  }
  return bestIndex;
}

type PagerDrag = {
  startX: number;
  startY: number;
  startTime: number;
  /** 判定为纵向翻页那一刻的位移；后续按它做零点，避免激活时画面跳一下。 */
  baselineY: number;
  anchorIndex: number;
  anchorTop: number;
  /** 本次手势允许到达的滚动范围：最多相邻一屏。 */
  minTop: number;
  maxTop: number;
  viewportHeight: number;
  slideTops: number[];
  /** 已判定为纵向翻页并接管 */
  committed: boolean;
  /** 判定为横向 seek / 多指 / 起点在禁用区：本次手势不归我管 */
  abandoned: boolean;
  /** 起手时打断了上一次的落点动画，位置多半不在吸附点上，收尾必须补一次 */
  interrupted: boolean;
  samples: ShortsPagerSample[];
};

export type ShortsSwipePagerHost = {
  /** slide 所在的容器；同时也是触摸监听的挂载点。 */
  root: HTMLElement;
  /** iPhone 浏览器壳的文档滚动模式：位移写在 window 上。 */
  usesDocumentScroll: boolean;
  /** 视口尺寸变化后用来重新对齐的当前屏。 */
  getAnchorSlide: () => HTMLElement | null;
};

/**
 * 手势状态机本体，不依赖 React。这样它能脱离渲染器直接接受完整的事件序列
 * 测试——按下 / 移动 / 抬手 / 多指 / 打断动画每条分支都能覆盖到，这些恰恰
 * 是"手感"真正落在的地方。返回值是解除绑定的函数。
 */
export function createShortsSwipePager(host: ShortsSwipePagerHost) {
  const { root, usesDocumentScroll } = host;
  // ---- 滚动目标适配：容器滚动 / 文档滚动共用同一套位移逻辑 ----
  const getScrollTop = () =>
    usesDocumentScroll ? window.scrollY : root.scrollTop;
  /** 上一次由本模块写入的滚动位置；null 表示当前没有在跟踪。 */
  let lastWrittenTop: number | null = null;
  const setScrollTop = (value: number) => {
    if (usesDocumentScroll) window.scrollTo(0, value);
    else root.scrollTop = value;
    lastWrittenTop = value;
  };
  /**
   * 外力（长会话队列裁剪重贴、scrollIntoView）挪动滚动位置时的位移量，
   * 没被挪过就是 0。裁剪几乎总是插在切屏过程中间：不跟着平移，跟手位移
   * 和落点动画都会按旧坐标继续写，画面会硬跳一大截。
   */
  const readExternalScrollDelta = () => {
    if (lastWrittenTop === null) return 0;
    const delta = getScrollTop() - lastWrittenTop;
    return Math.abs(delta) > SHORTS_PAGER_EXTERNAL_SCROLL_EPSILON_PX
      ? delta
      : 0;
  };
  const getViewportHeight = () =>
    usesDocumentScroll ? window.innerHeight : root.clientHeight;
  const getMaxScrollTop = () =>
    Math.max(
      0,
      usesDocumentScroll
        ? document.documentElement.scrollHeight - window.innerHeight
        : root.scrollHeight - root.clientHeight
    );
  /**
   * slide 在滚动坐标系里的位置。不用 offsetTop：那依赖 offsetParent 恰好
   * 是滚动容器的上沿，rect 差值在两种滚动模式下都成立。
   */
  const getSlideTop = (slide: HTMLElement) => {
    const base = usesDocumentScroll ? 0 : root.getBoundingClientRect().top;
    return slide.getBoundingClientRect().top - base + getScrollTop();
  };
  /** 一次手势只读一次：容器 rect 和滚动位置在循环外取好，避免逐条回读布局。 */
  const readSlideTops = () => {
    const base = usesDocumentScroll ? 0 : root.getBoundingClientRect().top;
    const origin = getScrollTop();
    return [...root.querySelectorAll<HTMLElement>("[data-shorts-slide]")].map(
      (slide) => slide.getBoundingClientRect().top - base + origin
    );
  };

  // ---- 落点动画 ----
  let settleFrame: number | null = null;
  /** 返回是否真的打断了一次进行中的动画。 */
  const cancelSettle = () => {
    if (settleFrame === null) return false;
    window.cancelAnimationFrame(settleFrame);
    settleFrame = null;
    return true;
  };
  const settleTo = (targetTop: number, velocityPxPerMs: number) => {
    cancelSettle();
    let from = getScrollTop();
    const distance = targetTop - from;
    const duration = resolveShortsPagerSettleDuration({
      remainingPx: distance,
      velocityPxPerMs,
      viewportHeight: getViewportHeight(),
    });
    if (duration <= 0) {
      setScrollTop(targetTop);
      return;
    }
    lastWrittenTop = from;
    const startedAt = performance.now();
    const step = (now: number) => {
      // 队列裁剪只是把整条 feed 换了坐标系，画面位置是连续的：起点跟着
      // 平移，终点（from + distance）自然一起平移，动画不受影响地跑完。
      from += readExternalScrollDelta();
      const progress = clamp((now - startedAt) / duration, 0, 1);
      setScrollTop(from + distance * shortsPagerEase(progress));
      if (progress < 1) {
        settleFrame = window.requestAnimationFrame(step);
      } else {
        settleFrame = null;
        lastWrittenTop = null;
      }
    };
    settleFrame = window.requestAnimationFrame(step);
  };

  // ---- 合成 click 兜底 ----
  // touchend 上的 preventDefault 按规范应当挡住合成 click，但个别 WebKit
  // 版本只认"第一个 touchmove 上的 preventDefault"。漏出来的那一次 click 会
  // 落到 slide 上被当成单击去暂停视频——每滑一屏暂停一次，非常显眼。
  // 这里只吞掉紧跟在一次成功竖滑之后的那一个 click。
  let clickGuardTimer: number | null = null;
  const swallowClick = (event: Event) => {
    releaseClickGuard();
    event.stopPropagation();
    event.preventDefault();
  };
  function releaseClickGuard() {
    if (clickGuardTimer === null) return;
    window.clearTimeout(clickGuardTimer);
    clickGuardTimer = null;
    root.removeEventListener("click", swallowClick, true);
  }
  const guardNextClick = () => {
    releaseClickGuard();
    root.addEventListener("click", swallowClick, true);
    clickGuardTimer = window.setTimeout(
      releaseClickGuard,
      SHORTS_PAGER_CLICK_GUARD_MS
    );
  };

  // ---- 手势 ----
  let drag: PagerDrag | null = null;

  /** 多指 / 取消等中断：就近吸附，不要停在两屏之间。 */
  const settleToNearest = () => {
    const slideTops = drag?.slideTops ?? readSlideTops();
    const index = findNearestShortsSlideIndex(slideTops, getScrollTop());
    if (index < 0) return;
    settleTo(clamp(slideTops[index], 0, getMaxScrollTop()), 0);
  };

  const handleTouchStart = (event: TouchEvent) => {
    // 上一次的落点动画还在跑：接住它，用当前位置作为新手势的起点。
    // 打断过动画、或上一次手势被第二根手指顶掉，位置就停在两屏之间，
    // 这次手势无论走哪条分支收尾都得把它吸回吸附点。
    // 新的一次按下：上一次竖滑的合成 click 早该到了，守卫立刻失效，
    // 这样紧接着的这次轻点一定能穿到 slide 上。
    releaseClickGuard();
    const previous = drag;
    drag = null;
    const interrupted = cancelSettle() || Boolean(previous?.committed);

    const bail = () => {
      if (interrupted) settleToNearest();
    };

    if (event.touches.length !== 1) return bail();

    // 用鸭子类型而不是 instanceof Element：这条状态机要能脱离浏览器全局
    // 直接被测试，而 target 在真实环境里一定是元素或 null。
    const target = event.target as Element | null;
    if (
      typeof target?.closest === "function" &&
      target.closest(`[${SHORTS_NO_SWIPE_ATTRIBUTE}]`)
    ) {
      return bail();
    }

    const slideTops = readSlideTops();
    if (slideTops.length === 0) return bail();
    const scrollTop = getScrollTop();
    const anchorIndex = findNearestShortsSlideIndex(slideTops, scrollTop);
    if (anchorIndex < 0) return bail();
    const maxScrollTop = getMaxScrollTop();
    const previousTop = slideTops[anchorIndex - 1];
    const nextTop = slideTops[anchorIndex + 1];
    const anchorTop = slideTops[anchorIndex];
    const touch = event.touches[0];
    const now = performance.now();
    // 几何刚刚重新采过，之前那次动画写下的位置不能再当作平移基准。
    lastWrittenTop = null;

    drag = {
      startX: touch.clientX,
      startY: touch.clientY,
      startTime: now,
      baselineY: 0,
      anchorIndex,
      // 手势起点就是当前实际位置：中途接住动画时不会先跳回吸附点。
      anchorTop: clamp(scrollTop, 0, maxScrollTop),
      // 一次手势最多离开当前屏一屏；否则松手只切一屏会看到明显的回抽。
      minTop: clamp(previousTop ?? anchorTop, 0, maxScrollTop),
      maxTop: clamp(nextTop ?? anchorTop, 0, maxScrollTop),
      viewportHeight: getViewportHeight(),
      slideTops,
      committed: false,
      abandoned: false,
      interrupted,
      samples: [{ y: touch.clientY, t: now }],
    };
  };

  const handleTouchMove = (event: TouchEvent) => {
    if (!drag || drag.abandoned) return;
    if (event.touches.length !== 1) {
      // 第二根手指落下：交出手势，已经拖开的部分就近吸附回去。
      const shouldSettle = drag.committed || drag.interrupted;
      drag.abandoned = true;
      if (shouldSettle) settleToNearest();
      return;
    }

    const touch = event.touches[0];
    const deltaX = touch.clientX - drag.startX;
    const deltaY = touch.clientY - drag.startY;

    if (!drag.committed) {
      // 与视频上的横向 seek 共用同一套方向判定，两者互斥，不会同时激活。
      const intent = classifyTouchSeekIntent(deltaX, deltaY);
      if (intent === "pending") return;
      if (intent === "seek") {
        drag.abandoned = true;
        // 横向 seek 与纵向落点互不干扰，被打断的那次动画照样要收尾。
        if (drag.interrupted) settleToNearest();
        return;
      }
      drag.committed = true;
      // 激活阈值那段位移不能再算进画面位移，否则接管的瞬间会跳一下。
      drag.baselineY = deltaY;
    }

    // 接管后必须挡掉浏览器的默认处理（容器已是 touch-action: none，
    // 这里是对老 WebKit 的双保险），否则会和原生滚动抢同一根手指。
    if (event.cancelable) event.preventDefault();

    const now = performance.now();
    drag.samples.push({ y: touch.clientY, t: now });
    // 采样窗口之外的点只用于兜底，留一个即可。
    while (
      drag.samples.length > 2 &&
      now - drag.samples[1].t > SHORTS_PAGER_VELOCITY_WINDOW_MS
    ) {
      drag.samples.shift();
    }

    // 拖动途中被队列裁剪换了坐标系：重新采一次几何，手指接着往下拖时
    // 位移仍然连续，不会突然跳到旧坐标对应的位置。
    const externalDelta = readExternalScrollDelta();
    if (externalDelta !== 0) {
      const maxScrollTop = getMaxScrollTop();
      drag.slideTops = readSlideTops();
      drag.anchorTop = clamp(drag.anchorTop + externalDelta, 0, maxScrollTop);
      const anchorIndex = findNearestShortsSlideIndex(
        drag.slideTops,
        drag.anchorTop
      );
      if (anchorIndex >= 0) {
        const ownTop = drag.slideTops[anchorIndex];
        drag.anchorIndex = anchorIndex;
        drag.minTop = clamp(
          drag.slideTops[anchorIndex - 1] ?? ownTop,
          0,
          maxScrollTop
        );
        drag.maxTop = clamp(
          drag.slideTops[anchorIndex + 1] ?? ownTop,
          0,
          maxScrollTop
        );
      }
    }

    const offset = drag.anchorTop - (deltaY - drag.baselineY);
    setScrollTop(clamp(offset, drag.minTop, drag.maxTop));
  };

  const handleTouchEnd = (event: TouchEvent) => {
    const current = drag;
    drag = null;
    if (!current || current.abandoned) return;
    if (!current.committed) {
      // 没构成滑动。若这一下只是"按住把飞行中的动画停下来"，同样要吸回
      // 吸附点，并吞掉这次点击——否则会被当成单击而暂停视频。
      if (current.interrupted) {
        if (event.cancelable) event.preventDefault();
        guardNextClick();
        settleToNearest();
      }
      return;
    }

    // 竖滑结束后浏览器还会补一次合成 click，会被 slide 当成单击而暂停视频。
    if (event.cancelable) event.preventDefault();
    guardNextClick();

    const changedTouch = event.changedTouches[0];
    const now = performance.now();
    if (changedTouch) {
      current.samples.push({ y: changedTouch.clientY, t: now });
    }
    const deltaY = changedTouch
      ? changedTouch.clientY - current.startY
      : current.samples[current.samples.length - 1].y - current.startY;

    const targetIndex = resolveShortsPagerTargetIndex({
      deltaY,
      elapsedMs: now - current.startTime,
      viewportHeight: current.viewportHeight,
      anchorIndex: current.anchorIndex,
      slideCount: current.slideTops.length,
    });
    const targetTop = clamp(
      current.slideTops[targetIndex] ?? current.anchorTop,
      0,
      getMaxScrollTop()
    );
    settleTo(targetTop, computeShortsPagerVelocity(current.samples));
  };

  const handleTouchCancel = () => {
    const current = drag;
    drag = null;
    if (current && (current.committed || current.interrupted)) {
      settleToNearest();
    }
  };

  // ---- 视口尺寸变化后重新对齐 ----
  // 接管手势的同时关掉了 scroll-snap，浏览器不会再在旋转 / 键盘弹出后
  // 自动把当前屏拉回吸附点，这里补上。
  let realignFrame: number | null = null;
  let realignTimer: number | null = null;
  const realign = () => {
    if (drag) return;
    const slide = host.getAnchorSlide();
    if (!slide) return;
    setScrollTop(clamp(getSlideTop(slide), 0, getMaxScrollTop()));
  };
  const handleViewportResize = () => {
    cancelSettle();
    realign();
    if (realignFrame !== null) window.cancelAnimationFrame(realignFrame);
    realignFrame = window.requestAnimationFrame(() => {
      realignFrame = null;
      realign();
    });
    // 移动端工具栏 / 输入法收放会连着发多次 resize，尺寸稳定后再对齐一次。
    if (realignTimer !== null) window.clearTimeout(realignTimer);
    realignTimer = window.setTimeout(() => {
      realignTimer = null;
      realign();
    }, 240);
  };

  root.addEventListener("touchstart", handleTouchStart, { passive: true });
  root.addEventListener("touchmove", handleTouchMove, { passive: false });
  root.addEventListener("touchend", handleTouchEnd);
  root.addEventListener("touchcancel", handleTouchCancel);
  window.addEventListener("resize", handleViewportResize);
  window.addEventListener("orientationchange", handleViewportResize);

  return () => {
    cancelSettle();
    releaseClickGuard();
    if (realignFrame !== null) window.cancelAnimationFrame(realignFrame);
    if (realignTimer !== null) window.clearTimeout(realignTimer);
    root.removeEventListener("touchstart", handleTouchStart);
    root.removeEventListener("touchmove", handleTouchMove);
    root.removeEventListener("touchend", handleTouchEnd);
    root.removeEventListener("touchcancel", handleTouchCancel);
    window.removeEventListener("resize", handleViewportResize);
    window.removeEventListener("orientationchange", handleViewportResize);
  };
}

export type ShortsSwipePagerOptions = {
  /** 关闭时完全不挂监听，页面回到原生 scroll-snap。 */
  enabled: boolean;
  containerRef: React.RefObject<HTMLElement | null>;
  usesDocumentScroll: boolean;
  getAnchorSlide: () => HTMLElement | null;
};

/** React 侧只负责生命周期；判定和动画全在 createShortsSwipePager 里。 */
export function useShortsSwipePager(options: ShortsSwipePagerOptions) {
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const { enabled, usesDocumentScroll } = options;

  useEffect(() => {
    if (!enabled) return;
    const root = optionsRef.current.containerRef.current;
    if (!root) return;
    return createShortsSwipePager({
      root,
      usesDocumentScroll,
      // 走 ref 读取，回调换引用不会重挂监听。
      getAnchorSlide: () => optionsRef.current.getAnchorSlide(),
    });
  }, [enabled, usesDocumentScroll]);
}
