import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type DragEvent,
  type ReactNode,
} from "react";
import {
  AlertTriangle,
  Check,
  Download,
  FileCode2,
  Plus,
  TestTube,
  Upload,
} from "lucide-react";
import * as api from "./api";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import { useToast } from "./ToastContext";
import { generationStateClass, generationStateLabel } from "./drive/constants";
import { CrawlerUploadTargetField } from "./drive/CrawlerUploadTargetField";
import { SpiderIcon } from "./icons/SpiderIcon";
import { AdminEmptyVisual } from "./AdminEmptyVisual";
import {
  CrawlerListControlsPlaceholder,
  CrawlerListSkeleton,
} from "./CrawlersPageLoading";
import { useAdminFloatingActionSpace } from "./useAdminFloatingActionSpace";
import {
  useAdminRouteActive,
  useAdminRouteRevalidation,
} from "./AdminRouteCache";

const BUSY_STATES = new Set(["scanning", "generating", "uploading", "queued"]);
const POLL_INTERVAL_MS = 5000;

function statusBusy(status?: api.DriveGenerationStatus) {
  return BUSY_STATES.has(status?.state ?? "");
}

function crawlerBusy(crawler: api.AdminCrawler) {
  return (
    statusBusy(crawler.scanGenerationStatus) ||
    statusBusy(crawler.thumbnailGenerationStatus) ||
    statusBusy(crawler.previewGenerationStatus) ||
    statusBusy(crawler.fingerprintGenerationStatus) ||
    statusBusy(crawler.uploadGenerationStatus)
  );
}

