import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createElement } from "react";
import { fireEvent, render } from "./helpers/dom";
import { VideoThumbnail } from "../src/components/VideoThumbnail";

test("a thumbnail stays hidden until its image loads", () => {
  const { container } = render(createElement(VideoThumbnail, { src: "/thumbnail.jpg" }));
  const image = container.querySelector("img")!;
  const placeholder = container.querySelector(".thumb-placeholder")!;

  assert.equal(placeholder.getAttribute("data-state"), "loading");
  assert.equal(image.classList.contains("is-ready"), false);
  fireEvent.load(image);
  assert.equal(placeholder.getAttribute("data-state"), "ready");
  assert.equal(image.classList.contains("is-ready"), true);
});

test("failed thumbnails recover when the source changes and ignore old image events", () => {
  const { container, rerender } = render(createElement(VideoThumbnail, { src: "/broken.jpg" }));
  const oldImage = container.querySelector("img")!;
  fireEvent.error(oldImage);
  assert.equal(container.querySelector(".thumb-placeholder")!.getAttribute("data-state"), "failed");
  assert.equal(oldImage.classList.contains("is-ready"), false);
  assert.equal(oldImage.getAttribute("src"), "/broken.jpg");

  rerender(createElement(VideoThumbnail, { src: "/replacement.jpg" }));
  const image = container.querySelector("img")!;
  assert.equal(image.getAttribute("src"), "/replacement.jpg");
  assert.equal(container.querySelector(".thumb-placeholder")!.getAttribute("data-state"), "loading");
  fireEvent.load(oldImage);
  assert.equal(image.classList.contains("is-ready"), false);
  fireEvent.load(image);
  assert.equal(container.querySelector(".thumb-placeholder")!.getAttribute("data-state"), "ready");
  assert.equal(image.classList.contains("is-ready"), true);
});

for (const { name, width, state } of [
  { name: "loaded", width: 320, state: "ready" },
  { name: "broken", width: 0, state: "failed" },
]) {
  test(`a cached ${name} thumbnail settles without a load or error event`, (t) => {
    const prototype = window.HTMLImageElement.prototype;
    t.mock.getter(prototype, "complete", () => true);
    t.mock.getter(prototype, "naturalWidth", () => width);
    const { container, rerender } = render(createElement(VideoThumbnail, { src: "/first.jpg" }));

    for (const src of ["/first.jpg", "/replacement.jpg"]) {
      rerender(createElement(VideoThumbnail, { src }));
      assert.equal(container.querySelector(".thumb-placeholder")!.getAttribute("data-state"), state);
      assert.equal(container.querySelector("img")!.classList.contains("is-ready"), state === "ready");
    }
  });
}

test("missing and deferred thumbnails do not create image resources", () => {
  const { container, rerender } = render(createElement(VideoThumbnail, { src: "" }));
  assert.equal(container.querySelector("img"), null);
  assert.equal(container.querySelector(".thumb-placeholder")!.getAttribute("data-state"), "failed");

  rerender(createElement(VideoThumbnail, { src: "/thumbnail.jpg", enabled: false }));
  assert.equal(container.querySelector("img"), null);
  assert.equal(container.querySelector(".thumb-placeholder")!.getAttribute("data-state"), "deferred");

  rerender(createElement(VideoThumbnail, { src: "/thumbnail.jpg", enabled: true }));
  assert.equal(container.querySelector("img")!.getAttribute("src"), "/thumbnail.jpg");
  assert.equal(container.querySelector(".thumb-placeholder")!.getAttribute("data-state"), "loading");

  rerender(createElement(VideoThumbnail, { src: "/thumbnail.jpg", enabled: false }));
  assert.equal(container.querySelector("img"), null);
});

test("image loading hints follow thumbnail priority", () => {
  const { container, rerender } = render(createElement(VideoThumbnail, { src: "/thumbnail.jpg" }));
  for (const { eager, highPriority, loading, priority } of [
    { eager: false, highPriority: false, loading: "lazy", priority: "auto" },
    { eager: true, highPriority: false, loading: "eager", priority: "auto" },
    { eager: false, highPriority: true, loading: "eager", priority: "high" },
  ]) {
    rerender(createElement(VideoThumbnail, { src: "/thumbnail.jpg", eager, highPriority }));
    const image = container.querySelector("img")!;
    assert.equal(image.getAttribute("loading"), loading);
    assert.equal(image.getAttribute("fetchpriority"), priority);
    assert.equal(image.getAttribute("decoding"), "async");
    assert.equal(image.getAttribute("alt"), "");
  }
});

test("thumbnail styles only reveal ready images", () => {
  const css = readFileSync(new URL("../src/styles/video-card.css", import.meta.url), "utf8");
  assert.match(css, /\.thumb-image\s*\{[^}]*opacity:\s*0/s);
  assert.match(css, /\.thumb-image\.is-ready\s*\{[^}]*opacity:\s*1/s);
});
