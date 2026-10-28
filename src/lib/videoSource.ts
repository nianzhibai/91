import type Hls from "hls.js";
import { diagnosePlaybackFailure } from "./playbackError";

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
  loading: boolean;
  processing: boolean;
  error: PlaybackSourceFailure | null;
  notice: string | null;
};
export const PLAYBACK_MODES: readonly { value: PlaybackMode; label: string }[] = [
  { value: "auto", label: "智能选择" },
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
  selectMode(mode: PlaybackMode): void;
  setOriginalDelivery(url: string | null): void;
  setActive(active: boolean): void;
  destroy(): void;
};
type RestoreState = { time: number; playing: boolean; rate: number; volume: number; muted: boolean };
const controllers = new WeakMap<HTMLVideoElement, SourceController>();
const STARTUP_TIMEOUT_MS = 10_000;

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
  return PLAYBACK_MODES.find((option) => option.value === mode)?.label ?? "智能选择";
}
export function playbackStateLabel(state: PlaybackState | null) {
  const active = state?.activeSource?.label;
  if (!state || state.mode === "auto") return active ? `智能选择 · ${active}` : "智能选择";
  return state.mode === "original" ? "原画播放" : "兼容播放";
}
export function selectPlaybackMode(video: HTMLVideoElement, mode: PlaybackMode) {
  const controller = controllers.get(video);
  if (!controller) return false;
  controller.selectMode(mode);
  return true;
}
/** Override original-file delivery without changing the selected playback mode. */
export function setOriginalVideoDelivery(video: HTMLVideoElement, url: string | null) {
  const controller = controllers.get(video);
  if (!controller) return false;
  controller.setOriginalDelivery(url);
  return true;
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
  let originalURL: string | null = null;
  let startupTimer: ReturnType<typeof setTimeout> | null = null;
  let startupComplete = false;
  const failed = new Set<string>();
  const refreshed = new Set<string>();
  const prepared = new Set<string>();
  const controller: SourceController = {
    key, url: src,
    state: { mode: options.mode ?? "auto", sources: [], activeSource: null, loading: false, processing: false, error: null, notice: null },
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
        void start(controller.state.activeSource, false);
      }
      watchStartup();
    },
    retry() {
      const sourceID = controller.state.error ? controller.state.error.sourceID : controller.state.activeSource?.id;
      const source = controller.state.sources.find((item) => item.id === sourceID);
      if (initialized && source && (controller.state.mode !== "auto" || !nativeUnsupported(source))) {
        failed.delete(source.id);
        refreshed.delete(source.id);
        void start(source, true);
        return;
      }
      failed.clear();
      refreshed.clear();
      if (!initialized || playbackUrl) void initialize(true);
      else void choose(true);
    },
    selectMode(mode) {
      if (controller.state.mode === mode) return;
      controller.state.mode = mode;
      controller.state.notice = null;
      failed.clear();
      refreshed.clear();
      changed();
      if (initialized) void choose(false);
    },
    setOriginalDelivery(url) {
      if (originalURL === url) return;
      originalURL = url;
      if (initialized && !controller.state.loading && !controller.state.error &&
          controller.state.activeSource?.kind === "original" && controller.state.activeSource.protocol === "file") {
        const source = controller.state.sources.find((item) => item.kind === "original");
        if (source) void start(source, false);
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
      video.removeEventListener("playing", watchStartup);
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
  video.addEventListener("playing", watchStartup);
  video.addEventListener("timeupdate", checkFrames);
  video.addEventListener("play", rememberPlaying);
  video.addEventListener("pause", rememberPause);
  video.addEventListener("volumechange", rememberVolume);
  document.addEventListener("visibilitychange", watchStartup);

  function changed() {
    if (!disposed) video.dispatchEvent(new CustomEvent(PLAYBACK_SOURCE_CHANGE));
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
    if (disposed || controller.state.loading || !restore) return;
    const state = restore;
    restore = null;
    video.playbackRate = state.rate;
    video.volume = state.volume;
    video.muted = state.muted;
    if (state.time > 0 && Number.isFinite(video.duration)) {
      try { video.currentTime = Math.min(state.time, Math.max(0, video.duration - 0.1)); }
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
    void recover(code === 3 || code === 4 ? "unsupported" : "network", "视频源加载失败", true);
  }
  async function recover(kind: "unsupported" | "network" | "throttled" | "timeout", message: string, diagnose = false) {
    if (disposed || controller.state.loading || controller.state.error) return;
    const source = controller.state.activeSource;
    if (!source) return;
    rememberState();
    clearStartupTimer();
    controller.state.loading = true;
    changed();
    const epoch = generation;
    if (diagnose) {
      const diagnostic = await diagnosePlaybackFailure(controller.url, { fetch: options.fetch, signal: abort?.signal });
      if (disposed || epoch !== generation) return;
      if (diagnostic) {
        message = diagnostic.message;
        if (diagnostic.kind === "access" || diagnostic.kind === "throttled") {
          fail(message, source, diagnostic.kind);
          return;
        }
        kind = "network";
      }
    }
    if (kind === "throttled") { fail("视频源当前正在限流，请稍后重试", source, kind); return; }
    if (kind === "network" && !refreshed.has(source.id)) {
      refreshed.add(source.id);
      await start(source, true);
      return;
    }
    failed.add(source.id);
    if (controller.state.mode === "auto") {
      const next = candidate();
      if (next) {
        await start(next, false);
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
    return controller.state.sources.find((source) =>
      !failed.has(source.id) && (["ready", "unknown", "processing"].includes(source.status) || (refresh && !!source.resolveUrl)) &&
      (mode === "original" ? source.kind === "original" : mode === "compatible" ? source.kind === "transcoded" : !nativeUnsupported(source)),
    );
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
    await start(source, refresh);
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
        const reason = response.status === 429 || data?.code === "drive_rate_limited" ? "throttled"
          : [401, 403, 404, 410].includes(response.status) ||
              ["drive_auth_failed", "drive_source_not_found"].includes(data?.code ?? "") ? "access" : "network";
        throw new SourceRequestError(data?.message || messages[response.status] || "获取播放源失败，请稍后重试", reason);
      }
      if (!data || !Array.isArray(data.sources) || !data.sources.length || data.sources.some((item) =>
        !item || typeof item.id !== "string" || !item.id || typeof item.label !== "string" ||
        !["original", "transcoded"].includes(item.kind) || !["file", "hls"].includes(item.protocol) ||
        !["unknown", "ready", "processing", "unavailable"].includes(item.status) ||
        (item.status === "ready" && (typeof item.url !== "string" || !item.url)) ||
        (item.resolveUrl !== undefined && typeof item.resolveUrl !== "string") ||
        (item.prepareUrl !== undefined && typeof item.prepareUrl !== "string"),
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
  async function start(selected: PlaybackSource, refresh: boolean) {
    const epoch = begin();
    const previous = controller.state.activeSource;
    try {
      const expired = !!selected.expiresAt && Date.parse(selected.expiresAt) <= Date.now();
      const resolver = selected.resolveUrl || (refresh ? playbackUrl : undefined);
      if (resolver && (refresh || expired || selected.status === "unknown" || selected.status === "processing")) {
        if (!await requestManifest(resolver, epoch, refresh || expired, selected.kind === "original" ? "original" : undefined)) return;
        selected = controller.state.sources.find((item) => item.id === selected.id) ??
          controller.state.sources.find((item) => item.kind === selected.kind && item.status === "ready" && !failed.has(item.id)) ?? selected;
      }
      if (selected.status === "processing") {
        const ready = await waitForProcessing(selected, epoch);
        if (!ready) return;
        selected = ready;
      }
      if (selected.kind === "original" && originalURL) selected = { ...selected, url: originalURL };
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
            const status = data.response?.code;
            void recover(status === 429 ? "throttled" : data.type === "mediaError" ? "unsupported" : "network", "兼容播放失败");
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
      failed.add(selected.id);
      const reason = failureReason(error);
      if (!stopsRecovery(reason) && controller.state.mode === "auto") {
        const next = candidate();
        if (next) { await start(next, false); return; }
      }
      fail(errorMessage(error), selected, reason);
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
