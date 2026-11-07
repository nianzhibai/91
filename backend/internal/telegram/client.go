package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	mt "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/sync/errgroup"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/filelock"
	"github.com/nianzhibai/91/backend/internal/tcpproxy"
)

// transport keeps MTProto details out of import admission and notifications.
type transport interface {
	Run(context.Context, func(user) error, func(context.Context, []update) error, func(bool)) error
	Self(context.Context) (user, error)
	Download(context.Context, string, io.WriterAt) error
	ResolveMediaLink(context.Context, messageLink) ([]*message, error)
	Send(context.Context, catalog.TelegramReceipt, botMessage) (int64, error)
	SetCommands(context.Context) error
	Synced() bool
}
type transportFactory func(config.Telegram, string) (transport, error)

type APIError struct {
	Code       int
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram 请求失败（%d）", e.Code) }

func telegramError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var outputErr *localError
	if errors.As(err, &outputErr) {
		return outputErr
	}
	var pathErr *os.PathError
	var linkErr *os.LinkError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) {
		return &localError{message: "Telegram 本地存储失败，请检查磁盘空间和目录权限", cause: err, retry: true}
	}
	var rpc *tgerr.Error
	if errors.As(err, &rpc) {
		delay := time.Duration(0)
		if strings.HasPrefix(rpc.Type, "FLOOD_WAIT") || strings.HasPrefix(rpc.Type, "FLOOD_PREMIUM_WAIT") {
			delay = time.Duration(rpc.Argument) * time.Second
		}
		return &APIError{Code: rpc.Code, RetryAfter: delay}
	}
	return &connectionError{}
}

type connectionError struct{}

func (*connectionError) Error() string { return "Telegram 连接或通信失败，请稍后重试" }

func tokenBotID(token string) int64 {
	id, _ := strconv.ParseInt(strings.SplitN(token, ":", 2)[0], 10, 64)
	return id
}

type mtprotoClient struct {
	cfg           config.Telegram
	token         string
	client        *mt.Client
	resolver      dcs.Resolver
	api           *tg.Client
	state         *runtimeState
	downloadPools downloadPools
	recovery      *updateRecovery
	authenticated atomic.Bool
}

func newMTProtoClient(cfg config.Telegram, token string) (transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dialer, err := tcpproxy.New(cfg.ConnectionProxy)
	if err != nil {
		return nil, err
	}
	return &mtprotoClient{cfg: cfg, token: token, resolver: dcs.Plain(dcs.PlainOptions{Dial: dialer.DialContext})}, nil
}

