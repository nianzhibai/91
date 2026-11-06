package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	mt "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/telegramstorage"
	"github.com/video-site/backend/internal/mediaimport"
)

func TestParseMessageLinks(t *testing.T) {
	for _, tc := range []struct {
		text string
		want messageLink
	}{
		{"https://t.me/chayinb/26307?single", messageLink{Username: "chayinb", MessageID: 26307}},
		{" t.me/chayinb/26307\n", messageLink{Username: "chayinb", MessageID: 26307}},
		{"https://t.me/s/chayinb/26307?single&t=10#video", messageLink{Username: "chayinb", MessageID: 26307}},
		{"http://telegram.me/channel/123/456", messageLink{Username: "channel", MessageID: 456}},
		{"https://t.me/c/123456789/26307?single", messageLink{ChannelID: 123456789, MessageID: 26307}},
		{"https://t.me/c/123456789/100/26307", messageLink{ChannelID: 123456789, MessageID: 26307}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, ok := parseMessageLink(tc.text)
			if !ok || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, ok, tc.want)
			}
		})
	}
	for _, text := range []string{
		"", "/help", "https://example.com/channel/1", "https://t.me.evil.test/channel/1",
		"https://t.me@evil.test/channel/1", "https://user@t.me/channel/1", "https://t.me:443/channel/1",
		"https://t.me/channel", "https://t.me/+invite", "https://t.me/channel/0", "https://t.me/channel/-1",
		"https://t.me/channel/2147483648", "https://t.me/c/0/1", "https://t.me/c/123/0/1",
		"https://t.me/channel/1?comment=2", "https://t.me/channel/1 https://t.me/channel/2",
	} {
		if got, ok := parseMessageLink(text); ok {
			t.Errorf("accepted %q: %+v", text, got)
		}
	}
}

func linkVideoMessage(id int, group int64) *tg.Message {
	return &tg.Message{ID: id, Date: 100, PeerID: &tg.PeerChannel{ChannelID: 777}, GroupedID: group, Media: &tg.MessageMediaDocument{Document: &tg.Document{
		ID: int64(id + 1000), DCID: 4, AccessHash: 88, FileReference: []byte("fresh"), Size: 5, MimeType: "video/mp4",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{}, &tg.DocumentAttributeFilename{FileName: fmt.Sprintf("clip-%d.mp4", id)}},
	}}}
}

func TestMTProtoResolvesWholeLinkedAlbumsAndRefreshesChannelReferences(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, targetID := range []int{100, 105, 109} {
			t.Run(fmt.Sprintf("private=%t/target=%d", private, targetID), func(t *testing.T) {
				link := messageLink{Username: "chayinb", MessageID: targetID}
				if private {
					link.Username, link.ChannelID = "", 777
				}
				members := make(map[int]*tg.Message)
				members[100] = &tg.Message{ID: 100, Date: 100, GroupedID: 22, Message: "媒体组共同说明", Media: &tg.MessageMediaPhoto{}}
				for id := 101; id <= 109; id++ {
					members[id] = linkVideoMessage(id, 22)
				}
				members[99] = linkVideoMessage(99, 33)
				var requested []int
				api := tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
					switch request := input.(type) {
					case *tg.ContactsResolveUsernameRequest:
						if private || request.Username != "chayinb" {
							t.Fatalf("wrong public channel: %+v", request)
						}
						*output.(*tg.ContactsResolvedPeer) = tg.ContactsResolvedPeer{Peer: &tg.PeerChannel{ChannelID: 777}, Chats: []tg.ChatClass{&tg.Channel{ID: 777, AccessHash: 99}}}
					case *tg.ChannelsGetChannelsRequest:
						if !private || request.ID[0].(*tg.InputChannel).ChannelID != 777 {
							t.Fatal("wrong private channel")
						}
						output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: 777, AccessHash: 99}}}
					case *tg.ChannelsGetMessagesRequest:
						ch := request.Channel.(*tg.InputChannel)
						if ch.ChannelID != 777 || ch.AccessHash != 99 {
							t.Fatalf("lost channel reference: %+v", ch)
						}
						var messages []tg.MessageClass
						// Return messages in reverse order to exercise album sorting.
						for i := len(request.ID) - 1; i >= 0; i-- {
							id := request.ID[i].(*tg.InputMessageID).ID
							requested = append(requested, id)
							if msg := members[id]; msg != nil {
								messages = append(messages, msg)
								if len(request.ID) > 1 {
									messages = append(messages, msg) // Duplicate responses must not duplicate videos.
								}
							}
						}
						output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: messages}
					default:
						t.Fatalf("unexpected RPC: %T", input)
					}
					return nil
				}))
				c := &mtprotoClient{api: api}
				messages, err := c.ResolveMediaLink(context.Background(), link)
				if err != nil || len(messages) != 10 || messages[0].Caption != "媒体组共同说明" {
					t.Fatalf("album incomplete: %+v, %v", messages, err)
				}
				for i, msg := range messages {
					if msg.ID != int64(100+i) || msg.MediaGroupID != "777:22" {
						t.Fatalf("wrong album order or identity: %+v", msg)
					}
					if i == 0 {
						continue
					}
					ref, err := decodeDocument(msg.Video.FileID)
					if err != nil || ref.ChannelID != 777 || ref.ChannelHash != 99 || ref.MessageID != int(msg.ID) {
						t.Fatalf("source was not retained: %+v %v", ref, err)
					}
					// A new client must refresh from the source channel after restart.
					ref.FileReference = []byte("expired")
					fresh, err := (&mtprotoClient{api: api}).refreshDocument(context.Background(), ref)
					if err != nil || string(fresh.FileReference) != "fresh" || fresh.ID != ref.ID {
						t.Fatalf("channel reference was not refreshed: %+v %v", fresh, err)
					}
				}
				if len(requested) < 19 {
					t.Fatal("did not fetch album neighbors")
				}
			})
		}
	}
}

