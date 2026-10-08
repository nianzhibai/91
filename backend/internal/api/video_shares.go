package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/localpath"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/subtitles"
)

const (
	videoShareCookieName = "vs_video_share"
	videoShareSessionTTL = 6 * time.Hour
	videoShareTokenBytes = 32
	videoShareIDBytes    = 16
)

type createVideoShareResponse struct {
	URL string `json:"url"`
}

type consumeVideoShareResponse struct {
	ShareID   string         `json:"shareId"`
	ExpiresAt string         `json:"expiresAt"`
	Video     VideoDetailDTO `json:"video"`
}

type consumeVideoShareRequest struct {
	Token string `json:"token"`
}

func (s *Server) handleCreateVideoShare(w http.ResponseWriter, r *http.Request) {
	videoID := routeParam(r, "id")
	v, err := s.availableVideo(r.Context(), videoID)
	if err != nil {
		writeCatalogLookupError(w, r, err)
		return
	}

	shareID, _, err := newOpaqueHex(videoShareIDBytes)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	token, tokenBytes, err := newOpaqueHex(videoShareTokenBytes)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := s.Catalog.CreateVideoShare(
		r.Context(),
		shareID,
		digestShareSecret(tokenBytes),
		v.ID,
		s.shareCurrentTime(),
	); err != nil {
		if errors.Is(err, catalog.ErrVideoShareUnavailable) {
			writeErr(w, r, http.StatusNotFound, sql.ErrNoRows)
			return
		}
		writeServiceUnavailable(w, r, "create video share", err)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, createVideoShareResponse{
		// URL fragments never reach the HTTP server or its access logs. The SPA
		// reads the token and submits it in the one-time POST body instead.
		URL: "/share#" + token,
	})
}

func (s *Server) handleConsumeVideoShare(w http.ResponseWriter, r *http.Request) {
	var body consumeVideoShareRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1024))
	if err := decoder.Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid share request"))
		return
	}
	tokenHash, ok := digestHexShareSecret(body.Token, videoShareTokenBytes)
	if !ok {
		writeErr(w, r, http.StatusNotFound, catalog.ErrVideoShareUnavailable)
		return
	}

	sessionValue, sessionHash, hasSession := videoShareSessionFromRequest(r)
	if !hasSession {
		var sessionBytes []byte
		var err error
		sessionValue, sessionBytes, err = newOpaqueHex(videoShareTokenBytes)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		sessionHash = digestShareSecret(sessionBytes)
	}

	now := s.shareCurrentTime()
	share, _, err := s.Catalog.ClaimVideoShare(
		r.Context(),
		tokenHash,
		sessionHash,
		now,
		now.Add(videoShareSessionTTL),
	)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrVideoShareConsumed):
			writeErr(w, r, http.StatusGone, err)
		case errors.Is(err, catalog.ErrVideoShareUnavailable):
			writeErr(w, r, http.StatusNotFound, err)
		default:
			writeServiceUnavailable(w, r, "claim video share", err)
		}
		return
	}

	v, err := s.availableVideo(r.Context(), share.VideoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, catalog.ErrVideoShareUnavailable)
		} else {
			writeServiceUnavailable(w, r, "load shared video", err)
		}
		return
	}
	setVideoShareSessionCookie(w, r, sessionValue, share.SessionExpiresAt, now)

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, consumeVideoShareResponse{
		ShareID:   share.ID,
		ExpiresAt: share.SessionExpiresAt.UTC().Format(time.RFC3339),
		Video:     s.mapSharedVideoDetail(r.Context(), v, share.ID),
	})
}

func (s *Server) handleSharedVideoSubtitles(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	subs, err := s.loadVideoSubtitles(r.Context(), v)
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, mapSharedSubtitles(routeParam(r, "shareID"), subs))
}

func (s *Server) handleSharedVideoView(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	views, err := s.Catalog.IncrementView(r.Context(), v.ID)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"views": views})
}

func (s *Server) handleSharedVideoStream(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	if v.DriveID == localUploadDriveID {
		s.serveUploadedVideo(w, r, v)
		return
	}
	if s.Proxy == nil {
		http.NotFound(w, r)
		return
	}
	s.Proxy.ServeStream(w, r, v.DriveID, v.FileID)
}

func (s *Server) handleSharedVideoPreview(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	s.servePreviewVideo(w, r, v)
}

