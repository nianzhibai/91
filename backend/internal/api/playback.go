package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

type PlaybackSourceDTO struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	Kind          string     `json:"kind"`
	Protocol      string     `json:"protocol"`
	Status        string     `json:"status"`
	URL           string     `json:"url,omitempty"`
	ResolveURL    string     `json:"resolveUrl,omitempty"`
	PrepareURL    string     `json:"prepareUrl,omitempty"`
	MediaType     string     `json:"mediaType,omitempty"`
	Message       string     `json:"message,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	Resolution    string     `json:"resolution,omitempty"`
	Width         int        `json:"width,omitempty"`
	Height        int        `json:"height,omitempty"`
	Bitrate       int        `json:"bitrate,omitempty"`
	Codec         string     `json:"codec,omitempty"`
	URLSource     string     `json:"urlSource,omitempty"`
	SourceInfoURL string     `json:"sourceInfoUrl,omitempty"`
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
	if prepare && selected == "" {
		selected = "cloud"
	}
	if !validPlaybackSourceID(selected) || (prepare && selected == "original") {
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
	basePath, baseRawPath := strings.TrimSuffix(r.URL.Path, "/prepare"), strings.TrimSuffix(r.URL.RawPath, "/prepare")
	if s.Proxy.HasOriginalPlaybackProvider(v.DriveID) {
		playbackID := r.URL.Query().Get("playbackId")
		if playbackID == "" {
			playbackID = uuid.NewString()
		} else if _, err := uuid.Parse(playbackID); err != nil || len(playbackID) != 36 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "无效的播放标识"})
			return
		}
		streamURL, err := url.Parse(originalURL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "无效的播放地址"})
			return
		}
		query := streamURL.Query()
		query.Set("playbackId", playbackID)
		streamURL.RawQuery = query.Encode()
		infoURL := &url.URL{Path: basePath, RawPath: baseRawPath, RawQuery: url.Values{"source": {"original"}, "playbackId": {playbackID}}.Encode()}
		manifest.Sources[0].URL = streamURL.String()
		manifest.Sources[0].SourceInfoURL = infoURL.String()
		manifest.Sources[0].URLSource = s.Proxy.ObservedStreamURLSource(v.DriveID, v.FileID, playbackID)
	}
	if !s.Proxy.HasPlaybackProvider(v.DriveID) {
		writeJSON(w, http.StatusOK, manifest)
		return
	}
	resolveURL := &url.URL{Path: basePath, RawPath: baseRawPath, RawQuery: "source=cloud"}
	cloud := PlaybackSourceDTO{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "unknown", ResolveURL: resolveURL.String()}
	if selected == "" || selected == "original" {
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
		sources, err = s.Proxy.PlaybackSources(r.Context(), v.DriveID, v.FileID, r.UserAgent(), refresh)
	}
	if err != nil {
		s.Proxy.WritePlaybackError(w, v.DriveID, err)
		return
	}
	// Resolve a group to its best ready rendition, while retaining concrete IDs
	// for later selection. Only the selected rendition gets a media session.
	resolvedID := selected
	if selected == "cloud" {
		for _, source := range sources {
			if source.ID != "original" && source.Status == "ready" {
				resolvedID = source.ID
				break
			}
		}
	}
	for _, source := range sources {
		if source.ID == "" || source.ID == "original" || !validPlaybackSourceID(source.ID) {
			continue
		}
		sourceURL := &url.URL{Path: basePath, RawPath: baseRawPath}
		query := url.Values{"source": {source.ID}}
		sourceURL.RawQuery = query.Encode()
		dto := PlaybackSourceDTO{
			ID: source.ID, Label: source.Label, Kind: source.Kind, Protocol: source.Protocol, Status: source.Status,
			MediaType: source.MediaType, Message: source.Message, ResolveURL: sourceURL.String(),
			Resolution: source.Resolution, Width: source.Width, Height: source.Height, Bitrate: source.Bitrate, Codec: source.Codec,
		}
		if source.Status == "processing" && s.Proxy.HasPlaybackPreparer(v.DriveID) {
			prepareURL := &url.URL{Path: basePath + "/prepare"}
			if source.ID != "cloud" {
				prepareURL.RawQuery = query.Encode()
			}
			if baseRawPath != "" {
				prepareURL.RawPath = baseRawPath + "/prepare"
			}
			dto.PrepareURL = prepareURL.String()
		}
		if source.Status == "ready" {
			if source.Protocol == "file" && source.FileRelay != nil && source.Link != nil {
				// These authenticated routes resolve signatures on each request.
				// Unlike HLS sessions, all file renditions are ready immediately
				// and their public URLs do not inherit the upstream expiry.
				dto.URL = "/p/playback/" + pathSegment(v.ID) + "/" + pathSegment(source.ID)
				if shareID := routeParam(r, "shareID"); shareID != "" {
					dto.URL = "/p/share/" + pathSegment(shareID) + "/playback/" + pathSegment(source.ID)
				}
			} else if source.ID != resolvedID {
				dto.Status = "unknown"
			} else if source.Relay != nil {
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
	// A valid rendition ID may have disappeared since the last manifest.
	// Return the current candidates so the player can recover or offer a switch.
	if len(manifest.Sources) == 1 {
		cloud.Status, cloud.Message = "unavailable", "网盘暂未提供该视频的兼容播放源"
		manifest.Sources = append(manifest.Sources, cloud)
	}
	writeJSON(w, http.StatusOK, manifest)
}

func validPlaybackSourceID(id string) bool {
	if len(id) > 64 {
		return false
	}
	if id != "" && id != "original" && id != "cloud" && !strings.HasPrefix(id, "cloud-") {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (s *Server) handleVideoHLS(w http.ResponseWriter, r *http.Request) {
	v, err := s.availableVideo(r.Context(), routeParam(r, "id"))
	if err != nil {
		writeCatalogLookupError(w, r, err)
		return
	}
	s.serveVideoHLS(w, r, v)
}

func (s *Server) handleVideoPlaybackFile(w http.ResponseWriter, r *http.Request) {
	v, err := s.availableVideo(r.Context(), routeParam(r, "id"))
	if err != nil {
		writeCatalogLookupError(w, r, err)
		return
	}
	s.servePlaybackFile(w, r, v)
}

func (s *Server) handleSharedVideoPlaybackFile(w http.ResponseWriter, r *http.Request) {
	v, ok := s.activeSharedVideo(w, r)
	if !ok {
		return
	}
	s.servePlaybackFile(w, r, v)
}

func (s *Server) servePlaybackFile(w http.ResponseWriter, r *http.Request, v *catalog.Video) {
	sourceID := routeParam(r, "sourceID")
	if s.Proxy == nil || sourceID == "" || sourceID == "original" || !validPlaybackSourceID(sourceID) {
		http.NotFound(w, r)
		return
	}
	s.Proxy.ServePlaybackFile(w, r, v.DriveID, v.FileID, sourceID)
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
