package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/mediaimport"
	"gopkg.in/yaml.v3"
)

func integrationFixture(t *testing.T) (*Integration, config.Telegram) {
	t.Helper()
	s, cat := testService(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg
	cfg.StorageDirectory = cfg.LocalFilesRoot
	i := NewIntegration(cat, manager, s.uploadDir, 1)
	i.newTransport = func(config.Telegram, string) (transport, error) { return &fakeTransport{}, nil }
	return i, cfg
}
func saveTelegramYAML(t *testing.T, i *Integration, cfg config.Telegram) {
	t.Helper()
	raw, err := yaml.Marshal(map[string]any{"telegram": cfg})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := i.configManager.ReplaceYAML(raw, ""); err != nil || result.RestartRequired {
		t.Fatalf("save: %+v %v", result, err)
	}
}
func waitForIntegration(t *testing.T, i *Integration, state string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if i.Status().State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %s, got %+v", state, i.Status())
}
func TestIntegrationAppliesChangesAndCancelsPreviousSession(t *testing.T) {
	i, cfg := integrationFixture(t)
	saveTelegramYAML(t, i, cfg)
	ctx := context.Background()
	i.reconcile(ctx)
	defer i.stop()
	waitForIntegration(t, i, "connected")
	old := i.session()
	for _, proxy := range []string{"socks5h://user:password@proxy.example:1080", ""} {
		cfg.UploadDriveID = "cloud"
		cfg.UploadProxy = proxy
		saveTelegramYAML(t, i, cfg)
		i.reconcile(ctx)
		if i.session() != old || old.runCtx.Err() != nil {
			t.Fatal("upload settings interrupted reception")
		}
		raw, _ := json.Marshal(i.Status())
		if strings.Contains(string(raw), "password") || strings.Contains(string(raw), cfg.APIHash) {
			t.Fatal("status leaked credentials")
		}
	}
	cfg.AllowedUserIDs = []int64{77}
	saveTelegramYAML(t, i, cfg)
	i.reconcile(ctx)
	if i.session() != old || old.runCtx.Err() != nil || !i.Allowed(77) || i.Allowed(42) {
		t.Fatal("receiver retained old configuration")
	}
	cfg.Enabled = false
	saveTelegramYAML(t, i, cfg)
	i.reconcile(ctx)
	if i.session() != nil || i.Status().State != "disabled" {
		t.Fatal("disable retained receiver")
	}
}
func TestConfigurationChangeRetriesActiveDownload(t *testing.T) {
	i, cfg := integrationFixture(t)
	ctx := context.Background()
	s := New(cfg, cfg.BotToken, i.cat, i.uploadDir, 1)
	s.botID = 123
	s.status.State = "connected"
	s.runCtx, s.cancel = context.WithCancel(ctx)
	started := make(chan struct{})
	s.client = &fakeTransport{download: func(ctx context.Context, _ string, _ io.WriterAt) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	i.active = s
	if err := s.accept(ctx, videoUpdate(1, 42)); err != nil {
		t.Fatal(err)
	}
	jobs, _ := i.cat.ListImportJobs(ctx, "telegram", "", 0, 30)
	result := make(chan error, 1)
	go func() {
		_, err := i.Fetch(ctx, jobs[0], func(string, int64, int64) error { return nil })
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	i.stop()
	select {
	case err := <-result:
		var retry *mediaimport.SourceError
		if !errors.As(err, &retry) || !retry.WaitForAvailability {
			t.Fatalf("not retried: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("download was not canceled")
	}
}

func TestPolicyChangesKeepActiveDownload(t *testing.T) {
	i, cfg := integrationFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	i.newTransport = func(config.Telegram, string) (transport, error) {
		return &fakeTransport{download: func(ctx context.Context, _ string, w io.WriterAt) error {
			if _, err := w.WriteAt([]byte("vi"), 0); err != nil {
				return err
			}
			close(started)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				_, err := w.WriteAt([]byte("deo"), 2)
				return err
			}
		}}, nil
	}
	saveTelegramYAML(t, i, cfg)
	i.reconcile(context.Background())
	defer i.stop()
	waitForIntegration(t, i, "connected")
	old := i.session()
	if err := old.accept(context.Background(), videoUpdate(1, 42)); err != nil {
		t.Fatal(err)
	}
	jobs, _ := i.cat.ListImportJobs(context.Background(), "telegram", "", 0, 30)
	result := make(chan error, 1)
	go func() {
		_, err := i.Fetch(context.Background(), jobs[0], func(string, int64, int64) error { return nil })
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not start")
	}
	cfg.SiteBaseURL = "https://new.example.com"
	cfg.MaxPendingJobs = 1
	cfg.MaxFileSizeBytes = 100
	cfg.FetchTimeoutSeconds = 60
	cfg.AllowedUserIDs = []int64{42, 77}
	saveTelegramYAML(t, i, cfg)
	i.reconcile(context.Background())
	if i.session() != old || old.runCtx.Err() != nil || !i.Allowed(77) {
		t.Fatal("policy update replaced or canceled the receiver")
	}
	if got := i.Status().Config; got.SiteBaseURL != cfg.SiteBaseURL || got.MaxPendingJobs != 1 || got.MaxFileSizeBytes != 100 || got.FetchTimeoutSeconds != 60 {
		t.Fatalf("policy update not visible: %+v", got)
	}
	u := videoUpdate(2, 77)
	u.Message.Video.UniqueID = "another-video"
	if err := old.accept(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := i.cat.ListImportJobs(context.Background(), "telegram", "", 0, 30); len(jobs) != 1 {
		t.Fatal("new queue limit was not applied")
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("active download failed after policy update", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active download did not finish")
	}
}
func TestUnconfiguredIntegrationStartsWithoutNetworkAndStops(t *testing.T) {
	i, _ := integrationFixture(t)
	i.Start(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := i.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if i.session() != nil || i.Status().State != "disabled" {
		t.Fatal("unconfigured receiver started")
	}
	if _, err := i.Test(ctx); err == nil {
		t.Fatal("probe succeeded without a receiver")
	}
}

func TestDownloadThreadsApplyLive(t *testing.T) {
	i, cfg := integrationFixture(t)
	ctx := context.Background()
	saveTelegramYAML(t, i, cfg)
	i.reconcile(ctx)
	defer i.stop()
	waitForIntegration(t, i, "connected")
	for _, threads := range []int{1, 8} {
		old := i.session()
		cfg.DownloadThreads = threads
		saveTelegramYAML(t, i, cfg)
		i.reconcile(ctx)
		waitForIntegration(t, i, "connected")
		if i.session() == old || old.runCtx.Err() == nil || i.session().cfg.DownloadThreads != threads || i.Status().Config.DownloadThreads != threads {
			t.Fatal("download threads did not apply to the replacement receiver")
		}
	}
}

func TestConnectionProxyChangesReconnectWithoutResettingCredentialsOrStorage(t *testing.T) {
	i, cfg := integrationFixture(t)
	ctx := context.Background()
	saveTelegramYAML(t, i, cfg)
	i.reconcile(ctx)
	defer i.stop()
	waitForIntegration(t, i, "connected")
	originalRoot := i.session().cfg.LocalFilesRoot
	for _, proxy := range []string{"socks5h://user:PRIVATE_PASSWORD@proxy.example:1080", "http://proxy.example:7890", ""} {
		old := i.session()
		cfg.ConnectionProxy = proxy
		saveTelegramYAML(t, i, cfg)
		i.reconcile(ctx)
		waitForIntegration(t, i, "connected")
		current := i.session()
		if current == old || old.runCtx.Err() == nil || current.cfg.ConnectionProxy != proxy || current.cfg.BotToken != cfg.BotToken || current.cfg.LocalFilesRoot != originalRoot {
			t.Fatal("proxy change did not replace the connection correctly")
		}
		raw, err := json.Marshal(i.Status())
		if err != nil || strings.Contains(string(raw), "PRIVATE_PASSWORD") || strings.Contains(string(raw), "connection_proxy") {
			t.Fatal("status exposed proxy credentials", err)
		}
	}
}
