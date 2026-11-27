package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

const (
	defaultShortsBatchSize   = 5
	maxShortsBatchSize       = 20
	shortsFeedLookupChunk    = 32
	shortsLatestSnapshotSize = 100
	shortsFeedTTL            = 24 * time.Hour
	maxShortsFeedSessions    = 64
	shortsLinkPrewarmCount   = 2
	maxShortsPrewarmRequest  = 3
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
	VideoSrc         string                `json:"videoSrc"`
	PlaybackURL      string                `json:"playbackUrl,omitempty"`
	Poster           string                `json:"poster"`
	BackgroundPoster string                `json:"backgroundPoster,omitempty"`
	SizeBytes        int64                 `json:"sizeBytes,omitempty"`
	DurationSeconds  int                   `json:"durationSeconds,omitempty"`
	FeedCursor       int                   `json:"feedCursor,omitempty"`
	Reaction         catalog.VideoReaction `json:"reaction,omitempty"`
}

type shortsFeedResponse struct {
	Items         []ShortsItemDTO `json:"items"`
	Total         int             `json:"total"`
	FeedToken     string          `json:"feedToken"`
	NextCursor    int             `json:"nextCursor"`
	RoundComplete bool            `json:"roundComplete"`
}

type shortsPrewarmRequest struct {
	VideoIDs []string `json:"videoIds"`
}

type shortsPrewarmResponse struct {
	Scheduled int `json:"scheduled"`
}

// handleShortsNext serves an idempotent, body-free feed endpoint. Ordered modes
// (latest/hot) snapshot bounded visible ID slices; recommend persists the full
// Fisher-Yates shuffled rowid sequence as a packed binary blob (4 or 8 bytes per
// video, O(N) per session) instead of a JSON array of string IDs. Later
// requests send only the opaque token and numeric cursor.
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

	count, err := feedQueryInt(r, "count", defaultShortsBatchSize)
	if err != nil || count < 1 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts count"))
		return
	}
	if count > maxShortsBatchSize {
		count = maxShortsBatchSize
	}

	cursor, err := feedQueryInt(r, "cursor", 0)
	if err != nil || cursor < 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts cursor"))
		return
	}

	feedToken := strings.TrimSpace(r.URL.Query().Get("feedToken"))
	if len(feedToken) > 128 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts feed token"))
		return
	}
	visitID := strings.TrimSpace(r.URL.Query().Get("visitId"))
	if len(visitID) > 128 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts visit id"))
		return
	}

	var snapshot *catalog.ShortsFeedSnapshot
	if feedToken == "" {
		if cursor != 0 {
			writeErr(w, r, http.StatusBadRequest, errors.New("shorts cursor requires a feed token"))
			return
		}
		snapshot, err = s.createShortsSnapshot(r.Context(), mode)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		if snapshot.Total() > 0 {
			feedToken, err = newFeedToken()
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, err)
				return
			}
			s.storeShortsFeedSnapshot(feedToken, snapshot)
		}
	} else {
		snapshot, err = s.loadShortsFeedSnapshot(feedToken)
		if err != nil {
			writeErr(w, r, http.StatusGone, err)
			return
		}
		// Reject tokens created for a different mode so switching tabs or
		// misrouted tokens cannot mix feed pools.
		if snapshot.Mode != "" && snapshot.Mode != mode {
			writeErr(w, r, http.StatusGone, errShortsFeedExpired)
			return
		}
		// Rebuild full-library snapshots created before the latest feed was bounded.
		if mode == "latest" && (snapshot.RowIDs != nil || len(snapshot.VideoIDs) > shortsLatestSnapshotSize) {
			writeErr(w, r, http.StatusGone, errShortsFeedExpired)
			return
		}
	}

	total := snapshot.Total()
	if cursor > total {
		writeErr(w, r, http.StatusBadRequest, errors.New("shorts cursor is outside the feed"))
		return
	}

	var videos []*catalog.Video
	var itemCursors []int
	var nextCursor int
	if snapshot.RowIDs != nil {
		videos, itemCursors, nextCursor, err = s.loadShortsRowIDBatch(
			r.Context(),
			snapshot.RowIDs,
			cursor,
			count,
			mode == "hot",
		)
	} else {
		videos, itemCursors, nextCursor, err = s.loadShortsFeedBatch(
			r.Context(),
			snapshot.VideoIDs,
			cursor,
			count,
			mode == "hot",
		)
	}
	roundComplete := nextCursor >= total
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}

	var reactions map[string]catalog.VideoReaction
	if visitID != "" && len(videos) > 0 && s.Catalog != nil {
		ids := make([]string, len(videos))
		for i, video := range videos {
			ids[i] = video.ID
		}
		if loadedReactions, reactionErr := s.Catalog.VisitReactionsByVideoIDs(r.Context(), visitID, ids); reactionErr == nil {
			reactions = loadedReactions
		}
	}

	items := s.mapShortsItemsWithReactions(r.Context(), videos, itemCursors, reactions)
	s.prewarmShortsStreamLinks(videos, r.Header)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, shortsFeedResponse{
		Items:         items,
		Total:         total,
		FeedToken:     feedToken,
		NextCursor:    nextCursor,
		RoundComplete: roundComplete,
	})
}

