import type { Schema } from "../contract.js";
import type { TagItem, VideoItem } from "../types.js";
import { ADMIN_BASE, type AdminContext } from "./context.js";

export type UploadVideoInput = {
  file: File;
  title: string;
  tags: string[];
};

export type RemoteUploadJob = Schema<"RemoteUploadJobDTO">;
export type RemoteUploadState = RemoteUploadJob["state"];

export type CreateRemoteUploadInput = {
  url: string;
  title: string;
  tags: string[];
};

export type AdminVideo = Schema<"adminVideoDTO">;

export type AdminVideoList = Omit<Schema<"AdminVideoList">, "items"> & {
  items: AdminVideo[];
};

export type AdminVideoListParams = {
  driveId?: string;
  crawlerId?: string;
  sourceKind?: "" | "telegram";
  createdFrom?: string;
  createdTo?: string;
  durationMinMinutes?: string;
  durationMaxMinutes?: string;
  page?: number;
  size?: number;
  keyword?: string;
};

export type VideoStats = {
  current: number;
  blacklisted: number;
  merged: number;
};

export type AdminDeletedVideo = Omit<Schema<"catalogDeletedVideo">, "restorePolicy"> & {
  // direct：本地上传这类无法被扫盘/爬取重新发现的来源，取消拉黑时当场重建记录。
  restorePolicy: "none" | "scan" | "crawler" | "direct";
};

export type AdminBlacklistList = Omit<Schema<"BlacklistList">, "items"> & {
  items: AdminDeletedVideo[];
};

export type BlacklistSourceDeleteStatus = Omit<
  Schema<"BlacklistSourceDeleteStatus">,
  "state"
> & {
  state: "idle" | "running" | "completed" | "failed" | "canceled";
};

export type UpdateVideoInput = Schema<"updateVideoReq">;
export type VideoDeletionPreview = Schema<"VideoDeletionPreview">;
export type DeleteVideoInput = Schema<"deleteVideoReq">;
export type DeleteVideoResult = Schema<"DeleteVideoResult">;

export type VideoDeletionErrorCode = "deletion_scope_changed" | "conflict";

export type MergedVideo = Schema<"MergedVideo">;
export type MergedVideoList = Schema<"MergedVideoList">;
export type MergedSourceDeleteResult = Schema<"MergedSourceDeleteResult">;
export type MergedSourceDeleteErrorCode =
  | "source_revision_changed"
  | "source_shared"
  | "source_replaced"
  | "source_unverified"
  | "source_removal_in_progress";

export type MergedVideoListParams = {
  canonicalVideoId?: string;
  driveId?: string;
  sourceState?: MergedVideo["sourceState"];
  page?: number;
  size?: number;
};

export type DuplicateReview = Schema<"DuplicateReview">;
export type DuplicateReviewEvidence = Schema<"DuplicateReviewEvidence">;
export type DuplicateReviewList = Schema<"DuplicateReviewList">;
export type DuplicateReviewListParams = {
  page?: number;
  size?: number;
};

