package quark

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/video-site/backend/internal/drives"
)

type playbackResponse struct {
	Data struct {
		DefaultResolution string          `json:"default_resolution"`
		Videos            []playbackVideo `json:"video_list"`
	} `json:"data"`
}

type playbackVideo struct {
	Resolution string `json:"resolution"`
	Status     string `json:"trans_status"`
	Info       struct {
		URL     string  `json:"url"`
		Format  string  `json:"file_format"`
		Codec   string  `json:"codec"`
		Width   int     `json:"width"`
		Height  int     `json:"height"`
		Bitrate float64 `json:"bitrate"`
	} `json:"video_info"`
}

func (d *Driver) playback(ctx context.Context, fileID string) (*playbackResponse, error) {
	if strings.TrimSpace(fileID) == "" {
		return nil, errors.New("quark playback: empty file ID")
	}
	var out playbackResponse
	err := d.requestAt(ctx, d.playbackAPI, "/file/v2/play", http.MethodPost, nil, map[string]any{
		"fid": fileID, "resolutions": "normal,low,high,super,2k,4k", "supports": "fmp4,m3u8",
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("quark playback: %w", err)
	}
	return &out, nil
}

// OriginalPlaybackURL is deliberately separate from StreamURL: scanning,
// downloads and fingerprinting must not initiate provider video preparation.
func (d *Driver) OriginalPlaybackURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	// Leave time inside the proxy's resolution deadline for the download fallback.
	playCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	out, err := d.playback(playCtx, fileID)
	cancel()
	if err == nil {
		for _, video := range out.Data.Videos {
			if video.Status != "raw" || video.Info.URL == "" {
				continue
			}
			link, linkErr := d.playbackLink(video.Info.URL)
			if linkErr == nil {
				link.URLSource = "quark-raw"
				link.Headers.Set("Cookie", d.cookieSnapshot())
				return link, nil
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return d.StreamURL(ctx, fileID)
}

// PlaybackSources is resolved lazily for an active viewer. Listing the catalog
// never calls this provider endpoint. A provider's right label describes a tier,
// not whether the current account may play it; availability comes from its URL
// and transcode status.
func (d *Driver) PlaybackSources(ctx context.Context, fileID string) ([]drives.PlaybackSource, error) {
	out, err := d.playback(ctx, fileID)
	if err != nil {
		return nil, err
	}
	sources := make([]drives.PlaybackSource, 0, len(out.Data.Videos))
	seen := make(map[string]bool)
	for _, video := range out.Data.Videos {
		if video.Status == "raw" || !validResolution(video.Resolution) || seen[video.Resolution] {
			continue
		}
		seen[video.Resolution] = true
		source := drives.PlaybackSource{
			ID: "cloud-" + video.Resolution, Label: resolutionLabel(video.Resolution),
			Kind: "transcoded", Protocol: "hls", Status: "unavailable",
			MediaType: "application/vnd.apple.mpegurl", Resolution: video.Resolution,
			Width: video.Info.Width, Height: video.Info.Height,
			Bitrate: int(math.Round(video.Info.Bitrate)), Codec: video.Info.Codec,
			Message: "夸克暂未提供该清晰度的可用播放源",
		}
		switch video.Status {
		case "transcoding", "waiting", "pending":
			source.Status = "processing"
			source.Message = "夸克正在转码，完成后可选择此清晰度"
		case "success":
			if video.Info.Format == "m3u8" && video.Info.URL != "" {
				link, err := d.playbackLink(video.Info.URL)
				if err != nil {
					return nil, err
				}
				// Quark rejects these signed playlists without its account cookie (412).
				// Authorize only validated media requests, using the current cookie rather
				// than embedding a credential snapshot in an advertised source.
				source.Link = link
				source.Relay = &drives.HLSRelay{
					AllowedHosts: []string{mustPlaybackHost(link.URL)},
					Authorize:    func(req *http.Request) { req.Header.Set("Cookie", d.cookieSnapshot()) },
				}
				link.Headers.Set("Origin", d.referer)
				source.Status = "ready"
				source.Message = ""
			}
		}
		sources = append(sources, source)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		return resolutionRank(sources[i].Resolution) > resolutionRank(sources[j].Resolution)
	})
	return sources, nil
}

func (d *Driver) playbackLink(raw string) (*drives.StreamLink, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || !strings.HasPrefix(u.Hostname(), "video-play-") || !(strings.HasSuffix(u.Hostname(), ".drive.quark.cn") || strings.HasSuffix(u.Hostname(), ".pds.quark.cn")) {
		return nil, errors.New("quark playback: untrusted media URL")
	}
	expires := time.Now().Add(5 * time.Minute)
	if seconds, err := strconv.ParseInt(u.Query().Get("Expires"), 10, 64); err == nil {
		deadline := time.Unix(seconds, 0).Add(-15 * time.Second)
		if deadline.Before(expires) {
			expires = deadline
		}
	}
	return &drives.StreamLink{URL: raw, Headers: http.Header{"User-Agent": {d.ua}, "Referer": {d.referer}}, Expires: expires}, nil
}

func mustPlaybackHost(raw string) string { u, _ := url.Parse(raw); return u.Host }
func validResolution(value string) bool {
	if value == "" || len(value) > 24 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
func resolutionLabel(value string) string {
	switch value {
	case "low":
		return "流畅"
	case "normal":
		return "标清"
	case "high":
		return "高清"
	case "super":
		return "超清"
	default:
		return strings.ToUpper(value)
	}
}
func resolutionRank(value string) int {
	switch value {
	case "4k":
		return 6
	case "2k":
		return 5
	case "super":
		return 4
	case "high":
		return 3
	case "normal":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

var _ drives.PlaybackSourceProvider = (*Driver)(nil)
var _ drives.OriginalPlaybackProvider = (*Driver)(nil)
