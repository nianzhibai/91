import type { Transport } from "./http.js";
import { ConfigConflictError, createAdminContext } from "./admin/context.js";
import { createAdminBackupsClient } from "./admin/backups.js";
import { createAdminDrivesClient } from "./admin/drives.js";
import { createAdminSystemClient } from "./admin/system.js";
import { createAdminTagsClient } from "./admin/tags.js";
import { createAdminTelegramClient } from "./admin/telegram.js";
import { createAdminVideosClient } from "./admin/videos.js";

export { ConfigConflictError };
export * from "./admin/backups.js";
export * from "./admin/drives.js";
export * from "./admin/system.js";
export * from "./admin/tags.js";
export * from "./admin/telegram.js";
export * from "./admin/videos.js";

export function createAdminClient(http: Transport) {
  const ctx = createAdminContext(http);
  return {
    ...createAdminSystemClient(ctx),
    ...createAdminBackupsClient(ctx),
    ...createAdminDrivesClient(ctx),
    ...createAdminVideosClient(ctx),
    ...createAdminTagsClient(ctx),
    ...createAdminTelegramClient(ctx),
  };
}
