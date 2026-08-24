import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

const drivesPageSource = readFileSync(
  new URL("../src/admin/DrivesPage.tsx", import.meta.url),
  "utf8"
);
const driveComponentsSource = readFileSync(
  new URL("../src/admin/drive/DriveComponents.tsx", import.meta.url),
  "utf8"
);
const storageSummarySource = readFileSync(
  new URL("../src/admin/drive/StorageSummary.tsx", import.meta.url),
  "utf8"
);
const drivesPageLoadingSource = readFileSync(
  new URL("../src/admin/DrivesPageLoading.tsx", import.meta.url),
  "utf8"
);
const skipDirsPanelSource = readFileSync(
  new URL("../src/admin/drive/SkipDirsPanel.tsx", import.meta.url),
  "utf8"
);
const skipDirsLoadingIndicatorSource = readFileSync(
  new URL("../src/admin/drive/SkipDirsLoadingIndicator.tsx", import.meta.url),
  "utf8"
);
const deleteDriveModalSource = readFileSync(
  new URL("../src/admin/drive/DeleteDriveModal.tsx", import.meta.url),
  "utf8"
);
const crawlerPageSource = readFileSync(
  new URL("../src/admin/CrawlersPage.tsx", import.meta.url),
  "utf8"
);
const crawlerPageLoadingSource = readFileSync(
  new URL("../src/admin/CrawlersPageLoading.tsx", import.meta.url),
  "utf8"
);
const adminLayoutSource = readFileSync(
  new URL("../src/admin/AdminLayout.tsx", import.meta.url),
  "utf8"
);
const loginPageSource = readFileSync(
  new URL("../src/admin/LoginPage.tsx", import.meta.url),
  "utf8"
);
const spiderIconSource = readFileSync(
  new URL("../src/admin/icons/SpiderIcon.tsx", import.meta.url),
  "utf8"
);
const confirmModalSource = readFileSync(
  new URL("../src/admin/ConfirmModal.tsx", import.meta.url),
  "utf8"
);
const appSource = readFileSync(
  new URL("../src/App.tsx", import.meta.url),
  "utf8"
);
const crawlerUploadTargetSource = readFileSync(
  new URL("../src/admin/drive/CrawlerUploadTargetField.tsx", import.meta.url),
  "utf8"
);
const driveFormSource = readFileSync(
  new URL("../src/admin/drive/DriveForm.tsx", import.meta.url),
  "utf8"
);
const adminCss = readFileSync(
  new URL("../src/styles/admin.css", import.meta.url),
  "utf8"
);
const adminControlsCss = readFileSync(
  new URL("../src/styles/admin-controls.css", import.meta.url),
  "utf8"
);
const loginCss = readFileSync(
  new URL("../src/styles/login.css", import.meta.url),
  "utf8"
);
const apiSource = readFileSync(
  new URL("../src/admin/api.ts", import.meta.url),
  "utf8"
);
const constantsSource = readFileSync(
  new URL("../src/admin/drive/constants.ts", import.meta.url),
  "utf8"
);
const credentialsSource = readFileSync(
  new URL("../src/admin/drive/credentials.ts", import.meta.url),
  "utf8"
);
const p123QRCodeLoginSource = readFileSync(
  new URL("../src/admin/drive/P123QRCodeLogin.tsx", import.meta.url),
  "utf8"
);
const p115QRCodeLoginSource = readFileSync(
  new URL("../src/admin/drive/P115QRCodeLogin.tsx", import.meta.url),
  "utf8"
);
const quarkQRCodeLoginSource = readFileSync(
  new URL("../src/admin/drive/QuarkQRCodeLogin.tsx", import.meta.url),
  "utf8"
);
const qrLoginSources = [
  quarkQRCodeLoginSource,
  p115QRCodeLoginSource,
  p123QRCodeLoginSource,
  readFileSync(new URL("../src/admin/drive/WopanQRCodeLogin.tsx", import.meta.url), "utf8"),
  readFileSync(new URL("../src/admin/drive/GuangYaPanQRCodeLogin.tsx", import.meta.url), "utf8"),
]
  .join("\n");

const combinedSource = drivesPageSource + "\n" + driveFormSource + "\n" + constantsSource + "\n" + credentialsSource + "\n" + crawlerUploadTargetSource;
const driveIconKinds = [
  "p115",
  "p123",
  "pikpak",
  "guangyapan",
  "onedrive",
  "googledrive",
  "quark",
  "webdav",
  "wopan",
];
const generatedDriveIcons = [{ kind: "localstorage", ext: "png" }];

function driveTypeOptions() {
  const match = /const DRIVE_OPTIONS:\s*DriveOption\[]\s*=\s*\[([\s\S]*?)\];/.exec(
    driveFormSource
  );
  assert.ok(match, "drive option card list should be present");
  return Array.from(
    match[1].matchAll(/\{\s*kind:\s*"([^"]+)",\s*label:\s*"([^"]+)"/g),
    (option) => ({ value: option[1], label: option[2] })
  );
}

function assertDriveTypeOption(value: string, label: string) {
  assert.ok(
    driveTypeOptions().some((option) => option.value === value && option.label === label),
    `${value} drive type option should be present`
  );
}

test("crawler sources are not selectable as storage drives", () => {
  assert.ok(
    !driveTypeOptions().some((option) => option.value === "spider91"),
    "spider91 should not be a storage drive option"
  );
  assert.ok(
    !driveTypeOptions().some((option) => option.value === "scriptcrawler"),
    "scriptcrawler should not be a storage drive option"
  );
});

test("crawler upload target uses explicit local-save option instead of auto target", () => {
  assert.match(combinedSource, /本地保存，不上传/);
  assert.match(
    crawlerPageSource,
    /drives\.filter\(\(d\) => d\.canUpload\)/
  );
  assert.match(apiSource, /canUpload: boolean/);
  assert.doesNotMatch(combinedSource, /自动：唯一/);
  assert.doesNotMatch(combinedSource, /自动模式/);
  assert.doesNotMatch(combinedSource, /较早的视频会上传到该云盘根目录下/);
});

test("crawler upload target select uses an aligned custom arrow", () => {
  assert.match(crawlerUploadTargetSource, /className="admin-form-select-wrap"/);
  assert.match(crawlerUploadTargetSource, /className="admin-form-select"/);
  assert.match(crawlerUploadTargetSource, /className="admin-form-select__icon"/);
  assert.match(adminCss, /\.admin-form__row \.admin-form-select\s*\{[^}]*appearance\s*:\s*none/s);
  assert.match(
    adminCss,
    /\.admin-form-select__icon\s*\{[^}]*top\s*:\s*50%[^}]*right\s*:\s*12px[^}]*transform\s*:\s*translateY\(-50%\)/s
  );
});

