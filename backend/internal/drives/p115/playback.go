package p115

import (
	"context"
	"errors"
	"strings"

	"github.com/nianzhibai/91/backend/internal/drives"
)

// PlaybackSources retains the master so HLS clients can select a rendition and
// its audio tracks. Account credentials stay in master/status requests and are
// never included in the returned playback source.
func (d *Driver) PlaybackSources(ctx context.Context, fileID string) ([]drives.PlaybackSource, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, errors.New("115 playback: empty file ID")
	}
	source := drives.PlaybackSource{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "unavailable", MediaType: "application/vnd.apple.mpegurl", Message: "115 暂未提供该视频的兼容播放源"}
	for attempt := 0; attempt < 2; attempt++ {
		masterURL, playlist, err := d.fetchHLSMaster(ctx, fileID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if _, limited := drives.RateLimitRetryAfter(err); limited {
				return nil, err
			}
			if !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
				return nil, err
			}
		} else if _, err := selectHLSVariant(masterURL, string(playlist)); err == nil {
			source.Status, source.Message = "ready", ""
			source.Link = p115HLSLink(masterURL)
			source.Relay = &drives.HLSRelay{Playlist: string(playlist), AllowedHosts: []string{".115.com"}}
			return []drives.PlaybackSource{source}, nil
		}
		if attempt > 0 || d.cookie == "" {
			break
		}
		// A rejected master alone cannot distinguish pending conversion from an
		// expired login or an unsupported file. local=1 only inspects the file.
		info, err := d.playbackVideoInfo(ctx, fileID, true)
		if err != nil {
			return nil, err
		}
		if info.needsTranscode(fileID) {
			source.Status, source.Message = "processing", "视频正在转码，完成后将自动播放"
			break
		}
		if info.FileStatus == nil || *info.FileStatus != 1 {
			break
		}
		// Conversion may have finished during the status query. Retry the
		// master once before reporting a completed file unavailable.
	}
	return []drives.PlaybackSource{source}, nil
}

var _ drives.PlaybackSourceProvider = (*Driver)(nil)
