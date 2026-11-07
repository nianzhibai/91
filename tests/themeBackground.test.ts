import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const readStyle = (name: string) => readFileSync(
  new URL(`../src/styles/${name}.css`, import.meta.url), "utf8",
);

test("all themes use plain page and detail backgrounds without decorative layers", () => {
  const tokens = readStyle("tokens");
  const base = readStyle("base");
  const layout = readStyle("layout");
  const app = readFileSync(new URL("../src/App.tsx", import.meta.url), "utf8");

  for (const theme of ["dark", "light"]) {
    const palette = tokens.match(new RegExp(`:root\\[data-theme="${theme}"\\]\\s*\\{([^}]+)\\}`));
    assert.ok(palette, `missing palette: ${theme}`);
    assert.match(palette[1], /--bg-page:\s*#[a-f0-9]{6};/);
    assert.doesNotMatch(palette[1], /gradient|--bg-grid/);
  }
  assert.match(base, /\nbody\s*\{[^}]*background:\s*var\(--bg-page\)/s);
  assert.doesNotMatch(base + tokens, /body::before|sky-star/);
  const foreground = layout.match(/\.video-detail-foreground\s*\{([^}]+)\}/);
  assert.ok(foreground);
  assert.match(foreground[1], /background-color:\s*var\(--bg-page\)/);
  assert.doesNotMatch(foreground[1], /gradient|background-image/);
  assert.doesNotMatch(app, /SkyStarfield/);
});
