import {
  FIRST_FRAME_WARM_TIME,
  PRELOAD_AHEAD_COUNT,
  VIDEO_WINDOW_SIZE,
  planSlideMedia,
  shouldWarmFirstFrame,
  videoHasBufferedData,
} from "./mediaBuffer.js";
import { restoreShortsPlaybackRate } from "./playbackRate.js";

export type ShortsPoolMediaItem = {
  id: string;
  videoSrc: string;
  playbackUrl?: string;
  poster: string;
};

export type ShortsPoolSourceAdapter = {
  attach: (
    video: HTMLVideoElement,
    src: string,
    playbackUrl?: string,
    options?: { active?: boolean }
  ) => void;
  setActive: (video: HTMLVideoElement, active: boolean) => void;
  dispose: (video: HTMLVideoElement) => void;
};

export type ShortsPlayerPoolOptions = {
  /** iOS/WebKit binds audio autoplay authorization per media element; use 2-element ping-pong. */
  iosShared: boolean;
  /** Disable the standby preload element on iOS (for `?iosPreload=0` diagnostics). */
  standbyPreloadDisabled?: boolean;
  /** Hidden connected container used to park elements during queue resets without losing WebKit audio auth. */
  getParkingContainer?: () => HTMLElement | null;
  sourceAdapter: ShortsPoolSourceAdapter;
  /** Called whenever a pooled non-active element has buffered reusable data. */
  onSourceCached?: (videoId: string) => void;
  /** Optional factory for testing without a full browser DOM. */
  createVideoElement?: () => HTMLVideoElement;
};

export type ShortsPoolActiveSyncInput = {
  items: readonly ShortsPoolMediaItem[];
  activeIndex: number;
  muted: boolean;
  volume?: number;
};

export type ShortsPoolStandbySyncInput = {
  items: readonly ShortsPoolMediaItem[];
  activeIndex: number;
  browseDirection: -1 | 1;
  activeReadyForPreload: boolean;
  cacheWindow: { start: number; end: number };
  cachedIds: ReadonlySet<string>;
  muted: boolean;
  volume?: number;
};

type PooledVideoEntry = {
  video: HTMLVideoElement;
  index: number;
  videoId: string;
  role: "active" | "standby" | "idle";
  cleanupCacheProbe: (() => void) | null;
};

const standbyFirstFrameWarmups = new WeakMap<HTMLVideoElement, AbortController>();

function preventMediaContextMenu(event: Event) {
  event.preventDefault();
}

export function applyVideoMutedState(video: HTMLVideoElement, nextMuted: boolean) {
  try {
    if (video.muted !== nextMuted) {
      video.muted = nextMuted;
    }
  } catch {
    // ignore
  }
}

export function normalizeVideoPlaybackRate(video: HTMLVideoElement) {
  try {
    restoreShortsPlaybackRate(video);
  } catch {
    // ignore
  }
}

/**
 * 在用户手势里给一个还没播放过的持久 media element 领取有声播放授权。
 * WebKit 在 play() 内部同步就解除了该元素的自动播放限制，所以紧接着
 * 暂停不会把授权还回去；保持静音发起则避免和当前视频重叠出声。
 */
export function unlockVideoAudioPlayback(video: HTMLVideoElement) {
  applyVideoMutedState(video, true);
  let request: Promise<void> | undefined;
  try {
    request = video.play();
  } catch {
    return;
  }
  request?.catch(() => undefined);
  try {
    video.pause();
  } catch {
    // ignore
  }
}

export function stabilizeVideoAfterAudioToggle(
  video: HTMLVideoElement,
  shouldResume: () => boolean
) {
  const stabilize = () => {
    normalizeVideoPlaybackRate(video);
    if (shouldResume() && video.paused && !video.ended) {
      video.play().catch(() => undefined);
    }
  };

  stabilize();
  for (const delay of [80, 240, 600]) {
    window.setTimeout(stabilize, delay);
  }
}

