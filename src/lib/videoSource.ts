import type Hls from "hls.js";
import {
  diagnosePlaybackFailure,
  isSameOriginPlaybackURL,
  playbackFailureKind,
  type PlaybackFailure,
} from "./playbackError";

export type PlaybackMode = "auto" | "original" | "compatible";
export type PlaybackSource = {
  id: string;
  label: string;
  kind: "original" | "transcoded";
  protocol: "file" | "hls";
  status: "unknown" | "ready" | "processing" | "unavailable";
  url?: string;
  resolveUrl?: string;
  prepareUrl?: string;
  mediaType?: string;
  message?: string;
  expiresAt?: string;
  resolution?: string;
  width?: number;
  height?: number;
  bitrate?: number;
  codec?: string;
  urlSource?: string;
  sourceInfoUrl?: string;
};
type PlaybackFailureReason = "unsupported" | "network" | "timeout" | "processing" | "unavailable" | "access" | "throttled" | "other";
export type PlaybackSourceFailure = {
  sourceID: string | null;
  reason: PlaybackFailureReason;
  message: string;
  recoveryModes: readonly Exclude<PlaybackMode, "auto">[];
};
export type PlaybackState = {
  mode: PlaybackMode;
  sources: readonly PlaybackSource[];
  activeSource: PlaybackSource | null;
  selectedSourceID: string | null;
  loading: boolean;
  processing: boolean;
  error: PlaybackSourceFailure | null;
  notice: string | null;
};
export const PLAYBACK_MODES: readonly { value: Exclude<PlaybackMode, "auto">; label: string }[] = [
  { value: "original", label: "原画播放" },
  { value: "compatible", label: "兼容播放" },
];
export const PLAYBACK_SOURCE_ERROR = "playback-source-error";
export const PLAYBACK_SOURCE_READY = "playback-source-ready";
export const PLAYBACK_SOURCE_CHANGE = "playback-source-change";

type SourceOptions = {
  active?: boolean;
  refresh?: boolean;
  mode?: PlaybackMode;
  loadNative?: (url: string) => void;
  fetch?: typeof fetch;
  loadHls?: () => Promise<typeof Hls>;
};
type SourceController = {
  key: string;
  url: string;
  state: PlaybackState;
  retry(): void;
  reload(): void;
  reportTimeout(): void;
  selectMode(mode: PlaybackMode): void;
  selectSource(id: string): void;
  setRelay(enabled: boolean): void;
  setActive(active: boolean): void;
  destroy(): void;
};
type RestoreState = { time: number; playing: boolean; rate: number; volume: number; muted: boolean };
const controllers = new WeakMap<HTMLVideoElement, SourceController>();
const STARTUP_TIMEOUT_MS = 10_000;
export const TRIPLE_SCREEN_RELAY_QUERY = "tripleScreenRelay";

export function playbackSourceState(video: HTMLVideoElement) {
  const state = controllers.get(video)?.state;
  return state ? { ...state } : null;
}
export function playbackSourceError(video: HTMLVideoElement) {
  return playbackSourceState(video)?.error?.message ?? null;
}
export function videoSourceLoading(video: HTMLVideoElement) {
  return playbackSourceState(video)?.loading ?? false;
}
/** Only the visible/current video may prepare cloud media or poll its status. */
export function setVideoSourceActive(video: HTMLVideoElement, active: boolean) {
  controllers.get(video)?.setActive(active);
}
export function playbackSourceURL(video: HTMLVideoElement) {
  return controllers.get(video)?.url ?? video.currentSrc;
}
export function playbackModeLabel(mode: PlaybackMode) {
  return PLAYBACK_MODES.find((option) => option.value === mode)?.label ?? "正在选择";
}
export function originalPlaybackLabel(urlSource?: string, compact = false) {
  const suffix = urlSource === "quark-raw" ? "RAW" : urlSource === "quark-download" ? "下载" : null;
  if (!suffix) return "原画播放";
  return compact ? `原画·${suffix}` : `原画播放（${suffix}接口）`;
}
export function playbackStateLabel(state: PlaybackState | null) {
  const source = state?.activeSource;
  if (!source) return "正在选择";
  return source.kind === "original" ? originalPlaybackLabel(source.urlSource, true) : playbackModeLabel("compatible");
}
export function selectPlaybackMode(video: HTMLVideoElement, mode: PlaybackMode) {
  const controller = controllers.get(video);
  if (!controller) return false;
  controller.selectMode(mode);
  return true;
}
/** Select one concrete rendition without changing the shared media controller. */
export function selectPlaybackSource(video: HTMLVideoElement, id: string) {
  const controller = controllers.get(video);
  if (!controller || !controller.state.sources.some((source) => source.id === id)) return false;
  controller.selectSource(id);
  return true;
}
/** Keep the relay preference across file refreshes and rendition changes. */
export function setVideoSourceRelay(video: HTMLVideoElement, enabled: boolean) {
  const controller = controllers.get(video);
  if (!controller) return false;
  controller.setRelay(enabled);
  return true;
}

