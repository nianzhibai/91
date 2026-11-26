// 短视频播放的缓冲水位与视频缓存窗口判定。全部是纯函数，只读取
// media element 的少量字段，因此可以在 Node 测试里用普通对象驱动。

// 当前视频至少有这么多秒的前向缓冲后，才允许后续视频开始预加载。
// 这同时是自适应门槛的上限：低码率视频完全保持这个原有行为。
export const ACTIVE_PRELOAD_BUFFER_SECONDS = 12;

// 预加载授权一旦发出，只有当前视频前向缓冲跌破这个秒数（或发生 stall）
// 才收回。高低水位之间不动作，避免缓冲量在 12s 附近波动时
// 反复绑定/剥离后续视频的 src、丢弃已预加载的数据。
export const ACTIVE_PRELOAD_KEEP_SECONDS = 4;

// 门槛按"秒"定，带宽却是按字节付的。本库平均码率约 10 Mbps——12 秒就是
// ~15 MB，网速跟不上时这个门槛永远达不到，预载授权一直发不出去，整页
// 退化成没有预加载的串行加载（HUD 上表现为 preload 长期 held）。所以
// 真正的预算是这份字节数，秒数只是它在某个码率下的换算结果。
export const ACTIVE_PRELOAD_BUFFER_BYTES = 4 * 1024 * 1024;

// 换算结果的下限。字节预算再紧也要留出够播一会儿的余量，否则授权发得
// 太早，预载会直接和当前视频抢带宽。
export const ACTIVE_PRELOAD_MIN_BUFFER_SECONDS = 4;

// 低水位始终是高水位的这个比例（现状 4/12 正好是 1/3，低码率下行为不变），
// 并且有自己的地板，保证高低水位永远拉开距离、滞回不会退化成单一阈值。
export const ACTIVE_PRELOAD_KEEP_RATIO = 1 / 3;
export const ACTIVE_PRELOAD_MIN_KEEP_SECONDS = 1.5;

// 维护一个固定大小的视频窗口：窗口内才 mount 真实 <video> 壳。
// 下一屏可以轻量准备；全速预加载要等当前屏缓冲健康后才开始。
// 窗口内只要已经产生过可复用缓冲，就保留 src 复用浏览器缓存。
export const VIDEO_WINDOW_SIZE = 4;

/** 当前屏缓冲健康时，向后预载几条。 */
export const PRELOAD_AHEAD_COUNT = 2;

export function getPreloadAheadCount(activeReadyForPreload: boolean): number {
  return activeReadyForPreload ? PRELOAD_AHEAD_COUNT : 0;
}

export type SlideMediaPlan = {
  /** 挂载真实 <video> 壳（否则只画海报）。 */
  shouldMount: boolean;
  /** 绑定 src。 */
  shouldLoad: boolean;
  /** preload="auto"；否则只取 metadata。 */
  shouldEagerLoad: boolean;
  /** 已有可复用缓冲而保留 src（不是为了当前播放或预载）。 */
  shouldRetainCached: boolean;
};

export function planSlideMedia(input: {
  /** 这一屏相对当前屏的偏移，0 即当前屏。 */
  offset: number;
  inCacheWindow: boolean;
  activeReadyForPreload: boolean;
  /** 浏览器里已有这条视频可复用的缓冲。 */
  cached: boolean;
  /** 走自适应流（HLS），只预载紧邻的下一条。 */
  hasPlaybackUrl: boolean;
  usesSharedVideo: boolean;
}): SlideMediaPlan {
  const isActive = input.offset === 0;
  if (input.usesSharedVideo) {
    return {
      shouldMount: isActive,
      shouldLoad: isActive,
      shouldEagerLoad: isActive,
      shouldRetainCached: false,
    };
  }
  const ahead = input.offset > 0;
  const prepareNext = input.offset === 1;
  const preload =
    ahead &&
    (!input.hasPlaybackUrl || prepareNext) &&
    input.offset <= getPreloadAheadCount(input.activeReadyForPreload);
  const retainCached =
    !isActive &&
    input.cached &&
    (input.inCacheWindow || (ahead && input.offset <= PRELOAD_AHEAD_COUNT));
  return {
    shouldMount:
      isActive || input.inCacheWindow || prepareNext || preload || retainCached,
    shouldLoad: isActive || prepareNext || preload || retainCached,
    shouldEagerLoad: isActive || preload,
    shouldRetainCached: retainCached,
  };
}