export function warmStandbyFirstFrame(
  video: HTMLVideoElement,
  isStillStandby: () => boolean
) {
  standbyFirstFrameWarmups.get(video)?.abort();
  const attempt = () => {
    if (!isStillStandby()) return false;
    if (
      !shouldWarmFirstFrame({
        isActive: false,
        shouldLoad: true,
        isPlaybackElement: false,
        readyState: video.readyState,
        currentTime: video.currentTime,
      })
    ) {
      return false;
    }
    try {
      video.currentTime = FIRST_FRAME_WARM_TIME;
      return true;
    } catch {
      return false;
    }
  };
  if (attempt()) return;

  const controller = new AbortController();
  standbyFirstFrameWarmups.set(video, controller);
  const onReady = () => {
    if (!isStillStandby() || attempt()) {
      controller.abort();
    }
  };
  video.addEventListener("loadedmetadata", onReady, { signal: controller.signal });
  video.addEventListener("loadeddata", onReady, { signal: controller.signal });
}

export function releasePooledVideoSource(
  video: HTMLVideoElement,
  disposeSource: (video: HTMLVideoElement) => void
) {
  standbyFirstFrameWarmups.get(video)?.abort();
  disposeSource(video);
  try {
    video.pause();
    video.removeAttribute("src");
    video.load();
  } catch {
    // ignore
  }
}

function defaultCreateVideoElement(iosShared: boolean): HTMLVideoElement {
  const video = document.createElement("video");
  video.className = iosShared
    ? "shorts-slide__video shorts-slide__video--ios-shared"
    : "shorts-slide__video";
  video.loop = !iosShared;
  video.playsInline = true;
  video.disablePictureInPicture = true;
  video.setAttribute("playsinline", "");
  video.setAttribute("webkit-playsinline", "");
  video.setAttribute("controlslist", "nodownload");
  video.setAttribute("aria-hidden", "true");
  video.addEventListener("contextmenu", preventMediaContextMenu);
  return video;
}

/**
 * 统一的短视频命令式媒体池：
 * - iOS（iosShared=true）：维护 2 个持久 `<video>` 乒乓复用，保住 WebKit 按元素记账的有声授权；
 * - 桌面/Android（iosShared=false）：维护固定容量窗口的持久 `<video>` 池，按 `planSlideMedia`
 *   在 slide 插槽间分配与复用已缓冲源，避免每条 slide 各自维护独立 JSX `<video>` 生命周期。
 */
export class ShortsPlayerPool {
  private readonly iosShared: boolean;
  private readonly standbyPreloadDisabled: boolean;
  private readonly getParkingContainer: () => HTMLElement | null;
  private readonly sourceAdapter: ShortsPoolSourceAdapter;
  private readonly onSourceCached?: (videoId: string) => void;
  private readonly createVideoElement: () => HTMLVideoElement;

  private readonly slots = new Map<number, HTMLElement>();
  private readonly entries: PooledVideoEntry[] = [];
  private activeEntry: PooledVideoEntry | null = null;
  private iosStandbyEntry: PooledVideoEntry | null = null;

  constructor(options: ShortsPlayerPoolOptions) {
    this.iosShared = options.iosShared;
    this.standbyPreloadDisabled = Boolean(options.standbyPreloadDisabled);
    this.getParkingContainer = options.getParkingContainer ?? (() => null);
    this.sourceAdapter = options.sourceAdapter;
    this.onSourceCached = options.onSourceCached;
    this.createVideoElement =
      options.createVideoElement ?? (() => defaultCreateVideoElement(this.iosShared));
  }

  registerSlot(index: number, slot: HTMLElement | null) {
    if (slot) {
      this.slots.set(index, slot);
    } else {
      this.slots.delete(index);
    }
  }

  clearSlots() {
    this.slots.clear();
  }

  getActiveVideo(): HTMLVideoElement | undefined {
    return this.activeEntry?.video;
  }

  getStandbyVideo(): HTMLVideoElement | undefined {
    if (this.iosShared) return this.iosStandbyEntry?.video;
    return this.entries.find((entry) => entry.role === "standby")?.video;
  }

