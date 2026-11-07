import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const navigationCss = readFileSync(
  new URL("../src/styles/navigation.css", import.meta.url),
  "utf8"
);

const baseCss = readFileSync(
  new URL("../src/styles/base.css", import.meta.url),
  "utf8"
);

const topBarSource = readFileSync(
  new URL("../src/components/TopBar.tsx", import.meta.url),
  "utf8"
);

const mainNavSource = readFileSync(
  new URL("../src/components/MainNav.tsx", import.meta.url),
  "utf8"
);

const adminLayoutSource = readFileSync(
  new URL("../src/admin/AdminLayout.tsx", import.meta.url),
  "utf8"
);

const videoIconSource = readFileSync(
  new URL("../src/components/icons/VideoIcon.tsx", import.meta.url),
  "utf8"
);

const uploadIconSource = readFileSync(
  new URL("../src/components/icons/UploadIcon.tsx", import.meta.url),
  "utf8"
);

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

test("mobile menu links fill the full expanded menu row", () => {
  // 默认 .main-nav__link 用 inline-flex（mobile 段不重写 display，所以仍是 flex 容器）。
  const baseBody = ruleBody(navigationCss, ".main-nav__link");
  assert.match(baseBody, /display\s*:\s*(?:inline-)?flex\b/);
  // mobile 展开态把链接铺满整行。
  const openBody = ruleBody(navigationCss, ".main-nav.is-open .main-nav__link");
  assert.match(openBody, /width\s*:\s*100%/);
});

test("mobile logo motion resets after a tap", () => {
  assert.match(
    navigationCss,
    /@media \(hover: hover\) and \(pointer: fine\) \{\s*\.main-nav__logo:hover \{[^}]*transform:\s*scale\(1\.02\);[^}]*\}\s*\.main-nav__logo:hover \.main-nav__logo-mark \{[^}]*transform:\s*scale\(1\.02\);/s
  );

  const activeLogo = ruleBody(navigationCss, ".main-nav__logo:active");
  const activeMark = ruleBody(
    navigationCss,
    ".main-nav__logo:active .main-nav__logo-mark"
  );
  assert.match(activeLogo, /transform\s*:\s*scale\(0\.98\)/);
  assert.match(activeMark, /transform\s*:\s*scale\(0\.98\)/);
});

test("short video navigation uses the supplied Font Awesome icon", () => {
  assert.doesNotMatch(mainNavSource, /\bSparkles\b/);
  assert.match(mainNavSource, /function ShortVideoIcon/);
  assert.match(mainNavSource, /viewBox="0 0 448 512"/);
  assert.match(mainNavSource, /fill="currentColor"/);
  assert.match(mainNavSource, /M448\.5 209\.9c-44 \.1-87-13\.6-122\.8-39\.2/);
  assert.match(mainNavSource, /\{ to: "\/shorts", label: "短视频", icon: ShortVideoIcon \}/);
});

test("public and admin video navigation share the supplied Font Awesome icon", () => {
  assert.doesNotMatch(mainNavSource, /\bFilm\b/);
  assert.doesNotMatch(adminLayoutSource, /\bFilm\b/);
  assert.match(mainNavSource, /import \{ VideoIcon \} from "@\/components\/icons\/VideoIcon";/);
  assert.match(adminLayoutSource, /import \{ VideoIcon \} from "@\/components\/icons\/VideoIcon";/);
  assert.match(videoIconSource, /export function VideoIcon/);
  assert.match(videoIconSource, /viewBox="0 0 576 512"/);
  assert.match(videoIconSource, /M549\.7 124\.1C543\.5 100\.4 524\.9 81\.8 501\.4 75\.5/);
  assert.match(mainNavSource, /\{ to: "\/list", label: "视频", icon: VideoIcon \}/);
  assert.match(adminLayoutSource, /<VideoIcon size=\{15\} \/>/);
});

test("admin navigation uses the supplied Font Awesome icon", () => {
  assert.doesNotMatch(mainNavSource, /\bSettings\b/);
  assert.match(mainNavSource, /function AdminIcon/);
  assert.match(mainNavSource, /viewBox="0 0 512 512"/);
  assert.match(mainNavSource, /M195\.1 9\.5C198\.1-5\.3 211\.2-16 226\.4-16/);
  assert.match(mainNavSource, /const adminNavItem = \{ to: "\/admin", label: "后台", icon: AdminIcon \}/);
});

test("upload navigation uses the supplied Font Awesome Pro icon", () => {
  assert.doesNotMatch(mainNavSource, /icon: Upload\b/);
  assert.match(mainNavSource, /import \{ UploadIcon \} from "@\/components\/icons\/UploadIcon";/);
  assert.match(uploadIconSource, /commercial license: https:\/\/fontawesome\.com\/license/);
  assert.match(uploadIconSource, /export function UploadIcon/);
  assert.match(uploadIconSource, /M512 384c0 35\.3-28\.7 64-64 64L64 448/);
  assert.match(mainNavSource, /const uploadNavItem = \{ to: "\/upload", label: "上传", icon: UploadIcon \}/);
});

test("mobile navigation toggle uses the supplied Font Awesome Pro menu icon", () => {
  assert.doesNotMatch(mainNavSource, /<Menu size=\{22\} \/>/);
  assert.match(mainNavSource, /function MobileMenuIcon/);
  assert.match(mainNavSource, /viewBox="0 0 540 540"/);
  assert.match(mainNavSource, /opacity="\.4"/);
  assert.match(mainNavSource, /M27 306c0 7\.5 6 13\.5 13\.5 13\.5/);
  assert.match(mainNavSource, /M454\.9 216C481\.4 216 503 195 504 168\.7/);
  assert.match(mainNavSource, /open \? <X size=\{22\} \/> : <MobileMenuIcon size=\{26\} \/>/);

  const menuToggle = ruleBody(navigationCss, ".main-nav__toggle");
  const hoveredMenuToggle = ruleBody(navigationCss, ".main-nav__toggle:hover");
  assert.match(menuToggle, /background\s*:\s*transparent/);
  assert.match(menuToggle, /border\s*:\s*0/);
  assert.match(hoveredMenuToggle, /background\s*:\s*transparent/);
  assert.doesNotMatch(hoveredMenuToggle, /border-color/);
});

test("main nav keeps tap targets below the iOS PWA status area", () => {
  const navBody = ruleBody(navigationCss, ".main-nav");
  assert.match(navBody, /padding-top\s*:\s*env\(safe-area-inset-top,\s*0px\)/);

  const openListBody = ruleBody(navigationCss, ".main-nav.is-open .main-nav__list");
  assert.match(
    openListBody,
    /top\s*:\s*calc\(64px\s*\+\s*env\(safe-area-inset-top,\s*0px\)\)/
  );
});

test("top bar does not render inactive public auth links", () => {
  assert.doesNotMatch(topBarSource, /href="#(?:register|login)"/);
  assert.doesNotMatch(topBarSource, />\s*(?:注册|登录)\s*</);
});

test("browser history restores the previous scroll position without animation", () => {
  const htmlBody = ruleBody(baseCss, "html");
  assert.match(htmlBody, /scroll-behavior\s*:\s*auto/);
  assert.doesNotMatch(htmlBody, /scroll-behavior\s*:\s*smooth/);
});
