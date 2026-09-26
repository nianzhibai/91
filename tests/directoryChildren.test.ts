import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { createElement } from "react";
import { act, create } from "react-test-renderer";
import { useDirectoryChildren } from "../src/admin/drive/useDirectoryChildren";

type Props = { driveId: string; parentId: string; open: boolean };
const initial: Props = { driveId: "115", parentId: "", open: true };

async function mount(t: TestContext, props = initial) {
  let current!: ReturnType<typeof useDirectoryChildren>;
  function Probe(next: Props) {
    current = useDirectoryChildren(next.driveId, next.parentId, next.open);
    return null;
  }
  let renderer!: ReturnType<typeof create>;
  await act(async () => { renderer = create(createElement(Probe, props)); });
  t.after(async () => { await act(async () => renderer.unmount()); });
  return {
    state: () => current,
    update: async (next: Props) => { await act(async () => renderer.update(createElement(Probe, next))); },
    retry: async () => { await act(async () => current.retry()); },
    unmount: async () => { await act(async () => renderer.unmount()); },
  };
}

test("directory failures stay stopped across renders and reopen until an explicit retry", async (t) => {
  const fetchMock = t.mock.method(globalThis, "fetch", async () => Response.json({ error: "TLS handshake timeout" }, { status: 502 }));
  const view = await mount(t);
  assert.equal(view.state().status, "error");
  assert.equal(view.state().error, "TLS handshake timeout");
  for (let i = 0; i < 5; i++) await view.update({ ...initial });
  await view.update({ ...initial, open: false });
  await view.update(initial);
  assert.equal(fetchMock.mock.callCount(), 1);

  fetchMock.mock.mockImplementation(async () => Response.json([{ id: "folder", name: "Recovered" }]));
  await view.retry();
  assert.equal(view.state().status, "success");
  assert.deepEqual(view.state().children, [{ id: "folder", name: "Recovered" }]);
  assert.equal(fetchMock.mock.callCount(), 2);
  await view.update({ ...initial, open: false });
  await view.update(initial);
  assert.equal(fetchMock.mock.callCount(), 2, "successful directories remain cached on reopen");
});

test("switching drives or directories cancels requests and ignores stale responses", async (t) => {
  const pending: { signal: AbortSignal; finish: (response: Response) => void }[] = [];
  t.mock.method(globalThis, "fetch", async (_url: string, init: RequestInit) => new Promise<Response>((finish) => {
    pending.push({ signal: init.signal as AbortSignal, finish });
  }));
  const view = await mount(t);
  await view.update({ ...initial });
  assert.equal(pending.length, 1, "renders cannot duplicate an in-flight request");
  await view.update({ ...initial, driveId: "123" });
  assert.equal(pending[0].signal.aborted, true);
  assert.equal(pending.length, 2);
  await act(async () => pending[1].finish(Response.json([{ id: "new", name: "New drive" }])));
  await act(async () => pending[0].finish(Response.json([{ id: "old", name: "Old drive" }])));
  assert.deepEqual(view.state().children, [{ id: "new", name: "New drive" }]);
  await view.update({ ...initial, driveId: "123", parentId: "new" });
  assert.equal(view.state().status, "loading");
  assert.deepEqual(view.state().children, []);
  await view.unmount();
  assert.equal(pending[2].signal.aborted, true);
  await act(async () => pending[2].finish(Response.json([])));
});

test("collapsed nodes wait for expansion, and canceled loads can restart", async (t) => {
  const signals: AbortSignal[] = [];
  t.mock.method(globalThis, "fetch", async (_url: string, init: RequestInit) => {
    const signal = init.signal as AbortSignal;
    signals.push(signal);
    return new Promise<Response>((_resolve, reject) => {
      signal.addEventListener("abort", () => reject(new DOMException("Canceled", "AbortError")), { once: true });
    });
  });
  const view = await mount(t, { ...initial, open: false });
  assert.equal(view.state().status, "idle");
  assert.equal(signals.length, 0);
  await view.update(initial);
  assert.equal(signals.length, 1);
  await view.update({ ...initial, open: false });
  assert.equal(signals[0].aborted, true);
  assert.equal(view.state().error, "");
  await view.update(initial);
  assert.equal(signals.length, 2);
  await view.unmount();
  assert.equal(signals[1].aborted, true);
});
