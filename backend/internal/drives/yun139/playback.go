package yun139

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

// PlaybackSources uses Mobile Cloud's browser-ready HLS independently of the
// original bytes used by downloads, fingerprints and background generation.
// The shared playback resolver owns caching and explicit link refreshes.
func (d *Driver) PlaybackSources(ctx context.Context, fileID string) ([]drives.PlaybackSource, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, errors.New("yun139 playback: empty file ID")
	}
	var result struct {
		Data struct {
			FileID      string `json:"fileId"`
			PreviewInfo struct {
				Status string `json:"status"`
				URL    string `json:"url"`
			} `json:"previewInfo"`
		} `json:"data"`
	}
	if err := d.post(ctx, "/videoPreview/getPreviewInfo", map[string]string{
		"fileId": fileID, "category": "video",
	}, &result); err != nil {
		return nil, err
	}
	if result.Data.FileID != "" && result.Data.FileID != fileID {
		return nil, errors.New("yun139 playback: preview response belongs to a different file")
	}
	preview := result.Data.PreviewInfo
	source := drives.PlaybackSource{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "unavailable", MediaType: "application/vnd.apple.mpegurl", Message: "移动云盘暂未提供该视频的兼容播放源"}
	if strings.EqualFold(strings.TrimSpace(preview.Status), "Finished") && strings.TrimSpace(preview.URL) != "" {
		url := strings.TrimSpace(preview.URL)
		if !validHTTPURL(url) {
			return nil, errors.New("yun139 playback: invalid HLS URL")
		}
		source.Status, source.Message = "ready", ""
		source.Link = &drives.StreamLink{
			URL: url, Expires: time.Now().Add(2 * time.Minute), ClientRedirectSafe: true,
		}
	} else if strings.EqualFold(preview.Status, "Pending") || strings.EqualFold(preview.Status, "Running") {
		source.Status = "processing"
		source.Message = "移动云盘正在转码，请稍后重试兼容播放"
	}
	return []drives.PlaybackSource{source}, nil
}

var _ drives.PlaybackSourceProvider = (*Driver)(nil)
