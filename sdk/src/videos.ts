import { APIResponseError, type Transport } from "./http.js";
import type { TagItem, VideoCollection, VideoCollectionSummary, VideoDetail, VideoItem, VideoSubtitle, VideoReaction, VideoReactionCounts, ShortsFeedMode } from "./types.js";

export type VideoShareClaim = {
  shareId: string;
  expiresAt: string;
  video: VideoDetail;
};

export class VideoShareUnavailableError extends Error {
  constructor(readonly status: number) {
    super("Video share unavailable");
    this.name = "VideoShareUnavailableError";
  }
}

export type VideoFeedKind = "listing" | "recommend" | "latest";

export type VideoFeedCursor = {
  feedToken: string;
  position: number;
};

export type VideoFeedResponse = {
  items: VideoItem[];
  total: number;
  feedToken: string;
  nextCursor: number;
  exhausted: boolean;
};

export class VideoFeedExpiredError extends Error {
  constructor() {
    super("Video feed expired");
    this.name = "VideoFeedExpiredError";
  }
}

/**
 * Idempotent snapshot feed. A first response with more data creates an ordered
 * server-side snapshot; later requests address it with a token and cursor.
 * A result completed by the first response intentionally has no token.
 */

export type VideoReactionResult = VideoReactionCounts & {
  reaction: VideoReaction;
};


export type ShortsItem = VideoItem & {
  videoSrc: string;
  playbackUrl?: string;
  poster: string;
  /** Tiny server-preblurred texture for the viewport-sized letterbox backdrop. */
  backgroundPoster?: string;
  /**
   * 文件大小与时长，用来算平均码率，进而把预加载门槛从固定秒数换算成
   * 一份固定的字节预算。元数据缺失时后端会省略这两个字段（omitempty），
   * 消费方必须按"未知码率"兜底回原有的固定门槛。
   */
  sizeBytes?: number;
  durationSeconds?: number;
};

export type ShortsFeedItem = ShortsItem & {
  /** Resume position immediately after this item in the server-side feed. */
  feedCursor: number;
};

/** 短视频"取下一批"接口的响应。 */

export type ShortsNextResponse = {
  items: ShortsFeedItem[];
  total: number;
  feedToken: string;
  nextCursor: number;
  /** true 表示当前服务端随机轮次已经读到末尾。 */
  roundComplete: boolean;
};

export class ShortsFeedExpiredError extends Error {
  constructor() {
    super("Shorts feed expired");
    this.name = "ShortsFeedExpiredError";
  }
}

/**
 * 拉取服务端随机 feed 的下一批候选。请求只携带固定大小的令牌和游标，
 * 不会再随已看视频数量增长，也没有请求体。
 */