  getVideoAt(index: number): HTMLVideoElement | undefined {
    if (this.activeEntry && this.activeEntry.index === index) {
      return this.activeEntry.video;
    }
    if (this.iosShared) return undefined;
    return this.entries.find((entry) => entry.index === index && entry.role !== "idle")?.video;
  }

  forEachVideo(callback: (video: HTMLVideoElement, index: number) => void) {
    for (const entry of this.entries) {
      callback(entry.video, entry.index);
    }
  }

  /**
   * 在浏览器绘制前（useLayoutEffect）把当前活跃视频挂载到对应插槽并绑定媒体源。
   * 如果备用/缓存池里已经为这一屏准备好视频，直接提升复用，零重下。
   */
  syncActive(input: ShortsPoolActiveSyncInput): HTMLVideoElement | undefined {
    const item = input.items[input.activeIndex];
    const slot = this.slots.get(input.activeIndex);
    if (!item || !slot) return undefined;

    if (this.iosShared) {
      return this.syncIOSActive(item, input.activeIndex, slot, input.muted);
    }
    return this.syncStandardActive(item, input.activeIndex, slot, input.muted, input.volume);
  }

  /**
   * 在浏览器绘制后（useEffect）安排非活跃预载和缓存窗口内的视频元素，
   * 避免把后台 load() 和 DOM 搬移压进当帧同步渲染。
   */
  syncStandby(input: ShortsPoolStandbySyncInput) {
    if (this.iosShared) {
      this.syncIOSStandby(input);
      return;
    }
    this.syncStandardStandby(input);
  }

  applyMuted(muted: boolean) {
    if (this.iosShared) {
      if (this.activeEntry) applyVideoMutedState(this.activeEntry.video, muted);
      if (this.iosStandbyEntry) applyVideoMutedState(this.iosStandbyEntry.video, true);
      return;
    }
    for (const entry of this.entries) {
      applyVideoMutedState(entry.video, entry === this.activeEntry ? muted : true);
    }
  }

  applyVolume(volume: number | undefined) {
    if (volume === undefined) return;
    for (const entry of this.entries) {
      try {
        entry.video.volume = volume;
      } catch {
        // ignore
      }
    }
  }

  unlockStandbyAudio() {
    if (!this.iosShared) return;
    if (this.iosStandbyEntry && this.iosStandbyEntry !== this.activeEntry) {
      unlockVideoAudioPlayback(this.iosStandbyEntry.video);
    }
  }

  pauseNonActive() {
    for (const entry of this.entries) {
      if (entry === this.activeEntry) continue;
      try {
        if (!entry.video.paused) entry.video.pause();
      } catch {
        // ignore
      }
    }
  }

  pauseAll() {
    for (const entry of this.entries) {
      try {
        entry.video.pause();
      } catch {
        // ignore
      }
    }
  }

  rebaseIndices(removeCount: number) {
    if (removeCount <= 0) return;
    for (const entry of this.entries) {
      if (entry.index < 0) continue;
      entry.index = entry.index < removeCount ? -1 : entry.index - removeCount;
    }
  }

  /**
   * 切换栏目或空库重置队列时，把持久元素停放到隐藏容器再释放源，
   * 保证 WebKit 授予持久 `<video>` 的有声播放权限不会随插槽卸载丢失。
   */
  resetForQueueClear() {
    const parking = this.getParkingContainer();
    for (const entry of this.entries) {
      this.detachCacheProbe(entry);
      if (parking && entry.video.parentElement !== parking) {
        parking.appendChild(entry.video);
      }
      releasePooledVideoSource(entry.video, this.sourceAdapter.dispose);
      delete entry.video.dataset.shortsVideoId;
      normalizeVideoPlaybackRate(entry.video);
      entry.index = -1;
      entry.videoId = "";
      entry.role = "idle";
    }
    this.slots.clear();
  }

