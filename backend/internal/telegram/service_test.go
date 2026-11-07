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

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

type fakeTransport struct {
	run      func(context.Context, func(user) error, func(context.Context, []update) error, func(bool)) error
	download func(context.Context, string, io.WriterAt) error
	resolve  func(context.Context, messageLink) ([]*message, error)
	send     func(context.Context, catalog.TelegramReceipt, botMessage) (int64, error)
	commands func(context.Context) error
}

func (f *fakeTransport) Run(ctx context.Context, ready func(user) error, handle func(context.Context, []update) error, connection func(bool)) error {
	if f.run != nil {
		return f.run(ctx, ready, handle, connection)
	}
	if err := ready(user{ID: 123, Username: "test_bot"}); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeTransport) Synced() bool { return true }
func (f *fakeTransport) ResolveMediaLink(ctx context.Context, link messageLink) ([]*message, error) {
	if f.resolve != nil {
		return f.resolve(ctx, link)
	}
	return nil, errors.New("机器人无法访问该频道或群组")
}
func (f *fakeTransport) Self(context.Context) (user, error) {
	return user{ID: 123, Username: "test_bot"}, nil
}
func (f *fakeTransport) Download(ctx context.Context, id string, w io.WriterAt) error {
	if f.download != nil {
		return f.download(ctx, id, w)
	}
	_, err := w.WriteAt([]byte("video"), 0)
	return err
}
func (f *fakeTransport) Send(ctx context.Context, r catalog.TelegramReceipt, m botMessage) (int64, error) {
	if f.send != nil {
		return f.send(ctx, r, m)
	}
	return 55, nil
}
func (f *fakeTransport) SetCommands(ctx context.Context) error {
	if f.commands != nil {
		return f.commands(ctx)
	}
	return nil
}

