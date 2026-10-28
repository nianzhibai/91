import "./helpers/dom-environment.ts";
import assert from "node:assert/strict";
import test, { afterEach } from "node:test";
import { createElement } from "react";
import { cleanup, render, act } from "@testing-library/react";
import Artplayer from "artplayer";
import { VideoPlayer } from "../src/components/VideoPlayer";
import { playbackSourceState, retryVideoSource, selectPlaybackMode } from "../src/lib/videoSource";
import { TripleScreenRenderer } from "../src/lib/tripleScreen";

for (const name of ["Element", "HTMLMediaElement", "HTMLCanvasElement", "Image"] as const) {
  Object.defineProperty(globalThis, name, { configurable: true, value: window[name] });
}
Object.assign(globalThis, {
  screen: window.screen,
  location: window.location,
  localStorage: window.localStorage,
  getComputedStyle: window.getComputedStyle.bind(window),
  requestAnimationFrame: window.requestAnimationFrame.bind(window),
  cancelAnimationFrame: window.cancelAnimationFrame.bind(window),
  ResizeObserver: class { observe() {} unobserve() {} disconnect() {} },
});
Artplayer.LOG_VERSION = false;
const paused = new WeakMap<HTMLMediaElement, boolean>();
Object.defineProperty(window.HTMLMediaElement.prototype, "paused", {
  configurable: true, get() { return paused.get(this) ?? true; },
});
window.HTMLMediaElement.prototype.load = () => {};
window.HTMLMediaElement.prototype.pause = function () {
  if (paused.get(this) !== false) return;
  paused.set(this, true);
  this.dispatchEvent(new Event("pause"));
};
window.HTMLMediaElement.prototype.play = async function () {
  paused.set(this, false);
  this.dispatchEvent(new Event("play"));
};
window.HTMLMediaElement.prototype.canPlayType = () => "probably";
const realFetch = window.fetch;
const reconnectDelay = Artplayer.RECONNECT_SLEEP_TIME;
afterEach(() => {
  cleanup();
  window.fetch = realFetch;
  Artplayer.RECONNECT_SLEEP_TIME = reconnectDelay;
});
const settle = async () => { await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); }); };

async function tripleScreenPlayer(src = "/p/stream/115/file") {
  const requests: string[] = [];
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: src, mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  window.fetch = (async (path) => {
    requests.push(String(path));
    if (String(path).includes("/p/stream/")) return new Response(null, { status: 206 });
    return new Response(JSON.stringify({ sources: [original, String(path).includes("source=cloud")
      ? { ...cloud, status: "ready", url: "/p/hls/one/session/index.m3u8" } : cloud] }), { status: 200 });
  }) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  const control = view.container.querySelector<HTMLElement>("[data-triple-screen-control]")!;
  Object.defineProperties(video, {
    videoWidth: { configurable: true, value: 720 }, videoHeight: { configurable: true, value: 1280 },
    readyState: { configurable: true, value: 4 },
  });
  const frameReady = async () => {
    await act(async () => {
      video.dispatchEvent(new Event("loadedmetadata"));
      video.dispatchEvent(new Event("loadeddata"));
    });
    await settle();
  };
  await frameReady();
  return { view, video, control, requests, frameReady };
}

test("triple screen uses the active compatible source even when its video URL is a blob", async (t) => {
  const enable = t.mock.method(TripleScreenRenderer.prototype, "enable", () => true);
  const env = await tripleScreenPlayer();
  await act(async () => { selectPlaybackMode(env.video, "compatible"); });
  await settle();
  env.video.src = "blob:http://localhost/compatible-video";
  await env.frameReady();
  const requests = env.requests.length;
  await act(async () => { env.control.click(); });
  await settle();
  assert.equal(playbackSourceState(env.video)?.mode, "compatible");
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
  assert.equal(env.video.src, "blob:http://localhost/compatible-video");
  assert.equal(env.requests.length, requests);
  assert.equal(env.control.getAttribute("aria-pressed"), "true");
  assert.equal(enable.mock.callCount(), 1);
  await act(async () => { env.control.click(); });
  assert.equal(env.control.getAttribute("aria-pressed"), "false");
  assert.equal(playbackSourceState(env.video)?.mode, "compatible");
});

