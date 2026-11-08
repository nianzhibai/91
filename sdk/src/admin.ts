import { APIResponseError, UnauthorizedError, type Transport } from "./http.js";
import type { Theme } from "./types.js";
export const ADMIN_LOG_REQUEST_TIMEOUT_MS = 15000;

export type UpdateCheck = {
  currentVersion: string;
  latestVersion: string;
  hasUpdate: boolean;
  releaseUrl?: string;
  releaseNotes?: string;
  checkedAt: string;
};

export type AdminLogSource = "application" | "http";

export type AdminLogLevel = "info" | "warning" | "error";

export type AdminLogMethod =
  | "GET"
  | "POST"
  | "PUT"
  | "PATCH"
  | "DELETE"
  | "OPTIONS"
  | "HEAD";

export type AdminLogEntry = {
  id: number;
  timestamp: string;
  source: AdminLogSource;
  level: AdminLogLevel;
  method?: AdminLogMethod;
  status?: number;
  path?: string;
  remote?: string;
  bytes?: number;
  elapsed?: string;
  requestId?: string;
  taskId?: string;
  component?: string;
  driveId?: string;
  videoId?: string;
  fileId?: string;
  stage?: string;
  error?: string;
  stack?: string;
  message: string;
};

export type AdminLogSnapshot = {
  entries: AdminLogEntry[];
  matched: number;
  storageBytes: number;
  maxStorageBytes: number;
  nextCursor?: string;
  reset?: boolean;
  hasMore?: boolean;
  writeHealth?: {
    lastError?: string;
    failedWrites: number;
    lastFailureAt?: string;
    lastSuccessAt?: string;
  };
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

export type BackupEstimate = {
  fileCount: number;
  totalBytes: number;
  availableBytes: number;
  requiredBytes: number;
};

export type BackupTask = {
  id: string;
  state: string;
  phase?: string;
  name?: string;
  startedAt: string;
  finishedAt?: string;
  fileCount: number;
  processedFiles: number;
  totalBytes: number;
  processedBytes: number;
  bytesPerSecond: number;
  error?: string;
  cancellable: boolean;
};

export type BackupRecord = {
  id: string;
  name: string;
  size: number;
  sha256?: string;
  createdAt: string;
  verificationStatus: "verified" | "unchecked" | "invalid" | string;
  verificationError?: string;
  imported: boolean;
  appVersion?: string;
  sourceDataRoot?: string;
  fileCount?: number;
  expandedSize?: number;
  included?: string[];
  selection?: BackupSelection;
};

export type BackupList = {
  backups: BackupRecord[];
  current?: BackupTask;
  restoreProgress?: BackupOperationProgress;
  estimate: BackupEstimate;
  restartManaged: boolean;
  pendingRestore: boolean;
};

export type BackupOperationProgress = {
  phase: string;
  processedBytes: number;
  totalBytes: number;
  processedFiles: number;
  totalFiles: number;
};

export type BackupUploadChunk = {
  index: number;
  size: number;
};

export type BackupUploadSession = {
  id: string;
  fileName: string;
  size: number;
  sha256?: string;
  chunkSize: number;
  totalChunks: number;
  received: BackupUploadChunk[];
  state: string;
  progress?: BackupOperationProgress;
  createdAt: string;
  expiresAt: string;
};

export type BackupManifest = {
  formatVersion: number;
  appVersion: string;
  createdAt: string;
  sourceDataRoot: string;
  fileCount: number;
  totalSize: number;
  included: string[];
  selection: BackupSelection;
};

export type BackupSelection = {
  cloudDrives: boolean;
  crawlerScripts: boolean;
  uploadStorage: boolean;
  localStorage: boolean;
  userInfo: boolean;
};

export type RestoreReport = {
  manifest: BackupManifest;
  verificationStatus: string;
  pathRewrites?: string[];
  localStorageWarnings?: string[];
  missingAssets?: string[];
  warnings?: string[];
};

export type BackupTransferState =
  | "queued"
  | "connecting"
  | "uploading"
  | "finalizing"
  | "retrying"
  | "completed"
  | "failed"
  | "canceled";

export type BackupTransferJob = {
  id: string;
  backupId: string;
  backupName: string;
  targetUrl: string;
  state: BackupTransferState | string;
  size: number;
  sha256: string;
  processedBytes: number;
  bytesPerSecond: number;
  totalRanges?: number;
  processedRanges?: number;
  attempts?: number;
  nextAttemptAt?: string;
  error?: string;
  targetBackupId?: string;
  targetBackupName?: string;
  createdAt: string;
  startedAt?: string;
  updatedAt: string;
  finishedAt?: string;
  cancellable: boolean;
  retryable: boolean;
};

export type BackupReceiveToken = {
  id: string;
  token: string;
  createdAt: string;
  expiresAt: string;
};

export type BackupReceiveTransfer = {
  id: string;
  sourceServerId: string;
  backupId: string;
  backupName: string;
  state: BackupTransferState | string;
  size: number;
  processedBytes: number;
  bytesPerSecond: number;
  createdAt: string;
  updatedAt: string;
  finishedAt?: string;
  targetBackupId?: string;
  error?: string;
  cancellable: boolean;
};

export type AdminDrive = {
  id: string;
  kind: "quark" | "p115" | "p123" | "pikpak" | "wopan" | "yun139" | "guangyapan" | "onedrive" | "googledrive" | "webdav" | "localstorage";
  name: string;
  rootId: string;
  status: string;
  lastError?: string;
  hasCredential: boolean;
  /** 后端能力表声明该挂载可写入文件；爬虫上传目标据此展示。 */
  canUpload: boolean;
  /**
   * 用户在 admin 配置的"扫描跳过目录"集合（drive 侧目录 fileID 列表）。
   * 命中其中任一目录时 scanner 直接跳过、不递归；空数组 = 不跳过任何目录。
   * 名单变化后的下一次扫盘会先从媒体库清理对应目录，不删除网盘源文件。
   * 替代旧版硬编码 p115 "影视" 目录例外分支。
   */
  skipDirIds: string[];
  /** Changes after the mounted runtime configuration has been applied. */
  directoryRevision: number;
  scanGenerationStatus?: DriveGenerationStatus;
  thumbnailGenerationStatus?: DriveGenerationStatus;
  previewGenerationStatus?: DriveGenerationStatus;
  fingerprintGenerationStatus?: DriveGenerationStatus;
  thumbnailReadyCount: number;
  thumbnailPendingCount: number;
  thumbnailFailedCount: number;
  thumbnailDurationPendingCount: number;
  teaserReadyCount: number;
  teaserPendingCount: number;
  teaserFailedCount: number;
  fingerprintReadyCount: number;
  fingerprintPendingCount: number;
  fingerprintFailedCount: number;
};

export type DriveGenerationStatus = {
  result?: ScanResult;
  state: string;
  currentTitle?: string;
  queueLength: number;
  cooldownUntil?: string;
  scannedCount: number;
  addedCount: number;
  doneCount: number;
  totalCount: number;
};

export type DriveStorageUsage = {
  thumbnailBytes: number;
  teaserBytes: number;
  totalBytes: number;
};

export type AdminDriveStorage = DriveStorageUsage & {
  availableBytes: number;
  capacityBytes: number;
  drives: Record<string, DriveStorageUsage>;
};

export type DriveRuntime = Pick<AdminDrive, "scanGenerationStatus" | "thumbnailGenerationStatus" | "previewGenerationStatus" | "fingerprintGenerationStatus"> & { maintenanceStatus?: MaintenanceJobStatus };

export type DriveStats = Pick<AdminDrive, "thumbnailReadyCount" | "thumbnailPendingCount" | "thumbnailFailedCount" | "thumbnailDurationPendingCount" | "teaserReadyCount" | "teaserPendingCount" | "teaserFailedCount" | "fingerprintReadyCount" | "fingerprintPendingCount" | "fingerprintFailedCount">;

export type DriveConfig = Omit<AdminDrive, keyof DriveRuntime | keyof DriveStats>;

export type DriveResourceData = { config: DriveConfig; runtime: DriveRuntime; stats: DriveStats; storage: DriveStorageUsage };

export type DriveResource = keyof DriveResourceData;

export type DriveSnapshot<R extends DriveResource = DriveResource> = {
  epoch: string;
  driveId: string;
  resource: R;
  revision: number;
  updatedAt: string;
  data?: DriveResourceData[R];
  error?: string;
  status?: number;
};

export type UpsertDriveInput = {
  id: string;
  kind: "quark" | "p115" | "p123" | "pikpak" | "wopan" | "yun139" | "guangyapan" | "onedrive" | "googledrive" | "webdav" | "localstorage";
  name: string;
  rootId: string;
  credentials: Record<string, string>;
  /**
   * 可选：写入"扫描跳过目录"集合。`undefined` 表示不变（沿用服务端旧值），
   * 空数组 `[]` 表示清空。常见保存路径走 setDriveSkipDirIds 专用接口；
   * 这里允许同时上传是为了批量编辑场景。
   */
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

export type CrawlerTaskResult = {
  taskId: string;
  feedId?: string;
  feedLabel?: string;
  stopRequested?: boolean;
  state: "queued" | "running" | "completed" | "partial" | "failed" | "canceled" | "interrupted";
  stage: string;
  checked: number;
  newVideos: number;
  message?: string;
};

export type CrawlerUploadResult = {
  taskId: string;
  driveId: string;
  targetDriveId: string;
  state: "queued" | "running" | "succeeded" | "partial" | "blocked" | "failed" | "canceled" | "interrupted";
  startedAt: string;
  finishedAt: string;
  candidateCount: number;
  uploadedCount: number;
  reusedCount: number;
  blockedCount: number;
  failedCount: number;
  remainingCount: number;
  issueCount: number;
  message?: string;
  issues?: Array<{ videoId?: string; title?: string; stage: string; reason: string; message: string }>;
};

export type CrawlerFeed = {
  id: string;
  label: string;
  default?: boolean;
};

export type AdminCrawler = {
  id: string;
  name: string;
  kind: "scriptcrawler";
  feeds: CrawlerFeed[];
  selectedFeedId: string;
  status: string;
  lastError?: string;
  scriptPath: string;
  scriptSourceUrl?: string;
  proxy?: string;
  uploadProxy?: string;
  targetNew?: string;
  uploadDriveId?: string;
  paused: boolean;
  lastCrawlAt?: number;
  scanGenerationStatus?: DriveGenerationStatus;
  thumbnailGenerationStatus?: DriveGenerationStatus;
  previewGenerationStatus?: DriveGenerationStatus;
  fingerprintGenerationStatus?: DriveGenerationStatus;
  uploadGenerationStatus?: DriveGenerationStatus;
  lastUploadResult?: CrawlerUploadResult;
  currentTask?: CrawlerTaskResult;
  lastCrawlResult?: CrawlerTaskResult;
  scriptError?: string;
  thumbnailReadyCount: number;
  thumbnailPendingCount: number;
  thumbnailFailedCount: number;
  teaserReadyCount: number;
  teaserPendingCount: number;
  teaserFailedCount: number;
  fingerprintReadyCount: number;
  fingerprintPendingCount: number;
  fingerprintFailedCount: number;
  totalCrawledCount: number;
  localVideoCount: number;
  migratedVideoCount: number;
};

export type UpsertCrawlerInput = {
  id?: string;
  selectedFeedId: string;
  scriptPath: string;
  scriptSourceUrl?: string;
  proxy?: string;
  uploadProxy?: string;
  targetNew?: string;
  uploadDriveId?: string;
};

export type ImportCrawlerScriptResult = {
  scriptPath: string;
  name: string;
  feeds: CrawlerFeed[];
  sourceUrl?: string;
};

export type CrawlerDryRunItem = {
  title: string;
  sourceId?: string;
  mediaUrl?: string;
  thumbnailUrl?: string;
  detailUrl?: string;
};

export type CrawlerDryRunMediaCheck = {
  ok: boolean;
  status?: number;
  contentType?: string;
  contentLengthBytes?: number;
  error?: string;
};

export type CrawlerDryRunResult = {
  ok: boolean;
  feedId?: string;
  feedLabel?: string;
  validated: Array<"protocol" | "media_probe">;
  items: CrawlerDryRunItem[];
  mediaCheck?: CrawlerDryRunMediaCheck;
  error?: string;
  log?: string[];
  durationMs: number;
};

export type QuarkQRSession = {
  token: string;
  qrCodeUrl: string;
  qrImageDataUrl: string;
  expiresAt: string;
};

export type QuarkQRStatus = {
  state: "waiting" | "success" | "expired" | "error";
  status: number;
  statusText: string;
  cookie?: string;
};

export type P115QRSession = {
  uid: string;
  time: number;
  sign: string;
  qrCodeUrl: string;
  qrImageDataUrl: string;
};

export type P115QRStatus = {
  state: "waiting" | "scanned" | "success" | "expired" | "canceled" | "error";
  status: number;
  statusText: string;
  cookie?: string;
};

export type P123QRSession = {
  loginUuid: string;
  uniID: string;
  qrCodeUrl: string;
  qrImageDataUrl: string;
  expiresAt?: string;
};

export type P123QRStatus = {
  loginStatus: number;
  statusText: string;
  scanPlatform?: number;
  platformText?: string;
  accessToken?: string;
};

export type Yun139QRSession = {
  sessionId: string;
  qrCodeUrl: string;
  qrImageDataUrl: string;
  expiresAt: string;
};

export type Yun139QRStatus = {
  state: "waiting" | "scanned" | "success" | "expired" | "canceled" | "error";
  statusText: string;
  authorization?: string;
};

export type WopanQRSession = {
  uuid: string;
  qrImageDataUrl: string;
  expiresAt?: string;
};

export type WopanQRStatus = {
  state: number;
  statusText: string;
  accessToken?: string;
  refreshToken?: string;
  familyID?: string;
};

export type GuangYaPanQRSession = {
  deviceCode: string;
  qrCodeUrl: string;
  qrImageDataUrl: string;
  intervalSeconds: number;
  expiresAt?: string;
};

export type GuangYaPanQRStatus = {
  state: "pending" | "success" | "expired" | "denied" | "error";
  statusText: string;
  intervalSeconds?: number;
  accessToken?: string;
  refreshToken?: string;
  tokenType?: string;
  expiresIn?: number;
};

export type DriveDirEntry = {
  id: string;
  name: string;
};

/**
 * 列指定 drive 在 parentId 目录下的直接子目录。
 * parentId 留空 → 走 drive 的 RootID。
 */

export type DriveGenerationResult = { state: "started" | "busy" | "ready"; message: string };

export type AdminVideo = {
  id: string;
  driveId: string;
  fileId: string;
  title: string;
  author: string;
  tags: string[];
  tagSources?: Record<string, string>;
  tagEvidence?: Record<string, string>;
  durationSeconds: number;
  size: number;
  ext: string;
  thumbnailUrl: string;
  previewStatus: string;
  views: number;
  favorites: number;
  comments: number;
  likes: number;
  badges: string[];
  publishedAt: string;
  updatedAt: string;
};

export type AdminVideoList = {
  items: AdminVideo[];
  total: number;
  page: number;
  size: number;
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
};

export type AdminDeletedVideo = {
  id: string;
  driveId: string;
  fileId: string;
  fileName: string;
  size: number;
  reason?: string;
  sourceDeleted: boolean;
  canonicalVideoId?: string;
  canonicalTitle?: string;
  // direct：本地上传这类无法被扫盘/爬取重新发现的来源，取消拉黑时当场重建记录。
  restorePolicy: "none" | "scan" | "crawler" | "direct";
  deletedAt: number;
};

export type AdminBlacklistList = {
  items: AdminDeletedVideo[];
  total: number;
  page: number;
  size: number;
};

export type BlacklistSourceDeleteStatus = {
  state: "idle" | "running" | "completed" | "failed" | "canceled";
  running: boolean;
  pending: number;
  total: number;
  processed: number;
  deleted: number;
  skipped: number;
  failed: number;
  currentFile?: string;
  lastError?: string;
  startedAt?: string;
  lastFinishedAt?: string;
};

export type UpdateVideoInput = Partial<{
  tags: string[];
  badges: string[];
  thumbnail: string;
  durationSeconds: number;
}>;

export type AdminTag = {
  id: number;
  label: string;
  matchRules?: {
    keywords?: string[];
    matchAvCode?: boolean;
    avCodePrefixes?: string[];
  };
  source: string;
  count: number;
  crawlerOwned?: boolean;
};

export type TagMatchRules = NonNullable<AdminTag["matchRules"]>;

export type Settings = {
  theme: Theme;
};

export type ConfigYAMLDocument = {
  content: string;
  version: string;
};

export type ConfigSaveResult = {
  version: string;
  restartRequired: boolean;
  settings: {
    useGPU: boolean;
    telegramEnabled: boolean;
    nightlyDisabled: boolean;
    nightlyStartTime: string;
    nightlyTimezone: string;
    previewEnabled: boolean;
    previewConcurrency: number;
    thumbnailConcurrency: number;
    fingerprintConcurrency: number;
  };
};

export type GPUStatus = {
  enabled: boolean;
  checked: boolean;
  available: boolean;
  backend?: "vaapi" | "cuda" | "rkmpp";
  device?: string;
  name?: string;
  reason?: string;
  fallbackCount: number;
};

export class ConfigConflictError extends Error {
  constructor(message = "config.yaml 已被其他操作修改") {
    super(message);
    this.name = "ConfigConflictError";
  }
}

export type ScanOutcome = "succeeded" | "partial" | "failed" | "canceled" | "skipped";

export type ScanIssue = { stage: string; message: string };

export type ScanResult = {
  driveId: string;
  state: ScanOutcome;
  startedAt: string;
  finishedAt: string;
  scannedCount: number;
  addedCount: number;
  updatedCount: number;
  duplicateCount: number;
  tombstonedCount: number;
  cleanedCount: number;
  errorCount: number;
  message?: string;
  issues?: ScanIssue[];
};

/**
 * 扫描所有已配置的真实网盘，等待新视频资产处理完成后执行全库视频去重。
 * 不触发脚本爬虫、爬虫上传或恢复，也不占用当天的定时 nightly 执行标记。
 * 任务已在跑或已排队时，后端会拒绝重复触发。
 */

export type MaintenanceJobStatus = {
  state: "idle" | "queued" | "running" | "running_queued";
  running: boolean;
  queued: boolean;
  startedAt?: string;
  lastFinishedAt?: string;
  outcome?: ScanOutcome;
  scanResults?: ScanResult[];
  issues?: ScanIssue[];
};

export type AdminUser = {
  id: number;
  username: string;
  role: string;
  banned: boolean;
  createdAt: number;
};

export type BannedIP = {
  ip: string;
  reason: string;
  createdAt: number;
};

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
    enabled: boolean; state: string; botId: string; username: string; error: string;
    lastConnected: string; lastMessage: string; cacheAvailableBytes: number;
    notificationFailures: number; config: TelegramConfig;
  };
};