test("drive form labels the optional root directory and hides it for localstorage", () => {
  assert.match(driveFormSource, /<label[^>]*>\{rootDirectoryLabel\(form\.kind\)\}<\/label>/);
  assert.match(driveFormSource, /placeholder=\{rootIdPlaceholder\(form\.kind\)\}/);
  assert.match(constantsSource, /kind === "webdav" \? "WebDAV根目录\(可选\)" : "自定义网盘根目录\(可选\)"/);
  assert.match(constantsSource, /if \(kind === "webdav"\) return "可填写目录路径，留空表示根目录"/);
  assert.match(constantsSource, /return "可填写目录ID，留空表示根目录"/);
  assert.doesNotMatch(combinedSource, />根目录 ID</);
  assert.match(
    combinedSource,
    /usesRootDirectoryID\(kind:\s*Kind\):\s*boolean\s*\{\s*return kind !== "localstorage";\s*\}/
  );
  assert.match(combinedSource, /\{usesRootDirectoryID\(form\.kind\) && \(/);
  assert.match(combinedSource, /\{usesRootDirectoryID\(d\.kind\) && \(/);
  assert.ok(
    driveFormSource.indexOf("{usesRootDirectoryID(form.kind) && (") >
      driveFormSource.indexOf("{fields.length > 0 && ("),
    "root directory should be the final form section after credential fields"
  );
  assert.doesNotMatch(combinedSource, /扫描起点目录 ID/);
  assert.doesNotMatch(combinedSource, /set\("scanRootId"/);
});

test("onedrive drive form supports default and custom OAuth applications", () => {
  const match =
    /case "onedrive":\s*return \[([\s\S]*?)\];\s*case "googledrive":/.exec(
      combinedSource
    );
  assert.ok(match, "onedrive credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "auth_mode"/);
  assert.match(fields, /label: "认证方式"/);
  assert.match(fields, /defaultValue: ONEDRIVE_AUTH_MODE_OPENLIST_API/);
  assert.match(fields, /label: "OpenList API"/);
  assert.match(fields, /label: "自建应用"/);
  assert.match(fields, /key: "client_id"/);
  assert.match(fields, /label: "客户端 ID"/);
  assert.match(fields, /key: "client_secret"/);
  assert.match(fields, /label: "客户端密钥"/);
  assert.match(fields, /visibleWhen:[\s\S]*?ONEDRIVE_AUTH_MODE_CUSTOM_APP/);
  assert.match(fields, /请填写客户端密钥值，不是密钥 ID/);
  assert.match(fields, /key: "refresh_token"/);
  assert.match(fields, /与所用应用匹配的 OneDrive refresh_token/);
  assert.doesNotMatch(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /key: "api_url_address"/);
  assert.doesNotMatch(fields, /key: "region"/);
  assert.doesNotMatch(fields, /key: "is_sharepoint"/);
  assert.doesNotMatch(fields, /key: "site_id"/);
  assert.match(driveFormSource, /const visibleFields = fields\.filter/);
  assert.match(driveFormSource, /\{visibleFields\.map\(\(f\) => \(/);
  assert.match(
    driveFormSource,
    /v === ONEDRIVE_AUTH_MODE_OPENLIST_API[\s\S]*?creds\.client_id = "";[\s\S]*?creds\.client_secret = "";/
  );
  assert.match(drivesPageSource, /driveCredentialsForForm\(/);
});

test("googledrive drive form only supports a custom OAuth client", () => {
  assertDriveTypeOption("googledrive", "Google Drive");

  const match =
    /case "googledrive":\s*return \[([\s\S]*?)\];\s*case "webdav":/.exec(
      combinedSource
    );
  assert.ok(match, "googledrive credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "refresh_token"/);
  assert.match(fields, /key: "client_id"/);
  assert.match(fields, /key: "client_secret"/);
  assert.doesNotMatch(fields, /key: "use_online_api"/);
  assert.doesNotMatch(fields, /OpenList 在线 API|api\.oplist\.org/);
  assert.doesNotMatch(fields, /key: "api_url_address"/);
  assert.doesNotMatch(constantsSource, /请参考OpenList文档中关于谷歌云盘的配置方法。/);
  assert.match(driveFormSource, /<select/);
  assert.match(driveFormSource, /value=\{form\.creds\[f\.key\] \?\? f\.defaultValue \?\? ""\}/);
  assert.match(driveFormSource, /className="admin-form-select"/);
  assert.match(driveFormSource, /ChevronDown/);
  assert.doesNotMatch(drivesPageSource, /googleDriveUseOnlineAPI|googleDriveOpenListApiUrl/);
  assert.doesNotMatch(apiSource, /googleDriveUseOnlineAPI|googleDriveOpenListApiUrl/);
  assert.doesNotMatch(fields, /key: "access_token"/);
});

test("webdav drive form asks for a standard endpoint and basic auth credentials", () => {
  assertDriveTypeOption("webdav", "WebDAV");

  const match =
    /case "webdav":\s*return \[([\s\S]*?)\];\s*case "localstorage":/.exec(
      combinedSource
    );
  assert.ok(match, "webdav credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "base_url"/);
  assert.match(fields, /placeholder: "https:\/\/openlist\.example\.com\/dav"/);
  assert.match(fields, /key: "username"/);
  assert.match(fields, /key: "password"/);
  assert.doesNotMatch(fields, /encrypt|crypt|302/i);
});

test("pikpak drive form presents email login before refresh token fallback", () => {
  const match =
    /case "pikpak":\s*return \[([\s\S]*?)\];\s*case "wopan":/.exec(
      combinedSource
    );
  assert.ok(match, "pikpak credential field block should be present");
  const fields = match[1];

  assert.match(
    fields,
    /key: "username"[\s\S]*?label: "邮箱"[\s\S]*?methodLabel: "方式一"[\s\S]*?key: "password"[\s\S]*?key: "refresh_token"[\s\S]*?label: "refresh_token"[\s\S]*?methodLabel: "方式二"/
  );
  assert.doesNotMatch(fields, /key: "platform"/);
  assert.doesNotMatch(fields, /key: "captcha_token"/);
  assert.doesNotMatch(fields, /key: "device_id"/);
  assert.doesNotMatch(fields, /key: "disable_media_link"/);
  assert.doesNotMatch(fields, /推荐/);
  assert.doesNotMatch(
    driveFormSource,
    /优先填写 refresh_token；没有时请同时填写用户名和密码/
  );
  assert.match(
    driveFormSource,
    /className="admin-form__method-label">\{f\.methodLabel\}<\/div>/
  );
  assert.match(
    drivesPageSource,
    /driveCredentialError\(form\.kind, form\.creds, !form\.id\)/
  );
  assert.match(drivesPageSource, /changedCredentialValues\(/);
});

test("all existing drive edits submit credential deltas", () => {
  assert.match(
    drivesPageSource,
    /const credentials = existing\s*\? changedCredentialValues\(/
  );
  assert.doesNotMatch(
    drivesPageSource,
    /existing\s*&&\s*form\.kind\s*===\s*["']pikpak["']/
  );
});

test("busy drive edits are saved and report deferred activation", () => {
  assert.doesNotMatch(drivesPageSource, /const runtimeConfigChanged = Boolean/);
  assert.doesNotMatch(
    drivesPageSource,
    /runtimeConfigChanged && isDriveBusy\(existing\)/
  );
  assert.match(drivesPageSource, /resp\.deferred/);
  assert.match(drivesPageSource, /已保存，将在当前网盘任务结束后生效/);
});

test("wopan drive form omits the optional family space field", () => {
  const match =
    /case "wopan":\s*return \[([\s\S]*?)\];\s*case "guangyapan":/.exec(
      combinedSource
    );
  assert.ok(match, "wopan credential field block should be present");
  assert.doesNotMatch(match[1], /key: "family_id"|家庭空间可选/);
});

test("p115 drive form supports qr login and manual cookie fallback", () => {
  assertDriveTypeOption("p115", "115 网盘");
  assert.match(driveFormSource, /P115QRCodeLogin/);
  assert.match(driveFormSource, /form\.kind === "p115"/);
  assert.match(p115QRCodeLoginSource, /<label>方式一<\/label>/);
  assert.match(p115QRCodeLoginSource, /status\?\.state === "scanned"/);
  assert.match(p115QRCodeLoginSource, /onCookie\(next\.cookie\)/);
  assert.match(apiSource, /startP115QRLogin/);
  assert.match(apiSource, /getP115QRStatus/);
  assert.match(apiSource, /request<P115QRStatus>\("\/drives\/p115\/qr\/status", \{/);
  assert.match(apiSource, /body: JSON\.stringify\(\{/);

  const match =
    /case "p115":\s*return \[([\s\S]*?)\];\s*case "p123":/.exec(
      combinedSource
    );
  assert.ok(match, "p115 credential field block should be present");
  assert.match(match[1], /key: "cookie"/);
  assert.match(match[1], /UID=xxx; CID=xxx; SEID=xxx; KID=xxx/);
  assert.match(match[1], /methodLabel: "方式二"/);
});

test("quark drive form supports qr login and manual cookie fallback", () => {
  assertDriveTypeOption("quark", "夸克网盘");
  assert.match(driveFormSource, /QuarkQRCodeLogin/);
  assert.match(driveFormSource, /form\.kind === "quark"/);
  assert.match(quarkQRCodeLoginSource, /<label>方式一<\/label>/);
  assert.match(quarkQRCodeLoginSource, /onCookie\(next\.cookie\)/);
  assert.match(quarkQRCodeLoginSource, /getQuarkQRStatus\(activeSession\.token\)/);
  assert.match(apiSource, /startQuarkQRLogin/);
  assert.match(apiSource, /getQuarkQRStatus/);
  assert.match(apiSource, /request<QuarkQRStatus>\("\/drives\/quark\/qr\/status", \{/);
  assert.match(apiSource, /body: JSON\.stringify\(\{ token \}\)/);

  const match =
    /case "quark":\s*return \[([\s\S]*?)\];\s*case "p115":/.exec(
      combinedSource
    );
  assert.ok(match, "quark credential field block should be present");
  assert.match(match[1], /key: "cookie"/);
  assert.match(match[1], /methodLabel: "方式二"/);
  assert.match(match[1], /__pus=\.\.\.; __puus=\.\.\.; \.\.\./);
  assert.doesNotMatch(match[1], /use_transcoding_address|客户端 302/);
});

test("p123 drive form exposes qr login and phone or email password login", () => {
  assertDriveTypeOption("p123", "123网盘");
  assert.match(driveFormSource, /P123QRCodeLogin/);
  assert.match(p123QRCodeLoginSource, /<label>方式一<\/label>/);
  assert.match(driveFormSource, /className="admin-form__method-label">\{f\.methodLabel\}<\/div>/);
  assert.doesNotMatch(p123QRCodeLoginSource, /方式一：扫码登录/);
  assert.doesNotMatch(driveFormSource, /方式二：手机号密码登录/);
  assert.match(credentialsSource, /hasScannedToken/);
  assert.match(credentialsSource, /请使用方式一扫码登录，或填写方式二的手机号\/邮箱和密码/);

  const match =
    /case "p123":\s*return \[([\s\S]*?)\];\s*case "pikpak":/.exec(
      combinedSource
    );
  assert.ok(match, "p123 credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "username"/);
  assert.match(fields, /label: "手机号\/邮箱"/);
  assert.match(fields, /methodLabel: "方式二"/);
  assert.match(fields, /key: "password"/);
  assert.match(fields, /label: "密码"/);
  assert.doesNotMatch(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /access_token（推荐用于风控场景）|可选/);
  assert.doesNotMatch(p123QRCodeLoginSource, /已填入 access_token/);
});

test("guangyapan drive form exposes qr login and token fields", () => {
  assertDriveTypeOption("guangyapan", "光鸭网盘");
  assert.match(driveFormSource, /GuangYaPanQRCodeLogin/);
  assert.match(driveFormSource, /form\.kind === "guangyapan"/);
  assert.match(apiSource, /startGuangYaPanQRLogin/);
  assert.match(apiSource, /getGuangYaPanQRStatus/);

  const match =
    /case "guangyapan":\s*return \[([\s\S]*?)\];\s*case "onedrive":/.exec(
      combinedSource
    );
  assert.ok(match, "guangyapan credential field block should be present");
  const fields = match[1];

  assert.doesNotMatch(fields, /key: "root_path"/);
  assert.match(fields, /key: "refresh_token"/);
  assert.match(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /key: "phone_number"/);
  assert.doesNotMatch(fields, /key: "send_code"/);
  assert.doesNotMatch(fields, /key: "verify_code"/);
  assert.doesNotMatch(fields, /key: "captcha_token"/);
  assert.doesNotMatch(fields, /key: "client_id"/);
  assert.doesNotMatch(fields, /key: "device_id"/);
  assert.match(combinedSource, /if \(kind === "guangyapan"\) return ""/);
});

test("localstorage drive form asks for a server directory path", () => {
  assertDriveTypeOption("localstorage", "本地存储");

  const match =
    /case "localstorage":\s*return \[([\s\S]*?)\];\s*\}\s*\}/.exec(
      combinedSource
    );
  assert.ok(match, "localstorage credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "path"/);
  assert.match(fields, /label: "本地目录路径"/);
  assert.match(combinedSource, /if \(kind === "localstorage"\) return "\/"/);
  assert.match(combinedSource, /kind !== "localstorage"/);
  assert.doesNotMatch(combinedSource, /spider91/);
});

test("drive type selector keeps primary source order", () => {
  assert.deepEqual(driveTypeOptions(), [
    { value: "p115", label: "115 网盘" },
    { value: "p123", label: "123网盘" },
    { value: "pikpak", label: "PikPak" },
    { value: "guangyapan", label: "光鸭网盘" },
    { value: "onedrive", label: "OneDrive" },
    { value: "googledrive", label: "Google Drive" },
    { value: "quark", label: "夸克网盘" },
    { value: "wopan", label: "联通网盘" },
    { value: "webdav", label: "WebDAV" },
    { value: "localstorage", label: "本地存储" },
  ]);
});

test("drive create form keeps selected type step visually minimal", () => {
  assert.doesNotMatch(driveFormSource, /302直链|扫码登录|稳定快速|服务器中转模式|本机文件目录/);
  assert.doesNotMatch(driveFormSource, /留空时使用该网盘类型的默认根目录/);
  assert.doesNotMatch(driveFormSource, /重选类型|admin-drive-selected-bar__back|onBack/);
  assert.doesNotMatch(driveFormSource, /凭证配置|credentialHelp|admin-form__help--lead|f\.help/);
  assert.doesNotMatch(
    constantsSource,
    /credentialHelp|help\?:|help:|请参考OpenList文档|填写服务器可访问|扫码成功后会自动填入|Google Cloud Console|路径必须是后端服务器/
  );
  assert.doesNotMatch(
    combinedSource,
    /使用微信或 123网盘 App 扫码并确认登录|使用联通网盘 App 扫码并确认登录|使用光鸭 App 扫码并确认登录/
  );
  assert.doesNotMatch(qrLoginSources, /admin-status|QRStatusClass|statusText|statusClass/);
  assert.doesNotMatch(drivesPageSource, /onBack=\{\(\) => setNameTouched\(false\)\}/);
  assert.doesNotMatch(adminCss, /\.admin-drive-selected-bar__back/);
  assert.doesNotMatch(adminCss, /\.admin-drive-selected-bar__desc/);
  assert.doesNotMatch(adminCss, /\.admin-drive-type-card__desc/);
  assert.doesNotMatch(adminCss, /\.admin-form__help--lead/);
  assert.match(
    adminCss,
    /\.admin-p123-qr\s*\{[^}]*padding\s*:\s*0;[^}]*border\s*:\s*0;[^}]*background\s*:\s*transparent/s
  );
});

test("drive form required fields use save-time prompts instead of label stars", () => {
  assert.doesNotMatch(driveFormSource, /名称 \*/);
  assert.doesNotMatch(driveFormSource, /f\.required && " \*"/);
  assert.doesNotMatch(driveFormSource, /给这个盘起个名字/);
  assert.doesNotMatch(driveFormSource, /nameError|onNameBlur|is-invalid|aria-invalid|aria-describedby|请填写网盘名称/);
  assert.doesNotMatch(drivesPageSource, /nameTouched|setNameTouched|nameError|onNameBlur|请填名称和类型/);
  assert.doesNotMatch(drivesPageSource, /disabled=\{saving \|\| nameMissing\}/);
  assert.match(drivesPageSource, /disabled=\{saving\}/);
  assert.match(drivesPageSource, /if \(!form\.kind\) \{[\s\S]*show\("请选择网盘类型", "error"\)/);
  assert.match(drivesPageSource, /if \(!name\) \{[\s\S]*show\("请填写网盘名称", "error"\)/);
  assert.match(
    drivesPageSource,
    /credentialFields\(form\.kind\)\.find\([\s\S]*field\.required[\s\S]*show\(`请填写\$\{missingField\.label\}`, "error"\)/
  );
});

test("crawler management is a separate admin section", () => {
  assert.match(adminLayoutSource, /to="\/admin\/crawlers"/);
  assert.match(adminLayoutSource, /admin-nav__title">爬虫管理/);
  assert.match(adminLayoutSource, /import \{ SpiderIcon \} from "\.\/icons\/SpiderIcon";/);
  assert.match(adminLayoutSource, /<SpiderIcon size=\{15\} \/>/);
  assert.doesNotMatch(adminLayoutSource, /<Bot\b/);
  assert.match(spiderIconSource, /Font Awesome Pro 7\.3\.1 by @fontawesome/);
  assert.match(spiderIconSource, /Commercial License, Copyright 2026 Fonticons, Inc\./);
  assert.match(spiderIconSource, /xmlns="http:\/\/www\.w3\.org\/2000\/svg"/);
  assert.match(spiderIconSource, /viewBox="0 0 512 512"/);
  assert.match(spiderIconSource, /fill="currentColor"/);
  assert.match(spiderIconSource, /M362\.7-30\.9c7\.2-2\.8 15\.2 0 19\.2 6\.2/);
  assert.match(spiderIconSource, /zM256\.5 128c-26\.5 0-48 21\.5-48 48/);
  assert.doesNotMatch(spiderIconSource, /stroke(?:Width|Linecap|Linejoin)?=/);
  assert.match(
    appSource,
    /path="crawlers"[\s\S]*<PageSuspense fallback=\{<CrawlersPageLoading \/>\}>[\s\S]*<CrawlersPage \/>[\s\S]*<\/PageSuspense>/
  );
  assert.doesNotMatch(crawlerPageSource, /SpiderWebAnimation/);
  assert.doesNotMatch(crawlerPageLoadingSource, /SpiderWebAnimation/);
  assert.doesNotMatch(adminCss, /admin-spider-animation/);
  assert.equal(
    existsSync(new URL("../spider-web-animation.html", import.meta.url)),
    false
  );
  assert.match(crawlerPageSource, /export function CrawlersPage/);
  assert.match(crawlerPageSource, /SpiderIcon/);
  assert.match(crawlerPageSource, /添加爬虫/);
  assert.doesNotMatch(crawlerPageSource, /<h1 className="admin-page__title">爬虫管理<\/h1>/);
  assert.doesNotMatch(crawlerPageSource, /<RefreshCw size=\{14\}[\s\S]*刷新/);
  assert.doesNotMatch(crawlerPageSource, /<Plus size=\{1[34]\}/);
  assert.match(
    crawlerPageSource,
    /className="admin-btn admin-create-fab"\s*onClick=\{\(\) => setEditorTarget\(null\)\}\s*>\s*<Plus size="1em" aria-hidden="true" \/>\s*添加爬虫/
  );
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-page-actions|<header className="admin-page__header">/);
  assert.match(crawlerPageSource, /useAdminFloatingActionSpace<HTMLElement>\(\)/);
  assert.match(crawlerPageSource, /data-admin-floating-actions/);
  assert.match(adminCss, /\.admin-page--with-floating-actions\s*\{[^}]*--admin-floating-actions-space/s);
  assert.doesNotMatch(crawlerPageSource, /className="admin-btn is-primary"[\s\S]*添加爬虫/);
  assert.doesNotMatch(crawlerPageSource, /导入脚本 → 测试运行 → 保存启用，三步接入一个新片源/);
  assert.doesNotMatch(crawlerPageSource, /导入脚本后才能保存/);
  assert.doesNotMatch(crawlerPageSource, /点击选择或拖拽到这里/);
  assert.doesNotMatch(crawlerPageSource, /placeholder="https:\/\/example\.com\/crawler\.py"/);
  assert.doesNotMatch(crawlerPageSource, /选择本地文件或脚本链接|管理当前脚本或替换版本|脚本链接/);
  assert.doesNotMatch(crawlerPageSource, /保存前验证抓取结果/);
  assert.doesNotMatch(crawlerPageSource, /抓取数量、代理和上传目标/);
  assert.match(
    adminCss,
    /\.admin-crawler-editor__summary\s*\{[^}]*grid-template-columns\s*:\s*repeat\(3,\s*minmax\(0,\s*1fr\)\);[^}]*width\s*:\s*100%;[^}]*align-items\s*:\s*stretch/s
  );
  assert.match(
    adminCss,
    /\.admin-crawler-editor-status\s*\{[^}]*grid-template-columns\s*:\s*minmax\(0,\s*1fr\);[^}]*justify-items\s*:\s*center;[^}]*text-align\s*:\s*center/s
  );
  assert.match(adminCss, /\.admin-crawler-editor-status__icon\s*\{[^}]*display\s*:\s*none/s);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-overview/);
  assert.doesNotMatch(crawlerPageSource, /CrawlerMetric/);
  assert.doesNotMatch(crawlerPageSource, /已配置爬虫/);
  assert.match(
    adminCss,
    /\.admin-modal\.admin-modal--crawler\s*\{[^}]*border\s*:\s*0;[^}]*box-shadow\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--crawler \.admin-modal__header,[\s\S]*?\.admin-modal--crawler \.admin-modal__footer\s*\{[^}]*border\s*:\s*0;[^}]*background\s*:\s*var\(--bg-surface\)/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--crawler \.admin-crawler-editor-status,[\s\S]*?\.admin-modal--crawler \.admin-crawler-test-result\s*\{[^}]*border\s*:\s*0;[^}]*box-shadow\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-crawler-current-script\s*\{[^}]*padding\s*:\s*0;[^}]*border\s*:\s*0;[^}]*background\s*:\s*transparent/s
  );
  assert.match(crawlerPageSource, /\{isEdit && form\.scriptPath && \(/);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-current-script__main|admin-crawler-current-script__title|未命名脚本|\{form\.name \|\|/);
  assert.doesNotMatch(adminCss, /admin-crawler-current-script__main|admin-crawler-current-script__title/);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-current-script__icon/);
  assert.doesNotMatch(adminCss, /admin-crawler-current-script__icon/);
  assert.doesNotMatch(adminCss, /\.admin-crawler-current-script\.is-replaced\s*\{[^}]*background/s);
  assert.doesNotMatch(adminCss, /\.admin-crawler-current-script\.is-replaced \.admin-crawler-current-script__icon/);
  // 新设计：列表 + 行内展开详情 + Modal 三步编辑器，删除确认走 ConfirmModal，任务进行中按卡片标记
  assert.match(crawlerPageSource, /CrawlerEditorModal/);
  assert.match(crawlerPageSource, /title=\{isEdit \? \(crawler\?\.name \?\? "编辑爬虫"\) : "添加爬虫"\}/);
  assert.doesNotMatch(crawlerPageSource, /编辑爬虫 ·/);
  assert.match(crawlerPageSource, /\{editorTarget !== undefined && \(\s*<CrawlerEditorModal[\s\S]*key=\{editorTarget\?\.id \?\? "new"\}[\s\S]*open/s);
  assert.match(crawlerPageSource, /useState<EditorForm>\(\(\) => editorFormFromCrawler\(crawler\)\)/);
  assert.doesNotMatch(crawlerPageSource, /open=\{editorTarget !== undefined\}/);
  assert.match(crawlerPageSource, /ConfirmModal/);
  assert.match(crawlerPageSource, /const \[detailTargetId, setDetailTargetId\] = useState\(""\)/);
  assert.doesNotMatch(crawlerPageSource, /<CrawlerDetailModal|function CrawlerDetailModal/);
  assert.doesNotMatch(crawlerPageSource, /className="admin-modal--crawler-detail"/);
  assert.doesNotMatch(crawlerPageSource, /className="admin-crawler-detail-modal__actions"/);
  assert.doesNotMatch(crawlerPageSource, /<Modal open title=\{crawler\.name\}/);
  assert.doesNotMatch(crawlerPageSource, /爬虫详情 ·/);
  assert.doesNotMatch(crawlerPageSource, /aria-haspopup="dialog"/);
  assert.match(crawlerPageSource, /aria-expanded=\{expanded\}/);
  assert.match(crawlerPageSource, /className=\{`admin-crawler-row \$\{expanded \? "is-expanded" : ""\}`\}/);
  assert.doesNotMatch(crawlerPageSource, /任务进行中，自动刷新|admin-crawler-list__head|admin-crawler-list__live/);
  assert.doesNotMatch(adminCss, /admin-crawler-list__head|admin-crawler-list__live/);
  assert.doesNotMatch(crawlerPageSource, /expandedId|ChevronDown|admin-crawler-row__chevron|admin-crawler-row__delete/);
  const crawlerRowSource = crawlerPageSource.match(/function CrawlerRow[\s\S]*?(?=function CrawlerDetail\()/)?.[0] ?? "";
  assert.ok(crawlerRowSource, "crawler row component should exist");
  assert.match(crawlerRowSource, /const crawling = running \|\| crawler\.scanGenerationStatus\?\.state === "scanning"/);
  assert.match(crawlerRowSource, /<SpiderIcon size=\{20\} className="admin-crawler-row__icon" \/>/);
  assert.doesNotMatch(crawlerRowSource, /admin-crawler-row__brand/);
  assert.match(crawlerRowSource, /className="admin-crawler-row__title-line"/);
  assert.match(crawlerRowSource, /className="admin-crawler-row__meta"/);
  assert.match(crawlerRowSource, /admin-status admin-generation-state is-generating[\s\S]*正在抓取/);
  assert.match(crawlerRowSource, /暂停使用/);
  assert.match(crawlerRowSource, /立即抓取/);
  assert.match(crawlerRowSource, /触发上传/);
  assert.match(crawlerRowSource, /编辑/);
  assert.match(crawlerRowSource, /删除/);
  assert.match(crawlerRowSource, /onClick=\{onUpload\}/);
  assert.match(crawlerRowSource, /onClick=\{onEdit\}/);
  assert.match(crawlerRowSource, /onClick=\{onDelete\}/);
  assert.match(crawlerRowSource, /\{expanded && \(/);
  assert.doesNotMatch(crawlerRowSource, /上传视频|admin-crawler-row__delete/);
  const crawlerDetailSource = crawlerPageSource.match(/function CrawlerDetail\([\s\S]*?(?=function crawlerUploadDisplayStatus)/)?.[0] ?? "";
  assert.ok(crawlerDetailSource, "crawler detail component should exist");
  assert.match(crawlerDetailSource, /className="admin-crawler-detail__actions"/);
  assert.match(crawlerDetailSource, /暂停中\.\.\.|暂停/);
  assert.doesNotMatch(crawlerDetailSource, /className="admin-btn is-stop"|停止中\.\.\.|>\s*停止\s*</);
  assert.doesNotMatch(crawlerDetailSource, /编辑|删除|触发上传|admin-gen-col__button|action=\{/);
  assert.doesNotMatch(adminCss, /admin-modal--crawler-detail/);
  assert.match(
    adminCss,
    /\.admin-crawler-detail__actions\s*\{[^}]*display\s*:\s*flex;[^}]*justify-content\s*:\s*flex-end/s
  );
  assert.match(
    adminCss,
    /\.admin-crawler-row__title-line\s*\{[^}]*display\s*:\s*flex;[^}]*flex-wrap\s*:\s*wrap/s
  );
  assert.match(adminCss, /\.admin-crawler-row__main\s*\{[^}]*grid-template-columns\s*:\s*20px minmax\(160px,\s*1fr\)/s);
  assert.match(adminCss, /\.admin-crawler-row__icon\s*\{[^}]*color\s*:\s*var\(--accent\)/s);
  assert.doesNotMatch(adminCss, /\.admin-crawler-row__brand/);
  assert.match(
    adminCss,
    /\.admin-crawler-row__meta\s*\{[^}]*overflow\s*:\s*hidden;[^}]*text-overflow\s*:\s*ellipsis;[^}]*white-space\s*:\s*nowrap/s
  );
  assert.match(
    adminCss,
    /\.admin-crawler-detail__grid\s*\{[^}]*grid-template-columns\s*:\s*repeat\(5,\s*minmax\(0,\s*1fr\)\);[^}]*grid-auto-rows\s*:\s*1fr;[^}]*align-items\s*:\s*stretch/s
  );
  assert.doesNotMatch(adminCss, /grid-template-columns\s*:\s*repeat\(6,\s*minmax\(0,\s*1fr\)\)/);
  assert.doesNotMatch(adminCss, /\.admin-crawler-detail__grid \.admin-gen-col\s*\{[^}]*grid-column\s*:\s*span 2/s);
  assert.doesNotMatch(adminCss, /min-height\s*:\s*148px/);
  assert.doesNotMatch(adminCss, /\.admin-crawler-detail__grid \.admin-gen-col:nth-child\(1\)/);
  assert.doesNotMatch(adminCss, /\.admin-crawler-detail__grid \.admin-gen-col:nth-child\(2\)/);
  assert.match(adminCss, /\.admin-status\.admin-generation-state\s*\{[^}]*gap\s*:\s*0/s);
  assert.match(
    adminCss,
    /\.admin-status\.admin-generation-state::before\s*\{[^}]*content\s*:\s*none;[^}]*display\s*:\s*none/s
  );
  assert.doesNotMatch(adminCss, /admin-gen-col__action|admin-gen-col__button/);
  const crawlerDeleteModal = crawlerPageSource.match(/<ConfirmModal[\s\S]*?title="删除爬虫"[\s\S]*?\/>/)?.[0] ?? "";
  assert.ok(crawlerDeleteModal, "crawler delete confirm modal should exist");
  assert.match(crawlerDeleteModal, /plainConfirm/);
  assert.match(crawlerDeleteModal, /hideIcon/);
  assert.doesNotMatch(crawlerDeleteModal, /details=/);
  assert.doesNotMatch(crawlerDeleteModal, /danger/);
  assert.doesNotMatch(crawlerDeleteModal, /confirmText=/);
  assert.match(crawlerDeleteModal, /正在运行的任务将先自动停止/);
  assert.match(crawlerDeleteModal, /本地保留的视频、封面、预览和抓取文件将一并删除/);
  assert.match(crawlerDeleteModal, /已迁移到网盘的视频不受影响/);
  assert.match(confirmModalSource, /plainConfirm \? "" : danger \? " is-danger" : " is-primary"/);
  assert.match(confirmModalSource, /hideIcon \? " has-no-icon" : ""/);
  assert.match(adminCss, /\.admin-confirm\.has-no-icon\s*\{[^}]*grid-template-columns\s*:\s*minmax\(0,\s*1fr\)/s);
  assert.doesNotMatch(crawlerPageSource, /window\.confirm/);
  assert.match(crawlerPageSource, /POLL_INTERVAL_MS/);
  assert.match(crawlerPageSource, /api\.listCrawlers/);
  assert.match(crawlerPageSource, /api\.listDrives/);
  assert.match(crawlerPageSource, /api\.upsertCrawler/);
  assert.match(crawlerPageSource, /api\.runCrawler/);
  assert.match(crawlerPageSource, /api\.uploadCrawlerVideos/);
  assert.match(crawlerPageSource, /api\.stopCrawlerTasks/);
  assert.match(crawlerPageSource, /api\.setCrawlerPaused/);
  assert.match(crawlerPageSource, /api\.deleteCrawler/);
  assert.match(crawlerPageSource, /api\.importCrawlerScriptFile/);
  assert.match(crawlerPageSource, /api\.importCrawlerScriptURL/);
  assert.match(crawlerPageSource, /api\.testCrawlerScript/);
  assert.match(crawlerPageSource, /type="file"/);
  assert.match(crawlerPageSource, /<button className="admin-btn" type="button" onClick=\{importURL\} disabled=\{importing\}>[\s\S]*导入[\s\S]*<\/button>/);
  assert.match(crawlerPageSource, /placeholder="支持http或socks5代理"/);
  assert.match(crawlerPageSource, /<label htmlFor="crawler-proxy">抓取代理<\/label>/);
  assert.match(crawlerPageSource, /<label htmlFor="crawler-upload-proxy">上传代理<\/label>/);
  assert.match(crawlerPageSource, /uploadProxy: form\.uploadProxy\.trim\(\)/);
  assert.match(crawlerPageSource, /placeholder="仅本地视频上传到网盘时使用"/);
  assert.doesNotMatch(crawlerPageSource, /LinkIcon/);
  assert.match(crawlerPageSource, /<div className="admin-crawler-local-import">\s*<span>本地导入<\/span>[\s\S]*?className=\{`admin-crawler-dropzone/);
  assert.match(crawlerPageSource, /<label htmlFor="crawler-script-url">链接导入<\/label>/);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-import-label/);
  assert.doesNotMatch(adminCss, /admin-crawler-import-label/);
  assert.match(adminCss, /\.admin-crawler-local-import,[\s\S]*?\.admin-crawler-link-import\s*\{[^}]*display\s*:\s*grid;[^}]*gap\s*:\s*6px/s);
  assert.match(adminCss, /\.admin-crawler-local-import > span,[\s\S]*?\.admin-crawler-link-import label\s*\{[^}]*font-size\s*:\s*var\(--font-xs\);[^}]*font-weight\s*:\s*var\(--weight-medium\)/s);
  assert.match(crawlerPageSource, /维护脚本/);
  assert.doesNotMatch(crawlerPageSource, /脚本来源/);
  assert.match(crawlerPageSource, /测试脚本/);
  assert.match(crawlerPageSource, /配置参数/);
  assert.doesNotMatch(crawlerPageSource, /运行参数/);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-panel__icon/);
  assert.doesNotMatch(adminCss, /admin-crawler-panel__icon/);
  assert.doesNotMatch(crawlerPageSource, /<Activity/);
  assert.doesNotMatch(crawlerPageSource, /<TestTube size=\{13\} \/>\s*\{testing \?/);
  assert.match(crawlerPageSource, /测试通过/);
  assert.match(crawlerPageSource, /从原链接更新/);
  assert.match(crawlerPageSource, /替换脚本文件/);
  assert.doesNotMatch(crawlerPageSource, />\s*替换脚本\s*</);
  assert.doesNotMatch(crawlerPageSource, /<RefreshCw size=\{12\} className=\{importing \? "admin-spin" : undefined\} \/>/);
  assert.doesNotMatch(crawlerPageSource, /<Upload size=\{12\} \/>\s*替换脚本文件/);
  assert.match(crawlerPageSource, /CrawlerUploadTargetField/);
  assert.match(crawlerPageSource, /uploadDriveId/);
  assert.match(crawlerPageSource, /api\.setDriveTeaserEnabled/);
  assert.match(crawlerPageSource, /toggleCrawlerTeasers/);
  assert.match(crawlerPageSource, /className="admin-crawler-global-teaser"/);
  assert.equal(crawlerPageSource.match(/className="admin-crawler-global-teaser"/g)?.length, 1);
  assert.match(crawlerPageSource, /const allCrawlerTeasersEnabled = list\.every/);
  assert.doesNotMatch(crawlerPageSource, /暂无爬虫，新增后默认开启预览视频生成/);
  assert.match(
    crawlerPageSource,
    /\{hasCrawlers && \(\s*<div className="admin-crawler-list__controls">\s*<div className="admin-crawler-global-teaser">/
  );
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-row__preview-toggle|onToggleTeaser/);
  assert.match(crawlerPageSource, /className=\{`toggle-switch \$\{allCrawlerTeasersEnabled \? "is-on" : ""\}/);
  assert.match(crawlerPageSource, /role="switch"/);
  assert.match(crawlerPageSource, /aria-checked=\{allCrawlerTeasersEnabled\}/);
  assert.match(crawlerPageSource, /className="toggle-switch__dot"/);
  assert.match(crawlerPageSource, /预览视频/);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-preview-card-toggle/);
  assert.doesNotMatch(crawlerPageSource, /预览：开/);
  assert.doesNotMatch(crawlerPageSource, /预览：关/);
  assert.match(crawlerPageSource, /触发上传/);
  assert.match(crawlerPageSource, /已触发当前爬虫的上传任务/);
  assert.match(crawlerPageSource, /暂停使用/);
  assert.match(crawlerPageSource, /恢复使用/);
  assert.doesNotMatch(crawlerPageSource, /<Download size=\{13\} \/>\s*\{running \? "触发中\.\.\." : "立即抓取"\}/);
  assert.doesNotMatch(crawlerPageSource, /<Upload size=\{13\} \/>\s*\{uploading \? "上传中\.\.\." : "上传视频"\}/);
  assert.doesNotMatch(crawlerPageSource, /<Pencil size=\{13\} \/>\s*编辑/);
  assert.doesNotMatch(crawlerPageSource, /aria-pressed=\{crawler\.teaserEnabled\}/);
  assert.doesNotMatch(crawlerPageSource, /crawlerUploadBlockedReason/);
  assert.doesNotMatch(crawlerPageSource, /disabled=\{uploading/);
  assert.doesNotMatch(crawlerPageSource, /crawlerStatusLabel/);
  assert.match(crawlerPageSource, /\{ label: "已上传", value: crawler\.migratedVideoCount \?\? 0 \}/);
  assert.match(crawlerPageSource, /\{ label: "本地保留", value: crawler\.localVideoCount \?\? 0 \}/);
  assert.doesNotMatch(crawlerPageSource, /label: crawler\.uploadDriveId \? "待上传" : "本地保留"/);
  assert.doesNotMatch(crawlerPageSource, /label: "本轮处理"/);
  assert.doesNotMatch(crawlerPageSource, /label: "本轮总数"/);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-preview-card-toggle \$\{crawler\.teaserEnabled/);
  assert.doesNotMatch(adminCss, /admin-crawler-preview-card-toggle\.is-on/);
  assert.match(adminCss, /\.admin-crawler-list__controls\s*\{[^}]*align-items\s*:\s*flex-start;[^}]*justify-content\s*:\s*flex-start/s);
  assert.match(adminCss, /\.admin-crawler-global-teaser\s*\{[^}]*display\s*:\s*inline-grid;[^}]*justify-items\s*:\s*center/s);
  assert.match(adminCss, /\.admin-crawler-console\s*\{[^}]*width\s*:\s*min\(100%,\s*920px\);[^}]*margin-inline\s*:\s*auto/s);
  assert.match(adminCss, /\.admin-crawler-list\s*\{[^}]*border\s*:\s*0;[^}]*background\s*:\s*transparent;[^}]*box-shadow\s*:\s*none/s);
  assert.match(adminCss, /\.admin-crawler-table\s*\{[^}]*display\s*:\s*grid;[^}]*gap\s*:\s*var\(--space-3\);[^}]*padding\s*:\s*var\(--space-4\)/s);
  assert.match(adminCss, /\.admin-crawler-row\s*\{[^}]*border\s*:\s*1px solid var\(--border-subtle\);[^}]*border-radius\s*:\s*var\(--radius-sm\)/s);
  assert.doesNotMatch(crawlerPageSource, /admin-crawler-pipeline/);
  assert.doesNotMatch(adminCss, /admin-crawler-(pipeline|stage)/);
  assert.doesNotMatch(crawlerPageSource, /teaserEnabled: form\.teaserEnabled/);
  assert.doesNotMatch(crawlerPageSource, /aria-pressed=\{form\.teaserEnabled\}/);
  assert.match(crawlerPageSource, /drives\.filter\(\(d\) => d\.canUpload\)/);
  assert.doesNotMatch(crawlerPageSource, /新建脚本/);
  assert.doesNotMatch(crawlerPageSource, /爬虫 ID/);
  assert.doesNotMatch(crawlerPageSource, /crawler-id/);
  assert.doesNotMatch(crawlerPageSource, /crawler-name/);
  // 脚本路径只读展示，不允许手动填写
  assert.doesNotMatch(crawlerPageSource, /crawler-script-path/);
  assert.doesNotMatch(crawlerPageSource, /Python 解释器/);
  assert.doesNotMatch(crawlerPageSource, /自定义配置 JSON/);
  assert.doesNotMatch(crawlerPageSource, /Bot/);
  // 项目不再内置任何爬虫：不允许出现内置 91 预设
  assert.doesNotMatch(crawlerPageSource, /builtin/);
  assert.doesNotMatch(crawlerPageSource, /内置 91/);
  assert.match(apiSource, /type AdminCrawler/);
  assert.match(apiSource, /uploadDriveId\?: string/);
  assert.match(apiSource, /uploadProxy\?: string/);
  assert.match(apiSource, /paused: boolean/);
  assert.match(apiSource, /teaserEnabled: boolean/);
  assert.doesNotMatch(apiSource, /teaserEnabled\?: boolean/);
  assert.match(apiSource, /"\/crawlers"/);
  assert.match(apiSource, /\/crawlers\/\$\{encodeURIComponent\(id\)\}\/upload/);
  assert.match(apiSource, /\/crawlers\/\$\{encodeURIComponent\(id\)\}\/paused/);
  assert.match(apiSource, /"\/crawlers\/import-file"/);
  assert.match(apiSource, /"\/crawlers\/import-url"/);
  assert.match(apiSource, /"\/crawlers\/test-script"/);
  assert.match(apiSource, /type CrawlerDryRunResult/);
  assert.match(apiSource, /id\?: string/);
  assert.match(apiSource, /new FormData\(\)/);
  assert.doesNotMatch(driveFormSource, /scriptcrawler/);
});

test("system sidebar contains navigation only while global actions live in the layout", () => {
  assert.match(
    adminLayoutSource,
    /admin-nav__group-label">系统[\s\S]*?to="\/admin\/backup"[\s\S]*?to="\/admin\/logs"[\s\S]*?to="\/admin\/settings"/
  );
  assert.doesNotMatch(adminLayoutSource, /to="\/admin\/theme"/);
  assert.doesNotMatch(adminLayoutSource, /admin-nav__action|admin-sidebar__mobile-panel/);
  assert.match(adminLayoutSource, /<AdminGlobalActions[\s\S]*?onCheckUpdate=[\s\S]*?onLogout=/);
});

test("admin layout and its stylesheet load as one lazy route chunk", () => {
  assert.doesNotMatch(appSource, /import \{ AdminLayout \} from "@\/admin\/AdminLayout";/);
  assert.match(
    appSource,
    /const AdminLayout\s*=\s*lazy\(\(\) =>[\s\S]*?import\("@\/admin\/AdminLayout"\)[\s\S]*?default: module\.AdminLayout/
  );
  assert.match(
    adminLayoutSource,
    /import "@\/styles\/admin-controls\.css";\s*import "@\/styles\/admin\.css";/
  );
  assert.match(
    loginPageSource,
    /import "@\/styles\/admin-controls\.css";\s*import "@\/styles\/login\.css";/
  );
  for (const selector of ["admin-form", "admin-password-input", "admin-btn"]) {
    const baseRule = new RegExp(`^\\.${selector}\\s*\\{`, "m");
    assert.match(adminControlsCss, baseRule);
    assert.doesNotMatch(adminCss, baseRule);
    assert.doesNotMatch(loginCss, baseRule);
  }
  assert.match(
    appSource,
    /import \{ DrivesPageLoading \} from "@\/admin\/DrivesPageLoading";/
  );
  assert.match(
    appSource,
    /import \{ CrawlersPageLoading \} from "@\/admin\/CrawlersPageLoading";/
  );
  assert.doesNotMatch(appSource, /<Suspense fallback=\{null\}>\s*<Routes>/);
  assert.match(appSource, /function PageSuspense\(\{[\s\S]*fallback = null,[\s\S]*fallback\?: ReactNode/);
  assert.match(appSource, /path="\/admin"[\s\S]*?<PageSuspense>\s*<AdminLayout \/>\s*<\/PageSuspense>/);
  assert.match(
    appSource,
    /path="drives"[\s\S]*<PageSuspense fallback=\{<DrivesPageLoading \/>\}>[\s\S]*<DrivesPage \/>[\s\S]*<\/PageSuspense>/
  );
  assert.match(
    appSource,
    /path="crawlers"[\s\S]*<PageSuspense fallback=\{<CrawlersPageLoading \/>\}>[\s\S]*<CrawlersPage \/>[\s\S]*<\/PageSuspense>/
  );
  assert.match(
    drivesPageLoadingSource,
    /<StorageSummary storage=\{null\} loading \/>[\s\S]*<DriveListSkeleton \/>/
  );
});

test("drive icons use real assets with abbreviation fallback", () => {
  for (const kind of driveIconKinds) {
    assert.match(
      constantsSource,
      new RegExp(`import ${kind}Icon from "\\./icons/${kind}\\.png"`)
    );
    assert.match(constantsSource, new RegExp(`${kind}:\\s*${kind}Icon`));
    assert.ok(
      existsSync(new URL(`../src/admin/drive/icons/${kind}.png`, import.meta.url)),
      `${kind} icon asset should exist`
    );
  }
  for (const { kind, ext } of generatedDriveIcons) {
    assert.match(
      constantsSource,
      new RegExp(`import ${kind}Icon from "\\./icons/${kind}\\.${ext}"`)
    );
    assert.match(constantsSource, new RegExp(`${kind}:\\s*${kind}Icon`));
    assert.ok(
      existsSync(new URL(`../src/admin/drive/icons/${kind}.${ext}`, import.meta.url)),
      `${kind} generated icon asset should exist`
    );
  }
  assert.match(constantsSource, /googledrive:\s*"GD"/);
  assert.match(constantsSource, /webdav:\s*"WD"/);
  assert.match(constantsSource, /function driveKindAbbr\(kind: string\)/);
  assert.match(constantsSource, /function driveKindIconPath\(kind: string\)/);
  assert.match(constantsSource, /\.slice\(0, 2\)\.toUpperCase\(\)/);
  assert.doesNotMatch(constantsSource, /\/admin-drive-icons\//);
  assert.match(driveFormSource, /driveKindIconPath\(opt\.kind\)/);
  assert.match(driveFormSource, /className="admin-drive-type-card__icon-img"/);
  assert.match(driveFormSource, /className="admin-drive-selected-bar__icon-img"/);
  assert.match(drivesPageSource, /driveKindIconPath\(d\.kind\)/);
  assert.match(drivesPageSource, /driveKindAbbr\(d\.kind\)/);
  assert.match(drivesPageSource, /className="admin-drive-card__brand-icon-img"/);
  assert.match(adminCss, /\.admin-drive-card__brand-icon-img\s*\{[^}]*object-fit\s*:\s*contain/s);
  assert.match(adminCss, /\.admin-drive-card__brand-icon\.has-image\[data-kind\]\s*\{[^}]*background\s*:\s*transparent/s);
});

test("drive management exposes stop task controls", () => {
  assert.match(apiSource, /stopDriveTasks/);
  assert.match(apiSource, /\/drives\/\$\{encodeURIComponent\(id\)\}\/tasks\/stop/);
  assert.match(apiSource, /stopAllTasks/);
  assert.match(apiSource, /"\/tasks\/stop"/);
  assert.match(drivesPageSource, /停止所有任务/);
  assert.match(drivesPageSource, /停止所有存储当前的扫描、封面、预览视频和视频指纹生成任务/);
  assert.doesNotMatch(drivesPageSource, /停止所有网盘当前的扫描、封面、预览视频和视频指纹生成任务。/);
  assert.doesNotMatch(drivesPageSource, /停止所有网盘任务/);
});

test("drive list actions use ordinary text buttons in the requested positions", () => {
  assert.doesNotMatch(
    drivesPageSource,
    /<h1 className="admin-page__title">网盘管理<\/h1>/
  );
  assert.match(
    drivesPageSource,
    /<AdminPageActions>\s*<div className="admin-page__actions admin-drive-list-actions">[\s\S]*?<\/AdminPageActions>\s*\{\(storage \|\| loading\) && \(\s*<StorageSummary storage=\{storage\} loading=\{!storage\} \/>\s*\)\}/
  );
  assert.match(
    drivesPageSource,
    /className="admin-page__actions admin-drive-list-actions"[\s\S]*aria-label="所有网盘任务控制"[\s\S]*onClick=\{handleScanAll\}[\s\S]*onClick=\{handleStopAllTasks\}/
  );
  assert.match(
    drivesPageSource,
    /className="admin-btn admin-create-fab"\s*onClick=\{openCreate\}\s*>\s*<Plus size="1em" aria-hidden="true" \/>\s*添加网盘/
  );
  assert.doesNotMatch(drivesPageSource, /className="admin-drive-footer-actions"/);
  assert.doesNotMatch(drivesPageSource, /PlayCircle/);
  assert.doesNotMatch(drivesPageSource, /<CircleStop size=\{14\}/);
  assert.match(
    drivesPageSource,
    /className="admin-btn"\s+onClick=\{handleScanAll\}[\s\S]*?<Search size="1em" aria-hidden="true" \/>\s*\{scanAllButtonText\(maintenanceStatus, scanningAll\)\}/
  );
  assert.match(
    drivesPageSource,
    /className="admin-btn"\s+onClick=\{handleStopAllTasks\}[\s\S]*?<Ban size="1em" aria-hidden="true" \/>\s*\{stoppingAll \? "停止中\.\.\." : "停止所有任务"\}/
  );
  assert.match(
    drivesPageSource,
    /title=\{form\.id && list\.find\(\(x\) => x\.id === form\.id\) \? "编辑网盘" : "添加网盘"\}/
  );
  assert.match(
    adminCss,
    /\.admin-current-page-header\s*\{[^}]*display\s*:\s*flex[^}]*align-items\s*:\s*center[^}]*justify-content\s*:\s*space-between[^}]*flex-wrap\s*:\s*wrap/s
  );
  assert.match(
    adminCss,
    /\.admin-current-page-actions\s*\{[^}]*display\s*:\s*flex[^}]*justify-content\s*:\s*flex-end[^}]*margin-left\s*:\s*auto/s
  );
  assert.match(
    adminCss,
    /\.admin-drive-list-actions \.admin-task-controls \.admin-btn\s*\{[^}]*padding-inline\s*:\s*var\(--space-4\)[^}]*border-radius\s*:\s*var\(--radius-pill\)[^}]*background\s*:\s*transparent/s
  );
  assert.match(
    adminCss,
    /\.admin-drive-list-actions \.admin-task-controls \.admin-btn:hover:not\(:disabled\)\s*\{[^}]*background\s*:\s*color-mix\(in srgb, var\(--text-default\) 8%, transparent\)[^}]*box-shadow\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-create-fab\s*\{[^}]*position\s*:\s*fixed[^}]*right\s*:\s*var\(--space-7\)[^}]*bottom\s*:\s*var\(--space-5\)[^}]*width\s*:\s*fit-content[^}]*min-width\s*:\s*0[^}]*min-height\s*:\s*44px[^}]*box-shadow\s*:\s*0 12px 32px/s
  );
  assert.match(
    adminCss,
    /\.admin-btn\.admin-create-fab\s*\{[^}]*background\s*:\s*transparent/s
  );
  assert.match(
    adminCss,
    /\.admin-btn\.admin-create-fab:hover:not\(:disabled\)\s*\{[^}]*background\s*:\s*transparent/s
  );
  assert.match(
    adminCss,
    /@media \(max-width: 640px\)\s*\{[\s\S]*?\.admin-drive-list-actions \.admin-btn\s*\{[^}]*background\s*:\s*transparent;[^}]*border-color\s*:\s*var\(--border-subtle\)/s
  );
  assert.match(
    adminCss,
    /@media \(max-width: 640px\)\s*\{[\s\S]*?\.admin-create-fab\s*\{[^}]*right\s*:\s*var\(--space-3\)[^}]*bottom\s*:\s*calc\(var\(--space-3\) \+ env\(safe-area-inset-bottom\)\)/s
  );
  assert.doesNotMatch(drivesPageSource, /admin-drive-list-toolbar|driveListSummary/);
  assert.doesNotMatch(adminCss, /--admin-drives-content-width|\.admin-drive-list-toolbar/);
  assert.doesNotMatch(adminCss, /\.admin-drive-footer-actions/);
});