export function videoSourceRelayURL(src: string) {
  if (typeof window === "undefined") return null;
  let url: URL;
  try { url = new URL(src, window.location.href); }
  catch { return null; }
  if (url.origin !== window.location.origin) return null;
  const path = url.pathname;
  if (!(path.startsWith("/p/stream/") || path.startsWith("/p/playback/") ||
      (path.startsWith("/p/share/") && (path.endsWith("/stream") || /\/playback\/[^/]+$/.test(path))))) return null;
  url.searchParams.set(TRIPLE_SCREEN_RELAY_QUERY, "1");
  return `${url.pathname}${url.search}${url.hash}`;
}
export function disposeVideoSource(video: HTMLVideoElement) {
  controllers.get(video)?.destroy();
  controllers.delete(video);
}
export function retryVideoSource(video: HTMLVideoElement) {
  const controller = controllers.get(video);
  if (!controller) return false;
  controller.retry();
  return true;
}
/** Rebuild the current media pipeline without reacquiring URLs or resetting recovery budgets. */
export function reloadVideoSource(video: HTMLVideoElement) {
  const controller = controllers.get(video);
  if (!controller) return false;
  controller.reload();
  return true;
}
/** Let frame observers use the same bounded fallback policy as startup detection. */
export function reportVideoSourceTimeout(video: HTMLVideoElement) {
  const controller = controllers.get(video);
  if (!controller) return false;
  controller.reportTimeout();
  return true;
}

class SourceRequestError extends Error {
  constructor(message: string, readonly reason: PlaybackFailureReason) { super(message); }
}

function stopsRecovery(reason: PlaybackFailureReason) {
  return reason === "access" || reason === "throttled";
}

