package telegram

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/drives/telegramstorage"
	"github.com/nianzhibai/91/backend/internal/mediaimport"
	"github.com/nianzhibai/91/backend/internal/videoname"
)

type Status struct {
	Enabled              bool            `json:"enabled"`
	State                string          `json:"state"`
	BotID                string          `json:"botId"`
	Username             string          `json:"username"`
	Error                string          `json:"error"`
	LastConnected        time.Time       `json:"lastConnected"`
	LastMessage          time.Time       `json:"lastMessage"`
	CacheAvailableBytes  int64           `json:"cacheAvailableBytes"`
	NotificationFailures int             `json:"notificationFailures"`
	Config               config.Telegram `json:"config"`
}
type Service struct {
	wakeImports      func()
	statusTimezone   func() string // Optional live timezone provider, assigned before Start.
	cfg              config.Telegram
	token            string
	runCtx           context.Context
	cat              *catalog.Catalog
	uploadDir        string
	reserve          int64
	mu               sync.RWMutex
	mediaGroupMu     sync.Mutex
	status           Status
	client           transport
	newTransport     transportFactory
	downloadProgress downloadProgress
	botID            int64
	wg               sync.WaitGroup
	cancel           context.CancelFunc
}

func New(cfg config.Telegram, token string, cat *catalog.Catalog, uploadDir string, reserve int64) *Service {
	return &Service{cfg: cfg, token: token, cat: cat, uploadDir: uploadDir, reserve: reserve, status: Status{Enabled: cfg.Enabled, State: "disabled", Config: cfg}, newTransport: newMTProtoClient}
}
func (s *Service) SetWake(wake func()) { s.mu.Lock(); s.wakeImports = wake; s.mu.Unlock() }

func (s *Service) Start(ctx context.Context) {
	if !s.settings().Enabled {
		return
	}
	s.mu.Lock()
	s.botID = tokenBotID(s.token)
	s.mu.Unlock()
	ctx, s.cancel = context.WithCancel(ctx)
	s.runCtx = ctx
	s.wg.Add(4)
	go func() { defer s.wg.Done(); s.receive(ctx) }()
	go func() { defer s.wg.Done(); s.notify(ctx) }()
	go func() { defer s.wg.Done(); s.processMediaGroups(ctx) }()
	go func() { defer s.wg.Done(); s.syncCommandMenu(ctx) }()
}
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) Status() Status {
	s.mu.RLock()
	out := s.status
	s.mu.RUnlock()
	if n, err := mediaimport.AvailableBytes(s.settings().LocalFilesRoot); err == nil {
		out.CacheAvailableBytes = n
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if out.BotID != "" {
		id, _ := strconv.ParseInt(out.BotID, 10, 64)
		out.NotificationFailures = s.cat.TelegramNotificationFailures(ctx, id)
	}
	return out
}
func (s *Service) Available() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status.State == "connected"
}
func (s *Service) setState(state string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = state
	s.status.Error = ""
	if err != nil {
		s.status.Error = err.Error()
	}
}
func (s *Service) settings() config.Telegram {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Service) applySettings(cfg config.Telegram) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.status.Config = cfg
}

func (s *Service) Allowed(id int64) bool {
	for _, allowed := range s.settings().AllowedUserIDs {
		if allowed == id {
			return true
		}
	}
	return false
}
func (s *Service) BotID() int64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.botID }
func (s *Service) Resume(ctx context.Context) error {
	return s.cat.ResumeTelegram(ctx, tokenBotID(s.token))
}

func (s *Service) Test(ctx context.Context) (string, error) {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil || !s.Available() {
		return "", errors.New("Telegram 尚未连接，请启用后等待连接完成")
	}
	u, err := c.Self(ctx)
	return u.Username, err
}

