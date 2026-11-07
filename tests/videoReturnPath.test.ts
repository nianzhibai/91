import assert from "node:assert/strict";
import test from "node:test";
import { JSDOM } from "jsdom";

import {
  normalizeVideoReturnPath,
  readVideoReturnPath,
  routeToPath,
} from "../src/lib/videoReturnPath.ts";

const ORIGIN = "http://66.92.50.106:9191";

test("builds a return path from router location parts", () => {
  assert.equal(
    routeToPath({ pathname: "/list", search: "?tag=AV", hash: "#top" }),
    "/list?tag=AV#top"
  );
});

test("accepts public same-origin pages as video delete return paths", () => {
  assert.equal(normalizeVideoReturnPath("/", ORIGIN), "/");
  assert.equal(
    normalizeVideoReturnPath("/list?q=test&page=2", ORIGIN),
    "/list?q=test&page=2"
  );
});

test("rejects unsafe video delete return paths", () => {
  assert.equal(normalizeVideoReturnPath("/video/abc", ORIGIN), null);
  assert.equal(normalizeVideoReturnPath("/login", ORIGIN), null);
  assert.equal(normalizeVideoReturnPath("/share#secret-token", ORIGIN), null);
  assert.equal(
    normalizeVideoReturnPath("https://example.com/list", ORIGIN),
    null
  );
});

for (const failedOperation of ["setItem", "removeItem"] as const) {
  test(`saved return paths are readable without storage ${failedOperation}`, (t) => {
    const dom = new JSDOM("", { url: ORIGIN });
    const previousWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
    Object.defineProperty(globalThis, "window", { configurable: true, value: dom.window });
    t.after(() => {
      if (previousWindow) Object.defineProperty(globalThis, "window", previousWindow);
      else Reflect.deleteProperty(globalThis, "window");
      dom.window.close();
    });
    dom.window.sessionStorage.setItem("video-site:video-return-path", "/list?q=saved");
    t.mock.method(dom.window.Storage.prototype, failedOperation, () => {
      throw new Error("Storage is not writable");
    });
    assert.equal(readVideoReturnPath(), "/list?q=saved");
    assert.equal(dom.window.sessionStorage.getItem("video-site:video-return-path"), "/list?q=saved");
  });
}