// handleShortsPrewarm lets the shorts player request rolling-window provider
// link resolution for upcoming videos right as the active video finishes
// buffering, rather than guessing only the first 2 items of a larger batch.
func (s *Server) handleShortsPrewarm(w http.ResponseWriter, r *http.Request) {
	var body shortsPrewarmRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid prewarm payload"))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return
	}
	if len(body.VideoIDs) > maxShortsPrewarmRequest {
		body.VideoIDs = body.VideoIDs[:maxShortsPrewarmRequest]
	}

	scheduled := 0
	if len(body.VideoIDs) > 0 && s.Catalog != nil {
		videos, err := s.Catalog.VisibleVideosByIDs(r.Context(), body.VideoIDs)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		scheduled = s.prewarmShortsStreamLinksLimit(videos, r.Header, maxShortsPrewarmRequest)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, shortsPrewarmResponse{Scheduled: scheduled})
}

func (s *Server) createShortsSnapshot(ctx context.Context, mode string) (*catalog.ShortsFeedSnapshot, error) {
	if mode == "recommend" {
		return s.Catalog.CreateShortsRecommendSnapshot(ctx)
	}
	limit := 0
	if mode == "latest" {
		limit = shortsLatestSnapshotSize
	}
	videoIDs, err := s.Catalog.ListVideoIDs(ctx, catalog.ListParams{
		Sort: mode, LikedOnly: mode == "hot",
	}, limit)
	if err != nil {
		return nil, err
	}
	return &catalog.ShortsFeedSnapshot{
		Version:  2,
		Mode:     mode,
		VideoIDs: videoIDs,
	}, nil
}

// Resolve only the first couple of browser-facing stream links in the background.
// The proxy coalesces this with a real /p/stream request if playback arrives first;
// a small global semaphore prevents several clients from flooding provider APIs.
func (s *Server) prewarmShortsStreamLinks(videos []*catalog.Video, requestHeader http.Header) {
	s.prewarmShortsStreamLinksLimit(videos, requestHeader, shortsLinkPrewarmCount)
}