test("triple screen restores rendering after compatibility refresh and both playback mode switches", async (t) => {
  const enable = t.mock.method(TripleScreenRenderer.prototype, "enable", () => true);
  const env = await tripleScreenPlayer();
  await act(async () => { selectPlaybackMode(env.video, "compatible"); });
  await settle();
  await env.frameReady();
  await act(async () => { env.control.click(); });
  assert.equal(enable.mock.callCount(), 1);
  await act(async () => { retryVideoSource(env.video); });
  await settle();
  await env.frameReady();
  assert.equal(playbackSourceState(env.video)?.mode, "compatible");
  assert.equal(enable.mock.callCount(), 2);
  await act(async () => { selectPlaybackMode(env.video, "original"); });
  await settle();
  await env.frameReady();
  assert.equal(playbackSourceState(env.video)?.mode, "original");
  assert.match(env.video.src, /tripleScreenRelay=1/);
  await env.frameReady();
  assert.equal(env.control.getAttribute("aria-pressed"), "true");
  await act(async () => { selectPlaybackMode(env.video, "compatible"); });
  await settle();
  await env.frameReady();
  assert.equal(playbackSourceState(env.video)?.mode, "compatible");
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
  assert.equal(env.control.getAttribute("aria-pressed"), "true");
  assert.equal(enable.mock.callCount(), 4);
});

test("original relay preserves smart selection and can recover to HLS without losing triple screen", async (t) => {
  const enable = t.mock.method(TripleScreenRenderer.prototype, "enable", () => true);
  const env = await tripleScreenPlayer();
  env.video.currentTime = 37;
  await act(async () => { env.control.click(); });
  await settle();
  assert.equal(playbackSourceState(env.video)?.mode, "auto");
  assert.match(env.video.src, /tripleScreenRelay=1/);
  await env.frameReady();
  assert.equal(env.video.currentTime, 37);
  Object.defineProperty(env.video, "error", { configurable: true, value: { code: 3 } });
  await act(async () => { env.video.dispatchEvent(new Event("error")); });
  await settle();
  Object.defineProperty(env.video, "error", { configurable: true, value: null });
  await env.frameReady();
  assert.equal(playbackSourceState(env.video)?.mode, "auto");
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
  assert.equal(env.control.getAttribute("aria-pressed"), "true");
  assert.equal(enable.mock.callCount(), 2);
});

test("shared compatible playback enables triple screen without selecting the shared original stream", async (t) => {
  t.mock.method(TripleScreenRenderer.prototype, "enable", () => true);
  const env = await tripleScreenPlayer("/p/share/share-token/stream");
  await act(async () => { selectPlaybackMode(env.video, "compatible"); });
  await settle();
  await env.frameReady();
  await act(async () => { env.control.click(); });
  assert.equal(playbackSourceState(env.video)?.mode, "compatible");
  assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud");
  assert.doesNotMatch(env.video.src, /tripleScreenRelay/);
});

test("ArtPlayer playback selector uses the shared controller without reconnecting an old source", async () => {
  const urls: URL[] = [];
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  window.fetch = (async (path) => {
    const url = new URL(String(path));
    urls.push(url);
    return new Response(JSON.stringify({ sources: [original, url.searchParams.get("source") === "cloud" ? { ...cloud, status: "ready", url: "https://hls.example/index.m3u8" } : cloud] }), { status: 200 });
  }) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  const art = Artplayer.instances.find((instance) => instance.video === video)!;
  Artplayer.RECONNECT_SLEEP_TIME = 5;
  let reconnects = 0;
  art.on("error", () => { reconnects++; });
  assert.ok(art.setting.find("playback-mode"));
  assert.equal(urls.length, 1);
  const setting = art.setting.find("playback-mode")!;
  await act(async () => { setting.$item.click(); });
  const compatible = view.container.querySelector<HTMLElement>('[data-name="playback-mode-compatible"]');
  assert.ok(compatible);
  await act(async () => { compatible.click(); });
  await settle();
  assert.equal(playbackSourceState(video)?.mode, "compatible");
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  assert.equal(urls.length, 2);
  assert.match(art.setting.find("playback-mode")!.tooltip!, /兼容播放/);
  await act(async () => {
    video.dispatchEvent(new Event("error"));
    await new Promise((resolve) => setTimeout(resolve, 30));
  });
  assert.equal(playbackSourceState(video)?.mode, "compatible");
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  assert.equal(reconnects, 0);
});

