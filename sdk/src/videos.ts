import { APIResponseError, responseError, type Transport } from "./http.js";
import type { VideoCollection, VideoCollectionSummary, VideoDetail, VideoItem, VideoSubtitle, VideoReaction, VideoReactionCounts, ShortsFeedMode } from "./types.js";

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

// 真实后端接口调用。未配置网盘时，各接口返回空数据。

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

export type UploadVideoInput = {
  file: File;
  title: string;
  tags: string[];
};

export type RemoteUploadState =
  | "queued"
  | "downloading"
  | "validating"
  | "saving"
  | "completed"
  | "failed"
  | "canceled";

export type RemoteUploadJob = {
  id: string;
  state: RemoteUploadState;
  sourceLabel: string;
  title?: string;
  tags: string[];
  bytesDownloaded: number;
  totalBytes: number;
  canCancel: boolean;
  cancelRequested?: boolean;
  error?: string;
  completedVideoId?: string;
  videoHref?: string;
  createdAt: string;
  startedAt?: string;
  updatedAt: string;
  finishedAt?: string;
};

export type CreateRemoteUploadInput = {
  url: string;
  title: string;
  tags: string[];
};

export type TagItem = { id: string; label: string; count?: number };

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

  async function fetchHomeVideos(count?: number): Promise<VideoItem[]> {
    // 整库随机轮次由服务端按登录会话维护；前端只需告知本次展示数量。
    const path = count === undefined ? "/api/home" : `/api/home?count=${count}`;
    const items = await apiGet<VideoItem[]>(path);
    if (!Array.isArray(items)) {
      throw new Error("Invalid /api/home response");
    }
    return items;
  }

  async function fetchLatestHomeVideos(count: number): Promise<VideoItem[]> {
    const items = await apiGet<VideoItem[]>(`/api/home/latest?count=${count}`);
    if (!Array.isArray(items)) {
      throw new Error("Invalid /api/home/latest response");
    }
    return items;
  }

  async function fetchListing(
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
    const result = await apiGet<{ items: VideoItem[]; total: number }>(
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

  async function fetchVideoFeed(
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
      result = await apiGet<VideoFeedResponse>(`/api/feed?${params.toString()}`, options);
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

  function fetchVideoDetail(id: string): Promise<VideoDetail | null> {
    return apiGet<VideoDetail>(`/api/video/${encodeURIComponent(id)}`)
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

  async function fetchVideoRecommendations(
    id: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<VideoItem[]> {
    const items = await apiGet<VideoItem[]>(
      `/api/video/${encodeURIComponent(id)}/recommendations`,
      options
    );
    if (!Array.isArray(items)) {
      throw new Error("Invalid video recommendations response");
    }
    return items;
  }

  async function fetchVideoCollection(
    id: string,
    options: { signal?: AbortSignal; includePreview?: boolean } = {}
  ): Promise<VideoCollection> {
    const previewQuery = options.includePreview ? "?preview=1" : "";
    const collection = await apiGet<VideoCollection>(
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

  async function fetchVideoCollectionSummary(
    id: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<VideoCollectionSummary | null> {
    const summary = await apiGet<VideoCollectionSummary>(
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

  function fetchVideoSubtitles(id: string): Promise<VideoSubtitle[]> {
    return apiGet<VideoSubtitle[]>(`/api/video/${encodeURIComponent(id)}/subtitles`);
  }

  function createVideoShare(id: string): Promise<{ url: string; token: string }> {
    return apiJSON<{ url: string; token: string }>(
      `/api/video/${encodeURIComponent(id)}/share`,
      { method: "POST" }
    );
  }

  // React StrictMode 会在开发环境重复运行 effect。共享同一个领取 Promise，
  // 避免两个并发 POST 用不同 cookie 抢占同一条一次性链接。

  const pendingVideoShareClaims = new Map<string, Promise<VideoShareClaim>>();

  function consumeVideoShare(token: string): Promise<VideoShareClaim> {
    const existing = pendingVideoShareClaims.get(token);
    if (existing) return existing;

    const request = http.fetch("/api/share/consume", {
      method: "POST",
      credentials: "include",
      cache: "no-store",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ token }),
    })
      .then(async (res) => {
        if (res.status === 404 || res.status === 410) {
          throw new VideoShareUnavailableError(res.status);
        }
        if (!res.ok) throw await responseError(res);
        const result = (await res.json()) as VideoShareClaim;
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

  function fetchSharedVideoSubtitles(
    shareId: string
  ): Promise<VideoSubtitle[]> {
    return apiGet<VideoSubtitle[]>(
      `/api/share/${encodeURIComponent(shareId)}/subtitles`
    );
  }

  function recordSharedVideoView(
    shareId: string
  ): Promise<{ views: number }> {
    return apiJSON<{ views: number }>(
      `/api/share/${encodeURIComponent(shareId)}/view`,
      { method: "POST" }
    );
  }

  function updateVideoTags(
    id: string,
    tags: string[]
  ): Promise<VideoItem> {
    return apiJSON<VideoItem>(`/api/video/${encodeURIComponent(id)}/tags`, {
      method: "PUT",
      body: JSON.stringify({ tags }),
    });
  }

  function hideVideo(id: string): Promise<{ ok: boolean }> {
    return apiJSON<{ ok: boolean }>(
      `/api/video/${encodeURIComponent(id)}/hide`,
      { method: "POST" }
    );
  }

  async function deleteVideo(
    id: string,
    options: { deleteSource?: boolean } = {}
  ): Promise<{ ok: boolean; deletedSource: boolean }> {
    const result = await apiJSON<{ ok: boolean; deletedSource: boolean }>(
      `/admin/api/videos/${encodeURIComponent(id)}`,
      {
        method: "DELETE",
        body: JSON.stringify({ deleteSource: !!options.deleteSource }),
      }
    );
    if (!result.ok) throw new Error("Video deletion failed");
    return result;
  }

  function recordView(id: string): Promise<{ views: number }> {
    return apiJSON<{ views: number }>(
      `/api/video/${encodeURIComponent(id)}/view`,
      { method: "POST" }
    );
  }

  /** Legacy counter-style like endpoint used by the immersive shorts UI. */

  async function setVideoLike(id: string, liked: boolean): Promise<number> {
    const result = await apiJSON<{ likes: number }>(
      `/api/video/${encodeURIComponent(id)}/like`,
      { method: liked ? "POST" : "DELETE" }
    );
    if (!result || !Number.isInteger(result.likes) || result.likes < 0) {
      throw new Error("Invalid video like response");
    }
    return result.likes;
  }

  async function setVideoVisitReaction(
    id: string,
    visitId: string,
    reaction: VideoReaction
  ): Promise<VideoReactionResult> {
    const result = await apiJSON<VideoReactionResult>(
      `/api/video/${encodeURIComponent(id)}/reaction`,
      {
        method: "PUT",
        body: JSON.stringify({ visitId, reaction }),
      }
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

  function uploadVideo(input: UploadVideoInput): Promise<VideoItem> {
    const body = new FormData();
    body.append("file", input.file);
    if (input.title.trim()) {
      body.append("title", input.title.trim());
    }
    for (const tag of input.tags) {
      body.append("tags", tag);
    }
    return apiForm<VideoItem>("/api/upload", body);
  }

  function createRemoteUpload(
    input: CreateRemoteUploadInput
  ): Promise<RemoteUploadJob> {
    return apiJSON<RemoteUploadJob>("/api/upload/remote", {
      method: "POST",
      body: JSON.stringify({
        url: input.url.trim(),
        title: input.title.trim(),
        tags: input.tags,
      }),
    });
  }

  function fetchRemoteUploads(limit = 20): Promise<RemoteUploadJob[]> {
    return apiGet<RemoteUploadJob[]>(
      `/api/upload/remote?limit=${encodeURIComponent(String(limit))}`
    );
  }

  function cancelRemoteUpload(id: string): Promise<RemoteUploadJob> {
    return apiJSON<RemoteUploadJob>(
      `/api/upload/remote/${encodeURIComponent(id)}/cancel`,
      { method: "POST" }
    );
  }

  function fetchTags(): Promise<TagItem[]> { return apiGet<TagItem[]>("/api/tags"); }

  // 上传选项由后台标签目录实时生成，不复用首页标签云的会话缓存，确保管理端
  // 新增或删除标签后再次进入上传页即可看到最新结果。

  async function fetchUploadTags(): Promise<TagItem[]> {
    const tags = await apiGet<TagItem[]>("/api/upload/tags");
    if (!Array.isArray(tags)) {
      throw new Error("Invalid /api/upload/tags response");
    }
    return tags;
  }

  /** 短视频模式单条记录。比 VideoItem 多 videoSrc / poster。 */

  async function fetchShortsNext(
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
      result = await apiGet<ShortsNextResponse>(
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

  const API_GET_MAX_ATTEMPTS = 2;

  const API_GET_RETRY_DELAY_MS = 200;

  const API_GET_TIMEOUT_MS = 10_000;

  function isRetryableGetError(error: unknown): boolean {
    if (!(error instanceof APIResponseError)) return true;
    return error.status === 408 || error.status === 425 || error.status === 429 || error.status >= 500;
  }

  function abortReason(signal: AbortSignal): unknown {
    return signal.reason ?? new DOMException("The operation was aborted", "AbortError");
  }

  function wait(ms: number, signal?: AbortSignal): Promise<void> {
    if (!signal) {
      return new Promise((resolve) => globalThis.setTimeout(resolve, ms));
    }
    if (signal.aborted) return Promise.reject(abortReason(signal));

    return new Promise((resolve, reject) => {
      const timeoutID = globalThis.setTimeout(() => {
        signal.removeEventListener("abort", handleAbort);
        resolve();
      }, ms);
      const handleAbort = () => {
        globalThis.clearTimeout(timeoutID);
        reject(abortReason(signal));
      };
      signal.addEventListener("abort", handleAbort, { once: true });
    });
  }

  async function apiGet<T>(
    path: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<T> {
    let lastError: unknown;

    for (let attempt = 1; attempt <= API_GET_MAX_ATTEMPTS; attempt += 1) {
      if (options.signal?.aborted) throw abortReason(options.signal);

      const controller = new AbortController();
      const handleExternalAbort = () => {
        controller.abort(abortReason(options.signal!));
      };
      options.signal?.addEventListener("abort", handleExternalAbort, { once: true });
      if (options.signal?.aborted) handleExternalAbort();
      const timeoutID = globalThis.setTimeout(
        () => controller.abort(new DOMException("API request timed out", "TimeoutError")),
        API_GET_TIMEOUT_MS
      );
      try {
        const res = await http.fetch(path, {
          credentials: "include",
          cache: "no-store",
          headers: { Accept: "application/json" },
          signal: controller.signal,
        });
        if (!res.ok) throw await responseError(res);
        return (await res.json()) as T;
      } catch (error) {
        if (options.signal?.aborted) throw abortReason(options.signal);
        lastError = error;
        if (attempt >= API_GET_MAX_ATTEMPTS || !isRetryableGetError(error)) {
          throw error;
        }
      } finally {
        globalThis.clearTimeout(timeoutID);
        options.signal?.removeEventListener("abort", handleExternalAbort);
      }

      await wait(API_GET_RETRY_DELAY_MS, options.signal);
    }

    throw lastError instanceof Error ? lastError : new Error("API request failed");
  }

  async function apiJSON<T>(path: string, init: RequestInit): Promise<T> {
    const res = await http.fetch(path, {
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      ...init,
    });
    if (!res.ok) throw await responseError(res);
    return res.json();
  }

  async function apiForm<T>(path: string, body: FormData): Promise<T> {
    const res = await http.fetch(path, {
      method: "POST",
      credentials: "include",
      body,
    });
    if (!res.ok) throw await responseError(res);
    return res.json();
  }

  return {
    fetchHomeVideos,
    fetchLatestHomeVideos,
    fetchListing,
    fetchVideoFeed,
    fetchVideoDetail,
    fetchVideoRecommendations,
    fetchVideoCollection,
    fetchVideoCollectionSummary,
    fetchVideoSubtitles,
    createVideoShare,
    consumeVideoShare,
    fetchSharedVideoSubtitles,
    recordSharedVideoView,
    updateVideoTags,
    hideVideo,
    deleteVideo,
    recordView,
    setVideoLike,
    setVideoVisitReaction,
    uploadVideo,
    createRemoteUpload,
    fetchRemoteUploads,
    cancelRemoteUpload,
    fetchTags,
    fetchUploadTags,
    fetchShortsNext
  };
}
