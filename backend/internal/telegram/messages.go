package telegram

import (
	"fmt"
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/video-site/backend/internal/catalog"
)

// Receipts store outcomes, not Telegram markup. Rendering stays here so changes
// to wording and layout do not affect reception, persistence, or delivery.
const (
	responseHelp        = "help"
	responseUserID      = "user_id"
	responseStatus      = "status"
	responseNeedsAccess = "needs_access"
	responseUnsupported = "unsupported_media"
)

type botMessage struct {
	text     string
	videoURL string
}

func renderReceiptMessage(r catalog.TelegramReceipt) botMessage {
	switch r.Response {
	case responseHelp:
		return botMessage{text: "🎬 <b>保存视频到91</b>\n\n" +
			"直接向机器人发送或转发其他群组的视频\n" +
			"也可发送 Telegram 消息链接，自动保存对应媒体组中的全部视频\n" +
			"视频附带的完整说明会作为标题，换行合并为空格\n\n" +
			"/status 转存统计\n" +
			"/id 查看当前 Telegram ID"}
	case responseUserID:
		return botMessage{text: "👤 <b>你的 Telegram ID</b>\n\n<code>" + strconv.FormatInt(r.SenderID, 10) +
			"</code>"}
	case responseNeedsAccess:
		return botMessage{text: "🔒 <b>需要开通权限</b>\n\n" +
			"你的 Telegram ID：<code>" + strconv.FormatInt(r.SenderID, 10) + "</code>"}
	case responseUnsupported:
		return botMessage{text: "ℹ️ <b>请发送视频</b>"}
	case catalog.TelegramResponseQueueFull:
		return botMessage{text: "⏳ <b>队列已满</b>\n\n请稍后重新发送视频"}
	default:
		// Plain rejection reasons and receipts from older versions are untrusted
		// text. They must never be interpreted as Telegram HTML.
		return botMessage{text: "ℹ️ <b>提示</b>\n\n" + messageText(r.Response, 600)}
	}
}

func renderStatusMessage(stats catalog.TelegramImportStats) botMessage {
	return botMessage{text: fmt.Sprintf("📊 <b>转存统计</b>\n\n"+
		"进行中：%d 个\n今日成功：%d 个\n累计成功：%d 个",
		stats.Active, stats.TodayCompleted, stats.TotalCompleted)}
}

func importMessageTitle(j *catalog.RemoteUploadJob) string {
	title := strings.TrimSpace(j.ResolvedTitle)
	if title == "" {
		title = strings.TrimSpace(j.RequestedTitle)
	}
	if title == "" {
		title = "未命名视频"
	}
	return messageText(title, 120)
}

func renderDuplicateImportMessage(j *catalog.RemoteUploadJob, siteBaseURL string) botMessage {
	out := renderImportMessage(j, siteBaseURL, 0)
	out.text = "♻️ <b>检测到重复视频</b>\n" + importMessageTitle(j) + "\n之前已保存，已跳过下载和入库"
	return out
}

func renderImportMessage(j *catalog.RemoteUploadJob, siteBaseURL string, bytesPerSecond int64) botMessage {
	title := importMessageTitle(j)
	var out botMessage
	switch j.State {
	case catalog.RemoteUploadCompleted:
		out.text = "✅ <b>已保存</b>\n" + title + "\n封面和预览将在后台生成"
		if j.CompletedVideoID != "" && siteBaseURL != "" {
			out.videoURL = strings.TrimRight(siteBaseURL, "/") + "/video/" + url.PathEscape(j.CompletedVideoID)
		}
	case catalog.RemoteUploadFailed:
		reason := messageText(j.ErrorMessage, 300)
		if reason == "" {
			reason = "暂时无法完成保存，请在网站后台查看详情"
		}
		out.text = "❌ <b>保存失败</b>\n" + title + "\n" + reason + "\n可在网站后台的 Telegram 页面重试"
		if j.ID != "" {
			out.text += "\n\n任务：<code>" + messageText(j.ID, 100) + "</code>"
		}
	case catalog.RemoteUploadCanceled:
		out.text = "⏹ <b>已取消保存</b>\n" + title
	default:
		icon := "⏳"
		if j.State == catalog.RemoteUploadDownloading && !j.CancelRequested {
			icon = "⬇️"
		}
		out.text = icon + " <b>" + importProgressText(j) + "</b>\n" + title
		if j.State == catalog.RemoteUploadDownloading && !j.CancelRequested && j.TotalBytes > 0 {
			downloaded := min(max(j.BytesDownloaded, 0), j.TotalBytes)
			ratio := float64(downloaded) / float64(j.TotalBytes)
			const width = 12
			filled := int(ratio * width)
			out.text += fmt.Sprintf("\n\n<code>%s%s %3d%%</code>\n%s / %s",
				strings.Repeat("█", filled), strings.Repeat("░", width-filled), int(ratio*100),
				formatDownloadBytes(downloaded), formatDownloadBytes(j.TotalBytes))
			if bytesPerSecond < 0 {
				out.text += "\n速度：测速中"
			} else {
				out.text += "\n速度：" + formatDownloadBytes(bytesPerSecond) + "/s"
			}
		}
	}
	return out
}

