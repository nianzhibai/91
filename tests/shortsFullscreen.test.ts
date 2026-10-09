import assert from "node:assert/strict";
import test from "node:test";
import {
  exitShortsFullscreen,
  isShortsFullscreen,
  observeShortsFullscreen,
  requestShortsFullscreen,
  requestShortsFullscreenOnEntry,
  supportsShortsFullscreen,
} from "../src/shorts/fullscreen";

function createHarness(prefixed = false) {
  const events = new EventTarget();
  const state = {
    path: "/shorts", requests: 0, exits: 0, reject: false, options: null as FullscreenOptions | null,
    touchOnly: false, viewportWidth: 1440,
  };
  const root: Record<string, unknown> = {};
  const doc = Object.assign(events, {
    documentElement: root,
    fullscreenEnabled: !prefixed,
    webkitFullscreenEnabled: prefixed,
    fullscreenElement: null as unknown,
    webkitFullscreenElement: null as unknown,
    async exitFullscreen() {
      state.exits++;
      doc.fullscreenElement = null;
      doc.webkitFullscreenElement = null;
      events.dispatchEvent(new Event("fullscreenchange"));
      events.dispatchEvent(new Event("webkitfullscreenchange"));
    },
  });
  root[prefixed ? "webkitRequestFullscreen" : "requestFullscreen"] = async (options: FullscreenOptions) => {
    state.requests++;
    state.options = options;
    if (state.reject) throw new Error("Fullscreen denied");
    if (prefixed) doc.webkitFullscreenElement = root;
    else doc.fullscreenElement = root;
    events.dispatchEvent(new Event(prefixed ? "webkitfullscreenchange" : "fullscreenchange"));
  };
  if (prefixed) {
    Object.assign(doc, { webkitExitFullscreen: doc.exitFullscreen, exitFullscreen: undefined });
  }
  const previousDocument = Object.getOwnPropertyDescriptor(globalThis, "document");
  const previousWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
  Object.defineProperty(globalThis, "document", { configurable: true, value: doc });
  const browserWindow = {
    location: { get pathname() { return state.path; } },
    get innerWidth() { return state.viewportWidth; },
    matchMedia(query: string) {
      return { matches: query === "(hover: none) and (pointer: coarse)" && state.touchOnly };
    },
  };
  Object.defineProperty(globalThis, "window", { configurable: true, value: browserWindow });
  return {
    doc, root, state, browserWindow,
    restore() {
      if (previousDocument) Object.defineProperty(globalThis, "document", previousDocument);
      else delete (globalThis as Record<string, unknown>).document;
      if (previousWindow) Object.defineProperty(globalThis, "window", previousWindow);
      else delete (globalThis as Record<string, unknown>).window;
    },
  };
}

test("desktop entry stays outside fullscreen at both wide and narrow window sizes", async () => {
  const h = createHarness();
  try {
    for (const width of [1440, 800, 320]) {
      h.state.viewportWidth = width;
      assert.equal(await requestShortsFullscreenOnEntry(), false);
      assert.equal(isShortsFullscreen(), false);
      assert.equal(h.state.requests, 0);
    }
    // 默认入口策略不限制用户主动请求全屏。
    assert.equal(await requestShortsFullscreen(), true);
    assert.equal(h.state.requests, 1);
  } finally { h.restore(); }
});

test("touch-only entry keeps capability-gated automatic fullscreen", async () => {
  for (const prefixed of [false, true]) {
    const h = createHarness(prefixed);
    try {
      h.state.touchOnly = true;
      assert.equal(await requestShortsFullscreenOnEntry(), true);
      assert.equal(h.state.requests, 1);
      assert.equal(isShortsFullscreen(), true);
    } finally { h.restore(); }
  }
});

test("entry without input capability detection defaults to ordinary playback", async () => {
  const h = createHarness();
  try {
    Reflect.deleteProperty(h.browserWindow, "matchMedia");
    assert.equal(await requestShortsFullscreenOnEntry(), false);
    assert.equal(h.state.requests, 0);
  } finally { h.restore(); }
});