/** One controller owns selection, delivery and recovery for every player UI. */
export function attachVideoSource(
  video: HTMLVideoElement,
  src: string,
  playbackUrl?: string,
  options: SourceOptions = {},
) {
  const key = `${src}\n${playbackUrl ?? ""}`;
  if (controllers.get(video)?.key === key) return;
  const replacing = controllers.has(video);
  disposeVideoSource(video);
  if (replacing) {
    video.pause();
    video.removeAttribute("src");
    video.querySelectorAll("source").forEach((source) => source.remove());
    video.load();
  }
  let disposed = false;
  let active = options.active ?? true;
  let generation = 0;
  let abort: AbortController | null = null;
  let hls: Hls | null = null;
  let restore: RestoreState | null = null;
  let frameStart: number | null = null;
  let wasPlaying = false;
  let initialized = false;
  let forceRelay = false;
  let startupTimer: ReturnType<typeof setTimeout> | null = null;
  let startupComplete = false;
  let sourceInfoSequence = 0;
  const failed = new Set<string>();
  const refreshed = new Set<string>();
  const prepared = new Set<string>();
  const controller: SourceController = {
    key, url: src,
    state: { mode: options.mode ?? "auto", sources: [], activeSource: null, selectedSourceID: null, loading: false, processing: false, error: null, notice: null },
    setActive(next) {
      if (active === next) return;
      active = next;
      if (!active) {
        wasPlaying = false;
        if (restore) restore.playing = false;
        if (controller.state.processing) {
          generation++;
          abort?.abort();
        }
      } else if (controller.state.processing && !controller.state.error && controller.state.activeSource) {
        void start(controller.state.activeSource, "select");
      }
      watchStartup();
    },
    retry() {
      const sourceID = controller.state.error ? controller.state.error.sourceID : controller.state.activeSource?.id;
      const source = controller.state.sources.find((item) => item.id === sourceID);
      if (initialized && source && (controller.state.mode !== "auto" || !nativeUnsupported(source))) {
        failed.delete(source.id);
        refreshed.delete(source.id);
        void start(source, "refresh");
        return;
      }
      failed.clear();
      refreshed.clear();
      if (!initialized || playbackUrl) void initialize(true);
      else void choose(true);
    },
    reload() {
      const source = controller.state.activeSource;
      if (controller.state.loading || controller.state.error || source?.status !== "ready") return;
      void start(source, "reload");
    },
    reportTimeout() {
      void recover("timeout", "视频画面加载超时，请重试或切换播放方式");
    },
    selectMode(mode) {
      if (controller.state.mode === mode && !controller.state.selectedSourceID) return;
      controller.state.selectedSourceID = null;
      controller.state.mode = mode;
      controller.state.notice = null;
      failed.clear();
      refreshed.clear();
      changed();
      if (initialized) void choose(false);
    },
    selectSource(id) {
      const source = controller.state.sources.find((item) => item.id === id);
      if (!source) return;
      controller.state.selectedSourceID = id;
      controller.state.mode = source.kind === "original" ? "original" : "compatible";
      failed.clear();
      refreshed.clear();
      changed();
      if (initialized) void start(source, "select");
    },
    setRelay(enabled) {
      if (forceRelay === enabled) return;
      forceRelay = enabled;
      const activeSource = controller.state.activeSource;
      if (initialized && !controller.state.loading && !controller.state.error && activeSource?.protocol === "file") {
        const source = controller.state.sources.find((item) => item.id === activeSource.id);
        if (source) void start(source, "select");
      }
    },
    destroy() {
      disposed = true;
      generation++;
      clearStartupTimer();
      abort?.abort();
      hls?.destroy();
      hls = null;
      video.removeEventListener("error", mediaFailed);
      video.removeEventListener("loadedmetadata", loaded);
      video.removeEventListener("loadeddata", watchStartup);
      video.removeEventListener("playing", playbackStarted);
      video.removeEventListener("seeked", refreshOriginalSourceInfo);
      video.removeEventListener("timeupdate", checkFrames);
      video.removeEventListener("play", rememberPlaying);
      video.removeEventListener("pause", rememberPause);
      video.removeEventListener("volumechange", rememberVolume);
      document.removeEventListener("visibilitychange", watchStartup);
    },
  };
  controllers.set(video, controller);
  video.setAttribute("referrerpolicy", "no-referrer");
  video.addEventListener("error", mediaFailed);
  video.addEventListener("loadedmetadata", loaded);
  video.addEventListener("loadeddata", watchStartup);
  video.addEventListener("playing", playbackStarted);
  video.addEventListener("seeked", refreshOriginalSourceInfo);
  video.addEventListener("timeupdate", checkFrames);
  video.addEventListener("play", rememberPlaying);
  video.addEventListener("pause", rememberPause);
  video.addEventListener("volumechange", rememberVolume);
  document.addEventListener("visibilitychange", watchStartup);

  function changed() {
    if (!disposed) video.dispatchEvent(new CustomEvent(PLAYBACK_SOURCE_CHANGE));
  }
  function playbackStarted() {
    watchStartup();
    refreshOriginalSourceInfo();
  }
  function refreshOriginalSourceInfo() {
    const source = controller.state.activeSource;
    if (disposed || controller.state.loading || source?.kind !== "original" || !source.sourceInfoUrl) return;
    const epoch = generation;
    const sequence = ++sourceInfoSequence;
    let infoURL: URL;
    try { infoURL = new URL(source.sourceInfoUrl, window.location.href); }
    catch { return; }
    if (infoURL.origin !== window.location.origin) return;
    void (async () => {
      const response = await (options.fetch ?? window.fetch.bind(window))(infoURL.href, {
        credentials: "same-origin", cache: "no-store", signal: abort?.signal,
      });
      if (!response.ok) return;
      const data = await response.json() as { sources?: PlaybackSource[] };
      const urlSource = data.sources?.find((item) => item.id === "original")?.urlSource;
      const activeSource = controller.state.activeSource;
      if (disposed || generation !== epoch || sourceInfoSequence !== sequence || !activeSource ||
          activeSource.sourceInfoUrl !== source.sourceInfoUrl ||
          (urlSource !== "quark-raw" && urlSource !== "quark-download")) return;
      if (activeSource.urlSource === urlSource) return;
      controller.state.activeSource = { ...activeSource, urlSource };
      controller.state.sources = controller.state.sources.map((item) => item.id === "original" ? { ...item, urlSource } : item);
      changed();
    })().catch(() => { /* Optional diagnostics must never interrupt playback. */ });
  }
  function rememberPlaying() {
    wasPlaying = true;
    if (restore) restore.playing = true;
    if (hls) hls.config.maxBufferLength = 15;
    watchStartup();
  }
  function rememberPause() {
    if (!controller.state.loading && !video.error && !controller.state.error) {
      wasPlaying = false;
      if (restore) restore.playing = false;
    }
    watchStartup();
  }
  function rememberVolume() {
    if (restore) {
      restore.volume = video.volume;
      restore.muted = video.muted;
    }
  }
  function rememberState() {
    restore ??= {
      time: initialized ? video.currentTime || 0 : 0,
      playing: wasPlaying || !video.paused,
      rate: video.playbackRate || 1,
      volume: video.volume, muted: video.muted,
    };
  }
  function clearStartupTimer() {
    if (startupTimer !== null) clearTimeout(startupTimer);
    startupTimer = null;
  }
  function startupPending() {
    if (disposed || controller.state.loading || controller.state.error || !controller.state.activeSource) return false;
    if (video.readyState >= 2 && video.videoWidth > 0 && video.videoHeight > 0) startupComplete = true;
    const playing = wasPlaying || !!restore?.playing || !video.paused;
    // Metadata preloading may stop before the first frame. Only wait for that
    // frame after a play request, and never rotate sources for later buffering.
    return active && !document.hidden && !startupComplete &&
      (video.readyState < 1 || playing) && (video.preload !== "none" || playing);
  }
  function watchStartup() {
    if (!startupPending()) { clearStartupTimer(); return; }
    if (startupTimer !== null) return;
    const epoch = generation;
    startupTimer = setTimeout(() => {
      startupTimer = null;
      if (epoch !== generation || !startupPending()) return;
      const label = controller.state.activeSource!.label;
      // A responsive download can still scan fragments indefinitely. Refreshing
      // its signed URL would repeat the same scan, so try the next source once.
      void recover("timeout", `${label}加载超时，请重试或切换播放方式`);
    }, STARTUP_TIMEOUT_MS);
  }
  function resetMedia() {
    hls?.destroy();
    hls = null;
    video.pause();
    if (controller.state.activeSource) {
      video.removeAttribute("src");
      video.querySelectorAll("source").forEach((source) => source.remove());
      video.load();
    }
  }
  function begin() {
    rememberState();
    const epoch = ++generation;
    clearStartupTimer();
    startupComplete = false;
    controller.state.loading = true;
    controller.state.processing = false;
    controller.state.notice = null;
    controller.state.error = null;
    frameStart = null;
    abort?.abort();
    abort = new AbortController();
    resetMedia();
    changed();
    return epoch;
  }
  function fail(message: string, source: PlaybackSource | null, reason: PlaybackFailureReason) {
    if (disposed) return;
    clearStartupTimer();
    const recoveryModes: Exclude<PlaybackMode, "auto">[] = [];
    if (!stopsRecovery(reason)) {
      for (const alternative of controller.state.sources) {
        if (alternative.id === source?.id || failed.has(alternative.id) ||
            !["ready", "unknown", "processing"].includes(alternative.status) || nativeUnsupported(alternative)) continue;
        const mode = alternative.kind === "original" ? "original" : "compatible";
        if (!recoveryModes.includes(mode)) recoveryModes.push(mode);
      }
    }
    const original = controller.state.sources.find((item) => item.kind === "original");
    if (reason === "unsupported" && source?.kind === "original" &&
        !controller.state.sources.some((item) => item.kind === "transcoded")) {
      message = "浏览器无法播放当前视频";
    }
    if (controller.state.mode === "auto" && source?.kind === "transcoded" &&
        !stopsRecovery(reason) && original && (failed.has(original.id) || nativeUnsupported(original))) {
      message = `原画播放暂不可用，${message}`;
    }
    if (controller.state.mode === "auto" && source?.kind === "original" && !stopsRecovery(reason)) {
      const cloud = controller.state.sources.find((item) => item.kind === "transcoded" &&
        (["processing", "unavailable"].includes(item.status) || failed.has(item.id)));
      if (cloud) message = `${message}；${cloud.message || "兼容播放暂不可用"}`;
    }
    controller.state.loading = false;
    controller.state.processing = false;
    controller.state.error = { sourceID: source?.id ?? null, reason, message, recoveryModes };
    controller.state.notice = null;
    resetMedia();
    changed();
    video.dispatchEvent(new CustomEvent(PLAYBACK_SOURCE_ERROR));
  }
  function loaded() {
    watchStartup();
    refreshOriginalSourceInfo();
    if (disposed || controller.state.loading || !restore) return;
    const state = restore;
    restore = null;
    video.playbackRate = state.rate;
    video.volume = state.volume;
    video.muted = state.muted;
    if (state.time > 0 && Number.isFinite(video.duration)) {
      try {
        // Renditions can be slightly shorter than the original. Restoring a
        // saved position at their end can leave HLS waiting without new frames.
        video.currentTime = !Number.isFinite(state.time) || state.time >= video.duration - 0.1
          ? 0 : Math.min(state.time, Math.max(0, video.duration - 0.1));
      }
      catch { /* Some media becomes seekable only after metadata. */ }
    }
    if (state.playing || wasPlaying) void video.play().catch(() => undefined);
  }
  function checkFrames() {
    watchStartup();
    if (disposed || controller.state.loading || controller.state.error || video.paused) return;
    if (video.videoWidth > 0 && video.videoHeight > 0) { frameStart = null; return; }
    // Media time advancing without video dimensions detects audio-only decode
    // failures; wall-clock buffering must not trigger a source switch.
    if (video.readyState < 3) return;
    frameStart ??= video.currentTime;
    if (video.currentTime - frameStart >= 3)
      void recover("unsupported", "当前浏览器未能解码视频画面");
  }
  function mediaFailed() {
    if (disposed || controller.state.loading || controller.state.error) return;
    const code = video.error?.code;
    if (code === 1) return; // User/navigation aborts are not source failures.
    const source = controller.state.activeSource;
    // SRC_NOT_SUPPORTED can hide an HTTP failure, including native HLS segment
    // failures. Refresh any potentially playable source once; only confirmed
    // decode/container failures skip directly to another source.
    const unsupported = code === 3 || (code === 4 && !!source && nativeUnsupported(source));
    void recover(unsupported ? "unsupported" : "network", "视频源加载失败", controller.url);
  }
  async function recover(kind: "unsupported" | "timeout" | PlaybackFailure["kind"], message: string, diagnosticURL?: string) {
    if (disposed || controller.state.loading || controller.state.error) return;
    const source = controller.state.activeSource;
    if (!source) return;
    rememberState();
    clearStartupTimer();
    controller.state.loading = true;
    changed();
    const epoch = generation;
    if (diagnosticURL) {
      const diagnostic = await diagnosePlaybackFailure(diagnosticURL, { fetch: options.fetch, signal: abort?.signal });
      if (disposed || epoch !== generation) return;
      if (diagnostic) {
        message = diagnostic.message;
        kind = diagnostic.kind;
      }
    }
    if (stopsRecovery(kind)) { fail(message, source, kind); return; }
    if (kind === "network" && !refreshed.has(source.id)) {
      refreshed.add(source.id);
      await start(source, "refresh");
      return;
    }
    await rejectSource(source, kind, message);
  }
  async function rejectSource(source: PlaybackSource, kind: PlaybackFailureReason, message: string) {
    failed.add(source.id);
    if (!stopsRecovery(kind) && (controller.state.mode === "auto" || (controller.state.mode === "compatible" && !controller.state.selectedSourceID))) {
      const next = candidate();
      if (next) {
        await start(next, "select");
        return;
      }
    }
    fail(message, source, kind);
  }
  function nativeUnsupported(source: PlaybackSource) {
    return source.protocol === "file" && !!source.mediaType && video.canPlayType(source.mediaType) === "";
  }
  function candidate(refresh = false) {
    const mode = controller.state.mode;
    const candidates = controller.state.sources.filter((source) =>
      !failed.has(source.id) && (["ready", "unknown", "processing"].includes(source.status) || (refresh && !!source.resolveUrl)) &&
      (mode === "original" ? source.kind === "original" : mode === "compatible" ? source.kind === "transcoded" : !nativeUnsupported(source)),
    );
    return candidates.find((source) => source.status !== "processing") ?? candidates[0];
  }
  async function choose(refresh: boolean) {
    const source = candidate(refresh);
    if (!source) {
      const unavailable = controller.state.sources.find((item) =>
        item.kind === (controller.state.mode === "original" ? "original" : "transcoded")) ??
        controller.state.sources.find((item) => item.kind === "original") ?? null;
      const reason = unavailable?.status === "processing" ? "processing"
        : unavailable && nativeUnsupported(unavailable) ? "unsupported" : "unavailable";
      fail(unavailable?.message || (controller.state.mode === "compatible" || unavailable?.kind === "transcoded"
        ? "该视频暂未提供兼容播放源"
        : reason === "unsupported" ? "浏览器无法播放当前视频" : "原画播放暂不可用"), unavailable, reason);
      return;
    }
    if (!refresh && source.id === controller.state.activeSource?.id && !controller.state.error && !controller.state.loading) { changed(); return; }
    await start(source, refresh ? "refresh" : "select");
  }
  async function requestManifest(path: string, epoch: number, refresh: boolean, sourceID?: string, method = "GET") {
    const url = new URL(path, window.location.href);
    if (url.origin !== window.location.origin) throw new Error("无效的播放源解析地址");
    if (refresh) url.searchParams.set("refresh", "1");
    if (sourceID) url.searchParams.set("source", sourceID);
    const requestAbort = abort!;
    const timeout = setTimeout(() => requestAbort.abort(), 25_000);
    try {
      const response = await (options.fetch ?? window.fetch.bind(window))(url.href, {
        method, credentials: "same-origin", cache: "no-store", signal: requestAbort.signal,
      });
      const data = await response.json().catch(() => null) as { sources?: PlaybackSource[]; message?: string; code?: string } | null;
      if (disposed || epoch !== generation) return false;
      if (!response.ok) {
        const messages: Record<number, string> = {
          401: "当前登录状态已失效，请重新登录", 403: "当前账号无权访问该视频",
          404: "视频文件或分享凭证不存在、已失效", 410: "视频文件或分享凭证已失效",
          429: "网盘当前正在限流，请稍后重试",
        };
        const reason = playbackFailureKind(response.status, data?.code);
        throw new SourceRequestError(data?.message || messages[response.status] || "获取播放源失败，请稍后重试", reason);
      }
      if (!data || !Array.isArray(data.sources) || !data.sources.length || data.sources.some((item) =>
        !item || typeof item.id !== "string" || !item.id || typeof item.label !== "string" ||
        !["original", "transcoded"].includes(item.kind) || !["file", "hls"].includes(item.protocol) ||
        !["unknown", "ready", "processing", "unavailable"].includes(item.status) ||
        (item.status === "ready" && (typeof item.url !== "string" || !item.url)) ||
        (item.resolveUrl !== undefined && typeof item.resolveUrl !== "string") ||
        (item.prepareUrl !== undefined && typeof item.prepareUrl !== "string") ||
        (item.sourceInfoUrl !== undefined && typeof item.sourceInfoUrl !== "string") ||
        (item.urlSource !== undefined && typeof item.urlSource !== "string"),
      ) || new Set(data.sources.map((item) => item.id)).size !== data.sources.length)
        throw new Error("播放源返回了无效数据");
      controller.state.sources = data.sources;
      changed();
      return true;
    } finally { clearTimeout(timeout); }
  }
  function waitForPoll(milliseconds: number, signal: AbortSignal) {
    return new Promise<void>((resolve, reject) => {
      const aborted = () => {
        clearTimeout(timer);
        signal.removeEventListener("abort", aborted);
        reject(new DOMException("Playback request canceled", "AbortError"));
      };
      const timer = setTimeout(() => {
        signal.removeEventListener("abort", aborted);
        resolve();
      }, milliseconds);
      signal.addEventListener("abort", aborted, { once: true });
      if (signal.aborted) aborted();
    });
  }
  async function waitForProcessing(selected: PlaybackSource, epoch: number) {
    const deadline = Date.now() + 5 * 60_000;
    while (selected.status === "processing") {
      if (disposed || epoch !== generation) return null;
      controller.state.activeSource = selected;
      controller.state.processing = true;
      controller.state.notice = selected.message || "视频正在转码，完成后将自动播放";
      changed();
      if (!active) return null;
      if (selected.prepareUrl && !prepared.has(selected.id)) {
        if (!await requestManifest(selected.prepareUrl, epoch, false, undefined, "POST")) return null;
        prepared.add(selected.id);
      } else {
        if (!selected.resolveUrl) throw new SourceRequestError("视频正在转码，请稍后重新检查", "processing");
        const remaining = deadline - Date.now();
        if (remaining <= 0) throw new SourceRequestError("视频仍在转码，请稍后点击重新获取", "processing");
        await waitForPoll(Math.min(15_000, remaining), abort!.signal);
        if (Date.now() >= deadline) throw new SourceRequestError("视频仍在转码，请稍后点击重新获取", "processing");
        if (!await requestManifest(selected.resolveUrl, epoch, false)) return null;
      }
      const updated = controller.state.sources.find((item) => item.id === selected.id);
      if (!updated) throw new SourceRequestError("兼容播放源暂不可用", "unavailable");
      selected = updated;
      if (selected.status === "processing" && !controller.state.selectedSourceID) {
        const alternative = controller.state.sources.find((item) =>
          item.kind === "transcoded" && item.id !== selected.id &&
          (item.status === "ready" || item.status === "unknown") && !failed.has(item.id));
        if (alternative) {
          if (alternative.status === "ready") return alternative;
          await start(alternative, "select");
          return null;
        }
      }
    }
    controller.state.processing = false;
    return selected;
  }
  async function initialize(refresh: boolean) {
    const epoch = begin();
    try {
      if (playbackUrl) {
        if (!await requestManifest(playbackUrl, epoch, refresh, refresh ? "original" : undefined)) return;
      } else {
        controller.state.sources = [{ id: "original", label: "原画播放", kind: "original", protocol: /\.m3u8(?:[?#]|$)/i.test(src) ? "hls" : "file", status: "ready", url: src }];
      }
      initialized = true;
      await choose(false);
    } catch (error) {
      if (!disposed && epoch === generation) fail(errorMessage(error), null, failureReason(error));
    }
  }
  async function start(selected: PlaybackSource, action: "select" | "refresh" | "reload") {
    const epoch = begin();
    const previous = controller.state.activeSource;
    try {
      const refresh = action === "refresh";
      const expired = !!selected.expiresAt && Date.parse(selected.expiresAt) <= Date.now();
      const resolver = selected.resolveUrl || (refresh ? playbackUrl : undefined);
      if (action !== "reload" && resolver && (refresh || expired || selected.status === "unknown" || selected.status === "processing")) {
        if (!await requestManifest(resolver, epoch, refresh || expired, selected.kind === "original" ? "original" : undefined)) return;
        const resolved = controller.state.sources.find((item) => item.id === selected.id);
        const unavailable = controller.state.sources.find((item) => item.kind === selected.kind && item.status === "unavailable");
        if (resolved) selected = resolved;
        else {
          const missing: PlaybackSource = { ...selected, status: "unavailable", url: undefined,
            message: unavailable?.message || "当前播放源已不可用，请重新获取或切换清晰度" };
          if (selected.id !== "cloud" && controller.state.selectedSourceID === selected.id) {
            // Keep a manual choice retryable without silently replacing it.
            selected = missing;
            controller.state.sources = [...controller.state.sources, missing];
          } else {
            selected = controller.state.sources.find((item) => item.kind === selected.kind && item.status === "ready" && !failed.has(item.id)) ??
              controller.state.sources.find((item) => item.kind === selected.kind && item.status === "processing" && !failed.has(item.id)) ??
              unavailable ?? missing;
          }
        }
      }
      if (selected.status === "processing") {
        const ready = await waitForProcessing(selected, epoch);
        if (!ready) return;
        selected = ready;
      }
      if (forceRelay && selected.protocol === "file" && selected.url) {
        const relayURL = videoSourceRelayURL(selected.url);
        if (relayURL) selected = { ...selected, url: relayURL };
      }
      if (selected.status !== "ready" || !selected.url) {
        throw new SourceRequestError(selected.message || "该播放源暂不可用，请重新获取",
          selected.status === "processing" ? "processing" : "unavailable");
      }
      controller.state.activeSource = selected;
      controller.url = selected.url;
      if (selected.protocol === "file") {
        if (options.loadNative) options.loadNative(selected.url);
        else { video.src = selected.url; video.load(); }
      } else {
        const HlsCtor = await (options.loadHls?.() ?? import("hls.js/light").then((module) => module.default));
        if (disposed || epoch !== generation) return;
        if (HlsCtor.isSupported()) {
          hls = new HlsCtor({
            enableWorker: true, backBufferLength: 30,
            maxBufferLength: video.preload === "metadata" ? 2 : 15,
            maxBufferSize: 4 * 1024 * 1024,
            manifestLoadingMaxRetry: 1, fragLoadingMaxRetry: 2,
          });
          hls.on(HlsCtor.Events.ERROR, (_event, data) => {
            if (!data.fatal || disposed || epoch !== generation) return;
            const status = data.response?.code ?? 0;
            const failedURL = new URL(data.response?.url || data.url || selected.url!, window.location.href);
            const local = isSameOriginPlaybackURL(failedURL, new URL(window.location.href));
            const kind = status === 429 ? "throttled" : data.type === "mediaError" ? "unsupported"
              : local ? playbackFailureKind(status) : "network";
            // Inspect the failed local resource, including segments, to distinguish
            // signed-source expiry from site access failures. CDN rejections only
            // describe that source, never the viewer's site credentials.
            void recover(kind, kind === "throttled" ? "视频源当前正在限流，请稍后重试" : "兼容播放失败",
              local && data.type !== "mediaError" && status !== 429 ? failedURL.href : undefined);
          });
          hls.loadSource(selected.url);
          hls.attachMedia(video);
        } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
          video.src = selected.url;
          video.load();
        } else throw new SourceRequestError("当前浏览器不支持兼容播放，请换用新版浏览器", "unsupported");
      }
      if (disposed || epoch !== generation) return;
      controller.state.loading = false;
      controller.state.notice = null;
      if (controller.state.mode === "auto") {
        if (selected.kind === "transcoded" && previous?.kind !== "transcoded")
          controller.state.notice = "兼容播放";
        else if (selected.kind === "original" && previous?.kind === "transcoded")
          controller.state.notice = "兼容播放暂时不可用，已切换其他播放源";
      }
      changed();
      video.dispatchEvent(new CustomEvent(PLAYBACK_SOURCE_READY));
      watchStartup();
    } catch (error) {
      if (disposed || epoch !== generation) return;
      // Resolution has already been attempted. Do not recursively refresh a
      // failing resolver; use the same source rotation/stop policy as delivery.
      await rejectSource(selected, failureReason(error), errorMessage(error));
    }
  }
  function failureReason(error: unknown): PlaybackFailureReason {
    return error instanceof SourceRequestError ? error.reason
      : error instanceof TypeError || (error instanceof Error && error.name === "AbortError") ? "network" : "other";
  }
  function errorMessage(error: unknown) {
    return error instanceof Error && error.name !== "AbortError" ? error.message : "获取播放源超时，请稍后重试";
  }
  void initialize(!!options.refresh);
}
