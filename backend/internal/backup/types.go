package backup

import (
	"errors"
	"time"
)

const (
	// FormatVersion is the only supported on-disk backup format.
	FormatVersion = 3
	// ChunkSize is intentionally fixed so an interrupted migration upload can
	// be resumed by another browser session without renegotiating boundaries.
	ChunkSize int64 = 16 << 20
	// UploadTTL is the retention period for an unfinished migration upload.
	UploadTTL = 72 * time.Hour
	// RestartExitCode asks the supported systemd/Docker deployment to start a
	// fresh process after a pending restore has been staged.
	RestartExitCode = 75

	manifestName           = "manifest.json"
	backupNamePrefix       = "video-site-91-backup-"
	legacyBackupNamePrefix = "video-site-91-full-"
	restoreStageDirName    = ".restore-staging"
	maxRestoreOperations   = 1024
	maxJSONSidecarBytes    = int64(64 << 20)
)

var (
	ErrTaskRunning       = errors.New("已有备份任务正在运行")
	ErrNoRunningTask     = errors.New("当前没有正在运行的备份任务")
	ErrBackupNotFound    = errors.New("备份不存在")
	ErrUploadNotFound    = errors.New("迁移上传不存在或已过期")
	ErrUploadIncomplete  = errors.New("迁移上传仍有缺失分片")
	ErrUploadFinalizing  = errors.New("迁移上传正在完整校验")
	ErrUploadRangeBusy   = errors.New("迁移上传区间正在写入")
	ErrRestorePending    = errors.New("已有恢复任务等待重启")
	ErrInsufficientSpace = errors.New("磁盘可用空间不足")
	ErrNoBackupContent   = errors.New("请至少选择一项备份内容")
)

type Manifest struct {
	FormatVersion  int                `json:"formatVersion"`
	AppVersion     string             `json:"appVersion"`
	CreatedAt      time.Time          `json:"createdAt"`
	SourceDataRoot string             `json:"sourceDataRoot"`
	SourceDBPath   string             `json:"sourceDatabasePath,omitempty"`
	SourcePreview  string             `json:"sourcePreviewRoot,omitempty"`
	FileCount      int                `json:"fileCount"`
	TotalSize      int64              `json:"totalSize"`
	Included       []string           `json:"included"`
	Selection      *BackupSelection   `json:"selection,omitempty"`
	LocalStorage   []LocalStorageRoot `json:"localStorage,omitempty"`
	Files          []ManifestFile     `json:"files,omitempty"`
}

// BackupSelection is the user-visible scope of a backup. The database schema
// itself is always included because it is the catalog needed to restore the
// selected resources. Global tag definitions are included even without video
// references; resource rows and tag assignments are filtered to this scope.
type BackupSelection struct {
	CloudDrives    bool `json:"cloudDrives"`
	CrawlerScripts bool `json:"crawlerScripts"`
	UploadStorage  bool `json:"uploadStorage"`
	LocalStorage   bool `json:"localStorage"`
	UserInfo       bool `json:"userInfo"`
}

func FullBackupSelection() BackupSelection {
	return BackupSelection{
		CloudDrives:    true,
		CrawlerScripts: true,
		UploadStorage:  true,
		LocalStorage:   true,
		UserInfo:       true,
	}
}

func (s BackupSelection) Any() bool {
	return s.CloudDrives || s.CrawlerScripts || s.UploadStorage || s.LocalStorage || s.UserInfo
}

// LocalStorageRoot describes one selected localstorage drive copied into the
// archive. ArchivePath is relative to payload/localstorage.
type LocalStorageRoot struct {
	DriveID     string `json:"driveId"`
	ArchivePath string `json:"archivePath"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode,omitempty"`
}

type Estimate struct {
	FileCount      int   `json:"fileCount"`
	TotalBytes     int64 `json:"totalBytes"`
	AvailableBytes int64 `json:"availableBytes"`
	RequiredBytes  int64 `json:"requiredBytes"`
}