test("native fullscreen requests hide browser navigation UI and repeated entry is idempotent", async () => {
  const h = createHarness();
  try {
    assert.equal(supportsShortsFullscreen(), true);
    assert.equal(await requestShortsFullscreen(), true);
    assert.deepEqual(h.state.options, { navigationUI: "hide" });
    assert.equal(await requestShortsFullscreen(), true);
    assert.equal(h.state.requests, 1);
    await exitShortsFullscreen();
    assert.equal(isShortsFullscreen(), false);
    assert.equal(h.state.exits, 1);
  } finally { h.restore(); }
});

test("prefixed fullscreen APIs support entry and exit without using native video fullscreen", async () => {
  const h = createHarness(true);
  try {
    assert.equal(supportsShortsFullscreen(), true);
    assert.equal(await requestShortsFullscreen(), true);
    await exitShortsFullscreen();
    assert.equal(isShortsFullscreen(), false);
    assert.equal(h.state.exits, 1);
  } finally { h.restore(); }
});

test("unsupported or rejected fullscreen leaves ordinary playback available for retry", async () => {
  const h = createHarness();
  try {
    h.doc.fullscreenEnabled = false;
    assert.equal(supportsShortsFullscreen(), false);
    assert.equal(await requestShortsFullscreen(), false);
    assert.equal(h.state.requests, 0);
    h.doc.fullscreenEnabled = true;
    h.state.reject = true;
    assert.equal(await requestShortsFullscreen(), false);
    assert.equal(isShortsFullscreen(), false);
    h.state.reject = false;
    assert.equal(await requestShortsFullscreen(), true);
  } finally { h.restore(); }
});

test("a fullscreen request completing after navigation does not fullscreen the destination", async () => {
  const h = createHarness();
  try {
    const request = requestShortsFullscreen();
    h.state.path = "/";
    assert.equal(await request, false);
    assert.equal(isShortsFullscreen(), false);
    assert.equal(h.state.exits, 1);
  } finally { h.restore(); }
});

test("shorts cleanup does not exit a different player's fullscreen element", async () => {
  const h = createHarness();
  try {
    h.doc.fullscreenElement = { tagName: "VIDEO" };
    assert.equal(isShortsFullscreen(), false);
    await exitShortsFullscreen();
    assert.equal(h.state.exits, 0);
  } finally { h.restore(); }
});

test("standard and prefixed change events report each fullscreen transition once and detach on cleanup", async () => {
  const h = createHarness();
  try {
    const changes: boolean[] = [];
    const stop = observeShortsFullscreen(active => changes.push(active));
    await requestShortsFullscreen();
    h.doc.dispatchEvent(new Event("webkitfullscreenchange"));
    await exitShortsFullscreen();
    assert.deepEqual(changes, [false, true, false]);
    stop();
    await requestShortsFullscreen();
    assert.deepEqual(changes, [false, true, false]);
  } finally { h.restore(); }
});

test("Escape exits shorts fullscreen when the browser delivers the key to the page", async () => {
  const h = createHarness();
  const stop = observeShortsFullscreen(() => {});
  try {
    await requestShortsFullscreen();
    const event = Object.assign(new Event("keydown", { cancelable: true }), { key: "Escape" });
    h.doc.dispatchEvent(event);
    assert.equal(event.defaultPrevented, true);
    assert.equal(isShortsFullscreen(), false);
    assert.equal(h.state.exits, 1);
  } finally {
    stop();
    h.restore();
  }
});

test("fullscreen Escape respects menus, other fullscreen owners and observer cleanup", async () => {
  const h = createHarness();
  const stop = observeShortsFullscreen(() => {});
  const pressEscape = () => Object.assign(new Event("keydown", { cancelable: true }), { key: "Escape" });
  try {
    await requestShortsFullscreen();
    const handled = pressEscape();
    handled.preventDefault();
    h.doc.dispatchEvent(handled);
    assert.equal(isShortsFullscreen(), true);
    assert.equal(h.state.exits, 0);
    h.doc.fullscreenElement = { tagName: "VIDEO" };
    const otherPlayer = pressEscape();
    h.doc.dispatchEvent(otherPlayer);
    assert.equal(otherPlayer.defaultPrevented, false);
    assert.equal(h.state.exits, 0);
    h.doc.fullscreenElement = h.root;
    stop();
    const detached = pressEscape();
    h.doc.dispatchEvent(detached);
    assert.equal(detached.defaultPrevented, false);
    assert.equal(isShortsFullscreen(), true);
  } finally {
    stop();
    h.restore();
  }
});
