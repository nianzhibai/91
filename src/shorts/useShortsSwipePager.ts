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
 * 判定和运动两部分都照 zyronon/douyin 的 `utils/slide.ts` 来：
 * - 判定：位移 <20px 回弹、>1/3 屏必切、中间地带看 150ms 内是否抬手
 * - 运动：`translate3d` + CSS `transition`，**跑在合成器线程上**
 *
 * 运动机制这条尤其关键。切屏那几百毫秒恰好是主线程最忙的时候——activeIndex
 * 变化触发 React 重渲染、iOS 共享 <video> 换插槽、`play()` 起播、下一条预载。
 * 用 rAF 逐帧写 `scrollTop` 的话主线程一掉帧动画就顿；交给 CSS transition
 * 则完全不受主线程影响。
 *
 * ## 坐标模型
 *
 * 记 `T` 为轨道当前的 translateY，任意时刻的等效滚动位置恒为 `scrollTop - T`；
 * 下面所有几何换算都从这一条推出来。三个阶段：
 *
 * - 静止：`scrollTop` 精确落在某条 slide 上，`T = 0`，轨道上什么都没有。
 * - 跟手：`scrollTop` 不动，位移全部记在 `T` 上（逐帧只写 transform）。
 * - 落点：**先把 `scrollTop` 写到目标 slide，再用 `T` 反向补偿这次跳变，
 *   然后让 `T` 动画归零**（FLIP）。画面是平滑滑过去的，但滚动位置在第 0
 *   毫秒就已经是新的了。
 *
 * 落点这样排有两个直接后果：IntersectionObserver 在松手当场就判出新的活跃屏
 * （下一条视频立刻起播，与 douyin 在 touchEnd 里推进 index 一致），以及位置
 * 提交完全不依赖 transitionend——那个事件丢了也只是 will-change 多留一会儿。
 *
 * 页面其余部分因此完全不用改：判活跃屏、长会话队列裁剪重贴、键盘
 * `scrollIntoView`、隐藏视频后跳下一条，全都仍然建立在 `scrollTop` 几何上。
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
/**
 * 落点缓动。这是 easeOutCubic 的 cubic-bezier 形式，起始斜率
 * 0.61 / 0.215 ≈ 2.84，与下面按 3×距离/时长 反解时长的假设吻合：动画第一帧
 * 的速度就接上手指离开时的速度，读起来是继续减速滑过去而不是重新起步。
 * 换缓动就必须同步改 resolveShortsPagerSettleDuration 里的系数。
 */
export const SHORTS_PAGER_SETTLE_EASING = "cubic-bezier(0.215, 0.61, 0.355, 1)";
/** 抬手速度取这个时间窗内的平均值，避免最后一帧抖动主导结果。 */
export const SHORTS_PAGER_VELOCITY_WINDOW_MS = 100;
/**
 * transitionend 兜底。标签页切走、合成器丢事件时它可能不来；位置早已提交，
 * 这里只是保证 will-change 和内联 transform 不会一直挂在轨道上。
 */
export const SHORTS_PAGER_SETTLE_FALLBACK_MS = 80;
/** 低于这个速度就不按速度反解时长，直接按剩余距离取。 */
const SHORTS_PAGER_MIN_VELOCITY_PX_PER_MS = 0.05;
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
 * 落点动画时长。SHORTS_PAGER_SETTLE_EASING 的起始速度约为 3 × 距离 / 时长，
 * 按抬手速度反解时长，动画第一帧就能接上手指离开时的速度。
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

/**
 * 从 transform 值里取出 translateY。内联样式写的是 `translate3d(0, Npx, 0)`，
 * `getComputedStyle` 读回来的是 `matrix(...)` / `matrix3d(...)`——动画进行中
 * 只有后者能拿到当前的中间值，所以两种都要认。认不出来时返回 0：宁可当成
 * 没有位移（最坏是少平移一次），也不要把 NaN 传进滚动位置里。
 */