func TestMTProtoKeepsLinkTextWithWebPreview(t *testing.T) {
	c := &mtprotoClient{}
	link := "https://t.me/chayinb/26307?single"
	batch, err := c.normalize(context.Background(), &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 42}, Message: link, Media: &tg.MessageMediaWebPage{}}},
	}})
	if err != nil || len(batch) != 1 || batch[0].Message.Text != link || batch[0].Message.Video != nil {
		t.Fatalf("lost original link: %+v %v", batch, err)
	}
}

func TestMTProtoBotResolvesChannelsWithoutFullAccessHash(t *testing.T) {
	for _, min := range []bool{false, true} {
		t.Run(fmt.Sprintf("min=%t", min), func(t *testing.T) {
			api := tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
				switch request := input.(type) {
				case *tg.ContactsResolveUsernameRequest:
					channel := &tg.Channel{ID: 777, Min: min}
					if min {
						channel.AccessHash = 88
					}
					*output.(*tg.ContactsResolvedPeer) = tg.ContactsResolvedPeer{Peer: &tg.PeerChannel{ChannelID: 777}, Chats: []tg.ChatClass{channel}}
				case *tg.ChannelsGetMessagesRequest:
					if request.Channel.(*tg.InputChannel).AccessHash != 0 || len(request.ID) != 1 {
						t.Fatal("bot did not use a zero channel hash")
					}
					output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{linkVideoMessage(7, 0)}}
				default:
					t.Fatalf("unexpected RPC: %T", input)
				}
				return nil
			}))
			messages, err := (&mtprotoClient{api: api}).ResolveMediaLink(context.Background(), messageLink{Username: "channel", MessageID: 7})
			if err != nil || len(messages) != 1 || messages[0].Video == nil || messages[0].MediaGroupID != "" {
				t.Fatalf("bot could not resolve a single video: %+v %v", messages, err)
			}
		})
	}
}