func formatDownloadBytes(size int64) string {
	for _, unit := range []struct {
		bytes int64
		name  string
	}{
		{1 << 30, "GiB"},
		{1 << 20, "MiB"},
		{1 << 10, "KiB"},
	} {
		if size >= unit.bytes {
			return fmt.Sprintf("%.1f %s", float64(size)/float64(unit.bytes), unit.name)
		}
	}
	return fmt.Sprintf("%d B", size)
}

func renderMediaGroupMessage(jobs []*catalog.RemoteUploadJob, duplicateJobIDs []string, siteBaseURL string, speed func(string) int64) botMessage {
	completed, failed, canceled, duplicates := 0, 0, 0, 0
	var active, firstCompleted *catalog.RemoteUploadJob
	for _, job := range jobs {
		if slices.Contains(duplicateJobIDs, job.ID) {
			duplicates++
			if firstCompleted == nil {
				firstCompleted = job
			}
			continue
		}
		switch job.State {
		case catalog.RemoteUploadCompleted:
			completed++
			if firstCompleted == nil {
				firstCompleted = job
			}
		case catalog.RemoteUploadFailed:
			failed++
		case catalog.RemoteUploadCanceled:
			canceled++
		default:
			if active == nil || job.State != catalog.RemoteUploadQueued {
				active = job
			}
		}
	}
	heading := "⏳ <b>正在保存媒体组</b>"
	if active == nil {
		heading = "✅ <b>媒体组已保存</b>"
		if failed+canceled > 0 {
			heading = "⚠️ <b>媒体组处理完成</b>"
		}
	}
	out := botMessage{text: fmt.Sprintf("%s\n\n已保存：%d / %d", heading, completed, len(jobs))}
	if duplicates == len(jobs) && duplicates > 0 {
		out.text = fmt.Sprintf("♻️ <b>媒体组视频均已保存过</b>\n\n重复跳过：%d / %d\n已跳过下载和入库", duplicates, len(jobs))
	} else if duplicates > 0 {
		out.text = fmt.Sprintf("%s\n\n新增保存：%d / %d\n重复跳过：%d", heading, completed, len(jobs)-duplicates, duplicates)
	}
	if failed+canceled > 0 {
		out.text += fmt.Sprintf("\n失败：%d · 已取消：%d\n可在网站后台的 Telegram 页面查看和重试", failed, canceled)
	}
	if active != nil {
		out.text += "\n\n" + renderImportMessage(active, "", speed(active.ID)).text
	} else if firstCompleted != nil {
		out.videoURL = renderImportMessage(firstCompleted, siteBaseURL, 0).videoURL
	}
	return out
}

func importProgressText(j *catalog.RemoteUploadJob) string {
	if j.CancelRequested {
		return "正在取消保存"
	}
	switch j.State {
	case catalog.RemoteUploadDownloading:
		return "正在从 TG 获取视频"
	case catalog.RemoteUploadValidating:
		return "正在校验视频"
	case catalog.RemoteUploadSaving:
		return "正在入库"
	case catalog.RemoteUploadQueued:
		if j.Stage == "retry_wait" {
			return "等待重试保存"
		}
	}
	return "已加入队列"
}

// Bound dynamic fields before escaping, keeping messages short and HTML intact.
// Collapse embedded newlines so captions and errors cannot disrupt the layout.
func messageText(text string, limit int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > limit {
		runes = append(runes[:limit-1], '…')
	}
	return html.EscapeString(string(runes))
}
