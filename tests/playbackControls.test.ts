import "./helpers/dom-environment.ts";
import assert from "node:assert/strict";
import test, { afterEach, type TestContext } from "node:test";
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

function visibilityPlayer(t: TestContext, touch = false, readyState = 4) {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  if (touch) t.mock.method(window, "matchMedia", (query: string) => Object.assign(new window.EventTarget(), {
    matches: query.includes("pointer: coarse"), media: query, onchange: null,
    addListener() {}, removeListener() {},
  }));
  const view = render(createElement(VideoPlayer, { src: "/sample.mp4", poster: "", title: "Controls" }));
  const art = Artplayer.instances.at(-1)!;
  art.video.currentTime = 1;
  Object.defineProperty(art.video, "readyState", { configurable: true, value: readyState });
  if (readyState >= art.video.HAVE_FUTURE_DATA) art.video.dispatchEvent(new Event("canplay"));
  const tick = async (milliseconds: number) => { await act(async () => t.mock.timers.tick(milliseconds)); };
  const pointer = (target: EventTarget, type: string, time?: number) => {
    const event = new Event(type, { bubbles: true });
    Object.assign(event, { pointerId: 1, pointerType: touch ? "touch" : "mouse", button: 0, clientX: 0, clientY: 0 });
    if (time !== undefined) Object.defineProperty(event, "timeStamp", { value: time });
    target.dispatchEvent(event);
  };
  const tap = () => art.emit("click", new Event("click"));
  return { view, art, tick, pointer, tap };
}

test("playing controls fully hide after three seconds without depending on media timeupdate", async (t) => {
  const { art, tick } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  await tick(2999);
  assert.equal(art.controls.show, true);
  await tick(1);
  assert.equal(art.controls.show, false);
  assert.equal(art.template.$bottom.hasAttribute("inert"), true);
  assert.equal(art.template.$bottom.getAttribute("aria-hidden"), "true");
  assert.equal(art.template.$player.classList.contains("art-mini-progress-bar"), false);
});

test("mouse movement restarts the idle period and hovering controls keeps them visible", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  await tick(2000);
  pointer(art.video, "pointermove");
  await tick(2000);
  assert.equal(art.controls.show, true);
  pointer(art.template.$controls, "pointermove");
  await tick(10_000);
  assert.equal(art.controls.show, true);
  pointer(art.video, "pointermove");
  await tick(3000);
  assert.equal(art.controls.show, false);
  pointer(art.video, "pointermove");
  assert.equal(art.controls.show, true);
});

test("a progress drag stays visible across mouse leave and hides when released outside", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  pointer(art.template.$progress, "pointerdown");
  await tick(10_000);
  assert.equal(art.controls.show, true);
  pointer(art.template.$player, "pointerleave");
  await tick(10_000);
  assert.equal(art.controls.show, true);
  pointer(document, "pointerup");
  assert.equal(art.controls.show, false);
  await tick(3000);
  assert.equal(art.controls.show, false);
});

for (const paused of [true, false]) {
  test(`mouse leave immediately hides ${paused ? "paused" : "playing"} controls until re-entry`, async (t) => {
    const { art, tick, pointer } = visibilityPlayer(t);
    if (!paused) await act(async () => { await art.video.play(); });
    pointer(art.video, "pointermove");
    pointer(art.template.$player, "pointerleave");
    assert.equal(art.controls.show, false);
    assert.equal(art.template.$bottom.hasAttribute("inert"), true);
    await tick(10_000);
    assert.equal(art.controls.show, false);
    pointer(art.template.$player, "pointerenter");
    assert.equal(art.controls.show, true);
    await tick(3000);
    assert.equal(art.controls.show, false);
  });
}

test("mouse leave keeps an open settings menu usable and hides after it closes", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t);
  art.setting.show = true;
  pointer(art.template.$player, "pointerleave");
  assert.equal(art.controls.show, true);
  assert.equal(art.setting.show, true);
  await tick(10_000);
  assert.equal(art.controls.show, true);
  art.setting.show = false;
  assert.equal(art.controls.show, false);
  await tick(3000);
  assert.equal(art.controls.show, false);
});

test("mouse leave lets a held keyboard shortcut finish before hiding", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t);
  art.emit("hotkey", new KeyboardEvent("keydown", { code: "ArrowRight" }));
  pointer(art.template.$player, "pointerleave");
  await tick(10_000);
  assert.equal(art.controls.show, true);
  document.dispatchEvent(new KeyboardEvent("keyup", { code: "ArrowRight" }));
  assert.equal(art.controls.show, false);
  await tick(3000);
  assert.equal(art.controls.show, false);
});

