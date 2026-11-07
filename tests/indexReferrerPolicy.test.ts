import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { JSDOM } from "jsdom";

const indexHtml = readFileSync(
  new URL("../index.html", import.meta.url),
  "utf8"
);

test("app shell prevents referrer leakage for 302 video playback", () => {
  assert.match(
    indexHtml,
    /<meta\s+name="referrer"\s+content="no-referrer"\s*\/?>/
  );
});

test("app shell applies the saved theme without writing storage", () => {
  const dom = new JSDOM(indexHtml, {
    url: "http://localhost/",
    runScripts: "dangerously",
    beforeParse(window) {
      window.localStorage.setItem("video-site:theme", "light");
      window.Storage.prototype.setItem = () => { throw new Error("Storage quota exceeded"); };
    },
  });
  try {
    assert.equal(dom.window.document.documentElement.getAttribute("data-theme"), "light");
    assert.equal(dom.window.localStorage.getItem("video-site:theme"), "light");
  } finally {
    dom.window.close();
  }
});

test("retired light palettes restore as light before the app mounts", () => {
  for (const saved of ["pink", "sky"]) {
    const dom = new JSDOM(indexHtml, {
      url: "http://localhost/",
      runScripts: "dangerously",
      beforeParse(window) {
        window.localStorage.setItem("video-site:theme", saved);
      },
    });
    try {
      assert.equal(dom.window.document.documentElement.dataset.theme, "light");
    } finally {
      dom.window.close();
    }
  }
});
