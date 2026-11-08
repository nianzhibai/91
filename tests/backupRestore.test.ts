import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const backupApiHandler = readFileSync(
  new URL("../backend/internal/api/admin_backups.go", import.meta.url),
  "utf8"
);
const backupTransferRoutes = readFileSync(
  new URL("../backend/internal/api/admin.go", import.meta.url),
  "utf8"
);
const backupTransferApi = readFileSync(
  new URL("../backend/internal/api/backup_transfers.go", import.meta.url),
  "utf8"
);
const backupTransferSender = readFileSync(
  new URL("../backend/internal/backuptransfer/outgoing.go", import.meta.url),
  "utf8"
);
const backupTransferTypes = readFileSync(
  new URL("../backend/internal/backuptransfer/types.go", import.meta.url),
  "utf8"
);

const backupTypes = readFileSync(
  new URL("../backend/internal/backup/types.go", import.meta.url),
  "utf8"
);
const backupArchive = readFileSync(
  new URL("../backend/internal/backup/archive.go", import.meta.url),
  "utf8"
);
const backupRestorePrepare = readFileSync(
  new URL("../backend/internal/backup/restore_prepare.go", import.meta.url),
  "utf8"
);
const backupRestoreMerge = readFileSync(
  new URL("../backend/internal/backup/restore_merge.go", import.meta.url),
  "utf8"
);
const backupRestoreAssets = readFileSync(
  new URL("../backend/internal/backup/restore_assets.go", import.meta.url),
  "utf8"
);
const backupRestoreLocal = readFileSync(
  new URL("../backend/internal/backup/restore_local.go", import.meta.url),
  "utf8"
);

const serverMain = readFileSync(
  new URL("../backend/cmd/server/main.go", import.meta.url),
  "utf8"
);
const install = readFileSync(new URL("../install.sh", import.meta.url), "utf8");
const deploy = readFileSync(new URL("../deploy.sh", import.meta.url), "utf8");
const compose = readFileSync(
  new URL("../docker-compose.yml", import.meta.url),
  "utf8"
);

test("backup page keeps destructive restore confirmation concise", () => {

  assert.doesNotMatch(backupApiHandler, /CheckCurrentPassword|request\.Password/);
  assert.match(backupApiHandler, /request\.Confirmation != "确认恢复"/);

});

test("restore acknowledgement stays bounded and survives the planned restart gap", () => {
  assert.match(backupApiHandler, /writeRestoreAccepted/);
  assert.match(backupApiHandler, /compactRestoreReport/);
  assert.match(backupApiHandler, /http\.Flusher/);
  assert.match(backupApiHandler, /restoreRestartGracePeriod/);

});

test("backup creation rejects an empty durable backup scope", () => {
  assert.match(backupApiHandler, /ErrNoBackupContent/);
});