export function CrawlersPage() {
  const floatingActionPageRef = useAdminFloatingActionSpace<HTMLElement>();
  const routeActive = useAdminRouteActive();
  const [list, setList] = useState<api.AdminCrawler[]>([]);
  const [uploadTargets, setUploadTargets] = useState<api.AdminDrive[]>([]);
  const [loading, setLoading] = useState(true);
  const [detailTargetId, setDetailTargetId] = useState("");
  const [runningId, setRunningId] = useState("");
  const [uploadingId, setUploadingId] = useState("");
  const [stoppingId, setStoppingId] = useState("");
  const [togglingPausedId, setTogglingPausedId] = useState("");
  const [togglingTeasers, setTogglingTeasers] = useState(false);
  // undefined = 编辑器关闭；null = 新建；其余 = 编辑已有爬虫
  const [editorTarget, setEditorTarget] = useState<api.AdminCrawler | null | undefined>(undefined);
  const [deleteTarget, setDeleteTarget] = useState<api.AdminCrawler | null>(null);
  const [deleting, setDeleting] = useState(false);
  const refreshingRef = useRef(false);
  const { show } = useToast();

  const refresh = useCallback(
    async (silent = false) => {
      if (refreshingRef.current) return;
      refreshingRef.current = true;
      if (!silent) setLoading(true);
      try {
        const [data, drives] = await Promise.all([api.listCrawlers(), api.listDrives()]);
        setList(data);
        setUploadTargets(drives.filter((d) => d.canUpload));
      } catch (e) {
        if (!silent) show(e instanceof Error ? e.message : "加载爬虫失败", "error");
      } finally {
        refreshingRef.current = false;
        if (!silent) setLoading(false);
      }
    },
    [show]
  );

  useEffect(() => {
    refresh();
  }, [refresh]);

  useAdminRouteRevalidation(() => {
    void refresh(true);
  });

  // 有任务进行中时自动轮询，页面切到后台时暂停
  const anyBusy = useMemo(() => list.some(crawlerBusy), [list]);
  useEffect(() => {
    if (!routeActive || !anyBusy) return;
    const timer = window.setInterval(() => {
      if (!document.hidden) refresh(true);
    }, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [anyBusy, refresh, routeActive]);

  async function run(crawler: api.AdminCrawler) {
    setRunningId(crawler.id);
    try {
      const resp = await api.runCrawler(crawler.id);
      if (!resp.accepted) {
        show(resp.message || "当前爬虫有正在进行的任务", "info");
        return;
      }
      show("已触发抓取任务", "success");
      await refresh(true);
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setRunningId("");
    }
  }

  async function uploadVideos(crawler: api.AdminCrawler) {
    setUploadingId(crawler.id);
    try {
      const resp = await api.uploadCrawlerVideos(crawler.id);
      if (!resp.accepted) {
        show(resp.message || "当前爬虫暂不满足上传条件", "info");
        return;
      }
      show("已触发当前爬虫的上传任务", "success");
      await refresh(true);
    } catch (e) {
      show(e instanceof Error ? e.message : "触发上传失败", "error");
    } finally {
      setUploadingId("");
    }
  }

  async function stop(crawler: api.AdminCrawler) {
    setStoppingId(crawler.id);
    try {
      const resp = await api.stopCrawlerTasks(crawler.id);
      show(resp.stopped ? "已请求停止任务" : "当前没有可停止任务", "info");
      await refresh(true);
    } catch (e) {
      show(e instanceof Error ? e.message : "停止失败", "error");
    } finally {
      setStoppingId("");
    }
  }

  async function togglePaused(crawler: api.AdminCrawler) {
    const next = !crawler.paused;
    const previous = list;
    setTogglingPausedId(crawler.id);
    setList((prev) => prev.map((item) => (item.id === crawler.id ? { ...item, paused: next } : item)));
    try {
      await api.setCrawlerPaused(crawler.id, next);
      show(next ? "已暂停该爬虫的凌晨抓取" : "已恢复该爬虫的凌晨抓取", "success");
      await refresh(true);
    } catch (e) {
      setList(previous);
      show(e instanceof Error ? e.message : "切换暂停状态失败", "error");
    } finally {
      setTogglingPausedId("");
    }
  }

  async function toggleCrawlerTeasers() {
    if (list.length === 0 || togglingTeasers) return;
    const next = !list.every((item) => item.teaserEnabled);
    const previous = list;
    setTogglingTeasers(true);
    setList((prev) => prev.map((item) => ({ ...item, teaserEnabled: next })));
    try {
      let deferred = false;
      for (const crawler of previous) {
        if (crawler.teaserEnabled !== next) {
          const resp = await api.setDriveTeaserEnabled(crawler.id, next);
          deferred = deferred || Boolean(resp.deferred);
        }
      }
      show(
        deferred
          ? "已保存，相关爬虫任务结束后生效"
          : next
            ? "已开启所有爬虫预览视频生成"
            : "已关闭所有爬虫预览视频生成",
        "success"
      );
      await refresh(true);
    } catch (e) {
      setList(previous);
      show(e instanceof Error ? e.message : "批量切换预览视频失败", "error");
    } finally {
      setTogglingTeasers(false);
    }
  }

  async function confirmDelete() {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      const resp = await api.deleteCrawler(deleteTarget.id);
      if (resp.warning) {
        show(`已删除爬虫配置，但脚本文件清理失败：${resp.warning}`, "error");
      } else {
        show(`已删除爬虫，并清理 ${resp.deletedVideos} 个本地视频`, "success");
      }
      setDeleteTarget(null);
      if (detailTargetId === deleteTarget.id) setDetailTargetId("");
      await refresh(true);
    } catch (e) {
      show(e instanceof Error ? e.message : "删除失败", "error");
    } finally {
      setDeleting(false);
    }
  }

  const hasCrawlers = list.length > 0;
  const allCrawlerTeasersEnabled = list.every((item) => item.teaserEnabled);
  const partialCrawlerTeasersEnabled = !allCrawlerTeasersEnabled && list.some((item) => item.teaserEnabled);

  return (
    <section
      ref={floatingActionPageRef}
      className="admin-page admin-page--with-floating-actions admin-crawlers-page"
    >
      <div className="admin-crawler-console">
        <div
          className="admin-card admin-crawler-list"
          aria-busy={loading || undefined}
        >
          {loading && !hasCrawlers && <CrawlerListControlsPlaceholder />}
          {hasCrawlers && (
            <div className="admin-crawler-list__controls">
              <div className="admin-crawler-global-teaser">
                <span>预览视频</span>
                <button
                  type="button"
                  className={`toggle-switch ${allCrawlerTeasersEnabled ? "is-on" : ""} ${
                    togglingTeasers ? "is-saving" : ""
                  }`}
                  role="switch"
                  aria-checked={allCrawlerTeasersEnabled}
                  aria-label="切换全部爬虫预览视频生成"
                  disabled={loading || togglingTeasers}
                  onClick={toggleCrawlerTeasers}
                  title={
                    loading
                      ? "正在加载爬虫列表"
                      : partialCrawlerTeasersEnabled
                        ? "部分爬虫已开启，点击开启全部"
                        : allCrawlerTeasersEnabled
                          ? "关闭所有爬虫预览视频生成"
                          : "开启所有爬虫预览视频生成"
                  }
                >
                  <span className="toggle-switch__dot" />
                </button>
              </div>
            </div>
          )}
          {loading ? (
            <CrawlerListSkeleton />
          ) : list.length === 0 ? (
            <AdminEmptyVisual
              variant="empty"
              text="暂无爬虫"
              className="admin-crawler-empty"
            />
          ) : (
            <div className="admin-crawler-table">
              {list.map((crawler) => (
                <CrawlerRow
                  key={crawler.id}
                  crawler={crawler}
                  expanded={detailTargetId === crawler.id}
                  running={runningId === crawler.id}
                  uploading={uploadingId === crawler.id}
                  stopping={stoppingId === crawler.id}
                  togglingPaused={togglingPausedId === crawler.id}
                  onToggleOpen={() => setDetailTargetId((current) => (current === crawler.id ? "" : crawler.id))}
                  onRun={() => run(crawler)}
                  onUpload={() => uploadVideos(crawler)}
                  onStop={() => stop(crawler)}
                  onEdit={() => {
                    setDetailTargetId("");
                    setEditorTarget(crawler);
                  }}
                  onDelete={() => {
                    setDetailTargetId("");
                    setDeleteTarget(crawler);
                  }}
                  onTogglePaused={() => togglePaused(crawler)}
                />
              ))}
            </div>
          )}
        </div>
      </div>

      <button
        data-admin-floating-actions
        type="button"
        className="admin-btn admin-create-fab"
        onClick={() => setEditorTarget(null)}
      >
        <Plus size="1em" aria-hidden="true" />
        添加爬虫
      </button>

      {editorTarget !== undefined && (
        <CrawlerEditorModal
          key={editorTarget?.id ?? "new"}
          open
          crawler={editorTarget}
          uploadTargets={uploadTargets}
          onClose={() => setEditorTarget(undefined)}
          onSaved={() => {
            setEditorTarget(undefined);
            refresh(true);
          }}
        />
      )}

      <ConfirmModal
        open={deleteTarget !== null}
        title="删除爬虫"
        message={`确定删除爬虫「${deleteTarget?.name ?? ""}」？正在运行的任务将先自动停止；退出后，本地保留的视频、封面、预览和抓取文件将一并删除，已迁移到网盘的视频不受影响。`}
        plainConfirm
        hideIcon
        loading={deleting}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={confirmDelete}
      />
    </section>
  );
}

function CrawlerRow({
  crawler,
  expanded,
  running,
  uploading,
  stopping,
  togglingPaused,
  onToggleOpen,
  onRun,
  onUpload,
  onStop,
  onEdit,
  onDelete,
  onTogglePaused,
}: {
  crawler: api.AdminCrawler;
  expanded: boolean;
  running: boolean;
  uploading: boolean;
  stopping: boolean;
  togglingPaused: boolean;
  onToggleOpen: () => void;
  onRun: () => void;
  onUpload: () => void;
  onStop: () => void;
  onEdit: () => void;
  onDelete: () => void;
  onTogglePaused: () => void;
}) {
  const crawling = running || crawler.scanGenerationStatus?.state === "scanning";
  return (
    <div className={`admin-crawler-row ${expanded ? "is-expanded" : ""}`}>
      <div className="admin-crawler-row__line">
        <button type="button" className="admin-crawler-row__main" onClick={onToggleOpen} aria-expanded={expanded}>
          <SpiderIcon size={20} className="admin-crawler-row__icon" />
          <span className="admin-crawler-row__title-wrap">
            <span className="admin-crawler-row__title-line">
              <strong>{crawler.name}</strong>
              {crawling && (
                <span className="admin-status admin-generation-state is-generating">
                  正在抓取
                </span>
              )}
            </span>
            <span className="admin-crawler-row__meta">
              上次抓取 {formatLastCrawl(crawler.lastCrawlAt)} · 每次新增 {crawler.targetNew || "10"} 条 · 累计爬取 {crawler.totalCrawledCount ?? 0} 条
            </span>
          </span>
        </button>
        <div className="admin-crawler-row__actions">
          <button className="admin-btn" type="button" onClick={onTogglePaused} disabled={togglingPaused}>
            {togglingPaused ? "处理中..." : crawler.paused ? "恢复使用" : "暂停使用"}
          </button>
          <button className="admin-btn" type="button" onClick={onRun} disabled={running}>
            {running ? "触发中..." : "立即抓取"}
          </button>
          <button className="admin-btn" type="button" onClick={onUpload}>
            {uploading ? "上传中..." : "触发上传"}
          </button>
          <button className="admin-btn" type="button" onClick={onEdit}>
            编辑
          </button>
          <button className="admin-btn is-danger" type="button" onClick={onDelete}>
            删除
          </button>
        </div>
      </div>
      {expanded && (
        <CrawlerDetail
          crawler={crawler}
          stopping={stopping}
          onStop={onStop}
        />
      )}
    </div>
  );
}

function CrawlerDetail({
  crawler,
  stopping,
  onStop,
}: {
  crawler: api.AdminCrawler;
  stopping: boolean;
  onStop: () => void;
}) {
  const scan = crawler.scanGenerationStatus;
  const upload = crawlerUploadDisplayStatus(crawler);
  const busy = crawlerBusy(crawler);
  return (
    <div className="admin-crawler-detail">
      {busy && (
        <div className="admin-crawler-detail__actions">
          <button className="admin-btn" type="button" onClick={onStop} disabled={stopping}>
            {stopping ? "暂停中..." : "暂停"}
          </button>
        </div>
      )}
      <div className="admin-crawler-detail__grid">
        <GenStageCard
          label="抓取"
          status={scan}
          stateText={scan?.state === "scanning" ? "抓取中" : generationStateLabel(scan?.state || "idle")}
          counts={[
            { label: "累计爬取", value: crawler.totalCrawledCount ?? 0 },
            { label: "本轮检查", value: scan?.scannedCount ?? 0 },
            { label: "本轮新增", value: scan?.addedCount ?? 0 },
          ]}
        />
        <GenStageCard
          label="上传"
          status={upload.status}
          stateText={upload.text}
          counts={[
            { label: "已上传", value: crawler.migratedVideoCount ?? 0 },
            { label: "本地保留", value: crawler.localVideoCount ?? 0 },
          ]}
        />
        <GenStageCard
          label="封面"
          status={crawler.thumbnailGenerationStatus}
          counts={[
            { label: "已生成", value: crawler.thumbnailReadyCount },
            { label: "待生成", value: crawler.thumbnailPendingCount },
            { label: "失败", value: crawler.thumbnailFailedCount, tone: "danger" },
          ]}
        />
        <GenStageCard
          label="预览视频"
          status={crawler.previewGenerationStatus}
          counts={[
            { label: "已生成", value: crawler.teaserReadyCount },
            { label: "待生成", value: crawler.teaserPendingCount },
            { label: "失败", value: crawler.teaserFailedCount, tone: "danger" },
          ]}
        />
        <GenStageCard
          label="视频指纹"
          status={crawler.fingerprintGenerationStatus}
          counts={[
            { label: "已生成", value: crawler.fingerprintReadyCount },
            { label: "待生成", value: crawler.fingerprintPendingCount },
            { label: "失败", value: crawler.fingerprintFailedCount, tone: "danger" },
          ]}
        />
      </div>
      {crawler.lastError && (
        <div className="admin-crawler-detail__error">
          <AlertTriangle size={14} />
          <span>{crawler.lastError}</span>
        </div>
      )}
    </div>
  );
}

function crawlerUploadDisplayStatus(crawler: api.AdminCrawler): {
  status: api.DriveGenerationStatus;
  text: string;
} {
  const live = crawler.uploadGenerationStatus;
  const state = live?.state || "idle";
  const localCount = crawler.localVideoCount ?? 0;
  const totalCount = crawler.totalCrawledCount ?? 0;
  const base: api.DriveGenerationStatus = {
    state,
    currentTitle: live?.currentTitle,
    queueLength: live?.queueLength ?? 0,
    cooldownUntil: live?.cooldownUntil,
    scannedCount: live?.scannedCount ?? 0,
    addedCount: live?.addedCount ?? 0,
    doneCount: live?.doneCount ?? 0,
    totalCount: live?.totalCount ?? 0,
  };

  if (!crawler.uploadDriveId) {
    return {
      status: base,
      text: localCount > 0 ? "本地保存" : generationStateLabel(state),
    };
  }
  if (state === "uploading") {
    return { status: base, text: "上传中" };
  }
  if (state === "queued") {
    return { status: base, text: "排队中" };
  }
  if (localCount > 0) {
    return {
      status: { ...base, state: "queued", queueLength: localCount },
      text: "待上传",
    };
  }
  if (totalCount > 0) {
    return { status: base, text: "完成" };
  }
  return { status: base, text: generationStateLabel(state) };
}

function GenStageCard({
  label,
  status,
  stateText,
  counts,
}: {
  label: string;
  status?: api.DriveGenerationStatus;
  stateText?: string;
  counts: Array<{ label: string; value: number; tone?: "danger" }>;
}) {
  const state = status?.state || "idle";
  return (
    <div className="admin-gen-col">
      <div className="admin-gen-col__head">
        <span className="admin-gen-col__label">{label}</span>
        <span className={`admin-status admin-generation-state is-${generationStateClass(state)}`}>
          {stateText ?? generationStateLabel(state)}
        </span>
      </div>
      {status?.currentTitle && <div className="admin-gen-col__detail">{status.currentTitle}</div>}
      <div className="admin-gen-col__counts">
        {counts.map((count) => (
          <div className="admin-gen-col__count" key={count.label}>
            <span>{count.label}</span>
            <strong className={count.tone === "danger" && count.value > 0 ? "is-danger" : undefined}>{count.value}</strong>
          </div>
        ))}
      </div>
    </div>
  );
}

// ---------- 编辑器 ----------

type EditorForm = {
  scriptPath: string;
  scriptSourceUrl: string;
  name: string;
  targetNew: string;
  proxy: string;
  uploadProxy: string;
  uploadDriveId: string;
};

function editorFormFromCrawler(crawler: api.AdminCrawler | null): EditorForm {
  return {
    scriptPath: crawler?.scriptPath ?? "",
    scriptSourceUrl: crawler?.scriptSourceUrl ?? "",
    name: crawler?.name ?? "",
    targetNew: crawler?.targetNew || "10",
    proxy: crawler?.proxy ?? "",
    uploadProxy: crawler?.uploadProxy ?? "",
    uploadDriveId: crawler?.uploadDriveId ?? "",
  };
}

function CrawlerEditorModal({
  open,
  crawler,
  uploadTargets,
  onClose,
  onSaved,
}: {
  open: boolean;
  crawler: api.AdminCrawler | null;
  uploadTargets: api.AdminDrive[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const isEdit = crawler !== null;
  const [form, setForm] = useState<EditorForm>(() => editorFormFromCrawler(crawler));
  const [scriptURL, setScriptURL] = useState("");
  const [importing, setImporting] = useState(false);
  const [replacingScript, setReplacingScript] = useState(false);
  // 已通过原链接拉取过新脚本（路径不变，内容已更新）
  const [scriptUpdated, setScriptUpdated] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<api.CrawlerDryRunResult | null>(null);
  const [saving, setSaving] = useState(false);
  const [dragOver, setDragOver] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const { show } = useToast();

  useEffect(() => {
    if (!open) return;
    setForm(editorFormFromCrawler(crawler));
    setScriptURL("");
    setTestResult(null);
    setDragOver(false);
    setReplacingScript(false);
    setScriptUpdated(false);
  }, [open, crawler]);

  // 编辑模式下默认收起导入区，点「替换脚本文件」再展开
  const showImportArea = !isEdit || replacingScript;
  const scriptChanged = form.scriptPath !== (crawler?.scriptPath ?? "");

  function set<K extends keyof EditorForm>(key: K, value: EditorForm[K]) {
    setForm((prev) => ({ ...prev, [key]: value }));
  }

  function cancelReplace() {
    setForm((prev) => ({
      ...prev,
      scriptPath: crawler?.scriptPath ?? "",
      scriptSourceUrl: crawler?.scriptSourceUrl ?? "",
      name: crawler?.name ?? "",
    }));
    setScriptURL("");
    setTestResult(null);
    setReplacingScript(false);
  }

  async function importFile(file: File | null | undefined) {
    if (!file) return;
    if (!file.name.toLowerCase().endsWith(".py")) {
      show("仅支持 .py 脚本文件", "error");
      return;
    }
    setImporting(true);
    try {
      const resp = await api.importCrawlerScriptFile(file);
      setForm((prev) => ({ ...prev, scriptPath: resp.scriptPath, name: resp.name, scriptSourceUrl: "" }));
      setTestResult(null);
      show("脚本已导入", "success");
    } catch (e) {
      show(e instanceof Error ? e.message : "导入失败", "error");
    } finally {
      setImporting(false);
    }
  }

  async function importURL() {
    const url = scriptURL.trim();
    if (!url) {
      show("请填写链接", "error");
      return;
    }
    setImporting(true);
    try {
      const resp = await api.importCrawlerScriptURL(url);
      setForm((prev) => ({
        ...prev,
        scriptPath: resp.scriptPath,
        name: resp.name,
        scriptSourceUrl: resp.sourceUrl || url,
      }));
      setScriptURL("");
      setTestResult(null);
      show("脚本已导入，保存后可随时从原链接更新", "success");
    } catch (e) {
      show(e instanceof Error ? e.message : "导入失败", "error");
    } finally {
      setImporting(false);
    }
  }

  async function updateFromSource() {
    const url = form.scriptSourceUrl.trim();
    if (!url) return;
    setImporting(true);
    try {
      const resp = await api.importCrawlerScriptURL(url);
      setForm((prev) => ({
        ...prev,
        scriptPath: resp.scriptPath,
        name: resp.name,
        scriptSourceUrl: resp.sourceUrl || url,
      }));
      setTestResult(null);
      setScriptUpdated(true);
      show("已从原链接拉取最新脚本", "success");
    } catch (e) {
      show(e instanceof Error ? e.message : "从原链接更新失败", "error");
    } finally {
      setImporting(false);
    }
  }

  async function test() {
    const scriptPath = form.scriptPath.trim();
    if (!scriptPath) {
      show("请先导入爬虫脚本", "error");
      return;
    }
    setTesting(true);
    setTestResult(null);
    try {
      const result = await api.testCrawlerScript({ scriptPath, proxy: form.proxy.trim() });
      setTestResult(result);
      if (result.ok) {
        show("测试通过", "success");
      } else {
        show(crawlerTestFailure(result) || "测试失败", "error");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "测试失败", "error");
    } finally {
      setTesting(false);
    }
  }

  async function save() {
    if (!form.scriptPath.trim()) {
      show("请先导入爬虫脚本", "error");
      return;
    }
    const target = form.targetNew.trim();
    if (target && (!/^\d+$/.test(target) || Number(target) < 1)) {
      show("每次新增视频数需为正整数", "error");
      return;
    }
    setSaving(true);
    try {
      const resp = await api.upsertCrawler({
        id: crawler?.id,
        scriptPath: form.scriptPath.trim(),
        scriptSourceUrl: form.scriptSourceUrl.trim(),
        targetNew: target,
        proxy: form.proxy.trim(),
        uploadProxy: form.uploadProxy.trim(),
        uploadDriveId: form.uploadDriveId,
      });
      if (resp.warning) {
        show(`已保存，但初始化失败：${resp.warning}`, "error");
      } else if (resp.deferred) {
        show(resp.message || "已保存，将在当前爬虫任务结束后生效", "success");
      } else {
        show("已保存并生效", "success");
      }
      onSaved();
    } catch (e) {
      show(e instanceof Error ? e.message : "保存失败", "error");
    } finally {
      setSaving(false);
    }
  }

  function onDrop(e: DragEvent<HTMLDivElement>) {
    e.preventDefault();
    setDragOver(false);
    if (importing) return;
    importFile(e.dataTransfer.files?.[0]);
  }

  const footerNote = !form.scriptPath
    ? null
    : testResult?.ok
      ? { text: "测试通过", tone: "is-ok" }
      : testResult
        ? { text: "测试未通过，建议检查脚本", tone: "is-error" }
        : scriptChanged || scriptUpdated
          ? { text: "建议先运行测试再保存", tone: "" }
          : null;

  return (
    <Modal
      open={open}
      title={isEdit ? (crawler?.name ?? "编辑爬虫") : "添加爬虫"}
      onClose={onClose}
      className="admin-modal--crawler"
      footer={
        <>
          {footerNote && <span className={`admin-modal__footer-note ${footerNote.tone}`}>{footerNote.text}</span>}
          <button type="button" className="admin-btn" onClick={onClose} disabled={saving}>
            取消
          </button>
          <button type="button" className="admin-btn is-primary" onClick={save} disabled={saving || !form.scriptPath}>
            {saving ? "保存中..." : "保存"}
          </button>
        </>
      }
    >
      <div className="admin-crawler-editor">
        <div className="admin-crawler-editor__summary" aria-label="爬虫配置状态">
          <CrawlerEditorSummaryItem
            label="脚本"
            value={form.scriptPath ? form.name || "已导入" : "未导入"}
            tone={form.scriptPath ? "ok" : "muted"}
            icon={<FileCode2 size={13} />}
          />
          <CrawlerEditorSummaryItem
            label="测试"
            value={testResult?.ok ? "已通过" : testResult ? "未通过" : "未测试"}
            tone={testResult?.ok ? "ok" : testResult ? "error" : "muted"}
            icon={<TestTube size={13} />}
          />
          <CrawlerEditorSummaryItem
            label="每轮新增"
            value={`${form.targetNew.trim() || "10"} 条`}
            tone="muted"
            icon={<Download size={13} />}
          />
        </div>

        <div className="admin-crawler-editor__grid">
          <section className="admin-crawler-panel admin-crawler-panel--script">
            <header className="admin-crawler-panel__head">
              <div>
                <strong>维护脚本</strong>
              </div>
            </header>

            <input
              ref={fileInputRef}
              type="file"
              accept=".py,text/x-python"
              hidden
              onChange={(e) => {
                importFile(e.target.files?.[0]);
                e.currentTarget.value = "";
              }}
            />

            {isEdit && form.scriptPath && (
              <div className={`admin-crawler-current-script${isEdit && scriptChanged ? " is-replaced" : ""}`}>
                <div className="admin-crawler-current-script__actions">
                  {replacingScript ? (
                    <button type="button" className="admin-btn" onClick={cancelReplace} disabled={importing}>
                      取消替换
                    </button>
                  ) : (
                    <>
                      {form.scriptSourceUrl && (
                        <button
                          type="button"
                          className="admin-btn"
                          onClick={updateFromSource}
                          disabled={importing}
                          title={`从 ${form.scriptSourceUrl} 重新拉取脚本`}
                        >
                          {importing ? "更新中..." : "从原链接更新"}
                        </button>
                      )}
                      <button
                        type="button"
                        className="admin-btn"
                        onClick={() => {
                          setScriptURL(form.scriptSourceUrl);
                          setReplacingScript(true);
                        }}
                      >
                        替换脚本文件
                      </button>
                    </>
                  )}
                </div>
              </div>
            )}

            {showImportArea && (
              <div className="admin-crawler-import-box">
                <div className="admin-crawler-local-import">
                  <span>本地导入</span>
                  <div
                    className={`admin-crawler-dropzone${dragOver ? " is-dragover" : ""}${importing ? " is-busy" : ""}`}
                    role="button"
                    tabIndex={0}
                    onClick={() => !importing && fileInputRef.current?.click()}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        if (!importing) fileInputRef.current?.click();
                      }
                    }}
                    onDragOver={(e) => {
                      e.preventDefault();
                      setDragOver(true);
                    }}
                    onDragLeave={() => setDragOver(false)}
                    onDrop={onDrop}
                  >
                    <Upload size={20} />
                    <strong>{importing ? "导入中..." : "上传 .py 脚本"}</strong>
                  </div>
                </div>

                <div className="admin-crawler-link-import">
                  <label htmlFor="crawler-script-url">链接导入</label>
                  <div className="admin-crawler-urlrow">
                    <input
                      id="crawler-script-url"
                      value={scriptURL}
                      onChange={(e) => setScriptURL(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") {
                          e.preventDefault();
                          importURL();
                        }
                      }}
                      disabled={importing}
                    />
                    <button className="admin-btn" type="button" onClick={importURL} disabled={importing}>
                      导入
                    </button>
                  </div>
                </div>
              </div>
            )}
          </section>

          <div className="admin-crawler-editor__side">
            <section className="admin-crawler-panel">
              <header className="admin-crawler-panel__head">
                <div>
                  <strong>测试脚本</strong>
                </div>
              </header>
              <button className="admin-btn" type="button" onClick={test} disabled={!form.scriptPath || importing || testing}>
                {testing ? "测试中..." : testResult ? "重新测试" : "运行测试"}
              </button>
              {testResult && <CrawlerTestResult result={testResult} />}
            </section>

            <section className="admin-crawler-panel">
              <header className="admin-crawler-panel__head">
                <div>
                  <strong>配置参数</strong>
                </div>
              </header>
              <div className="admin-crawler-params">
                <div className="admin-form__row">
                  <label htmlFor="crawler-target">每次新增视频数</label>
                  <input
                    id="crawler-target"
                    type="number"
                    min={1}
                    value={form.targetNew}
                    onChange={(e) => set("targetNew", e.target.value)}
                    placeholder="10"
                  />
                </div>
                <div className="admin-form__row">
                  <label htmlFor="crawler-proxy">抓取代理</label>
                  <input
                    id="crawler-proxy"
                    value={form.proxy}
                    onChange={(e) => {
                      set("proxy", e.target.value);
                      setTestResult(null);
                    }}
                    placeholder="支持http或socks5代理"
                  />
                </div>
                <CrawlerUploadTargetField
                  value={form.uploadDriveId}
                  onChange={(value) => set("uploadDriveId", value)}
                  uploadTargets={uploadTargets}
                />
                <div className="admin-form__row">
                  <label htmlFor="crawler-upload-proxy">上传代理</label>
                  <input
                    id="crawler-upload-proxy"
                    value={form.uploadProxy}
                    onChange={(e) => set("uploadProxy", e.target.value)}
                    placeholder="仅本地视频上传到网盘时使用"
                  />
                </div>
              </div>
            </section>
          </div>
        </div>
      </div>
    </Modal>
  );
}

