import { reportShortsEvents, type ShortsWatchEventPayload } from "@/data/videos";

/**
 * 观看结算上报。
 *
 * 这是推荐链路唯一的真实行为输入：没有它，服务端只能按随机或全局计数排序。
 * 上报必须做到「不阻塞播放、不影响用户」：异步批量、失败静默、页面卸载时用
 * keepalive 兜底。
 */

export type ShortsWatchKind = ShortsWatchEventPayload["kind"];

export type ShortsWatchEvent = ShortsWatchEventPayload;

/** 攒够这么多条就立刻发，不等定时器。 */
export const SHORTS_EVENT_BATCH_SIZE = 8;

/** 不足一批时最多等这么久，避免单条事件长时间滞留。 */
export const SHORTS_EVENT_FLUSH_DELAY_MS = 5000;

/** 看完的观看比例阈值。短视频里 90% 基本等于看完了。 */
export const SHORTS_COMPLETE_RATIO = 0.9;

/** 划走的观看比例阈值。 */
export const SHORTS_SKIP_RATIO = 0.15;

/** 时长未知时按绝对时长兜底的阈值。 */
export const SHORTS_SKIP_FALLBACK_MS = 3000;

/** 媒体时间回退到接近开头才算一次循环，拖动进度条不算。 */
const LOOP_RESTART_TOLERANCE_MS = 500;

/** 解析 <video> 的重试次数与间隔。 */
const VIDEO_RESOLVE_ATTEMPTS = 10;
const VIDEO_RESOLVE_INTERVAL_MS = 120;

/**
 * 把一次观看折算成结算类型。
 *
 * 用比例而不是绝对时长：15 秒的视频看 8 秒和 3 分钟的视频看 8 秒完全不是
 * 一回事。循环播放单独计，因为用户主动让它重播几乎没有误判空间。
 */
export function classifyShortsWatch(input: {
  watchMs: number;
  durationMs: number;
  loopCount: number;
}): ShortsWatchKind {
  if (input.loopCount > 0) return "complete";
  if (!(input.durationMs > 0)) {
    // 元数据缺失（服务端 sizeBytes/durationSeconds 会成对省略）时算不出
    // 比例，只能按绝对时长兜底。
    return input.watchMs >= SHORTS_SKIP_FALLBACK_MS ? "partial" : "skip";
  }
  const ratio = Math.min(input.watchMs / input.durationMs, 1);
  if (ratio >= SHORTS_COMPLETE_RATIO) return "complete";
  if (ratio <= SHORTS_SKIP_RATIO) return "skip";
  return "partial";
}

export type ShortsWatchTracker = {
  /** 喂一次媒体时间；只在播放中累加，时间回退视为循环。 */
  observe: (currentTimeMs: number, playing: boolean) => void;
  snapshot: () => { watchMs: number; loopCount: number };
  reset: () => void;
};

/**
 * 观看时长累加器。
 *
 * 用媒体时间增量而不是墙上时间：暂停、切后台、缓冲都不该计入「看了多久」，
 * 否则「离开去倒水」会被误判成深度兴趣。
 */
export function createShortsWatchTracker(): ShortsWatchTracker {
  let watchMs = 0;
  let loopCount = 0;
  let lastTimeMs: number | null = null;

  return {
    observe(currentTimeMs, playing) {
      if (!Number.isFinite(currentTimeMs)) return;
      const previous = lastTimeMs;
      lastTimeMs = currentTimeMs;
      if (previous === null || !playing) return;
      const delta = currentTimeMs - previous;
      if (delta > 0) {
        watchMs += delta;
        return;
      }
      if (delta < 0 && currentTimeMs <= LOOP_RESTART_TOLERANCE_MS) {
        loopCount += 1;
      }
    },
    snapshot() {
      return { watchMs, loopCount };
    },
    reset() {
      watchMs = 0;
      loopCount = 0;
      lastTimeMs = null;
    },
  };
}

