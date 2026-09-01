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
 * 是否由页面自己接管上下滑动（惯性收尾 + 大幅滑动才切屏），替代浏览器的
 * 原生 scroll-snap。默认只在"触屏是主输入"的设备上启用：桌面和触控本
 * （hover: hover）继续走原生滚动 + 滚轮 / 键盘，行为完全不变。
 *
 * iPhone 浏览器壳走的是文档滚动，目的是让 Safari 工具栏随刷动收起；接管
 * 触摸会让工具栏永远展开，属于功能回退，所以默认不启用。
 *
 * `?shortsPager=0` 强制回到原生滚动（真机回退开关）；`?shortsPager=1`
 * 强制开启，用来在 iPhone 文档滚动模式或桌面上做同机 A/B。
 */
export function shouldUseShortsTouchPager(usesDocumentScroll: boolean) {
  if (typeof window === "undefined") return false;
  const override = new URLSearchParams(window.location.search).get(
    "shortsPager"
  );
  if (override === "0") return false;
  if (override === "1") return true;
  if (usesDocumentScroll) return false;
  return (
    window.matchMedia?.("(hover: none) and (pointer: coarse)").matches === true
  );
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