test("an unsupported original-only video identifies the browser limitation without mentioning compatibility", async (t) => {
  t.mock.method(window.HTMLMediaElement.prototype, "canPlayType", (type: string) => type === "video/quicktime" ? "" : "probably");
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/googledrive/file", mediaType: "video/quicktime" };
  const requests: string[] = [];
  window.fetch = (async (path) => {
    requests.push(String(path));
    return new Response(JSON.stringify({ sources: [original] }), { status: 200 });
  }) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  assert.equal(playbackSourceState(video)?.error?.reason, "unsupported");
  assert.equal(playbackSourceState(video)?.error?.message, "浏览器无法播放当前视频");
  assert.equal(view.container.querySelector(".video-player__error-title")?.textContent, "视频无法播放");
  assert.equal(view.container.querySelector(".video-player__error-message")?.textContent, "浏览器无法播放当前视频");
  assert.doesNotMatch(view.getByRole("alert").textContent ?? "", /兼容播放|视频源暂不可用|其他浏览器|外部播放器/);
  assert.equal(requests.length, 1);
  assert.equal(video.getAttribute("src"), null);
  assert.equal(view.queryByRole("button", { name: "切换兼容播放" }), null);
});

test("an original-only decoding error uses the same browser-specific failure card", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/googledrive/file", mediaType: "video/mp4" };
  window.fetch = (async (path) => String(path).includes("/p/stream/")
    ? new Response(null, { status: 206 })
    : new Response(JSON.stringify({ sources: [original] }), { status: 200 })) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  Object.defineProperty(video, "error", { configurable: true, value: { code: 3 } });
  await act(async () => { video.dispatchEvent(new Event("error")); });
  await settle();
  assert.equal(playbackSourceState(video)?.error?.reason, "unsupported");
  assert.equal(playbackSourceState(video)?.error?.message, "浏览器无法播放当前视频");
  assert.equal(view.container.querySelector(".video-player__error-title")?.textContent, "视频无法播放");
  assert.equal(view.container.querySelector(".video-player__error-message")?.textContent, "浏览器无法播放当前视频");
  assert.doesNotMatch(view.getByRole("alert").textContent ?? "", /兼容播放|视频源暂不可用|其他浏览器|外部播放器/);
});

test("an original-only access error retains its source message without blaming browser support", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/googledrive/file", mediaType: "video/mp4" };
  window.fetch = (async (path) => String(path).includes("/p/stream/")
    ? new Response(JSON.stringify({ message: "当前账号无权访问该视频源" }), { status: 403, headers: { "Content-Type": "application/json" } })
    : new Response(JSON.stringify({ sources: [original] }), { status: 200 })) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  Object.defineProperty(video, "error", { configurable: true, value: { code: 4 } });
  await act(async () => { video.dispatchEvent(new Event("error")); });
  await settle();
  assert.equal(playbackSourceState(video)?.error?.reason, "access");
  assert.equal(view.container.querySelector(".video-player__error-title")?.textContent, "视频源暂不可用");
  assert.match(view.getByRole("alert").textContent ?? "", /当前账号无权访问该视频源/);
  assert.doesNotMatch(view.getByRole("alert").textContent ?? "", /浏览器无法播放当前视频|兼容播放/);
});

