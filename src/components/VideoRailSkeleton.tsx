export function VideoRailSkeleton() {
  return (
    <aside className="vd-rail" aria-label="视频列表加载中" aria-busy="true">
      <div
        className="vd-rail__tabs vd-rail__tabs--loading"
        aria-hidden="true"
      >
        <span className="vd-rail__tab" aria-selected="true">
          推荐视频
        </span>
        <span className="vd-rail__tab">相关合集</span>
      </div>
      <header className="vd-rail__head vd-rail__head--mobile-only">
        <span className="vd-rail__head-icon" aria-hidden="true">
          <span />
          <span />
        </span>
        <h2 className="vd-rail__head-title">推荐视频</h2>
      </header>
      <VideoRailRowsSkeleton label="正在加载视频列表" />
    </aside>
  );
}

export function VideoRailRowsSkeleton({
  label = "正在加载相关合集",
}: {
  label?: string;
}) {
  return (
    <div
      className="vd-rail__collection-loading"
      role="status"
      aria-label={label}
      aria-busy="true"
    >
      {Array.from({ length: 6 }).map((_, index) => (
        <div key={index} className="vd-rail__loading-row" aria-hidden="true">
          <span className="vd-rail__loading-thumb" />
          <span className="vd-rail__loading-body">
            <span />
            <span />
          </span>
        </div>
      ))}
    </div>
  );
}