func TestMTProtoRejectsUnavailableLinkedMediaWithoutPartialAlbums(t *testing.T) {
	for _, mode := range []string{"missing channel", "missing message", "non-video", "album access lost", "document changed"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			api := tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
				switch request := input.(type) {
				case *tg.ContactsResolveUsernameRequest:
					if mode == "missing channel" {
						return &tgerr.Error{Code: 400, Type: "USERNAME_NOT_OCCUPIED", Message: "SECRET"}
					}
					*output.(*tg.ContactsResolvedPeer) = tg.ContactsResolvedPeer{Peer: &tg.PeerChannel{ChannelID: 777}, Chats: []tg.ChatClass{&tg.Channel{ID: 777, AccessHash: 99}}}
				case *tg.ChannelsGetMessagesRequest:
					calls++
					if mode == "album access lost" && calls > 1 {
						return &tgerr.Error{Code: 406, Type: "CHANNEL_PRIVATE", Message: "SECRET"}
					}
					var messages []tg.MessageClass
					if mode != "missing message" {
						msg := linkVideoMessage(request.ID[0].(*tg.InputMessageID).ID, 0)
						if mode == "non-video" {
							msg.Media = &tg.MessageMediaPhoto{}
						}
						if mode == "album access lost" {
							msg.GroupedID = 22
						}
						messages = append(messages, msg)
					}
					output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: messages}
				default:
					t.Fatalf("unexpected RPC: %T", input)
				}
				return nil
			}))
			c := &mtprotoClient{api: api}
			var err error
			if mode == "document changed" {
				_, err = c.refreshDocument(context.Background(), documentReference{ID: 99, MessageID: 7, ChannelID: 777, ChannelHash: 99})
			} else {
				var messages []*message
				messages, err = c.ResolveMediaLink(context.Background(), messageLink{Username: "chayinb", MessageID: 7})
				if len(messages) > 0 {
					t.Fatal("failed resolution returned a partial group")
				}
			}
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
}

func linkUpdate(id, sender int64) update {
	u := videoUpdate(id, sender)
	u.Message.Video, u.Message.Caption = nil, ""
	u.Message.Text = "https://t.me/chayinb/26307?single"
	return u
}

func TestLinkedAlbumSharesTitlesDirectoryAndOneReceipt(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	s.client = &fakeTransport{resolve: func(ctx context.Context, link messageLink) ([]*message, error) {
		return []*message{albumPhoto(100, "整组标题\n完整说明").Message, albumVideo(101, "").Message, albumVideo(102, "").Message}, nil
	}}
	for _, id := range []int64{7, 7, 8} {
		if err := s.accept(ctx, linkUpdate(id, 42)); err != nil {
			t.Fatal(err)
		}
	}
	jobs := albumJobs(t, cat)
	if len(jobs) != 2 || jobs["unique-101"].RequestedTitle != "整组标题 完整说明 - 1" || jobs["unique-102"].RequestedTitle != "整组标题 完整说明 - 2" {
		t.Fatalf("wrong album titles or duplicates: %+v", jobs)
	}
	var directory string
	for _, job := range jobs {
		source, err := catalog.DecodeTelegramSource(job)
		if err != nil || source.SenderID != 42 || source.MediaGroupTitle != "整组标题\n完整说明" {
			t.Fatalf("lost sender or caption: %+v %v", source, err)
		}
		file, err := s.Fetch(ctx, job, func(string, int64, int64) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if directory != "" && directory != filepath.Dir(file.Path) {
			t.Fatal("linked album split into different directories")
		}
		directory = filepath.Dir(file.Path)
	}
	receipts, err := cat.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 2 || len(receipts[0].JobIDs) != 2 || len(receipts[1].JobIDs) != 2 || receipts[0].MessageID != 7 || receipts[1].MessageID != 8 {
		t.Fatalf("each link must have one receipt: %+v %v", receipts, err)
	}
}

func TestLinkAdmissionChecksAccessSizeAndCancellation(t *testing.T) {
	for _, mode := range []string{"unauthorized", "group", "oversized", "unavailable", "timeout", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, cat := testService(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.client = &fakeTransport{resolve: func(context.Context, messageLink) ([]*message, error) {
				calls++
				if mode == "unavailable" {
					return nil, messageLinkError(&tgerr.Error{Code: 406, Type: "CHANNEL_PRIVATE", Message: "SECRET"})
				}
				if mode == "timeout" {
					return nil, context.DeadlineExceeded
				}
				if mode == "canceled" {
					cancel()
					return nil, context.Canceled
				}
				first, second := albumVideo(100, "标题"), albumVideo(101, "")
				second.Message.Video.Size = s.cfg.MaxFileSizeBytes + 1
				return []*message{first.Message, second.Message}, nil
			}}
			u := linkUpdate(7, 42)
			if mode == "unauthorized" {
				u.Message.From.ID = 99
			}
			if mode == "group" {
				u.Message.Chat.Type = "group"
			}
			err := s.accept(ctx, u)
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was acknowledged: %v", err)
			} else if mode != "canceled" && err != nil {
				t.Fatal(err)
			}
			jobs, err := cat.ListImportJobs(context.Background(), "telegram", "", 0, 30)
			if err != nil || len(jobs) != 0 {
				t.Fatal("rejected link created import jobs", jobs, err)
			}
			if (mode == "unauthorized" || mode == "group") && calls != 0 {
				t.Fatal("unauthorized message resolved a link")
			}
			if mode == "unavailable" || mode == "timeout" {
				receipts, err := cat.PendingTelegramReceipts(context.Background(), 123)
				if err != nil || len(receipts) != 1 || strings.Contains(renderReceiptMessage(receipts[0]).text, "SECRET") {
					t.Fatalf("invalid link error receipt: %+v %v", receipts, err)
				}
			}
		})
	}
}

