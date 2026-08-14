import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const adminCss = readFileSync(
  new URL("../src/styles/admin.css", import.meta.url),
  "utf8"
);
const loginCss = readFileSync(
  new URL("../src/styles/login.css", import.meta.url),
  "utf8"
);
const adminControlsCss = readFileSync(
  new URL("../src/styles/admin-controls.css", import.meta.url),
  "utf8"
);
const searchCss = readFileSync(
  new URL("../src/styles/search.css", import.meta.url),
  "utf8"
);
const themeTokensCss = readFileSync(
  new URL("../src/styles/tokens.css", import.meta.url),
  "utf8"
);
const adminLayoutSource = readFileSync(
  new URL("../src/admin/AdminLayout.tsx", import.meta.url),
  "utf8"
);
const videosPageSource = readFileSync(
  new URL("../src/admin/VideosPage.tsx", import.meta.url),
  "utf8"
);
const usersPageSource = readFileSync(
  new URL("../src/admin/UsersPage.tsx", import.meta.url),
  "utf8"
);
const tagsPageSource = readFileSync(
  new URL("../src/admin/TagsPage.tsx", import.meta.url),
  "utf8"
);
const crawlersPageSource = readFileSync(
  new URL("../src/admin/CrawlersPage.tsx", import.meta.url),
  "utf8"
);
const crawlersPageLoadingSource = readFileSync(
  new URL("../src/admin/CrawlersPageLoading.tsx", import.meta.url),
  "utf8"
);
const drivesPageSource = readFileSync(
  new URL("../src/admin/DrivesPage.tsx", import.meta.url),
  "utf8"
);
const drivesPageLoadingSource = readFileSync(
  new URL("../src/admin/DrivesPageLoading.tsx", import.meta.url),
  "utf8"
);
const apiSource = readFileSync(
  new URL("../src/admin/api.ts", import.meta.url),
  "utf8"
);

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

function allRuleBodies(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return Array.from(css.matchAll(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`, "g")))
    .map((match) => match[1])
    .join("\n");
}

function lastRuleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const matches = Array.from(css.matchAll(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`, "g")));
  assert.ok(matches.length > 0, `Expected CSS rule for ${selector}`);
  return matches[matches.length - 1][1];
}

// ruleBodyByContains 处理 CSS 里"多 selector 共享 body"的合并写法：
//   .a, .b, .c {
//     ...
//   }
// 上面的 `.b` 用直接的 `selector\s*\{` 正则匹不到。这里改成"找到任何包含目标
// selector 的连续 selector 列表（可含逗号 + 空白），紧跟一个 { ... } body"。
//
// 仅支持 body 内不再嵌套 `{}`（admin.css 没有 nesting，足够）。
function ruleBodyByContains(css: string, needle: string): string {
  const escapedNeedle = needle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const re = new RegExp(`([^{}]*${escapedNeedle}[^{}]*)\\{([^}]*)\\}`, "g");
  const bodies: string[] = [];
  for (const m of css.matchAll(re)) {
    bodies.push(m[2]);
  }
  assert.ok(bodies.length > 0, `Expected at least one CSS rule containing ${needle}`);
  return bodies.join("\n");
}

function mobileCss(): string {
  const marker = "@media (max-width: 768px)";
  const start = adminCss.indexOf(marker);
  assert.notEqual(start, -1, "Expected mobile admin media query");
  return adminCss.slice(start);
}

test("admin login card fits narrow phone screens", () => {
  const body = ruleBody(loginCss, ".admin-login__card");

  // 桌面规则就用 min(...) 让窄屏自然适配；具体上限以 CSS 当前值为准（400px），
  // 关键是 `min(<某值>, 100%)` + `box-sizing: border-box`。
  assert.match(body, /width\s*:\s*min\(\d+px,\s*100%\)/);
  assert.match(body, /box-sizing\s*:\s*border-box/);
});