  destroy() {
    for (const entry of this.entries) {
      this.detachCacheProbe(entry);
      entry.video.removeEventListener("contextmenu", preventMediaContextMenu);
      releasePooledVideoSource(entry.video, this.sourceAdapter.dispose);
      entry.video.remove();
    }
    this.entries.length = 0;
    this.activeEntry = null;
    this.iosStandbyEntry = null;
    this.slots.clear();
  }

  private allocateEntry(): PooledVideoEntry {
    const entry: PooledVideoEntry = {
      video: this.createVideoElement(),
      index: -1,
      videoId: "",
      role: "idle",
      cleanupCacheProbe: null,
    };
    this.entries.push(entry);
    return entry;
  }

  private applyRole(
    entry: PooledVideoEntry,
    role: "active" | "standby",
    preload: "auto" | "metadata",
    muted: boolean,
    volume?: number
  ) {
    entry.role = role;
    const { video } = entry;
    this.sourceAdapter.setActive(video, role === "active");
    if (role === "active") {
      this.detachCacheProbe(entry);
      standbyFirstFrameWarmups.get(video)?.abort();
      video.preload = "auto";
      video.autoplay = true;
      video.setAttribute("autoplay", "");
      applyVideoMutedState(video, muted);
      try {
        video.defaultMuted = muted;
      } catch {
        // ignore
      }
      if (volume !== undefined) {
        try {
          video.volume = volume;
        } catch {
          // ignore
        }
      }
      return;
    }

    video.preload = preload;
    video.autoplay = false;
    video.removeAttribute("autoplay");
    applyVideoMutedState(video, true);
    try {
      video.defaultMuted = true;
    } catch {
      // ignore
    }
    if (volume !== undefined) {
      try {
        video.volume = volume;
      } catch {
        // ignore
      }
    }
  }

  private attachCacheProbe(entry: PooledVideoEntry, videoId: string) {
    this.detachCacheProbe(entry);
    if (!this.onSourceCached || this.iosShared) return;
    const onSourceCached = this.onSourceCached;
    const { video } = entry;
    const notifyIfBuffered = () => {
      if (entry.videoId !== videoId) return;
      if (video.readyState >= 3 || videoHasBufferedData(video)) {
        onSourceCached(videoId);
      }
    };
    video.addEventListener("canplay", notifyIfBuffered);
    video.addEventListener("progress", notifyIfBuffered);
    notifyIfBuffered();
    entry.cleanupCacheProbe = () => {
      video.removeEventListener("canplay", notifyIfBuffered);
      video.removeEventListener("progress", notifyIfBuffered);
    };
  }

  private detachCacheProbe(entry: PooledVideoEntry) {
    if (!entry.cleanupCacheProbe) return;
    entry.cleanupCacheProbe();
    entry.cleanupCacheProbe = null;
  }

  private bindSourceIfChanged(
    entry: PooledVideoEntry,
    item: ShortsPoolMediaItem,
    active: boolean
  ) {
    const { video } = entry;
    if (video.dataset.shortsVideoId !== item.id) {
      try {
        video.pause();
      } catch {
        // ignore
      }
      video.dataset.shortsVideoId = item.id;
      video.poster = item.poster;
      entry.videoId = item.id;
      this.sourceAdapter.attach(video, item.videoSrc, item.playbackUrl, { active });
    } else if (video.getAttribute("poster") !== item.poster) {
      video.poster = item.poster;
    }
  }

  private syncIOSActive(
    item: ShortsPoolMediaItem,
    activeIndex: number,
    slot: HTMLElement,
    muted: boolean
  ): HTMLVideoElement {
    const preloaded = this.iosStandbyEntry;
    if (
      preloaded &&
      preloaded.index === activeIndex &&
      preloaded.video.dataset.shortsVideoId === item.id
    ) {
      const demoted = this.activeEntry;
      this.activeEntry = preloaded;
      this.iosStandbyEntry = demoted;
      if (demoted) {
        try {
          demoted.video.pause();
        } catch {
          // ignore
        }
        this.applyRole(demoted, "standby", "metadata", true);
      }
    }

    if (!this.activeEntry) {
      this.activeEntry = this.allocateEntry();
    }
    const active = this.activeEntry;
    active.index = activeIndex;
    this.applyRole(active, "active", "auto", muted);

    if (!this.standbyPreloadDisabled && !this.iosStandbyEntry) {
      const standby = this.allocateEntry();
      this.iosStandbyEntry = standby;
      this.applyRole(standby, "standby", "metadata", true);
    }

    if (active.video.parentElement !== slot) {
      slot.appendChild(active.video);
    }
    this.bindSourceIfChanged(active, item, true);
    return active.video;
  }