func TestLinkedVideosImportAndReportWholeGroupResults(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=32x32:d=0.2", "-c:v", "mpeg4", "-y", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, album := range []bool{false, true} {
		t.Run(fmt.Sprintf("album=%t", album), func(t *testing.T) {
			s, cat := testService(t)
			ctx := context.Background()
			transport := &fakeTransport{download: func(ctx context.Context, id string, w io.WriterAt) error {
				_, err := w.WriteAt(data, 0)
				return err
			}, resolve: func(context.Context, messageLink) ([]*message, error) {
				first := albumVideo(101, "共同标题").Message
				first.Video.Size = int64(len(data))
				if !album {
					first.MediaGroupID = ""
					return []*message{first}, nil
				}
				second := albumVideo(102, "").Message
				second.Video.Size = int64(len(data))
				return []*message{first, second}, nil
			}}
			s.client = transport
			saved := make(chan *catalog.Video, 2)
			manager, err := mediaimport.New(mediaimport.Config{Catalog: cat, TelegramSource: s, UploadDir: s.uploadDir, DiskReserve: 1, OnVideoUploaded: func(v *catalog.Video) { saved <- v }})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				if err := manager.Shutdown(shutdownCtx); err != nil {
					t.Error(err)
				}
			}()
			s.SetWake(manager.Wake)
			if err := s.accept(ctx, linkUpdate(7, 42)); err != nil {
				t.Fatal(err)
			}
			count := 1
			if album {
				count = 2
			}
			var first *catalog.Video
			for i := 0; i < count; i++ {
				select {
				case video := <-saved:
					if first == nil {
						first = video
					}
					if video.DriveID != telegramstorage.DriveID || video.ParentID != first.ParentID || video.DirName != first.DirName {
						t.Fatalf("linked videos did not share a collection: %+v", video)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("linked video was not published")
				}
			}
			receipts, err := cat.PendingTelegramReceipts(ctx, 123)
			if err != nil || len(receipts) != 1 {
				t.Fatalf("invalid linked import receipt: %+v %v", receipts, err)
			}
			transport.send = func(ctx context.Context, r catalog.TelegramReceipt, m botMessage) (int64, error) {
				want := "已保存"
				if album {
					want = "已保存：2 / 2"
				}
				if !strings.Contains(m.text, want) || r.MessageID != 7 {
					t.Fatalf("wrong final link reply: %+v %s", r, m.text)
				}
				return 55, nil
			}
			s.notifyOne(ctx, transport, receipts[0])
			if err := s.accept(ctx, linkUpdate(8, 42)); err != nil {
				t.Fatal(err)
			}
			if jobs, err := cat.ListImportJobs(ctx, "telegram", "", 0, 30); err != nil || len(jobs) != count {
				t.Fatalf("completed link submission duplicated videos: %+v %v", jobs, err)
			}
			receipts, err = cat.PendingTelegramReceipts(ctx, 123)
			if err != nil || len(receipts) != 1 || receipts[0].MessageID != 8 || len(receipts[0].DuplicateJobIDs) != count {
				t.Fatalf("repeat link lost duplicate metadata: %+v %v", receipts, err)
			}
			transport.send = func(ctx context.Context, r catalog.TelegramReceipt, message botMessage) (int64, error) {
				if !strings.Contains(message.text, "重复") || !strings.Contains(message.text, "已跳过下载和入库") {
					t.Fatalf("repeat link did not explain duplicates: %s", message.text)
				}
				return 56, nil
			}
			s.notifyOne(ctx, transport, receipts[0])
		})
	}
}
