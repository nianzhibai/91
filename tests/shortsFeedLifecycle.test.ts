import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { act, create } from "react-test-renderer";
import { useShortsFeed } from "../src/shorts/useShortsFeed";
import { loadShortsFeedState, saveShortsFeedState, type ShortsFeedMode } from "../src/shorts/shortsFeed";

test("switching shorts modes aborts old work and ignores late responses after returning", async (t) => {
  const originals = ["window", "localStorage"].map(key => [key, Object.getOwnPropertyDescriptor(globalThis, key)] as const);
  const events = Object.assign(new EventTarget(), { setTimeout, clearTimeout });
  const storage = new Map<string, string>();
  Object.defineProperty(globalThis, "window", { configurable: true, value: events });
  Object.defineProperty(globalThis, "localStorage", { configurable: true, value: {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
  } });
  let renderer: ReturnType<typeof create> | undefined;
  t.after(async () => {
    await act(async () => renderer?.unmount());
    for (const [key, original] of originals) {
      if (original) Object.defineProperty(globalThis, key, original);
      else Reflect.deleteProperty(globalThis, key);
    }
  });
  const pending: { mode: string; signal: AbortSignal; resolve: (value: Response) => void }[] = [];
  t.mock.method(globalThis, "fetch", (input, init) => new Promise<Response>(resolve => {
    pending.push({ mode: new URL(String(input), "http://localhost").searchParams.get("mode")!,
      signal: init!.signal as AbortSignal, resolve });
  }));
  let state!: ReturnType<typeof useShortsFeed>;
  function Probe({ mode }: { mode: ShortsFeedMode }) {
    state = useShortsFeed(0, () => {}, mode);
    return null;
  }
  const response = (token: string) => Response.json({
    items: [1, 2].map(feedCursor => ({ id: `${token}-${feedCursor}`, feedCursor })),
    total: 2, feedToken: token, nextCursor: 2, roundComplete: true,
  });
  saveShortsFeedState({ feedToken: "latest-bookmark", cursor: 5 }, "latest");
  await act(async () => { renderer = create(createElement(Probe, { mode: "latest" })); });
  assert.equal(pending[0].mode, "latest");
  await act(async () => renderer!.update(createElement(Probe, { mode: "hot" })));
  assert.equal(pending[0].signal.aborted, true);
  await act(async () => pending[1].resolve(response("hot-feed")));
  assert.equal(state.items[0].id, "hot-feed-1");
  await act(async () => renderer!.update(createElement(Probe, { mode: "latest" })));
  assert.equal(state.loading, true);
  await act(async () => pending[0].resolve(response("stale-feed")));
  assert.deepEqual(state.items, []);
  assert.equal(state.loading, true);
  assert.equal(loadShortsFeedState("latest").feedToken, "latest-bookmark");
  await act(async () => pending[2].resolve(response("fresh-feed")));
  assert.equal(state.items[0].id, "fresh-feed-1");
  assert.equal(state.loading, false);
  await act(async () => events.dispatchEvent(new Event("pagehide")));
  assert.equal(loadShortsFeedState("latest").feedToken, "fresh-feed");
  assert.equal(loadShortsFeedState("hot").feedToken, "hot-feed");
});
