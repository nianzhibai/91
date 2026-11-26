import type { Schema } from "../contract.js";
import { ADMIN_BASE, type AdminContext } from "./context.js";

export type BackupEstimate = Schema<"backupEstimate">;
export type BackupTask = Schema<"backupTaskStatus">;
export type BackupOperationProgress = Schema<"backupOperationProgress">;
export type BackupUploadChunk = Schema<"backupUploadChunk">;
export type BackupSelection = Schema<"backupBackupSelection">;

export type BackupRecord = Omit<Schema<"backupBackupRecord">, "included" | "verificationStatus"> & {
  verificationStatus: "verified" | "unchecked" | "invalid" | string;
  included?: string[];
};

export type BackupList = Omit<Schema<"backupListResult">, "backups"> & {
  backups: BackupRecord[];
};

export type BackupUploadSession = Omit<Schema<"backupUploadSession">, "received"> & {
  received: BackupUploadChunk[];
};

export type BackupManifest = Omit<Schema<"backupManifest">, "included" | "selection"> & {
  included: string[];
  selection: BackupSelection;
};

export type RestoreReport = Omit<
  Schema<"backupValidationReport">,
  "manifest" | "pathRewrites" | "localStorageWarnings" | "missingAssets" | "warnings"
> & {
  manifest: BackupManifest;
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

export type BackupTransferJob = Schema<"backuptransferTransferJob">;
export type BackupReceiveToken = Schema<"backuptransferReceiveToken">;
export type BackupReceiveTokenInfo = Schema<"backuptransferReceiveTokenInfo">;
export type BackupReceiveTransfer = Schema<"backuptransferReceiveTransfer">;

export function createAdminBackupsClient({ http, request }: AdminContext) {
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
    return http.apiURL(`${ADMIN_BASE}/backups/${encodeURIComponent(id)}/download`);
  }

  function beginBackupUpload(input: { fileName: string; size: number }) {
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
    return request<BackupUploadSession>(
      `/backup-uploads/${encodeURIComponent(id)}/chunks/${index}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/octet-stream" },
        body: chunk,
        signal,
      }
    );
  }

  function finalizeBackupUpload(id: string, sha256: string) {
    return request<BackupRecord>(`/backup-uploads/${encodeURIComponent(id)}/finalize`, {
      method: "POST",
      body: JSON.stringify({ sha256 }),
    });
  }

  function cancelBackupUpload(id: string) {
    return request<{ ok: boolean }>(`/backup-uploads/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function restoreBackup(id: string, input: { confirmation: string }) {
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
    return request<BackupTransferJob>(`/backups/${encodeURIComponent(backupId)}/transfers`, {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  function cancelBackupTransfer(id: string) {
    return request<BackupTransferJob>(`/backup-transfers/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  function retryBackupTransfer(id: string) {
    return request<BackupTransferJob>(`/backup-transfers/${encodeURIComponent(id)}/retry`, {
      method: "POST",
    });
  }

  function listBackupReceiveTokens(signal?: AbortSignal) {
    return request<BackupReceiveTokenInfo[]>("/backup-receive-tokens", { signal });
  }

  function createBackupReceiveToken() {
    return request<BackupReceiveToken>("/backup-receive-tokens", { method: "POST" });
  }

  function revokeBackupReceiveToken(id: string) {
    return request<{ ok: boolean }>(`/backup-receive-tokens/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  return {
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
    listBackupReceiveTokens,
    createBackupReceiveToken,
    revokeBackupReceiveToken,
  };
}