test("drive loading keeps the storage summary shell and labels without value skeletons", () => {
  for (const label of ["封面占用", "预览视频占用", "本地媒体合计", "磁盘可用"]) {
    assert.match(storageSummarySource, new RegExp(`label: "${label}"`));
  }
  assert.match(storageSummarySource, /aria-busy=\{loading \|\| undefined\}/);
  assert.match(
    storageSummarySource,
    /<strong aria-hidden=\{loading \|\| undefined\}>\s*\{loading \? "\\u00a0" : metric\.value\}\s*<\/strong>/
  );
  assert.doesNotMatch(storageSummarySource, /admin-storage-summary__value-skeleton/);
  assert.doesNotMatch(adminCss, /admin-storage-summary__value-skeleton/);
});

test("empty drive list renders the shared empty visual and prompt", () => {
  const listViewStart = drivesPageSource.indexOf("// --- List view ---");
  const modalStart = drivesPageSource.indexOf("<Modal", listViewStart);
  assert.ok(listViewStart > -1, "list view branch should be present");
  assert.ok(modalStart > listViewStart, "list view modal should follow list content");

  const listViewSource = drivesPageSource.slice(listViewStart, modalStart);
  assert.match(listViewSource, /<AdminEmptyVisual[\s\S]*?variant="empty"[\s\S]*?text="请先添加网盘"[\s\S]*?className="admin-drive-empty-state"/);
  assert.doesNotMatch(listViewSource, /className="admin-card admin-empty"/);
  assert.match(listViewSource, /list\.length > 0 \? \(/);
  assert.match(
    adminCss,
    /\.admin-drive-empty-state\s*\{[^}]*display\s*:\s*flex;[^}]*flex\s*:\s*1 1 auto;[^}]*align-items\s*:\s*center;[^}]*justify-content\s*:\s*center/s
  );
});

