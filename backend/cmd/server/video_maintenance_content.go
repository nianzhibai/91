package main

import (
	"context"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/dedupe"
	"github.com/video-site/backend/internal/mediasim"
)

// 内容级查重通道：teaser 选段起点只由时长决定，时长几乎相等的两个视频即使
// 压制、标题、封面完全不同，teaser 对齐帧也来自同一源画面。判定、传递成组和
// canonical 选择由 internal/dedupe 统一规划；这里保留薄适配层供通道级测试使用。
type contentDuplicateMaintenanceStats struct {
	Candidates    int
	Extracted     int
	ExtractFailed int
	Comparisons   int
	CrossMatched  int
	Groups        int
	Deleted       int
}

// contentSignatureExtractor 允许测试注入合成签名，生产始终用 ffmpeg 提取。
var contentSignatureExtractor = mediasim.ExtractTeaserFrameSignature

func (a *App) cleanupContentDuplicateVideos(ctx context.Context, localDir string, videos []*catalog.Video, deleted map[string]struct{}) (contentDuplicateMaintenanceStats, error) {
	remaining := make([]*catalog.Video, 0, len(videos))
	for _, video := range videos {
		if video == nil {
			continue
		}
		if _, alreadyDeleted := deleted[video.ID]; alreadyDeleted {
			continue
		}
		current, err := a.cat.GetVideo(ctx, video.ID)
		if err != nil {
			return contentDuplicateMaintenanceStats{}, err
		}
		remaining = append(remaining, current)
	}
	plan, err := a.buildDuplicateMaintenancePlan(ctx, localDir, remaining, dedupe.ChannelContent)
	if err != nil {
		return contentDuplicateMaintenanceStats{}, err
	}
	if err := a.applyDuplicateMaintenancePlan(ctx, localDir, plan); err != nil {
		return contentDuplicateMaintenanceStats{}, err
	}
	for _, action := range plan.Actions {
		deleted[action.VideoID] = struct{}{}
	}
	stats := plan.Stats.Content
	return contentDuplicateMaintenanceStats{
		Candidates:    stats.Candidates,
		Extracted:     stats.Extracted,
		ExtractFailed: stats.ExtractFailed,
		Comparisons:   stats.Comparisons,
		CrossMatched:  stats.CrossMatched,
		Groups:        stats.Groups,
		Deleted:       stats.Deleted,
	}, nil
}
