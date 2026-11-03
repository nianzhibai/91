import "./helpers/dom-environment.ts";
import assert from "node:assert/strict";
import test, { afterEach, type TestContext } from "node:test";
import {
  attachVideoSource,
  disposeVideoSource,
  playbackSourceError,
  playbackSourceState,
  playbackSourceURL,
  retryVideoSource,
  selectPlaybackMode,
  selectPlaybackSource,
  playbackStateLabel,
  originalPlaybackLabel,
  setVideoSourceRelay,
  setVideoSourceActive,
  type PlaybackSource,
} from "../src/lib/videoSource.ts";

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
const videos = new Set<HTMLVideoElement>();
afterEach(() => {
  for (const video of videos) disposeVideoSource(video);
  videos.clear();
  FakeHls.instances = [];
  Object.defineProperty(document, "hidden", { value: false });
});
function media() {
  const video = document.createElement("video");
  videos.add(video);
  Object.defineProperties(video, {
    paused: { configurable: true, writable: true, value: true },
    error: { configurable: true, writable: true, value: null },
    readyState: { configurable: true, writable: true, value: 4 },
    videoWidth: { configurable: true, writable: true, value: 1080 },
    videoHeight: { configurable: true, writable: true, value: 1920 },
    duration: { configurable: true, writable: true, value: 180 },
  });
  video.canPlayType = (type) => type === "video/mp4" ? "probably" : "";
  video.load = () => {
    video.currentTime = 0;
    video.playbackRate = 1;
    Object.defineProperty(video, "error", { value: null, configurable: true, writable: true });
  };
  video.pause = () => {
    Object.defineProperty(video, "paused", { value: true, configurable: true, writable: true });
    video.dispatchEvent(new Event("pause"));
  };
  video.play = async () => {
    Object.defineProperty(video, "paused", { value: false, configurable: true, writable: true });
    video.dispatchEvent(new Event("play"));
  };
  return video;
}
const original: PlaybackSource = {
  id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready",
  url: "/p/stream/wo/file", mediaType: "video/mp4",
};
const cloud: PlaybackSource = {
  id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown",
  resolveUrl: "/api/video/one/playback?source=cloud",
};
const readyCloud: PlaybackSource = { ...cloud, status: "ready", url: "https://hls.example/index.m3u8" };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), {
  status, headers: { "Content-Type": "application/json" },
});
const manifest = (...sources: PlaybackSource[]) => json({ sources });
class FakeHls {
  static instances: FakeHls[] = [];
  static isSupported() { return true; }
  static Events = { ERROR: "error" };
  handler!: (_event: string, data: { fatal: boolean; type?: string; response?: { code: number } }) => void;
  destroyed = false;
  url = "";
  constructor(public config: Record<string, unknown>) { FakeHls.instances.push(this); }
  on(_event: string, handler: FakeHls["handler"]) { this.handler = handler; }
  loadSource(url: string) { this.url = url; }
  attachMedia() {}
  destroy() { this.destroyed = true; }
  fail(type = "networkError", code?: number) { this.handler("error", { fatal: true, type, response: code ? { code } : undefined }); }
}
const loadHls = async () => FakeHls as never;

function processingPlayback(t: TestContext, active = true) {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });
  const video = media();
  const calls: { url: URL; method: string; signal?: AbortSignal | null }[] = [];
  const pending: PlaybackSource = {
    ...cloud, status: "processing", prepareUrl: "/api/video/one/playback/prepare", message: "视频正在转码",
  };
  let ready = false;
  let preparationStatus = 200;
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    active, mode: "compatible", loadHls,
    fetch: (async (path, init) => {
      const url = new URL(String(path));
      const method = init?.method ?? "GET";
      calls.push({ url, method, signal: init?.signal });
      if (method === "POST" && preparationStatus !== 200) return json({}, preparationStatus);
      return manifest(original, method === "POST" || url.searchParams.get("source") === "cloud"
        ? ready ? readyCloud : pending : cloud);
    }) as typeof fetch,
  });
  const flush = () => new Promise<void>((resolve) => setImmediate(resolve));
  return {
    video, calls, flush,
    ready() { ready = true; },
    rejectPreparation(status: number) { preparationStatus = status; },
    async advance(ms: number) { t.mock.timers.tick(ms); await flush(); },
    submissions() { return calls.filter((call) => call.method === "POST"); },
  };
}

test("pending compatibility submits once and automatically plays after a read-only poll", async (t) => {
  const env = processingPlayback(t);
  await env.flush();
  assert.equal(env.submissions().length, 1);
  assert.equal(env.submissions()[0].url.pathname, "/api/video/one/playback/prepare");
  assert.equal(playbackSourceError(env.video), null);
  assert.equal(playbackSourceState(env.video)?.processing, true);
  await env.advance(15_000);
  assert.equal(env.submissions().length, 1);
  assert.equal(env.calls.at(-1)?.method, "GET");
  assert.equal(env.calls.at(-1)?.url.searchParams.has("refresh"), false);
  env.ready();
  await env.advance(15_000);
  assert.equal(playbackSourceState(env.video)?.processing, false);
  assert.equal(playbackSourceState(env.video)?.activeSource?.status, "ready");
  assert.equal(FakeHls.instances.length, 1);
});

test("preloaded pending videos neither submit nor poll until promoted to the active video", async (t) => {
  const env = processingPlayback(t, false);
  await env.flush();
  const preloadedCalls = env.calls.length;
  await env.advance(60_000);
  assert.equal(env.calls.length, preloadedCalls);
  assert.equal(env.submissions().length, 0);
  setVideoSourceActive(env.video, true);
  await env.flush();
  assert.equal(env.submissions().length, 1);
  env.ready();
  await env.advance(15_000);
  assert.equal(FakeHls.instances.length, 1);
});

test("leaving a processing video stops polling and returning does not resubmit", async (t) => {
  const env = processingPlayback(t);
  await env.flush();
  const before = env.calls.length;
  setVideoSourceActive(env.video, false);
  await env.advance(60_000);
  assert.equal(env.calls.length, before);
  assert.equal(playbackSourceError(env.video), null);
  setVideoSourceActive(env.video, true);
  await env.flush();
  assert.equal(env.submissions().length, 1);
  env.ready();
  await env.advance(15_000);
  assert.equal(FakeHls.instances.length, 1);
});

test("switching to original playback cancels conversion polling", async (t) => {
  const env = processingPlayback(t);
  await env.flush();
  selectPlaybackMode(env.video, "original");
  await env.flush();
  const before = env.calls.length;
  await env.advance(60_000);
  assert.equal(env.calls.length, before);
  assert.equal(playbackSourceState(env.video)?.activeSource?.kind, "original");
  assert.equal(playbackSourceState(env.video)?.processing, false);
});

