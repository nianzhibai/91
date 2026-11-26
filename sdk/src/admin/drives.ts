import { APIResponseError } from "../http.js";
import type { Schema } from "../contract.js";
import { ADMIN_BASE, type AdminContext } from "./context.js";

export type DriveKind =
  | "quark"
  | "p115"
  | "p123"
  | "pikpak"
  | "wopan"
  | "yun139"
  | "guangyapan"
  | "onedrive"
  | "googledrive"
  | "webdav"
  | "localstorage";

export type ScanOutcome = "succeeded" | "partial" | "failed" | "canceled" | "skipped";
export type ScanIssue = Schema<"scanjobIssue">;

export type ScanResult = Omit<Schema<"scanjobResult">, "state" | "issues"> & {
  state: ScanOutcome;
  issues?: ScanIssue[];
};

export type MaintenanceJobStatus = Omit<
  Schema<"NightlyJobStatus">,
  "state" | "outcome" | "scanResults" | "issues"
> & {
  state: "idle" | "queued" | "running" | "running_queued";
  outcome?: ScanOutcome;
  scanResults?: ScanResult[];
  issues?: ScanIssue[];
};

export type DriveGenerationStatus = Omit<Schema<"GenerationStatus">, "result"> & {
  result?: ScanResult;
};

export type AdminDrive = Omit<
  Schema<"driveDTO">,
  | "kind"
  | "skipDirIds"
  | "scanGenerationStatus"
  | "thumbnailGenerationStatus"
  | "previewGenerationStatus"
  | "fingerprintGenerationStatus"
  | "maintenanceStatus"
> & {
  kind: DriveKind;
  skipDirIds: string[];
  scanGenerationStatus?: DriveGenerationStatus;
  thumbnailGenerationStatus?: DriveGenerationStatus;
  previewGenerationStatus?: DriveGenerationStatus;
  fingerprintGenerationStatus?: DriveGenerationStatus;
  maintenanceStatus?: MaintenanceJobStatus;
};

export type DriveStorageUsage = Schema<"storageusageDriveUsage">;

export type AdminDriveStorage = Omit<Schema<"storageusageUsage">, "drives"> & {
  drives: Record<string, DriveStorageUsage>;
};

export type DriveRuntime = Pick<
  AdminDrive,
  | "scanGenerationStatus"
  | "thumbnailGenerationStatus"
  | "previewGenerationStatus"
  | "fingerprintGenerationStatus"
> & { maintenanceStatus?: MaintenanceJobStatus };

export type DriveStats = Pick<
  AdminDrive,
  | "thumbnailReadyCount"
  | "thumbnailPendingCount"
  | "thumbnailFailedCount"
  | "thumbnailDurationPendingCount"
  | "teaserReadyCount"
  | "teaserPendingCount"
  | "teaserFailedCount"
  | "fingerprintReadyCount"
  | "fingerprintPendingCount"
  | "fingerprintFailedCount"
>;

export type DriveConfig = Omit<AdminDrive, keyof DriveRuntime | keyof DriveStats>;

export type DriveResourceData = {
  config: DriveConfig;
  runtime: DriveRuntime;
  stats: DriveStats;
  storage: DriveStorageUsage;
};

export type DriveResource = keyof DriveResourceData;

export type DriveSnapshot<R extends DriveResource = DriveResource> = Omit<
  Schema<"driveviewSnapshot">,
  "resource" | "data"
> & {
  resource: R;
  data?: DriveResourceData[R];
};

export type UpsertDriveInput = {
  id: string;
  kind: DriveKind;
  name: string;
  rootId: string;
  credentials: Record<string, string>;
  skipDirIds?: string[];
};

export type DriveConfigSaveResult = {
  snapshot?: DriveSnapshot<"config">;
  ok: boolean;
  deferred?: boolean;
  message?: string;
  warning?: string;
};

export type DeleteDriveInput = {
  deleteVideos: true;
};

export type CrawlerTaskResult = Omit<Schema<"crawljobResult">, "state"> & {
  state: "queued" | "running" | "completed" | "partial" | "failed" | "canceled" | "interrupted";
};