test("keyboard shortcuts can reveal controls after the mouse has left", async (t) => {
  const { art, pointer } = visibilityPlayer(t);
  pointer(art.template.$player, "pointerleave");
  assert.equal(art.controls.show, false);
  art.emit("hotkey", new KeyboardEvent("keydown", { code: "ArrowRight" }));
  assert.equal(art.controls.show, true);
  document.dispatchEvent(new KeyboardEvent("keyup", { code: "ArrowRight" }));
  assert.equal(art.controls.show, false);
});

test("playback actions can reveal controls after mouse leave without keeping paused controls permanently visible", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  pointer(art.template.$player, "pointerleave");
  assert.equal(art.controls.show, false);
  art.video.pause();
  assert.equal(art.controls.show, true);
  await tick(3000);
  assert.equal(art.controls.show, false);
});

test("keyboard focus is retained across mouse leave until focus moves away", async (t) => {
  const { art, tick } = visibilityPlayer(t, true);
  document.dispatchEvent(new KeyboardEvent("keydown", { code: "Tab" }));
  const control = art.controls.orientationToggle!;
  control.focus();
  const leave = new Event("pointerleave");
  Object.assign(leave, { pointerType: "mouse" });
  art.template.$player.dispatchEvent(leave);
  await tick(10_000);
  assert.equal(art.controls.show, true);
  control.blur();
  assert.equal(art.controls.show, false);
});

test("an open settings menu pins controls and closing it starts a fresh idle period", async (t) => {
  const { art, tick } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  art.setting.show = true;
  await tick(10_000);
  assert.equal(art.controls.show, true);
  art.setting.show = false;
  await tick(2999);
  assert.equal(art.controls.show, true);
  await tick(1);
  assert.equal(art.controls.show, false);
});

test("touch controls use three seconds and a tap can dismiss them without pausing playback", async (t) => {
  const { art, tick, tap } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  await tick(2999);
  assert.equal(art.controls.show, true);
  await tick(1);
  assert.equal(art.controls.show, false);
  tap();
  assert.equal(art.controls.show, true);
  assert.equal(art.video.paused, false);
  tap();
  assert.equal(art.controls.show, false);
  assert.equal(art.video.paused, false);
});

test("a touch interaction releases stale mouse hover on hybrid devices", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  const mouse = new Event("pointermove", { bubbles: true });
  Object.assign(mouse, { pointerId: 2, pointerType: "mouse", button: 0 });
  art.template.$controls.dispatchEvent(mouse);
  await tick(5000);
  assert.equal(art.controls.show, true);
  pointer(art.video, "pointerdown");
  pointer(document, "pointerup");
  await tick(3000);
  assert.equal(art.controls.show, false);
});

test("releasing a long touch does not dismiss the overlay as if it were a tap", async (t) => {
  const { art, tick, pointer, tap } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  pointer(art.video, "pointerdown", 0);
  await tick(800);
  pointer(document, "pointerup", 800);
  tap();
  assert.equal(art.controls.show, true);
  await tick(2999);
  assert.equal(art.controls.show, true);
  await tick(1);
  assert.equal(art.controls.show, false);
});

test("tapping outside settings closes the menu and leaves time for another action", async (t) => {
  const { art, tick, tap } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  art.setting.show = true;
  tap();
  assert.equal(art.setting.show, false);
  assert.equal(art.controls.show, true);
  await tick(3000);
  assert.equal(art.controls.show, false);
});

test("a user seek reveals the timeline while background progress updates leave it hidden", async (t) => {
  const { art, tick } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  await tick(3000);
  art.emit("setBar", "played", 0.5);
  assert.equal(art.controls.show, false);
  const gesture = new Event("touchmove");
  art.video.dispatchEvent(gesture);
  art.emit("setBar", "played", 0.5, gesture);
  assert.equal(art.controls.show, true);
});

test("locking hides playback controls but allows taps to reveal the unlock button", async (t) => {
  const { art, tick, tap } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  art.isLock = true;
  art.emit("lock", true);
  assert.equal(art.template.$bottom.hasAttribute("inert"), true);
  await tick(3000);
  assert.equal(art.controls.show, false);
  tap();
  assert.equal(art.controls.show, true);
  assert.equal(art.template.$bottom.hasAttribute("inert"), true);
  art.isLock = false;
  art.emit("lock", false);
  assert.equal(art.template.$bottom.hasAttribute("inert"), false);
});