export function createAdminVideosClient({ http, request }: AdminContext) {
  function listVideos(params: AdminVideoListParams = {}, signal?: AbortSignal) {
    const qs = new URLSearchParams();
    if (params.driveId) qs.set("driveId", params.driveId);
    if (params.crawlerId) qs.set("crawlerId", params.crawlerId);
    if (params.sourceKind) qs.set("sourceKind", params.sourceKind);
    if (params.createdFrom) qs.set("createdFrom", params.createdFrom);
    if (params.createdTo) qs.set("createdTo", params.createdTo);
    if (params.durationMinMinutes) qs.set("durationMinMinutes", params.durationMinMinutes);
    if (params.durationMaxMinutes) qs.set("durationMaxMinutes", params.durationMaxMinutes);
    if (params.page) qs.set("page", String(params.page));
    if (params.size) qs.set("size", String(params.size));
    if (params.keyword) qs.set("keyword", params.keyword);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return request<AdminVideoList>(`/videos${suffix}`, { signal });
  }

  function getVideoStats() {
    return request<VideoStats>("/videos/stats");
  }

  function listBlacklist(
    params: { driveId?: string; page?: number; size?: number; keyword?: string } = {},
    signal?: AbortSignal
  ) {
    const qs = new URLSearchParams();
    if (params.driveId) qs.set("driveId", params.driveId);
    if (params.page) qs.set("page", String(params.page));
    if (params.size) qs.set("size", String(params.size));
    if (params.keyword) qs.set("keyword", params.keyword);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return request<AdminBlacklistList>(`/blacklist${suffix}`, { signal });
  }

  function removeBlacklist(id: string) {
    return request<{ ok: boolean }>(`/blacklist/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function getBlacklistSourceDeleteStatus(signal?: AbortSignal) {
    return request<BlacklistSourceDeleteStatus>("/blacklist/source-delete/status", { signal });
  }

  function startBlacklistSourceDelete(
    options: { deleteAllSources?: boolean; ids?: string[] } = { deleteAllSources: true }
  ) {
    const ids = Array.from(new Set((options.ids ?? []).map((id) => id.trim()).filter(Boolean)));
    const body =
      options.ids !== undefined ? { ids } : { deleteAllSources: options.deleteAllSources ?? true };
    return request<{
      ok: boolean;
      accepted: boolean;
      message?: string;
      status: BlacklistSourceDeleteStatus;
    }>("/blacklist/source-delete", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  function updateVideo(id: string, body: UpdateVideoInput) {
    return request<AdminVideo>(`/videos/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  }

  function previewVideoDeletion(id: string) {
    return request<VideoDeletionPreview>(`/videos/${encodeURIComponent(id)}/deletion-preview`);
  }

  async function deleteVideo(id: string, options: DeleteVideoInput = {}) {
    const result = await request<DeleteVideoResult>(`/videos/${encodeURIComponent(id)}`, {
      method: "DELETE",
      body: JSON.stringify({ ...options, deleteSource: !!options.deleteSource }),
    });
    if (!result?.ok) throw new Error("Video deletion failed");
    return result;
  }

  function regenPreview(id: string) {
    return request<{ ok: boolean }>(`/videos/${encodeURIComponent(id)}/regen-preview`, {
      method: "POST",
    });
  }

  function regenAllPreviews() {
    return request<Record<string, unknown>>("/videos/regen-preview", { method: "POST" });
  }

  function listMergedVideos(params: MergedVideoListParams = {}, signal?: AbortSignal) {
    const qs = new URLSearchParams();
    if (params.canonicalVideoId) qs.set("canonicalVideoId", params.canonicalVideoId);
    if (params.driveId) qs.set("driveId", params.driveId);
    if (params.sourceState) qs.set("sourceState", params.sourceState);
    if (params.page) qs.set("page", String(params.page));
    if (params.size) qs.set("size", String(params.size));
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return request<MergedVideoList>(`/merged-videos${suffix}`, { signal });
  }

  function getMergedVideo(id: string, signal?: AbortSignal) {
    return request<MergedVideo>(`/merged-videos/${encodeURIComponent(id)}`, { signal });
  }

  function deleteMergedVideoSource(id: string, sourceRevision: string) {
    const body: Schema<"MergedSourceDeleteRequest"> = { sourceRevision };
    return request<MergedSourceDeleteResult>(`/merged-videos/${encodeURIComponent(id)}/source`, {
      method: "DELETE",
      body: JSON.stringify(body),
    });
  }

  function listDuplicateReviews(params: DuplicateReviewListParams = {}, signal?: AbortSignal) {
    const qs = new URLSearchParams();
    if (params.page) qs.set("page", String(params.page));
    if (params.size) qs.set("size", String(params.size));
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return request<DuplicateReviewList>(`/duplicate-reviews${suffix}`, { signal });
  }

  function mergeDuplicateReview(keepVideoId: string, mergeVideoId: string) {
    const body: Schema<"DuplicateReviewMergeRequest"> = { keepVideoId, mergeVideoId };
    return request<Schema<"DuplicateReviewMergeRequest">>("/duplicate-reviews/merge", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  function dismissDuplicateReview(leftVideoId: string, rightVideoId: string) {
    const body: Schema<"DuplicateReviewDismissRequest"> = { leftVideoId, rightVideoId };
    return request<Schema<"DuplicateReviewDismissRequest">>("/duplicate-reviews/dismiss", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  /** Replace a video's manual tags; returns the updated public video item. */
  function updateVideoTags(id: string, tags: string[]) {
    return request<VideoItem>(`/videos/${encodeURIComponent(id)}/tags`, {
      method: "PUT",
      body: JSON.stringify({ tags }),
    });
  }

  /** Blacklist a video: remove it from the library while keeping the source file. */
  function hideVideo(id: string) {
    return request<{ ok: boolean }>(`/videos/${encodeURIComponent(id)}/hide`, { method: "POST" });
  }

  /** Tags that can be assigned while uploading. */
  async function listUploadTags(signal?: AbortSignal): Promise<TagItem[]> {
    const tags = await request<TagItem[]>("/upload/tags", { signal });
    if (!Array.isArray(tags)) throw new Error("Invalid /admin/api/upload/tags response");
    return tags;
  }

  /** Upload a local file. Use `uploadURL` with XMLHttpRequest when progress is needed. */
  function uploadVideo(input: UploadVideoInput) {
    const body = new FormData();
    body.append("file", input.file);
    if (input.title.trim()) body.append("title", input.title.trim());
    for (const tag of input.tags) body.append("tags", tag);
    return request<VideoItem>("/upload", { method: "POST", body });
  }

  const uploadURL = () => http.apiURL(`${ADMIN_BASE}/upload`);

  function createRemoteUpload(input: CreateRemoteUploadInput) {
    return request<RemoteUploadJob>("/upload/remote", {
      method: "POST",
      body: JSON.stringify({
        url: input.url.trim(),
        title: input.title.trim(),
        tags: input.tags,
      }),
    });
  }

  function listRemoteUploads(limit = 20, signal?: AbortSignal) {
    return request<RemoteUploadJob[]>(`/upload/remote?limit=${encodeURIComponent(String(limit))}`, {
      signal,
    });
  }

  function cancelRemoteUpload(id: string) {
    return request<RemoteUploadJob>(`/upload/remote/${encodeURIComponent(id)}/cancel`, {
      method: "POST",
    });
  }

  return {
    listVideos,
    getVideoStats,
    listBlacklist,
    removeBlacklist,
    getBlacklistSourceDeleteStatus,
    startBlacklistSourceDelete,
    updateVideo,
    previewVideoDeletion,
    deleteVideo,
    regenPreview,
    regenAllPreviews,
    listMergedVideos,
    getMergedVideo,
    deleteMergedVideoSource,
    listDuplicateReviews,
    mergeDuplicateReview,
    dismissDuplicateReview,
    updateVideoTags,
    hideVideo,
    listUploadTags,
    uploadVideo,
    uploadURL,
    createRemoteUpload,
    listRemoteUploads,
    cancelRemoteUpload,
  };
}
