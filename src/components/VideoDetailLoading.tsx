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

              <div className="vd-detail-panels" aria-hidden="true">
                <div className="vd-summary vd-skeleton__summary">
                  <div className="vd-header">
                    <div className="vd-meta vd-skeleton__chips">
                      <span className="vd-skeleton__chip" />
                      <span className="vd-skeleton__chip" />
                      <span className="vd-skeleton__chip" />
                      <span className="vd-skeleton__chip vd-skeleton__chip--mobile-hidden" />
                    </div>
                    <div className="vd-header__title vd-skeleton__title" />
                  </div>
                  <div className="vd-actions vd-skeleton__actions">
                    <div className="vd-actions__group">
                      <span className="vd-actions__pill vd-skeleton__action--like" />
                      <span className="vd-actions__pill vd-skeleton__action--dislike" />
                    </div>
                    <span className="vd-actions__btn vd-actions__share vd-skeleton__action--share" />
                    {isAdmin && (
                      <span className="vd-actions__btn vd-actions__delete vd-skeleton__action--delete" />
                    )}
                  </div>
                </div>

                <div className="vd-info vd-skeleton__info">
                  <div className="vd-info__tags">
                    <div className="vd-info__section-head">
                      <span className="vd-info__section-title vd-skeleton__section-head" />
                      {isAdmin && (
                        <span className="vd-info__tags-edit vd-skeleton__tag-edit" />
                      )}
                    </div>
                    <div className="vd-info__tags-list vd-skeleton__tag-row">
                      <span className="vd-tag" />
                      <span className="vd-tag" />
                      <span className="vd-tag" />
                    </div>
                  </div>
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