test("empty crawler list renders the shared empty visual", () => {
  assert.match(crawlerPageSource, /className="admin-page admin-page--with-floating-actions admin-crawlers-page"/);
  assert.match(
    crawlerPageSource,
    /<AdminEmptyVisual[\s\S]*?variant="empty"[\s\S]*?text="暂无爬虫"[\s\S]*?className="admin-crawler-empty"/
  );
  assert.match(
    crawlerPageSource,
    /\{hasCrawlers && \([\s\S]*?className="admin-crawler-list__controls"[\s\S]*?\)\}\s*\{loading \? \(/
  );
  assert.doesNotMatch(crawlerPageSource, /<SpiderIcon size=\{28\} \/>/);
  assert.match(
    adminCss,
    /\.admin-page\s*\{[^}]*display\s*:\s*flex;[^}]*flex\s*:\s*1 1 auto;[^}]*flex-direction\s*:\s*column;[^}]*min-height\s*:\s*0/s
  );
  assert.doesNotMatch(
    adminCss,
    /\.admin-crawlers-page\s*\{[^}]*min-height\s*:\s*calc\(100(?:d)?vh/s
  );
  assert.match(
    adminCss,
    /\.admin-crawler-empty\s*\{[^}]*flex\s*:\s*1 1 auto;[^}]*min-height\s*:\s*0;[^}]*justify-content\s*:\s*center/s
  );
});