test("a failed original can still offer a compatible source that is processing", async (t) => {
  const env = processingPlayback(t);
  await env.flush();
  selectPlaybackMode(env.video, "original");
  await env.flush();
  mediaError(env.video);
  await env.flush();
  assert.deepEqual(playbackSourceState(env.video)?.error?.recoveryModes, ["compatible"]);
});

test("processing wait is bounded and reacquisition rechecks without another submission", async (t) => {
  const env = processingPlayback(t);
  await env.flush();
  await env.advance(5 * 60_000);
  assert.equal(playbackSourceState(env.video)?.error?.reason, "processing");
  assert.match(playbackSourceError(env.video) ?? "", /重新获取/);
  assert.equal(env.submissions().length, 1);
  env.ready();
  retryVideoSource(env.video);
  await env.flush();
  assert.equal(playbackSourceError(env.video), null);
  assert.equal(env.submissions().length, 1);
  assert.equal(FakeHls.instances.length, 1);
});

test("disposing a processing video cancels every future status request", async (t) => {
  const env = processingPlayback(t);
  await env.flush();
  disposeVideoSource(env.video);
  const before = env.calls.length;
  await env.advance(5 * 60_000);
  assert.equal(env.calls.length, before);
  assert.equal(FakeHls.instances.length, 0);
});

test("processing metadata cannot submit to a foreign preparation URL", async () => {
  const video = media();
  const methods: string[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path, init) => {
      methods.push(init?.method ?? "GET");
      return manifest(original, String(path).includes("source=cloud")
        ? { ...cloud, status: "processing", prepareUrl: "https://foreign.example/prepare" } : cloud);
    }) as typeof fetch,
  });
  await settle();
  assert.deepEqual(methods, ["GET", "GET"]);
  assert.match(playbackSourceError(video) ?? "", /无效的播放源解析地址/);
});

for (const status of [401, 403, 429]) {
  test(`preparation HTTP ${status} stops without polling or automatic retries`, async (t) => {
    const env = processingPlayback(t, false);
    await env.flush();
    env.rejectPreparation(status);
    setVideoSourceActive(env.video, true);
    await env.flush();
    assert.equal(playbackSourceState(env.video)?.error?.reason, status === 429 ? "throttled" : "access");
    const before = env.calls.length;
    await env.advance(60_000);
    assert.equal(env.calls.length, before);
    assert.equal(env.submissions().length, 1);
  });
}

function setup(options: { original?: PlaybackSource; cloud?: PlaybackSource; mode?: "auto" | "original" | "compatible"; active?: boolean } = {}, video = media()) {
  const urls: URL[] = [];
  const originalSource = options.original ?? original;
  const fetcher = (async (path: RequestInfo | URL) => {
    const url = new URL(String(path));
    urls.push(url);
    if (url.pathname.startsWith("/p/")) return new Response(null, { status: 206 });
    return manifest(originalSource, url.searchParams.get("source") === "cloud" ? options.cloud ?? readyCloud : cloud);
  }) as typeof fetch;
  attachVideoSource(video, original.url!, "/api/video/one/playback", { fetch: fetcher, loadHls, mode: options.mode, active: options.active });
  return { video, urls };
}
function mediaError(video: HTMLVideoElement, code = 4) {
  Object.defineProperty(video, "error", { value: { code }, configurable: true, writable: true });
  video.dispatchEvent(new Event("error"));
}

function startupPlayback(t: TestContext, options: Parameters<typeof setup>[0] = {}) {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });
  const video = media();
  const load = video.load;
  video.load = () => {
    load();
    Object.assign(video, { readyState: 0, videoWidth: 0, videoHeight: 0, duration: NaN });
  };
  video.preload = "metadata";
  const env = setup(options, video);
  const flush = () => new Promise<void>((resolve) => setImmediate(resolve));
  return {
    ...env, flush,
    async advance(ms: number) { t.mock.timers.tick(ms); await flush(); },
    metadata() {
      Object.assign(video, { readyState: 1, videoWidth: 1080, videoHeight: 1920, duration: 180 });
      video.dispatchEvent(new Event("loadedmetadata"));
    },
    frame() {
      Object.assign(video, { readyState: 4, videoWidth: 1080, videoHeight: 1920 });
      video.dispatchEvent(new Event("loadeddata"));
    },
  };
}

test("smart playback switches a scanning original to HLS after 10 seconds without refreshing it", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  await env.video.play();
  for (const milliseconds of [5_000, 4_999]) {
    env.video.dispatchEvent(new Event("progress"));
    await env.advance(milliseconds);
    assert.equal(env.urls.length, 1);
  }
  await env.advance(1);
  assert.equal(env.urls.length, 2);
  assert.equal(env.urls[1].searchParams.get("source"), "cloud");
  assert.equal(env.urls[1].searchParams.has("refresh"), false);
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
  assert.equal(playbackSourceState(env.video)?.mode, "auto");
  env.metadata();
  env.frame();
  assert.equal(env.video.paused, false);
  await env.advance(60_000);
  assert.equal(env.urls.length, 2);
  assert.equal(playbackSourceError(env.video), null);
});

test("metadata-only preloading waits for a play request before timing out the first frame", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  env.metadata();
  await env.advance(60_000);
  assert.equal(env.urls.length, 1);
  await env.video.play();
  await env.advance(9_999);
  assert.equal(env.urls.length, 1);
  await env.advance(1);
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
});

test("a paused first-frame request gets a fresh startup budget when resumed", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  env.metadata();
  await env.video.play();
  await env.advance(5_000);
  env.video.pause();
  await env.advance(60_000);
  assert.equal(env.urls.length, 1);
  await env.video.play();
  await env.advance(9_999);
  assert.equal(env.urls.length, 1);
  await env.advance(1);
  assert.equal(env.urls.length, 2);
});

test("playback buffering after the first frame never triggers startup recovery", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  env.metadata();
  await env.video.play();
  env.frame();
  Object.assign(env.video, { readyState: 1 });
  for (const event of ["waiting", "stalled", "timeupdate"]) env.video.dispatchEvent(new Event(event));
  await env.advance(120_000);
  assert.equal(env.urls.length, 1);
  assert.equal(playbackSourceError(env.video), null);
});

test("manual original startup timeout offers compatibility without switching or retaining the download", async (t) => {
  const env = startupPlayback(t, { mode: "original" });
  await env.flush();
  await env.advance(10_000);
  assert.equal(env.urls.length, 1);
  assert.equal(FakeHls.instances.length, 0);
  assert.equal(playbackSourceState(env.video)?.mode, "original");
  assert.equal(playbackSourceState(env.video)?.error?.reason, "timeout");
  assert.deepEqual(playbackSourceState(env.video)?.error?.recoveryModes, ["compatible"]);
  assert.match(playbackSourceError(env.video)!, /原画播放加载超时/);
  assert.equal(env.video.getAttribute("src"), null);
  selectPlaybackMode(env.video, "compatible");
  await env.flush();
  env.metadata();
  env.frame();
  await env.advance(60_000);
  assert.equal(playbackSourceError(env.video), null);
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
});

