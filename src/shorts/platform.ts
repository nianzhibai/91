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
 * 短视频页是否由自己接管全部滚动输入——拖拽、滚轮、原生吸附一并接管。
 *
 * 对**所有**设备、**所有**输入方式一视同仁，这是刻意的：这个页面存在的整个
 * 理由就是"浏览器控制的吸附滚动手感不可控"，凡是还留给浏览器的输入通道，
 * 那份不可控就原样保留在那里。曾经分过两次特例，两次都被证伪：
 *
 * - "iPhone 走文档滚动，接管触摸会让 Safari 工具栏收不起来" —— 真机截图
 *   显示有 `scroll-snap-type: y mandatory` 在时工具栏本来就从没收起过。
 * - "桌面滚轮走原生吸附本来就好用" —— 实测手感和移动端原来一样糟，而且
 *   保留吸附还让鼠标拖拽彻底失效（吸附点跟着轨道 transform 走，mandatory
 *   会反向拉 scrollTop 把位移抵消掉）。
 *
 * 控制器用 pointer 事件 + wheel，同一套判定同时服务手指、鼠标拖拽和滚轮，
 * 三者共用同一条落点动画。参考实现 zyronon/douyin 也只绑 pointer 事件
 * （它是纯移动端 demo，没有滚轮，那一半是我们自己补的）。
 *
 * `?shortsPager=0` 整个关掉，回到纯原生滚动（真机回退开关）。
 */
export function shouldUseShortsSwipePager() {
  if (typeof window === "undefined") return false;
  return new URLSearchParams(window.location.search).get("shortsPager") !== "0";
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