/** shouldWarmFirstFrame 的输入切面；HTMLVideoElement 天然满足前两项。 */
export type FirstFrameWarmProbe = {
  isActive: boolean;
  /** 该 slide 是否已经绑定了 src */
  shouldLoad: boolean;
  /**
   * 该元素是不是"播放位"——即将被 play() 的那一个。播放位由 play() 自己
   * 清掉 show-poster 标志，不该被 seek 打断；只有候补位才需要预热。
   */
  isPlaybackElement: boolean;
  readyState: number;
  currentTime: number;
};

export function shouldWarmFirstFrame(probe: FirstFrameWarmProbe): boolean {
  if (probe.isActive) return false;
  if (!probe.shouldLoad) return false;
  if (probe.isPlaybackElement) return false;
  if (probe.readyState < 1) return false;
  return probe.currentTime === 0;
}

/** 预热用的落点：足够小以视觉上仍是首帧，又足以让 seek 真的发生。 */
export const FIRST_FRAME_WARM_TIME = 0.001;

/** 判定所需的最小视频元素切面；HTMLVideoElement 天然满足。 */
export type BufferedMediaProbe = {
  currentTime: number;
  duration: number;
  readyState: number;
  buffered: {
    length: number;
    start(index: number): number;
    end(index: number): number;
  };
};

export function clamp(n: number, min: number, max: number) {
  return n < min ? min : n > max ? max : n;
}

export function getVideoWindowBounds(highestViewedIndex: number, itemCount: number) {
  const size = Math.min(VIDEO_WINDOW_SIZE, itemCount);
  if (size <= 0 || highestViewedIndex < 0) return { start: 0, end: -1 };

  const end = clamp(highestViewedIndex, 0, itemCount - 1);
  const start = Math.max(0, end - size + 1);
  return { start, end };
}

/** 已经缓冲到片尾（含误差余量），不会再因网络卡顿 */
export function videoBufferedToEnd(video: BufferedMediaProbe) {
  const duration = Number.isFinite(video.duration) ? video.duration : 0;
  if (duration <= 0) return false;
  const remaining = Math.max(0, duration - (video.currentTime || 0));
  return bufferedAheadSeconds(video) >= remaining - 0.25;
}

export function videoHasBufferedData(video: BufferedMediaProbe) {
  for (let i = 0; i < video.buffered.length; i += 1) {
    if (video.buffered.end(i) > video.buffered.start(i)) {
      return true;
    }
  }
  return false;
}

export function preloadBufferSecondsFor(bytesPerSecond: number) {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) {
    return ACTIVE_PRELOAD_BUFFER_SECONDS;
  }
  return clamp(
    ACTIVE_PRELOAD_BUFFER_BYTES / bytesPerSecond,
    ACTIVE_PRELOAD_MIN_BUFFER_SECONDS,
    ACTIVE_PRELOAD_BUFFER_SECONDS
  );
}

/** 低水位跟着高水位等比缩放，保证滞回区间不会被压没。 */
export function preloadKeepSecondsFor(bufferSeconds: number) {
  return Math.max(
    ACTIVE_PRELOAD_MIN_KEEP_SECONDS,
    bufferSeconds * ACTIVE_PRELOAD_KEEP_RATIO
  );
}

/** 由 sizeBytes / durationSeconds 得到平均码率；任一缺失时返回 0。 */
export function averageBytesPerSecond(input: {
  sizeBytes?: number;
  durationSeconds?: number;
}) {
  const size = input.sizeBytes ?? 0;
  const duration = input.durationSeconds ?? 0;
  if (size <= 0 || duration <= 0) return 0;
  return size / duration;
}

/** 前向缓冲健康（达到高水位或已缓冲到结尾），可以放心预加载后续视频 */
export function videoHasComfortableBuffer(
  video: BufferedMediaProbe,
  bufferSeconds = ACTIVE_PRELOAD_BUFFER_SECONDS
) {
  if (video.readyState < 3) return false;
  if (videoBufferedToEnd(video)) return true;
  return bufferedAheadSeconds(video) >= bufferSeconds;
}

/** 前向缓冲告急（跌破低水位且没缓冲到结尾），应收回预加载授权 */
export function videoBufferIsCritical(
  video: BufferedMediaProbe,
  keepSeconds = ACTIVE_PRELOAD_KEEP_SECONDS
) {
  if (video.readyState < 3) return true;
  if (videoBufferedToEnd(video)) return false;
  return bufferedAheadSeconds(video) < keepSeconds;
}

export function bufferedAheadSeconds(video: BufferedMediaProbe) {
  const current = video.currentTime || 0;
  for (let i = 0; i < video.buffered.length; i += 1) {
    const start = video.buffered.start(i);
    const end = video.buffered.end(i);
    if (start <= current + 0.25 && end > current) {
      return Math.max(0, end - current);
    }
  }
  return 0;
}
