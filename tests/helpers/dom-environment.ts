import { after } from "node:test";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
  pretendToBeVisual: true,
});

// React checks for AnimationEvent before choosing the standard event names.
if (!dom.window.AnimationEvent) {
  dom.window.AnimationEvent = class AnimationEvent extends dom.window.Event {
    readonly animationName: string;
    readonly elapsedTime: number;
    readonly pseudoElement: string;

    constructor(type: string, init: AnimationEventInit = {}) {
      super(type, init);
      this.animationName = init.animationName ?? "";
      this.elapsedTime = init.elapsedTime ?? 0;
      this.pseudoElement = init.pseudoElement ?? "";
    }
  };
}

for (const name of [
  "window", "document", "navigator", "Node", "HTMLElement", "HTMLButtonElement",
  "HTMLInputElement", "HTMLVideoElement", "Event", "EventTarget", "MouseEvent",
  "KeyboardEvent", "FocusEvent", "CustomEvent", "MutationObserver", "DOMException",
] as const) {
  Object.defineProperty(globalThis, name, {
    configurable: true,
    value: dom.window[name],
    writable: true,
  });
}

Object.defineProperty(globalThis, "IS_REACT_ACT_ENVIRONMENT", {
  configurable: true,
  value: true,
  writable: true,
});
Object.defineProperties(dom.window.document, {
  hidden: { configurable: true, value: false, writable: true },
  visibilityState: { configurable: true, value: "visible", writable: true },
});
dom.window.matchMedia = (query: string) => Object.assign(new dom.window.EventTarget(), {
  matches: false,
  media: query,
  onchange: null,
  addListener() {},
  removeListener() {},
});

after(() => dom.window.close());