test("timing out both sources stops recovery and a manual retry only retries the failed cloud", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  await env.video.play();
  await env.advance(10_000);
  await env.advance(10_000);
  assert.equal(env.urls.length, 2);
  assert.equal(FakeHls.instances[0].destroyed, true);
  assert.equal(playbackSourceState(env.video)?.error?.sourceID, "cloud");
  assert.equal(playbackSourceState(env.video)?.error?.reason, "timeout");
  assert.deepEqual(playbackSourceState(env.video)?.error?.recoveryModes, []);
  assert.match(playbackSourceError(env.video)!, /原画播放暂不可用，兼容播放加载超时/);
  await env.advance(120_000);
  assert.equal(env.urls.length, 2);
  retryVideoSource(env.video);
  await env.flush();
  assert.equal(env.urls.length, 3);
  assert.equal(env.urls[2].searchParams.get("source"), "cloud");
  assert.equal(env.urls[2].searchParams.get("refresh"), "1");
  await env.advance(10_000);
  assert.equal(playbackSourceState(env.video)?.error?.sourceID, "cloud");
  assert.equal(env.urls.length, 3);
});

test("a missing compatible source reports failure after the original timeout without cycling", async (t) => {
  const env = startupPlayback(t, { cloud: { ...cloud, status: "unavailable", message: "网盘暂未提供兼容播放源" } });
  await env.flush();
  await env.advance(10_000);
  assert.match(playbackSourceError(env.video)!, /原画播放暂不可用，网盘暂未提供兼容播放源/);
  await env.advance(120_000);
  assert.equal(env.urls.length, 2);
  assert.equal(FakeHls.instances.length, 0);
});

test("inactive preloading starts its startup timeout only when promoted to active", async (t) => {
  const env = startupPlayback(t, { active: false });
  await env.flush();
  await env.advance(60_000);
  assert.equal(env.urls.length, 1);
  setVideoSourceActive(env.video, true);
  await env.advance(10_000);
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
});

test("leaving an active video cancels its startup timeout until it becomes active again", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  await env.advance(5_000);
  setVideoSourceActive(env.video, false);
  await env.advance(60_000);
  assert.equal(env.urls.length, 1);
  setVideoSourceActive(env.video, true);
  await env.advance(9_999);
  assert.equal(env.urls.length, 1);
  await env.advance(1);
  assert.equal(env.urls.length, 2);
});

test("hidden pages cancel startup detection and get a fresh timeout on return", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  await env.advance(5_000);
  Object.defineProperty(document, "hidden", { value: true });
  document.dispatchEvent(new Event("visibilitychange"));
  await env.advance(60_000);
  assert.equal(env.urls.length, 1);
  Object.defineProperty(document, "hidden", { value: false });
  document.dispatchEvent(new Event("visibilitychange"));
  await env.advance(10_000);
  assert.equal(env.urls.length, 2);
});

test("preload none does not run startup detection until playback is requested", async (t) => {
  const env = startupPlayback(t);
  env.video.preload = "none";
  await env.flush();
  await env.advance(60_000);
  assert.equal(env.urls.length, 1);
  await env.video.play();
  await env.advance(10_000);
  assert.equal(env.urls.length, 2);
});

test("disposing a stalled video cancels every future startup recovery", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  await env.advance(5_000);
  disposeVideoSource(env.video);
  await env.advance(120_000);
  assert.equal(env.urls.length, 1);
  assert.equal(FakeHls.instances.length, 0);
});

test("replacing a stalled source cancels its timeout without affecting the new video", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  await env.advance(5_000);
  attachVideoSource(env.video, "new", "/api/video/new/playback", {
    fetch: (async () => manifest({ ...original, url: "https://cdn.example/new.mp4" })) as typeof fetch,
  });
  await env.flush();
  env.metadata();
  env.frame();
  await env.advance(60_000);
  assert.equal(env.urls.length, 1);
  assert.equal(env.video.src, "https://cdn.example/new.mp4");
  assert.equal(playbackSourceError(env.video), null);
});

test("switching modes cancels the previous source's startup timeout", async (t) => {
  const env = startupPlayback(t);
  await env.flush();
  await env.advance(5_000);
  selectPlaybackMode(env.video, "compatible");
  await env.flush();
  env.metadata();
  env.frame();
  await env.advance(120_000);
  assert.equal(env.urls.length, 2);
  assert.equal(FakeHls.instances[0].destroyed, false);
  assert.equal(playbackSourceState(env.video)?.mode, "compatible");
});

test("automatic original playback does not resolve or load the optional HLS source", async () => {
  const { video, urls } = setup();
  await settle();
  assert.equal(urls.length, 1);
  assert.equal(video.src, "http://localhost/p/stream/wo/file");
  assert.equal(FakeHls.instances.length, 0);
  assert.equal(playbackSourceState(video)?.activeSource?.id, "original");
  video.dispatchEvent(new Event("waiting"));
  video.dispatchEvent(new Event("stalled"));
  await settle();
  assert.equal(urls.length, 1);
});

test("play intent during asynchronous discovery survives source loading", async () => {
  const video = media();
  let finish!: (response: Response) => void;
  let plays = 0;
  video.addEventListener("play", () => plays++);
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    fetch: (() => new Promise((resolve) => { finish = resolve; })) as typeof fetch,
  });
  await video.play();
  finish(manifest(original));
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(plays, 2);
});

test("unsupported original containers select HLS without fetching raw media", async () => {
  const { video, urls } = setup({ original: { ...original, mediaType: "video/mp2t" } });
  await settle();
  assert.equal(video.getAttribute("src"), null);
  assert.equal(urls.length, 2);
  assert.equal(urls[1].searchParams.get("source"), "cloud");
  assert.equal(FakeHls.instances[0].url, readyCloud.url);
});

test("audio-only decode failures switch to HLS without refreshing an incompatible original", async () => {
  const { video, urls } = setup();
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  await video.play();
  Object.defineProperties(video, { videoWidth: { value: 0 }, videoHeight: { value: 0 } });
  for (const time of [0, 4]) {
    video.currentTime = time;
    video.dispatchEvent(new Event("timeupdate"));
  }
  await settle();
  assert.equal(urls.length, 2);
  assert.equal(urls[1].searchParams.get("refresh"), null);
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  assert.equal(playbackSourceState(video)?.notice, "兼容播放");
});

test("manual original playback fails visibly without changing quality", async () => {
  const { video, urls } = setup({ mode: "original" });
  await settle();
  mediaError(video);
  await settle();
  assert.ok(playbackSourceError(video));
  assert.equal(FakeHls.instances.length, 0);
  assert.equal(urls.filter((url) => url.pathname.startsWith("/api/")).length, 1);
  assert.equal(playbackSourceState(video)?.mode, "original");
});

