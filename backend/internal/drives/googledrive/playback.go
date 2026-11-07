package googledrive

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

const (
	defaultPlaybackAPIURL = "https://workspacevideo-pa.clients6.google.com/v1/drive/media"
	// Public Drive web-client key, also used by yt-dlp's Google Drive extractor.
	// Account authorization comes from the administrator's Google login cookie.
	drivePlaybackAPIKey = "AIzaSyDVQw45DwoYh632gvsP5vPDqEKvb-Ywnb8"
	drivePlaybackOrigin = "https://drive.google.com"
)

type playbackResponse struct {
	MediaStreamingData struct {
		FormatStreamingData struct {
			ProgressiveTranscodes []playbackTranscode `json:"progressiveTranscodes"`
		} `json:"formatStreamingData"`
	} `json:"mediaStreamingData"`
}

type playbackTranscode struct {
	Itag     int    `json:"itag"`
	URL      string `json:"url"`
	Metadata struct {
		MimeType   string `json:"mimeType"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		Bitrate    int    `json:"maxContainerBitrate"`
		VideoCodec string `json:"videoCodecString"`
		AudioCodec string `json:"audioCodecString"`
	} `json:"transcodeMetadata"`
}

var _ drives.UserAgentPlaybackSourceProvider = (*Driver)(nil)

// Browser delivery must use the media request's own UA, including when that
// differs from the UA used by the player's earlier metadata fetch.
func (d *Driver) PlaybackSourcesWithUserAgent(ctx context.Context, fileID, userAgent string) ([]drives.PlaybackSource, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, errors.New("googledrive playback: empty file ID")
	}
	unavailable := drives.PlaybackSource{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "file", Status: "unavailable", MediaType: "video/mp4"}
	if d.playbackCookie == "" {
		unavailable.Message = "Google Drive 尚未配置网页兼容播放，请联系管理员"
		return []drives.PlaybackSource{unavailable}, nil
	}
	if userAgent == "" {
		unavailable.Message = "Google Drive 兼容播放需要浏览器 User-Agent"
		return []drives.PlaybackSource{unavailable}, nil
	}
	authUser, err := NormalizePlaybackAuthUser(d.playbackAuthUser)
	if err != nil {
		unavailable.Message = err.Error()
		return []drives.PlaybackSource{unavailable}, nil
	}
	authorization, err := playbackAuthorization(d.playbackCookie, time.Now())
	if err != nil {
		unavailable.Message = "Google Drive 兼容播放 Cookie 无效，请联系管理员更新网页登录 Cookie"
		return []drives.PlaybackSource{unavailable}, nil
	}
	u, err := url.Parse(d.playbackBaseURL + "/" + url.PathEscape(fileID) + "/playback")
	if err != nil {
		return nil, errors.New("googledrive playback: invalid API URL")
	}
	query := u.Query()
	query.Set("key", drivePlaybackAPIKey)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("googledrive playback: invalid request")
	}
	req.Header.Set("Cookie", d.playbackCookie)
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Origin", drivePlaybackOrigin)
	req.Header.Set("Referer", drivePlaybackOrigin+"/")
	req.Header.Set("X-Goog-AuthUser", authUser)
	req.Header.Set("User-Agent", userAgent)
	client := *d.client.GetClient()
	// A metadata redirect must never send the Google account cookie elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &drives.ProviderError{Kind: drives.ProviderErrorUnavailable, Err: errors.New("googledrive playback: request failed")}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("googledrive playback: invalid response body")
	}
	var out playbackResponse
	var apiErr apiErrorResp
	_ = json.Unmarshal(body, &apiErr)
	if isGoogleHTTPRateLimit(response.StatusCode, response.Header, apiErr.Error) {
		return nil, &drives.RateLimitError{Provider: Kind, RetryAfter: parseRetryAfterHeader(response.Header.Get("Retry-After")), Err: errors.New("googledrive playback: rate limited")}
	}
	if response.StatusCode == http.StatusUnauthorized {
		unavailable.Message = "Google Drive 网页登录已失效，请联系管理员更新兼容播放 Cookie"
		return []drives.PlaybackSource{unavailable}, nil
	}
	if response.StatusCode == http.StatusForbidden {
		unavailable.Message = "当前 Google 网页账号无权播放该视频，请检查网页登录状态、账号索引和视频访问权限"
		return []drives.PlaybackSource{unavailable}, nil
	}
	if response.StatusCode == http.StatusNotFound {
		unavailable.Message = "Google Drive 暂未提供该视频的兼容播放源"
		return []drives.PlaybackSource{unavailable}, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, &drives.ProviderError{Kind: drives.ProviderErrorUnavailable, Err: fmt.Errorf("googledrive playback: HTTP %d", response.StatusCode)}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("googledrive playback: invalid JSON response")
	}
	now := time.Now()
	sources := make([]drives.PlaybackSource, 0)
	seen := make(map[int]bool)
	for _, transcode := range out.MediaStreamingData.FormatStreamingData.ProgressiveTranscodes {
		meta := transcode.Metadata
		mediaType, _, err := mime.ParseMediaType(meta.MimeType)
		if err != nil || mediaType != "video/mp4" || transcode.Itag <= 0 || seen[transcode.Itag] || meta.VideoCodec == "" || meta.AudioCodec == "" {
			continue
		}
		u, err := url.Parse(transcode.URL)
		if err != nil || !validPlaybackMediaURL(u) {
			continue
		}
		expires := now.Add(2 * time.Minute)
		if raw := u.Query().Get("expire"); raw != "" {
			seconds, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				continue
			}
			expires = time.Unix(seconds, 0).Add(-15 * time.Second)
		}
		if !expires.After(now) {
			continue
		}
		seen[transcode.Itag] = true
		resolution := min(meta.Width, meta.Height)
		label := "兼容播放"
		if resolution > 0 {
			label = strconv.Itoa(resolution) + "p"
		}
		sources = append(sources, drives.PlaybackSource{
			ID: "cloud-" + strconv.Itoa(transcode.Itag), Label: label, Kind: "transcoded", Protocol: "file", Status: "ready",
			MediaType:  mime.FormatMediaType("video/mp4", map[string]string{"codecs": meta.VideoCodec + ", " + meta.AudioCodec}),
			Link:       &drives.StreamLink{URL: u.String(), Expires: expires, Headers: http.Header{"User-Agent": {userAgent}}, ClientRedirectSafe: true},
			FileRelay:  &drives.FileRelay{AllowedHosts: []string{".c.drive.google.com"}},
			Resolution: label, Width: meta.Width, Height: meta.Height, Bitrate: meta.Bitrate / 1000, Codec: meta.VideoCodec,
		})
	}
	sort.SliceStable(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		if a.Width*a.Height != b.Width*b.Height {
			return a.Width*a.Height > b.Width*b.Height
		}
		return a.Bitrate > b.Bitrate
	})
	if len(sources) == 0 {
		unavailable.Message = "Google Drive 暂未提供可用的 MP4 兼容播放源，请稍后重试"
		return []drives.PlaybackSource{unavailable}, nil
	}
	return sources, nil
}

func validPlaybackMediaURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && u.Fragment == "" &&
		(u.Port() == "" || u.Port() == "443") && u.Path == "/videoplayback" &&
		strings.HasSuffix(strings.ToLower(u.Hostname()), ".c.drive.google.com")
}

// NormalizePlaybackAuthUser selects the account from a multi-login web session.
// Google Drive's /u/N/ URL and X-Goog-AuthUser use the same zero-based index.
func NormalizePlaybackAuthUser(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "0", nil
	}
	index, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return "", errors.New("Google Drive 网页账号索引必须是非负整数")
	}
	return strconv.FormatUint(index, 10), nil
}

func playbackAuthorization(cookie string, now time.Time) (string, error) {
	if strings.ContainsAny(cookie, "\r\n") {
		return "", &drives.ProviderError{Kind: drives.ProviderErrorAuth, Err: errors.New("googledrive playback: invalid web login cookie")}
	}
	req := &http.Request{Header: http.Header{"Cookie": []string{cookie}}}
	var sapisid string
	for _, name := range []string{"SAPISID", "__Secure-3PAPISID"} {
		if value, err := req.Cookie(name); err == nil && value.Value != "" {
			sapisid = value.Value
			break
		}
	}
	if sapisid == "" {
		return "", &drives.ProviderError{Kind: drives.ProviderErrorAuth, Err: errors.New("googledrive playback: web login cookie is missing SAPISID")}
	}
	stamp := strconv.FormatInt(now.Unix(), 10)
	digest := sha1.Sum([]byte(stamp + " " + sapisid + " " + drivePlaybackOrigin))
	return "SAPISIDHASH " + stamp + "_" + hex.EncodeToString(digest[:]), nil
}