func testService(t *testing.T) (*Service, *catalog.Catalog) {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	cfg := config.Telegram{Enabled: true, APIID: 12345, APIHash: "0123456789abcdef0123456789abcdef", BotToken: "123:test", AllowedUserIDs: []int64{42}, LocalFilesRoot: t.TempDir()}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, cfg.BotToken, cat, t.TempDir(), 1)
	s.botID = 123
	s.status.State = "connected"
	s.client = &fakeTransport{}
	if _, err := cat.TelegramPaused(context.Background(), 123); err != nil {
		t.Fatal(err)
	}
	return s, cat
}
func videoUpdate(id, userID int64) update {
	m := &message{ID: id, Date: 1789975384, From: user{ID: userID}, Caption: "海边日落", Video: &media{FileID: "file", UniqueID: "unique", Name: "sunset.mp4", Size: 5, MIME: "video/mp4"}}
	m.Chat.Type = "private"
	m.Chat.ID = userID
	return update{ID: id, Message: m}
}
func TestReceiptsDeduplicateMessagesAndFiles(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	for _, u := range []update{videoUpdate(1, 42), videoUpdate(1, 42), videoUpdate(2, 42), videoUpdate(3, 99)} {
		if err := s.accept(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	if len(jobs[0].Tags) != 0 {
		t.Fatalf("Telegram task must leave content tags to automatic matching: %v", jobs[0].Tags)
	}
	replies, err := cat.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(replies) != 2 {
		t.Fatalf("receipts=%v err=%v", replies, err)
	}
	if replies[0].JobID != replies[1].JobID {
		t.Fatal("duplicate file created separate tasks")
	}
	public, err := cat.ListRemoteUploadJobs(ctx, 30)
	if err != nil || len(public) != 0 {
		t.Fatal("Telegram tasks leaked into HTTP list")
	}
}
func TestQueueLimitsAndBootstrapID(t *testing.T) {
	s, cat := testService(t)
	s.cfg.MaxPendingJobs = 1
	ctx := context.Background()
	if err := s.accept(ctx, videoUpdate(1, 42)); err != nil {
		t.Fatal(err)
	}
	u := videoUpdate(2, 42)
	u.Message.Video.UniqueID = "second"
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	u = videoUpdate(3, 99)
	u.Message.Video = nil
	u.Message.Text = "/id"
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	receipts, err := cat.PendingTelegramReceipts(ctx, 123)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 3 || !strings.Contains(renderReceiptMessage(receipts[1]).text, "队列已满") || !strings.Contains(renderReceiptMessage(receipts[2]).text, "<code>99</code>") {
		t.Fatalf("receipts=%+v", receipts)
	}
}

func TestFetchDownloadsDirectlyAndReusesCompletedAcquisition(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	if err := s.accept(ctx, videoUpdate(1, 42)); err != nil {
		t.Fatal(err)
	}
	jobs, _ := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
	calls := 0
	s.client = &fakeTransport{download: func(ctx context.Context, id string, w io.WriterAt) error {
		calls++
		if id != "file" {
			t.Errorf("wrong reference: %s", id)
		}
		// Chunks may arrive out of order during a parallel download.
		if _, err := w.WriteAt([]byte("deo"), 2); err != nil {
			return err
		}
		_, err := w.WriteAt([]byte("vi"), 0)
		return err
	}}
	var progress []int64
	report := func(stage string, done, total int64) error { progress = append(progress, done); return nil }
	source, err := s.Fetch(ctx, jobs[0], report)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(source.Path)
	if err != nil || string(bytes) != "video" || filepath.Base(filepath.Dir(source.Path)) != "library" {
		t.Fatalf("source: %+v %v", source, err)
	}
	if _, err := s.Fetch(ctx, jobs[0], report); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(progress) < 2 || progress[len(progress)-1] != 5 {
		t.Fatalf("calls=%d progress=%v", calls, progress)
	}
}

func TestMediaGroupDownloadsUseSeparateNamedDirectories(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	first, second := albumVideo(1, "海边旅行\n共同说明"), albumVideo(2, "")
	other := albumVideo(3, "海边旅行")
	other.Message.MediaGroupID = "another-album"
	single := videoUpdate(4, 42)
	for _, u := range []update{first, second, other, single} {
		if err := s.accept(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	flushTestAlbum(t, s)
	jobs := albumJobs(t, cat)
	download := func(service *Service, unique string) string {
		t.Helper()
		file, err := service.Fetch(ctx, jobs[unique], func(string, int64, int64) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(file.Path); err != nil || string(data) != "video" {
			t.Fatal("downloaded content changed", err)
		}
		return filepath.Dir(file.Path)
	}
	firstDirectory := download(s, "unique-1")
	if filepath.Dir(firstDirectory) != filepath.Join(s.cfg.LocalFilesRoot, "library") || !strings.HasPrefix(filepath.Base(firstDirectory), "海边旅行-") {
		t.Fatalf("album did not use a named directory under library: %s", firstDirectory)
	}
	// A restart and a late caption must not split an already started group.
	restarted := New(s.cfg, s.token, cat, s.uploadDir, s.reserve)
	restarted.botID, restarted.client = s.BotID(), &fakeTransport{}
	source, err := catalog.DecodeTelegramSource(jobs["unique-2"])
	if err != nil {
		t.Fatal(err)
	}
	source.MediaGroupTitle = "后到的新说明"
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	jobs["unique-2"].SourcePayload = string(raw)
	if secondDirectory := download(restarted, "unique-2"); secondDirectory != firstDirectory {
		t.Fatalf("restarting split the album: %s and %s", firstDirectory, secondDirectory)
	}
	otherDirectory := download(restarted, "unique-3")
	if otherDirectory == firstDirectory || !strings.HasPrefix(filepath.Base(otherDirectory), "海边旅行-") {
		t.Fatal("same-title albums shared a directory")
	}
	if directory := download(restarted, "unique"); directory != filepath.Join(s.cfg.LocalFilesRoot, "library") {
		t.Fatal("single video was saved in an album directory")
	}
	if retryDirectory := download(restarted, "unique-1"); retryDirectory != firstDirectory {
		t.Fatal("retry changed the album directory")
	}
}
func TestFailedDownloadsLeaveNoPublishedOrPartialFile(t *testing.T) {
	for _, body := range []string{"vide", "video-too-large"} {
		t.Run(body, func(t *testing.T) {
			s, cat := testService(t)
			ctx := context.Background()
			if err := s.accept(ctx, videoUpdate(1, 42)); err != nil {
				t.Fatal(err)
			}
			jobs, _ := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
			s.client = &fakeTransport{download: func(ctx context.Context, id string, w io.WriterAt) error {
				_, err := w.WriteAt([]byte(body), 0)
				return err
			}}
			if _, err := s.Fetch(ctx, jobs[0], func(string, int64, int64) error { return nil }); err == nil {
				t.Fatal("invalid size accepted")
			}
			entries, err := os.ReadDir(filepath.Join(s.cfg.LocalFilesRoot, "library"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("partial artifacts survived: %v %v", entries, err)
			}
		})
	}
}
func TestFetchRechecksBotAndSender(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	_ = s.accept(ctx, videoUpdate(1, 42))
	jobs, _ := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
	s.client = &fakeTransport{download: func(context.Context, string, io.WriterAt) error {
		t.Fatal("unauthorized download")
		return errors.New("unexpected")
	}}
	s.cfg.AllowedUserIDs = nil
	if _, err := s.Fetch(ctx, jobs[0], func(string, int64, int64) error { return nil }); err == nil {
		t.Fatal("revoked sender downloaded")
	}
}
