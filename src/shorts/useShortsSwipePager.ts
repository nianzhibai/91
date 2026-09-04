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
 * 落点这样排是为了健壮性：位置提交不依赖 transitionend（那个事件在标签页
 * 切走时会丢），也让队列裁剪的坐标平移天然连续。它**不**改变视频切换时机——
 * 活跃屏仍由 IntersectionObserver 按视觉位置判定，详见 settleTo 的注释。
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
/**
 * 中间地带的第二条通路：抬手速度够快也算轻扫。
 *
 * 只看整段按压时长（douyin 的做法）有个真实的糙点——先把手指搭在屏幕上停一会
 * 再甩，elapsedMs 早就超过 150ms，必定被判回弹。而"点一下暂停、再甩走"是很
 * 常见的用法。速度采样本来就在手里，用它补一条通路即可。
 * 0.5 px/ms 意味着最后 100ms 内至少走了 50px，是明确的甩动而不是慢拖。
 */
export const SHORTS_PAGER_FLICK_VELOCITY_PX_PER_MS = 0.5;
/**
 * 切屏落点时长，固定值——douyin 用的也是固定 300ms。
 *
 * 这里曾经按抬手速度反解时长（3 × 距离 / 时长），前提是"动画首帧速度接上
 * 手指离开时的速度"。但那个前提只在剩余距离与手指行程同量级时成立：FLIP
 * 之后剩余距离恒为约一屏（补偿量 = 屏高 − 手指位移），而甩动行程只有几十
 * 像素，反解出来的时长必然远超上限，整套机制退化成一个常数 380ms——甩得
 * 越快反而越慢。douyin 和 TikTok 明确不做速度连续：固定时长、让动画起速就
 * 快过手指，这才是"甩一下就翻过去"的手感来源。
 */
export const SHORTS_PAGER_COMMIT_SETTLE_MS = 300;
/** 回弹时长区间：按剩余距离取，短距离回弹不拖泥带水。 */
export const SHORTS_PAGER_MIN_SETTLE_MS = 180;
export const SHORTS_PAGER_MAX_BOUNCE_MS = 260;
/** 到头 / 到尾继续拖时的阻尼系数与最大越界距离（占一屏的比例）。 */
export const SHORTS_PAGER_EDGE_RESISTANCE = 0.35;
export const SHORTS_PAGER_EDGE_LIMIT_RATIO = 0.12;
/**
 * 落点缓动，easeOutCubic 的 cubic-bezier 形式。这是一条手调契约：起始斜率
 * （≈2.84）要明显快于手指，终点斜率为 0 才不会"撞"上落点。300ms 配这条曲线
 * 走完 95% 只要约 190ms，比 douyin 的 300ms 线性观感更干脆。
 */
export const SHORTS_PAGER_SETTLE_EASING = "cubic-bezier(0.215, 0.61, 0.355, 1)";
/** 抬手速度取这个时间窗内的平均值，避免最后一帧抖动主导结果。 */
export const SHORTS_PAGER_VELOCITY_WINDOW_MS = 100;
/**
 * transitionend 兜底。标签页切走、合成器丢事件时它可能不来；位置早已提交，
 * 这里只是保证 will-change 和内联 transform 不会一直挂在轨道上。
 */
export const SHORTS_PAGER_SETTLE_FALLBACK_MS = 80;
/**
 * 竖滑之后吞掉合成 click 的时间窗。合成 click 紧跟在同一次 touchend 之后
 * 到达，这个窗口只要覆盖住它即可；下一次按下也会立刻解除，用户真正的轻点
 * 不会被误伤。
 */
const SHORTS_PAGER_CLICK_GUARD_MS = 300;
/**
 * 一次滚轮手势内累计位移达到这个像素数就切一屏。
 * 鼠标一格通常是 100px（deltaMode=0）或 3 行（deltaMode=1，≈48px），
 * 取 40 保证任何一格都够；触控板轻推一下也够。
 */
