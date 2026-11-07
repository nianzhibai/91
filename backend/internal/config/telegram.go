package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nianzhibai/91/backend/internal/scopedproxy"
	"github.com/nianzhibai/91/backend/internal/tcpproxy"
	"gopkg.in/yaml.v3"
)

type Telegram struct {
	APIID               int     `yaml:"api_id" json:"apiId"`
	APIHash             string  `yaml:"api_hash" json:"-"`
	BotToken            string  `yaml:"bot_token" json:"-"`
	ConnectionProxy     string  `yaml:"connection_proxy" json:"-"`
	Enabled             bool    `yaml:"enabled" json:"enabled"`
	AllowedUserIDs      []int64 `yaml:"allowed_user_ids" json:"allowedUserIds"`
	SiteBaseURL         string  `yaml:"site_base_url" json:"siteBaseUrl"`
	MaxFileSizeBytes    int64   `yaml:"max_file_size_bytes" json:"maxFileSizeBytes"`
	DownloadThreads     int     `yaml:"download_threads" json:"downloadThreads"`
	MaxPendingJobs      int     `yaml:"max_pending_jobs" json:"maxPendingJobs"`
	FetchTimeoutSeconds int     `yaml:"fetch_timeout_seconds" json:"fetchTimeoutSeconds"`
	UploadDriveID       string  `yaml:"upload_drive_id" json:"uploadDriveId"`
	UploadDirectory     string  `yaml:"upload_directory" json:"uploadDirectory"`
	UploadProxy         string  `yaml:"upload_proxy" json:"-"`
	StorageDirectory    string  `yaml:"storage_directory" json:"storageDirectory"`

	// Resolved against the process working directory, like storage.data_dir.
	LocalFilesRoot string `yaml:"-" json:"-"`
}

var telegramTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
var telegramAPIHashPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

func (t *Telegram) UnmarshalYAML(node *yaml.Node) error {
	// yaml.v3 otherwise silently truncates fractional numbers into int fields.
	var fields struct {
		DownloadThreads any `yaml:"download_threads"`
	}
	if err := node.Decode(&fields); err != nil {
		return err
	}
	if fields.DownloadThreads != nil {
		if _, ok := fields.DownloadThreads.(int); !ok {
			return errors.New("telegram.download_threads 必须为整数")
		}
	}
	type plain Telegram
	return node.Decode((*plain)(t))
}

func (t *Telegram) Validate() error {
	t.APIHash = strings.TrimSpace(t.APIHash)
	if t.APIID < 0 || int64(t.APIID) > 2147483647 {
		return errors.New("telegram.api_id 必须为正整数")
	}
	if t.APIHash != "" && !telegramAPIHashPattern.MatchString(t.APIHash) {
		return errors.New("telegram.api_hash 必须为 32 位十六进制字符串")
	}
	if t.Enabled && (t.APIID == 0 || t.APIHash == "") {
		return errors.New("启用 Telegram 前请填写 api_id 和 api_hash")
	}
	t.StorageDirectory = strings.TrimSpace(t.StorageDirectory)
	if strings.ContainsAny(t.StorageDirectory, "\x00\r\n") {
		return errors.New("telegram.storage_directory 路径无效")
	}
	t.BotToken = strings.TrimSpace(t.BotToken)
	if t.BotToken != "" {
		id, err := strconv.ParseInt(strings.SplitN(t.BotToken, ":", 2)[0], 10, 64)
		if !telegramTokenPattern.MatchString(t.BotToken) || err != nil || id <= 0 {
			return errors.New("telegram.bot_token 格式无效")
		}
	}
	if t.Enabled && t.BotToken == "" {
		return errors.New("启用 Telegram 前请填写 bot_token")
	}

	if t.AllowedUserIDs == nil {
		t.AllowedUserIDs = []int64{}
	}
	connectionProxy, err := tcpproxy.Normalize(t.ConnectionProxy)
	if err != nil {
		return errors.New("telegram.connection_proxy 必须为有效的 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理地址")
	}
	t.ConnectionProxy = connectionProxy
	t.UploadDriveID = strings.TrimSpace(t.UploadDriveID)
	proxy, err := scopedproxy.Normalize(t.UploadProxy)
	if err != nil {
		return errors.New("telegram.upload_proxy 必须为有效的 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理地址")
	}
	t.UploadProxy = proxy
	t.UploadDirectory = strings.TrimSpace(t.UploadDirectory)
	if t.UploadDirectory == "" {
		t.UploadDirectory = "Telegram"
	}
	for _, part := range strings.Split(t.UploadDirectory, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return errors.New("telegram.upload_directory 必须为网盘根目录下的相对目录，如 Telegram/videos")
		}
	}
	if t.UploadDriveID == "local-upload" || t.UploadDriveID == "telegram-local" {
		return errors.New("telegram.upload_drive_id 必须选择支持上传的网盘")
	}
	if t.MaxFileSizeBytes == 0 {
		t.MaxFileSizeBytes = 4 << 30
	}
	if t.DownloadThreads == 0 {
		t.DownloadThreads = 6
	}
	if t.MaxPendingJobs == 0 {
		t.MaxPendingJobs = 100
	}
	if t.FetchTimeoutSeconds == 0 {
		t.FetchTimeoutSeconds = 1800
	}
	if t.MaxFileSizeBytes < 1 || t.MaxFileSizeBytes > 16<<30 {
		return errors.New("telegram.max_file_size_bytes 必须在 1 字节到 16 GiB 之间")
	}
	if t.DownloadThreads < 1 || t.DownloadThreads > 8 {
		return errors.New("telegram.download_threads 必须在 1 到 8 之间")
	}
	if t.MaxPendingJobs < 1 || t.MaxPendingJobs > 10000 {
		return errors.New("telegram.max_pending_jobs 必须在 1 到 10000 之间")
	}
	if t.FetchTimeoutSeconds < 30 || t.FetchTimeoutSeconds > 86400 {
		return errors.New("telegram.fetch_timeout_seconds 必须在 30 到 86400 之间")
	}
	if t.SiteBaseURL != "" {
		u, err := url.Parse(t.SiteBaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("telegram.site_base_url 必须为有效的站点 HTTP(S) 地址")
		}
	}
	for _, id := range t.AllowedUserIDs {
		if id <= 0 {
			return errors.New("telegram.allowed_user_ids 必须为正整数用户 ID")
		}
	}
	return nil
}

// TelegramSettings returns an isolated copy of the validated live configuration.
func (m *Manager) TelegramSettings() Telegram {
	if m == nil {
		cfg := Telegram{}
		_ = cfg.Validate()
		return cfg
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg := m.current.Telegram
	root := cfg.StorageDirectory
	if root == "" {
		root = m.telegramRoot
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(m.workingDir, root)
	}
	cfg.LocalFilesRoot = filepath.Clean(root)
	cfg.AllowedUserIDs = append([]int64{}, cfg.AllowedUserIDs...)
	return cfg
}
