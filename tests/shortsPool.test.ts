import assert from "node:assert/strict";
import test from "node:test";
import {
  ShortsPlayerPool,
  type ShortsPoolMediaItem,
  type ShortsPoolSourceAdapter,
} from "../sdk/src/shorts/playerPool";

type FakeElement = {
  parentElement: FakeSlot | null;
  className: string;
  loop: boolean;
  playsInline: boolean;
  disablePictureInPicture: boolean;
  preload: string;
  autoplay: boolean;
  muted: boolean;
  defaultMuted: boolean;
  volume: number;
  paused: boolean;
  ended: boolean;
  readyState: number;
  currentTime: number;
  defaultPlaybackRate: number;
  playbackRate: number;
  poster: string;
  dataset: Record<string, string | undefined>;
  attrs: Map<string, string>;
  listeners: Map<string, Set<() => void>>;
  buffered: { length: number; start: (i: number) => number; end: (i: number) => number };
  playCalls: number;
  pauseCalls: number;
  loadCalls: number;
  setAttribute: (name: string, value: string) => void;
  getAttribute: (name: string) => string | null;
  removeAttribute: (name: string) => void;
  addEventListener: (name: string, fn: () => void) => void;
  removeEventListener: (name: string, fn: () => void) => void;
  play: () => Promise<void>;
  pause: () => void;
  load: () => void;
  remove: () => void;
};

type FakeSlot = {
  children: FakeElement[];
  appendChild: (el: FakeElement) => FakeElement;
};

function makeSlot(): FakeSlot {
  const slot: FakeSlot = {
    children: [],
    appendChild(el: FakeElement) {
      if (el.parentElement && el.parentElement !== slot) {
        const idx = el.parentElement.children.indexOf(el);
        if (idx >= 0) el.parentElement.children.splice(idx, 1);
      }
      if (!slot.children.includes(el)) slot.children.push(el);
      el.parentElement = slot;
      return el;
    },
  };
  return slot;
}

function makeFakeVideo(): FakeElement {
  const el: FakeElement = {
    parentElement: null,
    className: "",
    loop: false,
    playsInline: true,
    disablePictureInPicture: true,
    preload: "none",
    autoplay: false,
    muted: false,
    defaultMuted: false,
    volume: 1,
    paused: true,
    ended: false,
    readyState: 0,
    currentTime: 0,
    defaultPlaybackRate: 1,
    playbackRate: 1,
    poster: "",
    dataset: {},
    attrs: new Map(),
    listeners: new Map(),
    buffered: { length: 0, start: () => 0, end: () => 0 },
    playCalls: 0,
    pauseCalls: 0,
    loadCalls: 0,
    setAttribute(name, value) {
      el.attrs.set(name, value);
    },
    getAttribute(name) {
      return el.attrs.get(name) ?? null;
    },
    removeAttribute(name) {
      el.attrs.delete(name);
    },
    addEventListener(name, fn) {
      if (!el.listeners.has(name)) el.listeners.set(name, new Set());
      el.listeners.get(name)!.add(fn);
    },
    removeEventListener(name, fn) {
      el.listeners.get(name)?.delete(fn);
    },
    play() {
      el.playCalls += 1;
      el.paused = false;
      return Promise.resolve();
    },
    pause() {
      el.pauseCalls += 1;
      el.paused = true;
    },
    load() {
      el.loadCalls += 1;
    },
    remove() {
      if (el.parentElement) {
        const idx = el.parentElement.children.indexOf(el);
        if (idx >= 0) el.parentElement.children.splice(idx, 1);
        el.parentElement = null;
      }
    },
  };
  return el;
}

function makeItems(count: number): ShortsPoolMediaItem[] {
  return Array.from({ length: count }, (_, i) => ({
    id: `vid-${i}`,
    videoSrc: `/p/stream/drive/vid-${i}`,
    poster: `/p/thumb/vid-${i}`,
  }));
}