export const SHORTS_PAGER_WHEEL_STEP_PX = 40;
/**
 * 两次 wheel 事件间隔超过这个毫秒数就算新的一次手势。
 *
 * 这是"一次手势最多切一屏"在滚轮上的落点，也是触控板惯性尾巴的分水岭：
 * 触控板一次轻扫会连发几十个事件、间隔只有十几毫秒，尾巴能拖一秒多；
 * 不按手势聚合的话一次轻扫会连翻十几条视频。鼠标滚轮有意识地一格一格滚
 * 时，间隔通常在 150ms 以上，每一格因此都是独立手势。
 */
export const SHORTS_PAGER_WHEEL_GESTURE_GAP_MS = 120;
/** deltaMode=1（按行）时一行折算多少像素。 */
export const SHORTS_WHEEL_LINE_HEIGHT_PX = 16;
/**
 * 静止时允许偏离吸附点的最大像素数，超过就自我纠正。
 *
 * 这是一条**不变量**而不是又一个特例修复："静止时画面绝不停在两条视频之间"。
 * 关掉原生吸附之后，任何一条我们没预料到的路径——浏览器的滚动锚定、某个
 * 漏掉 preventDefault 的输入、扩展程序、页内查找、无障碍工具——都会把滚动
 * 位置留在半路，而没有任何机制会把它纠回来。与其逐个堵，不如让这条不变量
 * 自己成立。
 */
export const SHORTS_PAGER_ALIGNMENT_TOLERANCE_PX = 2;
/** 纠正前的防抖：等滚动真正停下来再判断，别和进行中的动作打架。 */
export const SHORTS_PAGER_ALIGNMENT_CHECK_MS = 120;
/** 起点标记：滑动手势不接管这个子树内按下的触摸（如底部进度条）。 */
export const SHORTS_NO_SWIPE_ATTRIBUTE = "data-shorts-no-swipe";
/** 这些元素上的 click 永远不能被合成 click 守卫吞掉。 */
export const SHORTS_INTERACTIVE_SELECTOR =
  `button, a, input, [role="button"], [${SHORTS_NO_SWIPE_ATTRIBUTE}]`;

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
  /** 抬手瞬间的纵向速度（px/ms，向下为正）。 */
  velocityPxPerMs: number;
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
  const { deltaY, elapsedMs, velocityPxPerMs, viewportHeight, anchorIndex, slideCount } =
    input;
  const lastIndex = Math.max(0, slideCount - 1);
  const distance = Math.abs(deltaY);
  const stay = clamp(anchorIndex, 0, lastIndex);

  if (distance < SHORTS_PAGER_MIN_COMMIT_PX) return stay;

  if (distance <= viewportHeight * SHORTS_PAGER_COMMIT_RATIO) {
    // 中间地带：短促轻扫才切屏。抬手速度与位移同向且够快时也算——
    // 只看整段按压时长会把"先按住再甩"误判成回弹。
    const flickedFast =
      Math.abs(velocityPxPerMs) >= SHORTS_PAGER_FLICK_VELOCITY_PX_PER_MS &&
      velocityPxPerMs < 0 === deltaY < 0;
    if (elapsedMs >= SHORTS_PAGER_FLICK_MS && !flickedFast) return stay;
  }

  const next = deltaY < 0 ? anchorIndex + 1 : anchorIndex - 1;
  // 到头/到尾时 clamp 会把目标压回 anchorIndex，等价于回弹。
  return clamp(next, 0, lastIndex);
}

/**
 * 落点动画时长。切屏用固定值，回弹按剩余距离取。
 *
 * 切屏之所以是常数：见 SHORTS_PAGER_COMMIT_SETTLE_MS 的注释——"按抬手速度
 * 反解"在 FLIP 的几何下永远被夹到上限，是一套写了但从不生效的机制。
 * 回弹之所以按距离：拖 20px 松手和拖 400px 松手要是同样时长，短距离会显得
 * 黏；按距离取之后回弹一定比切屏短，符合直觉。
 */
