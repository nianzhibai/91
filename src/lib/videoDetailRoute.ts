type VideoDetailPageModule = typeof import("@/pages/VideoDetailPage");

let routeModulePromise: Promise<VideoDetailPageModule> | null = null;
let idlePreloadScheduled = false;

/**
 * Keep React.lazy and navigation-intent preloading on the same import promise.
 * Reset a failed preload so a later click can retry instead of retaining a
 * rejected module for the rest of the browser session.
 */
export function loadVideoDetailPage(): Promise<VideoDetailPageModule> {
  if (!routeModulePromise) {
    routeModulePromise = import("@/pages/VideoDetailPage").catch((error) => {
      routeModulePromise = null;
      throw error;
    });
  }
  return routeModulePromise;
}

export function preloadVideoDetailPage(): void {
  void loadVideoDetailPage().catch(() => undefined);
}

/**
 * Video grids can mount more than once on the home page. Schedule one shared
 * idle preload so those grids do not create duplicate timers or downloads.
 */
export function scheduleVideoDetailPagePreload(): void {
  if (
    routeModulePromise ||
    idlePreloadScheduled ||
    typeof window === "undefined"
  ) {
    return;
  }

  idlePreloadScheduled = true;
  const preload = () => {
    idlePreloadScheduled = false;
    preloadVideoDetailPage();
  };

  if (typeof window.requestIdleCallback === "function") {
    window.requestIdleCallback(preload, { timeout: 1_500 });
    return;
  }
  window.setTimeout(preload, 250);
}