export type ImportJob = {
  id: string; state: string; title?: string; sourceKind: string; stage: string;
  bytesDownloaded: number; totalBytes: number; error?: string;
  createdAt: string; videoHref?: string; canCancel: boolean; canRetry: boolean;
  cancelRequested?: boolean; sequence: string; senderId?: string;
  retryCount: number; nextAttempt: number;
};

export function createAdminClient(http: Transport) {
  const BASE = "/admin/api";
  const request = <T>(path: string, init?: RequestInit) => http.request<T>(BASE + path, init);
  function checkUpdate() {
    return request<UpdateCheck>("/update/check");
  }

  // ---------- Runtime logs ----------

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
    const params = logQueryParams(filters);
    const timeoutController = new AbortController();
    let timedOut = false;
    const abortFromCaller = () => timeoutController.abort();
    if (signal?.aborted) timeoutController.abort();
    else signal?.addEventListener("abort", abortFromCaller, { once: true });
    const timeout = globalThis.setTimeout(() => {
      timedOut = true;
      timeoutController.abort();
    }, ADMIN_LOG_REQUEST_TIMEOUT_MS);

    return request<AdminLogSnapshot>(`/logs?${params.toString()}`, {
      signal: timeoutController.signal,
    })
      .catch((error: unknown) => {
        if (timedOut) throw new Error("日志请求超时，请稍后重试");
        throw error;
      })
      .finally(() => {
        globalThis.clearTimeout(timeout);
        signal?.removeEventListener("abort", abortFromCaller);
      });
  }

  async function downloadLogs(filters: AdminLogFilters = {}, signal?: AbortSignal) {
    const params = logQueryParams(filters);
    params.set("download", "1");
    params.delete("cursor");
    params.delete("before");
    const response = await http.fetch(`${BASE}/logs?${params}`, { credentials: "include", signal });
    if (response.status === 401) throw new UnauthorizedError();
    if (!response.ok) throw new APIResponseError(response.status, "日志导出失败");
    return response.blob();
  }

  function clearLogs() {
    return request<{ success: boolean }>("/logs", { method: "DELETE" });
  }

  // ---------- Full backup / migration restore ----------

  function listBackups(signal?: AbortSignal) {
    return request<BackupList>("/backups", { signal });
  }

  function createBackup(selection?: BackupSelection) {
    return request<BackupTask>("/backups", {
      method: "POST",
      body: selection ? JSON.stringify(selection) : undefined,
    });
  }

  function cancelBackup() {
    return request<{ ok: boolean }>("/backups/current/cancel", { method: "POST" });
  }

  function deleteBackup(id: string) {
    return request<{ ok: boolean }>(`/backups/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function backupDownloadURL(id: string) {
    return http.apiURL(`${BASE}/backups/${encodeURIComponent(id)}/download`);
  }

  function beginBackupUpload(input: {
    fileName: string;
    size: number;
  }) {
    return request<BackupUploadSession>("/backup-uploads", {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  function getBackupUpload(id: string) {
    return request<BackupUploadSession>(`/backup-uploads/${encodeURIComponent(id)}`);
  }

  async function putBackupUploadChunk(
    id: string,
    index: number,
    chunk: Blob,
    signal?: AbortSignal
  ): Promise<BackupUploadSession> {
    const res = await http.fetch(
      `${BASE}/backup-uploads/${encodeURIComponent(id)}/chunks/${index}`,
      {
        method: "PUT",
        credentials: "include",
        headers: {
          "Content-Type": "application/octet-stream",
        },
        body: chunk,
        signal,
      }
    );
    if (res.status === 401) throw new UnauthorizedError();
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      let message = text;
      try {
        const parsed = JSON.parse(text) as { error?: unknown };
        if (typeof parsed.error === "string") message = parsed.error;
      } catch {
        // Keep plain text.
      }
      throw new Error(message || `HTTP ${res.status}`);
    }
    return (await res.json()) as BackupUploadSession;
  }

  function finalizeBackupUpload(id: string, sha256: string) {
    return request<BackupRecord>(
      `/backup-uploads/${encodeURIComponent(id)}/finalize`,
      { method: "POST", body: JSON.stringify({ sha256 }) }
    );
  }

  function cancelBackupUpload(id: string) {
    return request<{ ok: boolean }>(`/backup-uploads/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function restoreBackup(
    id: string,
    input: { confirmation: string }
  ) {
    return request<{
      ok: boolean;
      restarting: boolean;
      restartManaged: boolean;
      report: RestoreReport;
    }>(`/backups/${encodeURIComponent(id)}/restore`, {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  function listBackupTransfers(signal?: AbortSignal) {
    return request<BackupTransferJob[]>("/backup-transfers", { signal });
  }

  function listBackupReceiveTransfers(signal?: AbortSignal) {
    return request<BackupReceiveTransfer[]>("/backup-receives", { signal });
  }

  function cancelBackupReceiveTransfer(id: string) {
    return request<void>(`/backup-receives/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function createBackupTransfer(
    backupId: string,
    input: { targetUrl: string; receiveToken: string }
  ) {
    return request<BackupTransferJob>(
      `/backups/${encodeURIComponent(backupId)}/transfers`,
      { method: "POST", body: JSON.stringify(input) }
    );
  }

  function cancelBackupTransfer(id: string) {
    return request<BackupTransferJob>(`/backup-transfers/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function retryBackupTransfer(id: string) {
    return request<BackupTransferJob>(
      `/backup-transfers/${encodeURIComponent(id)}/retry`,
      { method: "POST" }
    );
  }

  function createBackupReceiveToken() {
    return request<BackupReceiveToken>("/backup-receive-tokens", { method: "POST" });
  }

  // ---------- Drives ----------

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

  function isDriveErrorSnapshot<R extends DriveResource>(body: unknown, id: string, resource: R, status: number): body is DriveSnapshot<R> {
    if (!body || typeof body !== "object") return false;
    const snapshot = body as Partial<DriveSnapshot>;
    return snapshot.driveId === id && snapshot.resource === resource && snapshot.status === status
      && typeof snapshot.epoch === "string" && snapshot.epoch.length > 0
      && typeof snapshot.revision === "number" && Number.isSafeInteger(snapshot.revision) && snapshot.revision > 0
      && typeof snapshot.updatedAt === "string" && typeof snapshot.error === "string" && snapshot.error.length > 0;
  }

  async function getDriveSnapshot<R extends DriveResource>(id: string, resource: R, signal?: AbortSignal) {
    try {
      return await request<DriveSnapshot<R>>(`/drives/${encodeURIComponent(id)}/${resource}?refresh=true`, { signal });
    } catch (error) {
      if (error instanceof APIResponseError && error.status !== 403 && isDriveErrorSnapshot(error.body, id, resource, error.status)) {
        return error.body;
      }
      throw error;
    }
  }

  function subscribeDriveSnapshots(id: string, onSnapshot: (snapshot: DriveSnapshot) => void, onOpen: () => void, onError: () => void) {
    const source = new EventSource(http.apiURL(`${BASE}/drives/${encodeURIComponent(id)}/events`), { withCredentials: true });
    source.onopen = onOpen;
    source.onerror = onError;
    source.addEventListener("heartbeat", onOpen);
    source.addEventListener("snapshot", (event) => {
      try { onSnapshot(JSON.parse((event as MessageEvent<string>).data) as DriveSnapshot); }
      catch { onError(); }
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
    return request<{ ok: boolean; accepted: boolean; message?: string; status?: MaintenanceJobStatus; snapshot?: DriveSnapshot<"runtime"> }>(
      `/drives/${encodeURIComponent(id)}/rescan`,
      { method: "POST" }
    );
  }

  function stopDriveTasks(id: string) {
    return request<{ ok: boolean; stopped: boolean; snapshot?: DriveSnapshot<"runtime"> }>(
      `/drives/${encodeURIComponent(id)}/tasks/stop`,
      { method: "POST" }
    );
  }

  // ---------- Crawlers ----------

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
    return request<{ ok: boolean; accepted: boolean; message?: string; status?: MaintenanceJobStatus }>(
      `/crawlers/${encodeURIComponent(id)}/run`,
      { method: "POST" }
    );
  }

  function uploadCrawlerVideos(id: string) {
    return request<{ ok: boolean; accepted: boolean; message?: string; status?: MaintenanceJobStatus }>(
      `/crawlers/${encodeURIComponent(id)}/upload`,
      { method: "POST" }
    );
  }

  function stopCrawlerTasks(id: string) {
    return request<{ ok: boolean; stopped: boolean }>(
      `/crawlers/${encodeURIComponent(id)}/tasks/stop`,
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
    return request<{ ok: boolean; deletedVideos: number; deletedScript?: boolean; warning?: string }>(`/crawlers/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function startQuarkQRLogin() {
    return request<QuarkQRSession>("/drives/quark/qr", { method: "POST" });
  }

  function getQuarkQRStatus(token: string) {
    return request<QuarkQRStatus>("/drives/quark/qr/status", {
      method: "POST",
      body: JSON.stringify({ token }),
    });
  }

  function startP115QRLogin(signal?: AbortSignal) {
    return request<P115QRSession>("/drives/p115/qr", { method: "POST", signal });
  }

  function getP115QRStatus(session: Pick<P115QRSession, "uid" | "time" | "sign">, signal?: AbortSignal) {
    return request<P115QRStatus>("/drives/p115/qr/status", {
      method: "POST",
      signal,
      body: JSON.stringify({
        uid: session.uid,
        time: session.time,
        sign: session.sign,
      }),
    });
  }

  function startP123QRLogin() {
    return request<P123QRSession>("/drives/p123/qr", { method: "POST" });
  }

  function getP123QRStatus(uniID: string, loginUuid: string) {
    const qs = new URLSearchParams({ loginUuid });
    return request<P123QRStatus>(
      `/drives/p123/qr/${encodeURIComponent(uniID)}?${qs.toString()}`
    );
  }

  function startYun139QRLogin(signal?: AbortSignal) {
    return request<Yun139QRSession>("/drives/yun139/qr", { method: "POST", signal });
  }

  function getYun139QRStatus(sessionId: string, signal?: AbortSignal) {
    return request<Yun139QRStatus>("/drives/yun139/qr/status", {
      method: "POST",
      signal,
      body: JSON.stringify({ sessionId }),
    });
  }

  function startWopanQRLogin() {
    return request<WopanQRSession>("/drives/wopan/qr", { method: "POST" });
  }

  function getWopanQRStatus(uuid: string) {
    return request<WopanQRStatus>(`/drives/wopan/qr/${encodeURIComponent(uuid)}`);
  }

  function startGuangYaPanQRLogin() {
    return request<GuangYaPanQRSession>("/drives/guangyapan/qr", { method: "POST" });
  }

  function getGuangYaPanQRStatus(deviceCode: string) {
    const qs = new URLSearchParams({ deviceCode });
    return request<GuangYaPanQRStatus>(`/drives/guangyapan/qr/status?${qs.toString()}`);
  }


  /**
   * dirtree 接口的一个目录条目。前端构建按需展开的树时用。
   *
   * 后端只返回直接子目录（不递归），文件忽略。前端每展开一层就调一次
   * listDriveDirChildren(parentId)。115 等慢盘按需展开比一次性铺开整棵树体感
   * 好得多，也避免触发风控。
   */

  function listDriveDirChildren(id: string, parentId?: string, signal?: AbortSignal) {
    const qs = parentId ? `?parent=${encodeURIComponent(parentId)}` : "";
    return request<DriveDirEntry[]>(
      `/drives/${encodeURIComponent(id)}/dirtree${qs}`,
      { signal }
    );
  }

  /**
   * 整体覆盖某盘的"扫描跳过目录"集合（drive 侧目录 fileID）。
   * 传空数组 = 清空跳过列表。不会立刻重扫或删除；下次扫描时执行策略清理。
   */

  function setDriveSkipDirIds(id: string, dirIds: string[]) {
    return request<DriveConfigSaveResult & { skipDirIds: string[] }>(
      `/drives/${encodeURIComponent(id)}/skip-dirs`,
      {
        method: "POST",
        body: JSON.stringify({ dirIds }),
      }
    );
  }

  function generateDrivePreviews(id: string) {
    return request<DriveGenerationResult>(
      `/drives/${encodeURIComponent(id)}/previews/generate`,
      { method: "POST" }
    );
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

  // ---------- Videos ----------

  function listVideos(
    params: AdminVideoListParams = {},
    signal?: AbortSignal,
  ) {
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

  // 后台视频管理两个标签页的计数。

  function getVideoStats() {
    return request<VideoStats>("/videos/stats");
  }

  // 黑名单（被拉黑/手动删除、扫盘不再入库的视频）。原始记录已删除，
  // 这里只保留重新发现所需信息和恢复策略；源文件删除成功后记录会被移除。

  function listBlacklist(
    params: { driveId?: string; page?: number; size?: number; keyword?: string } = {},
    signal?: AbortSignal,
  ) {
    const qs = new URLSearchParams();
    if (params.driveId) qs.set("driveId", params.driveId);
    if (params.page) qs.set("page", String(params.page));
    if (params.size) qs.set("size", String(params.size));
    if (params.keyword) qs.set("keyword", params.keyword);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return request<AdminBlacklistList>(`/blacklist${suffix}`, { signal });
  }

  // 允许视频在后续手动/定时任务中重新入库；此操作不会立即触发扫盘或爬取。

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
    const body = options.ids !== undefined ? { ids } : { deleteAllSources: options.deleteAllSources ?? true };
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

  function deleteVideo(id: string, options: { deleteSource?: boolean } = {}) {
    return request<{ ok: boolean; deletedSource: boolean }>(
      `/videos/${encodeURIComponent(id)}`,
      {
        method: "DELETE",
        body: JSON.stringify({ deleteSource: !!options.deleteSource }),
      }
    );
  }

  function regenPreview(id: string) {
    return request<{ ok: boolean }>(
      `/videos/${encodeURIComponent(id)}/regen-preview`,
      { method: "POST" }
    );
  }

  // ---------- Tags ----------

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
    return request<{ tag: AdminTag }>(
      `/tags/${encodeURIComponent(String(id))}`,
      {
        method: "PUT",
        body: JSON.stringify({ matchRules }),
      }
    );
  }

  function deleteTag(id: number) {
    return request<{ ok: boolean; removedVideos: number }>(
      `/tags/${encodeURIComponent(String(id))}`,
      { method: "DELETE" }
    );
  }

  // ---------- Settings ----------

  function getSettings() {
    return request<Settings>("/settings");
  }

  /**
   * 更新设置。后端按字段存在与否判断是否变更，所以可以传 Partial 局部更新。
   *
   * 例：只切换主题，其它字段保持原状：
   *   updateSettings({ theme: "light" })
   */

  function updateSettings(body: Partial<Settings>) {
    return request<Settings>("/settings", {
      method: "PUT",
      body: JSON.stringify(body),
    });
  }

  const getGPUStatus = (signal?: AbortSignal) =>
    request<GPUStatus>("/generation/gpu", { signal });

  const probeGPU = (signal?: AbortSignal) =>
    request<GPUStatus>("/generation/gpu/probe", { method: "POST", signal });

  function etagVersion(value: string | null) {
    return (value ?? "").replace(/^W\//, "").replace(/^"|"$/g, "");
  }

  async function configResponseError(res: Response): Promise<string> {
    const text = await res.text().catch(() => "");
    try {
      const parsed = JSON.parse(text) as { error?: unknown };
      if (typeof parsed.error === "string") return parsed.error;
    } catch {
      // Keep a plain-text response as-is.
    }
    return text || `HTTP ${res.status}`;
  }

  async function getConfigYAML(): Promise<ConfigYAMLDocument> {
    const res = await http.fetch(`${BASE}/config.yaml`, {
      credentials: "include",
      headers: { Accept: "application/yaml" },
      cache: "no-store",
    });
    if (res.status === 401) throw new UnauthorizedError();
    if (!res.ok) throw new Error(await configResponseError(res));
    return {
      content: await res.text(),
      version: etagVersion(res.headers.get("ETag")),
    };
  }

  async function updateConfigYAML(
    content: string,
    version: string
  ): Promise<ConfigSaveResult> {
    const headers = new Headers({
      "Content-Type": "application/yaml; charset=utf-8",
      Accept: "application/json",
    });
    if (version) headers.set("If-Match", `"${version}"`);
    const res = await http.fetch(`${BASE}/config.yaml`, {
      method: "PUT",
      credentials: "include",
      headers,
      body: content,
    });
    if (res.status === 401) throw new UnauthorizedError();
    if (res.status === 409) {
      throw new ConfigConflictError(await configResponseError(res));
    }
    if (!res.ok) throw new Error(await configResponseError(res));
    const result = (await res.json()) as ConfigSaveResult;
    return result;
  }


  // ---------- Jobs ----------

  function getScanAllJobStatus(signal?: AbortSignal) {
    return request<MaintenanceJobStatus>("/jobs/scan-all/status", { signal });
  }

  function runScanAllJob() {
    return request<{ ok: boolean; accepted: boolean; status: MaintenanceJobStatus; message?: string }>(
      "/jobs/scan-all/run",
      { method: "POST" }
    );
  }

  function stopAllTasks() {
    return request<{ ok: boolean; stoppedDrives: number; status: MaintenanceJobStatus }>(
      "/tasks/stop",
      { method: "POST" }
    );
  }

  // ---------- Users ----------

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

  // ---------- Banned IPs ----------

  function listBannedIPs(signal?: AbortSignal) {
    return request<BannedIP[]>("/banned-ips", { signal });
  }

  function unbanIP(ip: string) {
    return request<{ ok: boolean }>(`/banned-ips/${encodeURIComponent(ip)}`, {
      method: "DELETE",
    });
  }

  // Telegram imports use the same authenticated administrator API as settings.

  const getTelegramAvailability = (signal?: AbortSignal) => request<{ enabled: boolean }>("/telegram/availability", { signal });

  const getTelegramStatus = (signal?: AbortSignal) => request<TelegramStatus>("/telegram/status", { signal });

  const testTelegram = () => request<{username: string}>("/telegram/test", {method: "POST"});

  const resumeTelegram = () => request<void>("/telegram/resume", {method:"POST"});
  // The Telegram page filters and paginates within this recent record window.

  const listTelegramImports = (limit: number, signal?: AbortSignal) => request<ImportJob[]>(`/import-jobs?${new URLSearchParams({source: "telegram", limit: String(limit)})}`, { signal });

  const cancelImport = (id: string) => request<ImportJob>(`/import-jobs/${encodeURIComponent(id)}/cancel`, {method:"POST"});

  const retryImport = (id: string) => request<void>(`/import-jobs/${encodeURIComponent(id)}/retry`, {method:"POST"});

  return {
    checkUpdate,
    logQueryParams,
    listLogs,
    downloadLogs,
    clearLogs,
    listBackups,
    createBackup,
    cancelBackup,
    deleteBackup,
    backupDownloadURL,
    beginBackupUpload,
    getBackupUpload,
    putBackupUploadChunk,
    finalizeBackupUpload,
    cancelBackupUpload,
    restoreBackup,
    listBackupTransfers,
    listBackupReceiveTransfers,
    cancelBackupReceiveTransfer,
    createBackupTransfer,
    cancelBackupTransfer,
    retryBackupTransfer,
    createBackupReceiveToken,
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
    setCrawlerPaused,
    deleteCrawler,
    startQuarkQRLogin,
    getQuarkQRStatus,
    startP115QRLogin,
    getP115QRStatus,
    startP123QRLogin,
    getP123QRStatus,
    startYun139QRLogin,
    getYun139QRStatus,
    startWopanQRLogin,
    getWopanQRStatus,
    startGuangYaPanQRLogin,
    getGuangYaPanQRStatus,
    listDriveDirChildren,
    setDriveSkipDirIds,
    generateDrivePreviews,
    generateDriveThumbnails,
    generateDriveFingerprints,
    listVideos,
    getVideoStats,
    listBlacklist,
    removeBlacklist,
    getBlacklistSourceDeleteStatus,
    startBlacklistSourceDelete,
    updateVideo,
    deleteVideo,
    regenPreview,
    listTags,
    createTag,
    updateTag,
    deleteTag,
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
    getTelegramAvailability,
    getTelegramStatus,
    testTelegram,
    resumeTelegram,
    listTelegramImports,
    cancelImport,
    retryImport
  };
}