test("a drive initialization failure takes precedence over an unsupported TS format", async (t) => {
  t.mock.method(window.HTMLMediaElement.prototype, "canPlayType", () => "");
  let requests = 0;
  window.fetch = (async () => {
    requests++;
    return new Response(JSON.stringify({ code: "drive_auth_failed", message: "联通网盘登录或授权已失效，请联系管理员重新登录" }), {
      status: 424, headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: "/p/stream/wopan/file", playbackUrl: "/api/video/ts/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  assert.equal(playbackSourceState(video)?.error?.reason, "access");
  assert.equal(view.container.querySelector(".video-player__error-title")?.textContent, "视频源暂不可用");
  assert.equal(view.container.querySelector(".video-player__error-message")?.textContent, "联通网盘登录或授权已失效，请联系管理员重新登录");
  assert.doesNotMatch(view.getByRole("alert").textContent ?? "", /浏览器无法播放当前视频|兼容播放/);
  assert.equal(video.getAttribute("src"), null);
  assert.equal(requests, 1);
  await act(async () => { view.getByRole("button", { name: "重新获取" }).click(); });
  await settle();
  assert.equal(requests, 2);
  assert.equal(playbackSourceState(video)?.error?.reason, "access");
});

test("a failed compatibility selection exposes a working original switch in the error overlay", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  window.fetch = (async (path) => new Response(JSON.stringify({
    sources: [original, String(path).includes("source=cloud") ? { ...cloud, status: "unavailable", message: "兼容播放源暂不可用" } : cloud],
  }), { status: 200 })) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  const art = Artplayer.instances.find((instance) => instance.video === video)!;
  await act(async () => { art.setting.find("playback-mode")!.$item.click(); });
  await act(async () => { view.container.querySelector<HTMLElement>('[data-name="playback-mode-compatible"]')!.click(); });
  await settle();
  assert.match(view.getByRole("alert").textContent ?? "", /兼容播放源暂不可用/);
  await act(async () => { view.getByRole("button", { name: "尝试原画播放" }).click(); });
  await settle();
  assert.equal(view.queryByRole("alert"), null);
  assert.equal(playbackSourceState(video)?.mode, "original");
  assert.equal(playbackSourceState(video)?.activeSource?.id, "original");
});

test("conversion appears as a waiting status and can be canceled by selecting original", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  let submissions = 0;
  window.fetch = (async (path, init) => {
    if (init?.method === "POST") submissions++;
    return new Response(JSON.stringify({ sources: [original,
      String(path).includes("source=cloud") || init?.method === "POST"
        ? { ...cloud, status: "processing", message: "视频正在转码，完成后将自动播放", prepareUrl: "/api/video/one/playback/prepare" } : cloud,
    ] }), { status: 200 });
  }) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  await act(async () => { selectPlaybackMode(video, "compatible"); });
  await settle();
  assert.equal(view.queryByRole("alert"), null);
  assert.match(view.getByRole("status").textContent ?? "", /正在转码/);
  assert.equal(submissions, 1);
  await act(async () => { selectPlaybackMode(video, "original"); });
  await settle();
  assert.equal(view.queryByRole("status"), null);
  assert.equal(playbackSourceState(video)?.activeSource?.id, "original");
});

test("missing cloud source offers reacquisition instead of switching to an unavailable source", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp2t" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  const canPlayType = window.HTMLMediaElement.prototype.canPlayType;
  window.HTMLMediaElement.prototype.canPlayType = (type) => type === "video/mp2t" ? "" : "probably";
  const urls: URL[] = [];
  window.fetch = (async (path) => {
    const url = new URL(String(path));
    urls.push(url);
    const resolved = url.searchParams.get("source") === "cloud";
    const refreshed = url.searchParams.get("refresh") === "1";
    return new Response(JSON.stringify({ sources: [original, !resolved ? cloud : refreshed
      ? { ...cloud, status: "ready", url: "https://hls.example/index.m3u8" }
      : { ...cloud, status: "unavailable", message: "联通网盘暂未提供该视频的兼容播放源" }] }), { status: 200 });
  }) as typeof fetch;
  try {
    const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
    await settle();
    const alert = view.getByRole("alert");
    assert.match(alert.textContent ?? "", /原画播放暂不可用，联通网盘暂未提供/);
    assert.equal(view.queryByRole("button", { name: "切换兼容播放" }), null);
    assert.equal(view.queryByRole("button", { name: "尝试原画播放" }), null);
    await act(async () => { view.getByRole("button", { name: "重新获取" }).click(); });
    await settle();
    assert.equal(urls.at(-1)?.searchParams.get("source"), "cloud");
    assert.equal(urls.at(-1)?.searchParams.get("refresh"), "1");
    assert.equal(view.queryByRole("alert"), null);
    const video = view.container.querySelector("video")!;
    assert.equal(playbackSourceState(video)?.mode, "auto");
    assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud");
  } finally {
    window.HTMLMediaElement.prototype.canPlayType = canPlayType;
  }
});

test("a failed automatic fallback does not offer the last attached original or the unavailable cloud", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  const urls: URL[] = [];
  window.fetch = (async (path) => {
    const url = new URL(String(path));
    urls.push(url);
    if (url.pathname.startsWith("/p/")) return new Response(null, { status: 206 });
    return new Response(JSON.stringify({ sources: [original, url.searchParams.get("source") === "cloud"
      ? { ...cloud, status: "unavailable", message: "联通网盘暂未提供该视频的兼容播放源" } : cloud] }), { status: 200 });
  }) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  Object.defineProperty(video, "error", { configurable: true, value: { code: 4 } });
  await act(async () => { video.dispatchEvent(new Event("error")); });
  await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "original");
  assert.equal(playbackSourceState(video)?.error?.sourceID, "cloud");
  assert.equal(view.queryByRole("button", { name: "切换兼容播放" }), null);
  assert.equal(view.queryByRole("button", { name: "尝试原画播放" }), null);
  await act(async () => { view.getByRole("button", { name: "重新获取" }).click(); });
  await settle();
  assert.equal(urls.at(-1)?.searchParams.get("source"), "cloud");
  assert.equal(urls.at(-1)?.searchParams.get("refresh"), "1");
  assert.match(view.getByRole("alert").textContent ?? "", /原画播放暂不可用，联通网盘暂未提供/);
});

