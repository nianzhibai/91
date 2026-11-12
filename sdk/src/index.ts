import { createTransport, type ClientOptions } from "./http.js";
import { createAuthClient } from "./auth.js";
import { createAdminClient } from "./admin.js";
import { createVideoClient } from "./videos.js";
import { createPlaybackClient } from "./playback.js";
import { createFrontendClient } from "./frontend.js";

export { APIResponseError, UnauthorizedError } from "./http.js";
export type { ClientOptions, RequestOptions } from "./http.js";
export * from "./types.js";
export * from "./auth.js";
export * from "./playback.js";
export * from "./frontend.js";
export * from "./admin.js";
export {
  VideoFeedExpiredError, VideoShareUnavailableError, ShortsFeedExpiredError,
} from "./videos.js";
export type {
  VideoFeedKind, VideoFeedCursor, VideoFeedResponse, VideoShareClaim,
  VideoReactionResult, ShortsItem, ShortsFeedItem, ShortsNextResponse,
} from "./videos.js";

export type ServerInfo = {
  name: string;
  version: string;
  apiVersion: string;
  auth: { type: "cookie"; login: string; session: string };
  endpoints: { openapi: string; media: string; admin: string };
};

export function createClient(options: ClientOptions = {}) {
  const http = createTransport(options);
  return {
    http,
    resourceURL: http.resourceURL,
    server: () => http.request<ServerInfo>("/api/server"),
    auth: createAuthClient(http),
    videos: createVideoClient(http),
    admin: createAdminClient(http),
    playback: createPlaybackClient(http),
    frontend: createFrontendClient(http),
    settings: {
      theme: () => http.request<{ theme: "dark" | "light" }>("/api/settings/theme"),
      preview: () => http.request<{ previewEnabled: boolean }>("/api/settings/preview"),
    },
  };
}
