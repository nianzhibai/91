package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	mt "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/filelock"
)

func TestMTProtoDownloadsWithConfiguredParallelism(t *testing.T) {
	for _, threads := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(threads), func(t *testing.T) {
			transport, err := newMTProtoClient(config.Telegram{DownloadThreads: threads}, "123:token")
			if err != nil {
				t.Fatal(err)
			}
			c := transport.(*mtprotoClient)
			expected := threads
			if expected == 0 {
				expected = 6
			}
			data := make([]byte, 512*1024*expected*2+17)
			for i := range data {
				data[i] = byte(i % 251)
			}
			part, err := os.CreateTemp(t.TempDir(), "video-*")
			if err != nil {
				t.Fatal(err)
			}
			defer part.Close()
			var progress []int64
			writer := &downloadWriter{file: part, limit: int64(len(data)), progress: func(_ string, done, _ int64) error {
				progress = append(progress, done)
				return nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan struct{}, expected)
			release := make(chan struct{})
			var calls, active, maximum atomic.Int32
			api := tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
				request, ok := input.(*tg.UploadGetFileRequest)
				if !ok {
					return fmt.Errorf("unexpected download request: %T", input)
				}
				location := request.Location.(*tg.InputDocumentFileLocation)
				if location.ID != 9 || location.AccessHash != 17 || string(location.FileReference) != "reference" {
					return errors.New("download used another document")
				}
				current := active.Add(1)
				defer active.Add(-1)
				for previous := maximum.Load(); current > previous; previous = maximum.Load() {
					if maximum.CompareAndSwap(previous, current) {
						break
					}
				}
				if calls.Add(1) <= int32(expected) {
					started <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				var chunk []byte
				if request.Offset < int64(len(data)) {
					end := min(request.Offset+int64(request.Limit), int64(len(data)))
					chunk = data[request.Offset:end]
				}
				output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: chunk}
				return nil
			}))
			result := make(chan error, 1)
			go func() {
				result <- c.downloadDocument(ctx, api, documentReference{ID: 9, AccessHash: 17, FileReference: []byte("reference")}, writer)
			}()
			for range expected {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatalf("only %d of %d download workers started", calls.Load(), expected)
				}
			}
			close(release)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(part.Name())
			if err != nil || !bytes.Equal(actual, data) || writer.done != int64(len(data)) || maximum.Load() != int32(expected) || calls.Load() > int32(2*expected) {
				t.Fatalf("invalid parallel download: bytes=%d written=%d workers=%d requests=%d error=%v", len(actual), writer.done, maximum.Load(), calls.Load(), err)
			}
			for i, done := range progress {
				if done <= 0 || done > int64(len(data)) || (i > 0 && done < progress[i-1]) {
					t.Fatalf("invalid progress: %v", progress)
				}
			}
		})
	}
}

func TestMTProtoNormalizesPrivateVideosAndIgnoresOutgoingAndGroups(t *testing.T) {
	ctx := context.Background()
	state, err := openRuntimeState(filepath.Join(t.TempDir(), "state.json"), func() {})
	if err != nil {
		t.Fatal(err)
	}
	c := &mtprotoClient{token: "123:token", state: state}
	doc := &tg.Document{ID: 9, AccessHash: 17, FileReference: []byte("reference"), DCID: 4, Size: 100, MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{}, &tg.DocumentAttributeFilename{FileName: "clip.mp4"}}}
	input := &tg.Updates{Users: []tg.UserClass{&tg.User{ID: 42, AccessHash: 99}}, Updates: []tg.UpdateClass{
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 42}, Message: "标题", Date: 100, GroupedID: 12, Media: &tg.MessageMediaDocument{Document: doc}}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 8, Out: true, PeerID: &tg.PeerUser{UserID: 42}}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 9, PeerID: &tg.PeerChat{ChatID: 42}}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 10, PeerID: &tg.PeerUser{UserID: 42}, Message: "/id", Media: &tg.MessageMediaEmpty{}}},
	}}
	batch, err := c.normalize(ctx, input)
	if err != nil || len(batch) != 2 {
		t.Fatalf("batch %v %v", batch, err)
	}
	m := batch[0].Message
	if batch[0].ID != -7 || m.From.ID != 42 || m.MediaGroupID != "12" || m.Video.Name != "clip.mp4" || m.Caption != "标题" || m.Text != "" || batch[1].Message.Text != "/id" {
		t.Fatalf("bad message: %+v", m)
	}
	ref, err := decodeDocument(m.Video.FileID)
	if err != nil || ref.ID != 9 || ref.DCID != 4 || ref.MessageID != 7 || string(ref.FileReference) != "reference" {
		t.Fatalf("bad document: %+v %v", ref, err)
	}
	hash, ok, _ := state.GetUserAccessHash(ctx, 123, 42)
	if !ok || hash != 99 {
		t.Fatal("sender access hash not retained")
	}
}
func TestTelegramErrorsRedactDetailsAndKeepFloodWait(t *testing.T) {
	err := telegramError(&tgerr.Error{Code: 420, Type: "FLOOD_WAIT", Argument: 45, Message: "PRIVATE_TOKEN"})
	var api *APIError
	if !errors.As(err, &api) || api.RetryAfter != 45*time.Second || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("bad error: %v", err)
	}
	if got := telegramError(errors.New("PRIVATE_TOKEN")); strings.Contains(got.Error(), "PRIVATE") {
		t.Fatal("transport details leaked")
	}
}
func TestSecondReceiverCannotOpenSameBotSession(t *testing.T) {
	s, _ := testService(t)
	dir := filepath.Join(s.cfg.LocalFilesRoot, "sessions", "123")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "receiver.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := filelock.TryLock(lock); err != nil {
		t.Fatal(err)
	}
	defer filelock.Unlock(lock)
	c, err := newMTProtoClient(s.cfg, s.token)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Run(context.Background(), func(user) error { t.Fatal("connected despite lock"); return nil }, func(context.Context, []update) error { return nil }, func(bool) {})
	if err == nil || !strings.Contains(err.Error(), "已有接收实例") {
		t.Fatalf("lock ignored: %v", err)
	}
}