test("drive management status pills omit the leading status dot", () => {
  assert.match(drivesPageSource, /<section className="admin-page admin-drives-page">/);
  assert.match(adminCss, /\.admin-drives-page \.admin-status\s*\{[^}]*gap\s*:\s*0/s);
  assert.match(
    adminCss,
    /\.admin-drives-page \.admin-status::before\s*\{[^}]*content\s*:\s*none;[^}]*display\s*:\s*none/s
  );
});

test("drive form modal uses flatter chrome", () => {
  assert.match(drivesPageSource, /className="admin-modal--drive-form"/);
  assert.match(
    drivesPageSource,
    /title="编辑网盘"[\s\S]*?className="admin-modal--drive-form"/
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form\s*\{[^}]*width\s*:\s*min\(520px,\s*100%\);[^}]*min-height\s*:\s*min\(440px,[^;]+\);[^}]*border\s*:\s*0;[^}]*box-shadow\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-drive-type-picker\s*\{[^}]*align-content\s*:\s*center;[^}]*flex\s*:\s*1 1 auto/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form\s*\{[^}]*scrollbar-width\s*:\s*none;[^}]*-ms-overflow-style\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form::-webkit-scrollbar,[\s\S]*?\.admin-modal--drive-form \.admin-modal__body::-webkit-scrollbar\s*\{[^}]*display\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-modal__body\s*\{[^}]*scrollbar-width\s*:\s*none;[^}]*-ms-overflow-style\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-modal__header,[\s\S]*?\.admin-modal--drive-form \.admin-modal__footer\s*\{[^}]*border\s*:\s*0;[^}]*background\s*:\s*var\(--bg-surface\)/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-form__section\s*\{[^}]*padding\s*:\s*0;[^}]*border\s*:\s*0;[^}]*background\s*:\s*transparent/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-drive-selected-bar\s*\{[^}]*padding\s*:\s*0 0 var\(--space-1\);[^}]*border\s*:\s*0;[^}]*background\s*:\s*transparent/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-form__section \+ \.admin-form__section\s*\{[^}]*margin-top\s*:\s*0/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-drive-type-grid\s*\{[^}]*grid-template-columns\s*:\s*repeat\(3,\s*minmax\(0,\s*1fr\)\);[^}]*width\s*:\s*100%/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-drive-type-card\s*\{[^}]*justify-self\s*:\s*stretch;[^}]*min-width\s*:\s*0;[^}]*min-height\s*:\s*82px;[^}]*border\s*:\s*1px solid transparent;[^}]*background\s*:\s*transparent;[^}]*box-shadow\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-drive-type-card:hover,[\s\S]*?\.admin-modal--drive-form \.admin-drive-type-card:active,[\s\S]*?\.admin-modal--drive-form \.admin-drive-type-card:focus-visible\s*\{[^}]*border-color\s*:\s*var\(--border-default\);[^}]*background\s*:\s*var\(--bg-surface\)/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-drive-type-card__icon,[\s\S]*?\.admin-modal--drive-form \.admin-drive-type-card__icon\[data-kind\]\s*\{[^}]*width\s*:\s*auto;[^}]*height\s*:\s*auto;[^}]*background\s*:\s*transparent/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--drive-form \.admin-drive-type-card__icon\.has-image,[\s\S]*?\.admin-modal--drive-form \.admin-drive-type-card__icon\.has-image\[data-kind\]\s*\{[^}]*width\s*:\s*40px;[^}]*height\s*:\s*40px/s
  );
  assert.match(adminCss, /\.admin-drive-type-card__icon-img\s*\{[^}]*object-fit\s*:\s*contain/s);
  assert.match(
    adminCss,
    /@media \(max-width:\s*520px\)\s*\{[\s\S]*?\.admin-modal--drive-form \.admin-drive-type-grid\s*\{[^}]*grid-template-columns\s*:\s*repeat\(3,\s*minmax\(0,\s*1fr\)\);[^}]*justify-content\s*:\s*stretch/s
  );
});