test("paused touch controls use the same idle delay and can also be dismissed manually", async (t) => {
  const { art, tick, tap } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  await tick(3000);
  art.video.pause();
  assert.equal(art.controls.show, true);
  await tick(2999);
  assert.equal(art.controls.show, true);
  await tick(1);
  assert.equal(art.controls.show, false);
  tap();
  assert.equal(art.controls.show, true);
  tap();
  assert.equal(art.controls.show, false);
  assert.equal(art.video.paused, true);
  assert.equal(art.template.$mask.hasAttribute("inert"), true);
  tap();
  assert.equal(art.controls.show, true);
});

test("background buffering and source load events do not reopen hidden controls", async (t) => {
  const { art, tick } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  await tick(3000);
  for (const name of ["waiting", "loadstart", "loadeddata", "canplay", "playing"]) {
    await act(async () => art.video.dispatchEvent(new Event(name)));
    assert.equal(art.controls.show, false, name);
  }
});

test("initial loading keeps controls visible through mouse movement, leave and idle time", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t, false, 0);
  art.video.dispatchEvent(new Event("loadstart"));
  assert.equal(art.loading.show, true);
  await tick(10_000);
  assert.equal(art.controls.show, true);
  pointer(art.template.$player, "pointerenter");
  pointer(art.video, "pointermove");
  pointer(art.template.$player, "pointerleave");
  assert.equal(art.controls.show, true);
  await tick(10_000);
  assert.equal(art.controls.show, true);
  assert.equal(art.loading.show, true);
  assert.equal(art.template.$bottom.hasAttribute("inert"), false);
});

for (const readyEvent of ["canplay", "playing"]) {
  test(`initial loading keeps controls visible after ${readyEvent} until another interaction`, async (t) => {
    const { art, tick, pointer } = visibilityPlayer(t, false, 0);
    art.video.dispatchEvent(new Event("loadstart"));
    pointer(art.template.$player, "pointerenter");
    pointer(art.template.$player, "pointerleave");
    if (readyEvent === "playing") await act(async () => { await art.video.play(); });
    art.video.dispatchEvent(new Event("loadedmetadata"));
    Object.defineProperty(art.video, "readyState", { configurable: true, value: 2 });
    art.video.dispatchEvent(new Event("loadeddata"));
    await tick(10_000);
    assert.equal(art.controls.show, true);
    Object.defineProperty(art.video, "readyState", { configurable: true, value: 4 });
    art.video.dispatchEvent(new Event(readyEvent));
    assert.equal(art.controls.show, true);
    await tick(10_000);
    assert.equal(art.controls.show, true);
    assert.equal(art.template.$bottom.hasAttribute("inert"), false);
    pointer(art.video, "pointermove");
    assert.equal(art.controls.show, true);
    await tick(3000);
    assert.equal(art.controls.show, false);
    pointer(art.video, "pointermove");
    pointer(art.template.$player, "pointerleave");
    assert.equal(art.controls.show, false);
  });
}

for (const touch of [false, true]) {
  test(`${touch ? "touch" : "mouse"} controls remain usable during buffering and hide independently of its loading indicator`, async (t) => {
    const { art, tick, pointer, tap } = visibilityPlayer(t, touch);
    await act(async () => { await art.video.play(); });
    await tick(3000);
    art.video.dispatchEvent(new Event("waiting"));
    assert.equal(art.loading.show, true);
    assert.equal(art.controls.show, false);
    if (touch) tap(); else pointer(art.video, "pointermove");
    assert.equal(art.controls.show, true);
    await tick(3000);
    assert.equal(art.controls.show, false);
    assert.equal(art.loading.show, true);
    art.video.dispatchEvent(new Event("canplay"));
    assert.equal(art.controls.show, false);
  });
}

test("loading and recovery do not restart an existing controls idle deadline", async (t) => {
  const { art, tick } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  await tick(2000);
  art.video.dispatchEvent(new Event("waiting"));
  await tick(500);
  art.video.dispatchEvent(new Event("canplay"));
  await tick(499);
  assert.equal(art.controls.show, true);
  await tick(1);
  assert.equal(art.controls.show, false);
});