export function resolveShortsPagerSettleDuration(input: {
  remainingPx: number;
  viewportHeight: number;
  /** true = 切到相邻一屏，false = 回弹到原处 */
  committed: boolean;
}): number {
  const remaining = Math.abs(input.remainingPx);
  if (remaining < 1) return 0;
  if (input.committed) return SHORTS_PAGER_COMMIT_SETTLE_MS;
  // 回弹距离最多就是半屏（超过就该切屏了），按这个量程归一化。
  const span = Math.max(1, input.viewportHeight) / 2;
  const ratio = clamp(remaining / span, 0, 1);
  return (
    SHORTS_PAGER_MIN_SETTLE_MS +
    (SHORTS_PAGER_MAX_BOUNCE_MS - SHORTS_PAGER_MIN_SETTLE_MS) * ratio
  );
}

/**
 * 到头 / 到尾继续拖时的阻尼。硬 clamp 会让画面完全不动，读起来像卡死；
 * 给一段带阻尼的越界位移，手指能推动一点、松手弹回来，才是"到头了"。
 */
export function applyShortsPagerEdgeResistance(input: {
  value: number;
  min: number;
  max: number;
  viewportHeight: number;
}): number {
  const limit = Math.max(0, input.viewportHeight) * SHORTS_PAGER_EDGE_LIMIT_RATIO;
  if (input.value > input.max) {
    const over = input.value - input.max;
    return input.max + Math.min(over * SHORTS_PAGER_EDGE_RESISTANCE, limit);
  }
  if (input.value < input.min) {
    const over = input.min - input.value;
    return input.min - Math.min(over * SHORTS_PAGER_EDGE_RESISTANCE, limit);
  }
  return input.value;
}

/** 把 wheel 的三种 deltaMode 统一换算成像素，后面只跟像素打交道。 */
export function normalizeShortsWheelDelta(input: {
  deltaY: number;
  deltaMode: number;
  viewportHeight: number;
}): number {
  if (!Number.isFinite(input.deltaY)) return 0;
  // 1 = 按行（Firefox 常用），2 = 按页
  if (input.deltaMode === 1) return input.deltaY * SHORTS_WHEEL_LINE_HEIGHT_PX;
  if (input.deltaMode === 2) {
    return input.deltaY * Math.max(1, input.viewportHeight);
  }
  return input.deltaY;
}

export type ShortsWheelGestureState = {
  /** 上一次 wheel 事件的时间戳 */
  lastEventAt: number;
  /** 本次手势内累计的像素位移 */
  accumulated: number;
  /** 本次手势是否已经消费过一步——之后的惯性尾巴一律忽略 */
  consumed: boolean;
};

export const INITIAL_SHORTS_WHEEL_STATE: ShortsWheelGestureState = {
  lastEventAt: Number.NEGATIVE_INFINITY,
  accumulated: 0,
  consumed: false,
};

/**
 * 一次 wheel 事件该不该切屏，以及切哪个方向。
 *
 * 规则与手指完全一致：**一次手势最多切一屏**。事件流按间隔切分成手势，
 * 每个手势里累计位移第一次越过阈值就切一屏，之后这次手势里剩下的事件
 * （触控板的惯性尾巴）全部忽略。
 *
 * 返回新的状态而不是就地修改：这样整条判定是纯函数，能脱离浏览器直接测。
 */
export function resolveShortsWheelStep(input: {
  state: ShortsWheelGestureState;
  deltaPx: number;
  now: number;
}): { state: ShortsWheelGestureState; step: -1 | 0 | 1 } {
  const continuing =
    input.now - input.state.lastEventAt < SHORTS_PAGER_WHEEL_GESTURE_GAP_MS;
  const accumulated = continuing ? input.state.accumulated : 0;
  const consumed = continuing ? input.state.consumed : false;

  if (consumed) {
    // 同一次手势已经切过屏，剩下的惯性照单全收但不产生动作。
    return {
      state: { lastEventAt: input.now, accumulated: 0, consumed: true },
      step: 0,
    };
  }

  const next = accumulated + input.deltaPx;
  if (Math.abs(next) < SHORTS_PAGER_WHEEL_STEP_PX) {
    return {
      state: { lastEventAt: input.now, accumulated: next, consumed: false },
      step: 0,
    };
  }
  return {
    state: { lastEventAt: input.now, accumulated: 0, consumed: true },
    step: next > 0 ? 1 : -1,
  };
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
  /**
   * 手指正在拖动（已判定为纵向）时通知一次 true，手势收尾时 false。
   * 页面据此在跟手阶段冻结 IntersectionObserver 的活跃屏判定——IO 看的是
   * 视觉位置，拖动中轨道 translate 一直在变，它会在一次滑动里反复翻转，
   * 每翻一次就是一整套暂停/起播/预载授权清零/媒体监听重建，全砸在跟手那几帧上。
   * douyin 同样只在 touchEnd 里推进 localIndex。
   */
  onGestureActiveChange?: (active: boolean) => void;
};

