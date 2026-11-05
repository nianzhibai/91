import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import {
  ArrowLeft,
  Ban,
  ChevronRight,
  FolderTree,
  HardDrive,
  Loader2,
  Plus,
  RefreshCw,
  Search,
} from "lucide-react";
import * as api from "./api";
import { AdminPageActions } from "./AdminPageActions";
import { useToast } from "@/components/ToastContext";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import { formatBytes } from "./storageFormat";
import { makeUniqueDriveId } from "./driveId";
import {
  FormState,
  driveKindAbbr,
  driveKindIconPath,
  emptyForm,
  scanAllButtonText,
  maintenanceBusyText,
  usesRootDirectoryID,
  defaultRootId,
  credentialFields,
  rootDirectoryLabel,
} from "./drive/constants";
import {
  StatusTag,
  DriveCardMetrics,
  DriveGenerationPanel,
} from "./drive/DriveComponents";
import { StorageSummary } from "./drive/StorageSummary";
import { DriveDetailLoading, DriveListSkeleton } from "./DrivesPageLoading";
import { DriveForm } from "./drive/DriveForm";
import { DriveDetailUnavailable } from "./drive/DriveDetailUnavailable";
import {
  changedCredentialValues,
  driveCredentialsForForm,
  driveCredentialError,
} from "./drive/credentials";
import { DeleteDriveModal } from "./drive/DeleteDriveModal";
import { SkipDirsPanel } from "./drive/SkipDirsPanel";
import { ScanResultDetails } from "./drive/ScanResultDetails";
import { isGenerationBusy } from "./drive/scanResults";
import { AdminEmptyVisual } from "./AdminEmptyVisual";
import { useAdminFloatingActionSpace } from "./useAdminFloatingActionSpace";
import { useAdminRouteActive } from "./AdminRouteCache";
import { useDriveDetailData } from "./drive/useDriveDetailData";
import { useDriveListData } from "./drive/useDriveListData";
import { useAuth } from "./AuthContext";
import { useNavigate } from "react-router";

const DRIVE_BUSY_MESSAGE = "当前存储有正在进行的任务，请稍后重试";
const MAINTENANCE_BUSY_MESSAGE = "当前有全量扫描任务正在进行，请稍后重试";

function isDriveBusy(d: api.AdminDrive) {
  return [
    d.scanGenerationStatus,
    d.thumbnailGenerationStatus,
    d.previewGenerationStatus,
    d.fingerprintGenerationStatus,
  ].some((status) => {
    const state = status?.state || "idle";
    return isGenerationBusy(state);
  });
}