  private syncIOSStandby(input: ShortsPoolStandbySyncInput) {
    if (this.standbyPreloadDisabled) return;
    const nextIndex = input.activeIndex + input.browseDirection;
    const nextItem = nextIndex >= 0 ? input.items[nextIndex] : undefined;
    const nextSlot = this.slots.get(nextIndex);
    if (!nextItem || !nextSlot) return;

    if (!this.iosStandbyEntry) {
      this.iosStandbyEntry = this.allocateEntry();
    }
    const standby = this.iosStandbyEntry;
    if (standby === this.activeEntry) return;

    const preload = input.activeReadyForPreload ? "auto" : "metadata";
    this.applyRole(standby, "standby", preload, true);
    if (standby.video.parentElement !== nextSlot) {
      nextSlot.appendChild(standby.video);
    }
    standby.index = nextIndex;

    if (standby.video.dataset.shortsVideoId === nextItem.id) return;
    this.bindSourceIfChanged(standby, nextItem, false);
    warmStandbyFirstFrame(standby.video, () => this.iosStandbyEntry === standby);
  }

  private syncStandardActive(
    item: ShortsPoolMediaItem,
    activeIndex: number,
    slot: HTMLElement,
    muted: boolean,
    volume?: number
  ): HTMLVideoElement {
    let target = this.entries.find(
      (entry) =>
        entry.index === activeIndex &&
        entry.video.dataset.shortsVideoId === item.id &&
        entry.role !== "idle"
    );

    if (!target) {
      target =
        this.entries.find((entry) => entry.role === "idle") ??
        (this.entries.length < VIDEO_WINDOW_SIZE + PRELOAD_AHEAD_COUNT
          ? this.allocateEntry()
          : this.pickEvictableEntry(activeIndex, new Set([activeIndex])));
    }

    const previousActive = this.activeEntry;
    if (previousActive && previousActive !== target) {
      try {
        if (!previousActive.video.paused) previousActive.video.pause();
      } catch {
        // ignore
      }
      this.applyRole(previousActive, "standby", "metadata", true, volume);
      if (previousActive.videoId) {
        this.attachCacheProbe(previousActive, previousActive.videoId);
      }
    }

    this.activeEntry = target;
    target.index = activeIndex;
    this.applyRole(target, "active", "auto", muted, volume);
    if (target.video.parentElement !== slot) {
      slot.appendChild(target.video);
    }
    this.bindSourceIfChanged(target, item, true);
    this.pauseNonActive();
    return target.video;
  }