test("manual switches preserve progress, play intent, rate, volume and mute", async () => {
  const { video } = setup();
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  video.currentTime = 47;
  video.playbackRate = 1.5;
  video.volume = 0.35;
  video.muted = true;
  await video.play();
  selectPlaybackMode(video, "compatible");
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(video.currentTime, 47);
  assert.equal(video.playbackRate, 1.5);
  assert.equal(video.volume, 0.35);
  assert.equal(video.muted, true);
  assert.equal(video.paused, false);
  assert.equal(playbackSourceState(video)?.mode, "compatible");
  assert.equal(playbackSourceURL(video), readyCloud.url);
  video.pause();
  selectPlaybackMode(video, "original");
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(video.paused, true);
  assert.equal(video.currentTime, 47);
  assert.equal(FakeHls.instances[0].destroyed, true);
});

test("network recovery refreshes each source once and never cycles back to failed sources", async () => {
  const { video, urls } = setup();
  await settle();
  mediaError(video, 2);
  await settle();
  assert.equal(urls.filter((url) => url.searchParams.get("source") === "original").length, 1);
  mediaError(video, 2);
  await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  FakeHls.instances[0].fail("mediaError");
  await settle();
  assert.ok(playbackSourceError(video));
  assert.equal(urls.filter((url) => url.searchParams.get("source") === "original").length, 1);
});

test("signed HLS failures refresh once while manual compatibility remains pinned", async () => {
  const { video, urls } = setup({ mode: "compatible" });
  await settle();
  FakeHls.instances[0].fail("networkError", 403);
  await settle();
  assert.equal(FakeHls.instances[0].destroyed, true);
  assert.equal(urls.filter((url) => url.searchParams.get("refresh") === "1").length, 1);
  FakeHls.instances[1].fail("networkError", 403);
  await settle();
  assert.ok(playbackSourceError(video));
  assert.equal(playbackSourceState(video)?.mode, "compatible");
  assert.equal(video.getAttribute("src"), null);
});

test("opaque redirected MP4 failures refresh once before reporting a pinned rendition failure", async () => {
  const video = media();
  const file: PlaybackSource = { id: "cloud-22", label: "720p", kind: "transcoded", protocol: "file", status: "ready", mediaType: "video/mp4", url: "/p/playback/one/cloud-22", resolveUrl: "/api/video/one/playback?source=cloud-22" };
  const calls: URL[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path, init) => {
      const url = new URL(String(path)); calls.push(url);
      if (url.pathname.startsWith("/p/")) {
        assert.equal(init?.redirect, "manual");
        return new Response(null, { status: 302 });
      }
      return manifest(original, url.searchParams.has("source") ? file : cloud);
    }) as typeof fetch,
  });
  await settle(); await settle();
  mediaError(video, 4);
  await settle(); await settle();
  assert.equal(calls.filter((url) => url.searchParams.get("refresh") === "1").length, 1);
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-22");
  assert.equal(playbackSourceState(video)?.error, null);
  mediaError(video, 4);
  await settle(); await settle();
  assert.equal(calls.filter((url) => url.searchParams.get("refresh") === "1").length, 1);
  assert.ok(playbackSourceState(video)?.error);
  assert.equal(playbackSourceState(video)?.mode, "compatible");
});

test("HLS throttling stops without refresh or source rotation", async () => {
  const { video, urls } = setup({ mode: "compatible" });
  await settle();
  FakeHls.instances[0].fail("networkError", 429);
  await settle();
  assert.ok(playbackSourceError(video));
  assert.equal(urls.length, 2);
});

test("cloud processing waits without failing and user retry resolves it again", async () => {
  const video = media();
  let ready = false;
  const urls: URL[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path) => {
      const url = new URL(String(path));
      urls.push(url);
      return manifest(original, url.searchParams.get("source") === "cloud"
        ? ready ? readyCloud : { ...cloud, status: "processing", message: "网盘正在转码" } : cloud);
    }) as typeof fetch,
  });
  await settle();
  assert.equal(playbackSourceError(video), null);
  assert.equal(playbackSourceState(video)?.processing, true);
  assert.match(playbackSourceState(video)?.notice ?? "", /正在转码/);
  assert.equal(video.getAttribute("src"), null);
  ready = true;
  retryVideoSource(video);
  await settle();
  assert.equal(playbackSourceState(video)?.mode, "compatible");
  assert.equal(playbackSourceError(video), null);
  assert.equal(FakeHls.instances.length, 1);
  assert.equal(urls.at(-1)?.searchParams.get("refresh"), "1");
});

test("changing mode cancels a pending cloud resolution and ignores its late result", async () => {
  const video = media();
  let finish!: (response: Response) => void;
  let signal: AbortSignal | null | undefined;
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    loadHls,
    fetch: (async (path, init) => {
      if (String(path).includes("source=cloud")) {
        signal = init?.signal;
        return new Promise((resolve) => { finish = resolve; });
      }
      return manifest(original, cloud);
    }) as typeof fetch,
  });
  await settle();
  selectPlaybackMode(video, "compatible");
  await settle();
  selectPlaybackMode(video, "original");
  await settle();
  assert.equal(signal?.aborted, true);
  finish(manifest(original, readyCloud));
  await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "original");
  assert.equal(FakeHls.instances.length, 0);
});

test("reusing an iOS element aborts and ignores an earlier video's metadata", async () => {
  const video = media();
  let finish!: (response: Response) => void;
  let signal: AbortSignal | null | undefined;
  attachVideoSource(video, "old", "/api/video/old/playback", {
    fetch: ((_url, init) => {
      signal = init?.signal;
      return new Promise((resolve) => { finish = resolve; });
    }) as typeof fetch,
  });
  attachVideoSource(video, "new", "/api/video/new/playback", {
    fetch: (async () => manifest({ ...original, url: "https://cdn.example/new.mp4" })) as typeof fetch,
  });
  await settle();
  assert.equal(signal?.aborted, true);
  finish(manifest({ ...original, url: "https://cdn.example/old.mp4" }));
  await settle();
  assert.equal(video.src, "https://cdn.example/new.mp4");
});

for (const status of [401, 403, 404, 410, 429]) {
  test(`metadata HTTP ${status} stops without media loading or automatic retry`, async () => {
    const video = media();
    let requests = 0;
    attachVideoSource(video, original.url!, "/api/video/one/playback", {
      fetch: (async () => { requests++; return json({}, status); }) as typeof fetch,
    });
    await settle();
    assert.ok(playbackSourceError(video));
    assert.equal(requests, 1);
    assert.equal(video.getAttribute("src"), null);
  });
}