test("playback submenu keeps the root width and separates selected mode from the actual source", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  window.fetch = (async () => new Response(JSON.stringify({ sources: [original, cloud] }), { status: 200 })) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  const art = Artplayer.instances.find((instance) => instance.video === video)!;
  const setting = art.setting.find("playback-mode")!;
  assert.equal(setting.tooltip, "智能选择");
  assert.equal(setting.width, undefined);
  await act(async () => { art.setting.show = true; art.emit("resize"); });
  const menu = art.template.$setting;
  const rootWidth = menu.style.width;
  assert.equal(rootWidth, "172px");
  await act(async () => { setting.$item.click(); });
  assert.equal(menu.style.width, rootWidth);
  assert.match(menu.querySelector(".art-setting-panel.art-current .art-setting-item-back")?.textContent ?? "", /当前：原画播放/);
  assert.equal(menu.querySelectorAll('.art-setting-panel.art-current [data-name^="playback-mode-"]').length, 3);
});

test("updating the actual source keeps the playback submenu open and its mode label short", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp2t" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  const readyCloud = { ...cloud, status: "ready", url: "https://hls.example/index.m3u8" };
  let finish!: (response: Response) => void;
  const canPlayType = window.HTMLMediaElement.prototype.canPlayType;
  window.HTMLMediaElement.prototype.canPlayType = (type) => type === "video/mp2t" ? "" : "probably";
  window.fetch = (async (path) => {
    const url = new URL(String(path));
    if (url.searchParams.has("refresh")) return new Promise((resolve) => { finish = resolve; });
    return new Response(JSON.stringify({ sources: [original, url.searchParams.get("source") === "cloud" ? readyCloud : cloud] }), { status: 200 });
  }) as typeof fetch;
  try {
    const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
    await settle();
    const video = view.container.querySelector("video")!;
    const art = Artplayer.instances.find((instance) => instance.video === video)!;
    const setting = art.setting.find("playback-mode")!;
    await act(async () => { setting.$item.click(); });
    const menu = art.template.$setting;
    const activePanel = menu.querySelector(".art-setting-panel.art-current")!;
    assert.equal(setting.tooltip, "智能选择");
    assert.match(activePanel.textContent ?? "", /当前：兼容播放/);
    await act(async () => { video.dispatchEvent(new Event("error")); });
    assert.equal(menu.querySelector(".art-setting-panel.art-current"), activePanel);
    assert.match(activePanel.textContent ?? "", /正在获取播放源/);
    finish(new Response(JSON.stringify({ sources: [original, readyCloud] }), { status: 200 }));
    await settle();
    assert.equal(menu.querySelector(".art-setting-panel.art-current"), activePanel);
    assert.match(activePanel.textContent ?? "", /当前：兼容播放/);
    assert.equal(art.setting.find("playback-mode")!.tooltip, "智能选择");
  } finally {
    window.HTMLMediaElement.prototype.canPlayType = canPlayType;
  }
});
