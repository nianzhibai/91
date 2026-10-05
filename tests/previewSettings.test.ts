import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test, { type TestContext } from "node:test";
import { previewController } from "../src/lib/previewController";
import { applyPreviewEnabled, syncPreviewSettings, watchPreviewSettings } from "../src/lib/previewSettings";
import { updateConfigYAML } from "../src/admin/api";

function mockPreviewPage(t: TestContext) {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });
  const windowTarget = Object.assign(new EventTarget(), {
    setTimeout: globalThis.setTimeout,
    clearTimeout: globalThis.clearTimeout,
  });
  const documentTarget = Object.assign(new EventTarget(), { visibilityState: "visible" });
  const originals = new Map<string, PropertyDescriptor | undefined>();
  for (const [name, value] of [["window", windowTarget], ["document", documentTarget]] as const) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
  }
  let stop = () => {};
  t.after(() => {
    stop();
    for (const [name, original] of originals) {
      if (original) Object.defineProperty(globalThis, name, original);
      else Reflect.deleteProperty(globalThis, name);
    }
  });
  return {
    start() {
      stop = watchPreviewSettings();
      return stop;
    },
    focus: () => windowTarget.dispatchEvent(new Event("focus")),
    visibility(state: "visible" | "hidden") {
      documentTarget.visibilityState = state;
      documentTarget.dispatchEvent(new Event("visibilitychange"));
    },
    async advance(ms = 0) {
      t.mock.timers.tick(ms);
      await new Promise<void>(resolve => setImmediate(resolve));
    },
  };
}

test("previews stay inactive until the global policy has loaded", () => {
  assert.equal(previewController.isEnabled(), false);
});

test("preview policy changes notify every independent card", () => {
  applyPreviewEnabled(true);
  const updates: boolean[] = [];
  const unsubscribe = previewController.subscribe(enabled => updates.push(enabled));
  applyPreviewEnabled(false);
  assert.equal(previewController.isEnabled(), false);
  assert.deepEqual(updates, [false]);
  applyPreviewEnabled(true);
  assert.equal(previewController.isEnabled(), true);
  assert.deepEqual(updates, [false, true]);
  unsubscribe();
  applyPreviewEnabled(false);
});

test("public preview settings synchronize without trusting cached media URLs", async (t) => {
  t.mock.method(globalThis, "fetch", async (url: string, init: RequestInit) => {
    assert.equal(url, "/api/settings/preview");
    assert.equal(init.cache, "no-store");
    return Response.json({ previewEnabled: false });
  });
  applyPreviewEnabled(true);
  await syncPreviewSettings();
  assert.equal(previewController.isEnabled(), false);
});

test("a delayed settings response cannot undo a freshly saved global switch", async (t) => {
  let respond!: (response: Response) => void;
  t.mock.method(globalThis, "fetch", () => new Promise<Response>(resolve => { respond = resolve; }));
  const pending = syncPreviewSettings();
  applyPreviewEnabled(false);
  respond(Response.json({ previewEnabled: true }));
  await pending;
  assert.equal(previewController.isEnabled(), false);
});

test("failed or malformed settings responses do not enable previews", async (t) => {
  for (const response of [
    new Response(null, { status: 503 }),
    Response.json({}),
    Response.json({ previewEnabled: "true" }),
    new Response("invalid-json"),
  ]) {
    const mock = t.mock.method(globalThis, "fetch", async () => response);
    applyPreviewEnabled(true);
    await syncPreviewSettings();
    assert.equal(previewController.isEnabled(), false);
    mock.mock.restore();
  }
});

test("saving config immediately updates the shared frontend policy", async (t) => {
  const result = { settings: { previewEnabled: false, telegramEnabled: false }, restartRequired: false };
  t.mock.method(globalThis, "fetch", async () => Response.json(result));
  applyPreviewEnabled(true);
  assert.deepEqual(await updateConfigYAML("preview: {enabled: false}", "version"), result);
  assert.equal(previewController.isEnabled(), false);
});

test("preview synchronization is limited to authenticated listing and detail routes", () => {
  const source = readFileSync(new URL("../src/App.tsx", import.meta.url), "utf8");
  assert.match(source, /const videoDetailMatch = useMatch\("\/video\/:id"\)/);
  assert.match(source, /const shouldSyncPreviews = status === "authed" &&\s*\(isVideoListingPath\(location\.pathname\) \|\| videoDetailMatch !== null\)/);
  assert.match(source, /useEffect\(\(\) => \{\s*if \(shouldSyncPreviews\) return watchPreviewSettings\(\);\s*\}, \[shouldSyncPreviews\]\)/);
});