test("ShortsPlayerPool iOS mode ping-pongs two persistent elements and preserves preloaded video", () => {
  const created: FakeElement[] = [];
  const attached: Array<{ src: string; active?: boolean }> = [];
  const disposed: FakeElement[] = [];
  const parking = makeSlot();
  const adapter: ShortsPoolSourceAdapter = {
    attach(_video, src, _playbackUrl, options) {
      attached.push({ src, active: options?.active });
    },
    setActive() {},
    dispose(video) {
      disposed.push(video as unknown as FakeElement);
    },
  };

  const pool = new ShortsPlayerPool({
    iosShared: true,
    getParkingContainer: () => parking as unknown as HTMLElement,
    sourceAdapter: adapter,
    createVideoElement: () => {
      const el = makeFakeVideo();
      created.push(el);
      return el as unknown as HTMLVideoElement;
    },
  });

  const items = makeItems(4);
  const slots = items.map(() => makeSlot());
  slots.forEach((slot, index) => pool.registerSlot(index, slot as unknown as HTMLElement));

  const firstActive = pool.syncActive({ items, activeIndex: 0, muted: false });
  assert.equal(created.length, 2, "allocates active and standby eagerly for gesture audio unlock");
  assert.equal(pool.getActiveVideo(), firstActive);

  pool.syncStandby({
    items,
    activeIndex: 0,
    browseDirection: 1,
    activeReadyForPreload: true,
    cacheWindow: { start: 0, end: 0 },
    cachedIds: new Set(),
    muted: false,
  });
  const standbyBeforeSwap = pool.getStandbyVideo();
  assert.equal((standbyBeforeSwap as unknown as FakeElement).preload, "auto");
  assert.equal((standbyBeforeSwap as unknown as FakeElement).dataset.shortsVideoId, "vid-1");

  // Swiping to slide 1 promotes standby directly without re-attaching source.
  const attachCountBeforeSwap = attached.length;
  const promoted = pool.syncActive({ items, activeIndex: 1, muted: false });
  assert.equal(promoted, standbyBeforeSwap);
  assert.equal(attached.length, attachCountBeforeSwap, "promoted video keeps existing buffered source");
  assert.equal(pool.getStandbyVideo(), firstActive);

  // Queue reset parks both persistent elements without destroying them.
  pool.resetForQueueClear();
  assert.equal(parking.children.length, 2);
  assert.equal(disposed.length, 2);

  pool.destroy();
  assert.equal(parking.children.length, 0);
});

test("ShortsPlayerPool standard mode preloads ahead, retains cached slots, and rebases on queue trim", () => {
  const created: FakeElement[] = [];
  const attached: Array<{ id: string; active?: boolean }> = [];
  const adapter: ShortsPoolSourceAdapter = {
    attach(video, _src, _playbackUrl, options) {
      attached.push({ id: video.dataset.shortsVideoId ?? "", active: options?.active });
    },
    setActive() {},
    dispose() {},
  };

  const pool = new ShortsPlayerPool({
    iosShared: false,
    sourceAdapter: adapter,
    createVideoElement: () => {
      const el = makeFakeVideo();
      created.push(el);
      return el as unknown as HTMLVideoElement;
    },
  });

  const items = makeItems(6);
  const slots = items.map(() => makeSlot());
  slots.forEach((slot, index) => pool.registerSlot(index, slot as unknown as HTMLElement));

  const v0 = pool.syncActive({ items, activeIndex: 0, muted: false, volume: 0.8 });
  assert.equal((v0 as unknown as FakeElement).volume, 0.8);

  pool.syncStandby({
    items,
    activeIndex: 0,
    browseDirection: 1,
    activeReadyForPreload: true,
    cacheWindow: { start: 0, end: 0 },
    cachedIds: new Set(["vid-0"]),
    muted: false,
    volume: 0.8,
  });

  // Both +1 and +2 are mounted and eagerly preloaded.
  const v1 = pool.getVideoAt(1) as unknown as FakeElement;
  const v2 = pool.getVideoAt(2) as unknown as FakeElement;
  assert.ok(v1 && v2);
  assert.equal(v1.preload, "auto");
  assert.equal(v2.preload, "auto");

  // Advancing to slide 1 reuses preloaded v1 without re-attaching.
  const attachCountBefore = attached.length;
  const active1 = pool.syncActive({ items, activeIndex: 1, muted: false, volume: 0.8 });
  assert.equal(active1, v1 as unknown as HTMLVideoElement);
  assert.equal(attached.length, attachCountBefore);

  // Rebasing indices shifts pooled video slot references cleanly.
  pool.rebaseIndices(1);
  assert.equal(pool.getVideoAt(0), v1 as unknown as HTMLVideoElement);
  assert.equal(pool.getVideoAt(1), v2 as unknown as HTMLVideoElement);
  pool.destroy();
});