func (c *mtprotoClient) Run(parent context.Context, ready func(user) error, handle func(context.Context, []update) error, connection func(bool)) error {
	ctx, cancel := context.WithCancel(parent)
	defer c.downloadPools.close()
	defer cancel()
	dir := filepath.Join(c.cfg.LocalFilesRoot, "sessions", strconv.FormatInt(tokenBotID(c.token), 10))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("无法创建 Telegram 会话目录")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "receiver.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("无法打开 Telegram 会话锁")
	}
	defer lock.Close()
	if err := filelock.TryLock(lock); err != nil {
		return errors.New("此 Telegram 机器人已有接收实例，请停止其他实例")
	}
	defer filelock.Unlock(lock)
	identity := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", c.cfg.APIID, c.cfg.APIHash, c.token)))
	prefix := filepath.Join(dir, fmt.Sprintf("%x", identity[:16]))
	// Update sequences belong to the bot account, so credential rotation must
	// retain its cursor even though authorization keys use a new session file.
	state, err := openRuntimeState(filepath.Join(dir, "updates.json"), cancel)
	if err != nil {
		return errors.New("无法读取 Telegram 接收状态")
	}
	c.state = state
	c.recovery = newUpdateRecovery(state)
	handler := mt.UpdateHandlerFunc(func(ctx context.Context, input tg.UpdatesClass) error {
		batch, err := c.normalize(ctx, input)
		if err != nil {
			cancel()
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		// gotd logs handler errors and can then advance pts. Keep retrying here until
		// the entire batch is durable; canceled storage refuses cursor advancement.
		for ctx.Err() == nil {
			if err := handle(ctx, batch); err == nil {
				return nil
			}
			if !wait(ctx, time.Second) {
				break
			}
		}
		return ctx.Err()
	})
	manager := updates.New(updates.Config{Handler: handler, Storage: c.recovery, AccessHasher: state, UserAccessHasher: state})
	c.client = mt.NewClient(c.cfg.APIID, c.cfg.APIHash, mt.Options{
		Resolver:       c.resolver,
		SessionStorage: &atomicSession{path: prefix + ".session.json"},
		UpdateHandler:  manager,
		OnConnectionState: func(s mt.ConnectionState) {
			c.recovery.connection(s == mt.ConnectionStateReady)
			if c.authenticated.Load() {
				if s == mt.ConnectionStateReady {
					c.recovery.request()
				}
				connection(s == mt.ConnectionStateReady)
			}
		},
	})
	c.api = c.client.API()
	c.recovery.API = c.api
	err = c.client.Run(ctx, func(ctx context.Context) error {
		auth, err := c.client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !auth.Authorized {
			if _, err := c.client.Auth().Bot(ctx, c.token); err != nil {
				return err
			}
		}
		me, err := c.client.Self(ctx)
		if err != nil {
			return err
		}
		if !me.Bot || me.ID != tokenBotID(c.token) {
			return errors.New("机器人身份与会话不匹配")
		}
		group, runCtx := errgroup.WithContext(ctx)
		started := make(chan struct{})
		group.Go(func() error {
			return manager.Run(runCtx, c.recovery, me.ID, updates.AuthOptions{IsBot: true, OnStart: func(ctx context.Context) {
				if err := ready(user{ID: me.ID, Username: me.Username}); err != nil {
					cancel()
				} else {
					c.authenticated.Store(true)
				}
				close(started)
			}})
		})
		group.Go(func() error {
			select {
			case <-runCtx.Done():
				return runCtx.Err()
			case <-started:
				return c.recovery.run(runCtx, manager.Handle)
			}
		})
		return group.Wait()
	})
	if state.failure() != nil {
		return errors.New("无法保存 Telegram 接收状态，已停止接收")
	}
	return telegramError(err)
}

// Albums may flush only after getDifference has delivered the complete backlog
// and its final state is durable. Intermediate slices must not release them.
func (c *mtprotoClient) Synced() bool { return c.recovery != nil && c.recovery.Synced() }

func (c *mtprotoClient) Self(ctx context.Context) (user, error) {
	me, err := c.client.Self(ctx)
	if err != nil {
		return user{}, telegramError(err)
	}
	return user{ID: me.ID, Username: me.Username}, nil
}

// A document reference is private source metadata. It can be refreshed using
// its original message when Telegram expires file_reference.
type documentReference struct {
	ID            int64  `json:"id"`
	AccessHash    int64  `json:"accessHash"`
	FileReference []byte `json:"fileReference"`
	DCID          int    `json:"dcId"`
	MessageID     int    `json:"messageId"`
	ChannelID     int64  `json:"channelId,omitempty"`
	ChannelHash   int64  `json:"channelHash,omitempty"`
}

func encodeDocument(doc *tg.Document, messageID int, channel *tg.InputChannel) string {
	ref := documentReference{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference, DCID: doc.DCID, MessageID: messageID}
	if channel != nil {
		ref.ChannelID, ref.ChannelHash = channel.ChannelID, channel.AccessHash
	}
	data, _ := json.Marshal(ref)
	return base64.RawURLEncoding.EncodeToString(data)
}
func decodeDocument(raw string) (documentReference, error) {
	var ref documentReference
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err == nil {
		err = json.Unmarshal(data, &ref)
	}
	if err != nil || ref.ID == 0 || ref.DCID <= 0 || ref.MessageID <= 0 {
		return ref, errors.New("Telegram 文件引用无效，请重新发送视频")
	}
	return ref, nil
}

