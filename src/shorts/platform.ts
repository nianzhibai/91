// 短视频页的平台 / 浏览器形态探测。这些判定在页面生命周期内视为常量，
// ShortsPage 在渲染前各调用一次并按结果选择播放分支。

export function shouldUseDocumentScrollForShorts() {
  return isIPhoneBrowserShell();
}

/**
 * `?iosPreload=0` 关掉 iOS 备用元素预载（连元素本身都不创建）。
 * 用来隔离"预载抢媒体资源"是否是循环重播失败的原因：同一台机器、同一条
 * 视频，带不带这个参数各看一轮循环即可对照。
 */
export function isIOSStandbyPreloadDisabled() {
  if (typeof window === "undefined") return false;
  return new URLSearchParams(window.location.search).get("iosPreload") === "0";
}

/**
 * 默认让 touchmove 不再阻塞浏览器的纵向滚动起手。`?shortsPassiveTouch=0`
 * 仅用于老版本 iOS 真机回退/A-B；正常路径依赖 `.shorts-feed` 的
 * `touch-action: pan-y` 把横向手势留给视频 seek。
 */
export function shouldUsePassiveShortsTouchMove() {
  if (typeof window === "undefined") return true;
  return (
    new URLSearchParams(window.location.search).get("shortsPassiveTouch") !== "0"
  );
}

/**
 * 默认让 playing <video> 保持完全不透明、无缩放，避免 WebKit 首帧解码时
 * 进入额外混合路径。`?shortsVideoTransition=1` 可在同一台真机上恢复旧动效
 * 做 A/B，或在发现机型兼容问题时临时回退。
 */
export function isLegacyShortsVideoTransitionEnabled() {
  if (typeof window === "undefined") return false;
  return (
    new URLSearchParams(window.location.search).get("shortsVideoTransition") ===
    "1"
  );
}

/**
 * 是否挂载自己的滑动手势控制器（跟手 + 大幅滑动才切屏 + 固定时长落点）。
 *
 * 默认对**所有**设备开启。控制器用的是 pointer 事件，同一套代码同时吃手指
 * 和鼠标——桌面的拖拽翻页是白送的。参考实现 zyronon/douyin 的 SlideVertical
 * 也只绑 pointerdown/move/up，全仓一处 touch 事件都没有；把它拆成"移动端
 * 一套、桌面一套"是我们凭空加的平台特例。
 *
 * iPhone 浏览器壳（文档滚动模式）一度默认排除在外，理由是"接管触摸会让
 * Safari 工具栏收不起来"。真机截图推翻了这个前提：`scroll-snap-type: y
 * mandatory` 每次滑动都把滚动截断，工具栏本来就从没收起过——那是个不存在
 * 的功能，为它牺牲手感不划算。
 *
 * `?shortsPager=0` 整个关掉，回到纯原生滚动（真机回退开关）。
 */
export function shouldUseShortsSwipePager() {
  if (typeof window === "undefined") return false;
  return readShortsPagerOverride() !== "0";
}

/**
 * 是否连浏览器的原生滚动一起接管（关掉 scroll-snap 与 touch-action）。
 *
 * 只有触屏设备需要——手指的位移必须完全由我们写进 transform，不能和浏览器
 * 的滚动预测抢同一根手指。桌面则相反：滚轮和方向键走原生吸附本来就很好用，
 * 没有理由替浏览器重写一遍（参考实现是纯移动端 demo，桌面上只能拖、根本
 * 没有滚轮，那不是我们能照抄的）。桌面只叠加鼠标拖拽：拖动只写 transform，
 * 松手写回的 scrollTop 正好落在吸附点上，与原生吸附不冲突。
 *
 * `?shortsPager=1` 可在桌面上强制走触屏那套，用来做同机对照。
 */
export function shouldTakeOverShortsScrolling() {
  if (typeof window === "undefined") return false;
  const override = readShortsPagerOverride();
  if (override === "0") return false;
  if (override === "1") return true;
  return (
    window.matchMedia?.("(hover: none) and (pointer: coarse)").matches === true
  );
}

function readShortsPagerOverride() {
  return new URLSearchParams(window.location.search).get("shortsPager");
}

export function isWindowsPlatform() {
  if (typeof navigator === "undefined") return false;
  const platform = navigator.platform || "";
  const ua = navigator.userAgent || "";
  return /^Win/i.test(platform) || /\bWindows\b/i.test(ua);
}

export function shouldUseIOSSharedVideo() {
  if (typeof navigator === "undefined") return false;
  const ua = navigator.userAgent || "";
  if (/\biPhone\b|\biPad\b|\biPod\b/.test(ua)) return true;
  // iPadOS 在“请求桌面网站”模式下会伪装成 Macintosh。
  return navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1;
}

function isIPhoneBrowserShell() {
  if (typeof window === "undefined" || typeof navigator === "undefined") {
    return false;
  }
  const ua = navigator.userAgent || "";
  return /\biPhone\b|\biPod\b/.test(ua) && !isStandaloneDisplayMode();
}

function isStandaloneDisplayMode() {
  if (typeof window === "undefined" || typeof navigator === "undefined") {
    return false;
  }
  const nav = navigator as Navigator & { standalone?: boolean };
  return (
    nav.standalone === true ||
    window.matchMedia?.("(display-mode: standalone)").matches === true ||
    window.matchMedia?.("(display-mode: fullscreen)").matches === true
  );
}