  private syncStandardStandby(input: ShortsPoolStandbySyncInput) {
    const desired = new Map<number, { item: ShortsPoolMediaItem; eager: boolean }>();
    const minCandidate = Math.max(0, Math.min(input.cacheWindow.start, input.activeIndex - 1));
    const maxCandidate = Math.min(
      input.items.length - 1,
      Math.max(input.cacheWindow.end, input.activeIndex + PRELOAD_AHEAD_COUNT)
    );

    for (let index = minCandidate; index <= maxCandidate; index += 1) {
      if (index === input.activeIndex) continue;
      const item = input.items[index];
      if (!item) continue;
      const plan = planSlideMedia({
        offset: index - input.activeIndex,
        inCacheWindow: index >= input.cacheWindow.start && index <= input.cacheWindow.end,
        activeReadyForPreload: input.activeReadyForPreload,
        cached: input.cachedIds.has(item.id),
        hasPlaybackUrl: Boolean(item.playbackUrl),
        usesSharedVideo: false,
      });
      if (!plan.shouldLoad) continue;
      if (!this.slots.has(index)) continue;
      desired.set(index, { item, eager: plan.shouldEagerLoad });
    }

    // Release any non-active pooled video that is no longer inside the desired
    // preload/cache set so the browser aborts background cloud streams promptly.
    for (const entry of this.entries) {
      if (entry === this.activeEntry || entry.role === "idle") continue;
      const want = desired.get(entry.index);
      if (!want || want.item.id !== entry.videoId) {
        this.releaseEntry(entry);
      }
    }

    // Prioritize upcoming slides (+1, +2) before older cached slides.
    const orderedIndices = [...desired.keys()].sort((a, b) => {
      const aAhead = a > input.activeIndex ? 0 : 1;
      const bAhead = b > input.activeIndex ? 0 : 1;
      if (aAhead !== bAhead) return aAhead - bAhead;
      return Math.abs(a - input.activeIndex) - Math.abs(b - input.activeIndex);
    });

    const keepIndices = new Set<number>([input.activeIndex, ...orderedIndices]);
    const poolLimit = VIDEO_WINDOW_SIZE + PRELOAD_AHEAD_COUNT;

    for (const index of orderedIndices) {
      const spec = desired.get(index);
      const slot = this.slots.get(index);
      if (!spec || !slot) continue;

      let entry = this.entries.find(
        (candidate) =>
          candidate !== this.activeEntry &&
          candidate.role !== "idle" &&
          candidate.index === index &&
          candidate.videoId === spec.item.id
      );

      const preload = spec.eager ? "auto" : "metadata";
      if (entry) {
        this.applyRole(entry, "standby", preload, true, input.volume);
        if (entry.video.parentElement !== slot) {
          slot.appendChild(entry.video);
        }
        continue;
      }

      entry =
        this.entries.find((candidate) => candidate.role === "idle") ??
        (this.entries.length < poolLimit
          ? this.allocateEntry()
          : this.pickEvictableEntry(input.activeIndex, keepIndices));
      if (!entry || entry === this.activeEntry) continue;

      entry.index = index;
      this.applyRole(entry, "standby", preload, true, input.volume);
      if (entry.video.parentElement !== slot) {
        slot.appendChild(entry.video);
      }
      this.bindSourceIfChanged(entry, spec.item, false);
      this.attachCacheProbe(entry, spec.item.id);
      const targetEntry = entry;
      warmStandbyFirstFrame(
        targetEntry.video,
        () => targetEntry !== this.activeEntry && targetEntry.role === "standby"
      );
    }
  }

  private pickEvictableEntry(
    activeIndex: number,
    protectedIndices: ReadonlySet<number>
  ): PooledVideoEntry {
    let victim: PooledVideoEntry | null = null;
    let bestScore = -1;
    for (const entry of this.entries) {
      if (entry === this.activeEntry) continue;
      const unprotectedPenalty = protectedIndices.has(entry.index) ? 0 : 10_000;
      const distance = entry.index < 0 ? 5_000 : Math.abs(entry.index - activeIndex);
      const score = unprotectedPenalty + distance;
      if (!victim || score > bestScore) {
        victim = entry;
        bestScore = score;
      }
    }
    if (!victim) return this.allocateEntry();
    this.releaseEntry(victim);
    return victim;
  }

  private releaseEntry(entry: PooledVideoEntry) {
    this.detachCacheProbe(entry);
    releasePooledVideoSource(entry.video, this.sourceAdapter.dispose);
    delete entry.video.dataset.shortsVideoId;
    normalizeVideoPlaybackRate(entry.video);
    const parking = this.getParkingContainer();
    if (parking) {
      if (entry.video.parentElement !== parking) parking.appendChild(entry.video);
    } else {
      entry.video.remove();
    }
    entry.index = -1;
    entry.videoId = "";
    entry.role = "idle";
  }
}
