import { fetchHomeVideos, fetchListing } from "@/data/videos";
import { infiniteListingKey } from "@/lib/infiniteListing";
import type { SortKey, VideoItem } from "@/types";

/**
 * 无限滚动的数据来源。累积、去重、游标推进由 useInfiniteListing 统一负责，
 * 各个 feed 只描述"第 N 批怎么取"。
 */

export type InfiniteFeedBatch = { items: VideoItem[]; total: number };

export type InfiniteFeedSource = {
  /** 同一个 key 代表同一条累积会话，变了就重新开始。 */
  key: string;
  /** 每批请求多少条。 */
  batchSize: number;
  /**
   * 服务端轮换 feed 既没有 total、也永远返回满批，只能靠"整批都是已看过的
   * 内容"判断转完了一圈。
   */
  stopOnDuplicateBatch?: boolean;
  /**
   * 是否可以用一次大请求把之前的进度补回来。轮换 feed 的游标在服务端且不
   * 幂等，补回来的不是原来那些视频，因此不支持。
   */
  supportsRestore?: boolean;
  fetchBatch: (
    request: { offset: number; size: number },
    options: { signal: AbortSignal }
  ) => Promise<InfiniteFeedBatch>;
};

/**
 * page/size 接口只能表达页边界上的区间，所以偏移量必须是批大小的整数倍；
 * 恢复现场时的大请求也因此被约束成批大小的整数倍。
 */
export function listingPageFromOffset(
  offset: number,
  size: number
): number | null {
  if (!Number.isInteger(offset) || offset < 0) return null;
  if (!Number.isInteger(size) || size <= 0) return null;
  if (offset % size !== 0) return null;
  return offset / size + 1;
}

export type ListingFeedQuery = {
  q: string;
  tag: string;
  sort: SortKey;
  pageSize: number;
};

/** /api/list：真正的分页，有 total，可以恢复现场。 */
export function listingFeedSource(query: ListingFeedQuery): InfiniteFeedSource {
  return {
    key: `listing:${infiniteListingKey(query)}`,
    batchSize: query.pageSize,
    supportsRestore: true,
    fetchBatch: (request, options) => {
      const page = listingPageFromOffset(request.offset, request.size);
      if (page === null) {
        return Promise.reject(
          new Error(
            `Listing offset ${request.offset} is not aligned to size ${request.size}`
          )
        );
      }
      return fetchListing(
        page,
        request.size,
        { q: query.q, tag: query.tag, sort: query.sort },
        { signal: options.signal }
      );
    },
  };
}

/** /api/home 单次上限，后端超过就直接 400。 */
export const HOME_RECOMMENDATION_BATCH_SIZE = 12;

/**
 * /api/home：整库随机轮换，游标由服务端按会话维护，请求里带不了偏移量。
 * 每次拿一批新的随机视频接到列表尾部，转完一圈后开始重复，此时收尾。
 */
export function homeRecommendationFeedSource(): InfiniteFeedSource {
  return {
    key: "home:recommend",
    batchSize: HOME_RECOMMENDATION_BATCH_SIZE,
    stopOnDuplicateBatch: true,
    supportsRestore: false,
    fetchBatch: async (request) => {
      const items = await fetchHomeVideos(
        Math.min(request.size, HOME_RECOMMENDATION_BATCH_SIZE)
      );
      // 轮换 feed 不知道总量，交给"整批重复"和"返回不足一批"两个收尾条件。
      return { items, total: 0 };
    },
  };
}

/**
 * 首页"最新视频"走 /api/list?sort=latest 而不是 /api/home/latest：后者单次
 * 上限 12 条且只在最新 96 条里绕圈，撑不起无限滚动。
 */
export function homeLatestFeedSource(pageSize: number): InfiniteFeedSource {
  const source = listingFeedSource({
    q: "",
    tag: "",
    sort: "latest",
    pageSize,
  });
  return { ...source, key: `home:latest:${pageSize}` };
}