test("original media access failures stop instead of bypassing access through HLS", async () => {
  const video = media();
  let requests = 0;
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    fetch: (async (path) => {
      requests++;
      return String(path).includes("/p/stream/") ? json({}, 403) : manifest(original, cloud);
    }) as typeof fetch,
  });
  await settle();
  mediaError(video);
  await settle();
  assert.match(playbackSourceError(video)!, /无权/);
  assert.equal(requests, 2);
  assert.equal(FakeHls.instances.length, 0);
});

test("expired resolved links are refreshed before media loading", async () => {
  const video = media();
  const urls: URL[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path) => {
      const url = new URL(String(path));
      urls.push(url);
      return manifest(original, url.searchParams.has("refresh") ? readyCloud : { ...readyCloud, url: "https://hls.example/expired.m3u8", expiresAt: "2000-01-01T00:00:00Z" });
    }) as typeof fetch,
  });
  await settle();
  assert.equal(urls.length, 2);
  assert.equal(urls[1].searchParams.get("refresh"), "1");
  assert.equal(FakeHls.instances[0].url, readyCloud.url);
});

test("a canvas relay preference preserves smart selection, playback position and play intent", async () => {
  const { video } = setup();
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  video.currentTime = 33;
  await video.play();
  assert.equal(setVideoSourceRelay(video, true), true);
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(playbackSourceState(video)?.mode, "auto");
  assert.match(video.src, /tripleScreenRelay=1/);
  assert.equal(video.currentTime, 33);
  assert.equal(video.paused, false);
  setVideoSourceRelay(video, false);
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(new URL(video.src).pathname, "/p/stream/wo/file");
  assert.equal(new URL(video.src).search, "");
  assert.equal(video.currentTime, 33);
  assert.equal(playbackSourceState(video)?.mode, "auto");
});

test("file relay preferences leave active HLS playback attached", async () => {
  const { video, urls } = setup({ mode: "compatible" });
  await settle();
  const calls = urls.length;
  const hls = FakeHls.instances[0];
  setVideoSourceRelay(video, true);
  await settle();
  assert.equal(playbackSourceState(video)?.mode, "compatible");
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  assert.equal(playbackSourceURL(video), readyCloud.url);
  assert.equal(hls.destroyed, false);
  assert.equal(urls.length, calls);
  setVideoSourceRelay(video, false);
  await settle();
  assert.equal(hls.destroyed, false);
});

test("a relayed original can still fall back to compatible playback in smart mode", async () => {
  const { video } = setup();
  await settle();
  setVideoSourceRelay(video, true);
  await settle();
  mediaError(video);
  await settle();
  assert.equal(playbackSourceState(video)?.mode, "auto");
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  assert.equal(FakeHls.instances.length, 1);
});

test("unknown-source manifests cannot silently play the raw URL", async () => {
  const video = media();
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    fetch: (async () => json({ sources: [{ ...original, protocol: "invalid" }] })) as typeof fetch,
  });
  await settle();
  assert.match(playbackSourceError(video)!, /无效数据/);
  assert.equal(video.getAttribute("src"), null);
});

test("the controller also supports a direct HLS source without a metadata endpoint", async () => {
  const video = media();
  attachVideoSource(video, readyCloud.url!, undefined, { loadHls });
  await settle();
  assert.equal(FakeHls.instances[0].url, readyCloud.url);
  disposeVideoSource(video);
  FakeHls.instances[0].fail();
  await settle();
  assert.equal(FakeHls.instances.length, 1);
});

for (const status of [424, 502]) {
  test(`provider rate limits wrapped in HTTP ${status} stop original refresh and fallback`, async () => {
    const video = media();
    let requests = 0;
    attachVideoSource(video, original.url!, "/api/video/one/playback", {
      fetch: (async (path) => {
        requests++;
        return String(path).includes("/p/stream/")
          ? json({ code: "drive_rate_limited", message: "网盘当前正在限流" }, status)
          : manifest(original, cloud);
      }) as typeof fetch,
    });
    await settle();
    mediaError(video, 2);
    await settle();
    assert.match(playbackSourceError(video)!, /限流/);
    assert.equal(requests, 2);
  });
}

test("volume changes during cloud resolution remain effective after switching", async () => {
  const video = media();
  let finish!: (response: Response) => void;
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    loadHls,
    fetch: (async (path) => String(path).includes("source=cloud")
      ? new Promise((resolve) => { finish = resolve; }) : manifest(original, cloud)) as typeof fetch,
  });
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  selectPlaybackMode(video, "compatible");
  await settle();
  video.volume = 0.2;
  video.muted = true;
  video.dispatchEvent(new Event("volumechange"));
  finish(manifest(original, readyCloud));
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(video.volume, 0.2);
  assert.equal(video.muted, true);
});

test("unavailable cloud playback records the failed source when the original format was skipped", async () => {
  const { video } = setup({
    original: { ...original, mediaType: "video/mp2t" },
    cloud: { ...cloud, status: "unavailable", message: "联通网盘暂未提供该视频的兼容播放源" },
  });
  await settle();
  const error = playbackSourceState(video)?.error;
  assert.equal(error?.sourceID, "cloud");
  assert.equal(error?.reason, "unavailable");
  assert.deepEqual(error?.recoveryModes, []);
  assert.match(error?.message ?? "", /原画播放暂不可用，联通网盘暂未提供/);
  assert.equal(playbackSourceState(video)?.notice, null);
});

test("reacquiring a failed cloud source preserves the failure of the original instead of cycling", async () => {
  const { video, urls } = setup({
    cloud: { ...cloud, status: "unavailable", message: "联通网盘暂未提供该视频的兼容播放源" },
  });
  await settle();
  mediaError(video);
  await settle();
  const error = playbackSourceState(video)?.error;
  // The last attached source differs from the source whose resolution failed.
  assert.equal(playbackSourceState(video)?.activeSource?.id, "original");
  assert.equal(error?.sourceID, "cloud");
  assert.deepEqual(error?.recoveryModes, []);
  const previousCalls = urls.length;
  retryVideoSource(video);
  await settle();
  assert.equal(urls.length, previousCalls + 1);
  assert.equal(urls.at(-1)?.searchParams.get("source"), "cloud");
  assert.equal(urls.at(-1)?.searchParams.get("refresh"), "1");
  assert.equal(playbackSourceState(video)?.error?.sourceID, "cloud");
  assert.deepEqual(playbackSourceState(video)?.error?.recoveryModes, []);
});