func (c *mtprotoClient) normalize(ctx context.Context, input tg.UpdatesClass) ([]update, error) {
	var items []tg.UpdateClass
	var users []tg.UserClass
	switch u := input.(type) {
	case *tg.Updates:
		items, users = u.Updates, u.Users
	case *tg.UpdatesCombined:
		items, users = u.Updates, u.Users
	case *tg.UpdateShort:
		items = []tg.UpdateClass{u.Update}
	}
	for _, value := range users {
		if u, ok := value.(*tg.User); ok && !u.Min && u.AccessHash != 0 {
			if err := c.state.SetUserAccessHash(ctx, tokenBotID(c.token), u.ID, u.AccessHash); err != nil {
				return nil, err
			}
		}
	}
	batch := make([]update, 0, len(items))
	for _, item := range items {
		incoming, ok := item.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		msg, ok := incoming.Message.(*tg.Message)
		if !ok || msg.Out {
			continue
		}
		peer, ok := msg.PeerID.(*tg.PeerUser)
		if !ok {
			continue
		}
		m := &message{ID: int64(msg.ID), Date: int64(msg.Date), From: user{ID: peer.UserID}, Caption: msg.Message}
		m.Chat.ID, m.Chat.Type = peer.UserID, "private"
		switch msg.Media.(type) {
		case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
			m.Text, m.Caption = msg.Message, ""
		}
		if msg.GroupedID != 0 {
			m.MediaGroupID = strconv.FormatInt(msg.GroupedID, 10)
		}
		if file, video := documentMedia(msg, nil); file != nil {
			if video {
				m.Video = file
			} else {
				m.Document = file
			}
		}
		if _, ok := msg.Media.(*tg.MessageMediaPhoto); ok {
			m.Photo = []media{{}}
		}
		// Private-message IDs are account-wide. Negative IDs separate MTProto
		// receipts from the retired Bot API update-id namespace.
		batch = append(batch, update{ID: -int64(msg.ID), Message: m})
	}
	return batch, nil
}

func (c *mtprotoClient) Download(ctx context.Context, raw string, output io.WriterAt) error {
	ref, err := decodeDocument(raw)
	if err != nil {
		return err
	}
	ref, err = c.refreshDocument(ctx, ref)
	if err != nil {
		return err
	}
	pool, err := c.downloadPools.get(ctx, ref.DCID, c.cfg.DownloadThreads, c.openDownloadPool)
	if err != nil {
		return telegramError(err)
	}
	err = c.downloadDocument(ctx, tg.NewClient(pool), ref, output)
	if tgerr.Is(err, "FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_EMPTY", "FILEREF_UPGRADE_NEEDED") {
		return &APIError{Code: 503, RetryAfter: time.Second}
	}
	return telegramError(err)
}

func (c *mtprotoClient) downloadDocument(ctx context.Context, api *tg.Client, ref documentReference, output io.WriterAt) error {
	// Telegram accepts aligned chunks up to 1 MiB. Larger chunks reduce RPC
	// overhead on high-latency links without increasing connection concurrency.
	_, err := downloader.NewDownloader().WithPartSize(1<<20).Download(api, &tg.InputDocumentFileLocation{
		ID: ref.ID, AccessHash: ref.AccessHash, FileReference: ref.FileReference,
	}).WithThreads(c.cfg.DownloadThreads).Parallel(ctx, output)
	return err
}

func (c *mtprotoClient) refreshDocument(ctx context.Context, ref documentReference) (documentReference, error) {
	// Fetch a fresh reference on every attempt, including tasks restored after a
	// restart. Verify document identity before downloading a possibly edited message.
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: ref.MessageID}}
	var messages tg.MessagesMessagesClass
	var err error
	if ref.ChannelID != 0 {
		messages, err = c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ref.ChannelID, AccessHash: ref.ChannelHash}, ID: ids,
		})
		if err != nil {
			return ref, messageLinkError(err)
		}
	} else {
		messages, err = c.api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return ref, telegramError(err)
	}
	modified, ok := messages.AsModified()
	if !ok {
		return ref, errors.New("Telegram 原消息不可用，请重新发送视频")
	}
	found := false
	for _, value := range modified.GetMessages() {
		if msg, ok := value.(*tg.Message); ok && msg.ID == ref.MessageID {
			if attachment, ok := msg.Media.(*tg.MessageMediaDocument); ok {
				if doc, ok := attachment.Document.(*tg.Document); ok && doc.ID == ref.ID {
					ref.AccessHash, ref.FileReference, ref.DCID = doc.AccessHash, doc.FileReference, doc.DCID
					found = true
				}
			}
		}
	}
	if !found {
		return ref, errors.New("Telegram 原消息已删除或视频已变化，请重新发送")
	}
	return ref, nil
}