test("lifecycle replay and simultaneous refresh events send only one request", async (t) => {
  const page = mockPreviewPage(t);
  const fetch = t.mock.method(globalThis, "fetch", async () => Response.json({ previewEnabled: true }));
  const stop = page.start();
  stop();
  page.start();
  page.focus();
  page.visibility("visible");
  await page.advance();
  assert.equal(fetch.mock.callCount(), 1);

  page.focus();
  page.visibility("visible");
  await page.advance();
  await page.advance(1_000);
  page.focus();
  await page.advance();
  assert.equal(fetch.mock.callCount(), 1);

  await page.advance(14_000);
  page.focus();
  page.visibility("visible");
  await page.advance();
  assert.equal(fetch.mock.callCount(), 2);
});

test("refresh events do not overlap an in-flight request", async (t) => {
  const page = mockPreviewPage(t);
  let respond!: (response: Response) => void;
  const fetch = t.mock.method(globalThis, "fetch", () => new Promise<Response>(resolve => { respond = resolve; }));
  page.start();
  await page.advance();
  await page.advance(3_000);
  page.focus();
  page.visibility("visible");
  await page.advance();
  assert.equal(fetch.mock.callCount(), 1);

  respond(Response.json({ previewEnabled: true }));
  await page.advance();
  page.focus();
  await page.advance();
  assert.equal(fetch.mock.callCount(), 1);
  await page.advance(15_000);
  assert.equal(fetch.mock.callCount(), 2);
  respond(Response.json({ previewEnabled: false }));
  await page.advance();
});

test("hidden pages pause polling and resume with one fresh request", async (t) => {
  const page = mockPreviewPage(t);
  const fetch = t.mock.method(globalThis, "fetch", async () => Response.json({ previewEnabled: true }));
  page.visibility("hidden");
  page.start();
  page.focus();
  await page.advance(60_000);
  assert.equal(fetch.mock.callCount(), 0);

  page.visibility("visible");
  page.focus();
  await page.advance();
  assert.equal(fetch.mock.callCount(), 1);
  page.visibility("hidden");
  await page.advance(60_000);
  assert.equal(fetch.mock.callCount(), 1);
  page.visibility("visible");
  page.focus();
  await page.advance();
  assert.equal(fetch.mock.callCount(), 2);

  page.visibility("hidden");
  page.visibility("visible");
  await page.advance();
  assert.equal(fetch.mock.callCount(), 2);
  await page.advance(15_000);
  assert.equal(fetch.mock.callCount(), 3);
});

test("polling continues while previews are disabled and can enable them again", async (t) => {
  const page = mockPreviewPage(t);
  let enabled = false;
  const fetch = t.mock.method(globalThis, "fetch", async () => Response.json({ previewEnabled: enabled }));
  page.start();
  await page.advance();
  assert.equal(previewController.isEnabled(), false);
  enabled = true;
  await page.advance(15_000);
  assert.equal(fetch.mock.callCount(), 2);
  assert.equal(previewController.isEnabled(), true);
});

test("stopping aborts the active request and ignores its late response", async (t) => {
  const page = mockPreviewPage(t);
  let signal!: AbortSignal;
  let respond!: (response: Response) => void;
  const fetch = t.mock.method(globalThis, "fetch", (_url: string, init: RequestInit) => {
    signal = init.signal!;
    return new Promise<Response>(resolve => { respond = resolve; });
  });
  applyPreviewEnabled(false);
  const stop = page.start();
  await page.advance();
  stop();
  assert.equal(signal.aborted, true);
  respond(Response.json({ previewEnabled: true }));
  await page.advance();
  assert.equal(previewController.isEnabled(), false);
  page.focus();
  page.visibility("visible");
  await page.advance(60_000);
  assert.equal(fetch.mock.callCount(), 1);
});

test("a cancelled request cannot disable the policy after its watcher restarts", async (t) => {
  const page = mockPreviewPage(t);
  let fail!: (error: Error) => void;
  const fetch = t.mock.method(globalThis, "fetch", () => new Promise<Response>((_resolve, reject) => { fail = reject; }));
  const stop = page.start();
  await page.advance();
  stop();
  fetch.mock.mockImplementation(async () => Response.json({ previewEnabled: true }));
  page.start();
  await page.advance();
  fail(new Error("cancelled"));
  await page.advance();
  assert.equal(previewController.isEnabled(), true);
  await page.advance(15_000);
  assert.equal(fetch.mock.callCount(), 3);
});

