import type { Schema } from "../contract.js";
import type { AdminContext } from "./context.js";

export type TelegramConfig = {
  enabled: boolean;
  apiId: number;
  storageDirectory: string;
  allowedUserIds: number[];
  siteBaseUrl: string;
  maxFileSizeBytes: number;
  downloadThreads: number;
  maxPendingJobs: number;
  fetchTimeoutSeconds: number;
  uploadDriveId: string;
  uploadDirectory: string;
};

export type TelegramStatus = {
  enabled: boolean;
  connection: {
    enabled: boolean;
    state: string;
    botId: string;
    username: string;
    error: string;
    lastConnected: string;
    lastMessage: string;
    cacheAvailableBytes: number;
    notificationFailures: number;
    config: TelegramConfig;
  };
};

export type ImportJob = Schema<"ImportJobDTO">;

export function createAdminTelegramClient({ request }: AdminContext) {
  const getTelegramAvailability = (signal?: AbortSignal) =>
    request<{ enabled: boolean }>("/telegram/availability", { signal });

  const getTelegramStatus = (signal?: AbortSignal) =>
    request<TelegramStatus>("/telegram/status", { signal });

  const testTelegram = () => request<{ username: string }>("/telegram/test", { method: "POST" });

  const resumeTelegram = () => request<void>("/telegram/resume", { method: "POST" });

  const listTelegramImports = (limit: number, signal?: AbortSignal) =>
    request<ImportJob[]>(
      `/import-jobs?${new URLSearchParams({ source: "telegram", limit: String(limit) })}`,
      { signal }
    );

  const cancelImport = (id: string) =>
    request<ImportJob>(`/import-jobs/${encodeURIComponent(id)}/cancel`, { method: "POST" });

  const retryImport = (id: string) =>
    request<void>(`/import-jobs/${encodeURIComponent(id)}/retry`, { method: "POST" });

  return {
    getTelegramAvailability,
    getTelegramStatus,
    testTelegram,
    resumeTelegram,
    listTelegramImports,
    cancelImport,
    retryImport,
  };
}