/**
 * 挂一次观看追踪。返回的清理函数会把累计结果结算成一条事件并入队。
 *
 * getVideo 是解析器而不是元素：slide 挂载与 effect 执行不保证同时发生，这里
 * 自己重试几次。解析不到就静默放弃 —— 少一条数据好过上报一条错数据。
 */
export function trackShortsWatch(input: {
  getVideo: () => HTMLVideoElement | null | undefined;
  videoId: string;
  position: number;
  enqueue?: (event: ShortsWatchEvent) => void;
}): () => void {
  const tracker = createShortsWatchTracker();
  let video: HTMLVideoElement | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let attempts = 0;
  let disposed = false;

  const handleTimeUpdate = () => {
    if (!video) return;
    tracker.observe(video.currentTime * 1000, !video.paused && !video.ended);
  };
  const handleVisibilityChange = () => {
    if (!video) return;
    // 切到后台后 timeupdate 会停；恢复时不要把中间的时间算成观看时长。
    tracker.observe(video.currentTime * 1000, false);
  };

  const attach = (element: HTMLVideoElement) => {
    video = element;
    element.addEventListener("timeupdate", handleTimeUpdate);
    document.addEventListener("visibilitychange", handleVisibilityChange);
  };

  const resolve = () => {
    if (disposed) return;
    const element = input.getVideo();
    if (element) {
      attach(element);
      return;
    }
    attempts += 1;
    if (attempts > VIDEO_RESOLVE_ATTEMPTS) return;
    retryTimer = globalThis.setTimeout(resolve, VIDEO_RESOLVE_INTERVAL_MS);
  };
  resolve();

  return () => {
    disposed = true;
    if (retryTimer !== null) globalThis.clearTimeout(retryTimer);
    let durationMs = 0;
    if (video) {
      handleTimeUpdate();
      video.removeEventListener("timeupdate", handleTimeUpdate);
      document.removeEventListener("visibilitychange", handleVisibilityChange);
      durationMs = Number.isFinite(video.duration) ? video.duration * 1000 : 0;
    }

    const { watchMs, loopCount } = tracker.snapshot();
    if (watchMs <= 0 && loopCount === 0) return;
    (input.enqueue ?? enqueueShortsWatchEvent)({
      videoId: input.videoId,
      kind: classifyShortsWatch({ watchMs, durationMs, loopCount }),
      watchMs: Math.round(watchMs),
      loopCount,
      position: input.position,
    });
  };
}

let pendingEvents: ShortsWatchEvent[] = [];
let flushTimer: ReturnType<typeof setTimeout> | null = null;

/** 入队一条结算。攒够一批立即发送，否则等定时器。 */
export function enqueueShortsWatchEvent(event: ShortsWatchEvent): void {
  pendingEvents = [...pendingEvents, event];
  if (pendingEvents.length >= SHORTS_EVENT_BATCH_SIZE) {
    flushShortsWatchEvents();
    return;
  }
  if (flushTimer !== null) return;
  flushTimer = globalThis.setTimeout(() => {
    flushTimer = null;
    flushShortsWatchEvents();
  }, SHORTS_EVENT_FLUSH_DELAY_MS);
  // Node 下不要因为这个定时器拖住事件循环。
  (flushTimer as unknown as { unref?: () => void }).unref?.();
}

/**
 * 立刻发送待发队列。send 可注入，便于测试不触网。
 */
export function flushShortsWatchEvents(
  send: (events: ShortsWatchEvent[]) => void = reportShortsEvents
): void {
  if (flushTimer !== null) {
    globalThis.clearTimeout(flushTimer);
    flushTimer = null;
  }
  if (pendingEvents.length === 0) return;
  const batch = pendingEvents;
  pendingEvents = [];
  send(batch);
}

/** 仅测试使用：清空队列与定时器。 */
export function resetShortsWatchEventsForTests(): void {
  if (flushTimer !== null) {
    globalThis.clearTimeout(flushTimer);
    flushTimer = null;
  }
  pendingEvents = [];
}

/** 仅测试使用：查看当前积压条数。 */
export function shortsWatchEventBacklog(): number {
  return pendingEvents.length;
}