func (s *Server) prewarmShortsStreamLinksLimit(
	videos []*catalog.Video,
	requestHeader http.Header,
	maxCount int,
) int {
	if s.Proxy == nil || maxCount <= 0 {
		return 0
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
		if s.Proxy.HasPlaybackProvider(driveID) {
			continue
		}
		select {
		case shortsLinkPrewarmSlots <- struct{}{}:
		default:
			return scheduled
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
		if scheduled >= maxCount {
			return scheduled
		}
	}
	return scheduled
}

func (s *Server) shortsNow() time.Time {
	if s.shortsFeedNow != nil {
		return s.shortsFeedNow()
	}
	return time.Now()
}

// storeShortsFeed stores a legacy untyped ID-list snapshot (kept for tests covering
// pre-v2 persisted snapshots).
func (s *Server) storeShortsFeed(token string, videoIDs []string) {
	now := s.shortsNow()
	snap := &catalog.ShortsFeedSnapshot{VideoIDs: videoIDs}
	s.rememberShortsFeed(token, snap, now)
	if s.Catalog == nil {
		return
	}
	ctx := context.Background()
	if err := s.Catalog.SaveShortsFeed(ctx, token, videoIDs, now); err != nil {
		applog.Warn(ctx, "persist shorts feed session failed", err, "component", "shorts")
		return
	}
	if err := s.Catalog.PruneShortsFeeds(ctx, now.Add(-shortsFeedTTL), maxShortsFeedSessions); err != nil {
		applog.Warn(ctx, "prune persisted shorts feed sessions failed", err, "component", "shorts")
	}
}

func (s *Server) storeShortsFeedSnapshot(token string, snapshot *catalog.ShortsFeedSnapshot) {
	now := s.shortsNow()
	s.rememberShortsFeed(token, snapshot, now)
	s.persistShortsFeedSnapshot(token, snapshot, now)
}

func (s *Server) rememberShortsFeed(token string, snapshot *catalog.ShortsFeedSnapshot, now time.Time) {
	s.shortsFeeds.put(
		token,
		snapshot,
		now,
		shortsFeedTTL,
		maxShortsFeedSessions,
		nil,
		nil,
	)
}

// persistShortsFeedSnapshot 尽力而为地把快照落盘：失败只影响重启后的续播，
// 不影响当前内存会话，因此记录日志后继续。TTL 与数量上限的持久层
// 清理也在写入时顺带完成。
func (s *Server) persistShortsFeedSnapshot(token string, snapshot *catalog.ShortsFeedSnapshot, now time.Time) {
	if s.Catalog == nil {
		return
	}
	ctx := context.Background()
	if err := s.Catalog.SaveShortsFeedSnapshot(ctx, token, snapshot, now); err != nil {
		applog.Warn(ctx, "persist shorts feed session failed", err, "component", "shorts")
		return
	}
	if err := s.Catalog.PruneShortsFeeds(ctx, now.Add(-shortsFeedTTL), maxShortsFeedSessions); err != nil {
		applog.Warn(ctx, "prune persisted shorts feed sessions failed", err, "component", "shorts")
	}
}

func (s *Server) loadShortsFeedSnapshot(token string) (*catalog.ShortsFeedSnapshot, error) {
	now := s.shortsNow()
	if snapshot, ok := s.shortsFeeds.get(token, now, shortsFeedTTL); ok && snapshot != nil {
		s.touchPersistedShortsFeed(token, now)
		return snapshot, nil
	}

	// 内存里没有（进程重启或被 LRU 淘汰）：尝试从持久化快照恢复同一轮次。
	snapshot := s.loadPersistedShortsFeedSnapshot(token, now)
	if snapshot == nil {
		return nil, errShortsFeedExpired
	}
	s.rememberShortsFeed(token, snapshot, now)
	return snapshot, nil
}

// loadPersistedShortsFeedSnapshot 读取并校验持久化快照；确定过期时顺带删除，
// 读取出错时保留数据（等 TTL 清理），只当作会话失效处理。
func (s *Server) loadPersistedShortsFeedSnapshot(token string, now time.Time) *catalog.ShortsFeedSnapshot {
	if s.Catalog == nil {
		return nil
	}
	ctx := context.Background()
	snapshot, lastAccess, err := s.Catalog.LoadShortsFeedSnapshot(ctx, token)
	if err != nil {
		applog.Warn(ctx, "load persisted shorts feed session failed", err, "component", "shorts")
		return nil
	}
	if snapshot == nil {
		return nil
	}
	if snapshot.Total() == 0 || now.Sub(lastAccess) >= shortsFeedTTL {
		if err := s.Catalog.DeleteShortsFeed(ctx, token); err != nil {
			applog.Warn(ctx, "delete expired shorts feed session failed", err, "component", "shorts")
		}
		return nil
	}
	if err := s.Catalog.TouchShortsFeed(ctx, token, now); err != nil {
		applog.Warn(ctx, "touch persisted shorts feed session failed", err, "component", "shorts")
	}
	return snapshot
}

func (s *Server) touchPersistedShortsFeed(token string, now time.Time) {
	if s.Catalog == nil {
		return
	}
	if err := s.Catalog.TouchShortsFeed(context.Background(), token, now); err != nil {
		applog.Warn(context.Background(), "touch persisted shorts feed session failed", err, "component", "shorts")
	}
}

// loadShortsRowIDBatch serves a Fisher-Yates shuffled rowid snapshot using
// primary-key rowid IN lookups (zero full-table scan, zero OFFSET). Hidden or
// deleted entries are skipped in place without shifting any later slot.
func (s *Server) loadShortsRowIDBatch(
	ctx context.Context,
	rowIDs *catalog.ShortsRowIDSnapshot,
	cursor int,
	count int,
	likedOnly bool,
) ([]*catalog.Video, []int, int, error) {
	videos := make([]*catalog.Video, 0, count)
	itemCursors := make([]int, 0, count)

	for cursor < rowIDs.Count && len(videos) < count {
		end := cursor + shortsFeedLookupChunk
		if end > rowIDs.Count {
			end = rowIDs.Count
		}
		chunk := rowIDs.SliceRowIDs(cursor, end)
		visibleByRowID, err := s.Catalog.VisibleVideosByRowIDs(ctx, chunk)
		if err != nil {
			return nil, nil, cursor, err
		}
		for _, rowID := range chunk {
			if len(videos) >= count {
				break
			}
			cursor++
			video := visibleByRowID[rowID]
			if video == nil || (likedOnly && video.Likes <= 0) {
				continue
			}
			videos = append(videos, video)
			itemCursors = append(itemCursors, cursor)
		}
	}

	return videos, itemCursors, cursor, nil
}

// loadShortsFeedBatch advances over snapshot entries that are no longer
// eligible and records the precise resume cursor after each returned item.
func (s *Server) loadShortsFeedBatch(
	ctx context.Context,
	videoIDs []string,
	cursor int,
	count int,
	likedOnly bool,
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
			if likedOnly && video.Likes <= 0 {
				continue
			}
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
	return s.mapShortsItemsWithReactions(ctx, videos, itemCursors, nil)
}

func (s *Server) mapShortsItemsWithReactions(
	ctx context.Context,
	videos []*catalog.Video,
	itemCursors []int,
	reactions map[string]catalog.VideoReaction,
) []ShortsItemDTO {
	driveLabels := make(map[string]string)
	out := make([]ShortsItemDTO, 0, len(videos))
	for index, video := range videos {
		dto := mapVideo(video, s.previewsEnabled())
		videoSrc, sizeBytes := s.videoSourceAndSize(video)
		durationSeconds := video.DurationSeconds
		if s.Proxy != nil && s.Proxy.HasPlaybackProvider(video.DriveID) {
			// Provider HLS may be re-encoded; original size is not its bitrate.
			sizeBytes, durationSeconds = 0, 0
		}
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
			PlaybackURL:      playbackURL(video.ID),
			Poster:           poster,
			BackgroundPoster: shortsBackgroundPosterURL(poster),
			SizeBytes:        sizeBytes,
			DurationSeconds:  durationSeconds,
			FeedCursor:       feedCursor,
			Reaction:         reactions[video.ID],
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