export type CrawlerUploadResult = Omit<Schema<"uploadjobResult">, "state" | "issues"> & {
  state: "queued" | "running" | "succeeded" | "partial" | "blocked" | "failed" | "canceled" | "interrupted";
  issues?: Schema<"uploadjobIssue">[];
};

export type CrawlerFeed = Schema<"scriptcrawlerFeed">;

export type AdminCrawler = Omit<
  Schema<"crawlerDTO">,
  | "kind"
  | "feeds"
  | "scanGenerationStatus"
  | "thumbnailGenerationStatus"
  | "previewGenerationStatus"
  | "fingerprintGenerationStatus"
  | "uploadGenerationStatus"
  | "lastUploadResult"
  | "currentTask"
  | "lastCrawlResult"
> & {
  kind: "scriptcrawler";
  feeds: CrawlerFeed[];
  scanGenerationStatus?: DriveGenerationStatus;
  thumbnailGenerationStatus?: DriveGenerationStatus;
  previewGenerationStatus?: DriveGenerationStatus;
  fingerprintGenerationStatus?: DriveGenerationStatus;
  uploadGenerationStatus?: DriveGenerationStatus;
  lastUploadResult?: CrawlerUploadResult;
  currentTask?: CrawlerTaskResult;
  lastCrawlResult?: CrawlerTaskResult;
};

export type UpsertCrawlerInput = Omit<Schema<"upsertCrawlerReq">, "selectedFeedId" | "scriptPath"> & {
  selectedFeedId: string;
  scriptPath: string;
};

export type ImportCrawlerScriptResult = {
  scriptPath: string;
  name: string;
  feeds: CrawlerFeed[];
  sourceUrl?: string;
};

export type CrawlerDryRunItem = Schema<"scriptcrawlerDryRunItem">;
export type CrawlerDryRunMediaCheck = Schema<"scriptcrawlerDryRunMediaCheck">;

export type CrawlerDryRunResult = Omit<
  Schema<"scriptcrawlerDryRunResult">,
  "validated" | "items" | "log"
> & {
  validated: Array<"protocol" | "media_probe">;
  items: CrawlerDryRunItem[];
  log?: string[];
};

/** Drive kinds that support QR login. */
export type DriveLoginKind = Schema<"startDriveLoginRequest">["kind"];
export type DriveLoginSession = Schema<"DriveLoginSession">;
/** `waiting` and `scanned` continue; every other state ends the login. */
export type DriveLoginState = DriveLoginResult["state"];
export type DriveLoginResult = Schema<"DriveLoginResult">;
export type DriveDirEntry = Schema<"DriveDirEntry">;

export type DriveGenerationResult = Omit<Schema<"DriveGenerationResult">, "state"> & {
  state: "started" | "busy" | "ready";
};

