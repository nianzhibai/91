type FullscreenDocument = Document & {
  webkitFullscreenElement?: Element | null;
  webkitFullscreenEnabled?: boolean;
  webkitExitFullscreen?: () => Promise<void> | void;
};

type FullscreenRoot = HTMLElement & {
  webkitRequestFullscreen?: () => Promise<void> | void;
};

export function isShortsFullscreen(): boolean {
  if (typeof document === "undefined") return false;
  const doc = document as FullscreenDocument;
  return (doc.fullscreenElement ?? doc.webkitFullscreenElement) === doc.documentElement;
}

export function supportsShortsFullscreen(): boolean {
  if (typeof document === "undefined") return false;
  const doc = document as FullscreenDocument;
  const root = doc.documentElement as FullscreenRoot;
  return Boolean(
    (doc.fullscreenEnabled && root.requestFullscreen) ||
    (doc.webkitFullscreenEnabled && root.webkitRequestFullscreen)
  );
}

/** 仅触控入口自动全屏；电脑端不按窗口宽度推断，进入后由用户主动开启。 */
export async function requestShortsFullscreenOnEntry(): Promise<boolean> {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  if (!window.matchMedia("(hover: none) and (pointer: coarse)").matches) return false;
  return requestShortsFullscreen();
}

/** 必须在点击回调内调用，保留浏览器授予这次手势的全屏权限。 */
export async function requestShortsFullscreen(): Promise<boolean> {
  if (isShortsFullscreen()) return true;
  if (!supportsShortsFullscreen()) return false;
  const doc = document as FullscreenDocument;
  const root = doc.documentElement as FullscreenRoot;
  try {
    if (doc.fullscreenEnabled && root.requestFullscreen) {
      await root.requestFullscreen({ navigationUI: "hide" });
    } else {
      await root.webkitRequestFullscreen!();
    }
    // 进入全屏可能异步完成；用户已离开短视频时不能把下一页带进全屏。
    if (window.location.pathname !== "/shorts") {
      await exitShortsFullscreen();
      return false;
    }
    return isShortsFullscreen();
  } catch {
    // 权限被拒或浏览器不支持时保留普通播放，可由下一次点击重新尝试。
    return false;
  }
}

/** 只退出短视频使用的文档根元素全屏，不干预其他播放器的全屏元素。 */
export async function exitShortsFullscreen(): Promise<void> {
  if (!isShortsFullscreen()) return;
  const doc = document as FullscreenDocument;
  try {
    if (doc.exitFullscreen) await doc.exitFullscreen();
    else await doc.webkitExitFullscreen?.();
  } catch {
    // 系统可能已经先完成退出；无需打断后续的路由导航。
  }
}

export function observeShortsFullscreen(onChange: (active: boolean) => void) {
  let previous = isShortsFullscreen();
  onChange(previous);
  const handleChange = () => {
    const active = isShortsFullscreen();
    if (active === previous) return;
    previous = active;
    onChange(active);
  };
  const handleKeyDown = (event: KeyboardEvent) => {
    // 内嵌浏览器可能把 Esc 交给页面；原生浏览器会先自行退出全屏。
    // 菜单已处理的 Esc 和其他播放器的全屏继续由各自负责。
    if (event.key !== "Escape" || event.defaultPrevented || !isShortsFullscreen()) return;
    event.preventDefault();
    void exitShortsFullscreen();
  };
  document.addEventListener("fullscreenchange", handleChange);
  document.addEventListener("webkitfullscreenchange", handleChange);
  document.addEventListener("keydown", handleKeyDown);
  return () => {
    document.removeEventListener("fullscreenchange", handleChange);
    document.removeEventListener("webkitfullscreenchange", handleChange);
    document.removeEventListener("keydown", handleKeyDown);
  };
}
