import { AppShell } from "./AppShell";
import { VideoRailSkeleton } from "./VideoRailSkeleton";

export function VideoDetailLoading({ isAdmin = false }: { isAdmin?: boolean }) {
  return (
    <AppShell mobileAutoHideNav>
      <div className="vd-page">
        <div className="vd-ambient" aria-hidden="true" />
        <div className="container vd-page__inner">
          <div
            className="vd-layout vd-skeleton"
            aria-busy="true"
            aria-label="视频详情加载中"
          >
            <div className="vd-main">
              <div className="vd-skeleton__player" />

              <div className="vd-skeleton__summary">
                <div className="vd-skeleton__chips">
                  <span className="vd-skeleton__chip" />
                  <span className="vd-skeleton__chip" />
                  <span className="vd-skeleton__chip" />
                  <span className="vd-skeleton__chip vd-skeleton__chip--mobile-hidden" />
                </div>
                <div className="vd-skeleton__title" />
                <div className="vd-skeleton__actions">
                  <span className="vd-skeleton__action--like" />
                  <span className="vd-skeleton__action--dislike" />
                  <span className="vd-skeleton__action--share" />
                  {isAdmin && (
                    <span className="vd-skeleton__action--delete" />
                  )}
                </div>
              </div>

              <div className="vd-skeleton__info">
                <span className="vd-skeleton__section-head" />
                <div className="vd-skeleton__tag-row">
                  <span />
                  <span />
                  <span />
                </div>
              </div>
            </div>

            <VideoRailSkeleton />
          </div>
        </div>
      </div>
    </AppShell>
  );
}
