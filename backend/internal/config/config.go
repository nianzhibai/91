package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/video-site/backend/internal/atomicfile"
	"github.com/video-site/backend/internal/localpath"
	"github.com/video-site/backend/internal/schedule"
	"gopkg.in/yaml.v3"
)

const (
	DefaultNightlyDisabled       = false
	DefaultNightlyStartTime      = "01:00"
	DefaultNightlyTimezone       = schedule.DefaultTimezone
	DefaultGenerationConcurrency = 1
	MaxGenerationConcurrency     = 5
)

var ErrInvalidNightlyStartTime = errors.New("nightly start time must use HH:mm")
var ErrInvalidNightlyTimezone = schedule.ErrInvalidTimezone

var (
	legacyDefaultVideoExtensions = []string{".mp4", ".mkv", ".mov", ".webm", ".avi"}
	defaultVideoExtensions       = []string{".mp4", ".mkv", ".mov", ".webm", ".avi", ".strm"}
)

type Config struct {
	Server       Server       `yaml:"server"`
	Storage      Storage      `yaml:"storage"`
	Logging      Logging      `yaml:"logging"`
	Scanner      Scanner      `yaml:"scanner"`
	Preview      Preview      `yaml:"preview"`
	Generation   Generation   `yaml:"generation"`
	Proxy        Proxy        `yaml:"proxy"`
	Nightly      Nightly      `yaml:"nightly"`
	RemoteUpload RemoteUpload `yaml:"remote_upload"`
	// Telegram configuration is persisted in YAML and applied without restarting.
	Telegram Telegram `yaml:"telegram"`
}

type Server struct {
	Listen string `yaml:"listen"`
	// AllowedOrigins 是允许跨源访问的前端 Origin 白名单（如 "https://video.example.com"）。
	// 默认空 → 不开启 CORS 跨源；同源部署（前后端在同一个域名 + 端口下）不需要配置此项。
	// 浏览器对不在列表里的 Origin 不会拿到 Access-Control-Allow-Origin 头，自然就读不到响应。
	// 不要写 "*"；带 cookie 的 CORS 必须是具体 Origin。
	AllowedOrigins []string `yaml:"allowed_origins"`
}

