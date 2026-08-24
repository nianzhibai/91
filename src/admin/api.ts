// 管理后台 API 客户端
// 所有请求都带 cookie，401 会抛错让路由守卫跳登录
const BASE = "/admin/api";
export const ADMIN_LOG_REQUEST_TIMEOUT_MS = 15000;

export class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
  }
}

export class APIResponseError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
    this.name = "APIResponseError";
  }
}

async function request<T>(
  path: string,
  init: RequestInit = {}
): Promise<T> {
  const headers = new Headers(init.headers ?? {});
  if (!(init.body instanceof FormData) && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const res = await fetch(BASE + path, {
    credentials: "include",
    ...init,
    headers,
  });
  if (res.status === 401) {
    throw new UnauthorizedError();
  }
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    let message = text;
    try {
      const parsed = JSON.parse(text) as { error?: unknown };
      if (typeof parsed.error === "string") message = parsed.error;
    } catch {
      // Keep a plain-text error response as-is.
    }
    throw new APIResponseError(res.status, message || `HTTP ${res.status}`);
  }
  if (res.status === 204) return undefined as T;
  const ct = res.headers.get("content-type") ?? "";
  if (ct.includes("application/json")) {
    return (await res.json()) as T;
  }
  return (await res.text()) as unknown as T;
}

