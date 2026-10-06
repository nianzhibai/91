package api

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

const (
	defaultShortsBatchSize = 5
	maxShortsBatchSize     = 20
	shortsFeedLookupChunk  = 32
	shortsFeedTTL          = 24 * time.Hour
	maxShortsFeedSessions  = 64
	shortsLinkPrewarmCount = 2
	// Slightly longer than proxy link resolution's hard timeout so a timed-out
	// provider call keeps occupying its global slot until the detached resolver
	// actually exits.
	shortsLinkPrewarmTimeout = 16 * time.Second
)

var errShortsFeedExpired = errors.New("shorts feed expired")
var shortsLinkPrewarmSlots = make(chan struct{}, 4)

// ShortsItemDTO is a compact feed item that can be handed directly to a video
// element. FeedCursor is the resume position immediately after this item.
// SizeBytes/DurationSeconds 让前端能算出这一条的平均码率。短视频页的
// 预加载门槛原本只按"缓冲够多少秒"判定，但带宽是按字节付的：本库平均
// ~10 Mbps，12 秒就是 ~15 MB，网速跟不上时门槛永远达不到，预载授权
// 一直发不出去，退化成没有预载的串行加载。有了码率才能把门槛换算成
// 一份固定的字节预算。任一元数据缺失时两个字段会一起省略，前端需要兜底。
type ShortsItemDTO struct {
	VideoDTO
	VideoSrc         string `json:"videoSrc"`
	Poster           string `json:"poster"`
	BackgroundPoster string `json:"backgroundPoster,omitempty"`
	SizeBytes        int64  `json:"sizeBytes,omitempty"`
	DurationSeconds  int    `json:"durationSeconds,omitempty"`
	FeedCursor       int    `json:"feedCursor,omitempty"`
}

type shortsFeedSession struct {
	videoIDs   []string
	lastAccess time.Time
}

type shortsFeedResponse struct {
	Items         []ShortsItemDTO `json:"items"`
	Total         int             `json:"total"`
	FeedToken     string          `json:"feedToken"`
	NextCursor    int             `json:"nextCursor"`
	RoundComplete bool            `json:"roundComplete"`
}

// handleShortsNext serves an idempotent, body-free feed endpoint. A new feed
// snapshots the visible video IDs in the selected order once. Later requests send only
// the opaque token and numeric cursor, so request size stays constant even for
// libraries with many thousands of videos.
func (s *Server) handleShortsNext(w http.ResponseWriter, r *http.Request) {
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = "recommend"
	}
	switch mode {
	case "recommend", "latest", "hot":
	default:
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts mode"))
		return
	}

	count, err := shortsQueryInt(r, "count", defaultShortsBatchSize)
	if err != nil || count < 1 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts count"))
		return
	}
	if count > maxShortsBatchSize {
		count = maxShortsBatchSize
	}

	cursor, err := shortsQueryInt(r, "cursor", 0)
	if err != nil || cursor < 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts cursor"))
		return
	}

	feedToken := strings.TrimSpace(r.URL.Query().Get("feedToken"))
	if len(feedToken) > 128 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts feed token"))
		return
	}
	var videoIDs []string
	if feedToken == "" {
		if cursor != 0 {
			writeErr(w, r, http.StatusBadRequest, errors.New("shorts cursor requires a feed token"))
			return
		}
		if mode == "recommend" {
			videoIDs, err = s.Catalog.ListVisibleVideoIDs(r.Context())
		} else {
			videoIDs, err = s.Catalog.ListVideoIDs(r.Context(), catalog.ListParams{Sort: mode})
		}
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		if len(videoIDs) > 0 {
			if mode == "recommend" {
				rand.Shuffle(len(videoIDs), func(i, j int) {
					videoIDs[i], videoIDs[j] = videoIDs[j], videoIDs[i]
				})
			}
			feedToken, err = newShortsFeedToken()
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, err)
				return
			}
			s.storeShortsFeed(feedToken, videoIDs)
		}
	} else {
		videoIDs, err = s.loadShortsFeed(feedToken)
		if err != nil {
			writeErr(w, r, http.StatusGone, err)
			return
		}
	}

	if cursor > len(videoIDs) {
		writeErr(w, r, http.StatusBadRequest, errors.New("shorts cursor is outside the feed"))
		return
	}

	videos, itemCursors, nextCursor, err := s.loadShortsFeedBatch(
		r.Context(),
		videoIDs,
		cursor,
		count,
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}

	items := s.mapShortsItems(r.Context(), videos, itemCursors)
	s.prewarmShortsStreamLinks(videos, r.Header)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, shortsFeedResponse{
		Items:         items,
		Total:         len(videoIDs),
		FeedToken:     feedToken,
		NextCursor:    nextCursor,
		RoundComplete: nextCursor >= len(videoIDs),
	})
}

