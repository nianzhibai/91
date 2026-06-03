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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/localstorage"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/drives/spider91"
	"github.com/video-site/backend/internal/proxy"
)

const localUploadDriveID = localupload.DriveID

var allowedUploadExtensions = map[string]struct{}{
	".avi":  {},
	".mkv":  {},
	".mov":  {},
	".mp4":  {},
	".webm": {},
}

var allowedUploadTags = map[string]struct{}{
	"奶子": {},
	"臀":  {},
	"口交": {},
	"女大": {},
	"人妻": {},
	"AV": {},
}

type Server struct {
	Catalog         *catalog.Catalog
	Proxy           *proxy.Proxy
	LocalDir        string
	UploadDir       string
	OnVideoUploaded func(*catalog.Video)

	tagCacheMu    sync.Mutex
	tagCacheUntil time.Time
	tagCache      []TagDTO

	// GetTheme 返回当前生效的主题（"dark" | "pink"）。前台 /api/settings/theme 用，
	// 不需要登录。无注入时返回 "dark"。
	GetTheme func() string
}

const (
	homePageSize = 12
)

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
	Quality         string   `json:"quality,omitempty"`
	SourceLabel     string   `json:"sourceLabel,omitempty"`
	Author          string   `json:"author"`
	Views           int      `json:"views"`
	Favorites       int      `json:"favorites"`
	Comments        int      `json:"comments"`
	Likes           int      `json:"likes"`
	Dislikes        int      `json:"dislikes"`
	PublishedAt     string   `json:"publishedAt"`
	Tags            []string `json:"tags,omitempty"`
	Category        string   `json:"category,omitempty"`
}

type TagDTO struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type VideoDetailDTO struct {
	VideoDTO
	VideoSrc      string        `json:"videoSrc"`
	Poster        string        `json:"poster"`
	Description   string        `json:"description"`
	EmbedURL      string        `json:"embedUrl"`
	Points        int           `json:"points,omitempty"`
	AuthorProfile AuthorProfile `json:"authorProfile"`
	RelatedVideos []VideoDTO    `json:"relatedVideos"`
	CommentsList  []Comment     `json:"commentsList"`
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

	r.Group(func(r chi.Router) {
		r.Use(a.Required)
		r.Get("/api/home", s.handleHome)
		r.Get("/api/list", s.handleList)
		r.Get("/api/video/{id}", s.handleVideoDetail)
		r.Put("/api/video/{id}/tags", s.handleUpdateVideoTags)
		r.Post("/api/video/{id}/like", s.handleLike)
		r.Delete("/api/video/{id}/like", s.handleUnlike)
		r.Post("/api/video/{id}/view", s.handleView)
		r.Post("/api/video/{id}/hide", s.handleHideVideo)
		r.Post("/api/upload", s.handleUploadVideo)
		r.Get("/api/tags", s.handleTags)
		r.Post("/api/shorts/next", s.handleShortsNext)

		// 代理路由同样需要鉴权，防止绕过
		r.Get("/p/stream/{driveID}/{fileID}", s.handleStream)
		r.Get("/p/upload/{videoID}", s.handleUploadedVideo)
		r.Get("/p/spider91/{videoID}", s.handleSpider91Video)
		r.Get("/p/preview/{videoID}", s.handlePreview)
		r.Get("/p/thumb/{videoID}", s.handleThumb)
	})
}