func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (s *Service) receive(ctx context.Context) {
	for ctx.Err() == nil {
		botID := tokenBotID(s.token)
		paused, err := s.cat.TelegramPaused(ctx, botID)
		if err != nil {
			s.setState("error", errors.New("无法读取 Telegram 接收状态"))
		} else if paused {
			s.setState("needs_reconnect", errors.New("备份已恢复，请检查配置后恢复接收"))
		} else {
			s.setState("connecting", nil)
			c, connectErr := s.newTransport(s.settings(), s.token)
			if connectErr == nil {
				s.mu.Lock()
				s.client = c
				s.mu.Unlock()
				connectErr = c.Run(ctx, func(u user) error {
					if u.ID != botID {
						return errors.New("Telegram 会话与机器人身份不匹配")
					}
					s.mu.Lock()
					s.botID, s.status.BotID, s.status.Username = u.ID, strconv.FormatInt(u.ID, 10), u.Username
					s.status.LastConnected = time.Now()
					s.mu.Unlock()
					s.setState("connected", nil)
					return nil
				}, func(ctx context.Context, updates []update) error {
					// A complete batch is durable before albums can be flushed.
					s.mediaGroupMu.Lock()
					defer s.mediaGroupMu.Unlock()
					for _, u := range updates {
						if err := s.accept(ctx, u); err != nil {
							return err
						}
					}
					s.mu.Lock()
					s.status.LastMessage = time.Now()
					s.mu.Unlock()
					return nil
				}, func(connected bool) {
					s.mu.Lock()
					if s.status.State == "connected" || s.status.State == "connecting" {
						if connected && s.botID != 0 {
							s.status.State = "connected"
							s.status.LastConnected = time.Now()
						} else {
							s.status.State = "connecting"
						}
					}
					s.mu.Unlock()
				})
				s.mu.Lock()
				s.client = nil
				s.mu.Unlock()
			}
			if ctx.Err() != nil {
				return
			}
			err = connectErr
			s.setState("error", err)
		}
		delay := 5 * time.Second
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
			delay = apiErr.RetryAfter
		}
		if !wait(ctx, delay) {
			return
		}
	}
}

func (s *Service) accept(ctx context.Context, u update) error {
	// Keep the fallback timestamp in the buffered message too, so collecting an
	// album or restarting the receiver does not change an untitled video's name.
	if m := u.Message; m != nil && m.Date <= 0 {
		m.Date = time.Now().Unix()
	}
	if m := u.Message; m != nil && m.MediaGroupID != "" && m.Chat.Type == "private" && s.Allowed(m.From.ID) {
		return s.stageMediaGroup(ctx, u)
	}
	if m := u.Message; m != nil && m.Chat.Type == "private" && s.Allowed(m.From.ID) && videoMedia(m) == nil {
		if link, ok := parseMessageLink(m.Text); ok {
			return s.acceptMessageLink(ctx, u, link)
		}
	}
	input, err := s.prepareImport(ctx, u)
	if err != nil {
		return err
	}
	err = s.cat.AcceptTelegramUpdate(ctx, input.Receipt, input.Source, input.ID, input.Title, s.settings().MaxPendingJobs)
	if err == nil {
		s.wakeImportWorker()
	}
	return err
}

