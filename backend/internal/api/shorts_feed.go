package api

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/feed"
)

const (
	defaultShortsBatchSize   = 5
	maxShortsBatchSize       = 20
	shortsFeedLookupChunk    = 32
	shortsLatestSnapshotSize = 100
	shortsFeedTTL            = 24 * time.Hour
	maxShortsFeedSessions    = 64
	shortsLinkPrewarmCount   = 2
	// recommend 模式按批决策，这些上限约束每批一次的信号加载成本。
	// 候选池有上限意味着超大库只推荐最新的 shortsCandidateLimit 条；这是
	// 私有库场景下用内存换延迟的有意取舍，total 仍取真实可见总数。
	shortsCandidateLimit    = 5000
	shortsServedLimit       = 400
	shortsProfileWindow     = 30 * 24 * time.Hour
	shortsDirectoryCacheTTL = 10 * time.Minute
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
	Favorited        bool   `json:"favorited"`
	Liked            bool   `json:"liked"`
}

type shortsFeedSession struct {
	// videoIDs 是确定性栏目（latest / hot / favorites）的整轮快照。
	videoIDs []string
	// served 是 recommend 模式已下发的 id，按下发顺序。它同时用于本会话去重
	// 和持久化：进程重启后旧 token 仍能接着避免重复推荐。
	served []string
	// batches 把 cursor 映射到已下发的批次，只存内存。同一个 cursor 的重试
	// 直接重放，避免重算让前端 feedToken:feedCursor 去重键错位。
	batches map[int][]string
	// total 是最近一次决策时的可见视频总数，重放时直接复用。
	total      int
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
	case "recommend", "latest", "hot", "favorites":
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
	viewerUserID, _ := auth.UserIDFromContext(r.Context())
	after := strings.TrimSpace(r.URL.Query().Get("after"))
	if len(after) > 128 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid shorts after"))
		return
	}
	// 推荐栏目按批决策：下一批取决于用户刚看完的那一条，无法预先生成整轮
	// 快照，因此单独走一条路径。
	if mode == "recommend" {
		s.handleShortsRecommend(w, r, count, cursor, feedToken, after, viewerUserID)
		return
	}
	var videoIDs []string
	if feedToken == "" {
		if cursor != 0 {
			writeErr(w, r, http.StatusBadRequest, errors.New("shorts cursor requires a feed token"))
			return
		}
		switch mode {
		case "favorites":
			// 收藏是账号数据：快照只含该账号已收藏且仍可见的视频，顺序为
			// 收藏时间倒序，不参与随机洗牌。
			videoIDs, err = s.Catalog.FavoriteVideoIDs(r.Context(), viewerUserID)
		default:
			limit := 0
			if mode == "latest" {
				limit = shortsLatestSnapshotSize
			}
			videoIDs, err = s.Catalog.ListVideoIDs(r.Context(), catalog.ListParams{
				Sort: mode, LikedOnly: mode == "hot",
			}, limit)
		}
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		if len(videoIDs) > 0 {
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
		// Rebuild full-library snapshots created before the latest feed was bounded.
		if mode == "latest" && len(videoIDs) > shortsLatestSnapshotSize {
			writeErr(w, r, http.StatusGone, errShortsFeedExpired)
			return
		}
	}

	if cursor > len(videoIDs) {
		writeErr(w, r, http.StatusBadRequest, errors.New("shorts cursor is outside the feed"))
		return
	}

	batch, err := s.loadShortsFeedBatch(
		r.Context(),
		videoIDs,
		cursor,
		count,
		shortsFeedSelection{
			likedOnly:     mode == "hot",
			favoritesOnly: mode == "favorites",
			viewerUserID:  viewerUserID,
		},
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}

	items := s.mapShortsItems(r.Context(), batch.videos, batch.cursors, batch.favorited, batch.liked)
	s.prewarmShortsStreamLinks(batch.videos, r.Header)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, shortsFeedResponse{
		Items:         items,
		Total:         len(videoIDs),
		FeedToken:     feedToken,
		NextCursor:    batch.nextCursor,
		RoundComplete: batch.nextCursor >= len(videoIDs),
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

// rememberShortsFeed 把确定性栏目的整轮快照放进内存会话表。
func (s *Server) rememberShortsFeed(token string, videoIDs []string, now time.Time) {
	s.shortsFeedMu.Lock()
	defer s.shortsFeedMu.Unlock()
	s.rememberShortsSessionLocked(token, &shortsFeedSession{videoIDs: videoIDs}, now)
}

// rememberShortsSessionLocked 插入会话并淘汰最久未用的条目。
// 调用方必须持有 shortsFeedMu。
func (s *Server) rememberShortsSessionLocked(token string, session *shortsFeedSession, now time.Time) {
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
	session.lastAccess = now
	s.shortsFeeds[token] = session
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

// shortsFeedSelection describes which snapshot entries stay eligible for this
// request and whose favorite state the caller needs back.
type shortsFeedSelection struct {
	likedOnly     bool
	favoritesOnly bool
	viewerUserID  int64
}

// shortsFeedBatch is one page of a feed snapshot plus the viewer's own favorite
// and like state for the ids examined while filling it.
type shortsFeedBatch struct {
	videos     []*catalog.Video
	cursors    []int
	nextCursor int
	favorited  map[string]struct{}
	liked      map[string]struct{}
}

// loadShortsFeedBatch advances over snapshot entries that are no longer
// eligible and records the precise resume cursor after each returned item.
func (s *Server) loadShortsFeedBatch(
	ctx context.Context,
	videoIDs []string,
	cursor int,
	count int,
	selection shortsFeedSelection,
) (shortsFeedBatch, error) {
	batch := shortsFeedBatch{
		videos:     make([]*catalog.Video, 0, count),
		cursors:    make([]int, 0, count),
		nextCursor: cursor,
		favorited:  make(map[string]struct{}),
		liked:      make(map[string]struct{}),
	}

	for cursor < len(videoIDs) && len(batch.videos) < count {
		end := cursor + shortsFeedLookupChunk
		if end > len(videoIDs) {
			end = len(videoIDs)
		}
		visible, err := s.Catalog.VisibleVideosByIDs(ctx, videoIDs[cursor:end])
		if err != nil {
			batch.nextCursor = cursor
			return batch, err
		}
		visibleIDs := make([]string, 0, len(visible))
		for _, video := range visible {
			visibleIDs = append(visibleIDs, video.ID)
		}
		// 一次查询同时服务下面的收藏过滤和客户端要渲染的按钮状态。
		favorited, err := s.Catalog.FavoritedVideoIDsAmong(ctx, selection.viewerUserID, visibleIDs)
		if err != nil {
			batch.nextCursor = cursor
			return batch, err
		}
		// 点赞同样是账号数据：一次取回，供客户端渲染爱心状态。
		liked, err := s.Catalog.LikedVideoIDsAmong(ctx, selection.viewerUserID, visibleIDs)
		if err != nil {
			batch.nextCursor = cursor
			return batch, err
		}
		eligible := make(map[string]*catalog.Video, len(visible))
		for _, video := range visible {
			if selection.likedOnly && video.Likes <= 0 {
				continue
			}
			if selection.favoritesOnly {
				if _, ok := favorited[video.ID]; !ok {
					continue
				}
			}
			eligible[video.ID] = video
		}

		for cursor < end && len(batch.videos) < count {
			videoID := videoIDs[cursor]
			cursor++
			if video := eligible[videoID]; video != nil {
				batch.videos = append(batch.videos, video)
				batch.cursors = append(batch.cursors, cursor)
			}
		}
		for id := range favorited {
			batch.favorited[id] = struct{}{}
		}
		for id := range liked {
			batch.liked[id] = struct{}{}
		}
	}

	batch.nextCursor = cursor
	return batch, nil
}

func (s *Server) mapShortsItems(
	ctx context.Context,
	videos []*catalog.Video,
	itemCursors []int,
	favorited map[string]struct{},
	liked map[string]struct{},
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
		_, isFavorited := favorited[video.ID]
		_, isLiked := liked[video.ID]
		out = append(out, ShortsItemDTO{
			VideoDTO:         dto,
			VideoSrc:         videoSrc,
			Poster:           poster,
			BackgroundPoster: shortsBackgroundPosterURL(poster),
			SizeBytes:        sizeBytes,
			DurationSeconds:  durationSeconds,
			FeedCursor:       feedCursor,
			Favorited:        isFavorited,
			Liked:            isLiked,
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

// ---------------------------------------------------------------------------
// recommend 模式：按批决策
// ---------------------------------------------------------------------------

// handleShortsRecommend builds one batch from the live signals instead of
// serving a pre-computed round snapshot. 「下一条推什么」取决于用户刚看完哪一条，
// 固定快照表达不了这层依赖，所以推荐栏目单独走一条路径。
func (s *Server) handleShortsRecommend(
	w http.ResponseWriter,
	r *http.Request,
	count int,
	cursor int,
	feedToken string,
	after string,
	viewerUserID int64,
) {
	now := s.shortsNow()
	if feedToken == "" && cursor != 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("shorts cursor requires a feed token"))
		return
	}

	var session *shortsFeedSession
	if feedToken != "" {
		session = s.loadShortsRecommendSession(feedToken, now)
		if session == nil {
			writeErr(w, r, http.StatusGone, errShortsFeedExpired)
			return
		}
		// 同一个 cursor 的重试重放同一批。重算会让前端的
		// feedToken:feedCursor 去重键错位，进而触发"返回空批"的报错。
		if batch, ok := session.batches[cursor]; ok {
			s.writeShortsRecommendBatch(w, r, batch, feedToken, cursor, session.total, viewerUserID)
			return
		}
	}

	ids, total, err := s.buildShortsRecommendBatch(r, count, after, viewerUserID, session)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}

	if len(ids) == 0 {
		// 候选被排除干净：让前端换一轮，而不是停在一条永远不结束的批次上。
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, shortsFeedResponse{
			Items:         []ShortsItemDTO{},
			Total:         total,
			FeedToken:     feedToken,
			NextCursor:    cursor,
			RoundComplete: true,
		})
		return
	}

	if feedToken == "" {
		feedToken, err = newShortsFeedToken()
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
	}
	if session == nil {
		session = &shortsFeedSession{}
	}
	if session.batches == nil {
		session.batches = make(map[int][]string)
	}
	session.batches[cursor] = ids
	session.total = total
	session.served = append(session.served, ids...)
	if len(session.served) > shortsServedLimit {
		session.served = session.served[len(session.served)-shortsServedLimit:]
	}
	s.storeShortsRecommendSession(feedToken, session, now)

	s.writeShortsRecommendBatch(w, r, ids, feedToken, cursor, total, viewerUserID)
}

// buildShortsRecommendBatch loads the per-user signals, the candidate pool and
// the directory profiles, then asks the feed package for one batch.
func (s *Server) buildShortsRecommendBatch(
	r *http.Request,
	count int,
	after string,
	viewerUserID int64,
	session *shortsFeedSession,
) ([]string, int, error) {
	ctx := r.Context()
	now := s.shortsNow()

	total, err := s.Catalog.CountVisibleVideos(ctx)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	pool, err := s.Catalog.ShortsCandidates(ctx, shortsCandidateLimit)
	if err != nil {
		return nil, 0, err
	}
	shares, err := s.shortsDirectoryShares(ctx, now)
	if err != nil {
		return nil, 0, err
	}
	profile, err := s.Catalog.ShortsWatchProfile(ctx, viewerUserID, now.Add(-shortsProfileWindow))
	if err != nil {
		return nil, 0, err
	}

	candidates := make([]feed.Candidate, 0, len(pool))
	byID := make(map[string]feed.Candidate, len(pool))
	for _, item := range pool {
		candidate := feed.Candidate{
			ID:              item.ID,
			ParentID:        item.ParentID,
			FileName:        item.FileName,
			DurationSeconds: item.DurationSeconds,
			PublishedAt:     item.PublishedAt,
			Views:           item.Views,
			Likes:           item.Likes,
			Tags:            item.Tags,
		}
		candidates = append(candidates, candidate)
		byID[candidate.ID] = candidate
	}

	// 最近消费过的和本会话已下发的都不再出现。
	served := shortsSessionServed(session)
	exclude := make(map[string]struct{}, len(profile.Consumed)+len(served))
	for id := range profile.Consumed {
		exclude[id] = struct{}{}
	}
	sessionExclude := make(map[string]struct{}, len(served))
	for _, id := range served {
		exclude[id] = struct{}{}
		sessionExclude[id] = struct{}{}
	}
	// 正在看的那一条本身不再出现在这一批里：上报是异步的，此刻它还未必进
	// 了 profile.Consumed。
	if after != "" {
		exclude[after] = struct{}{}
	}

	var lastWatched *feed.Candidate
	if candidate, ok := byID[after]; ok {
		lastWatched = &candidate
	} else if candidate, ok := byID[profile.LastCompleted]; ok {
		lastWatched = &candidate
	}

	input := feed.Input{
		Now:         now,
		Count:       count,
		Pool:        candidates,
		Directories: feed.ProfileDirectories(candidates, feedTagShares(shares)),
		DirAffinity: profile.DirAffinity,
		TagAffinity: profile.TagAffinity,
		Exclude:     exclude,
		LastWatched: lastWatched,
	}
	ids := feed.BuildBatch(input)
	if len(ids) == 0 && len(exclude) > len(sessionExclude) {
		// 用户几乎看完了整个库：先放开历史消费的排除，只保留本会话去重，
		// 否则推荐栏会一直空着。
		input.Exclude = sessionExclude
		ids = feed.BuildBatch(input)
	}
	return ids, total, nil
}

func shortsSessionServed(session *shortsFeedSession) []string {
	if session == nil {
		return nil
	}
	return session.served
}

// writeShortsRecommendBatch loads the chosen ids in order and reports them with
// cursors offset by the request cursor, so the client resumes exactly after the
// last served item.
func (s *Server) writeShortsRecommendBatch(
	w http.ResponseWriter,
	r *http.Request,
	ids []string,
	feedToken string,
	cursor int,
	total int,
	viewerUserID int64,
) {
	batch, err := s.loadShortsFeedBatch(
		r.Context(),
		ids,
		0,
		len(ids),
		shortsFeedSelection{viewerUserID: viewerUserID},
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	cursors := make([]int, len(batch.cursors))
	for index, position := range batch.cursors {
		cursors[index] = cursor + position
	}
	items := s.mapShortsItems(r.Context(), batch.videos, cursors, batch.favorited, batch.liked)
	s.prewarmShortsStreamLinks(batch.videos, r.Header)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, shortsFeedResponse{
		Items:         items,
		Total:         total,
		FeedToken:     feedToken,
		NextCursor:    cursor + len(ids),
		RoundComplete: false,
	})
}

// loadShortsRecommendSession returns the recommend session for token. After a
// restart the served list is restored from the persisted snapshot, so the client
// keeps its resume position and the feed keeps avoiding repeats.
func (s *Server) loadShortsRecommendSession(token string, now time.Time) *shortsFeedSession {
	s.shortsFeedMu.Lock()
	s.pruneShortsFeedsLocked(now)
	if session := s.shortsFeeds[token]; session != nil {
		session.lastAccess = now
		s.shortsFeedMu.Unlock()
		s.touchPersistedShortsFeed(token, now)
		return session
	}
	s.shortsFeedMu.Unlock()

	served := s.loadPersistedShortsFeed(token, now)
	if served == nil {
		return nil
	}
	session := &shortsFeedSession{
		served:  served,
		batches: make(map[int][]string),
	}
	s.shortsFeedMu.Lock()
	s.rememberShortsSessionLocked(token, session, now)
	s.shortsFeedMu.Unlock()
	return session
}

func (s *Server) storeShortsRecommendSession(token string, session *shortsFeedSession, now time.Time) {
	s.shortsFeedMu.Lock()
	s.rememberShortsSessionLocked(token, session, now)
	s.shortsFeedMu.Unlock()
	s.persistShortsFeed(token, session.served, now)
}

// shortsDirectoryShares caches the per-directory tag concentration: it changes
// only when the library is rescanned, and recomputing it per batch would put a
// full-library aggregate on the swipe path.
func (s *Server) shortsDirectoryShares(
	ctx context.Context,
	now time.Time,
) (map[string]catalog.ShortsDirectoryTagShare, error) {
	s.shortsSignalMu.Lock()
	if s.shortsSignalShares != nil && now.Before(s.shortsSignalUntil) {
		shares := s.shortsSignalShares
		s.shortsSignalMu.Unlock()
		return shares, nil
	}
	s.shortsSignalMu.Unlock()

	shares, err := s.Catalog.ShortsDirectoryTagShares(ctx)
	if err != nil {
		return nil, err
	}
	s.shortsSignalMu.Lock()
	s.shortsSignalShares = shares
	s.shortsSignalUntil = now.Add(shortsDirectoryCacheTTL)
	s.shortsSignalMu.Unlock()
	return shares, nil
}

func feedTagShares(shares map[string]catalog.ShortsDirectoryTagShare) map[string]feed.TagShare {
	out := make(map[string]feed.TagShare, len(shares))
	for parentID, share := range shares {
		out[parentID] = feed.TagShare{Tagged: share.Tagged, TopShare: share.TopShare}
	}
	return out
}