func (c *mtprotoClient) Send(ctx context.Context, r catalog.TelegramReceipt, text botMessage) (int64, error) {
	hash, _, err := c.state.GetUserAccessHash(ctx, r.BotID, r.ChatID)
	if err != nil {
		return 0, err
	}
	peer := &tg.InputPeerUser{UserID: r.ChatID, AccessHash: hash}
	var builder entity.Builder
	if err := styling.Perform(&builder, html.String(nil, text.text)); err != nil {
		return 0, errors.New("无法生成 Telegram 回复")
	}
	body, entities := builder.Complete()
	var markup tg.ReplyMarkupClass
	if text.videoURL != "" {
		markup = &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{{Buttons: []tg.KeyboardInlineButton{{Text: "打开视频", Type: &tg.InlineButtonTypeURL{URL: text.videoURL}}}}}}
	}
	if r.ReplyID != 0 {
		_, err := c.api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{Peer: peer, ID: int(r.ReplyID), Message: body, Entities: entities, ReplyMarkup: markup, NoWebpage: true})
		if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
			err = nil
		}
		return r.ReplyID, telegramError(err)
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%s", r.BotID, r.ChatID, r.UpdateID, text.text)))
	request := &tg.MessagesSendMessageRequest{Peer: peer, Message: body, Entities: entities, ReplyMarkup: markup, NoWebpage: true, RandomID: int64(binary.LittleEndian.Uint64(digest[:8]))}
	if r.MessageID > 0 {
		request.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: int(r.MessageID)}
	}
	result, err := c.api.MessagesSendMessage(ctx, request)
	if tgerr.Is(err, "REPLY_MESSAGE_ID_INVALID", "MESSAGE_ID_INVALID") {
		request.ReplyTo = nil
		result, err = c.api.MessagesSendMessage(ctx, request)
	}
	if err != nil {
		return 0, telegramError(err)
	}
	var items []tg.UpdateClass
	switch u := result.(type) {
	case *tg.UpdateShortSentMessage:
		return int64(u.ID), nil
	case *tg.Updates:
		items = u.Updates
	case *tg.UpdatesCombined:
		items = u.Updates
	case *tg.UpdateShort:
		items = []tg.UpdateClass{u.Update}
	}
	for _, item := range items {
		if msg, ok := item.(*tg.UpdateMessageID); ok && msg.RandomID == request.RandomID {
			return int64(msg.ID), nil
		}
		if msg, ok := item.(*tg.UpdateNewMessage); ok {
			if sent, ok := msg.Message.(*tg.Message); ok && sent.Out {
				if destination, ok := sent.PeerID.(*tg.PeerUser); ok && destination.UserID == r.ChatID {
					return int64(sent.ID), nil
				}
			}
		}
	}
	return 0, errors.New("Telegram 回复未返回消息编号")
}

func (c *mtprotoClient) SetCommands(ctx context.Context) error {
	_, err := c.api.BotsSetBotCommands(ctx, &tg.BotsSetBotCommandsRequest{Scope: &tg.BotCommandScopeUsers{}, Commands: []tg.BotCommand{
		{Command: commandStart, Description: "开始"}, {Command: commandHelp, Description: "使用说明"},
		{Command: commandStatus, Description: "转存统计"}, {Command: commandID, Description: "我的 Telegram ID"},
	}})
	if err == nil {
		_, err = c.api.BotsSetBotMenuButton(ctx, &tg.BotsSetBotMenuButtonRequest{UserID: &tg.InputUserEmpty{}, Button: &tg.BotMenuButtonCommands{}})
	}
	return telegramError(err)
}

type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}
type media struct {
	FileID   string `json:"file_id"`
	UniqueID string `json:"file_unique_id"`
	Name     string `json:"file_name"`
	MIME     string `json:"mime_type"`
	Size     int64  `json:"file_size"`
}
type message struct {
	ID   int64 `json:"message_id"`
	Date int64 `json:"date"`
	From user  `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text         string  `json:"text"`
	Caption      string  `json:"caption"`
	MediaGroupID string  `json:"media_group_id"`
	Photo        []media `json:"photo,omitempty"`
	Video        *media  `json:"video"`
	Document     *media  `json:"document"`
}
type update struct {
	ID      int64    `json:"update_id"`
	Message *message `json:"message"`
}