export function DrivesPage() {
  const floatingActionPageRef = useAdminFloatingActionSpace<HTMLElement>();
  const routeActive = useAdminRouteActive();
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedDriveId = searchParams.get("drive") || null;
  const listData = useDriveListData(routeActive && !selectedDriveId);
  const detail = useDriveDetailData(selectedDriveId, routeActive);
  const { list, storage, setMaintenanceStatus } = listData;
  const maintenanceStatus = (selectedDriveId ? detail.resources.runtime.data?.maintenanceStatus : null) ?? listData.maintenanceStatus;
  const loading = selectedDriveId ? detail.loading : listData.loading;
  const loadError = selectedDriveId ? detail.resources.config.error : listData.loadError;
  const { invalidateSession } = useAuth();
  const navigate = useNavigate();
  useEffect(() => {
    if (detail.unauthorized || listData.unauthorized) { invalidateSession(); navigate("/login", { replace: true }); }
  }, [detail.unauthorized, listData.unauthorized, invalidateSession, navigate]);
  const [modalOpen, setModalOpen] = useState(false);
  const [discardConfirmOpen, setDiscardConfirmOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<api.AdminDrive | null>(null);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [initialForm, setInitialForm] = useState<FormState>(emptyForm);
  const [createDriveTypeSelected, setCreateDriveTypeSelected] = useState(false);
  const [saving, setSaving] = useState(false);
  const [editingCredentialsId, setEditingCredentialsId] = useState("");
  const [deletingId, setDeletingId] = useState("");
  const [scanningAll, setScanningAll] = useState(false);
  const [stoppingAll, setStoppingAll] = useState(false);
  const [scanningDriveIds, setScanningDriveIds] = useState<Record<string, boolean>>({});
  const scanningDriveIdsRef = useRef(new Set<string>());
  const [stoppingDriveId, setStoppingDriveId] = useState("");
  const { show } = useToast();
  const maintenanceBusy = scanningAll || maintenanceStatus.running || maintenanceStatus.queued;
  const formDirty = form.id
    ? !sameForm(form, initialForm)
    : hasCreateFormChanges(form);

  function openDriveDetail(id: string) {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      next.set("drive", id);
      return next;
    });
  }

  function closeDriveDetail(options?: { replace?: boolean }) {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      next.delete("drive");
      return next;
    }, options);
  }

  function refresh() {
    return selectedDriveId ? detail.refresh() : listData.refresh();
  }

  function refreshDriveList() { return refresh(); }

  function openCreate() {
    const nextForm = { ...emptyForm };
    setForm(nextForm);
    setInitialForm(nextForm);
    setCreateDriveTypeSelected(false);
    setModalOpen(true);
  }

  async function openEdit(d: api.AdminDrive) {
    if (editingCredentialsId) return;
    setEditingCredentialsId(d.id);
    try {
      const result = await api.getDriveCredentials(d.id);
      const creds = driveCredentialsForForm(
        d.kind,
        result.credentials ?? {}
      );
      const nextForm: FormState = {
        id: d.id,
        kind: d.kind,
        name: d.name,
        rootId: d.rootId,
        creds,
      };
      setForm(nextForm);
      setInitialForm(nextForm);
      setModalOpen(true);
    } catch (e) {
      show(e instanceof Error ? e.message : "加载网盘凭证失败", "error");
    } finally {
      setEditingCredentialsId("");
    }
  }

  function requestCloseDriveModal() {
    if (saving) return;
    if (formDirty) {
      setDiscardConfirmOpen(true);
      return;
    }
    setModalOpen(false);
  }

  function discardDriveChanges() {
    setDiscardConfirmOpen(false);
    setModalOpen(false);
    setForm(initialForm);
  }

  function handleCreateFormChange(nextForm: FormState) {
    setForm(nextForm);
    if (!nextForm.id && !hasCreateFormChanges(nextForm)) {
      setInitialForm(nextForm);
    }
  }

  async function handleSave() {
    const name = form.name.trim();
    if (!form.kind) {
      show("请选择网盘类型", "error");
      return;
    }
    if (!name) {
      show("请填写网盘名称", "error");
      return;
    }
    const credentialError = driveCredentialError(form.kind, form.creds, !form.id);
    if (credentialError) {
      show(credentialError, "error");
      return;
    }
    if (!form.id) {
      const missingField = credentialFields(form.kind).find(
        (field) =>
          field.required &&
          !((form.creds[field.key] ?? field.defaultValue ?? "").trim())
      );
      if (missingField) {
        show(`请填写${missingField.label}`, "error");
        return;
      }
    }
    const editing = Boolean(form.id);
    const driveID = editing
      ? form.id
      : makeUniqueDriveId(form.kind, name, list);
    const editableCredentialKeys = credentialFields(form.kind).map((field) => field.key);
    // QR login writes a few values that do not have a standalone input field.
    if (form.kind === "p123") editableCredentialKeys.push("access_token");
    if (form.kind === "wopan") editableCredentialKeys.push("family_id");
    const credentials = editing
      ? changedCredentialValues(
          form.creds,
          initialForm.creds,
          editableCredentialKeys
        )
      : form.creds;
    const rootId = usesRootDirectoryID(form.kind)
      ? form.rootId.trim() || defaultRootId(form.kind)
      : defaultRootId(form.kind);
    setSaving(true);
    try {
      const resp = await api.upsertDrive({
        id: driveID,
        kind: form.kind,
        name,
        rootId,
        credentials,
      });

      if (resp.warning) {
        show(`已保存，但 driver 初始化失败：${resp.warning}`, "error");
      } else if (resp.deferred) {
        show(resp.message || "已保存，将在当前网盘任务结束后生效", "success");
      } else {
        show("已保存并生效", "success");
      }
      detail.accept(resp.snapshot);
      setModalOpen(false);
      setInitialForm(form);
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "保存失败", "error");
    } finally {
      setSaving(false);
    }
  }

  async function confirmDeleteDrive() {
    if (!deleteTarget) return;
    const d = deleteTarget;
    setDeletingId(d.id);
    try {
      const resp = await api.deleteDrive(d.id, { deleteVideos: true });
      show(`已删除，并清理 ${resp.deletedVideos ?? 0} 个视频`, "success");
      setDeleteTarget(null);
      if (selectedDriveId === d.id) {
        closeDriveDetail({ replace: true });
      }
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "删除失败", "error");
    } finally {
      setDeletingId("");
    }
  }

  async function handleRescan(d: api.AdminDrive) {
    if (maintenanceBusy) {
      show(maintenanceBusyText(maintenanceStatus) || MAINTENANCE_BUSY_MESSAGE, "info");
      return;
    }
    if (isDriveBusy(d) || scanningDriveIdsRef.current.has(d.id)) {
      show(DRIVE_BUSY_MESSAGE, "info");
      return;
    }
    scanningDriveIdsRef.current.add(d.id);
    setScanningDriveIds((prev) => ({ ...prev, [d.id]: true }));
    try {
      const resp = await api.rescan(d.id);
      detail.accept(resp.snapshot);
      if (!resp.accepted) {
        if (resp.status) {
          setMaintenanceStatus(resp.status);
        }
        show(resp.message || DRIVE_BUSY_MESSAGE, "info");
        refreshDriveList();
        return;
      }
      show("已触发扫描，可稍后刷新视频列表查看", "success");
      refreshDriveList();
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      scanningDriveIdsRef.current.delete(d.id);
      setScanningDriveIds((prev) => {
        const next = { ...prev };
        delete next[d.id];
        return next;
      });
    }
  }

  async function handleScanAll() {
    if (maintenanceBusy) {
      show(maintenanceBusyText(maintenanceStatus) || MAINTENANCE_BUSY_MESSAGE, "info");
      return;
    }
    setScanningAll(true);
    try {
      const resp = await api.runScanAllJob();
      setMaintenanceStatus(resp.status);
      if (resp.accepted) {
        show("已触发全部网盘扫描，完成新视频处理后将执行视频去重", "success");
      } else {
        show(resp.message || MAINTENANCE_BUSY_MESSAGE, "info");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setScanningAll(false);
    }
  }

  async function handleStopAllTasks() {
    if (stoppingAll) return;
    setStoppingAll(true);
    try {
      const resp = await api.stopAllTasks();
      setMaintenanceStatus(resp.status);
      show(
        resp.stoppedDrives > 0
          ? `已停止 ${resp.stoppedDrives} 个网盘的当前任务`
          : "没有正在运行的网盘任务",
        "success"
      );
      refreshDriveList();
    } catch (e) {
      show(e instanceof Error ? e.message : "停止失败", "error");
    } finally {
      setStoppingAll(false);
    }
  }

  async function handleStopDriveTasks(d: api.AdminDrive) {
    if (stoppingDriveId) return;
    setStoppingDriveId(d.id);
    try {
      const resp = await api.stopDriveTasks(d.id);
      detail.accept(resp.snapshot);
      show(
        resp.stopped
          ? `已停止「${d.name || d.id}」的当前任务`
          : `「${d.name || d.id}」没有正在运行的任务`,
        "success"
      );
      refreshDriveList();
    } catch (e) {
      show(e instanceof Error ? e.message : "停止失败", "error");
    } finally {
      setStoppingDriveId("");
    }
  }

  const selectedDrive = detail.drive;

  if (selectedDriveId && !selectedDrive) {
    if (loading) {
      return (
        <DriveDetailLoading
          driveId={selectedDriveId}
          onBack={() => closeDriveDetail({ replace: true })}
        />
      );
    }

    return (
      <DriveDetailUnavailable
        notFound={detail.notFound}
        retrying={detail.resources.config.loading}
        onBack={() => closeDriveDetail({ replace: true })}
        onRetry={() => void detail.refresh()}
      />
    );
  }

  // --- Detail view ---
  if (selectedDriveId && selectedDrive) {
    const d = selectedDrive;
    const driveStorage = detail.storage;

    return (
      <section className="admin-page admin-drives-page">
        <header className="admin-drive-detail__header-bar">
          <button
            type="button"
            className="admin-drive-detail__back-btn"
            onClick={() => closeDriveDetail({ replace: true })}
            title="返回网盘列表"
          >
            <ArrowLeft size={16} />
          </button>
          <div className="admin-drive-detail__title-wrap">
            <h1 className="admin-drive-detail__title">{d.name || d.id}</h1>
          </div>
        </header>

        {detail.connection === "reconnecting" && (
          <div className="admin-detail-error" role="status">连接中断，正在重连；当前显示上次更新的数据。</div>
        )}
        {detail.resources.config.error && (
          <div className="admin-detail-error" role="status">
            网盘信息更新失败：{detail.resources.config.error}
            <button type="button" className="admin-btn" onClick={() => void detail.refresh(["config"])}>重试</button>
          </div>
        )}
        <div className="admin-drive-detail-layout">
          <div className="admin-drive-detail-layout__info">
            <div className="admin-detail-card">
              <header className="admin-detail-card__title">
                <div className="admin-detail-card__title-left">
                  <HardDrive size={16} />
                  <span>基本信息</span>
                </div>
              </header>

              <div className="admin-detail-grid">
                <div className="admin-detail-row">
                  <span className="admin-detail-label">网盘 ID</span>
                  <span className="admin-detail-value admin-mono-cell">{d.id}</span>
                </div>
                {usesRootDirectoryID(d.kind) && (
                  <div className="admin-detail-row">
                    <span className="admin-detail-label">{rootDirectoryLabel(d.kind)}</span>
                    <span className="admin-detail-value admin-mono-cell">{d.rootId}</span>
                  </div>
                )}
              </div>
              {d.lastError && (
                <div className="admin-detail-error">{d.lastError}</div>
              )}

              <div className="admin-detail-actions">
                <div className="admin-task-controls" aria-label="当前网盘任务控制">
                  <button
                    type="button"
                    className="admin-btn"
                    onClick={() => handleRescan(d)}
                    aria-disabled={maintenanceBusy || isDriveBusy(d) || !!scanningDriveIds[d.id]}
                    title={
                      maintenanceBusy
                        ? maintenanceBusyText(maintenanceStatus) || MAINTENANCE_BUSY_MESSAGE
                        : isDriveBusy(d) || scanningDriveIds[d.id]
                        ? DRIVE_BUSY_MESSAGE
                        : undefined
                    }
                  >
                    {scanningDriveIds[d.id] ? "触发中..." : "开始扫盘"}
                  </button>
                  <button
                    type="button"
                    className="admin-btn"
                    onClick={() => handleStopDriveTasks(d)}
                    disabled={!!stoppingDriveId}
                    title="停止此网盘当前的扫描、封面、预览视频和视频指纹生成任务。"
                  >
                    {stoppingDriveId === d.id ? "停止中..." : "停止任务"}
                  </button>
                </div>
                <button
                  type="button"
                  className="admin-btn admin-detail-actions__credentials"
                  onClick={() => openEdit(d)}
                  disabled={!!editingCredentialsId}
                >
                  {editingCredentialsId === d.id ? (
                    <>
                      <Loader2 size={14} className="admin-spin" />
                      加载中
                    </>
                  ) : (
                    "编辑凭证"
                  )}
                </button>
                <button type="button" className="admin-btn admin-detail-actions__danger" onClick={() => setDeleteTarget(d)}>
                  删除网盘
                </button>
              </div>
            </div>
          </div>

          <div className="admin-drive-detail-layout__status">
            <DriveGenerationPanel
              d={d}
              runtimeLoading={!detail.resources.runtime.data && detail.resources.runtime.loading}
              countsLoading={!detail.resources.stats.data}
              onGenerationUpdated={() => void refresh()}
            />

            <ScanResultDetails
              result={d.scanGenerationStatus?.result}
              scanning={isGenerationBusy(d.scanGenerationStatus?.state ?? "idle")}
              loading={!detail.resources.runtime.data}
            />

            {(["runtime", "stats"] as const).map((resource) => detail.resources[resource].error && (
              <div className="admin-detail-error" role="status" key={resource}>
                {resource === "runtime" ? "任务状态" : "资源数量"}更新失败：{detail.resources[resource].error}
                <button type="button" className="admin-btn" onClick={() => void detail.refresh([resource])}>重试</button>
              </div>
            ))}

            <div className="admin-detail-card">
              <header className="admin-detail-card__title">
                <div className="admin-detail-card__title-left">
                  <FolderTree size={16} />
                  <span>本地存储占用</span>
                </div>
              </header>
              {detail.resources.storage.error && (
                <div className="admin-detail-error" role="status">
                  存储用量更新失败：{detail.resources.storage.error}
                  <button type="button" className="admin-btn" onClick={() => void detail.refresh(["storage"])}>重试</button>
                </div>
              )}
              <div className="admin-local-storage-metrics" aria-busy={detail.resources.storage.loading || undefined}>
                <div className="admin-local-storage-metric">
                  <span>封面</span>
                  <strong>{driveStorage ? formatBytes(driveStorage.thumbnailBytes) : "—"}</strong>
                </div>
                <div className="admin-local-storage-metric">
                  <span>预览视频</span>
                  <strong>{driveStorage ? formatBytes(driveStorage.teaserBytes) : "—"}</strong>
                </div>
                <div className="admin-local-storage-metric">
                  <span>合计</span>
                  <strong>{driveStorage ? formatBytes(driveStorage.totalBytes) : "—"}</strong>
                </div>
              </div>
            </div>
          </div>

          <div className="admin-drive-detail-layout__skip-dirs">
            <SkipDirsPanel
              key={d.id}
              drive={d}
              directoryVersion={detail.directoryVersion}
              onSaved={(saved) => {
                detail.accept(saved.snapshot);
                if (!saved.snapshot) void detail.refresh(["config"]);
              }}
            />
          </div>
        </div>

        <Modal
          open={modalOpen}
          title="编辑凭证"
          className="admin-modal--drive-form"
          onClose={requestCloseDriveModal}
          footer={
            <>
              <button type="button" className="admin-btn" onClick={requestCloseDriveModal}>
                取消
              </button>
              <button
                type="button"
                className="admin-btn is-primary"
                onClick={handleSave}
                disabled={saving}
              >
                {saving ? "保存中..." : "保存更改"}
              </button>
            </>
          }
        >
          <DriveForm
            form={form}
            onChange={setForm}
            isEdit={true}
          />
        </Modal>
        <DeleteDriveModal
          drive={deleteTarget}
          deleting={deletingId === deleteTarget?.id}
          onCancel={() => {
            if (!deletingId) {
              setDeleteTarget(null);
            }
          }}
          onConfirm={confirmDeleteDrive}
        />
        <ConfirmModal
          open={discardConfirmOpen}
          title="放弃未保存更改"
          message="当前网盘配置有未保存的更改，确定要放弃吗？"
          confirmText="放弃更改"
          danger
          centerMessage
          modalClassName="admin-modal--delete-confirm"
          onCancel={() => setDiscardConfirmOpen(false)}
          onConfirm={discardDriveChanges}
        />
      </section>
    );
  }

  // --- List view ---
  return (
    <section
      ref={floatingActionPageRef}
      className="admin-page admin-page--with-floating-actions admin-drives-page admin-drives-page--list"
    >
      <AdminPageActions>
        <div className="admin-page__actions admin-drive-list-actions">
          <div className="admin-task-controls" aria-label="所有网盘任务控制">
            <button
              type="button"
              className="admin-btn"
              onClick={handleScanAll}
              disabled={scanningAll}
              title={maintenanceBusyText(maintenanceStatus) || "扫描已配置的存储、处理新视频并执行视频去重"}
            >
              <Search size="1em" aria-hidden="true" />
              {scanAllButtonText(maintenanceStatus, scanningAll)}
            </button>
            <button
              type="button"
              className="admin-btn"
              onClick={handleStopAllTasks}
              disabled={stoppingAll}
              title="停止所有存储当前的扫描、封面、预览视频和视频指纹生成任务"
            >
              <Ban size="1em" aria-hidden="true" />
              {stoppingAll ? "停止中..." : "停止所有任务"}
            </button>
          </div>
        </div>
      </AdminPageActions>

      <StorageSummary storage={storage} loading={listData.storageLoading} />

      {[
        { title: "网盘列表", error: listData.listError, retry: listData.refreshList },
        { title: "存储用量", error: listData.storageError, retry: listData.refreshStorage },
        { title: "维护状态", error: listData.maintenanceError, retry: listData.refreshMaintenance },
      ].map(({ title, error, retry }) => error && (
        <div className="admin-detail-error" role="status" key={title}>
          {title}更新失败：{error}
          <button type="button" className="admin-btn" onClick={() => void retry()}>重试</button>
        </div>
      ))}
      {loading ? (
        <DriveListSkeleton />
      ) : loadError ? (
        <div className="admin-error-state">
          <strong>网盘数据加载失败</strong>
          <span>{loadError}</span>
          <button type="button" className="admin-btn" onClick={refresh}>
            <RefreshCw size={13} /> 重试
          </button>
        </div>
      ) : list.length > 0 ? (
        <div className="admin-drives-grid">
          {list.map((d) => {
            const iconSrc = driveKindIconPath(d.kind);
            return (
              <button
                type="button"
                key={d.id}
                className="admin-drive-card"
                onClick={() => openDriveDetail(d.id)}
                aria-label={`管理网盘 ${d.name || d.id}`}
              >
                <div className="admin-drive-card__header">
                  <div className="admin-drive-card__title">
                    <span
                      className={`admin-drive-card__brand-icon${iconSrc ? " has-image" : ""}`}
                      data-kind={d.kind}
                    >
                      {iconSrc ? (
                        <img
                          src={iconSrc}
                          alt=""
                          aria-hidden="true"
                          className="admin-drive-card__brand-icon-img"
                        />
                      ) : (
                        driveKindAbbr(d.kind)
                      )}
                    </span>
                    <span>{d.name || d.id}</span>
                  </div>
                  <StatusTag status={d.status} error={d.lastError} hasCred={d.hasCredential} />
                </div>

                <DriveCardMetrics d={d} />

                <div className="admin-drive-card__footer">
                  <span>本地占用: {formatBytes(storage?.drives[d.id]?.totalBytes ?? 0)}</span>
                  <span className="admin-drive-card__manage-link">
                    管理 <ChevronRight size={14} />
                  </span>
                </div>
              </button>
            );
          })}
        </div>
      ) : (
        <AdminEmptyVisual
          variant="empty"
          text="请先添加网盘"
          className="admin-drive-empty-state"
        />
      )}

      <button
        data-admin-floating-actions
        type="button"
        className="admin-btn admin-create-fab"
        onClick={openCreate}
      >
        <Plus size="1em" aria-hidden="true" />
        添加网盘
      </button>

      <Modal
        open={modalOpen}
        title={form.id && list.find((x) => x.id === form.id) ? "编辑凭证" : "添加网盘"}
        className="admin-modal--drive-form"
        onClose={requestCloseDriveModal}
        footer={form.id || createDriveTypeSelected ? (
          <>
            <button type="button" className="admin-btn" onClick={requestCloseDriveModal}>
              取消
            </button>
            <button
              type="button"
              className="admin-btn is-primary"
              onClick={handleSave}
              disabled={saving}
            >
              {saving ? "保存中..." : "保存"}
            </button>
          </>
        ) : undefined}
      >
        <DriveForm
          form={form}
          onChange={handleCreateFormChange}
          isEdit={!!list.find((x) => x.id === form.id)}
          onTypeSelected={setCreateDriveTypeSelected}
        />
      </Modal>
      <DeleteDriveModal
        drive={deleteTarget}
        deleting={deletingId === deleteTarget?.id}
        onCancel={() => {
          if (!deletingId) {
            setDeleteTarget(null);
          }
        }}
        onConfirm={confirmDeleteDrive}
      />
      <ConfirmModal
        open={discardConfirmOpen}
        title="放弃未保存更改"
        message="当前网盘配置有未保存的更改，确定要放弃吗？"
        confirmText="放弃更改"
        danger
        centerMessage
        modalClassName="admin-modal--delete-confirm"
        onCancel={() => setDiscardConfirmOpen(false)}
        onConfirm={discardDriveChanges}
      />
    </section>
  );
}

function sameForm(a: FormState, b: FormState): boolean {
  return (
    a.id === b.id &&
    a.kind === b.kind &&
    a.name === b.name &&
    a.rootId === b.rootId &&
    sameRecord(a.creds, b.creds)
  );
}

function sameRecord(a: Record<string, string>, b: Record<string, string>): boolean {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const key of keys) {
    if ((a[key] ?? "") !== (b[key] ?? "")) return false;
  }
  return true;
}

function hasCreateFormChanges(form: FormState): boolean {
  if (form.name.trim() !== "") return true;
  if (form.rootId.trim() !== "") return true;
  return Object.values(form.creds).some((value) => value.trim() !== "");
}