test("buffering and recovery do not interrupt a progress drag outside the player", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  pointer(art.template.$progress, "pointerdown");
  art.video.dispatchEvent(new Event("waiting"));
  pointer(art.template.$player, "pointerleave");
  await tick(10_000);
  assert.equal(art.controls.show, true);
  art.video.dispatchEvent(new Event("canplay"));
  assert.equal(art.controls.show, true);
  pointer(document, "pointerup");
  assert.equal(art.controls.show, false);
});

test("keyboard focus holds controls until focus leaves, without pinning pointer activation", async (t) => {
  const { art, tick, pointer } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  document.dispatchEvent(new KeyboardEvent("keydown", { code: "Tab" }));
  const control = art.controls.orientationToggle!;
  control.focus();
  await tick(10_000);
  assert.equal(art.controls.show, true);
  pointer(control, "pointerdown");
  pointer(document, "pointerup");
  await tick(3000);
  assert.equal(art.controls.show, false);
});

test("held playback shortcuts keep controls visible until key release", async (t) => {
  const { art, tick } = visibilityPlayer(t);
  await act(async () => { await art.video.play(); });
  art.emit("hotkey", new KeyboardEvent("keydown", { code: "ArrowRight" }));
  await tick(10_000);
  assert.equal(art.controls.show, true);
  document.dispatchEvent(new KeyboardEvent("keyup", { code: "ArrowRight" }));
  await tick(3000);
  assert.equal(art.controls.show, false);
});

test("entering and leaving web fullscreen follows the same visibility timeout", async (t) => {
  const { art, tick } = visibilityPlayer(t, true);
  await act(async () => { await art.video.play(); });
  await tick(3000);
  art.fullscreenWeb = true;
  assert.equal(art.controls.show, true);
  await tick(3000);
  assert.equal(art.controls.show, false);
  art.fullscreenWeb = false;
  assert.equal(art.controls.show, true);
  await tick(3000);
  assert.equal(art.controls.show, false);
});

async function tripleScreenPlayer(src = "/p/stream/115/file", compatibleProtocol: "file" | "hls" = "hls") {
  const requests: string[] = [];
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: src, mediaType: "video/mp4" };
  const cloud = { id: compatibleProtocol === "file" ? "cloud-22" : "cloud", label: "兼容播放", kind: "transcoded", protocol: compatibleProtocol, status: "unknown", mediaType: compatibleProtocol === "file" ? "video/mp4" : undefined, resolveUrl: "/api/video/one/playback?source=cloud" };
  const compatibleURL = compatibleProtocol === "file"
    ? src.startsWith("/p/share/") ? src.replace(/\/stream$/, "/playback/cloud-22") : "/p/playback/one/cloud-22"
    : "/p/hls/one/session/index.m3u8";
  window.fetch = (async (path) => {
    requests.push(String(path));
    if (String(path).includes("/p/stream/")) return new Response(null, { status: 206 });
    return new Response(JSON.stringify({ sources: [original, String(path).includes("source=cloud")
      ? { ...cloud, status: "ready", url: compatibleURL } : cloud] }), { status: 200 });
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

for (const shared of [false, true]) {
  test(`${shared ? "shared" : "signed-in"} progressive playback switches between 302 delivery and triple-screen relay`, async (t) => {
    const enable = t.mock.method(TripleScreenRenderer.prototype, "enable", () => true);
    const src = shared ? "/p/share/share-token/stream" : "/p/stream/google/file";
    const env = await tripleScreenPlayer(src, "file");
    await act(async () => { selectPlaybackMode(env.video, "compatible"); });
    await settle(); await env.frameReady();
    const direct = env.video.src;
    env.video.currentTime = 29;
    await act(async () => { void env.video.play(); env.control.click(); });
    await settle(); await env.frameReady();
    assert.equal(playbackSourceState(env.video)?.mode, "compatible");
    assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud-22");
    assert.equal(new URL(env.video.src).searchParams.get("tripleScreenRelay"), "1");
    assert.equal(new URL(env.video.src).pathname, new URL(direct).pathname);
    assert.equal(env.video.currentTime, 29);
    assert.equal(env.video.paused, false);
    assert.equal(enable.mock.callCount(), 1);
    await act(async () => { env.control.click(); });
    await settle(); await env.frameReady();
    assert.equal(env.video.src, direct);
    assert.equal(env.video.currentTime, 29);
    assert.equal(playbackSourceState(env.video)?.activeSource?.id, "cloud-22");
  });
}

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

test("playback submenu follows the active source while automatic recovery stays internal", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  window.fetch = (async () => new Response(JSON.stringify({ sources: [original, cloud] }), { status: 200 })) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  const art = Artplayer.instances.find((instance) => instance.video === video)!;
  const setting = art.setting.find("playback-mode")!;
  assert.equal(setting.tooltip, "原画播放");
  assert.equal(setting.width, undefined);
  await act(async () => { art.setting.show = true; art.emit("resize"); });
  const menu = art.template.$setting;
  const rootWidth = menu.style.width;
  assert.equal(rootWidth, "172px");
  await act(async () => { setting.$item.click(); });
  assert.equal(menu.style.width, rootWidth);
  assert.equal(menu.querySelector(".art-setting-panel.art-current .art-setting-item-back")?.textContent?.trim(), "播放方式");
  assert.equal(menu.querySelectorAll('.art-setting-panel.art-current [data-name^="playback-mode-"]').length, 2);
  assert.equal(menu.querySelector('[data-name="playback-mode-auto"]'), null);
  assert.equal(menu.querySelector('[data-name="playback-mode-original"]')?.classList.contains("art-current"), true);
  assert.equal(playbackSourceState(video)?.mode, "auto");
  await act(async () => { menu.querySelector<HTMLElement>('[data-name="playback-mode-original"]')!.click(); });
  assert.equal(playbackSourceState(video)?.mode, "original");
});

test("refreshing the actual source keeps a plain playback submenu header and current selection", async () => {
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
    assert.equal(setting.tooltip, "兼容播放");
    assert.equal(activePanel.querySelector(".art-setting-item-back")?.textContent?.trim(), "播放方式");
    assert.equal(activePanel.querySelector('[data-name="playback-mode-compatible"]')?.classList.contains("art-current"), true);
    assert.equal(activePanel.querySelector(".video-player__playback-source-status"), null);
    await act(async () => { video.dispatchEvent(new Event("error")); });
    assert.equal(menu.querySelector(".art-setting-panel.art-current"), activePanel);
    assert.equal(activePanel.querySelector(".art-setting-item-back")?.textContent?.trim(), "播放方式");
    finish(new Response(JSON.stringify({ sources: [original, readyCloud] }), { status: 200 }));
    await settle();
    assert.equal(menu.querySelector(".art-setting-panel.art-current"), activePanel);
    assert.equal(activePanel.querySelector(".art-setting-item-back")?.textContent?.trim(), "播放方式");
    assert.equal(activePanel.querySelector('[data-name="playback-mode-compatible"]')?.classList.contains("art-current"), true);
    assert.equal(activePanel.querySelector(".video-player__playback-source-status"), null);
    assert.equal(art.setting.find("playback-mode")!.tooltip, "兼容播放");
  } finally {
    window.HTMLMediaElement.prototype.canPlayType = canPlayType;
  }
});

