import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { createElement } from "react";
import { act, create } from "react-test-renderer";
import { useShortsKeyboard, type ShortsKeyboardOptions } from "../src/shorts/useShortsKeyboard";

function setup(t: TestContext) {
  class FocusedElement {
    constructor(public tagName: string, public isContentEditable = false) {}
    closest() { return null; }
  }
  const browserWindow = new EventTarget();
  const browserDocument = Object.assign(new EventTarget(), {
    activeElement: null as FocusedElement | null, hidden: false,
  });
  const globals = { window: browserWindow, document: browserDocument, HTMLElement: FocusedElement };
  const originals = Object.keys(globals).map(key => [key, Object.getOwnPropertyDescriptor(globalThis, key)] as const);
  for (const [key, value] of Object.entries(globals)) {
    Object.defineProperty(globalThis, key, { configurable: true, value });
  }
  const calls = { autoAdvance: 0, clearScreen: 0, fullscreen: 0 };
  const actions = new Map<string, {
    clicks: number;
    disabled: boolean;
    ariaDisabled: boolean;
    matches: (selector: string) => boolean;
    getAttribute: (name: string) => string | null;
    click: () => void;
  }>();
  const activeIndexRef = { current: 0 };
  const root = {
    querySelector(selector: string) {
      const index = /data-index="(\d+)"/.exec(selector)?.[1];
      const key = /data-shorts-shortcut="([A-Z])"/.exec(selector)?.[1];
      return actions.get(`${index}:${key}`) ?? null;
    },
  };
  let options: ShortsKeyboardOptions = {
    containerRef: { current: root as unknown as HTMLDivElement }, activeIndexRef, itemsLengthRef: { current: 2 },
    getVideoAtIndex: () => undefined, isVideoPausedByUser: () => false,
    setUserPausedForIndex: () => {}, onToggleMute: () => {}, showHud: () => {},
    isWindowsShortsPlatform: true,
    onToggleAutoAdvance: () => { calls.autoAdvance++; },
    onToggleClearScreen: () => { calls.clearScreen++; },
    onToggleFullscreen: () => { calls.fullscreen++; },
    enableSidebarShortcuts: true,
  };
  function Probe() { useShortsKeyboard(options); return null; }
  let renderer: ReturnType<typeof create>;
  act(() => { renderer = create(createElement(Probe)); });
  t.after(() => {
    act(() => renderer.unmount());
    for (const [key, original] of originals) {
      if (original) Object.defineProperty(globalThis, key, original);
      else Reflect.deleteProperty(globalThis, key);
    }
  });
  return {
    calls,
    activeIndexRef,
    addAction(index: number, key: string) {
      const action = {
        clicks: 0, disabled: false, ariaDisabled: false,
        matches(selector: string) { return selector === ":disabled" && this.disabled; },
        getAttribute(name: string) { return name === "aria-disabled" && this.ariaDisabled ? "true" : null; },
        click() { this.clicks++; },
      };
      actions.set(`${index}:${key}`, action);
      return action;
    },
    press(key: string, modifiers: Partial<KeyboardEvent> = {}) {
      const event = Object.assign(new Event("keydown", { cancelable: true }), {
        key, repeat: false, ctrlKey: false, metaKey: false, altKey: false, ...modifiers,
      });
      act(() => { browserWindow.dispatchEvent(event); });
      return event;
    },
    update(next: Partial<ShortsKeyboardOptions>) {
      options = { ...options, ...next };
      act(() => { renderer.update(createElement(Probe)); });
    },
    focus(tagName: string, editable = false) {
      browserDocument.activeElement = new FocusedElement(tagName, editable);
    },
  };
}

test("desktop controls respond to K, J and H once per key press", t => {
  const h = setup(t);
  for (const key of ["k", "K", "j", "J", "h", "H"]) {
    assert.equal(h.press(key).defaultPrevented, true);
    h.press(key, { repeat: true });
  }
  assert.deepEqual(h.calls, { autoAdvance: 2, clearScreen: 2, fullscreen: 2 });
});

test("sidebar shortcuts activate only the current video's control and ignore held repeats", t => {
  const h = setup(t);
  for (const key of ["D", "S", "X"]) {
    const first = h.addAction(0, key);
    const next = h.addAction(1, key);
    assert.equal(h.press(key.toLowerCase()).defaultPrevented, true);
    h.press(key, { repeat: true });
    assert.equal(first.clicks, 1);
    assert.equal(next.clicks, 0);
    h.activeIndexRef.current = 1;
    h.press(key);
    assert.equal(first.clicks, 1);
    assert.equal(next.clicks, 1);
    h.activeIndexRef.current = 0;
  }
});

test("sidebar shortcuts respect unavailable, disabled and permission-restricted controls", t => {
  const h = setup(t);
  const share = h.addAction(0, "S");
  share.disabled = true;
  assert.equal(h.press("S").defaultPrevented, false);
  assert.equal(share.clicks, 0);
  share.disabled = false;
  share.ariaDisabled = true;
  assert.equal(h.press("S").defaultPrevented, false);
  assert.equal(share.clicks, 0);
  // 没有隐藏权限时，该操作不会渲染。
  assert.equal(h.press("X").defaultPrevented, false);
  const source = h.addAction(0, "D");
  h.update({ enableSidebarShortcuts: false });
  assert.equal(h.press("D").defaultPrevented, false);
  assert.equal(source.clicks, 0);
});

test("sidebar shortcuts preserve browser combinations and text input", t => {
  const h = setup(t);
  const source = h.addAction(0, "D");
  const share = h.addAction(0, "S");
  const hide = h.addAction(0, "X");
  for (const key of ["D", "S", "X"]) {
    for (const modifier of ["ctrlKey", "metaKey", "altKey"]) {
      assert.equal(h.press(key, { [modifier]: true }).defaultPrevented, false);
    }
  }
  h.focus("INPUT");
  for (const key of ["D", "S", "X"]) assert.equal(h.press(key).defaultPrevented, false);
  h.focus("DIV", true);
  for (const key of ["D", "S", "X"]) assert.equal(h.press(key).defaultPrevented, false);
  assert.deepEqual([source.clicks, share.clicks, hide.clicks], [0, 0, 0]);
});

test("control shortcuts preserve browser shortcuts and typing", t => {
  const h = setup(t);
  for (const key of ["K", "J", "H"]) {
    for (const modifier of ["ctrlKey", "metaKey", "altKey"]) {
      assert.equal(h.press(key, { [modifier]: true }).defaultPrevented, false);
    }
  }
  for (const [tag, editable] of [["INPUT", false], ["TEXTAREA", false], ["SELECT", false], ["DIV", true]] as const) {
    h.focus(tag, editable);
    for (const key of ["K", "J", "H"]) assert.equal(h.press(key).defaultPrevented, false);
  }
  assert.deepEqual(h.calls, { autoAdvance: 0, clearScreen: 0, fullscreen: 0 });
});

test("control shortcuts use current callbacks and release keys when desktop controls are unavailable", t => {
  const h = setup(t);
  let nextCalls = 0;
  h.update({ onToggleClearScreen: () => { nextCalls++; } });
  h.press("J");
  assert.equal(nextCalls, 1);
  assert.equal(h.calls.clearScreen, 0);
  h.update({ onToggleAutoAdvance: undefined, onToggleClearScreen: undefined, onToggleFullscreen: undefined });
  for (const key of ["K", "J", "H"]) assert.equal(h.press(key).defaultPrevented, false);
  assert.deepEqual(h.calls, { autoAdvance: 0, clearScreen: 0, fullscreen: 0 });
  assert.equal(nextCalls, 1);
});