export function parseTranslateY(transform: string | null | undefined): number {
  if (!transform || transform === "none") return 0;

  const translate3d = /translate3d\(\s*[^,]+,\s*(-?[\d.]+)px/.exec(transform);
  if (translate3d) return toFiniteNumber(translate3d[1]);

  const translateY = /translateY\(\s*(-?[\d.]+)px/.exec(transform);
  if (translateY) return toFiniteNumber(translateY[1]);

  const matrix3d = /^matrix3d\((.+)\)$/.exec(transform);
  if (matrix3d) {
    const parts = matrix3d[1].split(",");
    return parts.length === 16 ? toFiniteNumber(parts[13]) : 0;
  }

  const matrix = /^matrix\((.+)\)$/.exec(transform);
  if (matrix) {
    const parts = matrix[1].split(",");
    return parts.length === 6 ? toFiniteNumber(parts[5]) : 0;
  }

  return 0;
}

function toFiniteNumber(raw: string): number {
  const value = Number(raw.trim());
  return Number.isFinite(value) ? value : 0;
}

/**
 * 滚动位置相对某条 slide 顶端的偏移：0 表示正好贴在吸附点上，正值表示已经
 * 往下滑过了一部分。长会话队列裁剪要用它把"裁剪前的画面位置"原样搬到新
 * 坐标系里。夹在 ±一屏之内，异常几何不会把滚动位置甩飞。
 *
 * 传进来的 `slideTop` 必须是**消掉了轨道 transform 之后**的布局位置，
 * 否则动画进行中量出来的值会把 translateY 重复计入一次，重贴时画面会跳
 * 一整屏。取法见 readShortsSlideTopWithinTrack。
 */
export function measureOffsetWithinSlide(input: {
  scrollTop: number;
  slideTop: number;
  viewportHeight: number;
}): number {
  const limit = Math.max(0, input.viewportHeight);
  return clamp(input.scrollTop - input.slideTop, -limit, limit);
}

/**
 * slide 相对轨道内容原点的位置。两个 rect 受同一个 transform 影响，相减就
 * 把它消掉了，因此这个值在动画进行中同样可靠，且不依赖 offsetParent 语义。
 */
export function readShortsSlideTopWithinTrack(
  slide: HTMLElement | null,
  track: HTMLElement | null
): number {
  if (!slide || !track) return 0;
  return slide.getBoundingClientRect().top - track.getBoundingClientRect().top;
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
  /** 手势开始时轨道已有的位移（中途接住动画时非 0）。 */
  originTranslate: number;
  anchorIndex: number;
  /** 本次手势允许到达的 translateY 范围：最多相邻一屏。 */
  minTranslate: number;
  maxTranslate: number;
  viewportHeight: number;
  /** 手势开始时的 slide 节点与它们的等效滚动位置。 */
  slides: HTMLElement[];
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
  /** slide 所在的滚动容器；同时也是触摸监听的挂载点。 */
  root: HTMLElement;
  /** 承载位移的轨道，`root` 的唯一子元素。 */
  track: HTMLElement;
  /** iPhone 浏览器壳的文档滚动模式：位移写在 window 上。 */
  usesDocumentScroll: boolean;
  /** 视口尺寸变化后用来重新对齐的当前屏。 */
  getAnchorSlide: () => HTMLElement | null;
};

/**
 * 手势状态机本体，不依赖 React。这样它能脱离渲染器直接接受完整的事件序列
 * 测试——按下 / 移动 / 抬手 / 多指 / 打断动画 / 落点提交每条分支都能覆盖到，
 * 这些恰恰是"手感"真正落在的地方。返回值是解除绑定的函数。
 */
export function createShortsSwipePager(host: ShortsSwipePagerHost) {
  const { root, track, usesDocumentScroll } = host;

  // ---- 滚动目标适配：容器滚动 / 文档滚动共用同一套位移逻辑 ----
  const getScrollTop = () =>
    usesDocumentScroll ? window.scrollY : root.scrollTop;
  const setScrollTop = (value: number) => {
    if (usesDocumentScroll) window.scrollTo(0, value);
    else root.scrollTop = value;
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

  // ---- 轨道位移 ----
  /** 轨道当前的 translateY。等效滚动位置恒为 getScrollTop() - translate。 */
  let translate = 0;
  /** 动画进行中内联样式记的是终点值，当前值只能从 computed 里读。 */
  const readLiveTranslate = () => {
    const computed = window.getComputedStyle?.(track)?.transform;
    return computed === undefined ? translate : parseTranslateY(computed);
  };
  const applyTranslate = (value: number) => {
    translate = value;
    track.style.transform = `translate3d(0, ${value}px, 0)`;
  };
  /** 静止态：清掉 transform 和合成层提示。 */
  const clearTranslate = () => {
    translate = 0;
    track.style.transition = "";
    track.style.transform = "";
    track.style.willChange = "";
  };

  /**
   * 当前等效滚动位置。用**实时** translate 而不是内联终点值：动画进行中两者
   * 不同，rect 反映的是实时值，混用会算出差一整屏的结果。
   * 每次手势 / 落点只调用几次（逐帧的跟手位移不走这里），开销可以忽略。
   */
  const getEffectiveTop = () => getScrollTop() - readLiveTranslate();
  const readSlides = () => [
    ...root.querySelectorAll<HTMLElement>("[data-shorts-slide]"),
  ];
  /**
   * slide 的等效滚动位置。由 `slide.rect.top = rootRect.top + (S - scrollTop)
   * + translate` 反解而来；rect 和减掉的 translate 取自同一时刻，因此不管
   * 动画跑到哪一帧，结果都是这条 slide 真实的布局落点。
   */
  const readSlideTops = () => {
    const base = usesDocumentScroll ? 0 : root.getBoundingClientRect().top;
    const origin = getEffectiveTop();
    return readSlides().map(
      (slide) => slide.getBoundingClientRect().top - base + origin
    );
  };
  const readSlideTop = (slide: HTMLElement) =>
    slide.getBoundingClientRect().top -
    (usesDocumentScroll ? 0 : root.getBoundingClientRect().top) +
    getEffectiveTop();

  // ---- 落点动画 ----
  let settleTimer: number | null = null;
  let settling = false;

  const handleTransitionEnd = (event: Event) => {
    const transitionEvent = event as TransitionEvent;
    if (transitionEvent.target !== track) return;
    if (
      transitionEvent.propertyName &&
      transitionEvent.propertyName !== "transform"
    ) {
      return;
    }
    finishMotion();
  };

  const detachSettleListeners = () => {
    if (settleTimer !== null) {
      window.clearTimeout(settleTimer);
      settleTimer = null;
    }
    track.removeEventListener("transitionend", handleTransitionEnd);
  };

  /** 动画收尾：只是摘掉合成层提示，位置在动画开始时就已经落定了。 */
  const finishMotion = () => {
    if (!settling) return;
    settling = false;
    detachSettleListeners();
    clearTranslate();
  };

  /** 返回是否真的打断了一次进行中的动画。 */
  const cancelSettle = () => {
    if (!settling) return false;
    settling = false;
    detachSettleListeners();
    // 冻结在当前这一帧的位置，手指从这里接着走。
    const live = readLiveTranslate();
    track.style.transition = "none";
    applyTranslate(live);
    return true;
  };

  /**
   * 落点：**先提交位置，再补一段动画**（FLIP）。
   *
   * 松手的当下就把 `scrollTop` 写到目标 slide 上，然后给轨道加一个反向的
   * translate 抵消掉这次跳变，再让它动画归零——画面看起来是平滑滑过去的，
   * 但滚动位置在第 0 毫秒就已经是新的了。
   *
   * 这么排有两个好处，都关乎手感和健壮性：
   * 1. IntersectionObserver 在松手当场就判出新的活跃屏，下一条视频立刻起播，
   *    不用等动画走完（douyin 也是在 touchEnd 里就推进 localIndex 的）。
   *    也就不必去赌"合成器动画期间 IO 会不会被触发"。
   * 2. 位置提交不依赖 transitionend。那个事件在标签页切走、动画被打断时会丢；
   *    在这个排法里丢了也只是 will-change 多留一会儿，绝不会卡在两屏之间。
   *
   * 长会话队列裁剪同理：它保持 `scrollTop - slide 布局位置` 不变，轨道上的
   * translate 原样继续，视觉完全连续，不需要任何额外处理。
   */
  const settleTo = (target: HTMLElement | null, velocityPxPerMs: number) => {
    detachSettleListeners();
    settling = false;

    const live = readLiveTranslate();
    const from = getScrollTop();
    const targetTop = clamp(
      target ? readSlideTop(target) : from - live,
      0,
      getMaxScrollTop()
    );

    setScrollTop(targetTop);
    // 浏览器可能夹取实际落点，按真正生效的值算补偿量，画面才不会跳。
    const compensation = live + (getScrollTop() - from);

    const duration = resolveShortsPagerSettleDuration({
      remainingPx: compensation,
      velocityPxPerMs,
      viewportHeight: getViewportHeight(),
    });
    if (duration <= 0) {
      clearTranslate();
      return;
    }

    settling = true;
    track.style.willChange = "transform";
    // 起点必须先以"无过渡"的方式落到样式上，否则浏览器会把它和终点合并，
    // 直接跳到 0 而没有动画。
    track.style.transition = "none";
    applyTranslate(compensation);
    forceStyleFlush();
    track.style.transition = `transform ${Math.round(duration)}ms ${SHORTS_PAGER_SETTLE_EASING}`;
    applyTranslate(0);
    track.addEventListener("transitionend", handleTransitionEnd);
    settleTimer = window.setTimeout(
      finishMotion,
      Math.round(duration) + SHORTS_PAGER_SETTLE_FALLBACK_MS
    );
  };

  /**
   * 把刚写下的起点值真正提交给样式系统。用方法调用而不是 `void el.offsetHeight`：
   * 属性读取有被压缩器判成无副作用而删掉的风险，那会让 FLIP 的两次写入被合并，
   * 动画直接消失。
   */
  function forceStyleFlush() {
    track.getBoundingClientRect();
  }

  // ---- 合成 click 兜底 ----
  // touchend 上的 preventDefault 按规范应当挡住合成 click，但个别 WebKit
  // 版本只认"第一个 touchmove 上的 preventDefault"。漏出来的那一次 click 会
  // 落到 slide 上被当成单击去暂停视频——每滑一屏暂停一次，非常显眼。
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
    const slides = drag?.slides ?? readSlides();
    const slideTops = drag?.slideTops ?? readSlideTops();
    const index = findNearestShortsSlideIndex(slideTops, getEffectiveTop());
    if (index < 0) return;
    settleTo(slides[index] ?? null, 0);
  };

  const handleTouchStart = (event: TouchEvent) => {
    // 新的一次按下：上一次竖滑的合成 click 早该到了，守卫立刻失效，
    // 这样紧接着的这次轻点一定能穿到 slide 上。
    releaseClickGuard();
    // 上一次的落点动画还在跑：接住它，用当前位置作为新手势的起点。
    // 打断过动画、或上一次手势被第二根手指顶掉，位置就停在两屏之间，
    // 这次手势无论走哪条分支收尾都得把它吸回吸附点。
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

    const slides = readSlides();
    if (slides.length === 0) return bail();
    const slideTops = readSlideTops();
    const effectiveTop = getEffectiveTop();
    const anchorIndex = findNearestShortsSlideIndex(slideTops, effectiveTop);
    if (anchorIndex < 0) return bail();

    const anchorTop = slideTops[anchorIndex];
    const previousTop = slideTops[anchorIndex - 1] ?? anchorTop;
    const nextTop = slideTops[anchorIndex + 1] ?? anchorTop;
    const scrollTop = getScrollTop();
    const touch = event.touches[0];
    const now = performance.now();

    drag = {
      startX: touch.clientX,
      startY: touch.clientY,
      startTime: now,
      baselineY: 0,
      originTranslate: translate,
      anchorIndex,
      // 一次手势最多离开当前屏一屏；否则松手只切一屏会看到明显的回抽。
      // translate 与滚动位置反向，因此 next 对应下限、previous 对应上限。
      minTranslate: scrollTop - nextTop,
      maxTranslate: scrollTop - previousTop,
      viewportHeight: getViewportHeight(),
      slides,
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
      track.style.willChange = "transform";
      track.style.transition = "none";
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

    // 位移纯粹由手指决定，不读 scrollTop——队列裁剪在拖动中途换坐标系时
    // 跟手也不会被带偏（裁剪同时改 slide 布局与 scrollTop，视觉是连续的）。
    const next = drag.originTranslate + (deltaY - drag.baselineY);
    applyTranslate(clamp(next, drag.minTranslate, drag.maxTranslate));
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
      slideCount: current.slides.length,
    });
    // 记住目标**节点**而不是坐标：队列裁剪会在动画途中改坐标系，节点身份不会变。
    const target =
      current.slides[targetIndex] ?? current.slides[current.anchorIndex] ?? null;
    settleTo(target, computeShortsPagerVelocity(current.samples));
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
    const top = readSlideTop(slide);
    clearTranslate();
    setScrollTop(clamp(top, 0, getMaxScrollTop()));
  };
  const handleViewportResize = () => {
    // 尺寸都变了，正在跑的动画按旧尺寸算的终点已经没有意义。
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
    // 落点动画进行中时位置早已提交，直接清干净即可；只有拖到一半被卸载
    // （手指还按着）才需要把跟手位移落回 scrollTop。
    if (settling) {
      finishMotion();
    } else if (translate !== 0) {
      const top = getEffectiveTop();
      clearTranslate();
      setScrollTop(clamp(top, 0, getMaxScrollTop()));
    } else {
      clearTranslate();
    }
    detachSettleListeners();
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
  trackRef: React.RefObject<HTMLElement | null>;
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
    const track = optionsRef.current.trackRef.current;
    if (!root || !track) return;
    return createShortsSwipePager({
      root,
      track,
      usesDocumentScroll,
      // 走 ref 读取，回调换引用不会重挂监听。
      getAnchorSlide: () => optionsRef.current.getAnchorSlide(),
    });
  }, [enabled, usesDocumentScroll]);
}
