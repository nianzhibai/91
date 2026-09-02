/**
 * 移动端“双击侧边快进/快退”的分区与连击状态机。
 *
 * 参考移动端 YouTube 的机制而不只是它的秒数：真正决定手感的是连击窗口——
 * 第一次双击点亮某一侧之后，窗口内的后续点击继续在同一侧累加，而不是每次
 * 都要重新双击。ArtPlayer 的点击派发按 300ms 判定双击，一串连点拿到的事件
 * 是 click / dblclick 交替出现的，所以窗口开启后两种事件都算一次累加。
 *
 * 真实 seek 不在每次点击时提交：和键盘左右键一样，先累计目标时间并预览
 * 进度条，连击窗口结束后只提交一次，避免一串点击把 HLS 缓冲反复冲掉。
 */

/** 侧边热区各占播放器宽度的比例，中间留给双击播放/暂停。 */
export const SIDE_TAP_ZONE_RATIO = 0.35;
/** 单次双击快进/快退的秒数。 */
export const DOUBLE_TAP_SEEK_SECONDS = 10;
/** 连击窗口：最后一次点击后多久结束累计并提交真实 seek。 */
export const DOUBLE_TAP_CHAIN_WINDOW_MS = 700;

export type TapZone = "left" | "center" | "right";
export type TapKind = "click" | "dblclick";
export type SeekChainSide = "left" | "right";

export type SeekChain = {
  /** 累计净步数：向右为正、向左为负 */
  steps: number;
  /** 最近一次点击落在哪一侧，决定提示浮层出现的位置 */
  side: SeekChainSide;
  /** 最近一次点击的时间戳，用于判断连击窗口是否过期 */
  lastTapAt: number;
};

export type DoubleTapEvent = {
  kind: TapKind;
  zone: TapZone;
  at: number;
};

export type DoubleTapAction =
  /** 普通单击等不参与快进的点击，交回 ArtPlayer 的默认行为 */
  | { type: "ignore" }
  /** 累加一次快进/快退：stepSeconds 是本次增量，totalSeconds 是连击净值 */
  | { type: "seek"; side: SeekChainSide; stepSeconds: number; totalSeconds: number }
  /** 结束连击并提交；toggle 表示这次点击同时要切换播放/暂停 */
  | { type: "end"; toggle: boolean };

/** 按点击横坐标划分左 / 中 / 右热区。 */
export function classifyTapZone(
  localX: number,
  width: number,
  sideRatio = SIDE_TAP_ZONE_RATIO
): TapZone {
  if (!Number.isFinite(width) || width <= 0) return "center";
  if (!Number.isFinite(localX)) return "center";
  const side = width * sideRatio;
  if (localX < side) return "left";
  if (localX > width - side) return "right";
  return "center";
}

/** 连击窗口是否仍然有效。 */
export function isSeekChainActive(
  chain: SeekChain | null,
  at: number,
  windowMs = DOUBLE_TAP_CHAIN_WINDOW_MS
): chain is SeekChain {
  if (!chain) return false;
  return at - chain.lastTapAt <= windowMs;
}

/**
 * 连击状态机。窗口未开启时只有双击侧边才会启动，避免误碰单击就跳时间；
 * 窗口开启后，同侧点击继续累加，另一侧点击则把净值往回减（等价于 YouTube
 * 上换边重新计数，但因为这里还没提交，回退更符合直觉）。
 */
export function reduceDoubleTap(
  chain: SeekChain | null,
  event: DoubleTapEvent
): { chain: SeekChain | null; action: DoubleTapAction } {
  const active = isSeekChainActive(chain, event.at) ? chain : null;

  if (event.zone === "center") {
    // 连击中点中间：提交已累计的快进并结束；双击中间保持切换播放/暂停。
    if (!active && event.kind === "click") {
      return { chain: null, action: { type: "ignore" } };
    }
    return { chain: null, action: { type: "end", toggle: event.kind === "dblclick" } };
  }

  const direction = event.zone === "left" ? -1 : 1;
  if (!active) {
    if (event.kind === "click") return { chain: null, action: { type: "ignore" } };
    const started: SeekChain = {
      steps: direction,
      side: event.zone,
      lastTapAt: event.at,
    };
    return { chain: started, action: seekAction(started, direction) };
  }

  const next: SeekChain = {
    steps: active.steps + direction,
    side: event.zone,
    lastTapAt: event.at,
  };
  return { chain: next, action: seekAction(next, direction) };
}

/** 目标时间：从上一次的累计目标（没有就用当前播放位置）继续加减并夹住边界。 */
export function computeDoubleTapSeekTime(input: {
  baseTime: number;
  stepSeconds: number;
  duration: number;
}): number {
  const { baseTime, stepSeconds, duration } = input;
  const target = baseTime + stepSeconds;
  if (target < 0) return 0;
  if (target > duration) return duration;
  return target;
}

/** 浮层文案：连击净值抵消回 0 时明确告诉用户回到了原处。 */
export function formatDoubleTapSeekLabel(totalSeconds: number) {
  if (totalSeconds === 0) return "回到原位";
  const action = totalSeconds > 0 ? "快进" : "快退";
  return `${action} ${Math.abs(totalSeconds)} 秒`;
}

function seekAction(chain: SeekChain, direction: number): DoubleTapAction {
  return {
    type: "seek",
    side: chain.side,
    stepSeconds: direction * DOUBLE_TAP_SEEK_SECONDS,
    totalSeconds: chain.steps * DOUBLE_TAP_SEEK_SECONDS,
  };
}
