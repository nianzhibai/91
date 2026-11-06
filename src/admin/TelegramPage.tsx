import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import {
  CheckCircle2,
  CircleAlert,
  Circle,
  Loader2,
  ArrowUpRight,
  FileVideo,
  History,
  Radio,
} from "lucide-react";
import * as api from "./api";
import { AdminPagination } from "./AdminPagination";
import { useAdminRouteActive } from "./AdminRouteCache";
import { useToast } from "@/components/ToastContext";
import { formatBytes } from "./storageFormat";
import { importStageLabel } from "./telegram/config";
import { useTelegramAvailability } from "./telegram/useTelegramAvailability";
import { useTelegramStatus } from "./telegram/TelegramStatusProvider";
import { useAdminResource } from "./useAdminResource";
import { useAuth } from "./AuthContext";
import { TelegramUploadSettings } from "./telegram/TelegramUploadSettings";
import "@/styles/telegram.css";

const RECENT_IMPORT_LIMIT = 32;
const IMPORT_PAGE_SIZE = 8;
const activeImportStates = new Set([
  "queued",
  "downloading",
  "validating",
  "saving",
]);

const stateLabels: Record<string, string> = {
  disabled: "未启用",
  connecting: "连接中",
  connected: "连接正常",
  error: "连接异常",
  needs_reconnect: "等待恢复接收",
};
const dateLabel = (value?: string) =>
  !value || value.startsWith("0001-")
    ? "尚无记录"
    : new Date(value).toLocaleString();

export function TelegramPage() {
  const active = useAdminRouteActive();
  const { enabled, error } = useTelegramAvailability();
  if (enabled === false) {
    return active ? (
      <Navigate to="/admin/settings?section=telegram" replace />
    ) : null;
  }
  if (enabled === null) {
    if (!error) return null;
    return (
      <div className="admin-page telegram-page">
        <p className="tg-error" role="alert">
          {error}
        </p>
        <Link to="/admin/settings?section=telegram">前往 Telegram 配置</Link>
      </div>
    );
  }
  return <TelegramWorkspace />;
}

