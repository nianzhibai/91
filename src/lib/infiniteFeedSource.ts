import {
  fetchVideoFeed,
  VideoFeedExpiredError,
  type VideoFeedCursor,
  type VideoFeedKind,
} from "@/data/videos";
import { infiniteListingKey } from "@/lib/infiniteListing";
import type { SortKey, VideoItem } from "@/types";

/**
 * 无限滚动的数据来源。source 只描述逻辑 feed 和批次大小；快照 token、游标、
 * 累积、缓存与取消统一由 useInfiniteListing 管理。
 */

export type InfiniteFeedRequest = {
  cursor: VideoFeedCursor;
  size: number;
};

export type InfiniteFeedBatch = {
  items: VideoItem[];
  total: number;
  cursor: VideoFeedCursor;
  exhausted: boolean;
};

export type InfiniteFeedSource = {
  /** 同一个 key 代表同一个逻辑结果集；批次大小不是结果集身份的一部分。 */
  key: string;
  batchSize: number;
  fetchBatch: (
    request: InfiniteFeedRequest,
    options: { signal: AbortSignal }
  ) => Promise<InfiniteFeedBatch>;
  isExpiredError: (error: unknown) => boolean;
};

export type ListingFeedQuery = {
  q: string;
  tag: string;
  sort: SortKey;
  pageSize: number;
};

function snapshotFeedSource(input: {
  key: string;
  kind: VideoFeedKind;
  batchSize: number;
  q?: string;
  tag?: string;
  sort?: SortKey;
}): InfiniteFeedSource {
  return {
    key: input.key,
    batchSize: input.batchSize,
    isExpiredError: (error) => error instanceof VideoFeedExpiredError,
    fetchBatch: async (request, options) => {
      const response = await fetchVideoFeed(
        {
          kind: input.kind,
          cursor: request.cursor,
          count: request.size,
          q: input.q,
          tag: input.tag,
          sort: input.sort,
        },
        { signal: options.signal }
      );
      return {
        items: response.items,
        total: response.total,
        cursor: {
          feedToken: response.feedToken,
          position: response.nextCursor,
        },
        exhausted: response.exhausted,
      };
    },
  };
}

export function listingFeedSource(query: ListingFeedQuery): InfiniteFeedSource {
  return snapshotFeedSource({
    key: `listing:${infiniteListingKey(query)}`,
    kind: "listing",
    batchSize: query.pageSize,
    q: query.q.trim(),
    tag: query.tag.trim(),
    sort: query.sort,
  });
}

export const HOME_RECOMMENDATION_BATCH_SIZE = 12;

export function homeRecommendationFeedSource(): InfiniteFeedSource {
  return snapshotFeedSource({
    key: "home:recommend",
    kind: "recommend",
    batchSize: HOME_RECOMMENDATION_BATCH_SIZE,
  });
}

export function homeLatestFeedSource(pageSize: number): InfiniteFeedSource {
  return snapshotFeedSource({
    key: "home:latest",
    kind: "latest",
    batchSize: pageSize,
    sort: "latest",
  });
}
