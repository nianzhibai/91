import assert from "node:assert/strict";
import test, { afterEach, beforeEach } from "node:test";
import { createElement, StrictMode } from "react";
import { act, render } from "./helpers/dom";
import { AuthProvider, useAuth } from "../src/admin/AuthContext";
import { TelegramStatusProvider } from "../src/admin/telegram/TelegramStatusProvider";
import { getTelegramAvailability, resetTelegramAvailability } from "../src/admin/telegram/availability";
import { useTelegramAvailability } from "../src/admin/telegram/useTelegramAvailability";

beforeEach(resetTelegramAvailability);
afterEach(resetTelegramAvailability);

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((done) => { resolve = done; });
  return { promise, resolve };
}

for (const enabled of [true, false]) {
  test(`the first admin render knows Telegram is ${enabled ? "enabled" : "disabled"} before its configuration refresh completes`, async (t) => {
    const session = deferredResponse();
    t.mock.method(globalThis, "fetch", (input: string) => {
      if (input === "/admin/api/me") return session.promise;
      assert.equal(input, "/admin/api/telegram/availability");
      return new Promise<Response>(() => undefined);
    });
    const navigationStates: (boolean | null)[] = [];
    function Navigation() {
      const { enabled } = useTelegramAvailability();
      navigationStates.push(enabled);
      return createElement("nav", null, enabled === true ? "Telegram" : "");
    }
    function Page({ active }: { active: boolean }) {
      const { status } = useAuth();
      return active && status === "authed"
        ? createElement(TelegramStatusProvider, { workspaceActive: false }, createElement(Navigation))
        : null;
    }
    const page = (active: boolean) => createElement(StrictMode, null,
      createElement(AuthProvider, null, createElement(Page, { active })));
    let view!: ReturnType<typeof render>;
    await act(async () => { view = render(page(true)); });
    assert.equal(navigationStates.length, 0);
    await act(async () => session.resolve(Response.json({ authenticated: true, role: "admin", telegramEnabled: enabled })));
    assert.ok(navigationStates.length > 0);
    assert.ok(navigationStates.every((state) => state === enabled));
    assert.equal(view.container.querySelector("nav")!.textContent, enabled ? "Telegram" : "");

    await act(async () => view.rerender(page(false)));
    assert.equal(getTelegramAvailability().enabled, enabled);
    navigationStates.length = 0;
    await act(async () => view.rerender(page(true)));
    assert.ok(navigationStates.length > 0);
    assert.ok(navigationStates.every((state) => state === enabled));
  });
}

test("login initializes Telegram before publishing the authenticated session and logout clears it", async (t) => {
  const login = deferredResponse();
  t.mock.method(globalThis, "fetch", async (input: string) => {
    if (input === "/admin/api/me") return Response.json({ authenticated: false });
    if (input === "/admin/api/login") return login.promise;
    assert.equal(input, "/admin/api/logout");
    return Response.json({ ok: true });
  });
  let auth!: ReturnType<typeof useAuth>;
  const authenticatedStates: (boolean | null)[] = [];
  function Probe() {
    auth = useAuth();
    if (auth.status === "authed") authenticatedStates.push(getTelegramAvailability().enabled);
    return null;
  }
  await act(async () => { render(createElement(AuthProvider, null, createElement(Probe))); });
  assert.equal(auth.status, "guest");
  let pendingLogin!: Promise<string | undefined>;
  await act(async () => { pendingLogin = auth.login("admin", "secret"); });
  assert.equal(authenticatedStates.length, 0);
  await act(async () => {
    login.resolve(Response.json({ ok: true, role: "admin", telegramEnabled: true }));
    assert.equal(await pendingLogin, "admin");
  });
  assert.deepEqual(authenticatedStates, [true]);
  await act(async () => auth.logout());
  assert.equal(auth.status, "guest");
  assert.equal(getTelegramAvailability().enabled, null);
});

test("a failed session refresh retains Telegram availability and session invalidation clears it", async (t) => {
  let healthy = true;
  t.mock.method(globalThis, "fetch", async (input: string) => {
    assert.equal(input, "/admin/api/me");
    return healthy
      ? Response.json({ authenticated: true, role: "admin", telegramEnabled: true })
      : new Response("Unavailable", { status: 503 });
  });
  let auth!: ReturnType<typeof useAuth>;
  function Probe() { auth = useAuth(); return null; }
  await act(async () => { render(createElement(AuthProvider, null, createElement(Probe))); });
  healthy = false;
  await act(async () => auth.refresh());
  assert.equal(auth.status, "authed");
  assert.equal(getTelegramAvailability().enabled, true);
  await act(async () => auth.invalidateSession());
  assert.equal(auth.status, "guest");
  assert.equal(getTelegramAvailability().enabled, null);
});
