import { Activity } from "lucide-react";
import * as api from "../api";
import {
  generationStateLabel,
  generationStateClass,
  generationDetail,
  generationTitle,
} from "./constants";

export function GenerationCounts({
  ready,
  pending,
  failed,
  durationPending,
}: {
  ready?: number;
  pending?: number;
  failed?: number;
  durationPending?: number;
}) {
  return (
    <div className="admin-generation-counts">
      <span className="admin-drive-teaser__metric is-ready">
        就绪 {ready ?? 0}
      </span>
      <span className="admin-drive-teaser__metric is-pending">
        待生成 {pending ?? 0}
      </span>
      <span className="admin-drive-teaser__metric is-failed">
        失败 {failed ?? 0}
      </span>
      {(durationPending ?? 0) > 0 && (
        <span className="admin-drive-teaser__metric">
          待补时长 {durationPending}
        </span>
      )}
    </div>
  );
}

export function GenerationStatusLine({
  label,
  status,
}: {
  label: string;
  status?: api.DriveGenerationStatus;
}) {
  const state = status?.state || "idle";
  const queueLength = status?.queueLength ?? 0;
  const detail = generationDetail(status);
  const title = generationTitle(status, detail);
  const countText = queueLength > 0 ? `${label === "封面" ? "待处理" : "队列"} ${queueLength}` : "";

  return (
    <div className="admin-generation-row" title={title}>
      <span className="admin-generation-kind">{label}</span>
      <span className={`admin-status admin-generation-state is-${generationStateClass(state)}`}>
        {generationStateLabel(state)}
      </span>
      {(detail || queueLength > 0) && (
        <span className="admin-generation-detail">
          {[detail, countText].filter(Boolean).join(" / ")}
        </span>
      )}
    </div>
  );
}

export function StatusTag({
  status,
  error,
  hasCred,
}: {
  status: string;
  error?: string;
  hasCred: boolean;
}) {
  if (!hasCred) {
    return <span className="admin-status is-pending">未配置凭证</span>;
  }
  if (status === "ok") {
    return <span className="admin-status is-ok">已连接</span>;
  }
  if (status === "error")
    return (
      <span className="admin-status is-error" title={error}>
        错误
      </span>
    );
  return <span className="admin-status">{status || "未连接"}</span>;
}

export function DriveCardMetrics({ d }: { d: api.AdminDrive }) {
  return (
    <div className="admin-drive-card__info">
      <div className="admin-drive-card__metric">
        <span>封面数 (就绪/失败)</span>
        <strong>
          {d.thumbnailReadyCount ?? 0}
          <span style={{ fontSize: "11px", fontWeight: "normal", color: "var(--text-faint)" }}>
            {" "}/ {d.thumbnailFailedCount ?? 0}
          </span>
        </strong>
      </div>
      <div className="admin-drive-card__metric">
        <span>预览视频数 (就绪/失败)</span>
        <strong>
          {d.teaserReadyCount ?? 0}
          <span style={{ fontSize: "11px", fontWeight: "normal", color: "var(--text-faint)" }}>
            {" "}/ {d.teaserFailedCount ?? 0}
          </span>
        </strong>
      </div>
      <div className="admin-drive-card__metric">
        <span>视频指纹数 (就绪/失败)</span>
        <strong>
          {d.fingerprintReadyCount ?? 0}
          <span style={{ fontSize: "11px", fontWeight: "normal", color: "var(--text-faint)" }}>
            {" "}/ {d.fingerprintFailedCount ?? 0}
          </span>
        </strong>
      </div>
    </div>
  );
}

