package api

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/localstorage"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/localpath"
	"github.com/video-site/backend/internal/persistence"
	"github.com/video-site/backend/internal/proxy"
	"github.com/video-site/backend/internal/subtitles"
	"github.com/video-site/backend/internal/tagging"
	"github.com/video-site/backend/internal/videoname"
)

const localUploadDriveID = localupload.DriveID

var allowedUploadExtensions = map[string]struct{}{
	".avi":  {},
	".mkv":  {},
	".mov":  {},
	".mp4":  {},
	".webm": {},
}

const maxSubtitleBytes = 20 << 20

type Server struct {
	Catalog         *catalog.Catalog
	Proxy           *proxy.Proxy
	SubtitleClient  subtitles.Client
	LocalDir        string
	UploadDir       string
	OnVideoUploaded func(*catalog.Video)
	RemoteUploads   RemoteUploadService
	// OnHideVideo 处理前台「不再展示」。隐藏机制已废弃，改走拉黑逻辑：
	// 删除库中记录 + 本地封面/预览，保留网盘源文件，并写黑名单墓碑
	// （扫盘不再入库）。未注入时回退为旧的 hidden 标记。
	OnHideVideo func(ctx context.Context, videoID string) error

	tagCacheMu      sync.Mutex
	tagCacheUntil   time.Time
	tagCache        []TagDTO
	tagCacheVersion uint64

	shortsFeedMu  sync.Mutex
	shortsFeeds   map[string]*shortsFeedSession
	shortsFeedNow func() time.Time

	videoFeedMu     sync.Mutex
	videoFeeds      map[string]*videoFeedSession
	videoFeedShared map[videoFeedSnapshotKey]string
	videoFeedNow    func() time.Time

	homeRecommendationMu       sync.Mutex
	homeRecommendationSessions map[string]*homeRecommendationSession
	homeRecommendationNow      func() time.Time
	homeLatestSnapshotMu       sync.Mutex
	homeLatestSnapshot         []string
	homeLatestSnapshotUntil    time.Time

	// shareNow is injectable so one-time share expiry behavior can be tested
	// without sleeping. Production leaves it nil and uses time.Now.
	shareNow func() time.Time

	// GetTheme 返回当前生效的主题（"dark" | "pink" | "sky"）。前台 /api/settings/theme 用，
	// 不需要登录。无注入时返回 "dark"。
	GetTheme func() string

	subtitleCacheMu  sync.Mutex
	subtitleCache    map[string]subtitleCacheEntry
	subtitleCacheNow func() time.Time
}

// InvalidateTagCache makes catalog mutations visible to the public tag list
// immediately instead of waiting for the short response cache to expire.
func (s *Server) InvalidateTagCache() {
	if s == nil {
		return
	}
	s.tagCacheMu.Lock()
	s.tagCacheVersion++
	s.tagCache = nil
	s.tagCacheUntil = time.Time{}
	s.tagCacheMu.Unlock()
}

type subtitleCacheEntry struct {
	videoID string
	subs    []subtitles.Subtitle
	expires time.Time
	created time.Time
}

const (
	homePageSize = 12
)

var subtitleHTTPClient = &http.Client{Timeout: 30 * time.Second}

// VideoDTO 是返回给前端的视频对象，字段名跟前端 VideoItem 对齐
type VideoDTO struct {
	ID              string   `json:"id"`
	Href            string   `json:"href"`
	Title           string   `json:"title"`
	Thumbnail       string   `json:"thumbnail"`
	PreviewSrc      string   `json:"previewSrc"`
	PreviewDuration int      `json:"previewDuration"`
	PreviewStrategy string   `json:"previewStrategy"`
	Duration        string   `json:"duration"`
	Badges          []string `json:"badges"`
	SourceLabel     string   `json:"sourceLabel,omitempty"`
	Author          string   `json:"author"`
	Views           int      `json:"views"`
	Favorites       int      `json:"favorites"`
	Comments        int      `json:"comments"`
	Likes           int      `json:"likes"`
	Dislikes        int      `json:"dislikes"`
	PublishedAt     string   `json:"publishedAt"`
	Tags            []string `json:"tags,omitempty"`
}

// VideoCardDTO is the compact transport used by infinite listing feeds. The
// omitted reaction, tag, storage and processing fields are loaded by the video
// detail endpoint when the user actually opens a card.
type VideoCardDTO struct {
	ID              string   `json:"id"`
	Href            string   `json:"href"`
	Title           string   `json:"title"`
	Thumbnail       string   `json:"thumbnail"`
	PreviewSrc      string   `json:"previewSrc"`
	PreviewDuration int      `json:"previewDuration"`
	PreviewStrategy string   `json:"previewStrategy"`
	Duration        string   `json:"duration"`
	Badges          []string `json:"badges"`
	Author          string   `json:"author"`
	Views           int      `json:"views"`
	PublishedAt     string   `json:"publishedAt"`
}

type TagDTO struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type VideoDetailDTO struct {
	VideoDTO
	VideoSrc      string                  `json:"videoSrc"`
	MediaType     string                  `json:"mediaType,omitempty"`
	Poster        string                  `json:"poster"`
	Description   string                  `json:"description"`
	EmbedURL      string                  `json:"embedUrl"`
	Points        int                     `json:"points,omitempty"`
	AuthorProfile AuthorProfile           `json:"authorProfile"`
	Collection    *VideoCollectionSummary `json:"collection,omitempty"`
	RelatedVideos []VideoDTO              `json:"relatedVideos"`
	CommentsList  []Comment               `json:"commentsList"`
}

type VideoCollectionSummary struct {
	Name         string `json:"name"`
	Total        int    `json:"total"`
	CurrentIndex int    `json:"currentIndex"`
}

type VideoCollectionDTO struct {
	VideoCollectionSummary
	Items []VideoCollectionItemDTO `json:"items"`
}

// VideoCollectionItemDTO stays compact for the mobile sheet. Desktop callers
// can explicitly request PreviewSrc without pulling reactions, tags, badges or
// the rest of VideoDTO for every item in a large provider directory.
type VideoCollectionItemDTO struct {
	ID          string `json:"id"`
	Href        string `json:"href"`
	Title       string `json:"title"`
	Thumbnail   string `json:"thumbnail"`
	PreviewSrc  string `json:"previewSrc,omitempty"`
	Duration    string `json:"duration"`
	Views       int    `json:"views"`
	PublishedAt string `json:"publishedAt"`
}

