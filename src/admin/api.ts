// 管理后台 API 客户端
// 所有请求都带 cookie，401 会抛错让路由守卫跳登录
const BASE = "/admin/api";

export class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
  }
}

async function request<T>(
  path: string,
  init: RequestInit = {}
): Promise<T> {
  const res = await fetch(BASE + path, {
    credentials: "include",
    headers: {
      "Content-Type": "application/json",
      ...(init.headers ?? {}),
    },
    ...init,
  });
  if (res.status === 401) {
    throw new UnauthorizedError();
  }
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(text || `HTTP ${res.status}`);
  }
  if (res.status === 204) return undefined as T;
  const ct = res.headers.get("content-type") ?? "";
  if (ct.includes("application/json")) {
    return (await res.json()) as T;
  }
  return (await res.text()) as unknown as T;
}

export function login(username: string, password: string) {
  return request<{ ok: boolean }>("/login", {
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
  return request<{ authenticated: boolean }>("/me");
}

export type UpdateCheck = {
  currentVersion: string;
  latestVersion: string;
  hasUpdate: boolean;
  releaseUrl?: string;
  checkedAt: string;
};

export function checkUpdate() {
  return request<UpdateCheck>("/update/check");
}

// ---------- Drives ----------

export type AdminDrive = {
  id: string;
  kind: "quark" | "p115" | "p123" | "pikpak" | "wopan" | "onedrive" | "googledrive" | "localstorage" | "spider91";
  name: string;
  rootId: string;
  status: string;
  lastError?: string;
  hasCredential: boolean;
  /** 当前是否给该盘生成预览视频/封面（per-drive 开关，替代旧的全局 preview.enabled）。 */
  teaserEnabled: boolean;
  /**
   * 用户在 admin 配置的"扫描跳过目录"集合（drive 侧目录 fileID 列表）。
   * 命中其中任一目录时 scanner 直接跳过、不递归；空数组 = 不跳过任何目录。
   * 替代旧版硬编码 p115 "影视" 目录例外分支。
   */
  skipDirIds: string[];
  // spider91 上次成功爬取时间（unix 秒）；其它 kind 留空。
  lastCrawlAt?: number;
  // spider91 专用代理地址；仅后台管理接口返回，用于编辑表单回显。
  spider91Proxy?: string;
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
  state: string;
  currentTitle?: string;
  queueLength: number;
  cooldownUntil?: string;
};

export function listDrives() {
  return request<AdminDrive[]>("/drives");
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
  kind: "quark" | "p115" | "p123" | "pikpak" | "wopan" | "onedrive" | "googledrive" | "localstorage" | "spider91";
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

export function upsertDrive(body: UpsertDriveInput) {
  return request<{ ok: boolean; warning?: string }>("/drives", {
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
  return request<{ ok: boolean }>(
    `/drives/${encodeURIComponent(id)}/rescan`,
    { method: "POST" }
  );
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

/**
 * 切换某个云盘的预览视频生成开关。点击网盘列表里行内的 toggle 按钮时调用。
 *
 * 后端会写 catalog.drives.teaser_enabled，并在从关到开时立刻补扫该盘 pending 预览视频；
 * 关闭分支不补做任何事，新的入队判断会自动停。
 */
export function setDriveTeaserEnabled(id: string, enabled: boolean) {
  return request<{ ok: boolean; teaserEnabled: boolean }>(
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
  return request<{ ok: boolean; skipDirIds: string[] }>(
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

// ---------- Videos ----------

export type AdminVideo = {
  id: string;
  driveId: string;
  fileId: string;
  title: string;
  author: string;
  tags: string[];
  category: string;
  durationSeconds: number;
  size: number;
  ext: string;
  quality: string;
  thumbnailUrl: string;
  previewStatus: string;
  views: number;
  favorites: number;
  comments: number;
  likes: number;
  frontendSelected: boolean;
  badges: string[];
  description: string;
};

export type AdminVideoList = {
  items: AdminVideo[];
  total: number;
  page: number;
  size: number;
};

export type ListVideosParams = {
  driveId?: string;
  page?: number;
  size?: number;
  keyword?: string;
};

export type UpdateVideoInput = {
  title: string;
  author: string;
  tags: string[];
  category: string;
  badges: string[];
  description: string;
  thumbnail: string;
  quality: string;
  durationSeconds: number;
};

export function listVideos(params: ListVideosParams = {}) {
  const qs = new URLSearchParams();
  if (params.driveId) qs.set("driveId", params.driveId);
  if (params.page) qs.set("page", String(params.page));
  if (params.size) qs.set("size", String(params.size));
  if (params.keyword) qs.set("keyword", params.keyword);
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<AdminVideoList>(`/videos${suffix}`);
}

export function updateVideo(id: string, body: UpdateVideoInput) {
  return request<AdminVideo>(`/videos/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

export function setVideoFrontendSelected(id: string, enabled: boolean) {
  return request<AdminVideo>(`/videos/${encodeURIComponent(id)}/frontend-selected`, {
    method: "POST",
    body: JSON.stringify({ enabled }),
  });
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
  aliases?: string[];
  source: string;
  frontendAccess: boolean;
  count: number;
};

export function listTags() {
  return request<AdminTag[]>("/tags");
}

export function createTag(label: string, aliases: string[]) {
  return request<{ label: string; classified: number }>("/tags", {
    method: "POST",
    body: JSON.stringify({ label, aliases }),
  });
}

export function setTagFrontendAccess(id: number, enabled: boolean) {
  return request<{ ok: boolean; frontendAccess: boolean }>(
    `/tags/${encodeURIComponent(String(id))}/frontend-access`,
    {
      method: "POST",
      body: JSON.stringify({ enabled }),
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

export type Theme = "dark" | "pink";

export type Settings = {
  theme: Theme;
  /**
   * spider91 视频迁移到云盘时的目标 drive ID（必须是已挂载的 pikpak、p115 或 onedrive drive）。
   * - 空字符串：本地保存，不上传到云盘。
   * - 非空：显式指定。后端会校验 drive 存在且 kind ∈ {pikpak, p115, onedrive}。
   */
  spider91UploadDriveId: string;
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


// ---------- Jobs ----------

/**
 * 立即触发一次完整的凌晨流水线（Phase1 扫盘 + Phase2 91 爬虫 + Phase3 迁移），
 * 不论当前时间或今日是否已跑。立即返回 202；进度通过任务状态和 backend 日志观察。
 *
 * 流水线已在跑或已排队时，后端会拒绝重复触发。
 */
export type NightlyJobStatus = {
  state: "idle" | "queued" | "running" | "running_queued";
  running: boolean;
  queued: boolean;
  startedAt?: string;
  lastFinishedAt?: string;
};

export function getNightlyJobStatus() {
  return request<NightlyJobStatus>("/jobs/nightly/status");
}

export function runNightlyJob() {
  return request<{ ok: boolean; accepted: boolean; status: NightlyJobStatus }>(
    "/jobs/nightly/run",
    { method: "POST" }
  );
}
