// 主题系统：管理 <html data-theme> 属性 + localStorage 缓存。
//
// 流程：
//   1. index.html 内联脚本在挂载前先把 localStorage 里的值同步到 <html>，
//      避免首屏闪烁。
//   2. main.tsx 调 syncThemeFromServer()，异步 GET /api/settings/theme，
//      若与本地不同则覆盖。
//   3. 管理后台全局主题菜单切换时调 applyTheme(theme)，立刻生效。
//
// 公开端点 /api/settings/theme 不需要登录，原因见 backend/internal/api/api.go 中
// 的注释——登录页本身就要在用户登录之前正确显示主题。

export type Theme = "dark" | "light";

export const THEMES: Theme[] = ["dark", "light"];
const STORAGE_KEY = "video-site:theme";

export function isTheme(value: unknown): value is Theme {
  return value === "dark" || value === "light";
}

/**
 * 拿到当前 DOM 上生效的主题。如果 <html data-theme> 没设，返回 "dark"（兜底）。
 */
export function getCurrentTheme(): Theme {
  if (typeof document === "undefined") return "dark";
  const v = document.documentElement.getAttribute("data-theme");
  return isTheme(v) ? v : "dark";
}

/**
 * 立即把主题应用到 <html data-theme> 并写入 localStorage。
 * 用于管理后台切换时本地立即生效。
 *
 * 入参非法时（旧版后端可能不返主题字段，此时 theme 会是 undefined / ""）
 * 直接忽略，避免 setAttribute("data-theme", "undefined") 这类污染。
 */
export function applyTheme(theme: Theme | string | undefined | null): void {
  if (!isTheme(theme)) {
    return;
  }
  if (typeof document !== "undefined") {
    document.documentElement.setAttribute("data-theme", theme);
  }
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // 隐私模式 / quota 用尽：忽略
  }
}

/**
 * 从公开端点 /api/settings/theme 拉服务端配置的主题，覆盖本地。
 * 失败时不抛错，只是保持本地缓存的值。
 */
export async function syncThemeFromServer(): Promise<Theme> {
  try {
    const res = await fetch("/api/settings/theme", {
      credentials: "include",
      cache: "no-store",
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = (await res.json()) as { theme?: unknown };
    if (isTheme(data.theme)) {
      applyTheme(data.theme);
      return data.theme;
    }
  } catch {
    // 网络失败：保留 localStorage / data-theme 的现状
  }
  return getCurrentTheme();
}
