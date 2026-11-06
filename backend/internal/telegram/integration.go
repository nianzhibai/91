package telegram

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/drives/telegramstorage"
	"github.com/video-site/backend/internal/mediaimport"
)

// Integration consumes the validated YAML snapshot and replaces receiver sessions.
// Receiver sessions own their MTProto connections, update state and downloads.
type Integration struct {
	cat           *catalog.Catalog
	configManager *config.Manager
	uploadDir     string
	reserve       int64
	mu            sync.RWMutex
	active        *Service
	status        Status
	wake          func()
	newTransport  transportFactory
	cancel        context.CancelFunc
	done          chan struct{}
}

func NewIntegration(cat *catalog.Catalog, manager *config.Manager, uploadDir string, reserve int64) *Integration {
	cfg := manager.TelegramSettings()
	return &Integration{cat: cat, configManager: manager, uploadDir: uploadDir, reserve: reserve,
		status: Status{State: "disabled", Config: cfg}, newTransport: newMTProtoClient}
}
func (i *Integration) SetWake(wake func()) { i.mu.Lock(); i.wake = wake; i.mu.Unlock() }
func (i *Integration) Start(ctx context.Context) {
	ctx, i.cancel = context.WithCancel(ctx)
	i.done = make(chan struct{})
	go func() {
		defer close(i.done)
		defer i.stop()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			i.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (i *Integration) Shutdown(ctx context.Context) error {
	if i.cancel == nil {
		return nil
	}
	i.cancel()
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (i *Integration) session() *Service { i.mu.RLock(); defer i.mu.RUnlock(); return i.active }
func (i *Integration) Status() Status {
	i.mu.RLock()
	s, status := i.active, i.status
	i.mu.RUnlock()
	if s != nil {
		status = s.Status()
	} else if n, err := mediaimport.AvailableBytes(status.Config.LocalFilesRoot); err == nil {
		status.CacheAvailableBytes = n
	}
	return status
}
func (i *Integration) Available() bool { s := i.session(); return s != nil && s.Available() }
func (i *Integration) BotID() int64 {
	if s := i.session(); s != nil {
		return s.BotID()
	}
	return 0
}
func (i *Integration) Allowed(id int64) bool { s := i.session(); return s != nil && s.Allowed(id) }
func (i *Integration) Resume(ctx context.Context) error {
	if s := i.session(); s != nil {
		return s.Resume(ctx)
	}
	return errors.New("请先启用 Telegram 并等待连接配置生效")
}
func (i *Integration) Test(ctx context.Context) (string, error) {
	if s := i.session(); s != nil {
		return s.Test(ctx)
	}
	return "", errors.New("请先启用 Telegram 并等待连接配置生效")
}
func (i *Integration) Fetch(ctx context.Context, j *catalog.RemoteUploadJob, progress func(string, int64, int64) error) (mediaimport.SourceFile, error) {
	if s := i.session(); s != nil {
		file, err := s.Fetch(ctx, j, progress)
		if ctx.Err() == nil && (i.session() != s || (s.runCtx != nil && s.runCtx.Err() != nil)) {
			return mediaimport.SourceFile{}, &mediaimport.SourceError{Message: "Telegram 配置已变化，等待重新连接", WaitForAvailability: true, RetryAfter: time.Second}
		}
		return file, err
	}
	return mediaimport.SourceFile{}, &mediaimport.SourceError{Message: "Telegram 正在应用配置，请稍候", WaitForAvailability: true, RetryAfter: time.Second}
}
func (i *Integration) stop() {
	i.mu.Lock()
	s := i.active
	i.active = nil
	i.mu.Unlock()
	if s != nil {
		// Cancellation interrupts reception, notifications and outstanding downloads.
		// Wait for the old receiver before exposing a new bot identity.
		_ = s.Shutdown(context.Background())
	}
}
func (i *Integration) setStatus(cfg config.Telegram, state, message string) {
	i.mu.Lock()
	i.status = Status{Enabled: cfg.Enabled, Config: cfg, State: state, Error: message}
	i.mu.Unlock()
}
func (i *Integration) reconcile(ctx context.Context) {
	cfg := i.configManager.TelegramSettings()
	if active := i.session(); active != nil && !sameConnection(active.settings(), cfg) {
		i.stop()
	}
	if !cfg.Enabled {
		i.stop()
		i.setStatus(cfg, "disabled", "")
		return
	}
	if active := i.session(); active != nil {
		active.applySettings(cfg)
	} else {
		s := New(cfg, cfg.BotToken, i.cat, i.uploadDir, i.reserve)
		s.newTransport = i.newTransport
		// Read the live calendar timezone without restarting the receiver when
		// scheduling settings change.
		s.statusTimezone = func() string { return i.configManager.LiveSettings().NightlyTimezone }
		i.mu.RLock()
		wake := i.wake
		i.mu.RUnlock()
		s.SetWake(wake)
		s.setState("connecting", nil)
		s.Start(ctx)
		i.mu.Lock()
		i.active = s
		i.mu.Unlock()
	}
}

func (i *Integration) Discard(ctx context.Context, j *catalog.RemoteUploadJob) error {
	return telegramstorage.New(i.cat, func() string { return i.configManager.TelegramSettings().LocalFilesRoot }).Remove(ctx, j.ID+".media")
}

// Only credentials, transport and storage placement require a new session.
func sameConnection(a, b config.Telegram) bool {
	return a.APIID == b.APIID && a.APIHash == b.APIHash && a.BotToken == b.BotToken &&
		a.ConnectionProxy == b.ConnectionProxy && a.LocalFilesRoot == b.LocalFilesRoot &&
		a.DownloadThreads == b.DownloadThreads
}