func (s *Service) prepareImport(ctx context.Context, u update) (catalog.TelegramImport, error) {
	botID := s.BotID()
	r := catalog.TelegramReceipt{BotID: botID, UpdateID: u.ID, MessageID: -u.ID - 1, Response: "__ignore__"}
	var source *catalog.TelegramSource
	title := ""
	if m := u.Message; m != nil {
		r.ChatID = m.Chat.ID
		r.MessageID = m.ID
		r.SenderID = m.From.ID
		if m.Chat.Type == "private" {
			command := ""
			if fields := strings.Fields(m.Text); len(fields) > 0 {
				command = strings.Split(fields[0], "@")[0]
			}
			if command == "/"+commandID {
				r.Response = responseUserID
			} else if command == "/"+commandStart || command == "/"+commandHelp {
				r.Response = responseNeedsAccess
				if s.Allowed(m.From.ID) {
					r.Response = responseHelp
				}
			} else if command == "/"+commandStatus {
				r.Response = responseNeedsAccess
				if s.Allowed(m.From.ID) {
					r.Response = responseStatus
				}
			} else if s.Allowed(m.From.ID) {
				r.Response = responseUnsupported
				file := videoMedia(m)
				if file != nil {
					r.Response = ""
					if file.FileID == "" || file.UniqueID == "" {
						r.Response = "视频文件信息不完整，请重新发送"
					} else if err := validateVideoSize(file.Size, s.settings().MaxFileSizeBytes); err != nil {
						r.Response = err.Error()
					} else {
						source = &catalog.TelegramSource{BotID: botID, SenderID: m.From.ID, FileID: file.FileID, UniqueID: file.UniqueID, FileName: file.Name, Size: file.Size, MIME: file.MIME}
						title = videoTitle(m.Caption, file.Name, m.Date)
						if err := s.invalidateMissingVideo(ctx, botID, file.UniqueID); err != nil {
							return catalog.TelegramImport{}, err
						}
					}
				}
			}
		}
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return catalog.TelegramImport{}, err
	}
	return catalog.TelegramImport{Receipt: r, Source: source, ID: "tg-" + hex.EncodeToString(random[:]), Title: title}, nil
}

func (s *Service) wakeImportWorker() {
	s.mu.RLock()
	wake := s.wakeImports
	s.mu.RUnlock()
	if wake != nil {
		wake()
	}
}
func (s *Service) invalidateMissingVideo(ctx context.Context, botID int64, unique string) error {
	id, err := s.cat.TelegramVideoID(ctx, botID, unique)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	v, err := s.cat.GetVideo(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && v != nil && v.DriveID == telegramstorage.DriveID {
		info, e := telegramstorage.New(s.cat, func() string { return s.settings().LocalFilesRoot }).Stat(ctx, v.FileID)
		if e == nil && info.Size > 0 {
			return nil
		}
		return s.cat.InvalidateMissingTelegramVideo(ctx, botID, unique)
	}
	if err == nil && v != nil && v.DriveID != "local-upload" {
		return nil
	}
	if err == nil && v != nil && filepath.Base(v.FileID) == v.FileID {
		info, e := os.Stat(filepath.Join(s.uploadDir, v.FileID))
		if e == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return nil
		}
	}
	return s.cat.InvalidateMissingTelegramVideo(ctx, botID, unique)
}
func isVideoDocument(m media) bool {
	if strings.HasPrefix(strings.ToLower(m.MIME), "video/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(m.Name)) {
	case ".mp4", ".mkv", ".webm", ".mov", ".avi":
		return true
	}
	return false
}
func videoMedia(m *message) *media {
	if m.Video != nil {
		return m.Video
	}
	if m.Document != nil && isVideoDocument(*m.Document) {
		return m.Document
	}
	return nil
}
func videoTitle(caption, name string, timestamp int64) string {
	return videoTitleWithSuffix(caption, name, timestamp, "")
}

func videoTitleWithSuffix(caption, name string, timestamp int64, suffix string) string {
	title := strings.TrimSpace(caption)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(strings.ReplaceAll(name, `\`, "/")), filepath.Ext(name))
	}
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return ' '
		}
		return r
	}, title)
	title = strings.Trim(strings.Join(strings.Fields(title), " "), ". ")
	for len(title)+len(suffix) > 120 {
		rs := []rune(title)
		title = string(rs[:len(rs)-1])
	}
	if title == "" || title == "." {
		title = strconv.FormatInt(timestamp, 10)
	}
	title = strings.TrimSpace(title) + suffix
	if videoname.ValidateUploadTitle(title, ".webm") != nil {
		title = strconv.FormatInt(timestamp, 10) + suffix
	}
	return title
}