func (s *Server) handleSharedVideoThumb(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	s.serveVideoThumb(w, r, v.ID)
}

func (s *Server) handleSharedSubtitleFile(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	index, err := strconv.Atoi(routeParam(r, "index"))
	if err != nil || index < 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid subtitle index"))
		return
	}
	s.serveSubtitleSelection(w, r, v, index)
}

func (s *Server) activeSharedVideo(w http.ResponseWriter, r *http.Request) (*catalog.Video, bool) {
	_, sessionHash, ok := videoShareSessionFromRequest(r)
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	videoID, err := s.Catalog.ActiveVideoShare(
		r.Context(),
		routeParam(r, "shareID"),
		sessionHash,
		s.shareCurrentTime(),
	)
	if err != nil {
		if errors.Is(err, catalog.ErrVideoShareUnavailable) {
			http.NotFound(w, r)
		} else {
			writeServiceUnavailable(w, r, "validate video share", err)
		}
		return nil, false
	}
	v, err := s.availableVideo(r.Context(), videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			writeServiceUnavailable(w, r, "load shared video", err)
		}
		return nil, false
	}
	return v, true
}

func (s *Server) availableVideo(ctx context.Context, id string) (*catalog.Video, error) {
	if s.Catalog == nil {
		return nil, sql.ErrNoRows
	}
	v, err := s.videoByPublicID(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.Hidden {
		return nil, sql.ErrNoRows
	}
	if v.DriveID == localUploadDriveID || v.DriveID == catalog.TelegramLocalDriveID {
		return v, nil
	}
	if _, err := s.Catalog.GetDrive(ctx, v.DriveID); err == nil {
		return v, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Preserve the catalog-only development/test mode used by the existing API:
	// when no drives have been configured at all, metadata remains readable.
	drives, err := s.Catalog.ListDrives(ctx)
	if err != nil {
		return nil, err
	}
	if len(drives) > 0 {
		return nil, sql.ErrNoRows
	}
	return v, nil
}

func (s *Server) videoByPublicID(ctx context.Context, id string) (*catalog.Video, error) {
	if s.Catalog == nil {
		return nil, sql.ErrNoRows
	}
	resolvedID, err := s.Catalog.ResolveVideoID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.Catalog.GetVideo(ctx, resolvedID)
}

func (s *Server) mapSharedVideoDetail(ctx context.Context, v *catalog.Video, shareID string) VideoDetailDTO {
	dto := mapVideo(v)
	dto.Href = ""
	dto.Thumbnail = s.sharedThumbnailURL(v, shareID)
	dto.PreviewSrc = sharedAssetURL(shareID, "preview", v.PreviewUpdatedAt)
	dto.SourceLabel = s.videoSourceLabel(ctx, v)
	return VideoDetailDTO{
		VideoDTO: dto,
		VideoSrc: sharedAssetURL(shareID, "stream", time.Time{}),
		Poster:   dto.Thumbnail,
		AuthorProfile: AuthorProfile{
			ID:     "author-" + v.Author,
			Name:   v.Author,
			Badges: []string{},
		},
		CommentsList: []Comment{},
	}
}

func (s *Server) sharedThumbnailURL(v *catalog.Video, shareID string) string {
	thumbnail := thumbnailURL(v)
	if strings.HasPrefix(thumbnail, "/p/thumb/") {
		return sharedAssetURL(shareID, "thumb", v.ThumbnailUpdatedAt)
	}
	return thumbnail
}

func sharedAssetURL(shareID, asset string, updatedAt time.Time) string {
	base := fmt.Sprintf("/p/share/%s/%s", pathSegment(shareID), asset)
	if updatedAt.IsZero() {
		return base
	}
	return base + "?v=" + strconv.FormatInt(updatedAt.UnixMilli(), 10)
}

func mapSharedSubtitles(shareID string, subs []subtitles.Subtitle) []SubtitleDTO {
	out := mapSubtitles("", subs)
	for index := range out {
		out[index].URL = fmt.Sprintf(
			"/p/share/%s/subtitle/%d",
			pathSegment(shareID),
			index,
		)
	}
	return out
}

func (s *Server) serveUploadedVideo(w http.ResponseWriter, r *http.Request, v *catalog.Video) {
	if v == nil || v.Hidden || v.DriveID != localUploadDriveID {
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

func (s *Server) servePreviewVideo(w http.ResponseWriter, r *http.Request, v *catalog.Video) {
	if v == nil || v.Hidden || v.PreviewStatus != "ready" {
		http.Error(w, "preview not ready", http.StatusNotFound)
		return
	}
	if v.PreviewLocal != "" {
		localPreview, ok := localpath.Managed(s.LocalDir, v.PreviewLocal)
		if !ok {
			http.Error(w, "invalid local path", http.StatusForbidden)
			return
		}
		expectedVersion := ""
		if !v.PreviewUpdatedAt.IsZero() {
			expectedVersion = strconv.FormatInt(v.PreviewUpdatedAt.UnixMilli(), 10)
		}
		if expectedVersion != "" && r.URL.Query().Get("v") == expectedVersion {
			w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		s.Proxy.ServeLocal(w, r, localPreview)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) serveVideoThumb(w http.ResponseWriter, r *http.Request, videoID string) {
	var clean string
	for _, path := range mediaasset.ThumbnailPathCandidates(s.LocalDir, videoID) {
		candidate, ok := localpath.Within(s.LocalDir, path)
		if !ok {
			http.Error(w, "invalid path", http.StatusForbidden)
			return
		}
		if _, err := os.Stat(candidate); err == nil {
			clean = candidate
			break
		}
	}
	if clean == "" {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("variant") == "shorts-bg" {
		backgroundPath, ok := localpath.Within(
			s.LocalDir,
			mediaasset.ShortsBackgroundPath(s.LocalDir, videoID),
		)
		if !ok {
			http.Error(w, "invalid path", http.StatusForbidden)
			return
		}
		if err := mediaasset.EnsureShortsBackground(clean, backgroundPath); err == nil {
			clean = backgroundPath
		}
	}
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	s.Proxy.ServeLocal(w, r, clean)
}

func (s *Server) serveSubtitleSelection(
	w http.ResponseWriter,
	r *http.Request,
	v *catalog.Video,
	index int,
) {
	var sub subtitles.Subtitle
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		subs, err := s.loadVideoSubtitles(r.Context(), v)
		if err != nil {
			writeErr(w, r, http.StatusBadGateway, err)
			return
		}
		if index >= len(subs) {
			writeErr(w, r, http.StatusNotFound, errors.New("subtitle not found"))
			return
		}
		sub = subs[index]
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, sub.URL, nil)
		if err != nil {
			writeErr(w, r, http.StatusBadGateway, err)
			return
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err = subtitleHTTPClient.Do(req)
		if err != nil {
			writeErr(w, r, http.StatusBadGateway, err)
			return
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		status := resp.StatusCode
		_ = resp.Body.Close()
		if attempt == 0 && (status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusGone) {
			s.invalidateSubtitleCache(v.ID)
			continue
		}
		writeErr(w, r, http.StatusBadGateway, fmt.Errorf("subtitle upstream status=%d", status))
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleBytes+1))
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	if int64(len(data)) > maxSubtitleBytes {
		writeErr(w, r, http.StatusBadGateway, errSubtitleTooLarge)
		return
	}
	data, err = normalizeSubtitleUTF8(data, resp.Header.Get("Content-Type"), maxSubtitleBytes)
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", subtitleContentType(sub.Ext))
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) shareCurrentTime() time.Time {
	if s.shareNow != nil {
		return s.shareNow()
	}
	return time.Now()
}

func newOpaqueHex(byteCount int) (string, []byte, error) {
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(value), value, nil
}

func digestShareSecret(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func digestHexShareSecret(value string, expectedBytes int) (string, bool) {
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != expectedBytes {
		return "", false
	}
	return digestShareSecret(decoded), true
}

func videoShareSessionFromRequest(r *http.Request) (string, string, bool) {
	cookie, err := r.Cookie(videoShareCookieName)
	if err != nil {
		return "", "", false
	}
	hash, ok := digestHexShareSecret(cookie.Value, videoShareTokenBytes)
	if !ok {
		return "", "", false
	}
	return cookie.Value, hash, true
}

func setVideoShareSessionCookie(
	w http.ResponseWriter,
	r *http.Request,
	value string,
	expiresAt time.Time,
	now time.Time,
) {
	maxAge := int(expiresAt.Sub(now).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     videoShareCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   requestUsesHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func requestUsesHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(forwarded, "https")
}