/**
 * 手势状态机本体，不依赖 React。这样它能脱离渲染器直接接受完整的事件序列
 * 测试——按下 / 移动 / 抬手 / 多指 / 打断动画 / 落点提交每条分支都能覆盖到，
 * 这些恰恰是"手感"真正落在的地方。返回值是解除绑定的函数。
 */
export function createShortsSwipePager(host: ShortsSwipePagerHost) {
  const { root, track, usesDocumentScroll } = host;

  // ---- 滚动目标适配：容器滚动 / 文档滚动共用同一套位移逻辑 ----
  // 文档滚动产生的 scroll 事件只会到达 document/window，不会向下传播给
  // `.shorts-feed`；事件订阅必须和下面的读写操作使用同一个真实滚动宿主。
  const scrollEventTarget = usesDocumentScroll ? window : root;
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
   * 这么排换来的是健壮性：
   * 1. 位置提交不依赖 transitionend。那个事件在标签页切走、合成器丢帧时会丢；
   *    在这个排法里丢了也只是 will-change 多留一会儿，绝不会卡在两屏之间。
   * 2. 长会话队列裁剪天然连续：它保持 `scrollTop - slide 布局位置` 不变，
   *    轨道上的 translate 原样继续，不需要任何额外处理。
   *
   * 注意它**不**改变视频切换的时机。活跃屏仍由 IntersectionObserver 判定，
   * 而 IO 看的是视觉矩形——补偿量正是为了让视觉位置保持连续，所以切换依旧
   * 发生在画面视觉越过 60% 的那一刻（easeOutCubic 前段快，约在动画前 1/4）。
   * 做不做 FLIP 在这一点上完全一样。
   */
  const settleTo = (target: HTMLElement | null, committed: boolean) => {
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
      viewportHeight: getViewportHeight(),
      committed,
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
    // 只吞掉落在 slide 空白处的那一次——它唯一的去处是"单击切换播放/暂停"。
    // 点赞、分享、隐藏、详情链接、进度条都必须放行：守卫在"按住把飞行中的
    // 动画停下来"时也会武装，那一下如果正好按在按钮上，吞掉就是功能失灵。
    const target = event.target as Element | null;
    if (
      typeof target?.closest === "function" &&
      target.closest(SHORTS_INTERACTIVE_SELECTOR)
    ) {
      return;
    }
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
  // 用 pointer 事件而不是 touch 事件：同一套代码同时吃手指和鼠标，桌面
  // 拖拽是白送的。zyronon/douyin 的 SlideVertical 也只绑 pointerdown/move/up，
  // 全仓一处 touch 事件都没有——分成"移动端一套、桌面一套"是我们凭空加的
  // 平台特例。
  let drag: PagerDrag | null = null;
  /** 当前按在屏幕上的所有指针；多于一个就交出手势。 */
  const activePointers = new Set<number>();
  /** 正在驱动本次手势的那一个指针。 */
  let dragPointerId: number | null = null;
  let gestureActive = false;
  const setGestureActive = (active: boolean) => {
    if (gestureActive === active) return;
    gestureActive = active;
    host.onGestureActiveChange?.(active);
  };

  /** 多指 / 取消等中断：就近吸附，不要停在两屏之间。 */
  const settleToNearest = () => {
    const slides = drag?.slides ?? readSlides();
    const slideTops = drag?.slideTops ?? readSlideTops();
    const index = findNearestShortsSlideIndex(slideTops, getEffectiveTop());
    if (index < 0) return;
    settleTo(slides[index] ?? null, false);
  };

  const handlePointerDown = (event: PointerEvent) => {
    activePointers.add(event.pointerId);
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

    dragPointerId = null;
    // 鼠标只认左键；第二根手指落下时整只手势作废。
    if (event.pointerType === "mouse" && event.button !== 0) return bail();
    if (activePointers.size !== 1) return bail();

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
    const now = performance.now();
    dragPointerId = event.pointerId;

    drag = {
      startX: event.clientX,
      startY: event.clientY,
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
      samples: [{ y: event.clientY, t: now }],
    };
  };

  const handlePointerMove = (event: PointerEvent) => {
    if (!drag || drag.abandoned) return;
    if (event.pointerId !== dragPointerId) return;
    if (activePointers.size !== 1) {
      // 第二根手指落下：交出手势，已经拖开的部分就近吸附回去。
      const shouldSettle = drag.committed || drag.interrupted;
      drag.abandoned = true;
      setGestureActive(false);
      if (shouldSettle) settleToNearest();
      return;
    }

    const touch = event;
    const deltaX = touch.clientX - drag.startX;
    const deltaY = touch.clientY - drag.startY;

    if (!drag.committed) {
      // 与视频上的横向 seek 共用同一套方向判定，两者互斥，不会同时激活。
      const intent = classifyTouchSeekIntent(deltaX, deltaY);
      if (intent === "pending") return;
      if (intent === "seek") {
        drag.abandoned = true;
        setGestureActive(false);
        // 横向 seek 与纵向落点互不干扰，被打断的那次动画照样要收尾。
        if (drag.interrupted) settleToNearest();
        return;
      }
      drag.committed = true;
      setGestureActive(true);
        // 鼠标拖到容器外面也要继续收事件；触摸本来就隐式捕获，这里是给桌面用的。
      try {
        root.setPointerCapture(event.pointerId);
      } catch {
        // 指针已经抬起 / 不支持捕获时忽略，后续事件照常走冒泡。
      }
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
    applyTranslate(
      applyShortsPagerEdgeResistance({
        value: next,
        min: drag.minTranslate,
        max: drag.maxTranslate,
        viewportHeight: drag.viewportHeight,
      })
    );
  };

  const handlePointerUp = (event: PointerEvent) => {
    activePointers.delete(event.pointerId);
    if (dragPointerId !== null && event.pointerId !== dragPointerId) return;
    dragPointerId = null;
    const current = drag;
    drag = null;
    setGestureActive(false);
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

    const now = performance.now();
    current.samples.push({ y: event.clientY, t: now });
    const deltaY = event.clientY - current.startY;

    const targetIndex = resolveShortsPagerTargetIndex({
      deltaY,
      elapsedMs: now - current.startTime,
      velocityPxPerMs: computeShortsPagerVelocity(current.samples),
      viewportHeight: current.viewportHeight,
      anchorIndex: current.anchorIndex,
      slideCount: current.slides.length,
    });
    // 记住目标**节点**而不是坐标：队列裁剪会在动画途中改坐标系，节点身份不会变。
    const target =
      current.slides[targetIndex] ?? current.slides[current.anchorIndex] ?? null;
    settleTo(target, targetIndex !== current.anchorIndex);
  };

  /**
   * 把当前屏推进 / 退回一屏。滚轮和拖拽最终都汇到 settleTo，因此过渡完全
   * 一致——用户不该从动画上看出自己刚才用的是手指、鼠标还是滚轮。
   */
  const stepBy = (direction: 1 | -1) => {
    if (drag) return;
    const slides = readSlides();
    if (slides.length === 0) return;
    const slideTops = readSlideTops();
    // 用视觉位置定锚：动画途中再来一次滚轮，是从画面此刻所在的位置推进。
    const index = findNearestShortsSlideIndex(slideTops, getEffectiveTop());
    if (index < 0) return;
    const target = clamp(index + direction, 0, slides.length - 1);
    if (target === index) return;
    settleTo(slides[target] ?? null, true);
  };

  let wheelState = INITIAL_SHORTS_WHEEL_STATE;

  const handleWheel = (event: WheelEvent) => {
    // 捏合缩放（Ctrl+滚轮）交还给浏览器，其余一律接管。
    if (event.ctrlKey) return;
    // 这里刻意**不**看 data-shorts-no-swipe：那个标记是给拖拽用的（进度条要
    // 吃掉整根手指），滚轮没有理由认它。漏掉任何一处 preventDefault，原生
    // 滚动就会从那里溜出去，而吸附已经关了，画面会停在两条视频中间。
    if (event.cancelable) event.preventDefault();
    if (drag) return;

    const decision = resolveShortsWheelStep({
      state: wheelState,
      deltaPx: normalizeShortsWheelDelta({
        deltaY: event.deltaY,
        deltaMode: event.deltaMode,
        viewportHeight: getViewportHeight(),
      }),
      now: performance.now(),
    });
    wheelState = decision.state;
    if (decision.step !== 0) stepBy(decision.step);
  };

  const handlePointerCancel = (event: PointerEvent) => {
    activePointers.delete(event.pointerId);
    if (dragPointerId !== null && event.pointerId !== dragPointerId) return;
    dragPointerId = null;
    const current = drag;
    drag = null;
    setGestureActive(false);
    if (current && (current.committed || current.interrupted)) {
      settleToNearest();
    }
  };

  // ---- 兜底不变量：静止时绝不停在两条视频之间 ----
  // 关掉原生吸附之后，没有任何机制会把意外的滚动偏移纠回来。这里不去逐个
  // 堵源头（浏览器滚动锚定、漏掉的 preventDefault、扩展、页内查找……），
  // 而是让"静止即对齐"这条不变量自己成立：滚动停下来之后一量，偏了就吸回去。
  let alignmentCheckTimer: number | null = null;
  const cancelAlignmentCheck = () => {
    if (alignmentCheckTimer === null) return;
    window.clearTimeout(alignmentCheckTimer);
    alignmentCheckTimer = null;
  };
  const handleScroll = () => {
    // 我们自己驱动的位移不算意外，跳过。
    if (drag || settling) return;
    cancelAlignmentCheck();
    alignmentCheckTimer = window.setTimeout(() => {
      alignmentCheckTimer = null;
      if (drag || settling) return;
      const slides = readSlides();
      if (slides.length === 0) return;
      const slideTops = readSlideTops();
      const effectiveTop = getEffectiveTop();
      const index = findNearestShortsSlideIndex(slideTops, effectiveTop);
      if (index < 0) return;
      const drift = Math.abs(slideTops[index] - effectiveTop);
      if (drift <= SHORTS_PAGER_ALIGNMENT_TOLERANCE_PX) return;
      // 就近吸附，用回弹时长——这不是一次切屏，只是把画面扶正。
      settleTo(slides[index] ?? null, false);
    }, SHORTS_PAGER_ALIGNMENT_CHECK_MS);
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

  scrollEventTarget.addEventListener("scroll", handleScroll, { passive: true });
  root.addEventListener("wheel", handleWheel, { passive: false });
  root.addEventListener("pointerdown", handlePointerDown, { passive: true });
  root.addEventListener("pointermove", handlePointerMove, { passive: false });
  root.addEventListener("pointerup", handlePointerUp);
  root.addEventListener("pointercancel", handlePointerCancel);
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
    setGestureActive(false);
    if (realignFrame !== null) window.cancelAnimationFrame(realignFrame);
    if (realignTimer !== null) window.clearTimeout(realignTimer);
    cancelAlignmentCheck();
    scrollEventTarget.removeEventListener("scroll", handleScroll);
    root.removeEventListener("wheel", handleWheel);
    root.removeEventListener("pointerdown", handlePointerDown);
    root.removeEventListener("pointermove", handlePointerMove);
    root.removeEventListener("pointerup", handlePointerUp);
    root.removeEventListener("pointercancel", handlePointerCancel);
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
  onGestureActiveChange?: (active: boolean) => void;
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
      onGestureActiveChange: (active) =>
        optionsRef.current.onGestureActiveChange?.(active),
    });
  }, [enabled, usesDocumentScroll]);
}