test("backup archives accept only the current protocol", () => {
  assert.match(backupTypes, /FormatVersion = 3/);
  assert.doesNotMatch(
    backupTypes,
    /ScopedFormatVersion|LegacyFormatVersion|UserConfig/
  );
  assert.match(
    backupArchive,
    /if manifest\.FormatVersion != FormatVersion \{[\s\S]*?unsupported format version/
  );
  assert.match(backupArchive, /if manifest\.Selection == nil/);
  assert.doesNotMatch(
    backupArchive,
    /ScopedFormatVersion|LegacyFormatVersion|EffectiveSelection/
  );
  assert.doesNotMatch(backupRestorePrepare, /\.IsFull\(\)|UserConfig/);

});

test("upload storage merges while each source local storage keeps an isolated namespace", () => {
  assert.match(backupRestoreMerge, /driveUsesMergeRestore/);
  assert.match(backupRestoreMerge, /restore_merged_drives/);
  assert.doesNotMatch(
    backupRestoreMerge,
    /DELETE FROM main\.remote_upload_jobs/
  );
  assert.doesNotMatch(backupRestoreMerge, /INSERT OR REPLACE/);
  assert.match(
    backupRestoreMerge,
    /INSERT OR IGNORE INTO main\.video_tags/
  );
  assert.match(backupRestoreAssets, /prepareMergedUploadStorage/);
  assert.match(
    backupRestoreAssets,
    /snapshotSource\(ctx, source, merged\)[\s\S]*?overlaySource\(ctx, target, merged\)/
  );
  assert.match(backupRestorePrepare, /prepareIsolatedLocalStorage/);
  assert.doesNotMatch(backupRestorePrepare, /prepareMergedLocalStorage/);
  assert.match(
    backupRestoreLocal,
    /fmt\.Sprintf\("localstorage-restore-%s-%03d", stageID, index\+1\)/
  );
  assert.match(backupRestoreLocal, /SourceDriveID/);
  assert.match(backupRestoreLocal, /rewriteLocalStorageCatalog/);
  assert.match(backupRestoreLocal, /videoid\.ForDrive\("localstorage", video\.newDriveID/);
  const isolatedLocalRestore = backupRestoreAssets.slice(
    backupRestoreAssets.indexOf("func prepareIsolatedLocalStorage"),
    backupRestoreAssets.indexOf("func overlaySource")
  );
  assert.doesNotMatch(
    isolatedLocalRestore,
    /preserve target local storage|overlaySource\(ctx, target, merged\)/
  );
});

test("migration upload uses resumable checkpoints and one whole-archive hash", () => {

  assert.doesNotMatch(backupApiHandler, /Content-Digest|X-Chunk-SHA256/);
  assert.match(backupApiHandler, /Backups\.PutChunk\([\s\S]*?http\.MaxBytesReader/);

});

test("server transfer uses parallel range streaming with scoped tokens and no automatic restore", () => {

  assert.match(backupTransferRoutes, /r\.Route\(backuptransfer\.PeerBackupPath/);
  assert.match(backupTransferTypes, /PeerBackupPath\s+=\s+"\/peer\/backups"/);
  assert.doesNotMatch(backupTransferTypes, /\/peer\/v\d+\/backups/);
  assert.match(backupTransferApi, /peerBearerToken\(r\)/);
  assert.match(backupTransferApi, /Content-Range/);
  assert.doesNotMatch(backupTransferApi, /Content-Digest|X-Chunk-SHA256/);
  assert.match(backupTransferSender, /scheme != "http" && scheme != "https"/);
  assert.match(backupTransferSender, /目标服务器仅支持 HTTP 或 HTTPS/);
  assert.doesNotMatch(backupTransferSender, /allowInsecure/);
  assert.match(backupTransferSender, /uploadRanges\(/);
  assert.match(backupTransferSender, /min\(ParallelStreams, capabilities\.ParallelStreams\)/);
  assert.match(backupTransferSender, /committedRanges\(status\)/);

  assert.match(backupTransferRoutes, /r\.Delete\("\/backup-receives\/\{id\}", a\.handleCancelBackupReceiveTransfer\)/);

});

test("supported deployments restart on the dedicated restore exit code", () => {
  assert.match(serverMain, /os\.Exit\(backup\.RestartExitCode\)/);
  assert.match(install, /RestartForceExitStatus=75/);
  assert.match(install, /VIDEO_RESTART_MANAGED=true/);
  assert.match(deploy, /RestartForceExitStatus=75/);
  assert.match(deploy, /VIDEO_RESTART_MANAGED=true/);
  assert.match(compose, /VIDEO_RESTART_MANAGED: "true"/);
  assert.match(compose, /restart: unless-stopped/);
});

test("deploy keeps systemd environment lines separate from LimitNOFILE", () => {
  assert.match(deploy, /\$\{env_lines\}\nLimitNOFILE=65536/);
  assert.doesNotMatch(deploy, /\$\{env_lines\}LimitNOFILE=65536/);
});