test("reacquiring a missing cloud source can succeed while preserving smart selection", async () => {
  const video = media();
  let cloudReady = false;
  const urls: URL[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    loadHls,
    fetch: (async (path) => {
      const url = new URL(String(path));
      urls.push(url);
      return manifest({ ...original, mediaType: "video/mp2t" }, url.searchParams.get("source") === "cloud"
        ? cloudReady ? readyCloud : { ...cloud, status: "unavailable", message: "联通网盘暂未提供该视频的兼容播放源" } : cloud);
    }) as typeof fetch,
  });
  await settle();
  cloudReady = true;
  retryVideoSource(video);
  await settle();
  assert.equal(urls.at(-1)?.searchParams.get("source"), "cloud");
  assert.equal(urls.at(-1)?.searchParams.get("refresh"), "1");
  assert.equal(playbackSourceState(video)?.mode, "auto");
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  assert.equal(playbackSourceError(video), null);
});

test("manual compatibility failure offers original only when it is still playable", async () => {
  const { video } = setup({
    mode: "compatible", cloud: { ...cloud, status: "unavailable", message: "兼容播放源暂不可用" },
  });
  await settle();
  assert.equal(playbackSourceState(video)?.error?.sourceID, "cloud");
  assert.equal(playbackSourceState(video)?.error?.reason, "unavailable");
  assert.deepEqual(playbackSourceState(video)?.error?.recoveryModes, ["original"]);
  selectPlaybackMode(video, "original");
  await settle();
  assert.equal(playbackSourceError(video), null);
});

test("manual compatibility failure does not offer an unsupported original", async () => {
  const { video } = setup({
    mode: "compatible", original: { ...original, mediaType: "video/mp2t" },
    cloud: { ...cloud, status: "unavailable", message: "兼容播放源暂不可用" },
  });
  await settle();
  assert.deepEqual(playbackSourceState(video)?.error?.recoveryModes, []);
});

test("manual original decoding failure still offers an unresolved compatible source", async () => {
  const { video } = setup({ mode: "original" });
  await settle();
  mediaError(video);
  await settle();
  assert.equal(playbackSourceState(video)?.error?.sourceID, "original");
  assert.equal(playbackSourceState(video)?.error?.reason, "unsupported");
  assert.deepEqual(playbackSourceState(video)?.error?.recoveryModes, ["compatible"]);
});

test("retrying an unsupported original refreshes discovery instead of loading unsupported bytes", async () => {
  const video = media();
  const urls: URL[] = [];
  const unsupportedOriginal = { ...original, mediaType: "video/mp2t" };
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    loadHls,
    fetch: (async (path) => {
      const url = new URL(String(path));
      urls.push(url);
      return urls.length === 1 ? manifest(unsupportedOriginal)
        : manifest(unsupportedOriginal, url.searchParams.get("source") === "cloud" ? readyCloud : cloud);
    }) as typeof fetch,
  });
  await settle();
  assert.equal(playbackSourceState(video)?.error?.sourceID, "original");
  retryVideoSource(video);
  await settle();
  assert.equal(urls.length, 3);
  assert.equal(urls[1].searchParams.get("refresh"), "1");
  assert.equal(video.getAttribute("src"), null);
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
});

test("an unsuccessful fallback never announces a successful source switch", async () => {
  const { video } = setup({ cloud: { ...cloud, status: "unavailable", message: "联通网盘暂未提供该视频的兼容播放源" } });
  await settle();
  const notices: string[] = [];
  video.addEventListener("playback-source-change", () => {
    const notice = playbackSourceState(video)?.notice;
    if (notice) notices.push(notice);
  });
  mediaError(video);
  await settle();
  assert.ok(playbackSourceError(video));
  assert.deepEqual(notices, []);
});

test("concrete HLS selection resolves only that rendition and preserves playback state", async () => {
  const video = media();
  const high: PlaybackSource = { ...readyCloud, id: "cloud-high", label: "高清", resolution: "high", resolveUrl: "/api/video/one/playback?source=cloud-high" };
  const low: PlaybackSource = { ...cloud, id: "cloud-low", label: "流畅", resolution: "low", resolveUrl: "/api/video/one/playback?source=cloud-low" };
  const calls: string[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path) => {
      const source = new URL(String(path)).searchParams.get("source") ?? "";
      calls.push(source);
      if (!source) return manifest(original, cloud);
      if (source === "cloud-low") return manifest(original, { ...high, status: "unknown", url: undefined }, { ...low, status: "ready", url: "/p/hls/low/index.m3u8" });
      return manifest(original, high, low);
    }) as typeof fetch,
  });
  await settle(); await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  video.currentTime = 42; video.playbackRate = 1.5; video.volume = 0.3; video.muted = true;
  await video.play();
  assert.equal(selectPlaybackSource(video, "cloud-low"), true);
  await settle(); await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(playbackSourceState(video)?.selectedSourceID, "cloud-low");
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-low");
  assert.equal(FakeHls.instances.at(-1)?.url, "/p/hls/low/index.m3u8");
  assert.equal(video.currentTime, 42);
  assert.equal(video.playbackRate, 1.5);
  assert.equal(video.volume, 0.3);
  assert.equal(video.muted, true);
  assert.equal(video.paused, false);
  assert.equal(calls.at(-1), "cloud-low");
});

test("progressive compatibility uses native MP4 and preserves progress when changing quality", async () => {
  const video = media();
  const high: PlaybackSource = { id: "cloud-22", label: "720p", kind: "transcoded", protocol: "file", status: "ready", mediaType: "video/mp4", url: "/p/playback/one/cloud-22", resolveUrl: "/api/video/one/playback?source=cloud-22" };
  const low: PlaybackSource = { ...high, id: "cloud-18", label: "360p", url: "/p/playback/one/cloud-18", resolveUrl: "/api/video/one/playback?source=cloud-18" };
  const requests: string[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path) => {
      const source = new URL(String(path)).searchParams.get("source") ?? "";
      requests.push(source);
      if (!source) return manifest(original, cloud);
      return manifest(original, high, low);
    }) as typeof fetch,
  });
  await settle(); await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-22");
  assert.equal(video.getAttribute("src"), "/p/playback/one/cloud-22");
  assert.equal(FakeHls.instances.length, 0);
  video.dispatchEvent(new Event("loadedmetadata"));
  video.currentTime = 42; video.playbackRate = 1.5;
  await video.play();
  setVideoSourceRelay(video, true);
  await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.match(video.src, /tripleScreenRelay=1/);
  const requestCount = requests.length;
  selectPlaybackSource(video, "cloud-18");
  await settle(); await settle();
  video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(video.getAttribute("src"), "/p/playback/one/cloud-18?tripleScreenRelay=1");
  assert.equal(video.currentTime, 42);
  assert.equal(video.playbackRate, 1.5);
  assert.equal(video.paused, false);
  assert.equal(requests.length, requestCount);
  assert.equal(FakeHls.instances.length, 0);
  setVideoSourceRelay(video, false);
  await settle();
  assert.equal(video.getAttribute("src"), "/p/playback/one/cloud-18");
});