test("admin password fields use shared PasswordInput with eye toggle", () => {
  const passwordInputSource = readFileSync(
    new URL("../src/admin/PasswordInput.tsx", import.meta.url),
    "utf8"
  );
  const loginPageSource = readFileSync(
    new URL("../src/admin/LoginPage.tsx", import.meta.url),
    "utf8"
  );
  const driveFormSource = readFileSync(
    new URL("../src/admin/drive/DriveForm.tsx", import.meta.url),
    "utf8"
  );

  assert.match(passwordInputSource, /from "lucide-react"/);
  assert.match(passwordInputSource, /EyeOff/);
  assert.match(passwordInputSource, /type=\{visible \? "text" : "password"\}/);
  assert.match(passwordInputSource, /aria-label=\{visible \? "隐藏密码" : "显示密码"\}/);
  assert.match(loginPageSource, /import \{ PasswordInput \} from "\.\/PasswordInput"/);
  assert.match(loginPageSource, /<PasswordInput[\s\S]*?id="admin-login-password"/);
  assert.match(loginPageSource, /<PasswordInput[\s\S]*?id="admin-login-password-confirm"/);
  assert.doesNotMatch(loginPageSource, /type="password"/);
  assert.match(driveFormSource, /import \{ PasswordInput \} from "\.\.\/PasswordInput"/);
  assert.match(driveFormSource, /isSecretCredential\(f\.key\) \? \(/);
  assert.match(driveFormSource, /<PasswordInput[\s\S]*?id=\{`\$\{idPrefix\}-credential-\$\{f\.key\}`\}/);
  assert.doesNotMatch(usersPageSource, /type="password"/);
  assert.doesNotMatch(driveFormSource, /type="password"/);
});

test("admin tables scroll inside the mobile viewport", () => {
  const css = mobileCss();
  // 视频/标签等"长内容"表的 mobile 形态：用 `.admin-table:not(.admin-drives-table)`
  // 把它们改成 display:block 卡片栈；网盘表 .admin-drives-table 走另一组 1280px 媒体
  // 查询。这里只断"非 drives 表的 mobile 卡片化"。
  const body = ruleBody(css, ".admin-table:not(.admin-drives-table)");
  const card = ruleBody(css, ".admin-table:not(.admin-drives-table) tr");

  assert.match(body, /display\s*:\s*block/);
  assert.match(card, /background\s*:\s*var\(--bg-surface\)/);
});

test("mobile user management cards keep identity, metadata, and actions separated", () => {
  const css = mobileCss();
  const userCard = ruleBodyByContains(css, ".admin-users-table:not(.admin-drives-table) tr");
  const ipCard = ruleBodyByContains(css, ".admin-banned-ips-table:not(.admin-drives-table) tr");
  const username = ruleBody(css, ".admin-users-table:not(.admin-drives-table) .admin-users-table__username");
  const userRole = ruleBodyByContains(css, ".admin-users-table:not(.admin-drives-table) .admin-users-table__role");
  const userTime = ruleBody(css, ".admin-users-table:not(.admin-drives-table) .admin-users-table__time");
  const userActions = ruleBody(css, ".admin-users-table:not(.admin-drives-table) .admin-users-table__actions");
  const userStatus = ruleBody(css, ".admin-users-table:not(.admin-drives-table) .admin-status");
  const userStatusDot = ruleBody(css, ".admin-users-table:not(.admin-drives-table) .admin-status::before");
  const actionRow = ruleBody(css, ".admin-users-table__action-row");
  const createUserModal = ruleBody(adminCss, ".admin-modal.admin-modal--user-create");
  const createUserModalChrome = ruleBodyByContains(adminCss, ".admin-modal--user-create .admin-modal__header");
  const createUserSelect = ruleBody(adminCss, ".admin-modal--user-create .admin-form-select");
  const createUserSelectIcon = ruleBody(adminCss, ".admin-modal--user-create .admin-form-select__icon");
  const passwordResetModal = ruleBody(adminCss, ".admin-modal.admin-modal--password-reset");
  const passwordResetChrome = ruleBodyByContains(adminCss, ".admin-modal--password-reset .admin-modal__header");
  const passwordResetClose = ruleBody(adminCss, ".admin-modal--password-reset .admin-modal__header .admin-btn");
  const ipIdentity = ruleBody(css, ".admin-banned-ips-table:not(.admin-drives-table) .admin-banned-ips-table__ip");
  const ipReason = ruleBodyByContains(css, ".admin-banned-ips-table:not(.admin-drives-table) .admin-banned-ips-table__reason");
  const ipActions = ruleBody(css, ".admin-banned-ips-table:not(.admin-drives-table) .admin-banned-ips-table__actions");

  assert.match(usersPageSource, /className="admin-table admin-users-table"/);
  assert.match(usersPageSource, /className="admin-table admin-banned-ips-table"/);
  assert.match(usersPageSource, /data-label="用户名"/);
  assert.match(usersPageSource, /data-label="IP 地址"/);
  assert.doesNotMatch(usersPageSource, /<th>ID<\/th>/);
  assert.doesNotMatch(usersPageSource, /admin-users-table__id/);
  assert.match(usersPageSource, /title="创建用户"[\s\S]*?className="admin-modal--user-create"/);
  assert.match(createUserModal, /width\s*:\s*min\(380px,\s*100%\)/);
  assert.match(createUserModal, /border\s*:\s*0/);
  assert.match(createUserModal, /box-shadow\s*:\s*none/);
  assert.match(createUserModalChrome, /border\s*:\s*0/);
  assert.match(createUserModalChrome, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(usersPageSource, /className="admin-form-select"[\s\S]*?<ChevronDown size=\{15\} className="admin-form-select__icon"/);
  assert.match(createUserSelect, /padding-right\s*:\s*44px/);
  assert.match(createUserSelectIcon, /right\s*:\s*15px/);
  assert.match(usersPageSource, /title="重置密码"[\s\S]*?className="admin-modal--password-reset"/);
  assert.match(usersPageSource, /import \{ PasswordInput \} from "\.\/PasswordInput"/);
  assert.match(usersPageSource, /<PasswordInput[\s\S]*?value=\{createPassword\}/);
  assert.match(usersPageSource, /<PasswordInput[\s\S]*?value=\{resetPasswordValue\}/);
  assert.match(
    usersPageSource,
    /title="删除用户"[\s\S]*?message=\{`确定要删除用户「\$\{deleteConfirm\?\.username \?\? ""\}」吗\？`\}/
  );
  assert.doesNotMatch(usersPageSource, /此操作不可撤销/);
  assert.match(
    usersPageSource,
    /title="删除用户"\s*message=\{`确定要删除用户「\$\{deleteConfirm\?\.username \?\? ""\}」吗\？`\}\s*hideIcon\s*danger\s*modalClassName="admin-modal--user-delete"/
  );
  assert.doesNotMatch(
    usersPageSource,
    /title="删除用户"[\s\S]{0,200}?confirmText=/
  );
  assert.match(
    usersPageSource,
    /title="解除IP封禁"\s*message=\{`确定要解除 IP「\$\{unbanIPConfirm \?\? ""\}」的封禁吗\？`\}\s*hideIcon\s*modalClassName="admin-modal--ip-unban"/
  );
  assert.doesNotMatch(
    usersPageSource,
    /title="解除IP封禁"[\s\S]{0,200}?confirmText=/
  );
  assert.match(passwordResetModal, /width\s*:\s*min\(380px,\s*100%\)/);
  assert.match(ruleBodyByContains(adminCss, ".admin-modal.admin-modal--user-delete"), /width\s*:\s*min\(380px,\s*100%\)/);
  assert.match(ruleBodyByContains(adminCss, ".admin-modal.admin-modal--ip-unban"), /width\s*:\s*min\(380px,\s*100%\)/);
  assert.match(passwordResetModal, /border\s*:\s*0/);
  assert.match(passwordResetModal, /box-shadow\s*:\s*none/);
  assert.match(passwordResetChrome, /border\s*:\s*0/);
  assert.match(passwordResetChrome, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(passwordResetClose, /border-color\s*:\s*transparent/);
  assert.match(passwordResetClose, /background\s*:\s*transparent/);
  assert.match(ruleBody(adminControlsCss, ".admin-password-input"), /position\s*:\s*relative/);
  assert.match(ruleBody(adminControlsCss, ".admin-password-input input"), /padding-right\s*:\s*42px/);
  assert.match(ruleBody(adminControlsCss, ".admin-password-input__toggle"), /position\s*:\s*absolute/);
  assert.match(usersPageSource, /className="admin-btn admin-btn--small is-danger"/);
  assert.match(usersPageSource, /className="admin-btn admin-btn--small"[\s\S]*?title="解除封禁"[\s\S]*?>\s*解除封禁\s*<\/button>/);
  assert.doesNotMatch(usersPageSource, /className="admin-btn admin-btn--small is-primary"[\s\S]*?解除封禁/);
  assert.doesNotMatch(usersPageSource, /<CheckCircle size=\{14\} \/> 解除封禁/);
  assert.match(ruleBody(adminCss, ".admin-users-toolbar"), /display\s*:\s*flex/);
  assert.match(ruleBody(adminCss, ".admin-users-toolbar"), /justify-content\s*:\s*flex-end/);
  assert.match(ruleBody(adminCss, ".admin-users-toolbar"), /position\s*:\s*relative/);
  assert.match(ruleBody(adminCss, ".admin-users-tabs"), /position\s*:\s*absolute/);
  assert.match(ruleBody(adminCss, ".admin-users-tabs"), /left\s*:\s*50%/);
  assert.match(ruleBody(adminCss, ".admin-users-tabs"), /transform\s*:\s*translateX\(-50%\)/);
  assert.match(userCard, /grid-template-columns\s*:\s*repeat\(12,\s*minmax\(0,\s*1fr\)\)/);
  assert.match(userCard, /border-radius\s*:\s*var\(--radius-sm\)/);
  assert.match(ipCard, /grid-template-columns\s*:\s*repeat\(12,\s*minmax\(0,\s*1fr\)\)/);
  assert.match(username, /grid-column\s*:\s*1\s*\/\s*-1/);
  assert.match(username, /text-align\s*:\s*center/);
  assert.match(userRole, /grid-row\s*:\s*2/);
  assert.match(userRole, /justify-items\s*:\s*center/);
  assert.match(userRole, /text-align\s*:\s*center/);
  assert.match(userRole, /border-top\s*:\s*1px\s+solid\s+var\(--border-subtle\)/);
  assert.match(userTime, /grid-column\s*:\s*1\s*\/\s*-1/);
  assert.match(userTime, /grid-row\s*:\s*3/);
  assert.match(userActions, /grid-row\s*:\s*4/);
  assert.match(userStatus, /gap\s*:\s*0/);
  assert.match(userStatusDot, /content\s*:\s*none/);
  assert.match(userStatusDot, /display\s*:\s*none/);
  assert.match(actionRow, /grid-template-columns\s*:\s*repeat\(3,\s*minmax\(0,\s*1fr\)\)/);
  assert.match(ipIdentity, /grid-column\s*:\s*1\s*\/\s*-1/);
  assert.match(ipReason, /grid-row\s*:\s*2/);
  assert.match(ipReason, /border-top\s*:\s*1px\s+solid\s+var\(--border-subtle\)/);
  assert.match(ipActions, /grid-row\s*:\s*4/);
  assert.match(ipActions, /display\s*:\s*flex/);
  assert.match(ipActions, /justify-content\s*:\s*center/);
  assert.match(
    ruleBody(css, ".admin-banned-ips-table:not(.admin-drives-table) .admin-banned-ips-table__actions .admin-btn"),
    /width\s*:\s*auto/
  );
  assert.doesNotMatch(
    ruleBody(css, ".admin-banned-ips-table:not(.admin-drives-table) .admin-banned-ips-table__actions .admin-btn"),
    /width\s*:\s*100%/
  );
});

test("user management loading keeps fixed controls and leaves both tables blank", () => {
  assert.doesNotMatch(usersPageSource, /AdminLoading|加载中\.\.\./);
  assert.match(
    usersPageSource,
    /className="admin-page admin-page--with-floating-actions"\s+aria-busy=\{loading \|\| undefined\}/
  );
  assert.match(
    usersPageSource,
    /className="admin-users-toolbar"[\s\S]*?\{!loading && tab === "users" && \(\s*<div className="admin-table-wrap admin-users-table-wrap">\s*<table className="admin-table admin-users-table">/
  );
  assert.match(
    usersPageSource,
    /\{!loading && tab === "ips" && \(\s*<div className="admin-table-wrap admin-users-table-wrap">\s*<table className="admin-table admin-banned-ips-table">/
  );
});

test("user create action reuses the crawler-style floating button", () => {
  const sharedFab = ruleBody(adminCss, ".admin-create-fab");
  const sharedFabSurface = ruleBody(adminCss, ".admin-btn.admin-create-fab");
  const mobileSharedFab = lastRuleBody(adminCss, ".admin-create-fab");

  assert.match(
    usersPageSource,
    /\{tab === "users" && \(\s*<button\s+data-admin-floating-actions\s+type="button"\s+className="admin-btn admin-create-fab"\s+onClick=\{\(\) => setShowCreate\(true\)\}\s*>\s*<Plus size="1em" aria-hidden="true" \/>\s*新建用户/
  );
  assert.match(
    crawlersPageSource,
    /className="admin-btn admin-create-fab"[\s\S]*?<Plus size="1em" aria-hidden="true" \/>[\s\S]*?添加爬虫/
  );

  for (const declaration of [
    /position\s*:\s*fixed/,
    /right\s*:\s*var\(--space-7\)/,
    /bottom\s*:\s*var\(--space-5\)/,
    /min-height\s*:\s*44px/,
    /border-radius\s*:\s*12px/,
    /box-shadow\s*:\s*0 12px 32px/,
  ]) {
    assert.match(sharedFab, declaration);
  }

  assert.match(sharedFab, /width\s*:\s*fit-content/);
  assert.match(sharedFab, /min-width\s*:\s*0/);
  assert.match(sharedFabSurface, /background\s*:\s*transparent/);
  assert.match(mobileSharedFab, /right\s*:\s*var\(--space-3\)/);
  assert.match(mobileSharedFab, /bottom\s*:\s*calc\(var\(--space-3\) \+ env\(safe-area-inset-bottom\)\)/);
  assert.doesNotMatch(usersPageSource, /admin-users-create-fab|admin-users-toolbar-actions/);
  assert.doesNotMatch(adminCss, /admin-users-create-fab|admin-users-toolbar-actions/);
});

test("admin video management separates source navigation from the modal advanced filter", () => {
  const desktopRange = ruleBody(adminCss, ".admin-modal--video-filters .admin-video-advanced-range");
  const desktopRangeInputs = ruleBody(adminCss, ".admin-modal--video-filters .admin-video-advanced-range__inputs");
  const desktopRangeControls = ruleBodyByContains(
    adminCss,
    '.admin-modal--video-filters .admin-video-advanced-range input[type="date"]'
  );
  const currentFilter = ruleBodyByContains(adminCss, ".admin-videos-filter--current");
  const videoGrid = ruleBody(adminCss, ".admin-video-card-grid");
  const currentActions = ruleBody(adminCss, ".admin-videos-filter__current-actions");
  const advancedToggle = ruleBody(adminCss, ".admin-video-advanced-toggle");
  const desktopSearchAction = ruleBody(adminCss, ".admin-videos-filter__search-action");
  const desktopSearchActionHover = ruleBody(
    adminCss,
    ".admin-videos-filter__search-action:hover:not(:disabled)"
  );
  const currentTabSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频")
  );

  assert.doesNotMatch(videosPageSource, /label: "正常视频"|label: "当前视频"/);
  assert.doesNotMatch(videosPageSource, /function DriveFilter/);
  assert.doesNotMatch(videosPageSource, /admin-videos-filter__select/);
  assert.doesNotMatch(videosPageSource, /<h1 className="admin-page__title">视频管理<\/h1>/);
  assert.doesNotMatch(videosPageSource, /const \[driveId, setDriveId\]/);
  assert.match(videosPageSource, /className="admin-videos-filter__current-actions"/);
  assert.match(currentTabSource, /admin-videos-filter--current[\s\S]*?<SearchBox[\s\S]*?admin-videos-filter__current-actions[\s\S]*?className="admin-btn admin-videos-filter__search-action admin-video-advanced-toggle"/);
  assert.doesNotMatch(videosPageSource, /admin-videos-card-toolbar|AdvancedFilterButton/);
  assert.match(videosPageSource, /SlidersHorizontal/);
  assert.match(videosPageSource, /aria-haspopup="dialog"/);
  assert.match(
    currentTabSource,
    /className="admin-btn admin-videos-filter__search-action admin-video-advanced-toggle"[\s\S]*?<SlidersHorizontal size=\{15\} aria-hidden="true" \/>[\s\S]*?<span>筛选<\/span>/
  );
  assert.match(videosPageSource, /<Modal[\s\S]*?open=\{advancedFiltersOpen\}[\s\S]*?title="筛选"[\s\S]*?className="admin-modal--video-filters"/);
  assert.match(videosPageSource, /function AdvancedVideoFilters/);
  assert.match(videosPageSource, /function VideoSourceNavigation/);
  assert.match(videosPageSource, /aria-label="视频来源筛选"/);
  assert.match(videosPageSource, /label: "本地上传"/);
  assert.match(videosPageSource, /`drive:\$\{drive\.id\}`/);
  assert.match(videosPageSource, /`crawler:\$\{crawler\.id\}`/);
  assert.match(videosPageSource, />黑名单管理<\/span>/);
  assert.doesNotMatch(videosPageSource, /function VideoSourcePicker|<span>来源<\/span>/);
  assert.doesNotMatch(videosPageSource, /className="admin-video-upload-source-options"/);
  assert.doesNotMatch(videosPageSource, /<optgroup|<option value="">全部来源/);
  assert.doesNotMatch(videosPageSource, /getVideoStats|admin-video-tab__count/);
  assert.doesNotMatch(adminCss, /admin-videos-filter__select/);
  assert.match(adminCss, /\.admin-video-advanced-toggle\s*\{/);
  assert.match(currentFilter, /width\s*:\s*100%/);
  assert.match(videoGrid, /grid-template-columns\s*:\s*repeat\(auto-fill,\s*minmax\(min\(100%,\s*340px\),\s*1fr\)\)/);
  assert.match(currentActions, /grid-column\s*:\s*2/);
  assert.match(currentActions, /justify-self\s*:\s*start/);
  assert.match(currentActions, /display\s*:\s*inline-flex/);
  assert.doesNotMatch(advancedToggle, /grid-column|grid-row|justify-self/);
  assert.match(desktopSearchAction, /height\s*:\s*32px/);
  assert.match(desktopSearchAction, /border-radius\s*:\s*var\(--radius-pill\)/);
  assert.match(desktopSearchAction, /background\s*:\s*transparent/);
  assert.match(desktopSearchAction, /box-shadow\s*:\s*none/);
  assert.match(desktopSearchActionHover, /background\s*:\s*transparent/);
  assert.match(desktopSearchActionHover, /border-color\s*:\s*var\(--border-strong\)/);
  assert.doesNotMatch(adminCss, /\.admin-video-advanced-toggle\.is-active/);
  assert.match(adminCss, /\.admin-modal\.admin-modal--video-filters\s*\{/);
  assert.match(adminCss, /\.admin-modal\.admin-modal--video-filters\s*\{[^}]*border:\s*0;/s);
  assert.match(
    adminCss,
    /\.admin-modal--video-filters \.admin-modal__header,\s*\.admin-modal--video-filters \.admin-modal__footer\s*\{[^}]*background:\s*var\(--bg-surface\);/s
  );
  assert.match(adminCss, /\.admin-video-advanced-filters\s*\{/);
  assert.match(adminCss, /\.admin-video-source-nav\s*\{/);
  assert.match(adminCss, /\.admin-video-source-tab__glyph\.is-crawler,/);
  assert.doesNotMatch(adminCss, /\.admin-video-source-tab__icon\.is-(?:crawler|upload)/);
  assert.doesNotMatch(adminCss, /admin-video-source-picker/);
  assert.match(desktopRange, /grid-column\s*:\s*1\s*\/\s*-1/);
  assert.match(desktopRangeInputs, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\) auto minmax\(0,\s*1fr\)/);
  assert.match(desktopRangeInputs, /justify-content\s*:\s*stretch/);
  assert.match(desktopRangeInputs, /width\s*:\s*100%/);
  assert.match(desktopRangeControls, /width\s*:\s*100%/);
  assert.doesNotMatch(adminCss, /admin-video-tab__count/);
});

test("current video bulk actions use ordinary text buttons", () => {
  const currentVideosSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频 ----------")
  );
  const base = ruleBody(adminCss, ".admin-videos-bulk-actions__btn");

  assert.equal(Array.from(currentVideosSource.matchAll(/className="admin-btn admin-videos-bulk-actions__btn"/g)).length, 3);
  assert.match(currentVideosSource, /onClick=\{selectPageVideos\}[\s\S]*?disabled=\{listItems\.length === 0 \|\| allPageSelected\}[\s\S]*?>\s*全选本页\s*<\/button>/);
  assert.match(currentVideosSource, /onClick=\{\(\) => setSelectedIds\(new Set\(\)\)\}[\s\S]*?disabled=\{selectedIds\.size === 0\}[\s\S]*?>\s*取消选中\s*<\/button>/);
  assert.doesNotMatch(currentVideosSource, />\s*重生预览\s*<\/button>/);
  assert.doesNotMatch(currentVideosSource, /handleBatchRegen|batchRegenOpen|batchRegening|confirmBatchRegen/);
  assert.match(currentVideosSource, />\s*批量删除\s*<\/button>/);
  assert.doesNotMatch(currentVideosSource, /admin-videos-bulk-actions__mobile-exit|退出选择|批量选择|toggleSelectMode|selectMode/);
  assert.doesNotMatch(currentVideosSource, /className="admin-btn admin-videos-bulk-actions__btn"[^>]*>\s*<(?:RefreshCw|Trash2)/);
  assert.doesNotMatch(currentVideosSource, /is-primary admin-videos-bulk-actions__btn|is-danger admin-videos-bulk-actions__btn/);
  assert.match(base, /box-shadow\s*:\s*none/);
  assert.doesNotMatch(adminCss, /admin-videos-bulk-actions__btn\.is-danger/);
});

test("admin tag bulk actions use a fixed floating toolbar", () => {
  const css = mobileCss();
  const floatingSpace = ruleBody(adminCss, ".admin-page--with-floating-actions");
  const toolbarToggle = ruleBodyByContains(
    adminCss,
    ".admin-btn.admin-tags-toolbar-actions__toggle"
  );
  const bulkToolbarSource = tagsPageSource.slice(
    tagsPageSource.indexOf('className="admin-tags-bulk-toolbar"'),
    tagsPageSource.indexOf("<Modal", tagsPageSource.indexOf('className="admin-tags-bulk-toolbar"'))
  );
  const toolbar = ruleBodyByContains(adminCss, ".admin-tags-bulk-toolbar");
  const actions = ruleBody(adminCss, ".admin-tags-bulk-actions");
  const count = ruleBodyByContains(adminCss, ".admin-tags-bulk-actions__count");
  const mobileToolbar = lastRuleBody(css, ".admin-tags-bulk-toolbar");
  const mobileActions = allRuleBodies(css, ".admin-tags-bulk-actions");
  const mobileButton = allRuleBodies(css, ".admin-tags-bulk-actions__btn");

  assert.match(tagsPageSource, /className="admin-tags-bulk-toolbar"/);
  assert.match(
    tagsPageSource,
    /\{!selectMode && \(\s*<div className="admin-tags-toolbar-actions"[\s\S]*?className="admin-btn admin-tags-toolbar-actions__toggle"[\s\S]*?>\s*批量删除\s*<\/button>/
  );
  assert.match(toolbarToggle, /background\s*:\s*transparent/);
  assert.doesNotMatch(tagsPageSource, /admin-tags-toolbar-actions__toggle[^\n]*is-primary/);
  assert.match(tagsPageSource, /aria-label="标签批量操作"/);
  assert.match(tagsPageSource, /aria-pressed=\{isSelected\}/);
  assert.match(tagsPageSource, />已选择 \{selected\.size\} 项</);
  assert.match(tagsPageSource, /全选本页/);
  assert.match(tagsPageSource, /取消选中/);
  assert.match(tagsPageSource, /删除选中/);
  assert.match(
    bulkToolbarSource,
    /className="admin-btn admin-tags-bulk-actions__btn"\s+onClick=\{toggleSelectMode\}\s*>\s*退出批量\s*<\/button>/
  );
  assert.doesNotMatch(bulkToolbarSource, /is-primary|is-danger|mobile-exit/);
  assert.doesNotMatch(tagsPageSource, /<Trash2 size=\{13\} \/> \{bulkDeleting \? "删除中\.\.\." : "删除选中"\}/);
  assert.doesNotMatch(tagsPageSource, /className="admin-btn is-danger admin-tags-bulk-actions__btn"/);
  assert.doesNotMatch(tagsPageSource, /全选本页 \(/);
  assert.doesNotMatch(tagsPageSource, /CheckSquare/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-card__check/);
  assert.doesNotMatch(tagsPageSource, /admin-tags-bulk-actions__select-all/);
  assert.doesNotMatch(tagsPageSource, /checked=\{allSelected\}/);
  assert.doesNotMatch(tagsPageSource, /admin-tags-bulkbar/);
  assert.doesNotMatch(adminCss, /admin-tags-bulkbar/);
  assert.doesNotMatch(adminCss, /admin-tag-card__check/);
  assert.doesNotMatch(adminCss, /admin-tags-bulk-actions__btn\.is-danger/);
  assert.match(tagsPageSource, /data-admin-floating-actions/);
  assert.match(floatingSpace, /padding-bottom\s*:\s*var\(--admin-floating-actions-space,\s*0px\)/);
  assert.match(toolbar, /position\s*:\s*fixed/);
  assert.match(toolbar, /left\s*:\s*calc\(50%\s*\+\s*\(var\(--admin-sidebar-width\)\s*\/\s*2\)\)/);
  assert.match(toolbar, /right\s*:\s*auto/);
  assert.match(toolbar, /bottom\s*:\s*calc\(12px\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(toolbar, /max-width\s*:\s*calc\(100vw\s*-\s*var\(--admin-sidebar-width\)\s*-\s*24px\)/);
  assert.match(toolbar, /margin\s*:\s*0/);
  assert.match(toolbar, /padding\s*:\s*10px 14px/);
  assert.match(toolbar, /border\s*:\s*1px solid var\(--border-default\)/);
  assert.match(toolbar, /border-radius\s*:\s*16px/);
  assert.match(toolbar, /background\s*:\s*color-mix\(in srgb,\s*var\(--bg-surface\) 88%,\s*transparent\)/);
  assert.match(toolbar, /box-shadow\s*:\s*var\(--shadow-lg\)/);
  assert.match(toolbar, /transform\s*:\s*translateX\(-50%\)/);
  assert.match(toolbar, /backdrop-filter\s*:\s*blur\(14px\)/);
  assert.match(actions, /display\s*:\s*inline-flex/);
  assert.match(count, /white-space\s*:\s*nowrap/);
  assert.match(mobileToolbar, /left\s*:\s*var\(--space-3\)/);
  assert.match(mobileToolbar, /right\s*:\s*var\(--space-3\)/);
  assert.match(mobileToolbar, /bottom\s*:\s*calc\(var\(--space-3\)\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(mobileToolbar, /border\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(mobileToolbar, /border-radius\s*:\s*12px/);
  assert.match(mobileToolbar, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(mobileToolbar, /box-shadow\s*:\s*0 12px 32px rgba\(0,\s*0,\s*0,\s*0\.34\)/);
  assert.match(mobileToolbar, /transform\s*:\s*none/);
  assert.match(mobileToolbar, /backdrop-filter\s*:\s*none/);
  assert.doesNotMatch(css, /\.admin-tags-page\.has-bulk-actions\s*\{[^}]*padding-bottom/s);
  assert.doesNotMatch(adminCss, /\.admin-tags-page\.has-bulk-actions \.admin-tags-toolbar-actions/);
  assert.match(mobileActions, /display\s*:\s*grid/);
  assert.match(mobileActions, /grid-template-columns\s*:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\)/);
  assert.match(count, /grid-column\s*:\s*1 \/ -1/);
  assert.match(mobileButton, /min-height\s*:\s*40px/);
  assert.match(mobileButton, /min-width\s*:\s*0/);
  assert.doesNotMatch(adminCss, /admin-tags-bulk-actions__mobile-exit/);
});

test("admin tag auto-generation setting is removed", () => {
  assert.doesNotMatch(tagsPageSource, /admin-tag-setting-toggle__switch/);
  assert.doesNotMatch(tagsPageSource, /role="switch"/);
  assert.doesNotMatch(tagsPageSource, /onClick=\{toggleAutoGenerateTags\}/);
  assert.doesNotMatch(tagsPageSource, /autoGenerateTagsEnabled/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-setting-toggle__hint/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-setting-toggle__body/);
  assert.doesNotMatch(tagsPageSource, /<label\s+className=\{`admin-tag-setting-toggle/);
  assert.doesNotMatch(adminCss, /admin-tag-setting-toggle/);
});

test("admin sidebar active item frame only wraps the centered option", () => {
  const nav = ruleBody(adminCss, ".admin-nav");
  const navIcon = ruleBody(adminCss, ".admin-nav__icon");
  const navLink = ruleBody(adminCss, ".admin-nav__link");
  const activeMarker = ruleBody(adminCss, ".admin-nav__link.is-active::before");

  assert.match(nav, /justify-content\s*:\s*space-evenly/);
  assert.match(nav, /gap\s*:\s*var\(--space-4\)/);
  assert.match(navIcon, /display\s*:\s*none/);
  assert.match(navLink, /align-self\s*:\s*center/);
  assert.match(navLink, /width\s*:\s*fit-content/);
  assert.match(navLink, /max-width\s*:\s*100%/);
  assert.match(activeMarker, /content\s*:\s*none/);
  assert.match(activeMarker, /display\s*:\s*none/);
});

test("desktop admin sidebar uses a fully transparent glass surface without changing the mobile drawer", () => {
  const desktopSidebarMatch = adminCss.match(
    /@media \(min-width:\s*769px\)\s*\{\s*\.admin-sidebar\s*\{([^}]*)\}/
  );
  assert.ok(desktopSidebarMatch, "Expected a desktop-only sidebar rule");
  const desktopSidebar = desktopSidebarMatch[1];
  const mobileSidebar = ruleBody(mobileCss(), ".admin-sidebar");

  assert.match(desktopSidebar, /background\s*:\s*transparent/);
  assert.doesNotMatch(desktopSidebar, /background\s*:[^;]*var\(--glass-sidebar/);
  assert.match(desktopSidebar, /border-right\s*:\s*1px solid var\(--glass-sidebar-border\)/);
  assert.match(desktopSidebar, /box-shadow\s*:[\s\S]*var\(--glass-sidebar-shadow\)/);
  assert.match(desktopSidebar, /backdrop-filter\s*:\s*saturate\(145%\) blur\(22px\)/);
  assert.match(desktopSidebar, /-webkit-backdrop-filter\s*:\s*saturate\(145%\) blur\(22px\)/);
  assert.equal(themeTokensCss.match(/--glass-sidebar-highlight\s*:/g)?.length, 3);
  assert.equal(themeTokensCss.match(/--glass-sidebar-border\s*:/g)?.length, 3);
  assert.equal(themeTokensCss.match(/--glass-sidebar-shadow\s*:/g)?.length, 3);
  assert.match(mobileSidebar, /background\s*:\s*var\(--bg-surface\)/);
});

test("admin sidebar keeps desktop separators but groups the mobile drawer with labels", () => {
  assert.match(
    adminCss,
    /\.admin-nav__group > \.admin-nav__link \+ \.admin-nav__link::after\s*\{[^}]*content\s*:\s*"";[^}]*height\s*:\s*1px;[^}]*background\s*:\s*var\(--border-subtle\)/s
  );
  const css = mobileCss();
  const mobileSeparator = ruleBody(css, ".admin-nav__group > .admin-nav__link + .admin-nav__link::after");
  assert.match(mobileSeparator, /content\s*:\s*none/);
  assert.match(mobileSeparator, /display\s*:\s*none/);
  assert.match(ruleBody(css, ".admin-nav__group-label"), /display\s*:\s*block/);
  assert.doesNotMatch(
    css,
    /\.admin-nav__group \+ \.admin-nav__group > \.admin-nav__link:first-of-type::after/
  );
});

test("current video list does not render the drive summary under filters", () => {
  const shell = ruleBody(adminCss, ".admin-shell");
  const navGroupLabel = ruleBody(adminCss, ".admin-nav__group-label");
  const navLink = ruleBody(adminCss, ".admin-nav__link");
  const navText = ruleBody(adminCss, ".admin-nav__text");
  const pagination = ruleBody(adminCss, ".admin-table-pagination");
  const filter = ruleBody(adminCss, ".admin-videos-filter");
  const toolbar = ruleBody(adminCss, ".admin-videos-list-toolbar");
  const currentToolbar = ruleBodyByContains(adminCss, ".admin-videos-current .admin-videos-list-toolbar");

  assert.doesNotMatch(videosPageSource, /listSummary/);
  assert.doesNotMatch(videosPageSource, /全部网盘：共/);
  assert.doesNotMatch(videosPageSource, /withCounts/);
  assert.doesNotMatch(videosPageSource, /teaserReadyCount|teaserPendingCount/);
  assert.match(videosPageSource, /admin-videos-filter admin-videos-filter--current/);
  assert.doesNotMatch(videosPageSource, /aria-label="刷新当前视频"/);
  const currentVideosSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideosTab"),
    videosPageSource.indexOf("// ---------- 拉黑视频 ----------")
  );
  assert.doesNotMatch(currentVideosSource, /selectMode|toggleSelectMode|批量选择|退出选择/);
  assert.doesNotMatch(currentVideosSource, /admin-videos-filter__batch/);
  assert.match(currentVideosSource, /admin-videos-current\$\{selectedIds\.size > 0 \? " has-bulk-actions" : ""\}/);
  assert.match(currentVideosSource, /\{selectedIds\.size > 0 && \(/);
  assert.match(shell, /--admin-sidebar-width\s*:\s*232px/);
  assert.match(shell, /grid-template-columns\s*:\s*var\(--admin-sidebar-width\)\s+minmax\(0,\s*1fr\)/);
  assert.match(navGroupLabel, /padding\s*:\s*0\s+12px/);
  assert.match(navGroupLabel, /text-align\s*:\s*left/);
  assert.match(navLink, /display\s*:\s*flex/);
  assert.match(navLink, /justify-content\s*:\s*flex-start/);
  assert.match(navLink, /text-align\s*:\s*left/);
  assert.match(navText, /justify-items\s*:\s*start/);
  assert.match(pagination, /justify-content\s*:\s*center/);
  assert.match(filter, /margin-bottom\s*:\s*var\(--space-4\)/);
  assert.match(toolbar, /margin\s*:\s*var\(--space-2\)\s+0\s+var\(--space-4\)/);
  assert.match(currentToolbar, /position\s*:\s*fixed/);
  assert.match(currentToolbar, /left\s*:\s*calc\(50%\s*\+\s*\(var\(--admin-sidebar-width\)\s*\/\s*2\)\)/);
  assert.match(currentToolbar, /right\s*:\s*auto/);
  assert.match(currentToolbar, /bottom\s*:\s*calc\(12px\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(currentToolbar, /max-width\s*:\s*calc\(100vw\s*-\s*var\(--admin-sidebar-width\)\s*-\s*24px\)/);
  assert.match(currentToolbar, /margin\s*:\s*0/);
  assert.match(currentToolbar, /border-radius\s*:\s*16px/);
  assert.match(currentToolbar, /background\s*:\s*color-mix\(in srgb,\s*var\(--bg-surface\) 88%,\s*transparent\)/);
  assert.match(currentToolbar, /transform\s*:\s*translateX\(-50%\)/);
  assert.match(currentToolbar, /backdrop-filter\s*:\s*blur\(14px\)/);
  assert.match(videosPageSource, /className="admin-videos-list-toolbar" data-admin-floating-actions/);
  assert.match(ruleBody(adminCss, ".admin-page--with-floating-actions"), /--admin-floating-actions-space/);
});

test("desktop current video list uses a CPA-style responsive card grid", () => {
  const grid = ruleBody(adminCss, ".admin-video-card-grid");
  const card = ruleBody(adminCss, ".admin-video-card");
  const titleCell = ruleBody(adminCss, ".admin-video-card .admin-video-title-cell");
  const thumb = ruleBody(adminCss, ".admin-video-card .admin-video-thumb-wrap");
  const title = ruleBody(adminCss, ".admin-video-card .admin-video-title");
  const pills = ruleBody(adminCss, ".admin-video-card .admin-video-filemeta-pills");
  const meta = ruleBody(adminCss, ".admin-video-card__meta");
  const actions = ruleBody(adminCss, ".admin-video-card__actions");
  const utilityActions = ruleBody(adminCss, ".admin-video-card__utility-actions");
  const selectionControl = ruleBody(adminCss, ".admin-video-card__select");
  const selectionInput = ruleBody(adminCss, ".admin-video-card__select-input");
  const selectionBox = ruleBody(adminCss, ".admin-video-card__select-box");
  const checkedSelectionBox = ruleBody(adminCss, ".admin-video-card__select-box.is-checked");
  const actionButton = ruleBody(adminCss, ".admin-video-action-icon-button");
  const dangerButton = ruleBody(adminCss, ".admin-video-action-icon-button.is-danger");
  const hoverCard = ruleBody(adminCss, ".admin-video-card:hover");
  const deleteIconButtonSource = videosPageSource.slice(
    videosPageSource.indexOf("function VideoDeleteIconButton"),
    videosPageSource.indexOf("function CurrentVideoCard")
  );
  const currentCardSource = videosPageSource.slice(
    videosPageSource.indexOf("function CurrentVideoCard"),
    videosPageSource.indexOf("function VideoTitleCell")
  );

  assert.match(
    videosPageSource,
    /className="admin-video-card-grid admin-videos-results__content"[\s\S]{0,80}?role="list"[\s\S]{0,80}?aria-label="视频列表"/
  );
  assert.match(videosPageSource, /<CurrentVideoCard[\s\S]*?video=\{v\}/);
  assert.match(currentCardSource, /<article className="admin-video-card" role="listitem">/);
  assert.match(currentCardSource, /<footer className="admin-video-card__actions">/);
  assert.match(currentCardSource, /className="admin-video-card__utility-actions"[\s\S]*?className="admin-btn admin-video-action-icon-button"[\s\S]*?<Edit size=\{15\}[\s\S]*?<VideoDeleteIconButton[\s\S]*?onClick=\{onDelete\}[\s\S]*?title="删除视频"[\s\S]*?ariaLabel="删除视频"[\s\S]*?className="admin-video-card__select"/);
  assert.match(deleteIconButtonSource, /className="admin-btn admin-video-action-icon-button is-danger"[\s\S]*?<Trash2 size=\{15\} aria-hidden="true" \/>/);
  assert.equal(Array.from(videosPageSource.matchAll(/<VideoDeleteIconButton/g)).length, 2);
  assert.match(currentCardSource, /className="admin-video-card__select"[\s\S]*?className="admin-video-card__select-input"[\s\S]*?type="checkbox"[\s\S]*?checked=\{selected\}[\s\S]*?onChange=\{onToggleSelect\}/);
  assert.match(currentCardSource, /admin-video-card__select-box\$\{selected \? " is-checked" : ""\}`\}[\s\S]*?aria-hidden="true"[\s\S]*?selected && <Check size=\{12\}/);
  assert.doesNotMatch(currentCardSource, /selectMode|is-selected|aria-selected|role=\{selectMode \? "option"|onKeyDown|onClick=\{\(event\)/);
  assert.doesNotMatch(adminCss, /\.admin-video-card\.is-selected|\.admin-video-card-grid\.is-select-mode/);
  assert.doesNotMatch(videosPageSource, /admin-table is-selectable admin-videos-table/);
  assert.doesNotMatch(videosPageSource, /<th>标题<\/th>|<th>作者<\/th>|<th>时长<\/th>|<th>预览视频<\/th>/);
  assert.doesNotMatch(videosPageSource, /data-label="预览视频"[\s\S]*?<PreviewStatus/);
  assert.match(grid, /display\s*:\s*grid/);
  assert.match(grid, /grid-template-columns\s*:\s*repeat\(auto-fill,\s*minmax\(min\(100%,\s*340px\),\s*1fr\)\)/);
  assert.match(grid, /gap\s*:\s*16px/);
  assert.match(card, /display\s*:\s*flex/);
  assert.match(card, /flex-direction\s*:\s*column/);
  assert.match(card, /padding\s*:\s*16px/);
  assert.match(card, /border-radius\s*:\s*14px/);
  assert.match(card, /--admin-video-card-bg\s*:\s*color-mix\(in srgb,\s*var\(--bg-surface\) 82%,\s*transparent\)/);
  assert.match(card, /height\s*:\s*100%/);
  assert.match(titleCell, /grid-template-columns\s*:\s*clamp\(112px,\s*38%,\s*156px\)\s+minmax\(0,\s*1fr\)/);
  assert.match(thumb, /aspect-ratio\s*:\s*16\s*\/\s*9/);
  assert.match(title, /-webkit-line-clamp\s*:\s*2/);
  assert.match(pills, /display\s*:\s*flex/);
  assert.match(meta, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)\s+auto/);
  assert.match(meta, /margin\s*:\s*auto\s+0\s+0/);
  assert.match(actions, /display\s*:\s*flex/);
  assert.match(actions, /justify-content\s*:\s*space-between/);
  assert.match(actions, /padding-top\s*:\s*12px/);
  assert.match(utilityActions, /display\s*:\s*flex/);
  assert.match(utilityActions, /gap\s*:\s*4px/);
  assert.match(selectionControl, /width\s*:\s*32px/);
  assert.match(selectionControl, /height\s*:\s*32px/);
  assert.match(selectionInput, /clip-path\s*:\s*inset\(50%\)/);
  assert.match(selectionBox, /width\s*:\s*22px/);
  assert.match(selectionBox, /border-radius\s*:\s*7px/);
  assert.match(checkedSelectionBox, /background\s*:\s*var\(--accent\)/);
  assert.match(checkedSelectionBox, /color\s*:\s*var\(--text-on-accent\)/);
  assert.match(actionButton, /width\s*:\s*30px/);
  assert.match(actionButton, /height\s*:\s*30px/);
  assert.match(actionButton, /background\s*:\s*transparent/);
  assert.match(actionButton, /border\s*:\s*1px solid var\(--admin-video-card-pill-border,\s*var\(--border-subtle\)\)/);
  assert.match(dangerButton, /border-color\s*:\s*color-mix\(in srgb,\s*var\(--admin-video-card-danger,\s*var\(--danger\)\) 26%,\s*transparent\)/);
  assert.match(dangerButton, /color\s*:\s*color-mix\(in srgb,\s*var\(--admin-video-card-danger,\s*var\(--danger\)\) 68%,\s*transparent\)/);
  assert.match(hoverCard, /transform\s*:\s*translateY\(-1px\)/);
});

test("desktop video management toolbar keeps its layout and reuses the home search style", () => {
  const css = adminCss;
  const currentFilter = ruleBodyByContains(css, ".admin-videos-filter--current");
  const blacklistFilter = ruleBodyByContains(css, ".admin-videos-filter--blacklist");
  const currentFilterSearch = ruleBodyByContains(css, ".admin-videos-filter--current .admin-videos-filter__search");
  const blacklistFilterSearch = ruleBodyByContains(css, ".admin-videos-filter--blacklist .admin-videos-filter__search");
  const adminSearch = allRuleBodies(css, ".admin-videos-filter__search");
  const sharedSearchForm = ruleBody(searchCss, ".search-panel--uiverse");
  const sharedSearchInput = ruleBody(searchCss, ".search-panel--uiverse .search-panel__uiverse-input");
  const currentActions = ruleBody(css, ".admin-videos-filter__current-actions");
  const actions = ruleBody(css, ".admin-videos-filter__actions");
  const batch = ruleBody(css, ".admin-videos-filter__batch");
  const sourceNav = ruleBody(css, ".admin-video-source-nav");
  const sourceTabs = ruleBody(css, ".admin-video-source-tabs");
  const sourceTab = ruleBody(css, ".admin-video-source-tab");
  const activeSourceTab = ruleBody(css, ".admin-video-source-tab.is-active");
  const activeSourceTabIndicator = ruleBody(css, ".admin-video-source-tab.is-active::after");
  const blacklistEntry = ruleBody(css, ".admin-video-source-nav__blacklist");
  const blacklistEntryHover = ruleBody(css, ".admin-video-source-nav__blacklist:hover");
  const activeBlacklistEntry = ruleBody(css, ".admin-video-source-nav__blacklist.is-active");

  assert.doesNotMatch(videosPageSource, /aria-label="刷新当前视频"/);
  assert.doesNotMatch(videosPageSource, /aria-label="刷新拉黑视频"/);
  assert.match(videosPageSource, /admin-videos-filter__batch/);
  assert.doesNotMatch(videosPageSource, /CheckSquare/);
  assert.match(currentFilter, /display\s*:\s*grid/);
  assert.match(currentFilter, /grid-template-columns\s*:\s*minmax\(240px,\s*360px\)\s+auto\s+minmax\(0,\s*1fr\)/);
  assert.match(currentFilter, /width\s*:\s*100%/);
  assert.match(blacklistFilter, /display\s*:\s*grid/);
  assert.match(blacklistFilter, /grid-template-columns\s*:\s*minmax\(240px,\s*360px\)\s+auto\s+minmax\(0,\s*1fr\)/);
  assert.match(blacklistFilter, /width\s*:\s*100%/);
  assert.match(
    videosPageSource,
    /<VideoSourceNavigation[\s\S]*?activeSourceKey=\{activeSourceKey\}[\s\S]*?blacklistActive=\{activeView === "blacklist"\}[\s\S]*?activeView === "current"[\s\S]*?<CurrentVideosTab/
  );
  assert.match(
    videosPageSource,
    /<VideoSourceNavigation[\s\S]*?activeView === "blacklist"[\s\S]*?<BlacklistTab/
  );
  assert.doesNotMatch(videosPageSource, /VideoTabSelector|selectTab|tabSelector/);
  assert.match(currentFilterSearch, /min-width\s*:\s*0/);
  assert.match(currentFilterSearch, /grid-column\s*:\s*1/);
  assert.match(currentFilterSearch, /justify-self\s*:\s*start/);
  assert.match(blacklistFilterSearch, /min-width\s*:\s*0/);
  assert.match(blacklistFilterSearch, /grid-column\s*:\s*1/);
  assert.match(blacklistFilterSearch, /justify-self\s*:\s*start/);
  assert.match(videosPageSource, /<SearchPanel[\s\S]*?className="admin-videos-filter__search search-panel--transparent"[\s\S]*?variant="uiverse"/);
  assert.match(adminSearch, /margin-top\s*:\s*0/);
  assert.match(sharedSearchForm, /--width-of-input\s*:\s*min\(100%,\s*360px\)/);
  assert.match(sharedSearchForm, /width\s*:\s*var\(--width-of-input\)/);
  assert.match(sharedSearchForm, /background\s*:\s*var\(--input-bg, var\(--bg-surface\)\)/);
  assert.match(sharedSearchInput, /padding-inline\s*:\s*0\.5em/);
  assert.match(sharedSearchInput, /background-color\s*:\s*transparent/);
  assert.doesNotMatch(adminCss, /\.admin-videos-filter__search input\s*\{/);
  assert.match(currentActions, /grid-column\s*:\s*2/);
  assert.match(currentActions, /display\s*:\s*inline-flex/);
  assert.match(actions, /grid-column\s*:\s*2/);
  assert.match(actions, /justify-self\s*:\s*start/);
  assert.match(actions, /display\s*:\s*inline-flex/);
  assert.match(actions, /justify-content\s*:\s*flex-end/);
  assert.match(batch, /grid-column\s*:\s*3/);
  assert.match(batch, /justify-self\s*:\s*end/);
  assert.match(batch, /white-space\s*:\s*nowrap/);
  assert.doesNotMatch(batch, /display\s*:\s*none/);
  assert.match(sourceNav, /display\s*:\s*grid/);
  assert.match(sourceNav, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\) auto/);
  assert.match(sourceNav, /width\s*:\s*100%/);
  assert.match(sourceNav, /border-bottom\s*:\s*1px solid var\(--border-default\)/);
  assert.match(sourceNav, /background\s*:\s*transparent/);
  assert.match(sourceNav, /margin\s*:\s*0\s+0\s+14px/);
  assert.match(sourceTabs, /overflow-x\s*:\s*auto/);
  assert.match(sourceTabs, /min-width\s*:\s*0/);
  assert.match(sourceTab, /padding\s*:\s*8px\s+12px\s+11px/);
  assert.match(sourceTab, /font-size\s*:\s*var\(--font-sm\)/);
  assert.match(activeSourceTab, /background\s*:\s*transparent/);
  assert.match(activeSourceTab, /color\s*:\s*var\(--text-strong\)/);
  assert.match(activeSourceTabIndicator, /background\s*:\s*var\(--accent\)/);
  assert.match(blacklistEntry, /border\s*:\s*1px solid var\(--border-default\)/);
  assert.match(blacklistEntry, /border-radius\s*:\s*var\(--radius-pill\)/);
  assert.match(blacklistEntry, /background\s*:\s*transparent/);
  assert.match(blacklistEntry, /white-space\s*:\s*nowrap/);
  assert.match(blacklistEntryHover, /background\s*:\s*transparent/);
  assert.match(activeBlacklistEntry, /background\s*:\s*transparent/);
});

test("admin table action headers center-align with action buttons", () => {
  const actionHeader = ruleBody(adminCss, ".admin-table th.is-actions");
  const actionCell = ruleBody(adminCss, ".admin-table td.is-actions");

  assert.match(actionHeader, /text-align\s*:\s*center/);
  assert.match(actionCell, /text-align\s*:\s*center/);
});

test("desktop table cell surfaces follow the rounded frame without clipping sticky headers", () => {
  const table = ruleBody(adminCss, ".admin-table");
  const header = ruleBody(adminCss, ".admin-table th");
  const topLeft = ruleBodyByContains(
    adminCss,
    ".admin-table > thead:first-child > tr:first-child > th:first-child"
  );
  const topRight = ruleBodyByContains(
    adminCss,
    ".admin-table > thead:first-child > tr:first-child > th:last-child"
  );
  const bottomLeft = ruleBody(
    adminCss,
    ".admin-table > tbody:last-child > tr:last-child > td:first-child"
  );
  const bottomRight = ruleBody(
    adminCss,
    ".admin-table > tbody:last-child > tr:last-child > td:last-child"
  );

  assert.match(table, /border-collapse\s*:\s*separate/);
  assert.match(table, /border-spacing\s*:\s*0/);
  assert.match(table, /border-radius\s*:\s*var\(--radius-md\)/);
  assert.match(table, /overflow\s*:\s*visible/);
  assert.match(header, /position\s*:\s*sticky/);
  assert.match(header, /top\s*:\s*0/);
  assert.match(topLeft, /border-top-left-radius\s*:\s*calc\(var\(--radius-md\) - 1px\)/);
  assert.match(topRight, /border-top-right-radius\s*:\s*calc\(var\(--radius-md\) - 1px\)/);
  assert.match(bottomLeft, /border-bottom-left-radius\s*:\s*calc\(var\(--radius-md\) - 1px\)/);
  assert.match(bottomRight, /border-bottom-right-radius\s*:\s*calc\(var\(--radius-md\) - 1px\)/);
  assert.match(
    adminCss,
    /@media \(min-width:\s*769px\)\s*\{\s*\.admin-table > thead:first-child > tr:first-child > th:first-child/s
  );
  assert.equal(Array.from(adminCss.matchAll(/\.admin-table\s*\{/g)).length, 1);
});

test("current video delete dialogs use flat modal chrome", () => {
  const flatModal = ruleBodyByContains(adminCss, ".admin-modal--video-delete-flat");
  const flatModalChrome = ruleBodyByContains(adminCss, ".admin-modal--video-delete-flat .admin-modal__header");
  const deleteSourceOption = ruleBody(adminCss, ".admin-delete-source-option");

  assert.equal(
    Array.from(videosPageSource.matchAll(/modalClassName="admin-modal--delete-confirm admin-modal--video-delete-flat"/g)).length,
    2
  );
  assert.match(flatModal, /border\s*:\s*0/);
  assert.match(flatModal, /box-shadow\s*:\s*none/);
  assert.match(flatModalChrome, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(flatModalChrome, /border\s*:\s*0/);
  assert.doesNotMatch(videosPageSource, /开启后会先删除源文件，失败则不会删除管理库记录。/);
  assert.doesNotMatch(videosPageSource, /开启后会先删除源文件，失败的视频会保留管理库记录。/);
  assert.match(videosPageSource, /title="删除视频"[\s\S]*?confirmText="确认"/);
  assert.match(videosPageSource, /title="批量删除视频"[\s\S]*?confirmText="确认"/);
  assert.doesNotMatch(videosPageSource, /confirmText="删除视频"|confirmText="批量删除"/);
  assert.match(deleteSourceOption, /padding\s*:\s*0/);
  assert.match(deleteSourceOption, /border\s*:\s*0/);
  assert.match(deleteSourceOption, /background\s*:\s*transparent/);
});

test("blacklist cancel action uses ordinary button styling", () => {
  const unavailable = ruleBody(adminCss, ".admin-blacklist-unavailable");

  assert.doesNotMatch(videosPageSource, /const \[driveId, setDriveId\] = useState\(""\);/);
  assert.match(videosPageSource, /api\.listBlacklist\(\{ page, size: pageSize, keyword: searchKeyword \}\)/);
  assert.match(videosPageSource, /admin-videos-filter admin-videos-filter--blacklist/);
  assert.doesNotMatch(videosPageSource, /<DriveFilter/);
  assert.match(apiSource, /listBlacklist\(\s*params: \{ driveId\?: string; page\?: number; size\?: number; keyword\?: string \}/);
  assert.match(apiSource, /if \(params\.driveId\) qs\.set\("driveId", params\.driveId\);/);
  assert.match(videosPageSource, /className="admin-btn"[\s\S]*?onClick=\{\(\) => setRemoveTarget\(v\)\}[\s\S]*?取消拉黑/);
  assert.doesNotMatch(videosPageSource, /admin-blacklist-restore-btn/);
  assert.doesNotMatch(videosPageSource, /RotateCcw/);
  assert.doesNotMatch(adminCss, /admin-blacklist-restore-btn/);
  assert.match(videosPageSource, /v\.restorePolicy !== "none"/);
  assert.match(videosPageSource, /取消拉黑/);
  assert.doesNotMatch(videosPageSource, /重新入库/);
  assert.match(
    videosPageSource,
    /open=\{removeTarget !== null\}[\s\S]*?确定取消拉黑「\$\{removeTarget\.fileName \|\| removeTarget\.id\}」吗？/
  );
  assert.doesNotMatch(videosPageSource, /视频将在下次扫盘时恢复/);
  assert.doesNotMatch(videosPageSource, /此操作不会立即运行爬虫/);
  assert.match(videosPageSource, /v\.sourceDeleted/);
  assert.doesNotMatch(
    videosPageSource,
    /<span className="admin-blacklist-reason-pill">本地上传<\/span>/
  );
  assert.doesNotMatch(videosPageSource, /被删除和被隐藏的视频会进入黑名单/);
  assert.doesNotMatch(videosPageSource, /原始记录、封面、预览已删除/);
  assert.match(unavailable, /color\s*:\s*var\(--text-faint\)/);
});

// 本地上传的源文件被保留，但这个盘不支持枚举，扫盘和爬虫都不会重新发现它，
// 所以取消拉黑是当场恢复，文案不能再说「等下次扫盘」。
test("local upload blacklist entries restore immediately instead of waiting for a scan", () => {
  assert.match(apiSource, /restorePolicy: "none" \| "scan" \| "crawler" \| "direct"/);
  assert.match(videosPageSource, /target\.restorePolicy === "direct"[\s\S]*?已取消拉黑，视频已恢复到媒体库/);
  assert.match(videosPageSource, /open=\{removeTarget !== null\}[\s\S]*?confirmText="确认"/);
  assert.doesNotMatch(videosPageSource, /视频将立即恢复到媒体库，封面和预览会重新生成。/);
  // 恢复按钮对 direct 一样要出现：判断条件是「不等于 none」而不是白名单。
  assert.match(videosPageSource, /v\.restorePolicy !== "none"/);
  assert.match(videosPageSource, /v\.restorePolicy !== "none"[\s\S]*?disabled=\{sourceDeleteRunning\}/);
  // 「不可自动恢复」这个中间态没有了：现在要么能恢复，要么就是真的不可恢复。
  assert.doesNotMatch(videosPageSource, /不可自动恢复/);
});

test("blacklist source deletion reports tombstones skipped after restore", () => {
  assert.match(apiSource, /skipped: number/);
  assert.match(videosPageSource, /status\.skipped > 0[\s\S]*?跳过 \$\{status\.skipped\}/);
});

test("blacklist duplicate reason renders as a compact pill", () => {
  const pill = ruleBody(adminCss, ".admin-blacklist-reason-pill");

  assert.match(videosPageSource, /admin-blacklist-reason-pill/);
  assert.match(videosPageSource, /重复文件/);
  assert.match(videosPageSource, /v\.canonicalVideoId/);
  assert.match(videosPageSource, /查看保留视频/);
  assert.doesNotMatch(videosPageSource, /保留视频不可用/);
  assert.doesNotMatch(videosPageSource, /admin-blacklist-canonical-btn/);
  assert.doesNotMatch(videosPageSource, /<ExternalLink[\s\S]*?查看保留视频/);
  assert.match(videosPageSource, /className="admin-btn"[\s\S]*?查看保留视频/);
  assert.match(pill, /border-radius\s*:\s*999px/);
  assert.match(pill, /white-space\s*:\s*nowrap/);
  assert.doesNotMatch(adminCss, /admin-blacklist-canonical-btn/);
});

test("blacklist source files can be deleted by one serialized background task", () => {
  const blacklistSource = videosPageSource.slice(
    videosPageSource.indexOf("function BlacklistTab"),
    videosPageSource.indexOf("function canDeleteBlacklistSource")
  );
  const sharedTable = ruleBody(adminCss, ".admin-table");
  const sharedTableHeader = allRuleBodies(adminCss, ".admin-table th");
  const table = ruleBody(adminCss, ".admin-blacklist-table");
  const flatModal = ruleBodyByContains(adminCss, ".admin-modal--source-delete-flat");
  const flatModalChrome = ruleBodyByContains(adminCss, ".admin-modal--source-delete-flat .admin-modal__header");
  const action = ruleBody(adminCss, ".admin-blacklist-source-delete");
  const status = ruleBody(adminCss, ".admin-blacklist-source-delete__status");
  const button = ruleBody(adminCss, ".admin-blacklist-source-delete__button");
  const searchAction = ruleBody(adminCss, ".admin-videos-filter__search-action");
  const searchActionHover = ruleBody(adminCss, ".admin-videos-filter__search-action:hover:not(:disabled)");
  const selectCell = ruleBody(adminCss, ".admin-blacklist-table td.admin-blacklist-select-cell");
  const disabledSelection = ruleBody(
    adminCss,
    ".admin-blacklist-row-select.is-disabled .admin-video-card__select-box"
  );
  const rowActions = ruleBody(adminCss, ".admin-blacklist-actions");
  const sharedDeleteButton = ruleBody(adminCss, ".admin-video-action-icon-button");
  const sharedDeleteButtonSource = videosPageSource.slice(
    videosPageSource.indexOf("function VideoDeleteIconButton"),
    videosPageSource.indexOf("function CurrentVideoCard")
  );
  const deleteAllButtonClass = videosPageSource.indexOf("admin-blacklist-source-delete__button");
  const deleteAllButtonStart = videosPageSource.lastIndexOf("<button", deleteAllButtonClass);
  const deleteAllButtonEnd = videosPageSource.indexOf("</button>", deleteAllButtonStart);
  const deleteAllButtonSource = videosPageSource.slice(deleteAllButtonStart, deleteAllButtonEnd);

  assert.match(apiSource, /startBlacklistSourceDelete/);
  assert.match(apiSource, /getBlacklistSourceDeleteStatus/);
  assert.match(apiSource, /ids\?: string\[\]/);
  assert.match(videosPageSource, /删除全部/);
  assert.match(videosPageSource, /批量删除/);
  assert.equal(Array.from(blacklistSource.matchAll(/className="admin-btn admin-videos-bulk-actions__btn"/g)).length, 3);
  assert.match(blacklistSource, /onClick=\{selectPageBlacklist\}[\s\S]*?>\s*全选本页\s*<\/button>/);
  assert.match(blacklistSource, /className="admin-btn admin-videos-bulk-actions__btn"[\s\S]*?>\s*批量删除\s*<\/button>/);
  assert.match(blacklistSource, /确定删除已选中的 \$\{selectedIds\.size\} 个拉黑视频源文件吗？/);
  assert.match(blacklistSource, /onClick=\{\(\) => setSelectedIds\(new Set\(\)\)\}[\s\S]*?disabled=\{selectedIds\.size === 0\}[\s\S]*?>\s*取消选中\s*<\/button>/);
  assert.doesNotMatch(blacklistSource, /selectMode|toggleSelectMode|批量选择|退出选择|admin-videos-bulk-actions__mobile-exit/);
  assert.doesNotMatch(blacklistSource, /className="admin-btn is-danger admin-videos-bulk-actions__btn"|<Trash2 size=\{13\} \/> 批量删除/);
  assert.match(videosPageSource, /title="删除源文件"/);
  assert.equal(
    Array.from(
      blacklistSource.matchAll(
        /confirmText="确认"[\s\S]{0,200}?modalClassName="admin-modal--delete-confirm admin-modal--source-delete-flat"/g
      )
    ).length,
    3
  );
  assert.doesNotMatch(videosPageSource, /confirmText="删除全部"|confirmText="删除"/);
  assert.doesNotMatch(videosPageSource, /<DeleteSourceNotice|function DeleteSourceNotice/);
  assert.doesNotMatch(adminCss, /admin-delete-source-option--notice/);
  assert.doesNotMatch(videosPageSource, /范围为整个黑名单，不受当前来源筛选或搜索条件影响。/);
  assert.doesNotMatch(videosPageSource, /此操作不可撤销；成功项会从黑名单和管理库中移除，失败项可再次执行重试。/);
  assert.doesNotMatch(videosPageSource, /成功后会从黑名单和管理库中移除。/);
  assert.doesNotMatch(videosPageSource, /失败时不会改变该拉黑记录，可稍后再次重试。/);
  assert.doesNotMatch(videosPageSource, /任务会在后台逐个删除，避免并发请求触发网盘限流。/);
  assert.doesNotMatch(videosPageSource, /成功项会从黑名单和管理库中移除，失败项可再次执行重试。/);
  assert.doesNotMatch(videosPageSource, /爬虫来源会保留已爬取标记，避免后续重复爬取。/);
  assert.equal(
    Array.from(videosPageSource.matchAll(/modalClassName="admin-modal--delete-confirm admin-modal--source-delete-flat"/g)).length,
    3
  );
  assert.notEqual(deleteAllButtonStart, -1);
  assert.notEqual(deleteAllButtonEnd, -1);
  assert.doesNotMatch(videosPageSource, /共 \{total\} 个拉黑视频/);
  assert.doesNotMatch(videosPageSource, /admin-videos-summary/);
  assert.match(blacklistSource, /admin-blacklist-source-delete__button[\s\S]*?\{sourceDeleteStatus\?\.running \? "删除中" : "删除全部"\}/);
  assert.doesNotMatch(blacklistSource, /admin-videos-filter__batch-select/);
  assert.match(deleteAllButtonSource, /<Trash2 size=\{15\} aria-hidden="true" \/>/);
  assert.doesNotMatch(deleteAllButtonSource, /is-danger/);
  assert.match(videosPageSource, /\{ ids: \[target\.id\] \}/);
  assert.match(videosPageSource, /\{ ids \}/);
  assert.match(blacklistSource, /className="admin-table admin-table--static-rows admin-blacklist-table admin-videos-results__content"/);
  assert.match(blacklistSource, /<td className="admin-blacklist-select-cell">/);
  assert.match(blacklistSource, /className="admin-video-card__select-input"[\s\S]*?type="checkbox"[\s\S]*?checked=\{isSelected\}[\s\S]*?disabled=\{selectionDisabled\}[\s\S]*?onChange=\{\(\) => toggleSelect\(v\)\}/);
  assert.match(blacklistSource, /admin-video-card__select-box\$\{isSelected \? " is-checked" : ""\}`\}[\s\S]*?isSelected && <Check size=\{12\}/);
  assert.doesNotMatch(blacklistSource, /is-row-select-mode|is-disabled-select|className=\{`[^`]*is-selected|aria-selected|rowSelectable/);
  assert.doesNotMatch(videosPageSource, /<th>文件名<\/th>|<th>来源<\/th>|<th>大小<\/th>|<th>拉黑时间<\/th>|<th className="is-actions">操作<\/th>/);
  assert.doesNotMatch(videosPageSource, /data-label="大小"|data-label="拉黑时间"|formatDateTime/);
  assert.doesNotMatch(videosPageSource, /admin-table-checkbox-btn/);
  assert.match(videosPageSource, /已开始后台顺序删除/);
  assert.match(videosPageSource, /sourceDeleteStatus\.processed/);
  assert.match(videosPageSource, /sourceDeleteStatus\.total/);
  assert.match(table, /width\s*:\s*100%/);
  assert.doesNotMatch(table, /max-width|width\s*:\s*min\(/);
  assert.match(sharedTable, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(sharedTable, /border\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(sharedTableHeader, /background\s*:\s*var\(--bg-elevated\)/);
  assert.equal(Array.from(adminCss.matchAll(/\.admin-blacklist-table\s*\{/g)).length, 2);
  assert.doesNotMatch(table, /background|border-color|box-shadow/);
  assert.match(
    adminCss,
    /@media \(min-width:\s*769px\)\s*\{\s*\.admin-blacklist-table\s*\{[^}]*background\s*:\s*color-mix\(in srgb,\s*var\(--bg-surface\) 72%,\s*transparent\);/s
  );
  assert.doesNotMatch(
    adminCss,
    /\.admin-blacklist-table\s*\{[^}]*border-color|\.admin-blacklist-table\s*\{[^}]*box-shadow/s
  );
  assert.match(selectCell, /width\s*:\s*52px/);
  assert.match(selectCell, /padding-right\s*:\s*0/);
  assert.match(disabledSelection, /opacity\s*:\s*0\.4/);
  assert.equal(
    Array.from(adminCss.matchAll(/\.admin-table:not\(\.admin-table--static-rows\) tbody tr:hover td/g)).length,
    2
  );
  assert.doesNotMatch(adminCss, /\.admin-table tbody tr:hover td/);
  assert.match(flatModal, /border\s*:\s*0/);
  assert.match(flatModal, /box-shadow\s*:\s*none/);
  assert.match(flatModalChrome, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(flatModalChrome, /border\s*:\s*0/);
  assert.match(action, /display\s*:\s*flex/);
  assert.match(status, /font-size\s*:\s*var\(--font-xs\)/);
  assert.match(button, /flex\s*:\s*none/);
  assert.match(deleteAllButtonSource, /admin-videos-filter__search-action/);
  assert.match(searchAction, /height\s*:\s*32px/);
  assert.match(searchAction, /border-radius\s*:\s*var\(--radius-pill\)/);
  assert.match(searchAction, /background\s*:\s*transparent/);
  assert.match(searchAction, /color\s*:\s*var\(--text-muted\)/);
  assert.match(searchAction, /font-size\s*:\s*var\(--font-xs\)/);
  assert.match(searchActionHover, /border-color\s*:\s*var\(--border-strong\)/);
  assert.match(searchActionHover, /color\s*:\s*var\(--text-strong\)/);
  assert.match(rowActions, /display\s*:\s*flex/);
  // 桌面端黑名单操作列保持单行（8be7ebd）；移动端媒体查询里仍允许换行
  assert.match(rowActions, /flex-wrap\s*:\s*nowrap/);
  assert.match(sharedDeleteButton, /width\s*:\s*30px/);
  assert.match(sharedDeleteButton, /height\s*:\s*30px/);
  assert.match(sharedDeleteButtonSource, /className="admin-btn admin-video-action-icon-button is-danger"[\s\S]*?<Trash2 size=\{15\} aria-hidden="true" \/>/);
  assert.match(
    blacklistSource,
    /<VideoDeleteIconButton[\s\S]*?onClick=\{\(\) => setSourceDeleteTarget\(v\)\}[\s\S]*?disabled=\{sourceDeleteRunning\}[\s\S]*?title="删除"[\s\S]*?ariaLabel=\{`删除 \$\{v\.fileName \|\| v\.id\}`\}[\s\S]*?\/>/
  );
  assert.doesNotMatch(videosPageSource, /admin-blacklist-delete-source-btn/);
  assert.doesNotMatch(adminCss, /admin-blacklist-delete-source-btn/);
});

test("admin video management controls wrap instead of covering text on mobile", () => {
  const css = mobileCss();
  const pagination = ruleBody(adminCss, ".admin-table-pagination");
  const paginationInfo = ruleBody(adminCss, ".admin-list-pagination__info");
  const currentFilter = allRuleBodies(css, ".admin-videos-filter--current");
  const currentFilterField = ruleBodyByContains(css, ".admin-videos-filter--current .admin-videos-filter__search");
  const currentFilterActions = ruleBodyByContains(
    css,
    ".admin-videos-filter--current .admin-videos-filter__current-actions"
  );
  const currentFilterActionButton = ruleBodyByContains(
    css,
    ".admin-videos-filter--current .admin-videos-filter__current-actions .admin-btn"
  );
  const currentFilterActionDivider = ruleBodyByContains(
    css,
    ".admin-videos-filter__current-actions"
  );
  const blacklistFilter = allRuleBodies(css, ".admin-videos-filter--blacklist");
  const blacklistFilterField = ruleBodyByContains(css, ".admin-videos-filter--blacklist .admin-videos-filter__search");
  const blacklistFilterActions = ruleBodyByContains(css, ".admin-videos-filter--blacklist .admin-videos-filter__actions");
  const blacklistFilterBatch = ruleBodyByContains(
    css,
    ".admin-videos-filter--blacklist .admin-videos-filter__actions .admin-videos-filter__batch"
  );
  const mobileFloatingActionIcons = ruleBodyByContains(
    css,
    ".admin-videos-filter--current .admin-video-advanced-toggle > svg"
  );
  const bulkToolbar = ruleBodyByContains(css, ".admin-videos-current .admin-videos-list-toolbar");
  const blacklistBulkToolbar = ruleBodyByContains(css, ".admin-videos-blacklist .admin-videos-list-toolbar");
  const bulkActions = allRuleBodies(css, ".admin-videos-bulk-actions");
  const bulkCount = allRuleBodies(css, ".admin-videos-bulk-actions__count");
  const bulkButton = allRuleBodies(css, ".admin-videos-bulk-actions__btn");
  const blacklistLastBulkButton = ruleBodyByContains(
    css,
    ".admin-videos-blacklist .admin-videos-bulk-actions__btn:last-child"
  );
  const blacklistName = ruleBody(
    css,
    '.admin-blacklist-table:not(.admin-drives-table) td[data-label="文件名"]'
  );
  const blacklistFileCell = lastRuleBody(css, ".admin-blacklist-filecell");
  const blacklistFilename = lastRuleBody(css, ".admin-blacklist-filename");
  const blacklistTrailingPill = ruleBody(
    css,
    ".admin-blacklist-filecell .admin-blacklist-reason-pill"
  );
  const blacklistSource = ruleBody(
    css,
    ".admin-blacklist-table:not(.admin-drives-table) .admin-blacklist-source-cell"
  );
  const blacklistSourceLabel = ruleBody(css, ".admin-blacklist-source-cell::before");
  const blacklistSourceName = ruleBody(css, ".admin-blacklist-source-name");
  const blacklistActions = ruleBody(
    css,
    ".admin-blacklist-table:not(.admin-drives-table) td.is-actions"
  );
  const blacklistActionsLabel = ruleBody(
    css,
    ".admin-blacklist-table:not(.admin-drives-table) td.is-actions::before"
  );
  const blacklistActionButton = ruleBody(
    css,
    ".admin-blacklist-table:not(.admin-drives-table) td.is-actions .admin-btn:not(.admin-video-action-icon-button)"
  );
  const blacklistCard = ruleBody(css, ".admin-blacklist-table:not(.admin-drives-table) tr");
  const blacklistSelectCell = ruleBody(
    css,
    ".admin-blacklist-table:not(.admin-drives-table) td.admin-blacklist-select-cell"
  );
  const blacklistSelectCellLabel = ruleBody(
    css,
    ".admin-blacklist-table:not(.admin-drives-table) td.admin-blacklist-select-cell::before"
  );

  assert.match(pagination, /flex-wrap\s*:\s*wrap/);
  assert.match(paginationInfo, /white-space\s*:\s*nowrap/);
  assert.doesNotMatch(css, /\.admin-list-pagination__info\s*\{[^}]*order\s*:/s);
  assert.match(currentFilter, /display\s*:\s*grid/);
  assert.match(currentFilter, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)/);
  assert.match(currentFilterField, /min-width\s*:\s*0/);
  assert.match(currentFilterActions, /position\s*:\s*fixed/);
  assert.match(currentFilterActions, /right\s*:\s*var\(--space-3\)/);
  assert.match(currentFilterActions, /bottom\s*:\s*calc\(var\(--space-3\)\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(currentFilterActions, /width\s*:\s*max-content/);
  assert.match(currentFilterActions, /border\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(currentFilterActions, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(currentFilterActions, /overflow\s*:\s*hidden/);
  assert.match(currentFilterActionButton, /position\s*:\s*relative/);
  assert.match(currentFilterActionButton, /min-height\s*:\s*44px/);
  assert.match(currentFilterActionButton, /width\s*:\s*auto/);
  assert.match(currentFilterActionButton, /min-width\s*:\s*0/);
  assert.match(currentFilterActionButton, /padding\s*:\s*0 14px/);
  assert.match(currentFilterActionButton, /border\s*:\s*0/);
  assert.match(currentFilterActionButton, /border-radius\s*:\s*0/);
  assert.match(currentFilterActionButton, /background\s*:\s*transparent/);
  assert.match(currentFilterActionButton, /box-shadow\s*:\s*none/);
  assert.match(currentFilterActionDivider, /content\s*:\s*""/);
  assert.match(currentFilterActionDivider, /height\s*:\s*18px/);
  assert.match(blacklistFilter, /display\s*:\s*grid/);
  assert.match(blacklistFilter, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)/);
  assert.match(blacklistFilterField, /min-width\s*:\s*0/);
  assert.match(blacklistFilterActions, /position\s*:\s*fixed/);
  assert.match(blacklistFilterActions, /right\s*:\s*var\(--space-3\)/);
  assert.match(blacklistFilterActions, /bottom\s*:\s*calc\(var\(--space-3\)\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(blacklistFilterActions, /grid-column\s*:\s*auto/);
  assert.match(blacklistFilterActions, /width\s*:\s*max-content/);
  assert.match(blacklistFilterActions, /max-width\s*:\s*calc\(100vw\s*-\s*\(var\(--space-3\)\s*\*\s*2\)\)/);
  assert.match(blacklistFilterActions, /border\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(blacklistFilterActions, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(blacklistFilterActions, /overflow\s*:\s*hidden/);
  assert.match(blacklistFilterBatch, /position\s*:\s*relative/);
  assert.match(blacklistFilterBatch, /min-height\s*:\s*44px/);
  assert.match(blacklistFilterBatch, /border\s*:\s*0/);
  assert.match(blacklistFilterBatch, /border-radius\s*:\s*0/);
  assert.match(blacklistFilterBatch, /background\s*:\s*transparent/);
  assert.match(blacklistFilterBatch, /box-shadow\s*:\s*none/);
  assert.match(blacklistFilterBatch, /white-space\s*:\s*nowrap/);
  assert.match(mobileFloatingActionIcons, /display\s*:\s*none/);
  assert.match(
    css,
    /\.admin-videos-filter--current \.admin-video-advanced-toggle > svg,\s*\.admin-videos-filter--blacklist \.admin-blacklist-source-delete__button > svg\s*\{[^}]*display\s*:\s*none/s
  );
  assert.match(css, /\.admin-videos-current\.has-bulk-actions \.admin-videos-filter__current-actions,[\s\S]*?\.admin-videos-blacklist\.has-bulk-actions \.admin-videos-filter__actions\s*\{[^}]*display\s*:\s*none/s);
  assert.match(bulkToolbar, /position\s*:\s*fixed/);
  assert.match(bulkToolbar, /bottom\s*:\s*calc\(var\(--space-3\)\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(bulkToolbar, /margin\s*:\s*0/);
  assert.match(blacklistBulkToolbar, /position\s*:\s*fixed/);
  assert.match(blacklistBulkToolbar, /bottom\s*:\s*calc\(var\(--space-3\)\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(blacklistBulkToolbar, /margin\s*:\s*0/);
  assert.doesNotMatch(css, /\.admin-videos-(?:current|blacklist)\.has-bulk-actions\s*\{[^}]*padding-bottom/s);
  assert.match(bulkActions, /display\s*:\s*grid/);
  assert.match(bulkActions, /grid-template-columns\s*:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\)/);
  assert.match(bulkCount, /grid-column\s*:\s*1\s*\/\s*-1/);
  assert.match(bulkButton, /min-height\s*:\s*40px/);
  assert.match(bulkButton, /min-width\s*:\s*0/);
  assert.match(blacklistLastBulkButton, /grid-column\s*:\s*1\s*\/\s*-1/);
  assert.doesNotMatch(adminCss, /admin-blacklist-bulk-toolbar|admin-videos-bulk-actions__mobile-exit/);
  assert.match(videosPageSource, /admin-videos-blacklist/);
  assert.match(blacklistName, /grid-column\s*:\s*1\s*\/\s*-1/);
  assert.match(blacklistName, /grid-row\s*:\s*1/);
  assert.match(blacklistFileCell, /display\s*:\s*block/);
  assert.doesNotMatch(blacklistFileCell, /flex-wrap/);
  assert.match(blacklistFilename, /display\s*:\s*inline/);
  assert.match(blacklistFilename, /white-space\s*:\s*normal/);
  assert.match(blacklistTrailingPill, /margin-inline-start\s*:\s*6px/);
  assert.match(blacklistTrailingPill, /vertical-align\s*:\s*middle/);
  assert.match(blacklistSource, /grid-column\s*:\s*1/);
  assert.match(blacklistSource, /grid-row\s*:\s*2/);
  assert.match(blacklistSource, /display\s*:\s*flex/);
  assert.match(blacklistSource, /min-width\s*:\s*0/);
  assert.match(blacklistSourceLabel, /flex\s*:\s*none/);
  assert.match(blacklistSourceName, /overflow\s*:\s*hidden/);
  assert.match(blacklistSourceName, /text-overflow\s*:\s*ellipsis/);
  assert.match(blacklistSourceName, /white-space\s*:\s*nowrap/);
  assert.match(blacklistActions, /grid-column\s*:\s*2/);
  assert.match(blacklistActions, /grid-row\s*:\s*2/);
  assert.match(blacklistCard, /position\s*:\s*relative/);
  assert.match(blacklistCard, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)\s+auto\s+auto/);
  assert.doesNotMatch(blacklistCard, /padding-left/);
  assert.match(blacklistSelectCell, /position\s*:\s*static/);
  assert.match(blacklistSelectCell, /grid-column\s*:\s*3/);
  assert.match(blacklistSelectCell, /grid-row\s*:\s*2/);
  assert.match(blacklistSelectCell, /transform\s*:\s*none/);
  assert.doesNotMatch(blacklistSelectCell, /left\s*:|top\s*:/);
  assert.match(blacklistSelectCellLabel, /content\s*:\s*none/);
  assert.match(
    css,
    /\.admin-table:not\(\.admin-drives-table\):not\(\.admin-table--static-rows\) tr:hover/
  );
  assert.doesNotMatch(adminCss, /\.admin-blacklist-table[^,{]*tr\.is-selected|\.admin-blacklist-table[^,{]*\.is-row-select-mode/);
  assert.match(blacklistActions, /justify-content\s*:\s*flex-end/);
  assert.match(blacklistActionsLabel, /content\s*:\s*none/);
  assert.match(blacklistActionButton, /flex\s*:\s*0\s+0\s+auto/);
  assert.match(blacklistActionButton, /white-space\s*:\s*nowrap/);
  assert.match(
    videosPageSource,
    /const sourceName = driveNameMap\.get\(v\.driveId\) \?\? v\.driveId;[\s\S]*?className="admin-mono-cell admin-blacklist-source-cell"[\s\S]*?className="admin-blacklist-source-name"/
  );
});

test("admin loading spinner rotates around icon center", () => {
  const spinner = ruleBody(adminCss, ".admin-spin");
  const pageLoading = ruleBody(adminCss, ".admin-loading");

  assert.match(spinner, /animation\s*:\s*admin-update-spin\s+0\.9s\s+linear\s+infinite/);
  assert.match(spinner, /transform-box\s*:\s*fill-box/);
  assert.match(spinner, /transform-origin\s*:\s*center/);
  assert.match(spinner, /will-change\s*:\s*transform/);
  assert.match(pageLoading, /flex\s*:\s*1 1 auto/);
  assert.match(pageLoading, /min-height\s*:\s*0/);
  assert.doesNotMatch(usersPageSource, /AdminLoading/);
  assert.doesNotMatch(tagsPageSource, /AdminLoading/);
  assert.doesNotMatch(crawlersPageSource, /AdminLoading/);
  assert.match(crawlersPageSource, /<CrawlerListSkeleton \/>/);
  assert.doesNotMatch(videosPageSource, /AdminLoading/);
  assert.equal(
    Array.from(drivesPageSource.matchAll(/<AdminLoading \/>/g)).length,
    0
  );
  assert.match(adminCss, /@media \(prefers-reduced-motion: reduce\)\s*\{\s*\.admin-spin\s*\{\s*animation-duration\s*:\s*0\.9s\s*!important/s);
});

test("drive list loading uses a local card-grid skeleton", () => {
  const skeleton = ruleBody(adminCss, ".admin-drive-card-skeleton");
  const skeletonSurface = ruleBody(adminCss, ".admin-card-skeleton-surface");

  assert.match(drivesPageLoadingSource, /const DRIVE_LIST_SKELETON_COUNT = 6/);
  assert.match(
    drivesPageLoadingSource,
    /className="admin-drives-grid admin-drives-grid--skeleton"[\s\S]*?aria-busy="true"[\s\S]*?admin-drive-card-skeleton admin-card-skeleton-surface/
  );
  assert.match(
    drivesPageSource,
    /\{loading \? \(\s*<DriveListSkeleton \/>\s*\) : loadError/
  );
  assert.match(skeleton, /height\s*:\s*230px/);
  assert.match(skeleton, /border-radius\s*:\s*var\(--radius-md\)/);
  assert.match(skeletonSurface, /background\s*:\s*linear-gradient/);
  assert.match(
    skeletonSurface,
    /animation\s*:\s*admin-card-skeleton-shimmer 1\.5s ease-in-out infinite/
  );
  assert.match(
    adminCss,
    /@media \(prefers-reduced-motion: reduce\)\s*\{\s*\.admin-card-skeleton-surface\s*\{\s*animation\s*:\s*none/s
  );
});

test("crawler loading keeps the real page structure and skeletonizes only crawler cards", () => {
  const skeleton = ruleBody(adminCss, ".admin-crawler-card-skeleton");
  const sharedSurface = ruleBody(adminCss, ".admin-card-skeleton-surface");

  assert.match(crawlersPageLoadingSource, /const CRAWLER_LIST_SKELETON_COUNT = 3/);
  assert.match(
    crawlersPageLoadingSource,
    /className="admin-page admin-page--with-floating-actions admin-crawlers-page"[\s\S]*?className="admin-card admin-crawler-list" aria-busy="true">\s*<CrawlerListControlsPlaceholder \/>\s*<CrawlerListSkeleton \/>/
  );
  assert.match(
    crawlersPageLoadingSource,
    /className="admin-crawler-list__controls admin-crawler-list__controls--placeholder"[\s\S]*?className="admin-crawler-global-teaser"[\s\S]*?预览视频[\s\S]*?className="toggle-switch"/
  );
  assert.match(
    crawlersPageSource,
    /\{loading && !hasCrawlers && <CrawlerListControlsPlaceholder \/>\}/
  );
  assert.match(
    ruleBody(adminCss, ".admin-crawler-list__controls--placeholder"),
    /visibility\s*:\s*hidden/
  );
  assert.match(
    crawlersPageLoadingSource,
    /className="admin-crawler-table admin-crawler-table--skeleton"[\s\S]*?role="status"[\s\S]*?aria-busy="true"[\s\S]*?admin-crawler-card-skeleton admin-card-skeleton-surface/
  );
  assert.match(
    crawlersPageSource,
    /className="admin-card admin-crawler-list"[\s\S]*?aria-busy=\{loading \|\| undefined\}[\s\S]*?\{loading \? \(\s*<CrawlerListSkeleton \/>/
  );
  assert.match(crawlersPageSource, /disabled=\{loading \|\| togglingTeasers\}/);
  assert.doesNotMatch(crawlersPageSource, /AdminLoading/);
  assert.match(skeleton, /height\s*:\s*64px/);
  assert.match(skeleton, /border\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(skeleton, /border-radius\s*:\s*var\(--radius-sm\)/);
  assert.match(sharedSurface, /background\s*:\s*linear-gradient/);
});

test("drive detail loading renders the real page shell without animated placeholders", () => {
  const detailLoadingStart = drivesPageLoadingSource.indexOf(
    "export function DriveDetailLoading"
  );
  const routeLoadingStart = drivesPageLoadingSource.indexOf(
    "export function DrivesPageLoading"
  );
  const detailLoadingSource = drivesPageLoadingSource.slice(
    detailLoadingStart,
    routeLoadingStart
  );

  assert.ok(detailLoadingStart > -1);
  assert.match(
    detailLoadingSource,
    /className="admin-drive-detail-layout admin-drive-detail-loading"[\s\S]*?aria-busy="true"/
  );
  assert.match(detailLoadingSource, /正在加载网盘详情/);
  assert.match(drivesPageLoadingSource, /searchParams\.get\("drive"\)/);
  for (const label of ["基本信息", "扫描跳过目录", "生成状态", "本地存储占用"]) {
    assert.match(detailLoadingSource, new RegExp(label));
  }
  assert.match(detailLoadingSource, /className="admin-detail-card"/);
  assert.match(detailLoadingSource, /className="admin-gen-columns"/);
  assert.match(detailLoadingSource, /className="admin-local-storage-metrics"/);
  assert.match(
    detailLoadingSource,
    /className="toggle-switch is-on"[\s\S]*disabled[\s\S]*aria-checked="true"/
  );
  assert.doesNotMatch(detailLoadingSource, /className="admin-status"/);
  assert.doesNotMatch(detailLoadingSource, /admin-card-skeleton-surface/);
  assert.doesNotMatch(detailLoadingSource, /admin-drive-detail-skeleton__card/);
  assert.match(
    drivesPageSource,
    /import \{ DriveDetailLoading, DriveListSkeleton \} from "\.\/DrivesPageLoading";/
  );
});

test("mobile video management uses compact theme-aware video cards", () => {
  const css = mobileCss();
  const grid = allRuleBodies(css, ".admin-video-card-grid");
  const card = allRuleBodies(css, ".admin-video-card");
  const titleCell = allRuleBodies(css, ".admin-video-card .admin-video-title-cell");
  const thumb = ruleBody(adminCss, ".admin-video-card .admin-video-thumb-wrap");
  const titleText = ruleBody(adminCss, ".admin-video-card .admin-video-title");
  const pills = ruleBody(adminCss, ".admin-video-card .admin-video-filemeta-pills");
  const meta = ruleBody(adminCss, ".admin-video-card__meta");
  const source = ruleBody(adminCss, ".admin-video-card__source");
  const actions = ruleBody(adminCss, ".admin-video-card__actions");
  const utilityActions = ruleBody(adminCss, ".admin-video-card__utility-actions");
  const actionButton = ruleBody(adminCss, ".admin-video-action-icon-button");
  const dangerButton = ruleBody(adminCss, ".admin-video-action-icon-button.is-danger");

  assert.match(grid, /gap\s*:\s*10px/);
  assert.match(card, /--admin-video-card-bg\s*:\s*color-mix\(in srgb,\s*var\(--bg-surface\) 82%,\s*transparent\)/);
  assert.match(card, /background\s*:\s*var\(--admin-video-card-bg\)/);
  assert.match(card, /border-radius\s*:\s*14px/);
  assert.match(card, /padding\s*:\s*12px\s+14px/);
  assert.match(card, /gap\s*:\s*10px/);
  assert.match(css, /:root:not\(\[data-theme="pink"\]\) \.admin-video-card\s*\{[^}]*--admin-video-card-bg\s*:\s*#1e1e1e/s);
  assert.match(css, /:root\[data-theme="pink"\] \.admin-video-card\s*\{/);
  assert.doesNotMatch(videosPageSource, /className="is-checkbox"/);
  assert.doesNotMatch(videosPageSource, /admin-table-checkbox-btn/);
  assert.match(titleCell, /grid-template-columns\s*:\s*clamp\(104px,\s*32vw,\s*156px\)\s+minmax\(0,\s*1fr\)/);
  assert.match(thumb, /aspect-ratio\s*:\s*16\s*\/\s*9/);
  assert.match(thumb, /border-radius\s*:\s*8px/);
  assert.match(titleText, /-webkit-line-clamp\s*:\s*2/);
  assert.match(titleText, /overflow-wrap\s*:\s*anywhere/);
  assert.match(videosPageSource, /loading="lazy"\s+decoding="async"/);
  assert.match(videosPageSource, /className="admin-video-title" title=\{v\.title\}/);
  assert.match(pills, /display\s*:\s*flex/);
  assert.doesNotMatch(videosPageSource, /admin-video-filemeta-pill is-category/);
  assert.doesNotMatch(css, /admin-video-card-category/);
  assert.match(videosPageSource, /<dt>来源<\/dt>[\s\S]*?className="admin-video-card__source"/);
  assert.match(videosPageSource, /<dt>时长<\/dt>[\s\S]*?formatDur\(video\.durationSeconds\)/);
  assert.match(meta, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)\s+auto/);
  assert.match(source, /text-overflow\s*:\s*ellipsis/);
  assert.doesNotMatch(videosPageSource, /data-label="预览视频"[\s\S]*?<PreviewStatus/);
  assert.match(actions, /display\s*:\s*flex/);
  assert.match(actions, /justify-content\s*:\s*space-between/);
  assert.match(actions, /gap\s*:\s*10px/);
  assert.match(utilityActions, /gap\s*:\s*4px/);
  assert.match(actionButton, /width\s*:\s*30px/);
  assert.match(actionButton, /height\s*:\s*30px/);
  assert.match(actionButton, /justify-content\s*:\s*center/);
  assert.match(actionButton, /border-radius\s*:\s*8px/);
  assert.match(actionButton, /background\s*:\s*transparent/);
  assert.match(dangerButton, /border-color\s*:\s*color-mix\(in srgb,\s*var\(--admin-video-card-danger,\s*var\(--danger\)\) 26%,\s*transparent\)/);
  assert.match(dangerButton, /color\s*:\s*color-mix\(in srgb,\s*var\(--admin-video-card-danger,\s*var\(--danger\)\) 68%,\s*transparent\)/);
  assert.doesNotMatch(adminCss, /\.admin-video-card\.is-selected/);
});

test("video edit modal stays focused on common metadata", () => {
  const editModalSource = videosPageSource.slice(
    videosPageSource.indexOf("function EditVideoModal"),
    videosPageSource.indexOf("function tagAssignmentSourceLabel")
  );
  const editModal = ruleBody(adminCss, ".admin-modal--video-edit");
  const editModalHeader = ruleBody(adminCss, ".admin-modal--video-edit .admin-modal__header");
  const editModalFooter = allRuleBodies(adminCss, ".admin-modal--video-edit .admin-modal__footer");
  const editTagPicker = ruleBody(adminCss, ".admin-modal--video-edit .admin-video-tag-picker");
  const selectedTagOption = ruleBody(adminCss, ".admin-modal--video-edit .admin-video-tag-option:has(input:checked)");
  const editBasics = ruleBody(adminCss, ".admin-video-edit-basics");
  const editMeta = ruleBody(adminCss, ".admin-video-edit-meta");
  const footerActions = ruleBody(adminCss, ".admin-video-edit-footer-actions");
  const previewActions = allRuleBodies(adminCss, ".admin-video-preview-actions");
  const previewButton = ruleBody(adminCss, ".admin-video-preview-button");
  const viewVideoLink = ruleBody(adminCss, ".admin-video-edit-view-link");
  const previewStatusDot = ruleBody(adminCss, ".admin-modal--video-edit .admin-video-preview-actions .admin-status::before");

  assert.match(videosPageSource, /ariaLabel="编辑视频"/);
  assert.match(editModalSource, /title="编辑视频"/);
  assert.match(editModalSource, /className="admin-modal--video-edit"/);
  assert.match(editModalSource, /className="admin-btn admin-video-edit-view-link"[\s\S]*?to=\{`\/video\/\$\{encodeURIComponent\(video\.id\)\}`\}[\s\S]*?target="_blank"[\s\S]*?rel="noreferrer"[\s\S]*?>\s*查看视频播放页\s*<\/Link>/);
  assert.doesNotMatch(videosPageSource, /title=\{`编辑视频 ·/);
  assert.doesNotMatch(videosPageSource, /const \[badges, setBadges\]/);
  assert.doesNotMatch(videosPageSource, /const \[thumbnail, setThumbnail\]/);
  assert.doesNotMatch(videosPageSource, /const \[quality, setQuality\]/);
  assert.doesNotMatch(videosPageSource, /video-badges/);
  assert.doesNotMatch(videosPageSource, /video-quality/);
  assert.doesNotMatch(videosPageSource, /video-thumbnail/);
  assert.doesNotMatch(videosPageSource, /徽标（/);
  assert.doesNotMatch(videosPageSource, /封面 URL/);
  assert.doesNotMatch(videosPageSource, /封面预览/);
  assert.doesNotMatch(videosPageSource, /badges:\s*splitList\(badges\)/);
  assert.doesNotMatch(videosPageSource, /thumbnail:\s*thumbnail\.trim\(\)/);
  assert.doesNotMatch(videosPageSource, /quality:\s*quality\.trim\(\)/);
  assert.match(editModalSource, /id=\{`\$\{idPrefix\}-video-title`\}[\s\S]*?value=\{video\.title\}[\s\S]*?readOnly/);
  assert.match(editModalSource, /id=\{`\$\{idPrefix\}-video-author`\}[\s\S]*?value=\{video\.author \?\? ""\}[\s\S]*?readOnly/);
  assert.doesNotMatch(editModalSource, /title:\s*title\.trim\(\)/);
  assert.doesNotMatch(editModalSource, /author:\s*author\.trim\(\)/);
  assert.doesNotMatch(editModalSource, /video-description|video-duration|技术信息（排查用）|内部视频 ID|网盘文件 ID/);
  assert.doesNotMatch(editModalSource, /const \[description, setDescription\]|const \[durationSec, setDurationSec\]/);
  assert.doesNotMatch(editModalSource, /description,|durationSeconds:/);
  assert.match(editModal, /width\s*:\s*min\(680px,\s*100%\)/);
  assert.match(editModal, /border\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(editModal, /box-shadow\s*:\s*var\(--shadow-xl\)/);
  assert.match(editModalHeader, /border-bottom\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(editModalFooter, /border-top\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(editTagPicker, /border\s*:\s*0/);
  assert.match(editTagPicker, /background\s*:\s*transparent/);
  assert.match(editModalSource, /<section className="admin-video-edit-section">\s*<h3>基本信息<\/h3>[\s\S]*?<h3>标签<\/h3>[\s\S]*?<h3>视频信息<\/h3>/);
  assert.match(editBasics, /grid-template-columns\s*:\s*minmax\(0,\s*2fr\) minmax\(160px,\s*1fr\)/);
  assert.match(selectedTagOption, /background\s*:\s*var\(--accent-soft\)/);
  assert.match(editMeta, /grid-template-columns\s*:\s*repeat\(3,\s*minmax\(0,\s*1fr\)\)/);
  assert.match(editMeta, /background\s*:\s*color-mix/);
  assert.match(footerActions, /display\s*:\s*flex/);
  assert.doesNotMatch(editModalSource, /admin-video-tag-option__count|\{tag\.count\}/);
  assert.doesNotMatch(adminCss, /admin-video-tag-option__count/);
  assert.doesNotMatch(editModalSource, /admin-video-tag-option__source|video\.tagSources\?\.\[tag\.label\]/);
  assert.doesNotMatch(adminCss, /admin-video-tag-option__source/);
  assert.match(previewActions, /display\s*:\s*flex/);
  assert.match(previewActions, /align-items\s*:\s*center/);
  assert.match(previewActions, /gap\s*:\s*var\(--space-5\)/);
  assert.match(previewButton, /padding\s*:\s*5px\s+9px/);
  assert.match(previewButton, /font-size\s*:\s*var\(--font-xs\)/);
  assert.match(previewStatusDot, /content\s*:\s*none/);
  assert.match(previewStatusDot, /display\s*:\s*none/);
  assert.match(viewVideoLink, /margin-right\s*:\s*auto/);
  assert.match(viewVideoLink, /white-space\s*:\s*nowrap/);
  assert.match(editModalSource, /<dt>预览视频<\/dt>\s*<dd className="admin-video-preview-actions">\s*<PreviewStatus[\s\S]*?className="admin-btn admin-video-preview-button"[\s\S]*?<\/button>/);
  assert.doesNotMatch(editModalSource, /<RefreshCw size=\{13\} className=\{previewBusy/);
});

test("admin modals and action footers adapt on mobile", () => {
  const css = mobileCss();

  // .admin-modal 桌面段已用 `width: min(620px, 100%)`，窄屏自然 100%；mobile 段
  // 只重写 max-height，所以这里断桌面规则即可。
  assert.match(ruleBody(adminCss, ".admin-modal"), /width\s*:\s*min\(\d+px,\s*100%\)/);
  assert.match(ruleBody(adminCss, ".admin-modal.admin-modal--crawler"), /width\s*:\s*min\(1080px,\s*100%\)/);
  assert.match(allRuleBodies(css, ".admin-modal"), /display\s*:\s*flex/);
  assert.match(allRuleBodies(css, ".admin-modal"), /overflow\s*:\s*hidden/);
  assert.match(allRuleBodies(css, ".admin-modal__body"), /overflow-y\s*:\s*auto/);
  assert.match(allRuleBodies(css, ".admin-modal-backdrop"), /safe-area-inset-top/);
  assert.match(allRuleBodies(css, ".admin-modal-backdrop"), /place-items\s*:\s*center/);
  assert.doesNotMatch(allRuleBodies(css, ".admin-modal-backdrop"), /align-items\s*:\s*stretch/);
  // 多按钮 footer 在 mobile 下要换行避免溢出。
  assert.match(allRuleBodies(css, ".admin-modal__footer"), /flex-wrap\s*:\s*wrap/);
  // 删除/放弃类确认弹窗在 mobile 下不能跟随通用 modal stretch 到顶部。
  const confirmModal = ruleBody(css, ".admin-modal--delete-confirm");
  assert.match(confirmModal, /align-self\s*:\s*center/);
  assert.match(confirmModal, /justify-self\s*:\s*center/);
  assert.match(ruleBody(adminCss, ".admin-modal__header.is-titleless"), /justify-content\s*:\s*flex-end/);
  // 表单 input/select/textarea 在 mobile 下铺满。规则用逗号合并写法（多 selector
  // 共享 body），所以走 ruleBodyByContains 而不是简单正则。
  assert.match(ruleBodyByContains(css, ".admin-form__row input"), /width\s*:\s*100%/);
});

test("mobile drive type picker uses compact three-column cards", () => {
  const driveTypeGridBodies = allRuleBodies(adminCss, ".admin-drive-type-grid");
  const driveTypeCardBodies = allRuleBodies(adminCss, ".admin-drive-type-card");
  const driveTypeIconBodies = allRuleBodies(adminCss, ".admin-drive-type-card__icon");

  assert.match(driveTypeGridBodies, /grid-template-columns\s*:\s*repeat\(3,\s*minmax\(0,\s*1fr\)\)/);
  assert.doesNotMatch(driveTypeGridBodies, /grid-template-columns\s*:\s*repeat\(2,\s*1fr\)/);
  assert.match(driveTypeCardBodies, /min-height\s*:\s*94px/);
  assert.match(driveTypeIconBodies, /width\s*:\s*38px/);
  assert.match(driveTypeIconBodies, /height\s*:\s*38px/);
});

test("mobile tags management does not create horizontal page overflow", () => {
  const css = mobileCss();
  const layout = allRuleBodies(css, ".admin-tags-layout");
  const desktopLayout = allRuleBodies(adminCss, ".admin-tags-layout");
  const main = allRuleBodies(adminCss, ".admin-tags-main");
  const board = allRuleBodies(adminCss, ".admin-tags-board");
  const mobileBoard = allRuleBodies(css, ".admin-tags-board");
  const toolbar = allRuleBodies(css, ".admin-tags-toolbar");
  const desktopToolbar = ruleBody(adminCss, ".admin-tags-toolbar");
  const search = allRuleBodies(css, ".admin-tags-search");
  const desktopSearch = ruleBody(adminCss, ".admin-tags-search");
  const sharedSearch = ruleBody(searchCss, ".search-panel--uiverse");
  const toolbarActions = allRuleBodies(css, ".admin-tags-toolbar-actions");
  const desktopToolbarActions = ruleBody(adminCss, ".admin-tags-toolbar-actions");
  const toolbarActionButton = ruleBody(css, ".admin-tags-toolbar-actions .admin-btn");
  const toolbarActionDivider = ruleBody(css, ".admin-tags-toolbar-actions .admin-btn + .admin-btn::before");
  const toolbarCreateIcon = ruleBody(css, ".admin-tags-toolbar-actions__create > svg");
  const filters = allRuleBodies(css, ".admin-tags-filter-tabs");
  const desktopFilters = allRuleBodies(adminCss, ".admin-tags-filter-tabs");
  const filterPanel = allRuleBodies(css, ".admin-tags-filter-panel");
  const desktopFilterPanel = ruleBody(adminCss, ".admin-tags-filter-panel");
  const filterTab = allRuleBodies(adminCss, ".admin-tags-filter-tab");
  const filterTabText = allRuleBodies(adminCss, ".admin-tags-filter-tab__text");
  const mobileFilterTabText = allRuleBodies(css, ".admin-tags-filter-tab__text");
  const grid = allRuleBodies(css, ".admin-tags-grid");
  const card = allRuleBodies(css, ".admin-tag-card");
  const cardFooter = allRuleBodies(adminCss, ".admin-tag-card__footer");
  const cardCount = allRuleBodies(adminCss, ".admin-tag-card__count");
  const cardActions = allRuleBodies(adminCss, ".admin-tag-card__footer-actions");
  const cardEdit = allRuleBodies(adminCss, ".admin-tag-card__edit");
  const cardDelete = allRuleBodies(adminCss, ".admin-tag-card__delete");
  const pagination = ruleBody(adminCss, ".admin-table-pagination");
  const paginationInfo = ruleBody(adminCss, ".admin-list-pagination__info");

  assert.match(desktopLayout, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)/);
  assert.match(main, /--tags-cards-width\s*:\s*calc\(\(240px \* 4\) \+ \(var\(--space-3\) \* 3\)\)/);
  assert.doesNotMatch(main, /--tags-search-width/);
  assert.doesNotMatch(board, /--tags-filter-width|--tags-board-width/);
  assert.match(board, /grid-template-columns\s*:\s*minmax\(0,\s*var\(--tags-cards-width\)\)/);
  assert.match(board, /justify-content\s*:\s*center/);
  assert.match(board, /align-items\s*:\s*stretch/);
  assert.match(layout, /width\s*:\s*100%/);
  assert.match(layout, /max-width\s*:\s*100%/);
  assert.match(layout, /overflow-x\s*:\s*clip/);
  assert.match(mobileBoard, /grid-template-columns\s*:\s*1fr/);
  assert.match(desktopToolbar, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)\s+minmax\(240px,\s*360px\)\s+minmax\(0,\s*1fr\)/);
  assert.match(desktopToolbar, /width\s*:\s*min\(100%,\s*var\(--tags-cards-width\)\)/);
  assert.match(desktopToolbar, /margin\s*:\s*0 auto var\(--space-4\)/);
  assert.match(toolbar, /max-width\s*:\s*100%/);
  assert.match(toolbar, /grid-template-columns\s*:\s*1fr/);
  assert.match(toolbar, /justify-items\s*:\s*stretch/);
  assert.match(desktopSearch, /grid-column\s*:\s*2/);
  assert.match(desktopSearch, /grid-row\s*:\s*2/);
  assert.match(desktopSearch, /justify-self\s*:\s*center/);
  assert.match(desktopToolbarActions, /grid-column\s*:\s*3/);
  assert.match(desktopToolbarActions, /grid-row\s*:\s*2/);
  assert.match(desktopToolbarActions, /justify-self\s*:\s*end/);
  assert.match(tagsPageSource, /className="admin-tags-search search-panel--transparent"/);
  assert.match(sharedSearch, /--width-of-input\s*:\s*min\(100%,\s*360px\)/);
  assert.doesNotMatch(adminCss, /\.admin-tags-search input\s*\{|\.admin-tags-search__icon/);
  assert.match(search, /grid-column\s*:\s*1/);
  assert.match(search, /grid-row\s*:\s*2/);
  assert.match(search, /min-width\s*:\s*0/);
  assert.match(toolbarActions, /position\s*:\s*fixed/);
  assert.match(toolbarActions, /right\s*:\s*var\(--space-3\)/);
  assert.match(toolbarActions, /bottom\s*:\s*calc\(var\(--space-3\)\s*\+\s*env\(safe-area-inset-bottom\)\)/);
  assert.match(toolbarActions, /grid-row\s*:\s*3/);
  assert.match(toolbarActions, /justify-self\s*:\s*end/);
  assert.match(toolbarActions, /width\s*:\s*max-content/);
  assert.match(toolbarActions, /max-width\s*:\s*calc\(100vw\s*-\s*\(var\(--space-3\)\s*\*\s*2\)\)/);
  assert.match(toolbarActions, /padding\s*:\s*0/);
  assert.match(toolbarActions, /border\s*:\s*1px solid var\(--border-subtle\)/);
  assert.match(toolbarActions, /border-radius\s*:\s*12px/);
  assert.match(toolbarActions, /background\s*:\s*var\(--bg-surface\)/);
  assert.match(toolbarActions, /gap\s*:\s*0/);
  assert.match(toolbarActions, /overflow\s*:\s*hidden/);
  assert.match(toolbarActionButton, /position\s*:\s*relative/);
  assert.match(toolbarActionButton, /right\s*:\s*auto/);
  assert.match(toolbarActionButton, /bottom\s*:\s*auto/);
  assert.match(toolbarActionButton, /min-height\s*:\s*44px/);
  assert.match(toolbarActionButton, /border\s*:\s*0/);
  assert.match(toolbarActionButton, /border-radius\s*:\s*0/);
  assert.match(toolbarActionButton, /background\s*:\s*transparent/);
  assert.match(toolbarActionButton, /box-shadow\s*:\s*none/);
  assert.match(toolbarActionDivider, /content\s*:\s*""/);
  assert.match(toolbarActionDivider, /height\s*:\s*18px/);
  assert.match(toolbarActionDivider, /background\s*:\s*var\(--border-subtle\)/);
  assert.match(toolbarCreateIcon, /display\s*:\s*none/);
  assert.doesNotMatch(css, /\.admin-tags-page\s*\{[^}]*padding-bottom/s);
  assert.doesNotMatch(adminCss, /\.admin-tags-page\.has-bulk-actions \.admin-tags-toolbar-actions/);
  assert.match(desktopFilterPanel, /grid-column\s*:\s*2/);
  assert.match(desktopFilterPanel, /grid-row\s*:\s*1/);
  assert.match(desktopFilterPanel, /justify-self\s*:\s*center/);
  assert.match(desktopFilterPanel, /display\s*:\s*flex/);
  assert.match(desktopFilterPanel, /justify-content\s*:\s*center/);
  assert.match(desktopFilterPanel, /width\s*:\s*auto/);
  assert.match(desktopFilterPanel, /max-width\s*:\s*100%/);
  assert.match(desktopFilterPanel, /min-width\s*:\s*0/);
  assert.match(desktopFilterPanel, /margin\s*:\s*0/);
  assert.doesNotMatch(desktopFilterPanel, /position\s*:\s*(fixed|sticky)/);
  assert.doesNotMatch(desktopFilterPanel, /\bleft\s*:/);
  assert.doesNotMatch(desktopFilterPanel, /\btop\s*:/);
  assert.doesNotMatch(desktopFilterPanel, /transform\s*:/);
  assert.match(filterPanel, /grid-row\s*:\s*1/);
  assert.match(filterPanel, /width\s*:\s*max-content/);
  assert.match(filterPanel, /max-width\s*:\s*100%/);
  assert.match(desktopFilters, /display\s*:\s*flex/);
  assert.match(desktopFilters, /flex-direction\s*:\s*row/);
  assert.match(desktopFilters, /gap\s*:\s*4px/);
  assert.match(desktopFilters, /padding\s*:\s*3px/);
  assert.match(desktopFilters, /width\s*:\s*auto/);
  assert.match(desktopFilters, /min-height\s*:\s*0/);
  assert.match(desktopFilters, /max-width\s*:\s*100%/);
  assert.match(desktopFilters, /overflow-x\s*:\s*auto/);
  assert.doesNotMatch(desktopFilters, /height\s*:\s*var\(--tags-filter-height\)/);
  assert.doesNotMatch(adminCss, /admin-tags-filter-tab__count/);
  assert.match(filterTab, /flex\s*:\s*0 0 auto/);
  assert.match(filterTab, /width\s*:\s*auto/);
  assert.match(filterTab, /min-height\s*:\s*0/);
  assert.match(filterTab, /flex-direction\s*:\s*row/);
  assert.match(filterTab, /padding\s*:\s*6px\s+12px/);
  assert.match(filterTab, /text-align\s*:\s*center/);
  assert.match(filterTab, /white-space\s*:\s*nowrap/);
  assert.match(filterTabText, /writing-mode\s*:\s*horizontal-tb/);
  assert.match(filterTabText, /text-orientation\s*:\s*mixed/);
  assert.match(filters, /width\s*:\s*max-content/);
  assert.match(filters, /min-width\s*:\s*0/);
  assert.match(filters, /max-width\s*:\s*100%/);
  assert.match(filters, /flex-direction\s*:\s*row/);
  assert.match(filters, /overflow-x\s*:\s*auto/);
  assert.match(mobileFilterTabText, /writing-mode\s*:\s*horizontal-tb/);
  assert.match(allRuleBodies(adminCss, ".admin-tags-grid"), /display\s*:\s*grid/);
  assert.match(allRuleBodies(adminCss, ".admin-tags-grid"), /grid-template-columns\s*:\s*repeat\(4,\s*minmax\(0,\s*240px\)\)/);
  assert.match(allRuleBodies(adminCss, ".admin-tags-grid"), /width\s*:\s*min\(100%,\s*var\(--tags-cards-width\)\)/);
  assert.match(allRuleBodies(adminCss, ".admin-tags-grid"), /margin\s*:\s*0 auto/);
  assert.match(allRuleBodies(adminCss, ".admin-tags-grid"), /justify-content\s*:\s*start/);
  assert.match(allRuleBodies(adminCss, ".admin-tags-grid"), /align-items\s*:\s*stretch/);
  assert.doesNotMatch(allRuleBodies(adminCss, ".admin-tags-grid"), /display\s*:\s*flex/);
  assert.doesNotMatch(allRuleBodies(adminCss, ".admin-tags-grid"), /flex-wrap\s*:\s*wrap/);
  assert.match(grid, /width\s*:\s*min\(100%,\s*320px\)/);
  assert.match(grid, /grid-template-columns\s*:\s*minmax\(0,\s*1fr\)/);
  assert.match(grid, /justify-content\s*:\s*stretch/);
  assert.match(grid, /max-width\s*:\s*100%/);
  assert.doesNotMatch(card, /flex-basis/);
  assert.match(card, /width\s*:\s*100%/);
  assert.match(card, /max-width\s*:\s*100%/);
  assert.match(cardFooter, /flex-wrap\s*:\s*nowrap/);
  assert.match(cardCount, /white-space\s*:\s*nowrap/);
  assert.match(cardActions, /min-width\s*:\s*0/);
  assert.match(cardActions, /white-space\s*:\s*nowrap/);
  assert.match(cardEdit, /white-space\s*:\s*nowrap/);
  assert.match(cardDelete, /white-space\s*:\s*nowrap/);
  assert.match(cardDelete, /width\s*:\s*0/);
  assert.match(cardDelete, /padding-inline\s*:\s*0/);
  assert.match(pagination, /flex-wrap\s*:\s*wrap/);
  assert.match(paginationInfo, /white-space\s*:\s*nowrap/);
  assert.match(tagsPageSource, /<AdminPagination[\s\S]*?itemLabel="标签"[\s\S]*?onPage=\{setPage\}/);
});

test("mobile admin navigation uses a CPA-style sliding drawer", () => {
  const css = mobileCss();
  const shell = ruleBody(css, ".admin-shell");
  const mobileToggle = ruleBody(css, ".admin-mobile-nav-toggle");
  const mobileBackdrop = ruleBody(css, ".admin-mobile-nav-backdrop");
  const visibleBackdrop = ruleBody(css, ".admin-mobile-nav-backdrop.is-visible");
  const mobileSidebar = ruleBody(css, ".admin-sidebar");
  const openSidebar = ruleBody(css, ".admin-sidebar.is-open");
  const mobileNav = ruleBody(css, ".admin-nav");
  const mobileNavLink = ruleBody(css, ".admin-nav__link");
  const activeNavLink = ruleBody(css, ".admin-nav__link.is-active");
  const globalActions = ruleBody(css, ".admin-global-actions");
  const themePopover = ruleBody(css, ".admin-theme-popover");

  assert.match(adminLayoutSource, /useState\(false\)[\s\S]*?mobileNavigationOpen/);
  assert.match(adminLayoutSource, /aria-controls="admin-navigation"/);
  assert.match(adminLayoutSource, /aria-expanded=\{mobileNavigationOpen\}/);
  assert.match(adminLayoutSource, /mobileNavigationOpen \? \(\s*<X[\s\S]*?<Menu/);
  assert.match(adminLayoutSource, /className=\{`admin-mobile-nav-backdrop\$\{mobileNavigationOpen \? " is-visible" : ""\}`\}/);
  assert.match(adminLayoutSource, /document\.addEventListener\("keydown", handleKeyDown\)/);
  assert.match(adminLayoutSource, /event\.key !== "Escape"/);
  assert.match(adminLayoutSource, /root\.classList\.add\("admin-mobile-nav-open"\)/);
  assert.match(adminLayoutSource, /<nav className="admin-nav" onClick=\{\(\) => setMobileNavigationOpen\(false\)\}>/);
  assert.match(shell, /--admin-mobile-header-offset\s*:\s*calc\(58px\s*\+\s*env\(safe-area-inset-top,\s*0px\)\)/);
  assert.match(shell, /display\s*:\s*block/);
  assert.match(shell, /height\s*:\s*auto/);
  assert.match(shell, /overflow\s*:\s*visible/);
  assert.match(mobileToggle, /position\s*:\s*fixed/);
  assert.match(mobileToggle, /top\s*:\s*calc\(6px\s*\+\s*env\(safe-area-inset-top,\s*0px\)\)/);
  assert.match(mobileToggle, /left\s*:\s*var\(--space-2\)/);
  assert.match(mobileToggle, /width\s*:\s*42px/);
  assert.match(mobileBackdrop, /position\s*:\s*fixed/);
  assert.match(mobileBackdrop, /opacity\s*:\s*0/);
  assert.match(mobileBackdrop, /pointer-events\s*:\s*none/);
  assert.match(visibleBackdrop, /opacity\s*:\s*1/);
  assert.match(visibleBackdrop, /pointer-events\s*:\s*auto/);
  assert.match(mobileSidebar, /position\s*:\s*fixed/);
  assert.match(mobileSidebar, /width\s*:\s*min\(280px,\s*calc\(100vw\s*-\s*24px\)\)/);
  assert.match(mobileSidebar, /transform\s*:\s*translateX\(calc\(-100%\s*-\s*24px\)\)/);
  assert.match(mobileSidebar, /visibility\s*:\s*hidden/);
  assert.match(openSidebar, /transform\s*:\s*translateX\(0\)/);
  assert.match(openSidebar, /visibility\s*:\s*visible/);
  assert.match(globalActions, /top\s*:\s*calc\(6px\s*\+\s*env\(safe-area-inset-top,\s*0px\)\)/);
  assert.match(globalActions, /right\s*:\s*var\(--space-2\)/);
  assert.match(ruleBody(css, ".admin-global-action"), /width\s*:\s*34px/);
  assert.match(themePopover, /width\s*:\s*min\(300px,\s*calc\(100vw\s*-\s*16px\)\)/);
  assert.match(ruleBody(css, ".admin-theme-popover__grid"), /grid-template-columns\s*:\s*repeat\(2,/);
  assert.match(mobileNav, /flex-direction\s*:\s*column/);
  assert.match(mobileNav, /align-items\s*:\s*stretch/);
  assert.match(mobileNav, /overflow\s*:\s*visible/);
  assert.match(mobileNavLink, /align-self\s*:\s*stretch/);
  assert.match(mobileNavLink, /width\s*:\s*100%/);
  assert.match(mobileNavLink, /min-height\s*:\s*40px/);
  assert.match(mobileNavLink, /gap\s*:\s*10px/);
  assert.match(ruleBody(css, ".admin-nav__icon"), /display\s*:\s*inline-flex/);
  assert.match(ruleBody(css, ".admin-nav__icon"), /width\s*:\s*18px/);
  assert.match(activeNavLink, /background\s*:\s*var\(--accent-soft\)/);
  assert.match(activeNavLink, /border-color\s*:\s*var\(--border-accent\)/);
  assert.match(activeNavLink, /box-shadow\s*:\s*none/);
  const mobileMain = ruleBody(css, ".admin-main");
  assert.match(mobileMain, /height\s*:\s*auto/);
  assert.match(
    mobileMain,
    /min-height\s*:\s*calc\(100dvh\s*\+\s*var\(--admin-mobile-scroll-range\)\)/
  );
  assert.match(mobileMain, /overflow\s*:\s*visible/);
  assert.match(mobileMain, /padding\s*:\s*var\(--admin-mobile-header-offset\)\s+var\(--space-3\)\s+var\(--space-4\)/);
  assert.match(adminCss, /html\.admin-mobile-nav-open,[\s\S]*?overscroll-behavior:\s*none/);
  assert.match(ruleBody(css, ".admin-page__header"), /margin-bottom\s*:\s*var\(--space-3\)/);
});

test("crawler card keeps the single global preview toggle at the upper left", () => {
  const controls = ruleBody(adminCss, ".admin-crawler-list__controls");
  const teaser = ruleBody(adminCss, ".admin-crawler-global-teaser");

  assert.match(
    crawlersPageSource,
    /\{hasCrawlers && \(\s*<div className="admin-crawler-list__controls">\s*<div className="admin-crawler-global-teaser">/
  );
  assert.equal(crawlersPageSource.match(/className="admin-crawler-global-teaser"/g)?.length, 1);
  assert.doesNotMatch(crawlersPageSource, /admin-crawler-row__preview-toggle|onToggleTeaser/);
  assert.match(controls, /display\s*:\s*flex/);
  assert.match(controls, /align-items\s*:\s*flex-start/);
  assert.match(controls, /justify-content\s*:\s*flex-start/);
  assert.match(controls, /padding\s*:\s*var\(--space-4\)\s+var\(--space-4\)\s+0/);
  assert.match(teaser, /display\s*:\s*inline-grid/);
  assert.match(teaser, /justify-items\s*:\s*center/);
});

test("crawler create action reuses the drive floating action button", () => {
  const fab = ruleBody(adminCss, ".admin-create-fab");

  assert.match(
    crawlersPageSource,
    /className="admin-btn admin-create-fab"[\s\S]*?<Plus size="1em" aria-hidden="true" \/>[\s\S]*?添加爬虫/
  );
  assert.match(fab, /position\s*:\s*fixed/);
  assert.match(fab, /right\s*:\s*var\(--space-7\)/);
  assert.match(fab, /bottom\s*:\s*var\(--space-5\)/);
  assert.match(fab, /min-height\s*:\s*44px/);
  assert.match(fab, /box-shadow\s*:\s*0 12px 32px/);
  assert.doesNotMatch(crawlersPageSource, /admin-crawler-page-actions/);
});

test("tag create action uses the shared desktop fab and preserves the mobile action group", () => {
  const fab = ruleBody(adminCss, ".admin-create-fab");
  const allFabRules = allRuleBodies(adminCss, ".admin-create-fab");

  assert.match(
    tagsPageSource,
    /\{!selectMode && \(\s*<div className="admin-tags-toolbar-actions" data-admin-floating-actions>\s*<button\s+data-admin-floating-actions\s+type="button"\s+className="admin-btn admin-create-fab admin-tags-toolbar-actions__create"\s+onClick=\{openCreateModal\}\s*>\s*<Plus size="1em" aria-hidden="true" \/>\s*新增标签[\s\S]*?onClick=\{toggleSelectMode\}/
  );
  assert.match(fab, /position\s*:\s*fixed/);
  assert.match(fab, /right\s*:\s*var\(--space-7\)/);
  assert.match(fab, /bottom\s*:\s*var\(--space-5\)/);
  assert.match(fab, /min-height\s*:\s*44px/);
  assert.match(fab, /box-shadow\s*:\s*0 12px 32px/);
  assert.match(allFabRules, /right\s*:\s*var\(--space-3\)/);
  assert.match(allFabRules, /bottom\s*:\s*calc\(var\(--space-3\) \+ env\(safe-area-inset-bottom\)\)/);
  assert.equal(Array.from(tagsPageSource.matchAll(/onClick=\{openCreateModal\}/g)).length, 1);
  assert.match(adminCss, /\.admin-tags-toolbar-actions__create > svg\s*\{[^}]*display\s*:\s*none/s);
  assert.match(adminCss, /@media \(max-width: 640px\)[\s\S]*?\.admin-tags-toolbar-actions\s*\{[^}]*position\s*:\s*fixed[^}]*right\s*:\s*var\(--space-3\)[^}]*bottom\s*:\s*calc\(var\(--space-3\) \+ env\(safe-area-inset-bottom\)\)/s);
});