export function DriveGenerationPanel({
  d,
  regenFailedId,
  regenFailedThumbId,
  regenFailedFingerprintId,
  togglingTeaserId,
  togglingTranscodeId,
  onToggleTeaser,
  onRegenFailed,
  onRegenFailedThumbnails,
  onRegenFailedFingerprints,
  onStartTranscode,
  onStopTranscode,
}: {
  d: api.AdminDrive;
  regenFailedId: string;
  regenFailedThumbId: string;
  regenFailedFingerprintId: string;
  togglingTeaserId: string;
  togglingTranscodeId: string;
  onToggleTeaser: () => void;
  onRegenFailed: () => void;
  onRegenFailedThumbnails: () => void;
  onRegenFailedFingerprints: () => void;
  onStartTranscode: () => void;
  onStopTranscode: () => void;
}) {
  const canQueueThumbnails =
    (d.thumbnailFailedCount ?? 0) > 0 ||
    (d.thumbnailPendingCount ?? 0) > 0 ||
    (d.thumbnailDurationPendingCount ?? 0) > 0;
  const canQueuePreviews =
    (d.teaserFailedCount ?? 0) > 0 || (d.teaserPendingCount ?? 0) > 0;
  const canQueueFingerprints =
    (d.fingerprintFailedCount ?? 0) > 0 || (d.fingerprintPendingCount ?? 0) > 0;
  // 转码默认不运行，只能在这里手动开启/停止。
  // 候选 = 还没出结果的不兼容格式视频 + 上次失败的（重新开始会自动重试）。
  const transcodeRunning =
    (d.transcodeGenerationStatus?.state || "idle") !== "idle";
  const canStartTranscode =
    d.canUpload &&
    ((d.transcodePendingCount ?? 0) > 0 || (d.transcodeFailedCount ?? 0) > 0);

  return (
    <div className="admin-detail-card">
      <header className="admin-detail-card__title">
        <div className="admin-detail-card__title-left">
          <Activity size={16} />
          <span>生成状态</span>
        </div>
        <div className="admin-detail-actions-inline">
          <span className="admin-drive-preview-toggle__label">预览视频</span>
          <button
            type="button"
            className={`toggle-switch ${d.teaserEnabled ? "is-on" : ""} ${
              togglingTeaserId === d.id ? "is-saving" : ""
            }`}
            onClick={onToggleTeaser}
            disabled={togglingTeaserId === d.id}
            role="switch"
            aria-checked={d.teaserEnabled}
            aria-label="生成预览视频"
            title={
              d.teaserEnabled
                ? "关闭预览视频生成"
                : "开启预览视频生成"
            }
          >
            <span className="toggle-switch__dot" />
          </button>
        </div>
      </header>

      <div className="admin-gen-columns">
        <DriveGenCol
          label="扫盘"
          status={d.scanGenerationStatus}
          showCounts={false}
        />
        <DriveGenCol
          label="封面"
          status={d.thumbnailGenerationStatus}
          ready={d.thumbnailReadyCount}
          pending={d.thumbnailPendingCount}
          failed={d.thumbnailFailedCount}
          extra={d.thumbnailDurationPendingCount}
        />
        <DriveGenCol
          label="预览视频"
          status={d.previewGenerationStatus}
          ready={d.teaserReadyCount}
          pending={d.teaserPendingCount}
          failed={d.teaserFailedCount}
        />
        <DriveGenCol
          label="视频指纹"
          status={d.fingerprintGenerationStatus}
          ready={d.fingerprintReadyCount}
          pending={d.fingerprintPendingCount}
          failed={d.fingerprintFailedCount}
        />
        <DriveGenCol
          label="转码"
          status={d.transcodeGenerationStatus}
          ready={d.transcodeReadyCount}
          pending={d.transcodePendingCount}
          failed={d.transcodeFailedCount}
        />
      </div>

      <div className="admin-detail-actions admin-generation-actions">
        <button
          className="admin-btn"
          disabled={!canQueueThumbnails || regenFailedThumbId === d.id}
          onClick={onRegenFailedThumbnails}
        >
          <span>{(d.thumbnailFailedCount ?? 0) > 0 ? "重试失败封面" : "继续生成封面"}</span>
        </button>
        <button
          className="admin-btn"
          disabled={!canQueuePreviews || regenFailedId === d.id}
          onClick={onRegenFailed}
        >
          <span>{(d.teaserFailedCount ?? 0) > 0 ? "重试失败预览" : "继续生成预览视频"}</span>
        </button>
        <button
          className="admin-btn"
          disabled={!canQueueFingerprints || regenFailedFingerprintId === d.id}
          onClick={onRegenFailedFingerprints}
        >
          <span>{(d.fingerprintFailedCount ?? 0) > 0 ? "重试失败指纹" : "继续生成指纹"}</span>
        </button>
        {transcodeRunning ? (
          <button
            className="admin-btn is-stop"
            disabled={togglingTranscodeId === d.id}
            onClick={onStopTranscode}
            title="停止当前的转码任务。未处理的视频保持原状态，下次开始时继续。"
          >
            <span>{togglingTranscodeId === d.id ? "停止中..." : "停止转码"}</span>
          </button>
        ) : (
          <button
            className="admin-btn"
            disabled={!canStartTranscode || togglingTranscodeId === d.id}
            onClick={onStartTranscode}
            title={
              d.canUpload
                ? "把浏览器播放不了的视频（AVI/WMV/RMVB、MPEG-4 等老格式）转码成 H.264 MP4 并上传回本存储。转码不会自动运行，只能在这里手动开启。"
                : "该存储不支持上传转码产物"
            }
          >
            <span>
              {togglingTranscodeId === d.id
                ? "开启中..."
                : (d.transcodeFailedCount ?? 0) > 0 && (d.transcodePendingCount ?? 0) === 0
                ? "重试失败转码"
                : "开始转码"}
            </span>
          </button>
        )}
      </div>
    </div>
  );
}

