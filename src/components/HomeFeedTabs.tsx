import type { HomeFeedKey } from "@/lib/listingSearchParams";

type Props = {
  feed: HomeFeedKey;
  onChange: (feed: HomeFeedKey) => void;
};

const HOME_FEED_TABS: { key: HomeFeedKey; label: string }[] = [
  { key: "recommend", label: "随机推荐" },
  { key: "latest", label: "最新视频" },
];

export function HomeFeedTabs({ feed, onChange }: Props) {
  return (
    <div className="home-feed-tabs" role="tablist" aria-label="首页视频">
      {HOME_FEED_TABS.map((tab) => {
        const active = tab.key === feed;
        return (
          <button
            key={tab.key}
            type="button"
            role="tab"
            aria-selected={active}
            className={`home-feed-tabs__tab ${active ? "is-active" : ""}`}
            onClick={() => onChange(tab.key)}
          >
            {tab.label}
          </button>
        );
      })}
    </div>
  );
}

export { HOME_FEED_TABS };