for (const shared of [false, true]) {
  test(`${shared ? "shared" : "signed-in"} progressive playback refreshes a disappeared rendition and recovers to the current quality`, async () => {
    const video = media();
    const metadataURL = shared ? "/api/share/share-1/playback" : "/api/video/one/playback";
    const mediaURL = shared ? "/p/share/share-1/playback" : "/p/playback/one";
    const high: PlaybackSource = { id: "cloud-22", label: "720p", kind: "transcoded", protocol: "file", status: "ready", mediaType: "video/mp4", url: `${mediaURL}/cloud-22`, resolveUrl: `${metadataURL}?source=cloud-22` };
    const low: PlaybackSource = { ...high, id: "cloud-18", label: "360p", url: `${mediaURL}/cloud-18`, resolveUrl: `${metadataURL}?source=cloud-18` };
    let renditions = [high, low];
    const calls: URL[] = [];
    attachVideoSource(video, original.url!, metadataURL, {
      mode: "compatible",
      fetch: (async (path) => {
        const url = new URL(String(path)); calls.push(url);
        if (url.pathname.startsWith("/p/")) return json({ code: "playback_source_expired", message: "兼容播放地址已过期，请重新获取" }, 410);
        return url.searchParams.has("source") ? manifest(original, ...renditions)
          : manifest(original, { ...cloud, resolveUrl: `${metadataURL}?source=cloud` });
      }) as typeof fetch,
    });
    await settle(); await settle();
    video.dispatchEvent(new Event("loadedmetadata"));
    video.currentTime = 42;
    renditions = [low];
    mediaError(video, 4);
    await settle(); await settle();
    video.dispatchEvent(new Event("loadedmetadata"));
    assert.equal(playbackSourceState(video)?.error, null);
    assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-18");
    assert.equal(video.getAttribute("src"), low.url);
    assert.equal(video.currentTime, 42);
    assert.equal(calls.filter((url) => url.searchParams.get("refresh") === "1").length, 1);
    assert.equal(calls.at(-1)?.searchParams.get("source"), "cloud-22");
  });
}

test("a disappeared pinned rendition stays retryable without silently selecting another quality", async () => {
  const video = media();
  const high: PlaybackSource = { id: "cloud-22", label: "720p", kind: "transcoded", protocol: "file", status: "ready", mediaType: "video/mp4", url: "/p/playback/one/cloud-22", resolveUrl: "/api/video/one/playback?source=cloud-22" };
  const low: PlaybackSource = { ...high, id: "cloud-18", label: "360p", url: "/p/playback/one/cloud-18", resolveUrl: "/api/video/one/playback?source=cloud-18" };
  let renditions = [high, low];
  const calls: URL[] = [];
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible",
    fetch: (async (path) => {
      const url = new URL(String(path)); calls.push(url);
      if (url.pathname.startsWith("/p/")) return json({ code: "playback_source_expired" }, 410);
      return url.searchParams.has("source") ? manifest(original, ...renditions) : manifest(original, cloud);
    }) as typeof fetch,
  });
  await settle(); await settle();
  selectPlaybackSource(video, high.id);
  await settle();
  renditions = [low];
  mediaError(video, 4);
  await settle(); await settle();
  assert.equal(playbackSourceState(video)?.selectedSourceID, high.id);
  assert.equal(playbackSourceState(video)?.error?.sourceID, high.id);
  assert.equal(playbackSourceState(video)?.error?.reason, "unavailable");
  assert.deepEqual(playbackSourceState(video)?.error?.recoveryModes, ["original", "compatible"]);
  assert.equal(video.getAttribute("src"), null);
  assert.equal(calls.filter((url) => url.searchParams.has("refresh")).length, 1);
  renditions = [high, low];
  retryVideoSource(video);
  await settle(); await settle();
  assert.equal(calls.at(-1)?.searchParams.get("source"), high.id);
  assert.equal(playbackSourceState(video)?.error, null);
  assert.equal(playbackSourceState(video)?.activeSource?.id, high.id);
  assert.equal(playbackSourceState(video)?.selectedSourceID, high.id);
});

for (const pinned of [false, true]) {
  test(`Google web login failure during ${pinned ? "pinned" : "automatic"} compatibility playback preserves original recovery`, async () => {
    const video = media();
    const high: PlaybackSource = { id: "cloud-22", label: "720p", kind: "transcoded", protocol: "file", status: "ready", mediaType: "video/mp4", url: "/p/playback/one/cloud-22", resolveUrl: "/api/video/one/playback?source=cloud-22" };
    const message = "Google Drive 网页登录已失效，请联系管理员更新兼容播放 Cookie";
    let cookieValid = true;
    attachVideoSource(video, original.url!, "/api/video/one/playback", {
      mode: "compatible",
      fetch: (async (path) => {
        const url = new URL(String(path));
        if (url.pathname.startsWith("/p/")) return json({ code: "playback_source_expired" }, 410);
        return url.searchParams.has("source")
          ? manifest(original, cookieValid ? high : { ...cloud, protocol: "file", status: "unavailable", message })
          : manifest(original, cloud);
      }) as typeof fetch,
    });
    await settle(); await settle();
    if (pinned) { selectPlaybackSource(video, high.id); await settle(); }
    cookieValid = false;
    mediaError(video, 4);
    await settle(); await settle();
    assert.equal(playbackSourceState(video)?.error?.message, message);
    assert.equal(playbackSourceState(video)?.error?.reason, "unavailable");
    assert.deepEqual(playbackSourceState(video)?.error?.recoveryModes, ["original"]);
    selectPlaybackMode(video, "original");
    await settle();
    assert.equal(playbackSourceState(video)?.error, null);
    assert.equal(playbackSourceState(video)?.activeSource?.id, "original");
    assert.equal(video.getAttribute("src"), original.url);
  });
}

test("file relay refresh uses the current resolved URL and keeps its query parameters", async () => {
  const video = media();
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    fetch: (async (path) => {
      const refreshed = new URL(String(path)).searchParams.get("refresh") === "1";
      return manifest({ ...original, url: `/p/stream/wo/file?token=${refreshed ? "new" : "old"}&playbackId=current` });
    }) as typeof fetch,
  });
  await settle();
  setVideoSourceRelay(video, true);
  await settle();
  retryVideoSource(video);
  await settle(); await settle();
  const url = new URL(video.src);
  assert.equal(url.searchParams.get("token"), "new");
  assert.equal(url.searchParams.get("playbackId"), "current");
  assert.equal(url.searchParams.get("tripleScreenRelay"), "1");
  setVideoSourceRelay(video, false);
  await settle();
  assert.equal(new URL(video.src).searchParams.get("token"), "new");
  assert.equal(new URL(video.src).searchParams.has("tripleScreenRelay"), false);
});