func TestMTProtoRefreshesFileReferencesAndRejectsEditedDocuments(t *testing.T) {
	ctx := context.Background()
	for _, id := range []int64{9, 10} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			c := &mtprotoClient{api: tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
				request := input.(*tg.MessagesGetMessagesRequest)
				if len(request.ID) != 1 || request.ID[0].(*tg.InputMessageID).ID != 7 {
					t.Fatal("requested another message")
				}
				output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: id, AccessHash: 88, FileReference: []byte("fresh"), DCID: 4}}}}}
				return nil
			}))}
			ref, err := c.refreshDocument(ctx, documentReference{ID: 9, DCID: 2, MessageID: 7, FileReference: []byte("expired")})
			if id != 9 {
				if err == nil {
					t.Fatal("download accepted an edited document")
				}
				return
			}
			if err != nil || ref.AccessHash != 88 || ref.DCID != 4 || string(ref.FileReference) != "fresh" {
				t.Fatalf("reference not refreshed: %+v %v", ref, err)
			}
		})
	}
}

func TestMTProtoReplyUsesEntitiesButtonAndOriginalMessage(t *testing.T) {
	ctx := context.Background()
	state, err := openRuntimeState(filepath.Join(t.TempDir(), "state.json"), func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetUserAccessHash(ctx, 123, 42, 99); err != nil {
		t.Fatal(err)
	}
	c := &mtprotoClient{state: state, api: tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		request := input.(*tg.MessagesSendMessageRequest)
		peer := request.Peer.(*tg.InputPeerUser)
		if peer.UserID != 42 || peer.AccessHash != 99 || request.Message != "已保存\n标题 & 内容" || len(request.Entities) != 1 || !request.NoWebpage {
			t.Fatalf("invalid message: %+v", request)
		}
		if request.ReplyTo.(*tg.InputReplyToMessage).ReplyToMsgID != 7 || request.RandomID == 0 {
			t.Fatal("missing reply or deduplication identity")
		}
		button := request.ReplyMarkup.(*tg.ReplyInlineMarkup).Rows[0].Buttons[0]
		if button.Type.(*tg.InlineButtonTypeURL).URL != "https://example.com/video/1" {
			t.Fatal("lost video link")
		}
		output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 55}
		return nil
	}))}
	id, err := c.Send(ctx, catalog.TelegramReceipt{BotID: 123, ChatID: 42, MessageID: 7, UpdateID: -7}, botMessage{text: "<b>已保存</b>\n标题 &amp; 内容", videoURL: "https://example.com/video/1"})
	if err != nil || id != 55 {
		t.Fatalf("reply: %d %v", id, err)
	}
}

func TestTelegramProxyRoutesPrimaryMediaAndCDNConnections(t *testing.T) {
	targets := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("expected CONNECT")
			w.WriteHeader(405)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		header := make([]byte, 4)
		if _, err := io.ReadFull(rw, header); err != nil {
			t.Error(err)
			return
		}
		targets <- r.Host
	}))
	defer server.Close()
	client, err := newMTProtoClient(config.Telegram{ConnectionProxy: server.URL}, "123:token")
	if err != nil {
		t.Fatal(err)
	}
	resolver := client.(*mtprotoClient).resolver
	list := dcs.List{Options: []tg.DCOption{
		{ID: 1, IPAddress: "192.0.2.1", Port: 443},
		{ID: 1, IPAddress: "192.0.2.2", Port: 443, MediaOnly: true},
		{ID: 1, IPAddress: "192.0.2.3", Port: 443, CDN: true},
	}}
	for i, want := range []string{"192.0.2.1:443", "192.0.2.2:443", "192.0.2.3:443"} {
		connect := resolver.Primary
		if i == 1 {
			connect = resolver.MediaOnly
		}
		if i == 2 {
			connect = resolver.CDN
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := connect(ctx, 1, list)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		select {
		case got := <-targets:
			if got != want {
				t.Fatalf("wrong proxied data center: %s want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("Telegram connection bypassed configured proxy")
		}
	}
}
