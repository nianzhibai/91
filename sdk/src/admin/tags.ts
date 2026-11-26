import type { Schema } from "../contract.js";
import type { AdminContext } from "./context.js";

export type TagMatchRules = {
  keywords?: string[];
  matchAvCode?: boolean;
  avCodePrefixes?: string[];
};

export type AdminTag = Omit<Schema<"catalogTag">, "matchRules"> & {
  matchRules?: TagMatchRules;
};

export type TagJobStatus = Schema<"TagJobStatus">;

export function createAdminTagsClient({ request }: AdminContext) {
  async function listTags(signal?: AbortSignal): Promise<AdminTag[]> {
    const tags = await request<AdminTag[] | null>("/tags", { signal });
    if (tags === null) return [];
    if (!Array.isArray(tags)) {
      throw new Error("Invalid /admin/api/tags response");
    }
    return tags;
  }

  function createTag(label: string) {
    return request<{ label: string; classified: number }>("/tags", {
      method: "POST",
      body: JSON.stringify({ label }),
    });
  }

  function updateTag(id: number, matchRules: TagMatchRules) {
    return request<{ tag: AdminTag }>(`/tags/${encodeURIComponent(String(id))}`, {
      method: "PUT",
      body: JSON.stringify({ matchRules }),
    });
  }

  function deleteTag(id: number) {
    return request<{ ok: boolean; removedVideos: number }>(
      `/tags/${encodeURIComponent(String(id))}`,
      { method: "DELETE" }
    );
  }

  function getTagJobStatus(signal?: AbortSignal) {
    return request<TagJobStatus>("/tags/jobs/status", { signal });
  }

  function retagVideos() {
    return request<Record<string, unknown>>("/tags/retag", { method: "POST" });
  }

  return {
    listTags,
    createTag,
    updateTag,
    deleteTag,
    getTagJobStatus,
    retagVideos,
  };
}