export function createAdminDrivesClient({ http, request }: AdminContext) {
  function listDrives(signal?: AbortSignal) {
    return request<AdminDrive[]>("/drives", { signal });
  }

  function getDriveCredentials(id: string) {
    return request<{ credentials: Record<string, string> }>(
      `/drives/${encodeURIComponent(id)}/credentials`
    );
  }

  function getDriveStorage(signal?: AbortSignal) {
    return request<AdminDriveStorage>("/drives/storage", { signal });
  }

  function isDriveErrorSnapshot<R extends DriveResource>(
    body: unknown,
    id: string,
    resource: R,
    status: number
  ): body is DriveSnapshot<R> {
    if (!body || typeof body !== "object") return false;
    const snapshot = body as Partial<DriveSnapshot>;
    return (
      snapshot.driveId === id &&
      snapshot.resource === resource &&
      snapshot.status === status &&
      typeof snapshot.epoch === "string" &&
      snapshot.epoch.length > 0 &&
      typeof snapshot.revision === "number" &&
      Number.isSafeInteger(snapshot.revision) &&
      snapshot.revision > 0 &&
      typeof snapshot.updatedAt === "string" &&
      typeof snapshot.error === "string" &&
      snapshot.error.length > 0
    );
  }

  async function getDriveSnapshot<R extends DriveResource>(
    id: string,
    resource: R,
    signal?: AbortSignal
  ) {
    try {
      return await request<DriveSnapshot<R>>(
        `/drives/${encodeURIComponent(id)}/${resource}?refresh=true`,
        { signal }
      );
    } catch (error) {
      if (
        error instanceof APIResponseError &&
        error.status !== 403 &&
        isDriveErrorSnapshot(error.body, id, resource, error.status)
      ) {
        return error.body;
      }
      throw error;
    }
  }

  function subscribeDriveSnapshots(
    id: string,
    onSnapshot: (snapshot: DriveSnapshot) => void,
    onOpen: () => void,
    onError: () => void
  ) {
    const source = new EventSource(
      http.apiURL(`${ADMIN_BASE}/drives/${encodeURIComponent(id)}/events`),
      { withCredentials: true }
    );
    source.onopen = onOpen;
    source.onerror = onError;
    source.addEventListener("heartbeat", onOpen);
    source.addEventListener("snapshot", (event) => {
      try {
        onSnapshot(JSON.parse((event as MessageEvent<string>).data) as DriveSnapshot);
      } catch {
        onError();
      }
    });
    return source;
  }

  function upsertDrive(body: UpsertDriveInput) {
    return request<DriveConfigSaveResult>("/drives", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  function deleteDrive(id: string, body: DeleteDriveInput) {
    return request<{ ok: boolean; deletedVideos: number }>(`/drives/${encodeURIComponent(id)}`, {
      method: "DELETE",
      body: JSON.stringify(body),
    });
  }

  function rescan(id: string) {
    return request<{
      ok: boolean;
      accepted: boolean;
      message?: string;
      status?: MaintenanceJobStatus;
      snapshot?: DriveSnapshot<"runtime">;
    }>(`/drives/${encodeURIComponent(id)}/rescan`, { method: "POST" });
  }

  function stopDriveTasks(id: string) {
    return request<{ ok: boolean; stopped: boolean; snapshot?: DriveSnapshot<"runtime"> }>(
      `/drives/${encodeURIComponent(id)}/tasks/stop`,
      { method: "POST" }
    );
  }

  function listCrawlers(signal?: AbortSignal) {
    return request<AdminCrawler[]>("/crawlers", { signal });
  }

  function upsertCrawler(body: UpsertCrawlerInput) {
    return request<DriveConfigSaveResult & { id: string }>("/crawlers", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  function importCrawlerScriptFile(file: File) {
    const form = new FormData();
    form.append("file", file);
    return request<ImportCrawlerScriptResult>("/crawlers/import-file", {
      method: "POST",
      body: form,
    });
  }

  function importCrawlerScriptURL(url: string) {
    return request<ImportCrawlerScriptResult>("/crawlers/import-url", {
      method: "POST",
      body: JSON.stringify({ url }),
    });
  }

  function testCrawlerScript(body: { scriptPath: string; proxy?: string; selectedFeedId: string }) {
    return request<CrawlerDryRunResult>("/crawlers/test-script", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  function runCrawler(id: string) {
    return request<{
      ok: boolean;
      accepted: boolean;
      message?: string;
      status?: MaintenanceJobStatus;
    }>(`/crawlers/${encodeURIComponent(id)}/run`, { method: "POST" });
  }

  function uploadCrawlerVideos(id: string) {
    return request<{
      ok: boolean;
      accepted: boolean;
      message?: string;
      status?: MaintenanceJobStatus;
    }>(`/crawlers/${encodeURIComponent(id)}/upload`, { method: "POST" });
  }

  function stopCrawlerTasks(id: string) {
    return request<{ ok: boolean; stopped: boolean }>(
      `/crawlers/${encodeURIComponent(id)}/tasks/stop`,
      { method: "POST" }
    );
  }

  function listCrawlerTasks(id: string, signal?: AbortSignal) {
    return request<CrawlerTaskResult[]>(`/crawlers/${encodeURIComponent(id)}/tasks`, { signal });
  }

  function getCrawlerTask(id: string, taskId: string, signal?: AbortSignal) {
    return request<{ task: CrawlerTaskResult; uploads: CrawlerUploadResult[] }>(
      `/crawlers/${encodeURIComponent(id)}/tasks/${encodeURIComponent(taskId)}`,
      { signal }
    );
  }

  function cancelCrawlerTask(id: string, taskId: string) {
    return request<{ ok: boolean; taskId: string }>(
      `/crawlers/${encodeURIComponent(id)}/tasks/${encodeURIComponent(taskId)}/cancel`,
      { method: "POST" }
    );
  }

  function setCrawlerPaused(id: string, paused: boolean) {
    return request<{ ok: boolean; paused: boolean }>(
      `/crawlers/${encodeURIComponent(id)}/paused`,
      {
        method: "POST",
        body: JSON.stringify({ paused }),
      }
    );
  }

  function deleteCrawler(id: string) {
    return request<{
      ok: boolean;
      deletedVideos: number;
      deletedScript?: boolean;
      warning?: string;
    }>(`/crawlers/${encodeURIComponent(id)}`, { method: "DELETE" });
  }

  function startDriveLogin(kind: DriveLoginKind, signal?: AbortSignal) {
    return request<DriveLoginSession>("/drive-logins", {
      method: "POST",
      body: JSON.stringify({ kind }),
      signal,
    });
  }

  /**
   * Checks the provider once. A final state is returned again to repeated polls
   * for one minute, so a lost response can be recovered; call `cancelDriveLogin`
   * once you have it. Afterwards polling returns 404.
   */
  function pollDriveLogin(id: string, signal?: AbortSignal) {
    return request<DriveLoginResult>(`/drive-logins/${encodeURIComponent(id)}`, { signal });
  }

  /** Forgets the login and any credentials the server still holds for it. */
  function cancelDriveLogin(id: string) {
    return request<void>(`/drive-logins/${encodeURIComponent(id)}`, { method: "DELETE" });
  }

  function listDriveDirChildren(id: string, parentId?: string, signal?: AbortSignal) {
    const qs = parentId ? `?parent=${encodeURIComponent(parentId)}` : "";
    return request<DriveDirEntry[]>(`/drives/${encodeURIComponent(id)}/dirtree${qs}`, { signal });
  }

  function setDriveSkipDirIds(id: string, dirIds: string[]) {
    return request<DriveConfigSaveResult & { skipDirIds: string[] }>(
      `/drives/${encodeURIComponent(id)}/skip-dirs`,
      {
        method: "POST",
        body: JSON.stringify({ dirIds }),
      }
    );
  }

  /**
   * 设置某盘同时生成资源的视频数（0-5）。0 表示按网盘类型：本地来源只受全局
   * 名额限制，云盘一次一个视频。立即生效，全局名额仍是上限。
   */
  function setDriveGenerationParallelism(id: string, parallelism: number) {
    return request<DriveConfigSaveResult & { generationParallelism: number }>(
      `/drives/${encodeURIComponent(id)}/generation-parallelism`,
      {
        method: "POST",
        body: JSON.stringify({ parallelism }),
      }
    );
  }

  function generateDrivePreviews(id: string) {
    return request<DriveGenerationResult>(`/drives/${encodeURIComponent(id)}/previews/generate`, {
      method: "POST",
    });
  }

  function generateDriveThumbnails(id: string) {
    return request<DriveGenerationResult>(
      `/drives/${encodeURIComponent(id)}/thumbnails/generate`,
      { method: "POST" }
    );
  }

  function generateDriveFingerprints(id: string) {
    return request<DriveGenerationResult>(
      `/drives/${encodeURIComponent(id)}/fingerprints/generate`,
      { method: "POST" }
    );
  }

  return {
    listDrives,
    getDriveCredentials,
    getDriveStorage,
    getDriveSnapshot,
    subscribeDriveSnapshots,
    upsertDrive,
    deleteDrive,
    rescan,
    stopDriveTasks,
    listCrawlers,
    upsertCrawler,
    importCrawlerScriptFile,
    importCrawlerScriptURL,
    testCrawlerScript,
    runCrawler,
    uploadCrawlerVideos,
    stopCrawlerTasks,
    listCrawlerTasks,
    getCrawlerTask,
    cancelCrawlerTask,
    setCrawlerPaused,
    deleteCrawler,
    startDriveLogin,
    pollDriveLogin,
    cancelDriveLogin,
    listDriveDirChildren,
    setDriveSkipDirIds,
    setDriveGenerationParallelism,
    generateDrivePreviews,
    generateDriveThumbnails,
    generateDriveFingerprints,
  };
}