func ensureDocumentMapping(root *yaml.Node) *yaml.Node {
	if root.Kind == 0 {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	if root.Kind != yaml.DocumentNode {
		clone := *root
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{&clone}
	}
	if len(root.Content) == 0 || root.Content[0] == nil {
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	if root.Content[0].Kind != yaml.MappingNode {
		root.Content[0].Kind = yaml.MappingNode
		root.Content[0].Content = nil
	}
	return root.Content[0]
}

func ensureMappingValue(parent *yaml.Node, key string) *yaml.Node {
	if parent.Kind != yaml.MappingNode {
		parent.Kind = yaml.MappingNode
		parent.Content = nil
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			if parent.Content[i+1].Kind != yaml.MappingNode {
				parent.Content[i+1].Kind = yaml.MappingNode
				parent.Content[i+1].Content = nil
			}
			return parent.Content[i+1]
		}
	}
	value := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
	return value
}

func setScalarValue(parent *yaml.Node, key, value string) {
	if parent.Kind != yaml.MappingNode {
		parent.Kind = yaml.MappingNode
		parent.Content = nil
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1].Kind = yaml.ScalarNode
			parent.Content[i+1].Tag = "!!str"
			parent.Content[i+1].Value = value
			parent.Content[i+1].Content = nil
			return
		}
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

type Storage struct {
	DataDir string `yaml:"data_dir"`
	DBDir   string `yaml:"db_dir"` // empty inherits DataDir
	// Derived paths are runtime details, never independent YAML settings.
	DBPath          string `yaml:"-"`
	LocalPreviewDir string `yaml:"-"`
}

type Logging struct {
	FileEnabled    *bool  `yaml:"file_enabled"`
	Directory      string `yaml:"-"` // derived from storage.data_dir
	MaxFileSizeMB  int    `yaml:"max_file_size_mb"`
	MaxTotalSizeMB int    `yaml:"max_total_size_mb"`
}

func (l Logging) IsFileEnabled() bool {
	return l.FileEnabled == nil || *l.FileEnabled
}

// ResolveStoragePaths returns the storage configuration used by the running
// process. Values in config.yaml remain unchanged, while every subsystem gets
// the same absolute paths resolved from the process startup directory.
func ResolveStoragePaths(storage Storage, baseDir string) (Storage, error) {
	dataDir, err := localpath.Resolve(baseDir, storage.DataDir)
	if err != nil {
		return Storage{}, fmt.Errorf("resolve data directory: %w", err)
	}
	dbDir := dataDir
	if strings.TrimSpace(storage.DBDir) != "" {
		dbDir, err = localpath.Resolve(baseDir, storage.DBDir)
		if err != nil {
			return Storage{}, fmt.Errorf("resolve database directory: %w", err)
		}
	}
	return Storage{
		DataDir:         dataDir,
		DBDir:           dbDir,
		DBPath:          filepath.Join(dbDir, "video-site.db"),
		LocalPreviewDir: filepath.Join(dataDir, "previews"),
	}, nil
}

// ResolveLoggingPaths resolves the log directory derived from storage.data_dir
// against the startup directory without changing the file configuration.
func ResolveLoggingPaths(logging Logging, baseDir string) (Logging, error) {
	directory, err := localpath.Resolve(baseDir, logging.Directory)
	if err != nil {
		return Logging{}, fmt.Errorf("resolve log directory: %w", err)
	}
	logging.Directory = directory
	return logging, nil
}

type Scanner struct {
	// IntervalSeconds 已废弃。旧版每天 02:00–07:00 窗口内按这个间隔重复扫盘；
	// 新版统一由 nightly 调度器调度，此字段被忽略，保留仅为兼容旧 yaml。
	IntervalSeconds int `yaml:"interval_seconds"`
	// MaxDepth 已废弃。扫描会递归到叶子目录；字段仅用于兼容旧 yaml。
	MaxDepth        int      `yaml:"max_depth"`
	VideoExtensions []string `yaml:"video_extensions"`
}

// Generation bounds work across every drive, independently of scan concurrency.
type Generation struct {
	ThumbnailConcurrency   int `yaml:"thumbnail_concurrency"`
	PreviewConcurrency     int `yaml:"preview_concurrency"`
	FingerprintConcurrency int `yaml:"fingerprint_concurrency"`
}

type Preview struct {
	Enabled         bool   `yaml:"enabled"`
	FFmpegPath      string `yaml:"ffmpeg_path"`
	FFprobePath     string `yaml:"ffprobe_path"`
	FFmpegThreads   int    `yaml:"ffmpeg_threads"`
	DurationSeconds int    `yaml:"duration_seconds"`
	Width           int    `yaml:"width"`
	Segments        int    `yaml:"segments"`
}

type Proxy struct {
	// AllowForcedRelay controls whether authenticated playback clients may ask
	// the backend to relay an otherwise redirectable stream. nil preserves the
	// historical/default behavior (enabled).
	AllowForcedRelay *bool `yaml:"allow_forced_relay"`
}

func (p Proxy) AllowsForcedRelay() bool {
	return p.AllowForcedRelay == nil || *p.AllowForcedRelay
}

// Nightly 是凌晨流水线（扫盘 → 爬虫 → 迁移 → 去重维护）的调度配置。
//
// 一个进程只跑一条 nightly 流水线；该 cron 时间到达且当天还没跑过时触发。
// 管理后台「扫描所有网盘」与它共享任务协调器，但只运行扫盘和去重阶段。
type Nightly struct {
	// Disabled 阻止每日自然调度触发新的流水线。它不影响管理员手动触发的
	// 扫描任务，也不会取消已经开始执行的流水线。
	Disabled bool `yaml:"disabled,omitempty"`
	// StartTime 是每日触发时间，采用严格的 24 小时 HH:mm 格式。该字段可在
	// 管理后台热更新；配置面板与源码编辑器都直接读写 config.yaml。
	StartTime string `yaml:"start_time,omitempty"`
	// Timezone 是独立于宿主机系统时区的 IANA 时区名。调度时间和每日去重
	// 日期均按该时区计算；修改它不会修改操作系统时区。
	Timezone string `yaml:"timezone,omitempty"`
	// CronHour 仅用于读取旧版配置。启动迁移会把它转换为 start_time。
	CronHour int `yaml:"cron_hour,omitempty"`
}

func NormalizeNightlyStartTime(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	parsed, err := time.Parse("15:04", trimmed)
	if err != nil || len(trimmed) != len("15:04") {
		return "", ErrInvalidNightlyStartTime
	}
	return parsed.Format("15:04"), nil
}

func NormalizeNightlyTimezone(value string) (string, error) {
	normalized, _, err := schedule.LoadTimezone(value)
	if err != nil {
		return "", ErrInvalidNightlyTimezone
	}
	return normalized, nil
}

type RemoteUpload struct {
	// DiskReserveBytes 是直链下载期间必须留给数据盘的最小可用空间。
	DiskReserveBytes int64 `yaml:"disk_reserve_bytes"`
	// IdleTimeoutSeconds 是响应正文连续无数据时中止任务的秒数。
	IdleTimeoutSeconds int `yaml:"idle_timeout_seconds"`
}

// Load 读取配置；若不存在则从 config.example.yaml 复制一份并返回
func Load(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		example := filepath.Join(filepath.Dir(path), "config.example.yaml")
		data, err := os.ReadFile(example)
		if err != nil {
			return nil, fmt.Errorf("config not found and example missing: %w", err)
		}
		if err := writeFileAtomically(path, data, 0o644); err != nil {
			return nil, fmt.Errorf("write default config: %w", err)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}

// Parse validates and normalizes a complete YAML configuration. Both startup
// loading and the management API use this function, so an accepted panel save
// is guaranteed to satisfy the same invariants as a process restart.
func Parse(data []byte) (*Config, error) {
	c := Config{Preview: Preview{Enabled: true}}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.importLegacyDataDirectory(data); err != nil {
		return nil, err
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.Telegram.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Server.Listen == "" {
		c.Server.Listen = "0.0.0.0:9191"
	}
	c.Storage.DataDir = strings.TrimSpace(c.Storage.DataDir)
	if c.Storage.DataDir == "" {
		c.Storage.DataDir = "./data"
	}
	c.Storage.DBDir = strings.TrimSpace(c.Storage.DBDir)
	dbDir := c.Storage.DBDir
	if dbDir == "" {
		dbDir = c.Storage.DataDir
	}
	c.Storage.DBPath = filepath.Join(dbDir, "video-site.db")
	c.Storage.LocalPreviewDir = filepath.Join(c.Storage.DataDir, "previews")
	if c.Logging.FileEnabled == nil {
		enabled := true
		c.Logging.FileEnabled = &enabled
	}
	c.Logging.Directory = filepath.Join(c.Storage.DataDir, "logs")
	if c.Logging.MaxFileSizeMB == 0 {
		c.Logging.MaxFileSizeMB = 5
	}
	if c.Logging.MaxFileSizeMB < 1 || c.Logging.MaxFileSizeMB > 1024 {
		return errors.New("logging.max_file_size_mb must be between 1 and 1024")
	}
	if c.Logging.MaxTotalSizeMB == 0 {
		c.Logging.MaxTotalSizeMB = 30
	}
	if c.Logging.MaxTotalSizeMB < c.Logging.MaxFileSizeMB || c.Logging.MaxTotalSizeMB > 10240 {
		return errors.New("logging.max_total_size_mb must be at least max_file_size_mb and no more than 10240")
	}
	if c.Scanner.MaxDepth == 0 {
		c.Scanner.MaxDepth = 5
	}
	if len(c.Scanner.VideoExtensions) == 0 {
		c.Scanner.VideoExtensions = append([]string{}, defaultVideoExtensions...)
	} else if isLegacyDefaultVideoExtensions(c.Scanner.VideoExtensions) {
		c.Scanner.VideoExtensions = append(c.Scanner.VideoExtensions, ".strm")
	}
	for _, field := range []struct {
		name  string
		value *int
	}{
		{"thumbnail_concurrency", &c.Generation.ThumbnailConcurrency},
		{"preview_concurrency", &c.Generation.PreviewConcurrency},
		{"fingerprint_concurrency", &c.Generation.FingerprintConcurrency},
	} {
		if *field.value == 0 {
			*field.value = DefaultGenerationConcurrency
		}
		if *field.value < 1 || *field.value > MaxGenerationConcurrency {
			return fmt.Errorf("generation.%s must be between 1 and %d", field.name, MaxGenerationConcurrency)
		}
	}
	if c.Preview.FFmpegThreads == 0 {
		c.Preview.FFmpegThreads = 1
	}
	if c.Preview.FFmpegThreads < 1 || c.Preview.FFmpegThreads > 16 {
		return fmt.Errorf("preview.ffmpeg_threads must be between 1 and 16")
	}
	if c.Preview.FFmpegPath == "" {
		c.Preview.FFmpegPath = "ffmpeg"
	}
	if c.Preview.FFprobePath == "" {
		c.Preview.FFprobePath = "ffprobe"
	}
	if c.Preview.DurationSeconds != 3 {
		c.Preview.DurationSeconds = 3
	}
	if c.Preview.Width == 0 {
		c.Preview.Width = 480
	}
	if c.Preview.Segments == 0 {
		c.Preview.Segments = 3
	}
	if c.Nightly.CronHour <= 0 || c.Nightly.CronHour > 23 {
		c.Nightly.CronHour = 1
	}
	if strings.TrimSpace(c.Nightly.StartTime) == "" {
		c.Nightly.StartTime = fmt.Sprintf("%02d:00", c.Nightly.CronHour)
	} else {
		startTime, err := NormalizeNightlyStartTime(c.Nightly.StartTime)
		if err != nil {
			return fmt.Errorf("nightly.start_time: %w", err)
		}
		c.Nightly.StartTime = startTime
	}
	if strings.TrimSpace(c.Nightly.Timezone) == "" {
		c.Nightly.Timezone = DefaultNightlyTimezone
	} else {
		timezone, err := NormalizeNightlyTimezone(c.Nightly.Timezone)
		if err != nil {
			return fmt.Errorf("nightly.timezone: %w", err)
		}
		c.Nightly.Timezone = timezone
	}
	if c.RemoteUpload.DiskReserveBytes <= 0 {
		c.RemoteUpload.DiskReserveBytes = 1 << 30
	}
	if c.RemoteUpload.IdleTimeoutSeconds <= 0 {
		c.RemoteUpload.IdleTimeoutSeconds = 120
	}
	return nil
}

func configFileMode(path string) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return 0o644
}

// writeFileAtomically replaces path only after the new bytes are fully written
// and synced in the same directory. A failed validation or write therefore
// never leaves a partially truncated configuration behind.
func writeFileAtomically(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	// Directory Sync is best-effort because several supported filesystems (and
	// Windows) reject syncing directory handles. The rename has already committed
	// at this point, so returning an error would falsely tell callers that the
	// save failed even though config.yaml was replaced.
	_ = atomicfile.SyncDirectory(dir)
	return nil
}

func isLegacyDefaultVideoExtensions(exts []string) bool {
	if len(exts) != len(legacyDefaultVideoExtensions) {
		return false
	}
	seen := make(map[string]struct{}, len(exts))
	for _, ext := range exts {
		seen[strings.ToLower(strings.TrimSpace(ext))] = struct{}{}
	}
	for _, ext := range legacyDefaultVideoExtensions {
		if _, ok := seen[ext]; !ok {
			return false
		}
	}
	return true
}
