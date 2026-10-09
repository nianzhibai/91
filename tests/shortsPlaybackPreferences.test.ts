import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { createElement } from "react";
import { create, act } from "react-test-renderer";
import { useShortsPlaybackPreferences } from "../src/shorts/useShortsPlaybackPreferences";
import { restoreShortsPlaybackRate } from "../src/shorts/playbackRate";

function setup(t: TestContext) {
  const browserDocument = { visibilityState: "visible" };
  const original = Object.getOwnPropertyDescriptor(globalThis, "document");
  Object.defineProperty(globalThis, "document", { configurable: true, value: browserDocument });
  t.after(() => {
    if (original) Object.defineProperty(globalThis, "document", original);
    else Reflect.deleteProperty(globalThis, "document");
  });
  const video = Object.assign(new EventTarget(), {
    defaultPlaybackRate: 1, playbackRate: 1, loop: true, ended: false, volume: 1,
  });
  let advances = 0;
  let options = {
    getVideoElement: () => video as unknown as HTMLVideoElement,
    shouldMount: true,
    usesSharedVideo: false,
    isActive: true,
    canGoNext: true,
    autoAdvance: false,
    playbackRate: 1,
    volume: undefined as number | undefined,
    isTemporarilyAccelerated: false,
    onAdvance: () => { advances++; },
  };
  function Probe() { useShortsPlaybackPreferences(options); return null; }
  let renderer: ReturnType<typeof create>;
  act(() => { renderer = create(createElement(Probe)); });
  t.after(() => act(() => renderer.unmount()));
  return {
    video, browserDocument,
    get advances() { return advances; },
    update(next: Partial<typeof options>) {
      options = { ...options, ...next };
      act(() => renderer.update(createElement(Probe)));
    },
    end() { video.ended = true; video.dispatchEvent(new Event("ended")); },
    unmount() { act(() => renderer.unmount()); },
  };
}

test("continuous playback advances only the active visible video at its end", t => {
  const h = setup(t);
  h.end();
  assert.equal(h.advances, 0);
  assert.equal(h.video.loop, true);
  h.update({ autoAdvance: true });
  assert.equal(h.video.loop, false);
  h.video.ended = false;
  h.video.dispatchEvent(new Event("ended"));
  assert.equal(h.advances, 0);
  h.end();
  assert.equal(h.advances, 1);
  h.update({ isActive: false });
  h.end();
  assert.equal(h.advances, 1);
  h.update({ isActive: true });
  h.browserDocument.visibilityState = "hidden";
  h.end();
  assert.equal(h.advances, 1);
});

test("continuous playback loops while the queue loads and responds when the next video arrives", t => {
  const h = setup(t);
  h.update({ autoAdvance: true, canGoNext: false });
  assert.equal(h.video.loop, true);
  h.end();
  assert.equal(h.advances, 0);
  h.update({ canGoNext: true });
  assert.equal(h.video.loop, false);
  h.end();
  assert.equal(h.advances, 1);
  h.update({ autoAdvance: false });
  assert.equal(h.video.loop, true);
  h.end();
  assert.equal(h.advances, 1);
});

test("selected speed survives a temporary boost and applies to the next media element", t => {
  const h = setup(t);
  h.update({ playbackRate: 1.5 });
  assert.equal(h.video.playbackRate, 1.5);
  h.video.playbackRate = 2;
  h.update({ isTemporarilyAccelerated: true, playbackRate: 0.75 });
  assert.equal(h.video.playbackRate, 2);
  restoreShortsPlaybackRate(h.video as unknown as HTMLVideoElement);
  assert.equal(h.video.playbackRate, 0.75);
  const nextVideo = Object.assign(new EventTarget(), {
    defaultPlaybackRate: 1, playbackRate: 1, loop: true, ended: false,
  });
  h.update({
    getVideoElement: () => nextVideo as unknown as HTMLVideoElement,
    isTemporarilyAccelerated: false,
  });
  assert.equal(nextVideo.playbackRate, 0.75);
});

test("desktop preferences leave iOS shared-video loop recovery unchanged and remove listeners on unmount", t => {
  const h = setup(t);
  h.update({ usesSharedVideo: true, playbackRate: 1.5, autoAdvance: true });
  assert.equal(h.video.playbackRate, 1);
  assert.equal(h.video.loop, true);
  h.end();
  assert.equal(h.advances, 0);
  h.update({ usesSharedVideo: false });
  h.unmount();
  h.end();
  assert.equal(h.advances, 0);
});

test("desktop volume follows the mounted media element and returns to system loudness on mobile", t => {
  const h = setup(t);
  h.update({ volume: 0.35 });
  assert.equal(h.video.volume, 0.35);
  const nextVideo = Object.assign(new EventTarget(), {
    defaultPlaybackRate: 1, playbackRate: 1, loop: true, ended: false, volume: 1,
  });
  h.update({ getVideoElement: () => nextVideo as unknown as HTMLVideoElement });
  assert.equal(nextVideo.volume, 0.35);
  h.update({ volume: 0 });
  assert.equal(nextVideo.volume, 0);
  h.update({ volume: undefined });
  assert.equal(nextVideo.volume, 1);
});

test("mobile and shared iOS media do not receive desktop volume changes", t => {
  const h = setup(t);
  let writes = 0;
  Object.defineProperty(h.video, "volume", {
    get: () => 1,
    set: () => { writes++; },
  });
  h.update({ playbackRate: 1.5 });
  assert.equal(writes, 0);
  h.update({ usesSharedVideo: true, volume: 0.2 });
  assert.equal(writes, 0);
});
