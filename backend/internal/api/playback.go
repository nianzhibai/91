package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

type PlaybackSourceDTO struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Kind       string     `json:"kind"`
	Protocol   string     `json:"protocol"`
	Status     string     `json:"status"`
	URL        string     `json:"url,omitempty"`
	ResolveURL string     `json:"resolveUrl,omitempty"`
	PrepareURL string     `json:"prepareUrl,omitempty"`
	MediaType  string     `json:"mediaType,omitempty"`
	Message    string     `json:"message,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
}

type PlaybackManifestDTO struct {
	Sources []PlaybackSourceDTO `json:"sources"`
}

func playbackURL(videoID string) string { return "/api/video/" + pathSegment(videoID) + "/playback" }

func (s *Server) handleVideoPlayback(w http.ResponseWriter, r *http.Request) {
	v, err := s.availableVideo(r.Context(), routeParam(r, "id"))
	if err != nil {
		writeCatalogLookupError(w, r, err)
		return
	}
	s.servePlaybackMetadata(w, r, v, s.videoSource(v))
}

func (s *Server) handleSharedVideoPlayback(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	s.servePlaybackMetadata(w, r, v, sharedAssetURL(routeParam(r, "shareID"), "stream", time.Time{}))
}

func (s *Server) servePlaybackMetadata(w http.ResponseWriter, r *http.Request, v *catalog.Video, originalURL string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	selected := r.URL.Query().Get("source")
	prepare := r.Method == http.MethodPost
	if prepare {
		selected = "cloud"
	}
	if selected != "" && selected != "original" && selected != "cloud" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "未知的播放源"})
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	original := PlaybackSourceDTO{ID: "original", Label: "原画播放", Kind: "original", Protocol: "file", Status: "ready", URL: originalURL, MediaType: playbackMediaType(v)}
	manifest := PlaybackManifestDTO{Sources: []PlaybackSourceDTO{original}}
	if s.Proxy == nil || v.DriveID == localUploadDriveID || v.DriveID == catalog.TelegramLocalDriveID {
		writeJSON(w, http.StatusOK, manifest)
		return
	}
	// An unmounted drive can have a known authorization or connection failure.
	// Return it before capability discovery can turn it into an original-only
	// manifest and let the browser's format check hide the actual drive error.
	if err := s.Proxy.DriveInitializationError(v.DriveID); err != nil {
		s.Proxy.WritePlaybackError(w, v.DriveID, err)
		return
	}
	if selected == "original" && refresh {
		s.Proxy.InvalidateStreamLink(v.DriveID, v.FileID, r.Header)
	}
	if !s.Proxy.HasPlaybackProvider(v.DriveID) {
		writeJSON(w, http.StatusOK, manifest)
		return
	}
	basePath, baseRawPath := strings.TrimSuffix(r.URL.Path, "/prepare"), strings.TrimSuffix(r.URL.RawPath, "/prepare")
	resolveURL := &url.URL{Path: basePath, RawPath: baseRawPath, RawQuery: "source=cloud"}
	cloud := PlaybackSourceDTO{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "unknown", ResolveURL: resolveURL.String()}
	if selected != "cloud" {
		// Listing candidates never calls a provider. Original playback can begin
		// even when the optional cloud preview API is slow or unavailable.
		manifest.Sources = append(manifest.Sources, cloud)
		writeJSON(w, http.StatusOK, manifest)
		return
	}
	var sources []drives.PlaybackSource
	var err error
	if prepare {
		sources, err = s.Proxy.PreparePlaybackSources(r.Context(), v.DriveID, v.FileID)
	} else {
		sources, err = s.Proxy.PlaybackSources(r.Context(), v.DriveID, v.FileID, refresh)
	}
	if err != nil {
		s.Proxy.WritePlaybackError(w, v.DriveID, err)
		return
	}
	for _, source := range sources {
		if source.ID == "" || source.ID == "original" {
			continue
		}
		dto := PlaybackSourceDTO{ID: source.ID, Label: source.Label, Kind: source.Kind, Protocol: source.Protocol, Status: source.Status, MediaType: source.MediaType, Message: source.Message, ResolveURL: cloud.ResolveURL}
		if source.Status == "processing" && s.Proxy.HasPlaybackPreparer(v.DriveID) {
			prepareURL := &url.URL{Path: basePath + "/prepare"}
			if baseRawPath != "" {
				prepareURL.RawPath = baseRawPath + "/prepare"
			}
			dto.PrepareURL = prepareURL.String()
		}
		if source.Status == "ready" {
			if source.Relay != nil {
				sessionID, expires, err := s.Proxy.StartHLSPlayback(v.DriveID, v.FileID, source)
				if err == nil {
					prefix := "/p/hls/" + pathSegment(v.ID)
					if shareID := routeParam(r, "shareID"); shareID != "" {
						prefix = "/p/share/" + pathSegment(shareID) + "/hls"
					}
					dto.URL = prefix + "/" + sessionID + "/index.m3u8"
					dto.ExpiresAt = &expires
				} else {
					dto.Status, dto.Message = "unavailable", "网盘暂未提供可用的兼容播放源"
				}
			} else if safePlaybackLink(source.Link) {
				dto.URL = source.Link.URL
				if !source.Link.Expires.IsZero() {
					expires := source.Link.Expires
					dto.ExpiresAt = &expires
				}
			} else {
				dto.Status, dto.Message = "unavailable", "网盘暂未提供浏览器可用的兼容播放源"
			}
		}
		manifest.Sources = append(manifest.Sources, dto)
	}
	if len(manifest.Sources) == 1 {
		cloud.Status, cloud.Message = "unavailable", "网盘暂未提供该视频的兼容播放源"
		manifest.Sources = append(manifest.Sources, cloud)
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *Server) handleVideoHLS(w http.ResponseWriter, r *http.Request) {
	v, err := s.availableVideo(r.Context(), routeParam(r, "id"))
	if err != nil {
		writeCatalogLookupError(w, r, err)
		return
	}
	s.serveVideoHLS(w, r, v)
}

func (s *Server) handleSharedVideoHLS(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	s.serveVideoHLS(w, r, v)
}

func (s *Server) serveVideoHLS(w http.ResponseWriter, r *http.Request, v *catalog.Video) {
	if s.Proxy == nil {
		http.NotFound(w, r)
		return
	}
	s.Proxy.ServeHLS(w, r, v.DriveID, v.FileID, routeParam(r, "sessionID"), routeParam(r, "resource"))
}

func safePlaybackLink(link *drives.StreamLink) bool {
	if link == nil {
		return false
	}
	u, err := url.Parse(link.URL)
	return link.ClientRedirectSafe && len(link.Headers) == 0 && err == nil && u.Host != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || u.Scheme == "http")
}
