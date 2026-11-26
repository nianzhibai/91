import { APIResponseError } from "../http.js";
import type { Schema } from "../contract.js";
import type { Theme } from "../types.js";
import { ADMIN_BASE, ConfigConflictError, type AdminContext } from "./context.js";
import type { MaintenanceJobStatus } from "./drives.js";

const LOG_REQUEST_TIMEOUT_MS = 15000;

export type UpdateCheck = Schema<"updateCheckDTO">;

export type AdminLogSource = "application" | "http";
export type AdminLogLevel = "info" | "warning" | "error";
export type AdminLogMethod = "GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "OPTIONS" | "HEAD";

export type AdminLogEntry = Omit<Schema<"applogEntry">, "source" | "level" | "method"> & {
  source: AdminLogSource;
  level: AdminLogLevel;
  method?: AdminLogMethod;
};

export type AdminLogSnapshot = Omit<Schema<"applogSnapshot">, "entries" | "hasMore" | "writeHealth"> & {
  entries: AdminLogEntry[];
  hasMore?: boolean;
  writeHealth?: Schema<"applogWriteHealth">;
};

export type AdminLogFilters = {
  source?: AdminLogSource;
  level?: AdminLogLevel;
  method?: AdminLogMethod;
  query?: string;
  limit?: number;
  cursor?: string;
  before?: number;
  from?: string;
  to?: string;
};

export type Settings = Omit<Schema<"settingsDTO">, "theme"> & {
  theme: Theme;
};

export type ConfigYAMLDocument = {
  content: string;
  version: string;
};

export type ConfigSaveResult = Schema<"configSaveResult">;

export type GPUStatus = Omit<Schema<"GPUStatus">, "backend"> & {
  backend?: "vaapi" | "cuda" | "rkmpp";
};

export type AdminUser = Schema<"userDTO">;
export type BannedIP = Schema<"bannedIPDTO">;

function etagVersion(value: string | null) {
  return (value ?? "").replace(/^W\//, "").replace(/^"|"$/g, "");
}

export function createAdminSystemClient({ http, request }: AdminContext) {
  function checkUpdate() {
    return request<UpdateCheck>("/update/check");
  }

  function logQueryParams(filters: AdminLogFilters = {}) {
    const params = new URLSearchParams();
    params.set("limit", String(filters.limit ?? 500));
    if (filters.cursor) params.set("cursor", filters.cursor);
    if (filters.source) params.set("source", filters.source);
    if (filters.level) params.set("level", filters.level);
    if (filters.method) params.set("method", filters.method);
    if (filters.query?.trim()) params.set("q", filters.query.trim());
    if (filters.before) params.set("before", String(filters.before));
    if (filters.from) params.set("from", filters.from);
    if (filters.to) params.set("to", filters.to);
    return params;
  }

  function listLogs(filters: AdminLogFilters = {}, signal?: AbortSignal) {
    return request<AdminLogSnapshot>(`/logs?${logQueryParams(filters)}`, {
      signal,
      timeoutMs: LOG_REQUEST_TIMEOUT_MS,
    });
  }

  async function downloadLogs(filters: AdminLogFilters = {}, signal?: AbortSignal) {
    const params = logQueryParams(filters);
    params.set("download", "1");
    params.delete("cursor");
    params.delete("before");
    const response = await http.send(`${ADMIN_BASE}/logs?${params}`, {
      signal,
      headers: { Accept: "*/*" },
    });
    return response.blob();
  }

  function clearLogs() {
    return request<{ success: boolean }>("/logs", { method: "DELETE" });
  }

  function getSettings() {
    return request<Settings>("/settings");
  }

  function updateSettings(body: Partial<Settings>) {
    return request<Settings>("/settings", {
      method: "PUT",
      body: JSON.stringify(body),
    });
  }

  const getGPUStatus = (signal?: AbortSignal) => request<GPUStatus>("/generation/gpu", { signal });

  const probeGPU = (signal?: AbortSignal) =>
    request<GPUStatus>("/generation/gpu/probe", { method: "POST", signal });

  async function getConfigYAML(): Promise<ConfigYAMLDocument> {
    const res = await http.send(`${ADMIN_BASE}/config.yaml`, {
      headers: { Accept: "application/yaml" },
    });
    return {
      content: await res.text(),
      version: etagVersion(res.headers.get("ETag")),
    };
  }

  async function updateConfigYAML(content: string, version: string): Promise<ConfigSaveResult> {
    const headers = new Headers({ "Content-Type": "application/yaml; charset=utf-8" });
    if (version) headers.set("If-Match", `"${version}"`);
    try {
      return await request<ConfigSaveResult>("/config.yaml", {
        method: "PUT",
        headers,
        body: content,
      });
    } catch (error) {
      if (error instanceof APIResponseError && error.status === 409) {
        throw new ConfigConflictError(error);
      }
      throw error;
    }
  }

  function getScanAllJobStatus(signal?: AbortSignal) {
    return request<MaintenanceJobStatus>("/jobs/scan-all/status", { signal });
  }

  function runScanAllJob() {
    return request<{
      ok: boolean;
      accepted: boolean;
      status: MaintenanceJobStatus;
      message?: string;
    }>("/jobs/scan-all/run", { method: "POST" });
  }

  function stopAllTasks() {
    return request<{ ok: boolean; stoppedDrives: number; status: MaintenanceJobStatus }>(
      "/tasks/stop",
      { method: "POST" }
    );
  }

  function listUsers(signal?: AbortSignal) {
    return request<AdminUser[]>("/users", { signal });
  }

  function createUser(body: { username: string; password: string; role: string }) {
    return request<{ ok: boolean; id: number }>("/users", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  function deleteUser(id: number) {
    return request<{ ok: boolean }>(`/users/${id}`, { method: "DELETE" });
  }

  function banUser(id: number) {
    return request<{ ok: boolean }>(`/users/${id}/ban`, { method: "POST" });
  }

  function unbanUser(id: number) {
    return request<{ ok: boolean }>(`/users/${id}/unban`, { method: "POST" });
  }

  function resetPassword(id: number, password: string) {
    return request<{ ok: boolean }>(`/users/${id}/password`, {
      method: "PUT",
      body: JSON.stringify({ password }),
    });
  }

  function listBannedIPs(signal?: AbortSignal) {
    return request<BannedIP[]>("/banned-ips", { signal });
  }

  function unbanIP(ip: string) {
    return request<{ ok: boolean }>(`/banned-ips/${encodeURIComponent(ip)}`, {
      method: "DELETE",
    });
  }

  return {
    checkUpdate,
    logQueryParams,
    listLogs,
    downloadLogs,
    clearLogs,
    getSettings,
    updateSettings,
    getGPUStatus,
    probeGPU,
    getConfigYAML,
    updateConfigYAML,
    getScanAllJobStatus,
    runScanAllJob,
    stopAllTasks,
    listUsers,
    createUser,
    deleteUser,
    banUser,
    unbanUser,
    resetPassword,
    listBannedIPs,
    unbanIP,
  };
}