type TaskStatus struct {
	ID             string    `json:"id"`
	State          string    `json:"state"`
	Phase          string    `json:"phase,omitempty"`
	Name           string    `json:"name,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt,omitempty"`
	FileCount      int       `json:"fileCount"`
	ProcessedFiles int       `json:"processedFiles"`
	TotalBytes     int64     `json:"totalBytes"`
	ProcessedBytes int64     `json:"processedBytes"`
	BytesPerSecond int64     `json:"bytesPerSecond"`
	Error          string    `json:"error,omitempty"`
	Cancellable    bool      `json:"cancellable"`
}

type BackupRecord struct {
	ID                 string           `json:"id"`
	Name               string           `json:"name"`
	Size               int64            `json:"size"`
	SHA256             string           `json:"sha256,omitempty"`
	CreatedAt          time.Time        `json:"createdAt"`
	VerificationStatus string           `json:"verificationStatus"`
	VerificationError  string           `json:"verificationError,omitempty"`
	Imported           bool             `json:"imported"`
	AppVersion         string           `json:"appVersion,omitempty"`
	SourceDataRoot     string           `json:"sourceDataRoot,omitempty"`
	FileCount          int              `json:"fileCount,omitempty"`
	ExpandedSize       int64            `json:"expandedSize,omitempty"`
	Included           []string         `json:"included,omitempty"`
	Selection          *BackupSelection `json:"selection,omitempty"`
}

type ListResult struct {
	Backups         []BackupRecord     `json:"backups"`
	Current         *TaskStatus        `json:"current,omitempty"`
	RestoreProgress *OperationProgress `json:"restoreProgress,omitempty"`
	Estimate        Estimate           `json:"estimate"`
	RestartManaged  bool               `json:"restartManaged"`
	PendingRestore  bool               `json:"pendingRestore"`
}

// OperationProgress is lightweight, in-memory telemetry for a synchronous
// backup operation. It is intentionally not persisted: durable recovery is
// still driven by upload sidecars and the restore marker.
type OperationProgress struct {
	Phase          string `json:"phase"`
	ProcessedBytes int64  `json:"processedBytes"`
	TotalBytes     int64  `json:"totalBytes"`
	ProcessedFiles int    `json:"processedFiles"`
	TotalFiles     int    `json:"totalFiles"`
}

type BeginUploadInput struct {
	FileName string `json:"fileName"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256,omitempty"`
}

type UploadChunk struct {
	Index int   `json:"index"`
	Size  int64 `json:"size"`
}

type UploadSession struct {
	ID          string             `json:"id"`
	FileName    string             `json:"fileName"`
	Size        int64              `json:"size"`
	SHA256      string             `json:"sha256,omitempty"`
	ChunkSize   int64              `json:"chunkSize"`
	TotalChunks int                `json:"totalChunks"`
	Received    []UploadChunk      `json:"received"`
	State       string             `json:"state"`
	Progress    *OperationProgress `json:"progress,omitempty"`
	CreatedAt   time.Time          `json:"createdAt"`
	ExpiresAt   time.Time          `json:"expiresAt"`
}

type ValidationReport struct {
	Manifest             Manifest `json:"manifest"`
	VerificationStatus   string   `json:"verificationStatus"`
	PathRewrites         []string `json:"pathRewrites,omitempty"`
	LocalStorageWarnings []string `json:"localStorageWarnings,omitempty"`
	MissingAssets        []string `json:"missingAssets,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
}

type RestoreRequest struct {
	Confirmation string `json:"confirmation"`
}

type RestoreResult struct {
	OK             bool             `json:"ok"`
	Restarting     bool             `json:"restarting"`
	RestartManaged bool             `json:"restartManaged"`
	Report         ValidationReport `json:"report"`
}

type archiveMeta struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	ModifiedAt time.Time `json:"modifiedAt"`
	VerifiedAt time.Time `json:"verifiedAt"`
	Imported   bool      `json:"imported"`
	UploadID   string    `json:"uploadId,omitempty"`
	Manifest   Manifest  `json:"manifest"`
}
