import type { Schema } from "./contract.js";

// Wire types come from the generated contract; field documentation lives in openapi.json.

export type VideoItem = Schema<"VideoDTO">;
export type AuthorProfile = Schema<"AuthorProfile">;
export type CommentItem = Schema<"Comment">;
export type VideoCollectionSummary = Schema<"VideoCollectionSummary">;
export type VideoCollectionItem = Schema<"VideoCollectionItemDTO">;
export type VideoCollection = Schema<"VideoCollectionDTO">;
export type VideoDetail = Schema<"VideoDetailDTO">;
export type VideoSubtitle = Schema<"SubtitleDTO">;
export type TagItem = Schema<"TagDTO">;

export type SortKey = "latest" | "hot" | "recent";
export type Theme = "dark" | "light";
export type VideoReaction = Schema<"Reaction">["reaction"];
export type VideoReactionCounts = Pick<Schema<"Reaction">, "likes" | "dislikes">;
export type ShortsFeedMode = "recommend" | "latest" | "hot";
