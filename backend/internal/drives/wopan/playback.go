package wopan

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	sdk "github.com/OpenListTeam/wopan-sdk-go"
	"github.com/go-resty/resty/v2"
	"github.com/video-site/backend/internal/drives"
)

func (d *Driver) PlaybackSources(ctx context.Context, fileID string) ([]drives.PlaybackSource, error) {
	d.clientMu.Lock()
	defer d.clientMu.Unlock()
	if d.client == nil {
		return nil, fmt.Errorf("wopan playback: driver not initialized")
	}
	var data []struct {
		FID  string `json:"fid"`
		M3U8 string `json:"m3u8"`
	}
	_, err := d.client.RequestWoHome("VideoDisplayFiles", sdk.Json{
		"fids":      []string{fileID},
		"userId":    d.client.Phone,
		"spaceType": d.spaceType(),
		"clientId":  sdk.DefaultClientID,
	}, sdk.JsonSecret, &data, func(req *resty.Request) { req.SetContext(ctx) })
	if err != nil {
		return nil, wopanRequestError("playback", err)
	}
	for _, item := range data {
		if item.FID != fileID {
			continue
		}
		hls, err := browserPlaybackLink(item.M3U8)
		if err != nil {
			return nil, err
		}
		source := drives.PlaybackSource{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "ready", Link: hls, MediaType: "application/vnd.apple.mpegurl"}
		if hls == nil {
			source.Status = "unavailable"
			source.Message = "联通网盘暂未提供该视频的兼容播放源"
		}
		return []drives.PlaybackSource{source}, nil
	}
	return nil, fmt.Errorf("wopan playback: source not found in preview response")
}

func browserPlaybackLink(raw string) (*drives.StreamLink, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("wopan playback: invalid source URL")
	}
	return &drives.StreamLink{URL: raw, Expires: time.Now().Add(2 * time.Minute), ClientRedirectSafe: true}, nil
}