function CrawlerEditorSummaryItem({
  label,
  value,
  tone,
  icon,
}: {
  label: string;
  value: string;
  tone: "ok" | "error" | "info" | "muted";
  icon: ReactNode;
}) {
  return (
    <div className={`admin-crawler-editor-status is-${tone}`}>
      <span className="admin-crawler-editor-status__icon">{tone === "ok" ? <Check size={13} /> : icon}</span>
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function CrawlerTestResult({ result }: { result: api.CrawlerDryRunResult }) {
  const item = result.items[0];
  const failure = crawlerTestFailure(result);
  const media = result.mediaCheck;

  return (
    <div className={`admin-crawler-test-result ${result.ok ? "is-ok" : "is-error"}`}>
      <div className="admin-crawler-test-result__head">
        <span className={`admin-status is-${result.ok ? "ok" : "error"}`}>{result.ok ? "测试通过" : "测试失败"}</span>
        <span>抓取到 {result.items.length} 条视频</span>
        {result.durationMs > 0 && <span>{Math.round(result.durationMs / 1000)} 秒</span>}
      </div>

      {failure && <div className="admin-crawler-test-result__error">{failure}</div>}

      {item && (
        <div className="admin-crawler-test-result__grid">
          <CrawlerTestField label="视频名" value={item.title} />
          <CrawlerTestField label="唯一标识" value={item.sourceId} />
          <CrawlerTestField label="视频直链" value={item.mediaUrl || item.mediaLocalFile} />
          <CrawlerTestField label="封面图" value={item.thumbnailUrl} />
          <CrawlerTestField label="详情页" value={item.detailUrl} />
        </div>
      )}

      {media && (
        <div className="admin-crawler-test-result__media">
          <span>直链校验</span>
          <strong>
            {media.ok ? "可访问" : "不可访问"}
            {media.status ? ` · HTTP ${media.status}` : ""}
            {media.contentType ? ` · ${media.contentType}` : ""}
            {media.contentLengthBytes ? ` · ${formatBytes(media.contentLengthBytes)}` : ""}
          </strong>
        </div>
      )}

      {result.log && result.log.length > 0 && (
        <details className="admin-crawler-test-result__log">
          <summary>脚本日志</summary>
          <pre>{result.log.join("\n")}</pre>
        </details>
      )}
    </div>
  );
}

function CrawlerTestField({ label, value }: { label: string; value?: string | number }) {
  if (value === undefined || value === "") return null;
  return (
    <div className="admin-crawler-test-result__field">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function crawlerTestFailure(result: api.CrawlerDryRunResult) {
  return result.error || result.mediaCheck?.error || "";
}

function formatLastCrawl(ts?: number) {
  if (!ts) return "从未";
  return new Date(ts * 1000).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "";
  if (bytes >= 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GB`;
  if (bytes >= 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${bytes} B`;
}
