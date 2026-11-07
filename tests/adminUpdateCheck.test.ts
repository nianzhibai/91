import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const layoutSource = readFileSync(new URL("../src/admin/AdminLayout.tsx", import.meta.url), "utf8");
const globalActionsSource = readFileSync(
  new URL("../src/admin/AdminGlobalActions.tsx", import.meta.url),
  "utf8"
);
const appSource = readFileSync(new URL("../src/App.tsx", import.meta.url), "utf8");
const apiSource = readFileSync(new URL("../src/admin/api.ts", import.meta.url), "utf8");
const adminCss = readFileSync(new URL("../src/styles/admin.css", import.meta.url), "utf8");

test("home, theme, update, and logout are layout-level global actions", () => {
  assert.match(globalActionsSource, /role="toolbar"[\s\S]*?aria-label="后台全局操作"/);
  assert.match(globalActionsSource, /<Link[\s\S]*?to="\/"[\s\S]*?title="返回主站"/);
  assert.match(globalActionsSource, /title="主题外观"/);
  assert.match(globalActionsSource, /title=\{checkingUpdate \? "正在检查更新" : "检查更新"\}/);
  assert.match(globalActionsSource, /title=\{loggingOut \? "正在退出" : "退出登录"\}/);
  assert.match(layoutSource, /<AdminGlobalActions[\s\S]*?checkingUpdate=\{checkingUpdate\}[\s\S]*?loggingOut=\{loggingOut\}/);
  assert.doesNotMatch(layoutSource, /返回主站|admin-nav__group--home/);
  assert.doesNotMatch(layoutSource, /to="\/admin\/theme"|admin-nav__action|admin-sidebar__mobile-panel/);
  assert.match(adminCss, /\.admin-global-actions\s*\{[^}]*position:\s*fixed;[^}]*backdrop-filter:\s*blur\(16px\)/s);
});

test("system navigation orders backup, logs, then settings", () => {
  assert.match(
    layoutSource,
    />系统<[\s\S]*?to="\/admin\/backup"[\s\S]*?to="\/admin\/logs"[\s\S]*?to="\/admin\/settings"/
  );
});

test("global theme picker persists the site-wide setting and rolls back failures", () => {
  assert.match(globalActionsSource, /api\s*\.getSettings\(\)/);
  assert.match(globalActionsSource, /setActiveTheme\(next\);\s*applyTheme\(next\);/);
  assert.match(globalActionsSource, /await api\.updateSettings\(\{ theme: next \}\)/);
  assert.match(globalActionsSource, /catch \(error\) \{\s*setActiveTheme\(previous\);\s*applyTheme\(previous\);/);
  assert.match(globalActionsSource, /role="menu"[\s\S]*?aria-label="选择全站主题"/);
  assert.match(globalActionsSource, /"dark"[\s\S]*?"light"/);
  assert.doesNotMatch(globalActionsSource, /<strong>全站主题<\/strong>/);
  assert.doesNotMatch(globalActionsSource, /所有访客将使用所选主题/);
});

test("legacy theme page route redirects to the admin landing page", () => {
  assert.doesNotMatch(appSource, /const ThemePage|import\("@\/admin\/ThemePage"\)/);
  assert.match(
    appSource,
    /<Route path="theme" element=\{<Navigate to="\/admin\/drives" replace \/>\} \/>/
  );
});

test("available updates open a release notes dialog", () => {
  assert.match(apiSource, /releaseNotes\?: string/);
  assert.match(layoutSource, /const \[availableUpdate, setAvailableUpdate\] = useState<api\.UpdateCheck \| null>\(null\)/);
  assert.match(layoutSource, /if \(result\.hasUpdate\) \{\s*setAvailableUpdate\(result\)/);
  assert.match(layoutSource, /className="admin-modal--release-notes"/);
  assert.match(layoutSource, /aria-label="Release Note"/);
  assert.match(layoutSource, /availableUpdate\.releaseNotes\?\.trim\(\) \|\| "该版本未提供 Release Note。"/);
  assert.match(layoutSource, /href=\{availableUpdate\.releaseUrl\}/);
  assert.doesNotMatch(layoutSource, /onClick=\{\(\) => setAvailableUpdate\(null\)\}>\s*关闭\s*<\/button>/);
  assert.match(adminCss, /\.admin-release-notes__content div\s*\{[^}]*white-space:\s*pre-wrap/s);
  assert.match(adminCss, /\.admin-modal--release-notes\s*\{[^}]*border:\s*0;[^}]*box-shadow:\s*none;/s);
  assert.match(adminCss, /\.admin-modal--release-notes \.admin-modal__header,[\s\S]*?\.admin-modal--release-notes \.admin-modal__footer\s*\{[^}]*border:\s*0;/);
  assert.doesNotMatch(layoutSource, /dangerouslySetInnerHTML/);
});