test("compatibility chooses a ready lower rendition while the higher one is processing", async () => {
  const video = media();
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path) => new URL(String(path)).searchParams.has("source")
      ? manifest(original, { ...cloud, id: "cloud-high", status: "processing", resolveUrl: "/api/video/one/playback?source=cloud-high" }, { ...readyCloud, id: "cloud-low", url: "/p/hls/low/index.m3u8" })
      : manifest(original, cloud)) as typeof fetch,
  });
  await settle(); await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-low");
  assert.equal(playbackSourceState(video)?.processing, false);
});

test("switching to a shorter rendition resets a saved position beyond its end", async () => {
  const env = setup();
  await settle();
  env.video.dispatchEvent(new Event("loadedmetadata"));
  env.video.currentTime = 179.9;
  selectPlaybackMode(env.video, "compatible");
  await settle(); await settle();
  Object.defineProperty(env.video, "duration", { value: 179.5, configurable: true });
  env.video.dispatchEvent(new Event("loadedmetadata"));
  assert.equal(env.video.currentTime, 0);
});

test("a higher rendition becoming ready during polling remains selected", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });
  const video = media();
  let ready = false;
  const high: PlaybackSource = { ...cloud, id: "cloud-high", label: "高清", status: "processing", resolveUrl: "/api/video/one/playback?source=cloud-high" };
  const low: PlaybackSource = { ...cloud, id: "cloud-low", label: "流畅", status: "processing", resolveUrl: "/api/video/one/playback?source=cloud-low" };
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path) => {
      const selected = new URL(String(path)).searchParams.get("source");
      if (!selected) return manifest(original, cloud);
      return ready ? manifest(original, { ...high, status: "ready", url: "/p/hls/high/index.m3u8" }, { ...low, status: "unknown" }) : manifest(original, high, low);
    }) as typeof fetch,
  });
  const flush = () => new Promise<void>((resolve) => setImmediate(resolve));
  await flush();
  assert.equal(playbackSourceState(video)?.processing, true);
  ready = true;
  t.mock.timers.tick(15000); await flush(); await flush();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-high");
  assert.equal(playbackSourceState(video)?.loading, false);
  assert.equal(FakeHls.instances.at(-1)?.url, "/p/hls/high/index.m3u8");
});

test("automatic compatibility falls back between renditions while a concrete selection stays pinned", async () => {
  const video = media();
  const high: PlaybackSource = { ...readyCloud, id: "cloud-high", label: "高清", resolveUrl: "/api/video/one/playback?source=cloud-high" };
  const low: PlaybackSource = { ...readyCloud, id: "cloud-low", label: "流畅", resolveUrl: "/api/video/one/playback?source=cloud-low" };
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    mode: "compatible", loadHls,
    fetch: (async (path) => {
      const selected = new URL(String(path)).searchParams.get("source");
      if (!selected) return manifest(original, cloud);
      return manifest(original, selected === "cloud-low" ? { ...high, status: "unknown", url: undefined } : high,
        selected === "cloud-low" ? low : { ...low, status: "unknown", url: undefined });
    }) as typeof fetch,
  });
  await settle(); await settle();
  FakeHls.instances.at(-1)!.fail(); await settle(); await settle();
  FakeHls.instances.at(-1)!.fail(); await settle(); await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-low");
  assert.equal(playbackSourceState(video)?.error, null);
  selectPlaybackSource(video, "cloud-high"); await settle(); await settle();
  FakeHls.instances.at(-1)!.fail(); await settle(); await settle();
  FakeHls.instances.at(-1)!.fail(); await settle(); await settle();
  assert.equal(playbackSourceState(video)?.selectedSourceID, "cloud-high");
  assert.equal(playbackSourceState(video)?.error?.sourceID, "cloud-high");
});

test("original source diagnostics follow actual playback and seeks without reloading media", async () => {
  const video = media();
  const id = "8a0986cc-e25b-4d86-ac35-8d8a6b242bcb";
  const tracked = { ...original, url: original.url + "?playbackId=" + id, sourceInfoUrl: "/api/video/one/playback?source=original&playbackId=" + id };
  let urlSource = "quark-raw", infoCalls = 0;
  attachVideoSource(video, tracked.url, "/api/video/one/playback", {
    fetch: (async (path) => {
      if (new URL(String(path)).searchParams.has("playbackId")) { infoCalls++; return manifest({ ...tracked, urlSource }); }
      return manifest(tracked, cloud);
    }) as typeof fetch,
    loadHls,
  });
  await settle();
  assert.equal(infoCalls, 0);
  assert.equal(playbackStateLabel(playbackSourceState(video)), "原画播放");
  video.dispatchEvent(new Event("loadedmetadata")); await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.urlSource, "quark-raw");
  assert.equal(playbackStateLabel(playbackSourceState(video)), "原画·RAW");
  assert.equal(originalPlaybackLabel("quark-raw"), "原画播放（RAW接口）");
  video.currentTime = 42; urlSource = "quark-download";
  video.dispatchEvent(new Event("seeked")); await settle();
  assert.equal(playbackStateLabel(playbackSourceState(video)), "原画·下载");
  assert.equal(originalPlaybackLabel("quark-download"), "原画播放（下载接口）");
  assert.equal(video.currentTime, 42);
  const before = playbackSourceURL(video);
  setVideoSourceRelay(video, true); await settle();
  const delivery = new URL(playbackSourceURL(video), window.location.href);
  assert.equal(delivery.searchParams.get("playbackId"), id);
  assert.equal(delivery.searchParams.get("tripleScreenRelay"), "1");
  assert.ok(before.includes(id));
});

test("late original-source information cannot relabel a different active source", async () => {
  const video = media();
  const tracked = { ...original, sourceInfoUrl: "/api/video/one/playback?source=original&playbackId=old" };
  let finish!: (response: Response) => void;
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    fetch: (async (path) => {
      const url = new URL(String(path));
      if (url.searchParams.has("playbackId")) return new Promise<Response>((resolve) => { finish = resolve; });
      return manifest(tracked, url.searchParams.get("source") === "cloud" ? readyCloud : cloud);
    }) as typeof fetch, loadHls,
  });
  await settle(); video.dispatchEvent(new Event("loadedmetadata"));
  selectPlaybackMode(video, "compatible"); await settle(); await settle();
  finish(manifest({ ...tracked, urlSource: "quark-download" })); await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  assert.equal(playbackStateLabel(playbackSourceState(video)), "兼容播放");
  assert.equal(playbackSourceState(video)?.activeSource?.urlSource, undefined);
});

test("source information never sends credentials to a foreign origin", async () => {
  const video = media(); let calls = 0;
  attachVideoSource(video, original.url!, "/api/video/one/playback", {
    fetch: (async () => { calls++; return manifest({ ...original, sourceInfoUrl: "https://external.example/source-info" }); }) as typeof fetch,
  });
  await settle(); video.dispatchEvent(new Event("loadedmetadata")); await settle();
  assert.equal(calls, 1);
  assert.equal(playbackStateLabel(playbackSourceState(video)), "原画播放");
});