type SubtitleDTO struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Language string `json:"language,omitempty"`
	Ext      string `json:"ext"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Source   string `json:"source"`
}

type AuthorProfile struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Href   string   `json:"href"`
	Badges []string `json:"badges"`
}

type Comment struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	Likes     int    `json:"likes,omitempty"`
}

// RegisterRoutes 挂载前台 REST 路由。前台接口需要登录态。
func (s *Server) RegisterRoutes(r chi.Router, a *auth.Authenticator) {
	// 公开端点：拿当前生效的主题。登录页本身要在挂前就能读，所以单独挂在
	// 鉴权组之外。只暴露 theme 一个字段，避免泄露其他设置。
	r.Get("/api/settings/theme", s.handleGetTheme)

	// 一次性分享的领取和媒体路由必须公开，但每个媒体请求都会再次校验
	// HttpOnly 分享会话，并且只能访问该分享绑定的单个视频。
	r.Post("/api/share/consume", s.handleConsumeVideoShare)
	r.Get("/api/share/{shareID}/subtitles", s.handleSharedVideoSubtitles)
	r.Post("/api/share/{shareID}/view", s.handleSharedVideoView)
	r.Get("/p/share/{shareID}/stream", s.handleSharedVideoStream)
	r.Get("/p/share/{shareID}/preview", s.handleSharedVideoPreview)
	r.Get("/p/share/{shareID}/thumb", s.handleSharedVideoThumb)
	r.Get("/p/share/{shareID}/subtitle/{index}", s.handleSharedSubtitleFile)

	r.Group(func(r chi.Router) {
		r.Use(a.Required)
		r.Get("/api/home", s.handleHome)
		r.Get("/api/home/latest", s.handleHomeLatest)
		r.Get("/api/list", s.handleList)
		r.Get("/api/feed", s.handleVideoFeed)
		r.Get("/api/video/{id}", s.handleVideoDetail)
		r.Get("/api/video/{id}/collection", s.handleVideoCollection)
		r.Get("/api/video/{id}/subtitles", s.handleVideoSubtitles)
		r.Post("/api/video/{id}/share", s.handleCreateVideoShare)
		r.Put("/api/video/{id}/reaction", s.handleSetVideoReaction)
		r.Post("/api/video/{id}/like", s.handleLike)
		r.Delete("/api/video/{id}/like", s.handleUnlike)
		r.Post("/api/video/{id}/view", s.handleView)
		r.Get("/api/tags", s.handleTags)
		r.Get("/api/shorts/next", s.handleShortsNext)

		// 代理路由同样需要鉴权，防止绕过
		r.Get("/p/stream/{driveID}/*", s.handleStream)
		r.Get("/p/subtitle/{id}/{index}", s.handleSubtitleFile)
		r.Get("/p/upload/{videoID}", s.handleUploadedVideo)
		r.Get("/p/preview/{videoID}", s.handlePreview)
		r.Get("/p/thumb/{videoID}", s.handleThumb)
	})

	r.Group(func(r chi.Router) {
		r.Use(a.AdminRequired)
		r.Put("/api/video/{id}/tags", s.handleUpdateVideoTags)
		r.Post("/api/video/{id}/hide", s.handleHideVideo)
		r.Get("/api/upload/tags", s.handleUploadTags)
		r.Post("/api/upload", s.handleUploadVideo)
		r.Post("/api/upload/remote", s.handleCreateRemoteUpload)
		r.Get("/api/upload/remote", s.handleListRemoteUploads)
		r.Post("/api/upload/remote/{jobId}/cancel", s.handleCancelRemoteUpload)
	})
}

// handleGetTheme 返回当前生效的主题。无需登录。响应永远是
// {"theme": "dark" | "pink" | "sky"}，便于前端无脑解析。
func (s *Server) handleGetTheme(w http.ResponseWriter, r *http.Request) {
	theme := "dark"
	if s.GetTheme != nil {
		if v := s.GetTheme(); v == "pink" || v == "dark" || v == "sky" {
			theme = v
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"theme": theme})
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	count, err := homeRecommendationCount(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	recommendationSession, persistentSession := s.homeSessionFromContext(r.Context())
	recommendationSession.requestMu.Lock()
	defer recommendationSession.requestMu.Unlock()

	items, roundVideoIDs, roundCursor, err := s.nextHomeRecommendationBatch(
		r.Context(),
		recommendationSession,
		count,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	recommendationSession.roundVideoIDs = roundVideoIDs
	recommendationSession.roundCursor = roundCursor
	if persistentSession {
		s.touchHomeRecommendationSession(recommendationSession)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, mapVideos(items))
}

func (s *Server) handleHomeLatest(w http.ResponseWriter, r *http.Request) {
	count, err := homeRecommendationCount(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	session, persistentSession := s.homeSessionFromContext(r.Context())
	session.latestRequestMu.Lock()
	defer session.latestRequestMu.Unlock()

	items, latestVideoIDs, latestCursor, err := s.nextHomeLatestBatch(
		r.Context(),
		session,
		count,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	session.latestVideoIDs = latestVideoIDs
	session.latestCursor = latestCursor
	if persistentSession {
		s.touchHomeRecommendationSession(session)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, mapVideos(items))
}

func (s *Server) homeSessionFromContext(ctx context.Context) (*homeRecommendationSession, bool) {
	if identity, ok := auth.SessionIdentityFromContext(ctx); ok {
		return s.homeRecommendationSession(identity), true
	}
	return &homeRecommendationSession{}, false
}

func homeRecommendationCount(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("count"))
	if raw == "" {
		return homePageSize, nil
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count < 1 || count > homePageSize {
		return 0, errors.New("invalid home recommendation count")
	}
	return count, nil
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	params := publicListParams(r)
	items, total, err := s.Catalog.ListVideos(r.Context(), params)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"items": mapVideos(items),
		"total": total,
		"page":  params.Page,
		"size":  params.PageSize,
	})
}

// publicListParams is shared by the paginated compatibility endpoint and the
// snapshot feed so both expose the same filters and ordering semantics.
func publicListParams(r *http.Request) catalog.ListParams {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = 24
	}
	sortKey := strings.TrimSpace(q.Get("sort"))
	switch sortKey {
	case "", "latest", "hot", "recent":
	default:
		sortKey = ""
	}
	return catalog.ListParams{
		Keyword:               strings.TrimSpace(q.Get("q")),
		Tag:                   strings.TrimSpace(q.Get("tag")),
		Sort:                  sortKey,
		Page:                  page,
		PageSize:              size,
		SkipTotal:             strings.EqualFold(q.Get("count"), "false"),
		PreferReadyThumbnails: sortKey == "" || sortKey == "latest",
	}
}

func (s *Server) handleVideoDetail(w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	v, err := s.availableVideo(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	related := s.pickRelatedVideos(r.Context(), v, 6)
	collection, _, _ := s.videoCollection(r.Context(), v)
	dto := mapVideo(v)
	if d, err := s.Catalog.GetDrive(r.Context(), v.DriveID); err == nil {
		dto.SourceLabel = driveKindLabel(d.Kind)
	}

	detail := VideoDetailDTO{
		VideoDTO:    dto,
		VideoSrc:    s.videoSource(v),
		MediaType:   playbackMediaType(v),
		Poster:      thumbnailURL(v),
		Description: v.Description,
		EmbedURL:    fmt.Sprintf(`<iframe src="/embed/%s" width="640" height="360" frameborder="0" allowfullscreen></iframe>`, pathSegment(v.ID)),
		AuthorProfile: AuthorProfile{
			ID:     "author-" + v.Author,
			Name:   v.Author,
			Href:   "/author/" + v.Author,
			Badges: []string{},
		},
		Collection:    collection,
		RelatedVideos: mapVideos(related),
		CommentsList:  []Comment{},
	}
	// 推荐每次随机生成，禁止浏览器和中间层缓存详情响应
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleVideoCollection(w http.ResponseWriter, r *http.Request) {
	v, err := s.availableVideo(r.Context(), routeParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	summary, items, err := s.videoCollection(r.Context(), v)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if summary == nil {
		summary = &VideoCollectionSummary{}
		items = []*catalog.Video{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, VideoCollectionDTO{
		VideoCollectionSummary: *summary,
		Items: mapVideoCollectionItems(
			items,
			r.URL.Query().Get("preview") == "1",
		),
	})
}

// videoCollection builds a directory-backed collection in one canonical order.
// The detail response keeps only its lightweight summary; the full item list is
// loaded through /collection when either collection UI is opened.
func (s *Server) videoCollection(ctx context.Context, current *catalog.Video) (*VideoCollectionSummary, []*catalog.Video, error) {
	if current == nil || strings.TrimSpace(current.ParentID) == "" {
		return nil, nil, nil
	}
	items, err := s.Catalog.ListVisibleVideosByDirectory(ctx, current.DriveID, current.ParentID)
	if err != nil {
		return nil, nil, err
	}
	if len(items) < 2 {
		return nil, items, nil
	}
	sort.SliceStable(items, func(i, j int) bool {
		return naturalVideoCollectionLess(items[i], items[j])
	})
	currentIndex := 0
	for index, item := range items {
		if item != nil && item.ID == current.ID {
			currentIndex = index + 1
			break
		}
	}
	if currentIndex == 0 {
		return nil, items, nil
	}
	name := strings.TrimSpace(current.DirName)
	if name == "" {
		for _, item := range items {
			if item != nil && strings.TrimSpace(item.DirName) != "" {
				name = strings.TrimSpace(item.DirName)
				break
			}
		}
	}
	if name == "" {
		name = "同目录视频"
	}
	return &VideoCollectionSummary{
		Name:         name,
		Total:        len(items),
		CurrentIndex: currentIndex,
	}, items, nil
}

func naturalVideoCollectionLess(left, right *catalog.Video) bool {
	if left == nil || right == nil {
		return left != nil
	}
	leftName := firstNonEmptyString(left.FileName, left.Title, left.ID)
	rightName := firstNonEmptyString(right.FileName, right.Title, right.ID)
	if compared := naturalCompare(leftName, rightName); compared != 0 {
		return compared < 0
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}

// naturalCompare compares ASCII digit runs numerically while keeping all other
// Unicode runes in a deterministic case-insensitive order (episode 2 < 10).
func naturalCompare(left, right string) int {
	leftRunes := []rune(strings.ToLower(strings.TrimSpace(left)))
	rightRunes := []rune(strings.ToLower(strings.TrimSpace(right)))
	for li, ri := 0, 0; li < len(leftRunes) && ri < len(rightRunes); {
		if isASCIIDigit(leftRunes[li]) && isASCIIDigit(rightRunes[ri]) {
			leftEnd, rightEnd := li, ri
			for leftEnd < len(leftRunes) && isASCIIDigit(leftRunes[leftEnd]) {
				leftEnd++
			}
			for rightEnd < len(rightRunes) && isASCIIDigit(rightRunes[rightEnd]) {
				rightEnd++
			}
			leftSignificant, rightSignificant := li, ri
			for leftSignificant < leftEnd-1 && leftRunes[leftSignificant] == '0' {
				leftSignificant++
			}
			for rightSignificant < rightEnd-1 && rightRunes[rightSignificant] == '0' {
				rightSignificant++
			}
			leftDigits := leftRunes[leftSignificant:leftEnd]
			rightDigits := rightRunes[rightSignificant:rightEnd]
			if len(leftDigits) != len(rightDigits) {
				if len(leftDigits) < len(rightDigits) {
					return -1
				}
				return 1
			}
			for index := range leftDigits {
				if leftDigits[index] < rightDigits[index] {
					return -1
				}
				if leftDigits[index] > rightDigits[index] {
					return 1
				}
			}
			// Numerically equal runs use the shorter spelling first: 2 before 02.
			if leftEnd-li != rightEnd-ri {
				if leftEnd-li < rightEnd-ri {
					return -1
				}
				return 1
			}
			li, ri = leftEnd, rightEnd
			continue
		}
		if leftRunes[li] < rightRunes[ri] {
			return -1
		}
		if leftRunes[li] > rightRunes[ri] {
			return 1
		}
		li++
		ri++
	}
	if len(leftRunes) < len(rightRunes) {
		return -1
	}
	if len(leftRunes) > len(rightRunes) {
		return 1
	}
	return 0
}

func isASCIIDigit(value rune) bool {
	return value >= '0' && value <= '9'
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// pickRelatedVideos 选 total 个推荐视频。
// 一半来自同标签命中，剩下用全库随机补齐；两段都优先取已有封面的视频，
// 不够时再回退到未生成封面的候选。结果不会重复，也不会包含当前视频。
func (s *Server) pickRelatedVideos(ctx context.Context, current *catalog.Video, total int) []*catalog.Video {
	if total <= 0 || current == nil {
		return nil
	}
	tagQuota := total / 2
	if tagQuota <= 0 && len(current.Tags) > 0 {
		tagQuota = 1
	}

	picked := make([]*catalog.Video, 0, total)
	seen := map[string]struct{}{current.ID: {}}

	// 1) 同标签候选：先取已有封面的候选，数量不够再从全部候选里补。
	if tagQuota > 0 && len(current.Tags) > 0 {
		picked = appendRandomRelated(
			picked,
			s.relatedTagPool(ctx, current.Tags, seen, true),
			tagQuota,
			seen,
		)
		if len(picked) < tagQuota {
			picked = appendRandomRelated(
				picked,
				s.relatedTagPool(ctx, current.Tags, seen, false),
				tagQuota,
				seen,
			)
		}
	}

	// 2) 随机补齐：同样优先已有封面的全库候选，不够再回退。
	if len(picked) < total {
		picked = appendRandomRelated(
			picked,
			s.relatedListPool(ctx, seen, true, 200),
			total,
			seen,
		)
	}
	if len(picked) < total {
		picked = appendRandomRelated(
			picked,
			s.relatedListPool(ctx, seen, false, 200),
			total,
			seen,
		)
	}

	return picked
}

func (s *Server) relatedTagPool(ctx context.Context, tags []string, seen map[string]struct{}, readyOnly bool) []*catalog.Video {
	var pool []*catalog.Video
	poolSeen := make(map[string]struct{})
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		items, _, err := s.Catalog.ListVideos(ctx, catalog.ListParams{
			Tag:                   tag,
			Sort:                  "latest",
			Page:                  1,
			PageSize:              30,
			ThumbnailReadyOnly:    readyOnly,
			PreferReadyThumbnails: !readyOnly,
			SkipTotal:             true,
		})
		if err != nil {
			continue
		}
		for _, v := range items {
			if v == nil {
				continue
			}
			if _, ok := seen[v.ID]; ok {
				continue
			}
			if _, ok := poolSeen[v.ID]; ok {
				continue
			}
			poolSeen[v.ID] = struct{}{}
			pool = append(pool, v)
		}
	}
	return pool
}

func (s *Server) relatedListPool(ctx context.Context, seen map[string]struct{}, readyOnly bool, pageSize int) []*catalog.Video {
	items, _, err := s.Catalog.ListVideos(ctx, catalog.ListParams{
		Sort:                  "latest",
		Page:                  1,
		PageSize:              pageSize,
		ThumbnailReadyOnly:    readyOnly,
		PreferReadyThumbnails: !readyOnly,
		SkipTotal:             true,
	})
	if err != nil {
		return nil
	}
	pool := make([]*catalog.Video, 0, len(items))
	for _, v := range items {
		if v == nil {
			continue
		}
		if _, ok := seen[v.ID]; ok {
			continue
		}
		pool = append(pool, v)
	}
	return pool
}

func appendRandomRelated(picked []*catalog.Video, pool []*catalog.Video, targetLen int, seen map[string]struct{}) []*catalog.Video {
	if len(picked) >= targetLen || len(pool) == 0 {
		return picked
	}
	rand.Shuffle(len(pool), func(i, j int) {
		pool[i], pool[j] = pool[j], pool[i]
	})
	for _, v := range pool {
		if len(picked) >= targetLen {
			break
		}
		if v == nil {
			continue
		}
		if _, ok := seen[v.ID]; ok {
			continue
		}
		seen[v.ID] = struct{}{}
		picked = append(picked, v)
	}
	return picked
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	s.tagCacheMu.Lock()
	cacheVersion := s.tagCacheVersion
	if s.tagCache != nil && now.Before(s.tagCacheUntil) {
		out := append([]TagDTO(nil), s.tagCache...)
		s.tagCacheMu.Unlock()
		w.Header().Set("Cache-Control", "private, max-age=15")
		writeJSON(w, http.StatusOK, out)
		return
	}
	s.tagCacheMu.Unlock()

	stats, err := s.Catalog.ListTags(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]TagDTO, 0, len(stats))
	for _, stat := range stats {
		out = append(out, TagDTO{ID: stat.Label, Label: stat.Label, Count: stat.Count})
	}
	s.tagCacheMu.Lock()
	if cacheVersion == s.tagCacheVersion {
		s.tagCache = append([]TagDTO(nil), out...)
		s.tagCacheUntil = now.Add(30 * time.Second)
	}
	s.tagCacheMu.Unlock()

	w.Header().Set("Cache-Control", "private, max-age=15")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUploadTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.Catalog.ListUserSelectableTags(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]TagDTO, 0, len(tags))
	for _, tag := range tags {
		out = append(out, TagDTO{ID: tag.Label, Label: tag.Label, Count: tag.Count})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

type updateVideoTagsReq struct {
	Tags []string `json:"tags"`
}

func (s *Server) handleUpdateVideoTags(w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	var body updateVideoTagsReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Catalog.SetManualVideoTags(r.Context(), id, body.Tags); err != nil {
		if errors.Is(err, catalog.ErrUnknownTag) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	v, err := s.Catalog.GetVideo(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, mapVideo(v))
}

func (s *Server) handleLike(w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	likes, err := s.Catalog.IncrementLike(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"likes": likes})
}

type setVideoReactionReq struct {
	VisitID  string                `json:"visitId"`
	Reaction catalog.VideoReaction `json:"reaction"`
}

func (s *Server) handleSetVideoReaction(w http.ResponseWriter, r *http.Request) {
	var body setVideoReactionReq
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return
	}

	result, err := s.Catalog.SetVisitReaction(
		r.Context(),
		routeParam(r, "id"),
		body.VisitID,
		body.Reaction,
	)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrInvalidVideoReaction),
			errors.Is(err, catalog.ErrInvalidVideoReactionVisitID):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, http.StatusNotFound, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleUnlike 取消点赞：likes - 1（保底 0）。
// 短视频模式中爱心按钮点击切换状态时使用。
func (s *Server) handleUnlike(w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	likes, err := s.Catalog.DecrementLike(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"likes": likes})
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	views, err := s.Catalog.IncrementView(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"views": views})
}

func (s *Server) handleHideVideo(w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	var err error
	if s.OnHideVideo != nil {
		// 走拉黑逻辑：删记录 + 删本地封面/预览 + 写墓碑，保留网盘源文件。
		err = s.OnHideVideo(r.Context(), id)
	} else {
		err = s.Catalog.HideVideo(r.Context(), id)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUploadVideo(w http.ResponseWriter, r *http.Request) {
	if s.LocalDir == "" {
		writeErr(w, http.StatusInternalServerError, errors.New("local storage is not configured"))
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("video file is required"))
		return
	}
	defer file.Close()

	originalName := filepath.Base(strings.TrimSpace(header.Filename))
	ext := strings.ToLower(filepath.Ext(originalName))
	if _, ok := allowedUploadExtensions[ext]; !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unsupported video extension: %s", ext))
		return
	}

	tags, err := s.canonicalUploadTags(r.Context(), uploadTagValues(r))
	if err != nil {
		status := http.StatusInternalServerError
		if isUploadTagValidationError(err) {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err)
		return
	}

	now := time.Now()
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = uploadTitleFromFileName(originalName)
	}
	if err := videoname.ValidateUploadTitle(title, ext); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	uploadID, err := newUploadID(now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	storedName := videoname.UploadFileName(title, ext, uploadID, false)
	dst, err := s.localUploadFilePath(storedName)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if _, statErr := os.Lstat(dst); statErr == nil {
		storedName = videoname.UploadFileName(title, ext, uploadID, true)
		dst, err = s.localUploadFilePath(storedName)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	} else if !os.IsNotExist(statErr) {
		writeErr(w, http.StatusInternalServerError, statErr)
		return
	}
	partPath := dst + ".part"
	out, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		storedName = videoname.UploadFileName(title, ext, uploadID, true)
		dst, err = s.localUploadFilePath(storedName)
		if err == nil {
			partPath = dst + ".part"
			out, err = os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	title = videoname.TitleFromFileName(storedName)
	size, copyErr := io.Copy(out, file)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(partPath)
		writeErr(w, http.StatusInternalServerError, copyErr)
		return
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		writeErr(w, http.StatusInternalServerError, closeErr)
		return
	}
	if size <= 0 {
		_ = os.Remove(partPath)
		writeErr(w, http.StatusBadRequest, errors.New("uploaded video is empty"))
		return
	}
	if err := os.Chmod(partPath, 0o644); err != nil {
		_ = os.Remove(partPath)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// The potentially long request-body copy above writes an excluded .part
	// file. Only publication of the final file and its catalog rows needs to be
	// atomic with respect to a backup snapshot.
	persistence.RLock()
	mutationLocked := true
	defer func() {
		if mutationLocked {
			persistence.RUnlock()
		}
	}()
	if _, err := os.Lstat(dst); err == nil {
		_ = os.Remove(partPath)
		writeErr(w, http.StatusConflict, errors.New("目标视频文件已存在，请重试"))
		return
	} else if !os.IsNotExist(err) {
		_ = os.Remove(partPath)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(partPath, dst); err != nil {
		_ = os.Remove(partPath)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	video := &catalog.Video{
		ID:            localUploadDriveID + "-" + uploadID,
		DriveID:       localUploadDriveID,
		FileID:        storedName,
		FileName:      storedName,
		Title:         title,
		Author:        "用户上传",
		Size:          size,
		Ext:           strings.TrimPrefix(ext, "."),
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.Catalog.UpsertVideo(r.Context(), video); err != nil {
		_ = os.Remove(dst)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(tags) > 0 {
		if err := s.Catalog.SetManualVideoTags(r.Context(), video.ID, tags); err != nil {
			_ = os.Remove(dst)
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if saved, err := s.Catalog.GetVideo(r.Context(), video.ID); err == nil {
			video = saved
		}
	}
	persistence.RUnlock()
	mutationLocked = false
	if s.OnVideoUploaded != nil {
		s.OnVideoUploaded(video)
	}
	writeJSON(w, http.StatusCreated, mapVideo(video))
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	driveID := routeParam(r, "driveID")
	fileID := routeWildcardParam(r, "*")
	s.Proxy.ServeStream(w, r, driveID, fileID)
}

func (s *Server) handleVideoSubtitles(w http.ResponseWriter, r *http.Request) {
	v, ok := s.visibleVideo(w, r, routeParam(r, "id"))
	if !ok {
		return
	}
	subs, err := s.loadVideoSubtitles(r.Context(), v)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSubtitles(v.ID, subs))
}

func (s *Server) handleSubtitleFile(w http.ResponseWriter, r *http.Request) {
	v, ok := s.visibleVideo(w, r, routeParam(r, "id"))
	if !ok {
		return
	}
	index, err := strconv.Atoi(routeParam(r, "index"))
	if err != nil || index < 0 {
		writeErr(w, http.StatusBadRequest, errors.New("invalid subtitle index"))
		return
	}
	s.serveSubtitleSelection(w, r, v, index)
}

func (s *Server) visibleVideo(w http.ResponseWriter, r *http.Request, id string) (*catalog.Video, bool) {
	v, err := s.Catalog.GetVideo(r.Context(), id)
	if err != nil || v.Hidden {
		writeErr(w, http.StatusNotFound, sql.ErrNoRows)
		return nil, false
	}
	return v, true
}

func (s *Server) handleUploadedVideo(w http.ResponseWriter, r *http.Request) {
	videoID := routeParam(r, "videoID")
	v, err := s.Catalog.GetVideo(r.Context(), videoID)
	if err != nil || v.Hidden || v.DriveID != localUploadDriveID {
		http.NotFound(w, r)
		return
	}
	s.serveUploadedVideo(w, r, v)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	videoID := routeParam(r, "videoID")
	v, err := s.Catalog.GetVideo(r.Context(), videoID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.servePreviewVideo(w, r, v)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	s.serveVideoThumb(w, r, routeParam(r, "videoID"))
}

// ---------- helpers ----------

func mapVideo(v *catalog.Video) VideoDTO {
	badges := v.Badges
	if badges == nil {
		badges = []string{}
	}
	tags := v.Tags
	if tags == nil {
		tags = []string{}
	}
	return VideoDTO{
		ID:              v.ID,
		Href:            "/video/" + pathSegment(v.ID),
		Title:           v.Title,
		Thumbnail:       thumbnailURL(v),
		PreviewSrc:      previewURL(v),
		PreviewDuration: 12,
		PreviewStrategy: "teaser-file",
		Duration:        formatDuration(v.DurationSeconds),
		Badges:          badges,
		Author:          v.Author,
		Views:           v.Views,
		Favorites:       v.Favorites,
		Comments:        v.Comments,
		Likes:           v.Likes,
		Dislikes:        v.Dislikes,
		PublishedAt:     v.PublishedAt.Format("2006-01-02"),
		Tags:            tags,
	}
}

func (s *Server) loadVideoSubtitles(ctx context.Context, v *catalog.Video) ([]subtitles.Subtitle, error) {
	if v == nil || s.SubtitleClient == nil {
		return []subtitles.Subtitle{}, nil
	}
	request := subtitles.Request{
		FileID:          v.FileID,
		FileName:        v.FileName,
		LookupNames:     subtitleLookupAliases(v),
		ContentHash:     v.ContentHash,
		SampledSHA256:   v.SampledSHA256,
		DurationSeconds: v.DurationSeconds,
	}
	cacheKey := subtitleCacheKey(v, request)
	if subs, ok := s.cachedSubtitles(cacheKey); ok {
		return subs, nil
	}
	subs, err := s.SubtitleClient.Subtitles(ctx, request)
	if err != nil {
		// Subtitle lookup is best effort. Authentication changes, timeouts, rate
		// limits, and malformed upstream responses must never interrupt playback.
		subs = []subtitles.Subtitle{}
	}
	subs = filterSupportedSubtitles(subs, v.DurationSeconds)
	s.cacheSubtitles(cacheKey, v.ID, subs)
	return cloneSubtitles(subs), nil
}

func subtitleLookupAliases(v *catalog.Video) []string {
	if v == nil {
		return nil
	}
	for _, candidate := range []string{v.FileName, v.ID, v.Title} {
		if code := tagging.FindSubtitleAVCode(candidate); code != "" {
			return []string{code}
		}
	}
	return nil
}

func subtitleCacheKey(v *catalog.Video, req subtitles.Request) string {
	return strings.Join([]string{
		v.ID, v.DriveID, req.FileID, req.FileName,
		strings.Join(req.LookupNames, "\x1f"), req.ContentHash, req.SampledSHA256,
		strconv.Itoa(req.DurationSeconds),
	}, "\x00")
}

func (s *Server) subtitleNow() time.Time {
	if s.subtitleCacheNow != nil {
		return s.subtitleCacheNow()
	}
	return time.Now()
}

func (s *Server) cachedSubtitles(key string) ([]subtitles.Subtitle, bool) {
	now := s.subtitleNow()
	s.subtitleCacheMu.Lock()
	defer s.subtitleCacheMu.Unlock()
	entry, ok := s.subtitleCache[key]
	if !ok || !now.Before(entry.expires) {
		if ok {
			delete(s.subtitleCache, key)
		}
		return nil, false
	}
	return cloneSubtitles(entry.subs), true
}

func (s *Server) cacheSubtitles(key, videoID string, subs []subtitles.Subtitle) {
	now := s.subtitleNow()
	ttl := 5 * time.Minute
	if len(subs) == 0 {
		ttl = time.Minute
	}
	s.subtitleCacheMu.Lock()
	defer s.subtitleCacheMu.Unlock()
	if s.subtitleCache == nil {
		s.subtitleCache = make(map[string]subtitleCacheEntry)
	}
	for existingKey, entry := range s.subtitleCache {
		if !now.Before(entry.expires) {
			delete(s.subtitleCache, existingKey)
		}
	}
	if len(s.subtitleCache) >= 2048 {
		var oldestKey string
		var oldest time.Time
		for existingKey, entry := range s.subtitleCache {
			if oldestKey == "" || entry.created.Before(oldest) {
				oldestKey, oldest = existingKey, entry.created
			}
		}
		delete(s.subtitleCache, oldestKey)
	}
	s.subtitleCache[key] = subtitleCacheEntry{videoID: videoID, subs: cloneSubtitles(subs), expires: now.Add(ttl), created: now}
}

func (s *Server) invalidateSubtitleCache(videoID string) {
	s.subtitleCacheMu.Lock()
	defer s.subtitleCacheMu.Unlock()
	for key, entry := range s.subtitleCache {
		if entry.videoID == videoID {
			delete(s.subtitleCache, key)
		}
	}
}

func cloneSubtitles(subs []subtitles.Subtitle) []subtitles.Subtitle {
	out := make([]subtitles.Subtitle, len(subs))
	copy(out, subs)
	return out
}

func filterSupportedSubtitles(subs []subtitles.Subtitle, videoDuration int) []subtitles.Subtitle {
	out := make([]subtitles.Subtitle, 0, len(subs))
	for _, sub := range subs {
		if subtitlePlayerType(sub.Ext) == "" {
			continue
		}
		if strings.TrimSpace(sub.URL) == "" {
			continue
		}
		if !subtitles.DurationCompatible(videoDuration, sub.DurationSeconds) {
			continue
		}
		out = append(out, sub)
	}
	sort.SliceStable(out, func(i, j int) bool {
		iGroup, iDelta := subtitleDurationOrder(out[i].DurationSeconds, videoDuration)
		jGroup, jDelta := subtitleDurationOrder(out[j].DurationSeconds, videoDuration)
		if iGroup != jGroup {
			return iGroup < jGroup
		}
		if iDelta != jDelta {
			return iDelta < jDelta
		}
		iLanguage := subtitleLanguageOrder(out[i])
		jLanguage := subtitleLanguageOrder(out[j])
		if iLanguage != jLanguage {
			return iLanguage < jLanguage
		}
		return subtitleOrderKey(out[i]) < subtitleOrderKey(out[j])
	})
	return out
}

func subtitleDurationOrder(subtitleDuration, videoDuration int) (group, delta int) {
	if videoDuration <= 0 {
		return 0, 0
	}
	if subtitleDuration <= 0 {
		return 1, 0
	}
	delta = subtitleDuration - videoDuration
	if delta < 0 {
		delta = -delta
	}
	if subtitles.DurationCompatible(videoDuration, subtitleDuration) {
		return 0, delta
	}
	return 2, delta
}

func subtitleLanguageOrder(sub subtitles.Subtitle) int {
	language := strings.ToLower(strings.TrimSpace(sub.Language))
	language = strings.ReplaceAll(language, "_", "-")
	if language == "zh" || strings.HasPrefix(language, "zh-") || language == "zho" || language == "chi" || language == "cmn" {
		return 0
	}
	if language != "" {
		return 3
	}
	name := strings.ToLower(strings.TrimSpace(sub.Name))
	if strings.Contains(name, "中文") || strings.Contains(name, "中字") || strings.Contains(name, "简体") || strings.Contains(name, "繁体") ||
		strings.Contains(name, "zh-cn") || strings.Contains(name, "zh_cn") || strings.Contains(name, "zh-tw") || strings.Contains(name, "zh_tw") ||
		subtitleNameHasMarker(name, "chs") || subtitleNameHasMarker(name, "cht") {
		return 1
	}
	return 2
}

func subtitleNameHasMarker(name, marker string) bool {
	for _, field := range strings.FieldsFunc(name, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		if field == marker {
			return true
		}
	}
	return false
}

func subtitleOrderKey(sub subtitles.Subtitle) string {
	rawURL := strings.TrimSpace(sub.URL)
	if parsed, err := url.Parse(rawURL); err == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		rawURL = parsed.String()
	}
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(sub.Language)),
		strings.ToLower(strings.TrimSpace(sub.Name)),
		normalizeSubtitleExt(sub.Ext),
		strconv.Itoa(sub.Source),
		strings.TrimSpace(sub.SourceLabel),
		strings.TrimSpace(sub.ID),
		rawURL,
	}, "\x00")
}

func mapSubtitles(videoID string, subs []subtitles.Subtitle) []SubtitleDTO {
	out := make([]SubtitleDTO, 0, len(subs))
	for index, sub := range subs {
		ext := normalizeSubtitleExt(sub.Ext)
		typ := subtitlePlayerType(ext)
		if typ == "" {
			continue
		}
		label := subtitleLabel(sub, index)
		out = append(out, SubtitleDTO{
			Name:     strings.TrimSpace(sub.Name),
			Label:    label,
			Language: strings.TrimSpace(sub.Language),
			Ext:      ext,
			Type:     typ,
			URL:      fmt.Sprintf("/p/subtitle/%s/%d", pathSegment(videoID), index),
			Source:   strings.TrimSpace(sub.SourceLabel),
		})
	}
	return out
}

func subtitleLabel(sub subtitles.Subtitle, index int) string {
	parts := make([]string, 0, 3)
	if lang := strings.TrimSpace(sub.Language); lang != "" {
		parts = append(parts, lang)
	}
	if name := strings.TrimSpace(sub.Name); name != "" {
		parts = append(parts, name)
	}
	if ext := normalizeSubtitleExt(sub.Ext); ext != "" {
		parts = append(parts, strings.ToUpper(ext))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("字幕 %d", index+1)
	}
	return strings.Join(parts, " · ")
}

func subtitlePlayerType(ext string) string {
	switch normalizeSubtitleExt(ext) {
	case "vtt":
		return "vtt"
	case "srt":
		return "srt"
	case "ass", "ssa":
		return "ass"
	default:
		return ""
	}
}

func subtitleContentType(ext string) string {
	switch subtitlePlayerType(ext) {
	case "vtt":
		return "text/vtt; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}

func normalizeSubtitleExt(ext string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
}

func previewURL(v *catalog.Video) string {
	return previewURLFor(v.ID, v.PreviewUpdatedAt)
}

func previewURLFor(videoID string, updatedAt time.Time) string {
	base := "/p/preview/" + pathSegment(videoID)
	if updatedAt.IsZero() {
		return base
	}
	return base + "?v=" + strconv.FormatInt(updatedAt.UnixMilli(), 10)
}

func thumbnailURL(v *catalog.Video) string {
	return thumbnailURLFor(v.ID, v.ThumbnailURL, v.ThumbnailUpdatedAt)
}

func thumbnailURLFor(videoID, thumbnail string, updatedAt time.Time) string {
	base := strings.TrimSpace(thumbnail)
	if base == "" {
		return ""
	}
	if thumbnailURLMatchesVideoID(base, videoID) {
		base = "/p/thumb/" + pathSegment(videoID)
	}
	if !strings.HasPrefix(base, "/p/thumb/") || updatedAt.IsZero() {
		return base
	}
	return base + "?v=" + strconv.FormatInt(updatedAt.UnixMilli(), 10)
}

// transcodedSource 在视频有就绪的浏览器兼容性转码产物时返回产物的播放地址。
// 产物和原始文件在同一个 drive 上，走同一条 /p/stream 代理/302 链路。
func transcodedSource(v *catalog.Video) (string, bool) {
	if v.TranscodeStatus == "ready" && v.TranscodedFileID != "" && v.DriveID != localUploadDriveID {
		return fmt.Sprintf("/p/stream/%s/%s", pathSegment(v.DriveID), pathSegment(v.TranscodedFileID)), true
	}
	return "", false
}

func (s *Server) videoSource(v *catalog.Video) string {
	src, _ := s.videoSourceAndSize(v)
	return src
}

// playbackMediaType describes the resource selected by videoSource. A ready
// transcode is always an MP4 even when the original catalog entry was MKV/AVI.
func playbackMediaType(v *catalog.Video) string {
	if v == nil {
		return ""
	}
	if v.TranscodeStatus == "ready" && v.TranscodedFileID != "" {
		return "video/mp4"
	}
	switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v.Ext), ".")) {
	case "mp4", "m4v":
		return "video/mp4"
	default:
		return ""
	}
}

// videoSourceAndSize 返回实际播放资源的地址和同一资源的字节数。转码就绪时
// 播放的是转码产物，因此不能继续沿用原文件大小来估算码率。
func (s *Server) videoSourceAndSize(v *catalog.Video) (string, int64) {
	if v.DriveID == localUploadDriveID {
		return "/p/upload/" + pathSegment(v.ID), v.Size
	}
	if driveID, fileID, ok := videoStreamTarget(v); ok {
		size := v.Size
		if v.TranscodeStatus == "ready" && fileID == v.TranscodedFileID && v.TranscodedFileID != "" {
			size = v.TranscodedSize
		}
		return fmt.Sprintf("/p/stream/%s/%s", pathSegment(driveID), pathSegment(fileID)), size
	}
	return fmt.Sprintf("/p/stream/%s/%s", pathSegment(v.DriveID), pathSegment(v.FileID)), v.Size
}

// videoStreamTarget returns the exact drive/file pair used by the browser-facing
// /p/stream URL. Shorts link prewarming must use the transcoded file when present,
// otherwise it would warm a different cache entry from the one playback requests.
func videoStreamTarget(v *catalog.Video) (driveID, fileID string, ok bool) {
	if v == nil || v.DriveID == localUploadDriveID || v.DriveID == "" {
		return "", "", false
	}
	fileID = v.FileID
	if v.TranscodeStatus == "ready" && v.TranscodedFileID != "" {
		fileID = v.TranscodedFileID
	}
	if fileID == "" {
		return "", "", false
	}
	return v.DriveID, fileID, true
}

// videoSource 兼容旧调用点，没有 server context 时按之前逻辑回退到 /p/stream。
// 内部新增的代码请使用 (*Server).videoSource。
func videoSource(v *catalog.Video) string {
	if v.DriveID == localUploadDriveID {
		return "/p/upload/" + pathSegment(v.ID)
	}
	if src, ok := transcodedSource(v); ok {
		return src
	}
	return fmt.Sprintf("/p/stream/%s/%s", pathSegment(v.DriveID), pathSegment(v.FileID))
}

func pathSegment(value string) string {
	return url.PathEscape(value)
}

func routeParam(r *http.Request, key string) string {
	value := chi.URLParam(r, key)
	if value == "" {
		return ""
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

func routeWildcardParam(r *http.Request, key string) string {
	value := chi.URLParam(r, key)
	if value == "" {
		return ""
	}
	value = strings.TrimPrefix(value, "/")
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

func thumbnailURLMatchesVideoID(value, videoID string) bool {
	if !strings.HasPrefix(value, "/p/thumb/") {
		return false
	}
	tail := strings.TrimPrefix(value, "/p/thumb/")
	if idx := strings.IndexByte(tail, '?'); idx >= 0 {
		tail = tail[:idx]
	}
	if tail == videoID {
		return true
	}
	decoded, err := url.PathUnescape(tail)
	return err == nil && decoded == videoID
}

func driveKindLabel(kind string) string {
	switch kind {
	case "quark":
		return "夸克网盘"
	case "p115":
		return "115 网盘"
	case "p123":
		return "123网盘"
	case "pikpak":
		return "PikPak"
	case "wopan":
		return "联通网盘"
	case "guangyapan":
		return "光鸭网盘"
	case "onedrive":
		return "OneDrive"
	case "googledrive":
		return "Google Drive"
	case "webdav":
		return "WebDAV"
	case localstorage.Kind:
		return "本地存储"
	default:
		return kind
	}
}

func (s *Server) localUploadFilePath(fileID string) (string, error) {
	if strings.TrimSpace(fileID) == "" || filepath.Base(fileID) != fileID {
		return "", errors.New("invalid upload file id")
	}
	root := s.localUploadDir()
	if root == "" {
		return "", errors.New("local upload storage is not configured")
	}
	path := filepath.Join(root, fileID)
	cleanPath, ok := localpath.Within(root, path)
	if !ok {
		return "", errors.New("invalid upload file id")
	}
	return cleanPath, nil
}

func (s *Server) localUploadDir() string {
	if s.UploadDir != "" {
		return s.UploadDir
	}
	if s.LocalDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.LocalDir), "uploads")
}

func uploadTagValues(r *http.Request) []string {
	if r.MultipartForm == nil {
		return nil
	}
	values := append([]string{}, r.MultipartForm.Value["tags"]...)
	values = append(values, r.MultipartForm.Value["tag"]...)
	return values
}

func uploadTitleFromFileName(fileName string) string {
	name := strings.TrimSpace(filepath.Base(fileName))
	ext := filepath.Ext(name)
	if ext != "" {
		if trimmed := strings.TrimSuffix(name, ext); strings.TrimSpace(trimmed) != "" {
			return trimmed
		}
	}
	if name != "" {
		return name
	}
	return "upload-" + time.Now().Format("20060102150405")
}

func parseUploadTags(values []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, label := range splitUploadTags(value) {
			if _, ok := seen[label]; ok {
				continue
			}
			seen[label] = struct{}{}
			out = append(out, label)
		}
	}
	return out
}

func (s *Server) canonicalUploadTags(ctx context.Context, values []string) ([]string, error) {
	tags := parseUploadTags(values)
	if len(tags) == 0 {
		return tags, nil
	}
	if s.Catalog == nil {
		return nil, errors.New("catalog is not configured")
	}
	canonicalTags := make([]string, 0, len(tags))
	for _, tag := range tags {
		label, ok, err := s.Catalog.LookupUserSelectableTagLabel(ctx, tag)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, uploadTagValidationError{
				fmt.Errorf("unknown upload tag: %s", tag),
			}
		}
		canonicalTags = append(canonicalTags, label)
	}
	return canonicalTags, nil
}

type uploadTagValidationError struct {
	error
}

func isUploadTagValidationError(err error) bool {
	var target uploadTagValidationError
	return errors.As(err, &target)
}

func splitUploadTags(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ',', '，', ';', '；', '\n', '\r', '\t', ' ':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if label := strings.TrimSpace(field); label != "" {
			out = append(out, label)
		}
	}
	return out
}

func newUploadID(now time.Time) (string, error) {
	var suffix [6]byte
	if _, err := crand.Read(suffix[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("upload-%d-%s", now.UnixNano(), hex.EncodeToString(suffix[:])), nil
}

func mapVideos(vs []*catalog.Video) []VideoDTO {
	out := make([]VideoDTO, 0, len(vs))
	for _, v := range vs {
		out = append(out, mapVideo(v))
	}
	return out
}

func mapVideoSummaries(videos []*catalog.VideoSummary) []VideoCardDTO {
	out := make([]VideoCardDTO, 0, len(videos))
	for _, video := range videos {
		badges := video.Badges
		if badges == nil {
			badges = []string{}
		}
		out = append(out, VideoCardDTO{
			ID:              video.ID,
			Href:            "/video/" + pathSegment(video.ID),
			Title:           video.Title,
			Thumbnail:       thumbnailURLFor(video.ID, video.ThumbnailURL, video.ThumbnailUpdatedAt),
			PreviewSrc:      previewURLFor(video.ID, video.PreviewUpdatedAt),
			PreviewDuration: 12,
			PreviewStrategy: "teaser-file",
			Duration:        formatDuration(video.DurationSeconds),
			Badges:          badges,
			Author:          video.Author,
			Views:           video.Views,
			PublishedAt:     video.PublishedAt.Format("2006-01-02"),
		})
	}
	return out
}

func mapVideoCollectionItems(videos []*catalog.Video, includePreview bool) []VideoCollectionItemDTO {
	items := make([]VideoCollectionItemDTO, 0, len(videos))
	for _, video := range videos {
		if video == nil {
			continue
		}
		item := VideoCollectionItemDTO{
			ID:          video.ID,
			Href:        "/video/" + pathSegment(video.ID),
			Title:       video.Title,
			Thumbnail:   thumbnailURL(video),
			Duration:    formatDuration(video.DurationSeconds),
			Views:       video.Views,
			PublishedAt: video.PublishedAt.Format("2006-01-02"),
		}
		if includePreview {
			item.PreviewSrc = previewURL(video)
		}
		items = append(items, item)
	}
	return items
}

func formatDuration(sec int) string {
	if sec <= 0 {
		return "00:00"
	}
	m := sec / 60
	s := sec % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
