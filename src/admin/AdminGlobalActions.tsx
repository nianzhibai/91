import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { Check, Home, Loader2, LogOut, Palette, RefreshCw } from "lucide-react";
import { applyTheme, getCurrentTheme, isTheme, type Theme } from "@/lib/theme";
import * as api from "./api";
import { useToast } from "@/components/ToastContext";
import { useNativeBackHandler } from "@/lib/useNativeBack";

type ThemeOption = {
  id: Theme;
  title: string;
};

const THEME_OPTIONS: ThemeOption[] = [
  { id: "dark", title: "暗色" },
  { id: "light", title: "亮色" },
];

type AdminGlobalActionsProps = {
  checkingUpdate: boolean;
  loggingOut: boolean;
  onCheckUpdate: () => void;
  onLogout: () => void;
};

/**
 * 后台布局级操作区。侧边栏只保留后台页面导航，返回主站、主题、更新和退出等
 * 全局操作统一放在这里，避免桌面端与移动端维护两套行为。
 */
export function AdminGlobalActions({
  checkingUpdate,
  loggingOut,
  onCheckUpdate,
  onLogout,
}: AdminGlobalActionsProps) {
  const { show } = useToast();
  const actionsRef = useRef<HTMLDivElement>(null);
  const themeTriggerRef = useRef<HTMLButtonElement>(null);
  const [themeMenuOpen, setThemeMenuOpen] = useState(false);
  const [activeTheme, setActiveTheme] = useState<Theme>(getCurrentTheme());
  const [loadingTheme, setLoadingTheme] = useState(true);
  const [savingTheme, setSavingTheme] = useState<Theme | null>(null);
  useNativeBackHandler(themeMenuOpen, () => {
    setThemeMenuOpen(false);
    themeTriggerRef.current?.focus();
  }, "menu");

  useEffect(() => {
    let mounted = true;
    api
      .getSettings()
      .then((settings) => {
        if (!mounted || !isTheme(settings.theme)) return;
        setActiveTheme(settings.theme);
        applyTheme(settings.theme);
      })
      .catch(() => {
        // 启动时同步主题失败不阻塞后台，其余操作仍可正常使用。
      })
      .finally(() => {
        if (mounted) setLoadingTheme(false);
      });

    return () => {
      mounted = false;
    };
  }, []);

  useEffect(() => {
    if (!themeMenuOpen) return;

    function handlePointerDown(event: PointerEvent) {
      if (!actionsRef.current?.contains(event.target as Node)) {
        setThemeMenuOpen(false);
      }
    }

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      setThemeMenuOpen(false);
      themeTriggerRef.current?.focus();
    }

    document.addEventListener("pointerdown", handlePointerDown);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("pointerdown", handlePointerDown);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [themeMenuOpen]);

  async function handleThemeSelect(next: Theme) {
    if (next === activeTheme || savingTheme) return;

    const previous = activeTheme;
    setActiveTheme(next);
    applyTheme(next);
    setSavingTheme(next);

    try {
      const response = await api.updateSettings({ theme: next });
      const savedTheme = isTheme(response.theme) ? response.theme : next;
      setActiveTheme(savedTheme);
      applyTheme(savedTheme);
      setThemeMenuOpen(false);
      show("主题已更新，全站访客将看到新主题", "success");
    } catch (error) {
      setActiveTheme(previous);
      applyTheme(previous);
      show(error instanceof Error ? error.message : "保存失败", "error");
    } finally {
      setSavingTheme(null);
    }
  }

  return (
    <div
      ref={actionsRef}
      className="admin-global-actions"
      role="toolbar"
      aria-label="后台全局操作"
    >
      <Link
        to="/"
        className="admin-global-action"
        title="返回主站"
        aria-label="返回主站"
      >
        <Home size={18} aria-hidden="true" />
      </Link>

      <button
        ref={themeTriggerRef}
        type="button"
        className={`admin-global-action${themeMenuOpen ? " is-active" : ""}`}
        onClick={() => setThemeMenuOpen((open) => !open)}
        title="主题外观"
        aria-label="主题外观"
        aria-haspopup="menu"
        aria-expanded={themeMenuOpen}
      >
        <Palette size={18} aria-hidden="true" />
      </button>

      <button
        type="button"
        className="admin-global-action"
        onClick={onCheckUpdate}
        disabled={checkingUpdate}
        title={checkingUpdate ? "正在检查更新" : "检查更新"}
        aria-label={checkingUpdate ? "正在检查更新" : "检查更新"}
      >
        {checkingUpdate ? (
          <Loader2 className="admin-global-action__spin" size={18} aria-hidden="true" />
        ) : (
          <RefreshCw size={18} aria-hidden="true" />
        )}
      </button>

      <button
        type="button"
        className="admin-global-action admin-global-action--danger"
        onClick={onLogout}
        disabled={loggingOut}
        title={loggingOut ? "正在退出" : "退出登录"}
        aria-label={loggingOut ? "正在退出" : "退出登录"}
      >
        {loggingOut ? (
          <Loader2 className="admin-global-action__spin" size={18} aria-hidden="true" />
        ) : (
          <LogOut size={18} aria-hidden="true" />
        )}
      </button>

      {themeMenuOpen && (
        <div
          className="admin-theme-popover"
          role="menu"
          aria-label="选择全站主题"
          aria-busy={loadingTheme || savingTheme !== null}
        >
          <div className="admin-theme-popover__grid">
            {THEME_OPTIONS.map((option) => {
              const isActive = activeTheme === option.id;
              const isSaving = savingTheme === option.id;
              return (
                <button
                  key={option.id}
                  type="button"
                  className={`theme-card${isActive ? " is-active" : ""}`}
                  data-preview={option.id}
                  onClick={() => void handleThemeSelect(option.id)}
                  disabled={loadingTheme || savingTheme !== null}
                  role="menuitemradio"
                  aria-checked={isActive}
                  aria-label={`切换到${option.title}主题`}
                >
                  <div className="theme-card__preview" aria-hidden="true">
                    <span className="theme-card__bar" />
                    <div className="theme-card__player" />
                    <div className="theme-card__lines">
                      <span className="theme-card__line theme-card__line--lg" />
                      <span className="theme-card__line theme-card__line--md" />
                    </div>
                    <div className="theme-card__chips">
                      <span className="theme-card__chip" />
                      <span className="theme-card__chip" />
                      <span className="theme-card__chip theme-card__chip--accent" />
                    </div>
                  </div>

                  <div className="theme-card__body">
                    <span className="theme-card__title">{option.title}</span>
                    <span className="theme-card__state" aria-hidden="true">
                      {isSaving ? (
                        <Loader2 className="theme-card__spin" size={15} />
                      ) : isActive ? (
                        <Check size={15} />
                      ) : null}
                    </span>
                  </div>
                </button>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}