// handleGetTheme 返回当前生效的主题。无需登录。响应永远是
// {"theme": "dark"} 或 {"theme": "pink"}，便于前端无脑解析。
func (s *Server) handleGetTheme(w http.ResponseWriter, r *http.Request) {
	theme := "dark"
	if s.GetTheme != nil {
		if v := s.GetTheme(); v == "pink" || v == "dark" {
			theme = v
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"theme": theme})
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	// 首页优先从全量已有封面的视频里随机抽取，避免只在最近一小段候选中反复出现。
	excludeIDs := parseVideoIDQuery(r, "exclude", 120)
	items, err := s.Catalog.RandomVideosWithReadyThumbnailsExcluding(r.Context(), excludeIDs, homePageSize)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(items) < homePageSize {
		fallbackExclude := append([]string{}, excludeIDs...)
		for _, item := range items {
			if item != nil {
				fallbackExclude = append(fallbackExclude, item.ID)
			}
		}
		fallback, err := s.Catalog.RandomVideosExcluding(r.Context(), fallbackExclude, homePageSize-len(items))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		items = appendUniqueVideos(items, fallback, homePageSize)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, mapVideos(items))
}

func parseVideoIDQuery(r *http.Request, key string, limit int) []string {
	if r == nil {
		return nil
	}
	values := r.URL.Query()[key]
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, id := range strings.Split(value, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func appendUniqueVideos(dst []*catalog.Video, candidates []*catalog.Video, limit int) []*catalog.Video {
	if len(dst) >= limit {
		return dst[:limit]
	}
	seen := make(map[string]struct{}, len(dst))
	for _, v := range dst {
		if v != nil {
			seen[v.ID] = struct{}{}
		}
	}
	for _, v := range candidates {
		if v == nil {
			continue
		}
		if _, ok := seen[v.ID]; ok {
			continue
		}
		dst = append(dst, v)
		seen[v.ID] = struct{}{}
		if len(dst) >= limit {
			return dst
		}
	}
	return dst
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = 24
	}
	sort := q.Get("sort")
	params := catalog.ListParams{
		Keyword:      q.Get("q"),
		Tag:          q.Get("tag"),
		Category:     q.Get("cat"),
		Sort:         sort,
		Page:         page,
		PageSize:     size,
		FrontendOnly: true,
		SkipTotal:    strings.EqualFold(q.Get("count"), "false"),
	}
	if sort == "" || sort == "latest" {
		params.PreferReadyThumbnails = true
	}
	items, total, err := s.Catalog.ListVideos(r.Context(), params)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": mapVideos(items),
		"total": total,
		"page":  params.Page,
		"size":  params.PageSize,
	})
}

func (s *Server) handleVideoDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	v, err := s.Catalog.GetVideo(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	accessible, err := s.Catalog.IsFrontendAccessible(r.Context(), id)
	if err != nil || !accessible {
		writeErr(w, http.StatusNotFound, sql.ErrNoRows)
		return
	}
	if v.DriveID != localUploadDriveID {
		if _, err := s.Catalog.GetDrive(r.Context(), v.DriveID); err != nil {
			drives, listErr := s.Catalog.ListDrives(r.Context())
			if listErr != nil || len(drives) > 0 {
				writeErr(w, http.StatusNotFound, sql.ErrNoRows)
				return
			}
		}
	}
	related := s.pickRelatedVideos(r.Context(), v, 6)
	dto := mapVideo(v)
	if d, err := s.Catalog.GetDrive(r.Context(), v.DriveID); err == nil {
		dto.SourceLabel = driveKindLabel(d.Kind)
	}

	detail := VideoDetailDTO{
		VideoDTO:    dto,
		VideoSrc:    s.videoSource(v),
		Poster:      thumbnailURL(v),
		Description: v.Description,
		EmbedURL:    fmt.Sprintf(`<iframe src="/embed/%s" width="640" height="360" frameborder="0" allowfullscreen></iframe>`, v.ID),
		AuthorProfile: AuthorProfile{
			ID:     "author-" + v.Author,
			Name:   v.Author,
			Href:   "/author/" + v.Author,
			Badges: []string{},
		},
		RelatedVideos: mapVideos(related),
		CommentsList:  []Comment{},
	}
	// 推荐每次随机生成，禁止浏览器和中间层缓存详情响应
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, detail)
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
			FrontendOnly:          true,
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
		FrontendOnly:          true,
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
	if s.tagCache != nil && now.Before(s.tagCacheUntil) {
		out := append([]TagDTO(nil), s.tagCache...)
		s.tagCacheMu.Unlock()
		w.Header().Set("Cache-Control", "private, max-age=15")
		writeJSON(w, http.StatusOK, out)
		return
	}
	s.tagCacheMu.Unlock()

	stats, err := s.Catalog.ListFrontendTags(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]TagDTO, 0, len(stats))
	for _, stat := range stats {
		out = append(out, TagDTO{ID: stat.Label, Label: stat.Label, Count: stat.Count})
	}
	s.tagCacheMu.Lock()
	s.tagCache = append([]TagDTO(nil), out...)
	s.tagCacheUntil = now.Add(30 * time.Second)
	s.tagCacheMu.Unlock()

	w.Header().Set("Cache-Control", "private, max-age=15")
	writeJSON(w, http.StatusOK, out)
}

