package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type messageLink struct {
	Username  string
	ChannelID int64
	MessageID int
}

var channelUsername = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,31}$`)

// Link parameters such as single and playback timestamps do not restrict album imports.
func parseMessageLink(text string) (messageLink, bool) {
	var link messageLink
	raw := strings.TrimSpace(text)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" {
		return link, false
	}
	switch strings.ToLower(u.Host) {
	case "t.me", "www.t.me", "telegram.me", "telegram.dog":
	default:
		return link, false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || query.Has("comment") {
		return link, false // A comment ID belongs to a different discussion chat.
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && parts[0] == "s" {
		parts = parts[1:]
	}
	if len(parts) > 0 && parts[0] == "c" {
		if len(parts) < 3 {
			return link, false
		}
		link.ChannelID, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || link.ChannelID <= 0 {
			return messageLink{}, false
		}
		parts = parts[1:]
	} else if len(parts) > 0 && channelUsername.MatchString(parts[0]) {
		link.Username = parts[0]
	} else {
		return link, false
	}
	if len(parts) != 2 && len(parts) != 3 {
		return messageLink{}, false
	}
	// Topic links contain a thread ID before the addressed message ID.
	for _, part := range parts[1:] {
		id, err := strconv.ParseInt(part, 10, 32)
		if err != nil || id <= 0 {
			return messageLink{}, false
		}
		link.MessageID = int(id)
	}
	return link, true
}

func documentMedia(msg *tg.Message, channel *tg.InputChannel) (*media, bool) {
	attachment, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	doc, ok := attachment.Document.(*tg.Document)
	if !ok {
		return nil, false
	}
	file := &media{FileID: encodeDocument(doc, msg.ID, channel), UniqueID: strconv.FormatInt(doc.ID, 10), Size: doc.Size, MIME: doc.MimeType}
	video := false
	for _, attr := range doc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeFilename:
			file.Name = a.FileName
		case *tg.DocumentAttributeVideo:
			video = true
		}
	}
	return file, video || isVideoDocument(*file)
}

func (c *mtprotoClient) ResolveMediaLink(ctx context.Context, link messageLink) ([]*message, error) {
	var chats []tg.ChatClass
	channelID := link.ChannelID
	if link.Username != "" {
		resolved, err := c.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: link.Username})
		if err != nil {
			return nil, messageLinkError(err)
		}
		peer, ok := resolved.Peer.(*tg.PeerChannel)
		if !ok {
			return nil, errors.New("链接不是频道或超级群组的消息链接")
		}
		channelID, chats = peer.ChannelID, resolved.Chats
	} else {
		resolved, err := c.api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: channelID}})
		if err != nil {
			return nil, messageLinkError(err)
		}
		chats = resolved.GetChats()
	}
	var channel *tg.InputChannel
	for _, chat := range chats {
		if ch, ok := chat.(*tg.Channel); ok && ch.ID == channelID {
			channel = &tg.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
			if ch.Min {
				channel.AccessHash = 0 // Bot sessions can use a zero hash when a full hash is unavailable.
			}
			break
		}
	}
	if channel == nil {
		return nil, errors.New("机器人无法访问该频道或群组，请将机器人加入后重试，或直接转发视频")
	}
	messages, err := c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channel, ID: []tg.InputMessageClass{&tg.InputMessageID{ID: link.MessageID}},
	})
	if err != nil {
		return nil, messageLinkError(err)
	}
	var target *tg.Message
	if modified, ok := messages.AsModified(); ok {
		for _, value := range modified.GetMessages() {
			if msg, ok := value.(*tg.Message); ok && msg.ID == link.MessageID {
				target = msg
				break
			}
		}
	}
	if target == nil {
		return nil, errors.New("链接指向的消息已删除或不可用，请检查链接或直接转发视频")
	}
	members := []*tg.Message{target}
	if target.GroupedID != 0 {
		// Telegram albums have at most ten members. Fetch the surrounding IDs,
		// then use grouped_id to exclude unrelated nearby messages.
		ids := make([]tg.InputMessageClass, 0, 18)
		for id := max(1, target.ID-9); id <= min(1<<31-1, target.ID+9); id++ {
			if id != target.ID {
				ids = append(ids, &tg.InputMessageID{ID: id})
			}
		}
		album, err := c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: channel, ID: ids})
		if err != nil {
			return nil, messageLinkError(err)
		}
		modified, ok := album.AsModified()
		if !ok {
			return nil, errors.New("无法读取完整媒体组，请稍后重新发送链接")
		}
		seen := map[int]bool{target.ID: true}
		for _, value := range modified.GetMessages() {
			if msg, ok := value.(*tg.Message); ok && msg.GroupedID == target.GroupedID && !seen[msg.ID] {
				members = append(members, msg)
				seen[msg.ID] = true
			}
		}
		sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	}
	out := make([]*message, 0, len(members))
	videos := 0
	for _, msg := range members {
		m := &message{ID: int64(msg.ID), Date: int64(msg.Date), Caption: msg.Message}
		if target.GroupedID != 0 {
			m.MediaGroupID = fmt.Sprintf("%d:%d", channel.ChannelID, target.GroupedID)
		}
		if file, video := documentMedia(msg, channel); video {
			m.Video = file
			videos++
		}
		if _, ok := msg.Media.(*tg.MessageMediaPhoto); ok {
			m.Photo = []media{{}}
		}
		out = append(out, m)
	}
	if videos == 0 {
		return nil, errors.New("链接指向的消息或媒体组不包含视频，请检查链接")
	}
	return out, nil
}

func messageLinkError(err error) error {
	switch {
	case tgerr.Is(err, "CHANNEL_PRIVATE", "CHANNEL_INVALID", "CHAT_FORBIDDEN", "USER_BANNED_IN_CHANNEL"):
		return errors.New("机器人无法访问该频道或群组，请将机器人加入后重试，或直接转发视频")
	case tgerr.Is(err, "USERNAME_INVALID", "USERNAME_NOT_OCCUPIED"):
		return errors.New("链接中的频道或群组不存在，请检查链接")
	case tgerr.Is(err, "MESSAGE_ID_INVALID", "MSG_ID_INVALID"):
		return errors.New("链接指向的消息已删除或不可用，请检查链接或直接转发视频")
	default:
		return telegramError(err)
	}
}
