import { type MouseEvent } from "react";
import { Link } from "react-router";
import {
  Clock3,
  Flame,
  Home,
  Settings,
  ThumbsUp,
} from "lucide-react";
import { SHORTS_FEED_TABS, type ShortsFeedMode } from "./shortsFeed";

const DESKTOP_FEED_TABS = [
  ...SHORTS_FEED_TABS.filter((tab) => tab.key === "recommend"),
  ...SHORTS_FEED_TABS.filter((tab) => tab.key !== "recommend"),
];

type NavigationProps = {
  isAdmin: boolean;
  mode: ShortsFeedMode;
  isDesktop: boolean;
  onModeChange: (mode: ShortsFeedMode, restoreFocus?: boolean) => void;
  onHomeClick: (event: MouseEvent<HTMLAnchorElement>) => void;
  onRouteClick: (event: MouseEvent<HTMLAnchorElement>, destination: string) => void;
};

export function ShortsDesktopSidebar({
  isAdmin,
  mode,
  isDesktop,
  onModeChange,
  onHomeClick,
  onRouteClick,
}: NavigationProps) {
  return (
    <aside className="shorts-desktop-sidebar" aria-label="网站导航">
      <div className="shorts-desktop-brand">
        <img src="/icon.png" alt="91" />
        <span className="shorts-desktop-brand__name">nine one</span>
      </div>
      <nav className="shorts-desktop-nav" aria-label="浏览视频">
        <div className="shorts-header__tabs" role="tablist" aria-label="短视频排序"
          aria-orientation={isDesktop ? "vertical" : "horizontal"}>
          {(isDesktop ? DESKTOP_FEED_TABS : SHORTS_FEED_TABS).map((tab, index, tabs) => {
            const Icon = tab.key === "recommend" ? ThumbsUp : tab.key === "latest" ? Clock3 : Flame;
            return (
              <button key={tab.key} id={`shorts-tab-${tab.key}`} type="button" role="tab"
                className="shorts-header__tab" aria-selected={mode === tab.key}
                aria-controls="shorts-feed" tabIndex={mode === tab.key ? 0 : -1}
                onClick={(event) => onModeChange(tab.key, event.detail === 0)}
                onKeyDown={(event) => {
                  let nextIndex: number;
                  switch (event.key) {
                    case "ArrowLeft": case "ArrowUp":
                      nextIndex = (index + tabs.length - 1) % tabs.length;
                      break;
                    case "ArrowRight": case "ArrowDown":
                      nextIndex = (index + 1) % tabs.length;
                      break;
                    case "Home": nextIndex = 0; break;
                    case "End": nextIndex = tabs.length - 1; break;
                    default: return;
                  }
                  event.preventDefault();
                  event.stopPropagation();
                  onModeChange(tabs[nextIndex].key, true);
                }}>
                <Icon className="shorts-desktop-tab-icon" size={21} /><span>{tab.label}</span>
              </button>
            );
          })}
        </div>
        <div className="shorts-desktop-nav__divider" />
        <Link to="/" onClick={onHomeClick}><Home size={21} /><span>首页</span></Link>
        {isAdmin && (
          <Link to="/admin" onClick={(event) => onRouteClick(event, "/admin")}>
            <Settings size={21} /><span>后台</span>
          </Link>
        )}
      </nav>
      <div className="shorts-desktop-sidebar__footer">
        <p><span className="shorts-desktop-sidebar__slogan-lead">探索</span><span>精彩瞬间</span></p>
      </div>
    </aside>
  );
}

export function ShortsFullscreenExit({ onExit }: { onExit: () => void }) {
  return (
    <button type="button" className="shorts-fullscreen-exit" data-shorts-no-swipe=""
      aria-label="退出全屏" aria-keyshortcuts="H Escape"
      onClick={(event) => { event.stopPropagation(); onExit(); }}>
      <svg width="18" height="18" viewBox="0 0 18 18" fill="currentColor" aria-hidden="true" focusable="false">
        <path d="M17.448 17.448a1.886 1.886 0 0 1-2.668 0L9 11.668l-5.78 5.78A1.886 1.886 0 1 1 .552 14.78L6.332 9 .552 3.22A1.886 1.886 0 1 1 3.22.552L9 6.332l5.78-5.78a1.886 1.886 0 1 1 2.668 2.668L11.668 9l5.78 5.78a1.886 1.886 0 0 1 0 2.668z" />
      </svg>
    </button>
  );
}