test("initial add-drive type picker omits cancel and save footer actions", () => {
  assert.match(drivesPageSource, /const \[createDriveTypeSelected, setCreateDriveTypeSelected\]/);
  assert.match(drivesPageSource, /setCreateDriveTypeSelected\(false\);[\s\S]*?setModalOpen\(true\)/);
  assert.match(
    drivesPageSource,
    /footer=\{form\.id \|\| createDriveTypeSelected \? \([\s\S]*?取消[\s\S]*?保存[\s\S]*?\) : undefined\}/
  );
  assert.match(driveFormSource, /onTypeSelected\?\.\(\)/);
});

test("drive credential editor loads stored values on demand", () => {
  assert.match(
    apiSource,
    /getDriveCredentials\(id: string\)[\s\S]*?\/drives\/\$\{encodeURIComponent\(id\)\}\/credentials/
  );
  assert.match(
    drivesPageSource,
    /async function openEdit\(d: api\.AdminDrive\)[\s\S]*?await api\.getDriveCredentials\(d\.id\)[\s\S]*?creds,/
  );
  assert.match(drivesPageSource, /editingCredentialsId === d\.id \? \([\s\S]*?<Loader2 size=\{14\} className="admin-spin" \/>[\s\S]*?加载中[\s\S]*?编辑凭证/);
});

test("drive detail actions use ordinary text buttons", () => {
  const detailViewSource = drivesPageSource.slice(
    drivesPageSource.indexOf("if (selectedDriveId && selectedDrive)"),
    drivesPageSource.indexOf("// --- List view ---")
  );

  assert.match(
    drivesPageSource,
    /className="admin-btn"\s+onClick=\{\(\) => handleRescan\(d\)\}/
  );
  assert.match(
    drivesPageSource,
    /className="admin-btn"\s+onClick=\{\(\) => handleStopDriveTasks\(d\)\}/
  );
  assert.match(
    drivesPageSource,
    /className="admin-btn"\s+onClick=\{\(\) => openEdit\(d\)\}/
  );
  assert.match(
    drivesPageSource,
    /className="admin-btn admin-detail-actions__danger"\s+onClick=\{\(\) => setDeleteTarget\(d\)\}/
  );
  assert.match(drivesPageSource, /stoppingDriveId === d\.id \? "停止中\.\.\." : "停止任务"/);
  assert.match(drivesPageSource, /editingCredentialsId === d\.id \? \([\s\S]*?<Loader2 size=\{14\} className="admin-spin" \/>[\s\S]*?加载中[\s\S]*?编辑凭证/);
  assert.match(drivesPageSource, />\s*删除网盘\s*<\/button>/);
  assert.doesNotMatch(drivesPageSource, /编辑配置凭证/);
  assert.doesNotMatch(
    detailViewSource,
    /<CircleStop[^>]*>|<Trash2[^>]*>|<RefreshCw[^>]*>[\s\S]*?开始扫盘/
  );
  assert.doesNotMatch(adminCss, /\.admin-detail-actions > \.admin-btn:not\(\.is-danger\)/);
});

test("drive delete and credential confirm buttons use ordinary styling", () => {
  const detailViewSource = drivesPageSource.slice(
    drivesPageSource.indexOf("if (selectedDriveId && selectedDrive)"),
    drivesPageSource.indexOf("// --- List view ---")
  );

  assert.match(deleteDriveModalSource, /const primaryText = deleting \? "删除中\.\.\." : "确认"/);
  assert.match(deleteDriveModalSource, /正在运行的任务将先自动停止，完全退出后再删除/);
  assert.match(deleteDriveModalSource, /className="admin-btn"\s+onClick=\{onConfirm\}/);
  assert.doesNotMatch(deleteDriveModalSource, /Trash2|确认删除|is-danger/);
  assert.match(
    detailViewSource,
    /title="编辑网盘"[\s\S]*?className="admin-btn"\s+onClick=\{handleSave\}[\s\S]*?\{saving \? "确认中\.\.\." : "确认"\}/
  );
  assert.doesNotMatch(
    detailViewSource,
    /title="编辑网盘"[\s\S]*?className="admin-btn is-primary"[\s\S]*?\{saving \? "保存中\.\.\." : "保存"\}/
  );
});

test("drive detail header omits type and connection status chips", () => {
  assert.doesNotMatch(drivesPageSource, /admin-drive-detail__header-right/);
  assert.doesNotMatch(drivesPageSource, /admin-drive-detail__kind-chip/);
  assert.doesNotMatch(adminCss, /\.admin-drive-detail__kind-chip/);
});

test("drive rescan reports busy storage tasks instead of queueing duplicates", () => {
  assert.match(apiSource, /accepted:\s*boolean;\s*message\?:\s*string/);
  assert.match(apiSource, /scanGenerationStatus\?: DriveGenerationStatus/);
  assert.match(drivesPageSource, /当前存储有正在进行的任务，请稍后重试/);
  assert.match(drivesPageSource, /function isDriveBusy\(d: api\.AdminDrive\)/);
  assert.match(drivesPageSource, /d\.scanGenerationStatus/);
  assert.match(drivesPageSource, /status\?\.state \|\| "idle"/);
  assert.match(drivesPageSource, /scanningDriveIdsRef\.current\.has\(d\.id\)/);
  assert.match(drivesPageSource, /if \(!resp\.accepted\)/);
  assert.doesNotMatch(drivesPageSource, /disabled=\{!!scanningDriveId\}/);
});

test("scan-all uses its dedicated endpoint and full-scan busy message", () => {
  assert.match(apiSource, /status:\s*MaintenanceJobStatus;\s*message\?:\s*string/);
  assert.match(
    apiSource,
    /function runScanAllJob\(\)[\s\S]*"\/jobs\/scan-all\/run"/
  );
  assert.doesNotMatch(apiSource, /function runNightlyJob\(/);
  assert.match(drivesPageSource, /扫描已配置的存储、处理新视频并执行视频去重/);
  assert.doesNotMatch(drivesPageSource, /扫描已配置网盘、处理新视频并执行视频去重。/);
  assert.match(drivesPageSource, /当前有全量扫描任务正在进行，请稍后重试/);
  assert.match(drivesPageSource, /resp\.message \|\| MAINTENANCE_BUSY_MESSAGE/);
  assert.match(constantsSource, /当前有全量扫描任务正在进行，请稍后重试/);
});

test("drive generation panel shows scan or crawler status first", () => {
  assert.match(driveComponentsSource, /label="扫盘"/);
  assert.match(driveComponentsSource, /status=\{d\.scanGenerationStatus\}/);
  assert.match(driveComponentsSource, /showCounts=\{false\}/);
  assert.match(driveComponentsSource, /status\?\.scannedCount/);
  assert.match(driveComponentsSource, /预计新增/);
  assert.match(apiSource, /scannedCount:\s*number/);
  assert.match(apiSource, /addedCount:\s*number/);
  assert.match(constantsSource, /if \(state === "scanning"\) return "扫盘中"/);
});

test("drive management has no spider91 storage branch", () => {
  assert.doesNotMatch(drivesPageSource, /spider91|91Spider/);
  assert.doesNotMatch(constantsSource, /spider91|91Spider/);
  assert.doesNotMatch(driveComponentsSource, /spider91|91Spider/);
});

test("drive detail selection is stored in the URL history", () => {
  assert.match(drivesPageSource, /useSearchParams/);
  assert.match(drivesPageSource, /searchParams\.get\("drive"\)/);
  assert.match(drivesPageSource, /function openDriveDetail\(id: string\)/);
  assert.match(drivesPageSource, /next\.set\("drive", id\)/);
  assert.match(drivesPageSource, /function closeDriveDetail/);
  assert.match(drivesPageSource, /next\.delete\("drive"\)/);
  assert.doesNotMatch(drivesPageSource, /setSelectedDriveId/);
});

test("drive detail refresh state uses the detail skeleton without list actions", () => {
  const pendingDetailStart = drivesPageSource.indexOf("if (selectedDriveId && !selectedDrive)");
  const listViewStart = drivesPageSource.indexOf("// --- List view ---");
  assert.ok(pendingDetailStart > -1, "pending detail branch should be present");
  assert.ok(listViewStart > pendingDetailStart, "pending detail branch should precede list view");

  const pendingDetailSource = drivesPageSource.slice(pendingDetailStart, listViewStart);
  assert.match(
    pendingDetailSource,
    /if \(loading\) \{[\s\S]*<DriveDetailLoading[\s\S]*onBack=\{\(\) => closeDriveDetail\(\{ replace: true \}\)\}/
  );
  assert.doesNotMatch(pendingDetailSource, /<AdminLoading \/>/);
  assert.match(pendingDetailSource, /网盘不存在/);
  assert.doesNotMatch(pendingDetailSource, /扫描所有网盘|停止所有任务|添加网盘/);
  assert.match(
    drivesPageLoadingSource,
    /className="admin-detail-tree-container"[\s\S]*<SkipDirsLoadingIndicator \/>/
  );
  assert.doesNotMatch(
    drivesPageLoadingSource,
    /保存更改|修改后自动保存|admin-skipdirs-autosave/
  );
  assert.doesNotMatch(drivesPageLoadingSource, /admin-drive-detail-loading__tree/);
  assert.doesNotMatch(adminCss, /admin-drive-detail-loading__tree/);
  assert.match(skipDirsPanelSource, /const showLoading = open && !loaded && !error/);
  assert.match(skipDirsPanelSource, /\{showLoading && <SkipDirsLoadingIndicator \/>\}/);
  assert.match(
    skipDirsLoadingIndicatorSource,
    /className="lds-ellipsis is-xs"/
  );
});

test("drive discard confirmation matches delete confirmation modal styling", () => {
  const discardModals = Array.from(
    drivesPageSource.matchAll(/<ConfirmModal[\s\S]*?title="放弃未保存更改"[\s\S]*?\/>/g),
    (match) => match[0]
  );

  assert.equal(discardModals.length, 2);
  for (const modal of discardModals) {
    assert.match(modal, /danger/);
    assert.match(modal, /centerMessage/);
    assert.match(modal, /modalClassName="admin-modal--delete-confirm"/);
  }
  assert.match(confirmModalSource, /admin-modal--confirm/);
  assert.match(confirmModalSource, /modalClassName \? ` \$\{modalClassName\}` : ""/);
  assert.match(
    adminCss,
    /\.admin-modal--confirm,[\s\S]*?\.admin-modal--delete-confirm\s*\{[^}]*border\s*:\s*0;[^}]*box-shadow\s*:\s*none/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--confirm \.admin-modal__header,[\s\S]*?\.admin-modal--delete-confirm \.admin-modal__footer\s*\{[^}]*border\s*:\s*0/s
  );
  assert.match(
    adminCss,
    /\.admin-modal--confirm \.admin-modal__header \.admin-btn,[\s\S]*?\.admin-modal--delete-confirm \.admin-modal__header \.admin-btn\s*\{[^}]*border-color\s*:\s*transparent;[^}]*box-shadow\s*:\s*none/s
  );
});

test("new drive type selection alone is not treated as unsaved config", () => {
  assert.match(
    drivesPageSource,
    /const formDirty = form\.id\s*\?\s*!sameForm\(form, initialForm\)\s*:\s*hasCreateFormChanges\(form\);/
  );
  assert.match(drivesPageSource, /function handleCreateFormChange\(nextForm: FormState\)/);
  assert.match(
    drivesPageSource,
    /if \(!nextForm\.id && !hasCreateFormChanges\(nextForm\)\) \{\s*setInitialForm\(nextForm\);/
  );
  assert.match(drivesPageSource, /onChange=\{handleCreateFormChange\}/);

  const match = /function hasCreateFormChanges\(form: FormState\): boolean \{([\s\S]*?)\n\}/.exec(
    drivesPageSource
  );
  assert.ok(match, "create form dirty helper should be present");
  const helper = match[1];

  assert.match(helper, /form\.name\.trim\(\) !== ""/);
  assert.match(helper, /form\.rootId\.trim\(\) !== ""/);
  assert.match(helper, /Object\.values\(form\.creds\)\.some/);
  assert.doesNotMatch(helper, /form\.kind/);
});

test("drive generation actions can resume pending work after stop", () => {
  assert.match(driveComponentsSource, /thumbnailPendingCount/);
  assert.match(driveComponentsSource, /teaserPendingCount/);
  assert.match(driveComponentsSource, /fingerprintPendingCount/);
  assert.match(driveComponentsSource, /继续生成封面/);
  assert.match(driveComponentsSource, /继续生成预览视频/);
  assert.match(driveComponentsSource, /继续生成指纹/);
});

test("drive generation actions are iconless and evenly distributed", () => {
  assert.match(
    driveComponentsSource,
    /className="admin-detail-actions admin-generation-actions"/
  );
  assert.match(driveComponentsSource, /重试失败预览/);
  assert.doesNotMatch(driveComponentsSource, /重试失败预览视频/);
  assert.doesNotMatch(driveComponentsSource, /RotateCcw|Wand2|CircleStop/);
  assert.match(
    adminCss,
    /\.admin-generation-actions\s*\{[^}]*grid-template-columns\s*:\s*repeat\(4,\s*minmax\(0,\s*1fr\)\)/s
  );
  assert.match(
    adminCss,
    /\.admin-generation-actions \.admin-btn\s*\{[^}]*width\s*:\s*100%/s
  );
});

test("drive preview generation uses an accessible slider switch", () => {
  assert.match(
    driveComponentsSource,
    /className=\{`toggle-switch \$\{d\.teaserEnabled \? "is-on" : ""\}/
  );
  assert.match(driveComponentsSource, /role="switch"/);
  assert.match(driveComponentsSource, /aria-checked=\{d\.teaserEnabled\}/);
  assert.match(driveComponentsSource, /className="toggle-switch__dot"/);
  assert.match(
    driveComponentsSource,
    /disabled=\{togglingTeaserId === d\.id\}/
  );
  assert.doesNotMatch(driveComponentsSource, /previewSettingBusy|暂不能修改预览开关/);
  assert.doesNotMatch(driveComponentsSource, /预览视频：开|预览视频：关|PowerOff/);
});

test("drive skip directory tree uses a solid selection box without status pills", () => {
  assert.doesNotMatch(skipDirsPanelSource, /SelectedDirsChips/);
  assert.doesNotMatch(skipDirsPanelSource, /admin-mono-cell/);
  assert.doesNotMatch(skipDirsPanelSource, /根目录/);
  assert.match(skipDirsPanelSource, /\{name\}/);
  assert.doesNotMatch(skipDirsPanelSource, /admin-skipdirs-flag|已跳过/);
  assert.doesNotMatch(adminCss, /\.admin-skipdirs-checkbox(?::checked)?::before/);
  assert.match(
    adminCss,
    /\.admin-skipdirs-checkbox:checked\s*\{[^}]*border-color:\s*var\(--accent\)[^}]*background:\s*var\(--accent\)/s
  );
  assert.doesNotMatch(adminCss, /\.admin-skipdirs-flag\s*\{/);
});

test("drive skip directory selections auto-save without polling away local edits", () => {
  assert.doesNotMatch(skipDirsPanelSource, /保存更改|handleSave/);
  assert.match(skipDirsPanelSource, /const AUTO_SAVE_DELAY_MS = 300/);
  assert.match(
    skipDirsPanelSource,
    /saveChainRef\.current = saveChainRef\.current\.then/
  );
  assert.match(
    skipDirsPanelSource,
    /draftRevisionRef\.current !== savedRevisionRef\.current/
  );
  assert.match(skipDirsPanelSource, /保存失败，正在重试…/);
  assert.match(skipDirsPanelSource, /已保存，任务结束后生效/);
  assert.doesNotMatch(skipDirsPanelSource, /drive\.scanGenerationStatus\?\.state/);
  assert.match(skipDirsPanelSource, /disabled=\{disabled\}/);
  assert.doesNotMatch(skipDirsPanelSource, /等待任务完成后再修改跳过目录/);
  assert.doesNotMatch(skipDirsPanelSource, /修改后自动保存/);
  assert.match(skipDirsPanelSource, /saveStatus === "idle"[\s\S]*\? null/);
  assert.match(drivesPageSource, /driveListRequestVersion\.current \+= 1/);
  assert.doesNotMatch(
    drivesPageSource,
    /onSaved=\{\(saved\)[\s\S]*?refreshDriveList\(\);[\s\S]*?\}\}/
  );
});

test("drive cards label fingerprint count as video fingerprint count", () => {
  assert.match(driveComponentsSource, /视频指纹数 \(就绪\/失败\)/);
  assert.doesNotMatch(driveComponentsSource, />指纹数 \(就绪\/失败\)</);
});