export function login(username: string, password: string) {
  return request<{ ok: boolean; role?: string }>("/login", {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
}

export function setupStatus() {
  return request<{ required: boolean }>("/setup");
}

export function setupAdmin(username: string, password: string) {
  return request<{ ok: boolean }>("/setup", {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
}

export function logout() {
  return request<{ ok: boolean }>("/logout", { method: "POST" });
}

export function me() {
  return request<{ authenticated: boolean; role?: string }>("/me");
}

export type UpdateCheck = {
  currentVersion: string;
  latestVersion: string;
  hasUpdate: boolean;
  releaseUrl?: string;
  releaseNotes?: string;
  checkedAt: string;
};

export function checkUpdate() {
  return request<UpdateCheck>("/update/check");
}

// ---------- Runtime logs ----------

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
  message: string;
};

export type AdminLogSnapshot = {
  entries: AdminLogEntry[];
  matched: number;
  storageBytes: number;
  maxStorageBytes: number;
  nextCursor?: string;
  reset?: boolean;
};

export function listLogs(
  filters: {
    source?: AdminLogSource;
    level?: AdminLogLevel;
    method?: AdminLogMethod;
    query?: string;
    limit?: number;
    cursor?: string;
  } = {},
  signal?: AbortSignal
) {
  const params = new URLSearchParams();
  params.set("limit", String(filters.limit ?? 500));
  if (filters.cursor) params.set("cursor", filters.cursor);
  if (filters.source) params.set("source", filters.source);
  if (filters.level) params.set("level", filters.level);
  if (filters.method) params.set("method", filters.method);
  if (filters.query?.trim()) params.set("q", filters.query.trim());
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

export function clearLogs() {
  return request<{ success: boolean }>("/logs", { method: "DELETE" });
}

// ---------- Full backup / migration restore ----------

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

export function listBackups() {
  return request<BackupList>("/backups");
}

export function createBackup(selection?: BackupSelection) {
  return request<BackupTask>("/backups", {
    method: "POST",
    body: selection ? JSON.stringify(selection) : undefined,
  });
}

export function cancelBackup() {
  return request<{ ok: boolean }>("/backups/current/cancel", { method: "POST" });
}

export function deleteBackup(id: string) {
  return request<{ ok: boolean }>(`/backups/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export function backupDownloadURL(id: string) {
  return `${BASE}/backups/${encodeURIComponent(id)}/download`;
}

export function beginBackupUpload(input: {
  fileName: string;
  size: number;
}) {
  return request<BackupUploadSession>("/backup-uploads", {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export function getBackupUpload(id: string) {
  return request<BackupUploadSession>(`/backup-uploads/${encodeURIComponent(id)}`);
}

export async function putBackupUploadChunk(
  id: string,
  index: number,
  chunk: Blob,
  signal?: AbortSignal
): Promise<BackupUploadSession> {
  const res = await fetch(
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

export function finalizeBackupUpload(id: string, sha256: string) {
  return request<BackupRecord>(
    `/backup-uploads/${encodeURIComponent(id)}/finalize`,
    { method: "POST", body: JSON.stringify({ sha256 }) }
  );
}

export function cancelBackupUpload(id: string) {
  return request<{ ok: boolean }>(`/backup-uploads/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export function restoreBackup(
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

export function listBackupTransfers() {
  return request<BackupTransferJob[]>("/backup-transfers");
}

export function listBackupReceiveTransfers() {
  return request<BackupReceiveTransfer[]>("/backup-receives");
}

export function cancelBackupReceiveTransfer(id: string) {
  return request<void>(`/backup-receives/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export function createBackupTransfer(
  backupId: string,
  input: { targetUrl: string; receiveToken: string }
) {
  return request<BackupTransferJob>(
    `/backups/${encodeURIComponent(backupId)}/transfers`,
    { method: "POST", body: JSON.stringify(input) }
  );
}

export function cancelBackupTransfer(id: string) {
  return request<BackupTransferJob>(`/backup-transfers/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export function retryBackupTransfer(id: string) {
  return request<BackupTransferJob>(
    `/backup-transfers/${encodeURIComponent(id)}/retry`,
    { method: "POST" }
  );
}

export function createBackupReceiveToken() {
  return request<BackupReceiveToken>("/backup-receive-tokens", { method: "POST" });
}

// ---------- Drives ----------

export type AdminDrive = {
  id: string;
  kind: "quark" | "p115" | "p123" | "pikpak" | "wopan" | "guangyapan" | "onedrive" | "googledrive" | "webdav" | "localstorage";
  name: string;
  rootId: string;
  status: string;
  lastError?: string;
  hasCredential: boolean;
  /** 后端能力表声明该挂载可写入文件；爬虫目标与转码入口据此展示。 */
  canUpload: boolean;
  /** 当前是否给该盘生成预览视频（per-drive 开关，替代旧的全局 preview.enabled；封面不受影响）。 */
  teaserEnabled: boolean;
  /**
   * 用户在 admin 配置的"扫描跳过目录"集合（drive 侧目录 fileID 列表）。
   * 命中其中任一目录时 scanner 直接跳过、不递归；空数组 = 不跳过任何目录。
   * 替代旧版硬编码 p115 "影视" 目录例外分支。
   */
  skipDirIds: string[];
  // localstorage 的 .strm 是否允许指向存储根目录之外；未配置时后端按 false 返回。
  strmAllowOutsideRoot?: boolean;
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
  // 浏览器兼容性转码：候选(待处理)/已转码/失败/检测后无需转码 计数与任务状态。
  transcodeGenerationStatus?: DriveGenerationStatus;
  transcodePendingCount: number;
  transcodeReadyCount: number;
  transcodeFailedCount: number;
  transcodeSkippedCount: number;
};

export type DriveGenerationStatus = {
  state: string;
  currentTitle?: string;
  queueLength: number;
  cooldownUntil?: string;
  scannedCount: number;
  addedCount: number;
  doneCount: number;
  totalCount: number;
};

export function listDrives() {
  return request<AdminDrive[]>("/drives");
}

export function getDriveCredentials(id: string) {
  return request<{ credentials: Record<string, string> }>(
    `/drives/${encodeURIComponent(id)}/credentials`
  );
}

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

export function getDriveStorage() {
  return request<AdminDriveStorage>("/drives/storage");
}

export type UpsertDriveInput = {
  id: string;
  kind: "quark" | "p115" | "p123" | "pikpak" | "wopan" | "guangyapan" | "onedrive" | "googledrive" | "webdav" | "localstorage";
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
  ok: boolean;
  deferred?: boolean;
  message?: string;
  warning?: string;
};

export function upsertDrive(body: UpsertDriveInput) {
  return request<DriveConfigSaveResult>("/drives", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export type DeleteDriveInput = {
  deleteVideos: true;
};

export function deleteDrive(id: string, body: DeleteDriveInput) {
  return request<{ ok: boolean; deletedVideos: number }>(`/drives/${encodeURIComponent(id)}`, {
    method: "DELETE",
    body: JSON.stringify(body),
  });
}

export function rescan(id: string) {
  return request<{ ok: boolean; accepted: boolean; message?: string; status?: MaintenanceJobStatus }>(
    `/drives/${encodeURIComponent(id)}/rescan`,
    { method: "POST" }
  );
}

export function stopDriveTasks(id: string) {
  return request<{ ok: boolean; stopped: boolean }>(
    `/drives/${encodeURIComponent(id)}/tasks/stop`,
    { method: "POST" }
  );
}

// ---------- Crawlers ----------

export type AdminCrawler = {
  id: string;
  name: string;
  kind: "scriptcrawler";
  status: string;
  lastError?: string;
  scriptPath: string;
  scriptSourceUrl?: string;
  proxy?: string;
  uploadProxy?: string;
  targetNew?: string;
  uploadDriveId?: string;
  paused: boolean;
  teaserEnabled: boolean;
  lastCrawlAt?: number;
  scanGenerationStatus?: DriveGenerationStatus;
  thumbnailGenerationStatus?: DriveGenerationStatus;
  previewGenerationStatus?: DriveGenerationStatus;
  fingerprintGenerationStatus?: DriveGenerationStatus;
  uploadGenerationStatus?: DriveGenerationStatus;
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
  sourceUrl?: string;
};

export type CrawlerDryRunItem = {
  title: string;
  sourceId?: string;
  mediaUrl?: string;
  mediaLocalFile?: string;
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
  items: CrawlerDryRunItem[];
  mediaCheck?: CrawlerDryRunMediaCheck;
  error?: string;
  log?: string[];
  durationMs: number;
};

export function listCrawlers() {
  return request<AdminCrawler[]>("/crawlers");
}

export function upsertCrawler(body: UpsertCrawlerInput) {
  return request<DriveConfigSaveResult & { id: string }>("/crawlers", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function importCrawlerScriptFile(file: File) {
  const form = new FormData();
  form.append("file", file);
  return request<ImportCrawlerScriptResult>("/crawlers/import-file", {
    method: "POST",
    body: form,
  });
}

export function importCrawlerScriptURL(url: string) {
  return request<ImportCrawlerScriptResult>("/crawlers/import-url", {
    method: "POST",
    body: JSON.stringify({ url }),
  });
}

export function testCrawlerScript(body: { scriptPath: string; proxy?: string }) {
  return request<CrawlerDryRunResult>("/crawlers/test-script", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function runCrawler(id: string) {
  return request<{ ok: boolean; accepted: boolean; message?: string; status?: MaintenanceJobStatus }>(
    `/crawlers/${encodeURIComponent(id)}/run`,
    { method: "POST" }
  );
}

export function uploadCrawlerVideos(id: string) {
  return request<{ ok: boolean; accepted: boolean; message?: string; status?: MaintenanceJobStatus }>(
    `/crawlers/${encodeURIComponent(id)}/upload`,
    { method: "POST" }
  );
}

export function stopCrawlerTasks(id: string) {
  return request<{ ok: boolean; stopped: boolean }>(
    `/crawlers/${encodeURIComponent(id)}/tasks/stop`,
    { method: "POST" }
  );
}

export function setCrawlerPaused(id: string, paused: boolean) {
  return request<{ ok: boolean; paused: boolean }>(
    `/crawlers/${encodeURIComponent(id)}/paused`,
    {
      method: "POST",
      body: JSON.stringify({ paused }),
    }
  );
}

export function deleteCrawler(id: string) {
  return request<{ ok: boolean; deletedVideos: number; deletedScript?: boolean; warning?: string }>(`/crawlers/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

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

export function startQuarkQRLogin() {
  return request<QuarkQRSession>("/drives/quark/qr", { method: "POST" });
}

export function getQuarkQRStatus(token: string) {
  return request<QuarkQRStatus>("/drives/quark/qr/status", {
    method: "POST",
    body: JSON.stringify({ token }),
  });
}

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

export function startP115QRLogin() {
  return request<P115QRSession>("/drives/p115/qr", { method: "POST" });
}

export function getP115QRStatus(session: Pick<P115QRSession, "uid" | "time" | "sign">) {
  return request<P115QRStatus>("/drives/p115/qr/status", {
    method: "POST",
    body: JSON.stringify({
      uid: session.uid,
      time: session.time,
      sign: session.sign,
    }),
  });
}

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

export function startP123QRLogin() {
  return request<P123QRSession>("/drives/p123/qr", { method: "POST" });
}

export function getP123QRStatus(uniID: string, loginUuid: string) {
  const qs = new URLSearchParams({ loginUuid });
  return request<P123QRStatus>(
    `/drives/p123/qr/${encodeURIComponent(uniID)}?${qs.toString()}`
  );
}

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

export function startWopanQRLogin() {
  return request<WopanQRSession>("/drives/wopan/qr", { method: "POST" });
}

export function getWopanQRStatus(uuid: string) {
  return request<WopanQRStatus>(`/drives/wopan/qr/${encodeURIComponent(uuid)}`);
}

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

export function startGuangYaPanQRLogin() {
  return request<GuangYaPanQRSession>("/drives/guangyapan/qr", { method: "POST" });
}

export function getGuangYaPanQRStatus(deviceCode: string) {
  const qs = new URLSearchParams({ deviceCode });
  return request<GuangYaPanQRStatus>(`/drives/guangyapan/qr/status?${qs.toString()}`);
}

/**
 * 切换某个云盘的预览视频生成开关。点击网盘列表里行内的 toggle 按钮时调用。
 *
 * 后端会写 catalog.drives.teaser_enabled；空闲时立即生效，有任务时返回
 * deferred=true 并在当前任务结束后切换。
 */
export function setDriveTeaserEnabled(id: string, enabled: boolean) {
  return request<DriveConfigSaveResult & { teaserEnabled: boolean }>(
    `/drives/${encodeURIComponent(id)}/teaser-enabled`,
    {
      method: "POST",
      body: JSON.stringify({ enabled }),
    }
  );
}

/**
 * dirtree 接口的一个目录条目。前端构建按需展开的树时用。
 *
 * 后端只返回直接子目录（不递归），文件忽略。前端每展开一层就调一次
 * listDriveDirChildren(parentId)。115 等慢盘按需展开比一次性铺开整棵树体感
 * 好得多，也避免触发风控。
 */
export type DriveDirEntry = {
  id: string;
  name: string;
};

/**
 * 列指定 drive 在 parentId 目录下的直接子目录。
 * parentId 留空 → 走 drive 的 RootID。
 */
export function listDriveDirChildren(id: string, parentId?: string) {
  const qs = parentId ? `?parent=${encodeURIComponent(parentId)}` : "";
  return request<DriveDirEntry[]>(
    `/drives/${encodeURIComponent(id)}/dirtree${qs}`
  );
}

/**
 * 整体覆盖某盘的"扫描跳过目录"集合（drive 侧目录 fileID）。
 * 传空数组 = 清空跳过列表。下次扫描时生效，不会立刻重扫。
 */
export function setDriveSkipDirIds(id: string, dirIds: string[]) {
  return request<DriveConfigSaveResult & { skipDirIds: string[] }>(
    `/drives/${encodeURIComponent(id)}/skip-dirs`,
    {
      method: "POST",
      body: JSON.stringify({ dirIds }),
    }
  );
}

export function regenFailedPreviews(id: string) {
  return request<{ ok: boolean }>(
    `/drives/${encodeURIComponent(id)}/previews/failed/regenerate`,
    { method: "POST" }
  );
}

/**
 * 触发某 drive 下所有 thumbnail_status=failed 的封面重新入队生成。
 * 与 regenFailedPreviews 行为对称（一个管预览视频，一个管封面）。
 *
 * 后端立即返回 202；实际状态变化在下次 listDrives 拉到的 thumbnailFailedCount /
 * thumbnailGenerationStatus 字段里观察。
 */
export function regenFailedThumbnails(id: string) {
  return request<{ ok: boolean }>(
    `/drives/${encodeURIComponent(id)}/thumbnails/failed/regenerate`,
    { method: "POST" }
  );
}

export function regenFailedFingerprints(id: string) {
  return request<{ ok: boolean }>(
    `/drives/${encodeURIComponent(id)}/fingerprints/failed/regenerate`,
    { method: "POST" }
  );
}

/**
 * 手动开启某存储的浏览器兼容性转码（AVI/WMV 等浏览器播不动的视频转 H.264 MP4，
 * 产物上传回同一存储）。转码默认关闭、从不自动运行，这是唯一入口；
 * 任务处理完候选列表后自然结束。
 */
export function startDriveTranscode(id: string) {
  return request<{ ok: boolean; accepted: boolean; message?: string }>(
    `/drives/${encodeURIComponent(id)}/transcode/start`,
    { method: "POST" }
  );
}

/** 手动停止某存储正在进行的转码任务。 */
export function stopDriveTranscode(id: string) {
  return request<{ ok: boolean; stopped: boolean }>(
    `/drives/${encodeURIComponent(id)}/transcode/stop`,
    { method: "POST" }
  );
}

// ---------- Videos ----------

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
  description: string;
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
  createdFrom?: string;
  createdTo?: string;
  durationMinMinutes?: string;
  durationMaxMinutes?: string;
  page?: number;
  size?: number;
  keyword?: string;
};

export function listVideos(
  params: AdminVideoListParams = {}
) {
  const qs = new URLSearchParams();
  if (params.driveId) qs.set("driveId", params.driveId);
  if (params.crawlerId) qs.set("crawlerId", params.crawlerId);
  if (params.createdFrom) qs.set("createdFrom", params.createdFrom);
  if (params.createdTo) qs.set("createdTo", params.createdTo);
  if (params.durationMinMinutes) qs.set("durationMinMinutes", params.durationMinMinutes);
  if (params.durationMaxMinutes) qs.set("durationMaxMinutes", params.durationMaxMinutes);
  if (params.page) qs.set("page", String(params.page));
  if (params.size) qs.set("size", String(params.size));
  if (params.keyword) qs.set("keyword", params.keyword);
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<AdminVideoList>(`/videos${suffix}`);
}

// 后台视频管理两个标签页的计数。
export type VideoStats = {
  current: number;
  blacklisted: number;
};

export function getVideoStats() {
  return request<VideoStats>("/videos/stats");
}

// 黑名单（被拉黑/手动删除、扫盘不再入库的视频）。原始记录已删除，
// 这里只保留重新发现所需信息和恢复策略；源文件删除成功后记录会被移除。
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

export function listBlacklist(
  params: { driveId?: string; page?: number; size?: number; keyword?: string } = {}
) {
  const qs = new URLSearchParams();
  if (params.driveId) qs.set("driveId", params.driveId);
  if (params.page) qs.set("page", String(params.page));
  if (params.size) qs.set("size", String(params.size));
  if (params.keyword) qs.set("keyword", params.keyword);
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<AdminBlacklistList>(`/blacklist${suffix}`);
}

// 允许视频在后续手动/定时任务中重新入库；此操作不会立即触发扫盘或爬取。
export function removeBlacklist(id: string) {
  return request<{ ok: boolean }>(`/blacklist/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

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

export function getBlacklistSourceDeleteStatus() {
  return request<BlacklistSourceDeleteStatus>("/blacklist/source-delete/status");
}

export function startBlacklistSourceDelete(
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

export type UpdateVideoInput = Partial<{
  tags: string[];
  badges: string[];
  description: string;
  thumbnail: string;
  durationSeconds: number;
}>;

export function updateVideo(id: string, body: UpdateVideoInput) {
  return request<AdminVideo>(`/videos/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

export function deleteVideo(id: string, options: { deleteSource?: boolean } = {}) {
  return request<{ ok: boolean; deletedSource: boolean }>(
    `/videos/${encodeURIComponent(id)}`,
    {
      method: "DELETE",
      body: JSON.stringify({ deleteSource: !!options.deleteSource }),
    }
  );
}

export function regenPreview(id: string) {
  return request<{ ok: boolean }>(
    `/videos/${encodeURIComponent(id)}/regen-preview`,
    { method: "POST" }
  );
}

// ---------- Tags ----------

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

export async function listTags(): Promise<AdminTag[]> {
  const tags = await request<AdminTag[] | null>("/tags");
  if (tags === null) return [];
  if (!Array.isArray(tags)) {
    throw new Error("Invalid /admin/api/tags response");
  }
  return tags;
}

export function createTag(label: string) {
  return request<{ label: string; classified: number }>("/tags", {
    method: "POST",
    body: JSON.stringify({ label }),
  });
}

export function updateTag(id: number, matchRules: TagMatchRules) {
  return request<{ tag: AdminTag }>(
    `/tags/${encodeURIComponent(String(id))}`,
    {
      method: "PUT",
      body: JSON.stringify({ matchRules }),
    }
  );
}

export function deleteTag(id: number) {
  return request<{ ok: boolean; removedVideos: number }>(
    `/tags/${encodeURIComponent(String(id))}`,
    { method: "DELETE" }
  );
}

// ---------- Settings ----------

export type Theme = "dark" | "pink" | "sky";

export type Settings = {
  theme: Theme;
};

export function getSettings() {
  return request<Settings>("/settings");
}

/**
 * 更新设置。后端按字段存在与否判断是否变更，所以可以传 Partial 局部更新。
 *
 * 例：只切换主题，其它字段保持原状：
 *   updateSettings({ theme: "pink" })
 */
export function updateSettings(body: Partial<Settings>) {
  return request<Settings>("/settings", {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

export type ConfigYAMLDocument = {
  content: string;
  version: string;
};

export type ConfigSaveResult = {
  version: string;
  restartRequired: boolean;
  settings: {
    nightlyStartTime: string;
    nightlyTimezone: string;
    builtinTagsEnabled: boolean;
  };
};

export class ConfigConflictError extends Error {
  constructor(message = "config.yaml 已被其他操作修改") {
    super(message);
    this.name = "ConfigConflictError";
  }
}

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

export async function getConfigYAML(): Promise<ConfigYAMLDocument> {
  const res = await fetch(`${BASE}/config.yaml`, {
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

export async function updateConfigYAML(
  content: string,
  version: string
): Promise<ConfigSaveResult> {
  const headers = new Headers({
    "Content-Type": "application/yaml; charset=utf-8",
    Accept: "application/json",
  });
  if (version) headers.set("If-Match", `"${version}"`);
  const res = await fetch(`${BASE}/config.yaml`, {
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
  return (await res.json()) as ConfigSaveResult;
}


// ---------- Jobs ----------

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
};

export function getScanAllJobStatus() {
  return request<MaintenanceJobStatus>("/jobs/scan-all/status");
}

export function runScanAllJob() {
  return request<{ ok: boolean; accepted: boolean; status: MaintenanceJobStatus; message?: string }>(
    "/jobs/scan-all/run",
    { method: "POST" }
  );
}

export function stopAllTasks() {
  return request<{ ok: boolean; stoppedDrives: number; status: MaintenanceJobStatus }>(
    "/tasks/stop",
    { method: "POST" }
  );
}

// ---------- Users ----------

export type AdminUser = {
  id: number;
  username: string;
  role: string;
  banned: boolean;
  createdAt: number;
};

export function listUsers() {
  return request<AdminUser[]>("/users");
}

export function createUser(body: { username: string; password: string; role: string }) {
  return request<{ ok: boolean; id: number }>("/users", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function deleteUser(id: number) {
  return request<{ ok: boolean }>(`/users/${id}`, { method: "DELETE" });
}

export function banUser(id: number) {
  return request<{ ok: boolean }>(`/users/${id}/ban`, { method: "POST" });
}

export function unbanUser(id: number) {
  return request<{ ok: boolean }>(`/users/${id}/unban`, { method: "POST" });
}

export function resetPassword(id: number, password: string) {
  return request<{ ok: boolean }>(`/users/${id}/password`, {
    method: "PUT",
    body: JSON.stringify({ password }),
  });
}

// ---------- Banned IPs ----------

export type BannedIP = {
  ip: string;
  reason: string;
  createdAt: number;
};

export function listBannedIPs() {
  return request<BannedIP[]>("/banned-ips");
}

export function unbanIP(ip: string) {
  return request<{ ok: boolean }>(`/banned-ips/${encodeURIComponent(ip)}`, {
    method: "DELETE",
  });
}
