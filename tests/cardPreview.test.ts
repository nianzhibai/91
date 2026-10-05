import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { act, create } from "react-test-renderer";
import { MemoryRouter, useNavigate } from "react-router";
import { useCardPreview } from "../src/lib/useCardPreview.ts";
import { previewController } from "../src/lib/previewController.ts";

function previewHarness() {
  const cards = new Map<string, ReturnType<typeof useCardPreview>>();
  const media = new Map<string, ReturnType<typeof fakeMedia>>();
  let navigate!: ReturnType<typeof useNavigate>;
  let firstActive = true;
  let firstInView = true;
  let firstSource = "/first.mp4";

  function fakeMedia() {
    return {
      paused: false,
      src: "/preview.mp4",
      pause() { this.paused = true; },
      removeAttribute() { this.src = ""; },
      load() {},
    };
  }

  function Card({ id }: { id: string }) {
    const preview = useCardPreview({
      id,
      src: id === "first" ? firstSource : "/second.mp4",
      active: id === "first" ? firstActive : true,
      inView: id === "first" ? firstInView : true,
    });
    cards.set(id, preview);
    if (!media.has(id)) media.set(id, fakeMedia());
    preview.videoRef.current = media.get(id) as unknown as HTMLVideoElement;
    return null;
  }

  function Harness() {
    navigate = useNavigate();
    return createElement("div", null,
      createElement(Card, { id: "first" }), createElement(Card, { id: "second" }));
  }

  const tree = () => createElement(MemoryRouter, { initialEntries: ["/video/current"] }, createElement(Harness));
  let renderer!: ReturnType<typeof create>;
  act(() => { renderer = create(tree()); });
  return {
    card: (id: string) => cards.get(id)!,
    media: (id: string) => media.get(id)!,
    startBoth() {
      act(() => {
        cards.get("first")!.startPreview();
        cards.get("second")!.startPreview();
      });
    },
    updateFirst(options: { active?: boolean; inView?: boolean; src?: string }) {
      firstActive = options.active ?? firstActive;
      firstInView = options.inView ?? firstInView;
      firstSource = options.src ?? firstSource;
      act(() => { renderer.update(tree()); });
    },
    navigate: (path: string) => act(() => navigate(path)),
    close: () => act(() => renderer.unmount()),
  };
}

test("card previews are independent and the global switch releases every card", () => {
  previewController.setEnabled(true);
  const harness = previewHarness();
  try {
    harness.startBoth();
    assert.equal(harness.card("first").shouldRenderPreview, true);
    assert.equal(harness.card("second").shouldRenderPreview, true);
    act(() => harness.card("first").stopPreview());
    assert.equal(harness.card("first").shouldRenderPreview, false);
    assert.equal(harness.card("second").shouldRenderPreview, true);
    assert.equal(harness.media("first").src, "");
    act(() => harness.card("first").startPreview());
    act(() => previewController.setEnabled(false));
    for (const id of ["first", "second"]) {
      assert.equal(harness.card(id).shouldRenderPreview, false);
      assert.equal(harness.card(id).showPreviewLoader, false);
      assert.equal(harness.media(id).paused, true);
      assert.equal(harness.media(id).src, "");
    }
    harness.startBoth();
    assert.equal(harness.card("first").shouldRenderPreview, false);
    assert.equal(harness.card("second").shouldRenderPreview, false);
  } finally { harness.close(); previewController.setEnabled(false); }
});

test("hidden panels, offscreen cards and retained navigation release their previews", () => {
  previewController.setEnabled(true);
  const harness = previewHarness();
  try {
    harness.startBoth();
    harness.updateFirst({ active: false });
    assert.equal(harness.card("first").shouldRenderPreview, false);
    assert.equal(harness.card("second").shouldRenderPreview, true);
    act(() => harness.card("first").startPreview());
    assert.equal(harness.card("first").shouldRenderPreview, false);
    harness.updateFirst({ active: true });
    harness.startBoth();
    harness.updateFirst({ inView: false });
    assert.equal(harness.card("first").shouldRenderPreview, false);
    assert.equal(harness.card("second").shouldRenderPreview, true);
    harness.updateFirst({ inView: true });
    harness.startBoth();
    harness.navigate("/video/next");
    assert.equal(harness.card("first").shouldRenderPreview, false);
    assert.equal(harness.card("second").shouldRenderPreview, false);
  } finally { harness.close(); previewController.setEnabled(false); }
});

test("new sources reset playback and repeated contact does not restart the loader", () => {
  previewController.setEnabled(true);
  const harness = previewHarness();
  try {
    harness.startBoth();
    act(() => {
      harness.card("first").handlePreviewPlay();
      harness.card("first").finishPreviewLoader();
    });
    act(() => harness.card("first").startPreview());
    assert.equal(harness.card("first").previewState, "playing");
    assert.equal(harness.card("first").showPreviewLoader, false);
    harness.updateFirst({ src: "/replacement.mp4" });
    assert.equal(harness.card("first").shouldRenderPreview, false);
    assert.equal(harness.card("second").shouldRenderPreview, true);
    harness.updateFirst({ src: "" });
    act(() => harness.card("first").startPreview());
    assert.equal(harness.card("first").shouldRenderPreview, false);
  } finally { harness.close(); previewController.setEnabled(false); }
});