test("an automatic fallback selects compatibility in the menu without fixing the strategy", async () => {
  const env = await tripleScreenPlayer();
  const art = Artplayer.instances.find((instance) => instance.video === env.video)!;
  await act(async () => { art.setting.show = true; art.setting.find("playback-mode")!.$item.click(); });
  assert.equal(art.template.$setting.querySelector('[data-name="playback-mode-original"]')?.classList.contains("art-current"), true);
  Object.defineProperty(env.video, "error", { configurable: true, value: { code: 3 } });
  await act(async () => { env.video.dispatchEvent(new Event("error")); });
  await settle();
  const state = playbackSourceState(env.video)!;
  assert.equal(state.mode, "auto");
  assert.equal(state.activeSource?.kind, "transcoded");
  assert.equal(art.setting.show, true);
  const setting = art.setting.find("playback-mode")!;
  assert.equal(setting.tooltip, "兼容播放");
  assert.equal(setting.selector?.find((item) => item.value === "compatible")?.default, true);
  assert.equal(setting.selector?.find((item) => item.value === "original")?.default, false);
  assert.equal(setting.selector?.some((item) => item.value === "auto"), false);
  assert.equal(art.template.$setting.querySelector(".video-player__playback-source-status"), null);
});

test("rendition menu follows active HLS sources and hides when switching back to the original", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file", mediaType: "video/mp4" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  const high = { ...cloud, id: "cloud-high", label: "高清", status: "ready", url: "/p/hls/high/index.m3u8" };
  const low = { ...cloud, id: "cloud-low", label: "流畅", status: "ready", url: "/p/hls/low/index.m3u8" };
  window.fetch = (async (path) => {
    const url = new URL(String(path));
    if (url.pathname.startsWith("/p/")) return new Response(null, { status: 206 });
    return new Response(JSON.stringify({ sources: url.searchParams.has("source") ? [original, high, low] : [original, cloud] }), { status: 200 });
  }) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle();
  const video = view.container.querySelector("video")!;
  const art = Artplayer.instances.find((instance) => instance.video === video)!;
  await act(async () => { selectPlaybackMode(video, "compatible"); });
  await settle();
  let setting = art.setting.find("playback-rendition")!;
  assert.ok(setting);
  assert.equal(setting.tooltip, "高清");
  assert.equal(setting.selector?.length, 2);
  assert.deepEqual(setting.selector?.map((item) => item.value), ["cloud-high", "cloud-low"]);
  assert.equal(setting.selector?.find((item) => item.value === "cloud-high")?.default, true);
  assert.equal(playbackSourceState(video)?.selectedSourceID, null);

  // Source metadata and playback mode stay the same; only the active rendition
  // changes, so the menu must follow its ID instead of the manual preference.
  Object.defineProperty(video, "error", { configurable: true, value: { code: 3 } });
  await act(async () => { video.dispatchEvent(new Event("error")); });
  await settle();
  setting = art.setting.find("playback-rendition")!;
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-low");
  assert.equal(playbackSourceState(video)?.selectedSourceID, null);
  assert.equal(setting.tooltip, "流畅");
  assert.equal(setting.selector?.find((item) => item.value === "cloud-high")?.default, false);
  assert.equal(setting.selector?.find((item) => item.value === "cloud-low")?.default, true);

  Object.defineProperty(video, "error", { configurable: true, value: null });
  await act(async () => { setting.$item.click(); });
  assert.equal(art.template.$setting.querySelector('[data-name="rendition-auto"]'), null);
  await act(async () => { art.template.$setting.querySelector<HTMLElement>('[data-name="rendition-cloud-low"]')!.click(); });
  assert.equal(playbackSourceState(video)?.selectedSourceID, "cloud-low");

  await act(async () => { selectPlaybackMode(video, "original"); });
  await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.kind, "original");
  assert.ok(!art.setting.find("playback-rendition"));
  assert.equal(art.template.$setting.querySelector('[data-name="playback-rendition"]'), null);

  await act(async () => { selectPlaybackMode(video, "compatible"); });
  await settle();
  assert.equal(playbackSourceState(video)?.activeSource?.id, "cloud-high");
  const restored = art.setting.find("playback-rendition")!;
  assert.ok(restored);
  assert.equal(restored.tooltip, "高清");
  assert.equal(restored.selector?.find((item) => item.value === "cloud-high")?.default, true);
});