// shortsNextReq 客户端把当前轮已看过的 video id 列表传上来。
// PreferredFromVideoID 来自短视频页最近一次点赞成功的视频，用于优先推荐相似标签。
type shortsNextReq struct {
	SeenIDs              []string `json:"seenIds"`
	Count                int      `json:"count"`
	PreferredFromVideoID string   `json:"preferredFromVideoId"`
}

// ShortsItemDTO 是短视频流单条的精简结构。比 VideoDTO 多 videoSrc / poster，
// 方便前端直接喂给 <video>，不必再为每条请求 /api/video/:id。
type ShortsItemDTO struct {
	VideoDTO
	VideoSrc string `json:"videoSrc"`
	Poster   string `json:"poster"`
}

// handleShortsNext 为短视频模式提供"不重复随机视频"接口。
//
// 行为：
//   - 入参 seenIds 为客户端当前轮已看过的视频 id（来自 localStorage）
//   - 服务器从未在 seenIds 中的可见视频里随机抽至多 count 条返回
//   - 当返回数量 < count 且小于全库可见总数时，说明本轮即将结束，
//     返回 roundComplete=true，前端应在用户看完返回的这些后清空本地已看记录开新一轮
//   - 当 seenIds 已经覆盖全库时，本接口直接返回新一轮的随机一批
//     （传 seenIds=[] 即可让客户端在轮次完成后重新开始）
func (s *Server) handleShortsNext(w http.ResponseWriter, r *http.Request) {
	var body shortsNextReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	count := body.Count
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}

	total, err := s.Catalog.CountVisibleVideos(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 如果客户端已看记录已经 ≥ 全库，则视为新一轮，直接忽略 seenIds
	exclude := body.SeenIDs
	if total > 0 && len(exclude) >= total {
		exclude = nil
	}

	var items []*catalog.Video
	if strings.TrimSpace(body.PreferredFromVideoID) != "" {
		items, err = s.Catalog.RandomVideosForPreferredVideoExcluding(r.Context(), body.PreferredFromVideoID, exclude, count)
	} else {
		items, err = s.Catalog.RandomVideosExcluding(r.Context(), exclude, count)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 注入 sourceLabel 以便前端展示来源网盘
	driveLabels := make(map[string]string)
	out := make([]ShortsItemDTO, 0, len(items))
	for _, v := range items {
		dto := mapVideo(v)
		if label, ok := driveLabels[v.DriveID]; ok {
			dto.SourceLabel = label
		} else if d, err := s.Catalog.GetDrive(r.Context(), v.DriveID); err == nil {
			label := driveKindLabel(d.Kind)
			driveLabels[v.DriveID] = label
			dto.SourceLabel = label
		}
		out = append(out, ShortsItemDTO{
			VideoDTO: dto,
			VideoSrc: s.videoSource(v),
			Poster:   thumbnailURL(v),
		})
	}

	// roundComplete: 服务端能给出的视频数小于 count，说明剩余可选已耗尽，
	// 前端把这批播完后应该清空本地 seenIds 开新一轮。
	roundComplete := len(out) < count

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"items":         out,
		"total":         total,
		"roundComplete": roundComplete,
	})
}