test("a request cancelled before synchronization does not fetch or change policy", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => Response.json({ previewEnabled: false }));
  const controller = new AbortController();
  controller.abort();
  applyPreviewEnabled(true);
  await syncPreviewSettings(controller.signal);
  assert.equal(fetch.mock.callCount(), 0);
  assert.equal(previewController.isEnabled(), true);
});

test("a timed-out request disables previews and polling retries", async (t) => {
  const page = mockPreviewPage(t);
  const fetch = t.mock.method(globalThis, "fetch", (_url: string, init: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init.signal!.addEventListener("abort", () => reject(new Error("timeout")), { once: true });
    })
  );
  applyPreviewEnabled(true);
  page.start();
  await page.advance();
  await page.advance(10_000);
  assert.equal(previewController.isEnabled(), false);
  fetch.mock.mockImplementation(async () => Response.json({ previewEnabled: true }));
  await page.advance(15_000);
  assert.equal(fetch.mock.callCount(), 2);
  assert.equal(previewController.isEnabled(), true);
});

test("every card surface shares independent previews and normal link navigation", () => {
  const hook = readFileSync(new URL("../src/lib/useCardPreview.ts", import.meta.url), "utf8");
  assert.match(hook, /const previewEnabled = usePreviewEnabled\(\)/);
  assert.match(hook, /!previewController\.isEnabled\(\) \|\| !src \|\| !active/);
  assert.match(hook, /if \(!previewEnabled \|\| !active \|\| !inView\) stopPreview\(\)/);
  assert.doesNotMatch(hook, /getActiveId|setActiveId|setTimeout/);
  for (const file of ["VideoCard", "RecommendedRail", "MobileVideoCollection"]) {
    const source = readFileSync(new URL(`../src/components/${file}.tsx`, import.meta.url), "utf8");
    assert.match(source, /useCardPreview\(\{ id: video\.id, src: video\.previewSrc, inView/);
    assert.match(source, /data-preview-enabled=\{previewEnabled\}/);
    assert.match(source, /onTouchStart=\{(?:handleTouchStart|startPreview)\}/);
    assert.doesNotMatch(source, /shouldInterceptPreviewTap|onClickCapture|useIsActivePreview\(|getActiveId|setActiveId|onBlur=/);
    assert.match(source, /previewEnabled && shouldRenderPreview &&/);
    assert.doesNotMatch(source, /\{previewState === "(?:loading|playing|error)"/);
  }
});

test("card feedback underlines titles without transforming thumbnails", () => {
  const cards = readFileSync(new URL("../src/styles/video-card.css", import.meta.url), "utf8");
  const detail = readFileSync(new URL("../src/styles/video-detail.css", import.meta.url), "utf8");
  const interactions = readFileSync(new URL("../src/styles/video-card-interactions.css", import.meta.url), "utf8");
  assert.doesNotMatch(cards, /\.video-card:hover\s*\{/);
  assert.doesNotMatch(interactions, /\.video-card__link:active \.thumb-frame/);
  assert.doesNotMatch(interactions, /\.thumb-frame::before/);
  assert.doesNotMatch(interactions, /transform:|::before/);
  assert.match(interactions, /\.vd-rail__link:focus-visible \.vd-rail__title/);
  assert.match(interactions, /\.vd-collection-item__link\[data-title-pressed="true"\] \.vd-collection-item__title/);
  assert.doesNotMatch(interactions, /data-preview-enabled/);
  assert.match(cards, /\.video-card__title-link:hover\s*\{/);
  assert.doesNotMatch(cards, /\.video-card\[data-preview-enabled="true"\]:(?:active|focus-within)/);
  assert.doesNotMatch(cards, /\.video-card\[data-preview-enabled="false"\](?:,|::after)/);
  assert.doesNotMatch(cards, /\.video-card\[data-preview-enabled="true"\]:hover \.thumb-image/);
  assert.match(cards, /\[data-preview-enabled="false"\][\s\S]*?transition: none/);
  assert.doesNotMatch(detail, /\.vd-rail__item\[data-preview-enabled="true"\] \.vd-rail__link:hover \.vd-rail__thumb img/);
  assert.doesNotMatch(detail, /\.vd-rail__link:hover \.vd-rail__title\s*\{/);
  assert.doesNotMatch(detail, /\[data-preview-enabled="false"\] \.vd-rail__(?:title|link)/);
});