function DriveGenCol({
  label,
  status,
  ready,
  pending,
  failed,
  extra,
  showCounts = true,
}: {
  label: string;
  status?: api.DriveGenerationStatus;
  ready?: number;
  pending?: number;
  failed?: number;
  extra?: number;
  showCounts?: boolean;
}) {
  const state = status?.state || "idle";
  const detail = generationDetail(status);
  const title = generationTitle(status, detail);
  const stateLabel = label === "抓取" && state === "scanning" ? "抓取中" : generationStateLabel(state);
  const showScanProgress = !showCounts && (state === "scanning" || (status?.scannedCount ?? 0) > 0 || (status?.addedCount ?? 0) > 0);
  const scannedLabel = label === "抓取" ? "已抓取" : "已扫描";
  return (
    <div className="admin-gen-col">
      <div className="admin-gen-col__head">
        <span className="admin-gen-col__label">{label}</span>
        <span
          className={`admin-status admin-generation-state is-${generationStateClass(state)}`}
          title={title || undefined}
        >
          {stateLabel}
        </span>
      </div>
      {detail && <div className="admin-gen-col__detail">{detail}</div>}
      {showScanProgress && (
        <div className="admin-gen-col__counts admin-gen-col__counts--scan">
          <div className="admin-gen-col__count"><span>{scannedLabel}</span><strong>{status?.scannedCount ?? 0}</strong></div>
          <div className="admin-gen-col__count"><span>预计新增</span><strong>{status?.addedCount ?? 0}</strong></div>
        </div>
      )}
      {showCounts && (
        <div className="admin-gen-col__counts">
          <div className="admin-gen-col__count"><span>就绪</span><strong>{ready ?? 0}</strong></div>
          <div className="admin-gen-col__count"><span>待生成</span><strong>{pending ?? 0}</strong></div>
          <div className="admin-gen-col__count"><span>失败</span><strong>{failed ?? 0}</strong></div>
          {(extra ?? 0) > 0 && (
            <div className="admin-gen-col__count"><span>待补时长</span><strong>{extra}</strong></div>
          )}
        </div>
      )}
    </div>
  );
}