// Resolve only the first couple of browser-facing stream links in the background.
// The proxy coalesces this with a real /p/stream request if playback arrives first;
// a small global semaphore prevents several clients from flooding provider APIs.
func (s *Server) prewarmShortsStreamLinks(videos []*catalog.Video, requestHeader http.Header) {
	if s.Proxy == nil {
		return
	}
	header := make(http.Header)
	if userAgent := requestHeader.Get("User-Agent"); userAgent != "" {
		header.Set("User-Agent", userAgent)
	}

	scheduled := 0
	for _, video := range videos {
		driveID, fileID, ok := videoStreamTarget(video)
		if !ok {
			continue
		}
		select {
		case shortsLinkPrewarmSlots <- struct{}{}:
		default:
			return
		}
		scheduled++
		go func(driveID, fileID string) {
			defer func() { <-shortsLinkPrewarmSlots }()
			ctx, cancel := context.WithTimeout(
				context.Background(),
				shortsLinkPrewarmTimeout,
			)
			defer cancel()
			_ = s.Proxy.WarmStreamLink(ctx, driveID, fileID, header)
		}(driveID, fileID)
		if scheduled >= shortsLinkPrewarmCount {
			return
		}
	}
}

func shortsQueryInt(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func newShortsFeedToken() (string, error) {
	var token [16]byte
	if _, err := crand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func (s *Server) shortsNow() time.Time {
	if s.shortsFeedNow != nil {
		return s.shortsFeedNow()
	}
	return time.Now()
}

func (s *Server) storeShortsFeed(token string, videoIDs []string) {
	now := s.shortsNow()
	s.rememberShortsFeed(token, videoIDs, now)
	s.persistShortsFeed(token, videoIDs, now)
}

// rememberShortsFeed 把快照放进内存会话表（含 TTL 清理与 LRU 淘汰）。
func (s *Server) rememberShortsFeed(token string, videoIDs []string, now time.Time) {
	s.shortsFeedMu.Lock()
	defer s.shortsFeedMu.Unlock()

	if s.shortsFeeds == nil {
		s.shortsFeeds = make(map[string]*shortsFeedSession)
	}
	s.pruneShortsFeedsLocked(now)
	for len(s.shortsFeeds) >= maxShortsFeedSessions {
		var oldestToken string
		var oldestAccess time.Time
		for candidateToken, feed := range s.shortsFeeds {
			if oldestToken == "" || feed.lastAccess.Before(oldestAccess) {
				oldestToken = candidateToken
				oldestAccess = feed.lastAccess
			}
		}
		if oldestToken == "" {
			break
		}
		delete(s.shortsFeeds, oldestToken)
	}

	// The slice is immutable after insertion, so readers can safely retain it
	// after releasing the mutex.
	s.shortsFeeds[token] = &shortsFeedSession{
		videoIDs:   videoIDs,
		lastAccess: now,
	}
}

// persistShortsFeed 尽力而为地把快照落盘：失败只影响重启后的续播，
// 不影响当前内存会话，因此记录日志后继续。TTL 与数量上限的持久层
// 清理也在写入时顺带完成。
func (s *Server) persistShortsFeed(token string, videoIDs []string, now time.Time) {
	if s.Catalog == nil {
		return
	}
	ctx := context.Background()
	if err := s.Catalog.SaveShortsFeed(ctx, token, videoIDs, now); err != nil {
		log.Printf("shorts: persist feed session: %v", err)
		return
	}
	if err := s.Catalog.PruneShortsFeeds(ctx, now.Add(-shortsFeedTTL), maxShortsFeedSessions); err != nil {
		log.Printf("shorts: prune persisted feed sessions: %v", err)
	}
}

func (s *Server) loadShortsFeed(token string) ([]string, error) {
	now := s.shortsNow()
	s.shortsFeedMu.Lock()
	s.pruneShortsFeedsLocked(now)
	if feed := s.shortsFeeds[token]; feed != nil {
		feed.lastAccess = now
		videoIDs := feed.videoIDs
		s.shortsFeedMu.Unlock()
		s.touchPersistedShortsFeed(token, now)
		return videoIDs, nil
	}
	s.shortsFeedMu.Unlock()

	// 内存里没有（进程重启或被 LRU 淘汰）：尝试从持久化快照恢复同一轮次。
	videoIDs := s.loadPersistedShortsFeed(token, now)
	if videoIDs == nil {
		return nil, errShortsFeedExpired
	}
	s.rememberShortsFeed(token, videoIDs, now)
	return videoIDs, nil
}

// loadPersistedShortsFeed 读取并校验持久化快照；确定过期时顺带删除，
// 读取出错时保留数据（等 TTL 清理），只当作会话失效处理。
func (s *Server) loadPersistedShortsFeed(token string, now time.Time) []string {
	if s.Catalog == nil {
		return nil
	}
	ctx := context.Background()
	videoIDs, lastAccess, err := s.Catalog.LoadShortsFeed(ctx, token)
	if err != nil {
		log.Printf("shorts: load persisted feed session: %v", err)
		return nil
	}
	if videoIDs == nil {
		return nil
	}
	if len(videoIDs) == 0 || now.Sub(lastAccess) >= shortsFeedTTL {
		if err := s.Catalog.DeleteShortsFeed(ctx, token); err != nil {
			log.Printf("shorts: delete expired feed session: %v", err)
		}
		return nil
	}
	if err := s.Catalog.TouchShortsFeed(ctx, token, now); err != nil {
		log.Printf("shorts: touch persisted feed session: %v", err)
	}
	return videoIDs
}

func (s *Server) touchPersistedShortsFeed(token string, now time.Time) {
	if s.Catalog == nil {
		return
	}
	if err := s.Catalog.TouchShortsFeed(context.Background(), token, now); err != nil {
		log.Printf("shorts: touch persisted feed session: %v", err)
	}
}

func (s *Server) pruneShortsFeedsLocked(now time.Time) {
	for token, feed := range s.shortsFeeds {
		if now.Sub(feed.lastAccess) >= shortsFeedTTL {
			delete(s.shortsFeeds, token)
		}
	}
}

// loadShortsFeedBatch advances over snapshot entries that are no longer
// visible and records the precise resume cursor after each returned item.
func (s *Server) loadShortsFeedBatch(
	ctx context.Context,
	videoIDs []string,
	cursor int,
	count int,
) ([]*catalog.Video, []int, int, error) {
	videos := make([]*catalog.Video, 0, count)
	itemCursors := make([]int, 0, count)

	for cursor < len(videoIDs) && len(videos) < count {
		end := cursor + shortsFeedLookupChunk
		if end > len(videoIDs) {
			end = len(videoIDs)
		}
		visible, err := s.Catalog.VisibleVideosByIDs(ctx, videoIDs[cursor:end])
		if err != nil {
			return nil, nil, cursor, err
		}
		visibleByID := make(map[string]*catalog.Video, len(visible))
		for _, video := range visible {
			visibleByID[video.ID] = video
		}

		for cursor < end && len(videos) < count {
			videoID := videoIDs[cursor]
			cursor++
			if video := visibleByID[videoID]; video != nil {
				videos = append(videos, video)
				itemCursors = append(itemCursors, cursor)
			}
		}
	}

	return videos, itemCursors, cursor, nil
}

func (s *Server) mapShortsItems(
	ctx context.Context,
	videos []*catalog.Video,
	itemCursors []int,
) []ShortsItemDTO {
	driveLabels := make(map[string]string)
	out := make([]ShortsItemDTO, 0, len(videos))
	for index, video := range videos {
		dto := mapVideo(video)
		videoSrc, sizeBytes := s.videoSourceAndSize(video)
		durationSeconds := video.DurationSeconds
		// 大小与时长必须成对出现。任一缺失时把两者都省略，让前端明确
		// 识别为"码率未知"，而不是收到一半可用的元数据。
		if sizeBytes <= 0 || durationSeconds <= 0 {
			sizeBytes = 0
			durationSeconds = 0
		}
		if label, ok := driveLabels[video.DriveID]; ok {
			dto.SourceLabel = label
		} else {
			label := s.videoSourceLabel(ctx, video)
			driveLabels[video.DriveID] = label
			dto.SourceLabel = label
		}
		feedCursor := 0
		if index < len(itemCursors) {
			feedCursor = itemCursors[index]
		}
		poster := thumbnailURL(video)
		out = append(out, ShortsItemDTO{
			VideoDTO:         dto,
			VideoSrc:         videoSrc,
			Poster:           poster,
			BackgroundPoster: shortsBackgroundPosterURL(poster),
			SizeBytes:        sizeBytes,
			DurationSeconds:  durationSeconds,
			FeedCursor:       feedCursor,
		})
	}
	return out
}

func shortsBackgroundPosterURL(poster string) string {
	if !strings.HasPrefix(poster, "/p/thumb/") {
		return ""
	}
	separator := "?"
	if strings.ContainsRune(poster, '?') {
		separator = "&"
	}
	return poster + separator + "variant=shorts-bg"
}
