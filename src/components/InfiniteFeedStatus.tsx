type Props = {
  state: "loading" | "end";
};

export function InfiniteFeedStatus({ state }: Props) {
  if (state === "end") {
    return (
      <div className="listing-infinite-status listing-infinite-status--end">
        没有更多了
      </div>
    );
  }

  return (
    <div className="listing-infinite-status" role="status" aria-live="polite">
      <span
        className="video-grid-refresh-overlay__spinner"
        aria-hidden="true"
      />
      <span>正在加载更多</span>
    </div>
  );
}
