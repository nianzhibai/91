import { isMap, isScalar, type Document } from "yaml";
import { parseTelegramUserIDs } from "../telegram/config";

export const TELEGRAM_DEFAULTS = {
  telegramEnabled: false,
  telegramApiId: 0,
  telegramApiHash: "",
  telegramBotToken: "",
  telegramConnectionProxy: "",
  telegramStorageDirectory: "",
  telegramAllowedUserIds: "",
  telegramSiteBaseUrl: "",
  telegramMaxFileSizeBytes: 4 * 1024 ** 3,
  telegramDownloadThreads: 6,
  telegramMaxPendingJobs: 100,
  telegramFetchTimeoutSeconds: 1800,
  telegramUploadDriveId: "",
  telegramUploadDirectory: "Telegram",
  telegramUploadProxy: "",
};

export type TelegramDraft = typeof TELEGRAM_DEFAULTS;
export type TelegramField = keyof TelegramDraft;

export const TELEGRAM_YAML_KEYS: Record<TelegramField, string> = {
  telegramEnabled: "enabled",
  telegramApiId: "api_id",
  telegramApiHash: "api_hash",
  telegramBotToken: "bot_token",
  telegramConnectionProxy: "connection_proxy",
  telegramStorageDirectory: "storage_directory",
  telegramAllowedUserIds: "allowed_user_ids",
  telegramSiteBaseUrl: "site_base_url",
  telegramMaxFileSizeBytes: "max_file_size_bytes",
  telegramDownloadThreads: "download_threads",
  telegramMaxPendingJobs: "max_pending_jobs",
  telegramFetchTimeoutSeconds: "fetch_timeout_seconds",
  telegramUploadDriveId: "upload_drive_id",
  telegramUploadDirectory: "upload_directory",
  telegramUploadProxy: "upload_proxy",
};
export const TELEGRAM_FIELDS = Object.keys(
  TELEGRAM_YAML_KEYS,
) as TelegramField[];

export function telegramDraftFromDocument(document: Document): TelegramDraft {
  const section = document.get("telegram", true);
  if (
    section != null &&
    !isMap(section) &&
    !(isScalar(section) && section.value === null)
  ) {
    throw new Error("telegram 必须是映射对象");
  }
  const draft = { ...TELEGRAM_DEFAULTS };
  for (const field of TELEGRAM_FIELDS) {
    const key = TELEGRAM_YAML_KEYS[field];
    const node = document.getIn(["telegram", key], true);
    if (node == null || (isScalar(node) && node.value === null)) continue;
    const value = document.toJS()?.telegram?.[key];
    if (field === "telegramAllowedUserIds") {
      if (
        !Array.isArray(value) ||
        value.some((id) => !Number.isSafeInteger(id) || id <= 0)
      ) {
        throw new Error("telegram.allowed_user_ids 必须是正整数 ID 列表");
      }
      draft[field] = [...new Set(value)].join(", ");
    } else if (typeof value !== typeof draft[field]) {
      throw new Error(`telegram.${key} 类型无效`);
    } else {
      Object.assign(draft, { [field]: value });
    }
  }
  return draft;
}

export function telegramFieldValue(draft: TelegramDraft, field: TelegramField) {
  return field === "telegramAllowedUserIds"
    ? parseTelegramUserIDs(draft[field])
    : draft[field];
}