test("original playback labels show the observed Quark resolver and update when it changes", async () => {
  const original = { id: "original", label: "原画播放", kind: "original", protocol: "file", status: "ready", url: "/p/stream/drive/file?playbackId=one", mediaType: "video/mp4", sourceInfoUrl: "/api/video/one/playback?source=original&playbackId=one" };
  const cloud = { id: "cloud", label: "兼容播放", kind: "transcoded", protocol: "hls", status: "unknown", resolveUrl: "/api/video/one/playback?source=cloud" };
  let urlSource = "quark-raw";
  window.fetch = (async (path) => new Response(JSON.stringify({ sources: [new URL(String(path)).searchParams.has("playbackId") ? { ...original, urlSource } : original, cloud] }), { status: 200 })) as typeof fetch;
  const view = render(createElement(VideoPlayer, { src: original.url, playbackUrl: "/api/video/one/playback", title: "Example", poster: "" }));
  await settle(); const video = view.container.querySelector("video")!;
  const art = Artplayer.instances.find((instance) => instance.video === video)!;
  assert.equal(art.setting.find("playback-mode")!.tooltip, "原画播放");
  await act(async () => { video.dispatchEvent(new Event("loadedmetadata")); }); await settle();
  let setting = art.setting.find("playback-mode")!;
  assert.equal(setting.tooltip, "原画·RAW");
  assert.equal(setting.selector?.find((item) => item.value === "original")?.html, "原画播放（RAW接口）");
  urlSource = "quark-download";
  await act(async () => { video.dispatchEvent(new Event("seeked")); }); await settle();
  setting = art.setting.find("playback-mode")!;
  assert.equal(setting.tooltip, "原画·下载");
  assert.equal(setting.selector?.find((item) => item.value === "original")?.html, "原画播放（下载接口）");
  assert.equal(playbackSourceState(video)?.mode, "auto");
});