function TelegramWorkspace() {
  const active = useAdminRouteActive();
  const { show } = useToast();
  const { invalidateSession } = useAuth();
  const statusResource = useTelegramStatus();
  const jobsResource = useAdminResource((signal) => api.listTelegramImports(RECENT_IMPORT_LIMIT, signal), {
    queryKey: "telegram-imports", active, initialData: [], onUnauthorized: invalidateSession,
    intervalMs: (items) => items.some((job) => activeImportStates.has(job.state)) ? 5000 : 15_000,
  });
  const status = statusResource.data;
  const jobs = jobsResource.data.slice(0, RECENT_IMPORT_LIMIT);
  const loading = jobsResource.loading;
  const error = jobsResource.error;
  const [filter, setFilter] = useState("");
  const [page, setPage] = useState(1);
  const [busy, setBusy] = useState("");

  const filteredJobs = jobs.filter(
    (job) =>
      !filter ||
      (filter === "active"
        ? activeImportStates.has(job.state)
        : job.state === filter),
  );
  const totalPages = Math.max(1, Math.ceil(filteredJobs.length / IMPORT_PAGE_SIZE));
  const currentPage = Math.min(page, totalPages);
  const pageStart = (currentPage - 1) * IMPORT_PAGE_SIZE;
  const pagedJobs = filteredJobs.slice(pageStart, pageStart + IMPORT_PAGE_SIZE);

  useEffect(() => {
    setPage((current) => Math.min(current, totalPages));
  }, [totalPages]);

  async function run(name: string, action: () => Promise<void>, target: "status" | "jobs" = "status") {
    setBusy(name);
    try {
      await action();
      await (target === "jobs" ? jobsResource : statusResource).invalidate();
    } catch (err) {
      show(err instanceof Error ? err.message : "操作失败", "error");
    } finally {
      setBusy("");
    }
  }
  const connection = status?.connection;
  const connectionError =
    !!connection &&
    ["error", "conflict", "needs_reconnect"].includes(connection.state);
  const connecting = !connection || connection.state === "connecting";
  const StatusIcon =
    connection?.state === "connected"
      ? CheckCircle2
      : connectionError
        ? CircleAlert
        : connecting
          ? Loader2
          : Circle;

  return (
    <div className="admin-page telegram-page">
      {[statusResource, jobsResource].map((resource, index) => resource.error && (
        <div className="tg-actions" key={index}>
          <p className="tg-error" role="alert">{resource.error}</p>
          <button type="button" className="admin-btn" onClick={() => void resource.refresh()}>重试</button>
        </div>
      ))}
      <div className="tg-overview">
        <section className="tg-panel" aria-labelledby="tg-connection">
          <div className="tg-heading">
            <h3 id="tg-connection">
              <Radio size={16} aria-hidden="true" />连接状态
            </h3>
            <button
              type="button"
              className="admin-btn"
              disabled={!!busy || !connection?.enabled}
              onClick={() =>
                void run("test", async () => {
                  const result = await api.testTelegram();
                  show(`已连接 @${result.username}`, "success");
                })
              }
            >
              {busy === "test" && <Loader2 size={14} className="tg-spin" />}
              测试连接
            </button>
          </div>
          <p
            role="status"
            className={`tg-status ${connection?.state === "connected" ? "is-connected" : connectionError ? "is-error" : ""}`}
          >
            <StatusIcon
              size={17}
              className={connecting ? "tg-spin" : undefined}
              aria-hidden="true"
            />
            {connection
              ? (stateLabels[connection.state] ?? connection.state)
              : "正在读取状态…"}
            {connection?.username && <span>@{connection.username}</span>}
          </p>
          <dl className="tg-connection-metrics">
            <div>
              <dt>最近连接</dt>
              <dd>{dateLabel(connection?.lastConnected)}</dd>
            </div>
            <div>
              <dt>最近消息</dt>
              <dd>{dateLabel(connection?.lastMessage)}</dd>
            </div>
            <div>
              <dt>存储可用</dt>
              <dd>
                {connection ? formatBytes(connection.cacheAvailableBytes) : "—"}
              </dd>
            </div>
          </dl>
          {!!connection?.notificationFailures && (
            <p className="tg-note">
              有 {connection.notificationFailures}{" "}
              条结果通知发送失败，视频保存结果不受影响。
            </p>
          )}
          {connection?.state === "needs_reconnect" && (
            <div className="tg-actions tg-panel-footer">
              <button
                className="admin-btn"
                disabled={!!busy}
                onClick={() =>
                  void run("resume", async () => {
                    await api.resumeTelegram();
                    show("已恢复接收", "success");
                  })
                }
              >
                恢复接收
              </button>
            </div>
          )}
        </section>
        <TelegramUploadSettings />
      </div>
      <section className="tg-panel" aria-labelledby="tg-jobs">
        <div className="tg-heading">
          <h3 id="tg-jobs">
            <History size={16} aria-hidden="true" />最近导入
          </h3>
          <select
            aria-label="任务状态"
            value={filter}
            onChange={(e) => {
              setFilter(e.target.value);
              setPage(1);
            }}
          >
            <option value="">全部状态</option>
            {Object.entries({
              active: "处理中",
              completed: "已保存",
              failed: "失败",
            }).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </div>
        {loading ? (
          <p className="tg-empty" role="status">
            正在加载记录…
          </p>
        ) : (
          !filteredJobs.length &&
          !error && (
            <div className="tg-empty">
              <p>{filter ? "暂无记录" : "暂无导入记录"}</p>
            </div>
          )
        )}
        <div className="tg-jobs" hidden={loading}>
          {pagedJobs.map((job) => (
            <article key={job.id} className="tg-job">
              <div className="tg-job-icon" aria-hidden="true">
                <FileVideo size={20} />
              </div>
              <div className="tg-job-content">
                <h4 className="tg-job-title">{job.title || "未命名视频"}</h4>
                <div className="tg-metrics">
                  <span>{formatBytes(job.totalBytes)}</span>
                  <span>{dateLabel(job.createdAt)}</span>
                  {job.senderId && <span>用户{job.senderId}</span>}
                  {job.retryCount > 0 && <span>已重试 {job.retryCount} 次</span>}
                </div>
                {job.error && <p className="tg-error">{job.error}</p>}
              </div>
              <span className={`tg-job-status tg-job-status--${job.state}`}>
                {importStageLabel(job)}
              </span>
              {(job.videoHref || job.canCancel || job.canRetry) && (
                <div className="tg-actions tg-job-actions">
                  {job.videoHref && (
                    <Link className="admin-btn" to={job.videoHref}>
                      打开视频<ArrowUpRight size={14} aria-hidden="true" />
                    </Link>
                  )}
                  {job.canCancel && (
                    <button
                      className="admin-btn"
                      disabled={!!busy}
                      onClick={() =>
                        void run(job.id, async () => {
                          await api.cancelImport(job.id);
                        }, "jobs")
                      }
                    >
                      取消
                    </button>
                  )}
                  {job.canRetry && (
                    <button
                      className="admin-btn"
                      disabled={!!busy}
                      onClick={() =>
                        void run(job.id, async () => {
                          await api.retryImport(job.id);
                        }, "jobs")
                      }
                    >
                      重试
                    </button>
                  )}
                </div>
              )}
            </article>
          ))}
        </div>
      </section>
      {totalPages > 1 && (
        <AdminPagination
          page={currentPage}
          totalPages={totalPages}
          total={filteredJobs.length}
          itemLabel="记录"
          pending={loading}
          onPage={setPage}
        />
      )}
    </div>
  );
}