type updateVideoTagsReq struct {
	Tags []string `json:"tags"`
}

func (s *Server) handleUpdateVideoTags(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
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
	id := chi.URLParam(r, "id")
	likes, err := s.Catalog.IncrementLike(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"likes": likes})
}

// handleUnlike 取消点赞：likes - 1（保底 0）。
// 短视频模式中爱心按钮点击切换状态时使用。
func (s *Server) handleUnlike(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
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
	id := chi.URLParam(r, "id")
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
	id := chi.URLParam(r, "id")
	if err := s.Catalog.HideVideo(r.Context(), id); err != nil {
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

	tags, err := parseUploadTags(uploadTagValues(r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	now := time.Now()
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = uploadTitleFromFileName(originalName)
	}

	uploadID, err := newUploadID(now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	storedName := uploadID + ext
	dst, err := s.localUploadFilePath(storedName)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	size, copyErr := io.Copy(out, file)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		writeErr(w, http.StatusInternalServerError, copyErr)
		return
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		writeErr(w, http.StatusInternalServerError, closeErr)
		return
	}
	if size <= 0 {
		_ = os.Remove(dst)
		writeErr(w, http.StatusBadRequest, errors.New("uploaded video is empty"))
		return
	}

	video := &catalog.Video{
		ID:            localUploadDriveID + "-" + uploadID,
		DriveID:       localUploadDriveID,
		FileID:        storedName,
		FileName:      originalName,
		Title:         title,
		Author:        "用户上传",
		Tags:          tags,
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
	if s.OnVideoUploaded != nil {
		s.OnVideoUploaded(video)
	}
	writeJSON(w, http.StatusCreated, mapVideo(video))
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	driveID := chi.URLParam(r, "driveID")
	fileID := chi.URLParam(r, "fileID")
	s.Proxy.ServeStream(w, r, driveID, fileID)
}
func (s *Server) handleUploadedVideo(w http.ResponseWriter, r *http.Request) {
	videoID := chi.URLParam(r, "videoID")
	v, err := s.Catalog.GetVideo(r.Context(), videoID)
	if err != nil || v.Hidden || v.DriveID != localUploadDriveID {
		http.NotFound(w, r)
		return
	}
	path, err := s.localUploadFilePath(v.FileID)
	if err != nil {
		http.Error(w, "invalid upload file", http.StatusForbidden)
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeFile(w, r, path)
}

// handleSpider91Video 服务 spider91 drive 下载到本地的视频文件。
// 路径形如 /p/spider91/<videoID>，videoID = "spider91-<driveID>-<sourceID>"。
// 通过 catalog 拿到 file_id（"<sourceID>.mp4"），再让 driver 解析到绝对路径并 ServeFile。
func (s *Server) handleSpider91Video(w http.ResponseWriter, r *http.Request) {
	videoID := chi.URLParam(r, "videoID")
	v, err := s.Catalog.GetVideo(r.Context(), videoID)
	if err != nil || v.Hidden {
		http.NotFound(w, r)
		return
	}
	if s.Proxy == nil || s.Proxy.Registry == nil {
		http.NotFound(w, r)
		return
	}
	d, ok := s.Proxy.Registry.Get(v.DriveID)
	if !ok || d.Kind() != spider91.Kind {
		http.NotFound(w, r)
		return
	}
	sd, ok := d.(*spider91.Driver)
	if !ok {
		http.NotFound(w, r)
		return
	}
	path, err := sd.VideoPath(v.FileID)
	if err != nil {
		http.Error(w, "invalid video id", http.StatusForbidden)
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeFile(w, r, path)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	videoID := chi.URLParam(r, "videoID")
	v, err := s.Catalog.GetVideo(r.Context(), videoID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if v.PreviewStatus != "ready" {
		http.Error(w, "preview not ready", http.StatusNotFound)
		return
	}
	if v.PreviewLocal != "" {
		if !strings.HasPrefix(filepath.Clean(v.PreviewLocal), filepath.Clean(s.LocalDir)) {
			http.Error(w, "invalid local path", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		s.Proxy.ServeLocal(w, r, v.PreviewLocal)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	videoID := chi.URLParam(r, "videoID")
	// 直接读本地 thumbs 目录中 <videoID>.jpg
	path := filepath.Join(s.LocalDir, "thumbs", videoID+".jpg")
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(s.LocalDir)) {
		http.Error(w, "invalid path", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(clean); err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	s.Proxy.ServeLocal(w, r, clean)
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
		Href:            "/video/" + v.ID,
		Title:           v.Title,
		Thumbnail:       thumbnailURL(v),
		PreviewSrc:      previewURL(v),
		PreviewDuration: 12,
		PreviewStrategy: "teaser-file",
		Duration:        formatDuration(v.DurationSeconds),
		Badges:          badges,
		Quality:         v.Quality,
		Author:          v.Author,
		Views:           v.Views,
		Favorites:       v.Favorites,
		Comments:        v.Comments,
		Likes:           v.Likes,
		Dislikes:        v.Dislikes,
		PublishedAt:     v.PublishedAt.Format("2006-01-02"),
		Tags:            tags,
		Category:        v.Category,
	}
}

func previewURL(v *catalog.Video) string {
	base := "/p/preview/" + v.ID
	if v.UpdatedAt.IsZero() {
		return base
	}
	return base + "?v=" + strconv.FormatInt(v.UpdatedAt.UnixMilli(), 10)
}

func thumbnailURL(v *catalog.Video) string {
	base := "/p/thumb/" + v.ID
	if v.ThumbnailURL != "" {
		base = v.ThumbnailURL
	}
	if !strings.HasPrefix(base, "/p/thumb/") || v.UpdatedAt.IsZero() {
		return base
	}
	return base + "?v=" + strconv.FormatInt(v.UpdatedAt.UnixMilli(), 10)
}

func (s *Server) videoSource(v *catalog.Video) string {
	if v.DriveID == localUploadDriveID {
		return "/p/upload/" + v.ID
	}
	if s.Proxy != nil && s.Proxy.Registry != nil {
		if d, ok := s.Proxy.Registry.Get(v.DriveID); ok && d.Kind() == spider91.Kind {
			return "/p/spider91/" + v.ID
		}
	}
	return fmt.Sprintf("/p/stream/%s/%s", v.DriveID, v.FileID)
}

// videoSource 兼容旧调用点，没有 server context 时按之前逻辑回退到 /p/stream。
// 内部新增的代码请使用 (*Server).videoSource。
func videoSource(v *catalog.Video) string {
	if v.DriveID == localUploadDriveID {
		return "/p/upload/" + v.ID
	}
	return fmt.Sprintf("/p/stream/%s/%s", v.DriveID, v.FileID)
}

func driveKindLabel(kind string) string {
	switch kind {
	case "quark":
		return "夸克网盘"
	case "p115":
		return "115 网盘"
	case "p123":
		return "123 云盘"
	case "pikpak":
		return "PikPak"
	case "wopan":
		return "联通沃盘"
	case "onedrive":
		return "OneDrive"
	case "googledrive":
		return "Google Drive"
	case localstorage.Kind:
		return "本地存储"
	case spider91.Kind:
		return "91 爬虫"
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
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if cleanPath != cleanRoot && !strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator)) {
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

func parseUploadTags(values []string) ([]string, error) {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, label := range splitUploadTags(value) {
			if _, ok := allowedUploadTags[label]; !ok {
				return nil, fmt.Errorf("unsupported upload tag: %s", label)
			}
			if _, ok := seen[label]; ok {
				continue
			}
			seen[label] = struct{}{}
			out = append(out, label)
		}
	}
	return out, nil
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