export function createVideoClient(http: Transport) {

  async function listHome(count?: number): Promise<VideoItem[]> {
    // 整库随机轮次由服务端按登录会话维护；前端只需告知本次展示数量。
    const path = count === undefined ? "/api/home" : `/api/home?count=${count}`;
    const items = await read<VideoItem[]>(path);
    if (!Array.isArray(items)) {
      throw new Error("Invalid /api/home response");
    }
    return items;
  }

  async function listLatest(count: number): Promise<VideoItem[]> {
    const items = await read<VideoItem[]>(`/api/home/latest?count=${count}`);
    if (!Array.isArray(items)) {
      throw new Error("Invalid /api/home/latest response");
    }
    return items;
  }

  async function list(
    page: number,
    pageSize: number,
    params?: { q?: string; tag?: string; sort?: string; includeTotal?: boolean },
    options: { signal?: AbortSignal } = {}
  ): Promise<{ items: VideoItem[]; total: number }> {
    const qs = new URLSearchParams({
      page: String(page),
      size: String(pageSize),
    });
    if (params?.q) qs.set("q", params.q);
    if (params?.tag) qs.set("tag", params.tag);
    if (params?.sort) qs.set("sort", params.sort);
    if (params?.includeTotal === false) qs.set("count", "false");
    const result = await read<{ items: VideoItem[]; total: number }>(
      `/api/list?${qs.toString()}`,
      options
    );
    if (
      !result ||
      !Array.isArray(result.items) ||
      typeof result.total !== "number"
    ) {
      throw new Error("Invalid /api/list response");
    }
    return result;
  }

  async function getFeed(
    input: {
      kind: VideoFeedKind;
      cursor: VideoFeedCursor;
      count: number;
      q?: string;
      tag?: string;
      sort?: string;
    },
    options: { signal?: AbortSignal } = {}
  ): Promise<VideoFeedResponse> {
    const params = new URLSearchParams({
      kind: input.kind,
      cursor: String(input.cursor.position),
      count: String(input.count),
    });
    if (input.cursor.feedToken) params.set("feedToken", input.cursor.feedToken);
    if (input.q?.trim()) params.set("q", input.q.trim());
    if (input.tag?.trim()) params.set("tag", input.tag.trim());
    if (input.sort) params.set("sort", input.sort);

    let result: VideoFeedResponse;
    try {
      result = await read<VideoFeedResponse>(`/api/feed?${params.toString()}`, options);
    } catch (error) {
      if (error instanceof APIResponseError && error.status === 410) {
        throw new VideoFeedExpiredError();
      }
      throw error;
    }

    if (
      !result ||
      !Array.isArray(result.items) ||
      !Number.isInteger(result.total) ||
      result.total < 0 ||
      typeof result.feedToken !== "string" ||
      result.feedToken.length > 128 ||
      (input.cursor.feedToken.length > 0 &&
        result.feedToken !== input.cursor.feedToken) ||
      !Number.isInteger(result.nextCursor) ||
      result.nextCursor < input.cursor.position ||
      result.nextCursor > result.total ||
      typeof result.exhausted !== "boolean" ||
      (!result.exhausted && result.feedToken.length === 0) ||
      (!result.exhausted && result.nextCursor <= input.cursor.position) ||
      result.exhausted !== (result.nextCursor >= result.total)
    ) {
      throw new Error("Invalid /api/feed response");
    }
    return result;
  }

  /** Resolves to `null` when the video does not exist or has been removed. */
  function get(id: string): Promise<VideoDetail | null> {
    return read<VideoDetail>(`/api/video/${encodeURIComponent(id)}`)
      .catch((error: unknown) => {
        if (
          error instanceof APIResponseError &&
          (error.status === 404 || error.status === 410)
        ) {
          return null;
        }
        throw error;
      });
  }

  async function listRecommendations(
    id: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<VideoItem[]> {
    const items = await read<VideoItem[]>(
      `/api/video/${encodeURIComponent(id)}/recommendations`,
      options
    );
    if (!Array.isArray(items)) {
      throw new Error("Invalid video recommendations response");
    }
    return items;
  }

  async function getCollection(
    id: string,
    options: { signal?: AbortSignal; includePreview?: boolean } = {}
  ): Promise<VideoCollection> {
    const previewQuery = options.includePreview ? "?preview=1" : "";
    const collection = await read<VideoCollection>(
      `/api/video/${encodeURIComponent(id)}/collection${previewQuery}`,
      options
    );
    if (
      !collection ||
      typeof collection.name !== "string" ||
      !Number.isInteger(collection.total) ||
      collection.total < 0 ||
      !Number.isInteger(collection.currentIndex) ||
      collection.currentIndex < 0 ||
      !Array.isArray(collection.items) ||
      collection.total !== collection.items.length ||
      collection.items.some(
        (item) =>
          !item ||
          typeof item.id !== "string" ||
          typeof item.href !== "string" ||
          typeof item.title !== "string" ||
          typeof item.thumbnail !== "string" ||
          typeof item.duration !== "string" ||
          (item.previewSrc !== undefined &&
            typeof item.previewSrc !== "string") ||
          (options.includePreview && typeof item.previewSrc !== "string") ||
          !Number.isInteger(item.views) ||
          item.views < 0 ||
          typeof item.publishedAt !== "string"
      ) ||
      (collection.total > 0 &&
        (collection.currentIndex < 1 ||
          collection.currentIndex > collection.total))
    ) {
      throw new Error("Invalid video collection response");
    }
    return collection;
  }

  async function getCollectionSummary(
    id: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<VideoCollectionSummary | null> {
    const summary = await read<VideoCollectionSummary>(
      `/api/video/${encodeURIComponent(id)}/collection/summary`,
      options
    );
    if (
      !summary ||
      typeof summary.name !== "string" ||
      !Number.isInteger(summary.total) ||
      summary.total < 0 ||
      !Number.isInteger(summary.currentIndex) ||
      summary.currentIndex < 0 ||
      (summary.total === 0 && summary.currentIndex !== 0) ||
      (summary.total > 0 &&
        (summary.currentIndex < 1 || summary.currentIndex > summary.total))
    ) {
      throw new Error("Invalid video collection summary response");
    }
    return summary.total > 1 ? summary : null;
  }

  function listSubtitles(id: string): Promise<VideoSubtitle[]> {
    return read<VideoSubtitle[]>(`/api/video/${encodeURIComponent(id)}/subtitles`);
  }

  function createShare(id: string): Promise<{ url: string; token: string }> {
    return post<{ url: string; token: string }>(`/api/video/${encodeURIComponent(id)}/share`);
  }

  // React StrictMode 会在开发环境重复运行 effect。共享同一个领取 Promise，
  // 避免两个并发 POST 用不同 cookie 抢占同一条一次性链接。

  const pendingVideoShareClaims = new Map<string, Promise<VideoShareClaim>>();

  function claimShare(token: string): Promise<VideoShareClaim> {
    const existing = pendingVideoShareClaims.get(token);
    if (existing) return existing;

    const request = post<VideoShareClaim>("/api/share/consume", { token })
      .catch((error: unknown) => {
        if (error instanceof APIResponseError && (error.status === 404 || error.status === 410)) {
          throw new VideoShareUnavailableError(error.status);
        }
        throw error;
      })
      .then((result) => {
        if (
          !result ||
          typeof result.shareId !== "string" ||
          !result.shareId ||
          typeof result.expiresAt !== "string" ||
          !result.video ||
          typeof result.video.videoSrc !== "string"
        ) {
          throw new Error("Invalid video share response");
        }
        return result;
      })
      .finally(() => {
        if (pendingVideoShareClaims.get(token) === request) {
          pendingVideoShareClaims.delete(token);
        }
      });

    pendingVideoShareClaims.set(token, request);
    return request;
  }

  function listSharedSubtitles(
    shareId: string
  ): Promise<VideoSubtitle[]> {
    return read<VideoSubtitle[]>(
      `/api/share/${encodeURIComponent(shareId)}/subtitles`
    );
  }

  function recordSharedView(
    shareId: string
  ): Promise<{ views: number }> {
    return post<{ views: number }>(`/api/share/${encodeURIComponent(shareId)}/view`);
  }

  function recordView(id: string): Promise<{ views: number }> {
    return post<{ views: number }>(`/api/video/${encodeURIComponent(id)}/view`);
  }

  /** Legacy counter-style like endpoint used by the immersive shorts UI. */

  async function setLike(id: string, liked: boolean): Promise<number> {
    const result = await post<{ likes: number }>(
      `/api/video/${encodeURIComponent(id)}/like`,
      undefined,
      liked ? "POST" : "DELETE"
    );
    if (!result || !Number.isInteger(result.likes) || result.likes < 0) {
      throw new Error("Invalid video like response");
    }
    return result.likes;
  }

  async function setReaction(
    id: string,
    visitId: string,
    reaction: VideoReaction
  ): Promise<VideoReactionResult> {
    const result = await post<VideoReactionResult>(
      `/api/video/${encodeURIComponent(id)}/reaction`,
      { visitId, reaction },
      "PUT"
    );
    if (
      !result ||
      (result.reaction !== "none" &&
        result.reaction !== "like" &&
        result.reaction !== "dislike") ||
      !Number.isInteger(result.likes) ||
      result.likes < 0 ||
      !Number.isInteger(result.dislikes) ||
      result.dislikes < 0
    ) {
      throw new Error("Invalid video reaction response");
    }
    return result;
  }

  function listTags(): Promise<TagItem[]> { return read<TagItem[]>("/api/tags"); }

  /** 短视频模式单条记录。比 VideoItem 多 videoSrc / poster。 */

  async function getShorts(
    feedToken: string,
    cursor: number,
    count: number,
    mode: ShortsFeedMode = "recommend",
    options: { signal?: AbortSignal } = {}
  ): Promise<ShortsNextResponse> {
    const params = new URLSearchParams({
      cursor: String(cursor),
      count: String(count),
      mode,
    });
    if (feedToken) params.set("feedToken", feedToken);

    let result: ShortsNextResponse;
    try {
      result = await read<ShortsNextResponse>(
        `/api/shorts/next?${params.toString()}`,
        options
      );
    } catch (error) {
      if (error instanceof APIResponseError && error.status === 410) {
        throw new ShortsFeedExpiredError();
      }
      throw error;
    }

    if (
      !result ||
      !Array.isArray(result.items) ||
      !Number.isInteger(result.total) ||
      result.total < 0 ||
      typeof result.feedToken !== "string" ||
      (result.total > 0 && result.feedToken.length === 0) ||
      !Number.isInteger(result.nextCursor) ||
      result.nextCursor < 0 ||
      typeof result.roundComplete !== "boolean" ||
      result.items.some(
        (item) =>
          !Number.isInteger(item.feedCursor) ||
          item.feedCursor < 1 ||
          item.feedCursor > result.nextCursor
      )
    ) {
      throw new Error("Invalid /api/shorts/next response");
    }
    return result;
  }

  // Reads are idempotent: retry once on transient failures and bound their wait.
  function read<T>(path: string, options: { signal?: AbortSignal } = {}): Promise<T> {
    return http.request<T>(path, { signal: options.signal, retry: true, timeoutMs: 10_000 });
  }

  function post<T>(path: string, body?: unknown, method = "POST"): Promise<T> {
    return http.request<T>(path, { method, body: body === undefined ? undefined : JSON.stringify(body) });
  }

  return {
    listHome,
    listLatest,
    list,
    getFeed,
    get,
    listRecommendations,
    getCollection,
    getCollectionSummary,
    listSubtitles,
    createShare,
    claimShare,
    listSharedSubtitles,
    recordSharedView,
    recordView,
    setLike,
    setReaction,
    listTags,
    getShorts
  };
}
