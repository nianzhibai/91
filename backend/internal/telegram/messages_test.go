package telegram

import (
	"context"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/video-site/backend/internal/catalog"
)

// Decode rendered HTML to catch broken tags/entities and enforce Telegram's
// message limit even for long input containing non-BMP characters.
func assertValidMessage(t *testing.T, text string) string {
	t.Helper()
	if !utf8.ValidString(text) {
		t.Fatal("message contains invalid UTF-8")
	}
	decoder := xml.NewDecoder(strings.NewReader("<message>" + text + "</message>"))
	var plain strings.Builder
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid message HTML: %v", err)
		}
		switch value := token.(type) {
		case xml.CharData:
			plain.Write(value)
		case xml.StartElement:
			if value.Name.Local != "message" && value.Name.Local != "b" && value.Name.Local != "code" {
				t.Fatalf("unexpected HTML tag: %s", value.Name.Local)
			}
		}
	}
	if size := len(utf16.Encode([]rune(plain.String()))); size == 0 || size > 4096 {
		t.Fatalf("message length out of bounds: %d", size)
	}
	return plain.String()
}

func TestImportMessagesKeepStatusTitleAndActionsDistinct(t *testing.T) {
	for _, tc := range []struct {
		state, heading string
		wantTask       bool
		wantLink       bool
	}{
		{catalog.RemoteUploadQueued, "已加入队列", false, false},
		{catalog.RemoteUploadDownloading, "正在从 TG 获取视频", false, false},
		{catalog.RemoteUploadValidating, "正在校验视频", false, false},
		{catalog.RemoteUploadSaving, "正在入库", false, false},
		{catalog.RemoteUploadCompleted, "已保存", false, true},
		{catalog.RemoteUploadFailed, "保存失败", true, false},
		{catalog.RemoteUploadCanceled, "已取消保存", false, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			job := &catalog.RemoteUploadJob{
				ID: "tg-task", State: tc.state, RequestedTitle: "原始标题",
				ResolvedTitle: "海边 & <日落>\n第二行", ErrorMessage: "读取 <video> 失败 & 请重试",
				CompletedVideoID: "video #1", TotalBytes: 100, BytesDownloaded: 50,
			}
			message := renderImportMessage(job, "https://example.com/library/", 8<<20)
			plain := assertValidMessage(t, message.text)
			if !strings.Contains(message.text, "<b>"+tc.heading+"</b>\n海边 &amp; &lt;日落&gt; 第二行") {
				t.Fatalf("status and title are not separated or escaped: %s", message.text)
			}
			if strings.Contains(plain, "原始标题") || (tc.state != catalog.RemoteUploadDownloading && strings.Contains(plain, "%")) {
				t.Fatalf("wrong title or unmeasured progress: %s", plain)
			}
			if tc.state == catalog.RemoteUploadDownloading && !strings.Contains(message.text, "<code>██████░░░░░░  50%</code>\n50 B / 100 B\n速度：8.0 MiB/s") {
				t.Fatalf("missing visual download progress: %s", message.text)
			}
			if strings.Contains(message.text, "<code>tg-task</code>") != tc.wantTask {
				t.Fatalf("task ID visibility: %s", message.text)
			}
			if (message.videoURL != "") != tc.wantLink {
				t.Fatalf("unexpected video action: %q", message.videoURL)
			}
			if tc.wantLink && message.videoURL != "https://example.com/library/video/video%20%231" {
				t.Fatalf("invalid video URL: %s", message.videoURL)
			}
			if tc.state == catalog.RemoteUploadFailed && !strings.Contains(plain, job.ErrorMessage) {
				t.Fatalf("failure reason lost: %s", plain)
			}
		})
	}
}

func TestMessagesBoundAndEscapeDynamicContent(t *testing.T) {
	input := strings.Repeat("🎬<&>\n", 2000)
	job := &catalog.RemoteUploadJob{State: catalog.RemoteUploadFailed, ID: input, ResolvedTitle: input, ErrorMessage: input}
	message := renderImportMessage(job, "", -1)
	plain := assertValidMessage(t, message.text)
	if strings.Count(plain, "…") != 3 || strings.Count(plain, "\n") > 8 {
		t.Fatalf("dynamic fields broke the compact layout: %s", plain)
	}
	plain = assertValidMessage(t, renderReceiptMessage(catalog.TelegramReceipt{Response: input}).text)
	if !strings.HasSuffix(plain, "…") {
		t.Fatal("long receipt text was not truncated")
	}
	message = renderReceiptMessage(catalog.TelegramReceipt{Response: `<b>不是标题</b> & "原文"`})
	if plain := assertValidMessage(t, message.text); !strings.Contains(plain, `<b>不是标题</b> & "原文"`) {
		t.Fatalf("stored plain text was interpreted as HTML: %s", plain)
	}
}

func TestDuplicateVideoMessageExplainsSkipAndKeepsVideoLink(t *testing.T) {
	job := &catalog.RemoteUploadJob{ID: "saved", State: catalog.RemoteUploadCompleted, ResolvedTitle: "已保存 & <标题>", CompletedVideoID: "video #1"}
	message := renderDuplicateImportMessage(job, "https://example.com/")
	plain := assertValidMessage(t, message.text)
	if !strings.Contains(plain, "检测到重复视频") || !strings.Contains(plain, "之前已保存，已跳过下载和入库") || !strings.Contains(plain, job.ResolvedTitle) {
		t.Fatalf("duplicate explanation incomplete: %s", message.text)
	}
	if strings.Contains(plain, "封面和预览将在后台生成") || message.videoURL != "https://example.com/video/video%20%231" {
		t.Fatalf("duplicate message repeated new-import text or lost its link: %+v", message)
	}
}

func TestMediaGroupMessagesSeparateNewAndDuplicateVideos(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		duplicate []string
		want      []string
		wantLink  bool
	}{
		{"new videos", catalog.RemoteUploadCompleted, nil, []string{"已保存：2 / 2"}, true},
		{"mixed downloading", catalog.RemoteUploadDownloading, []string{"saved"}, []string{"新增保存：0 / 1", "重复跳过：1", "50%"}, false},
		{"mixed complete", catalog.RemoteUploadCompleted, []string{"saved"}, []string{"新增保存：1 / 1", "重复跳过：1"}, true},
		{"mixed failure", catalog.RemoteUploadFailed, []string{"saved"}, []string{"新增保存：0 / 1", "重复跳过：1", "失败：1"}, true},
		{"all duplicate", catalog.RemoteUploadCompleted, []string{"saved", "new"}, []string{"媒体组视频均已保存过", "重复跳过：2 / 2", "已跳过下载和入库"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := []*catalog.RemoteUploadJob{
				{ID: "saved", State: catalog.RemoteUploadCompleted, CompletedVideoID: "existing", RequestedTitle: "已保存视频"},
				{ID: "new", State: tc.state, CompletedVideoID: "new-video", RequestedTitle: "新视频", BytesDownloaded: 50, TotalBytes: 100},
			}
			message := renderMediaGroupMessage(jobs, tc.duplicate, "https://example.com", func(string) int64 { return 0 })
			plain := assertValidMessage(t, message.text)
			for _, want := range tc.want {
				if !strings.Contains(plain, want) {
					t.Fatalf("group message missing %q: %s", want, plain)
				}
			}
			if (message.videoURL != "") != tc.wantLink {
				t.Fatalf("incorrect group video link: %s", message.videoURL)
			}
			if len(tc.duplicate) > 0 && strings.Contains(plain, "已保存：2 / 2") {
				t.Fatalf("duplicate videos counted as new imports: %s", plain)
			}
		})
	}
}

func TestImportMessagesHandleMissingDetails(t *testing.T) {
	job := &catalog.RemoteUploadJob{State: catalog.RemoteUploadCompleted, RequestedTitle: "  请求标题  ", CompletedVideoID: "video"}
	message := renderImportMessage(job, "", -1)
	if !strings.Contains(message.text, "\n请求标题\n") || message.videoURL != "" {
		t.Fatalf("title fallback or absent site URL: %+v", message)
	}
	job.CompletedVideoID = ""
	if message := renderImportMessage(job, "https://example.com", -1); message.videoURL != "" {
		t.Fatal("created a link without a video ID")
	}
	job.RequestedTitle, job.State = "", catalog.RemoteUploadFailed
	message = renderImportMessage(job, "", -1)
	if !strings.Contains(message.text, "未命名视频") || !strings.Contains(message.text, "查看详情") || strings.Contains(message.text, "<code>") {
		t.Fatalf("missing details produce an incomplete reply: %s", message.text)
	}
	job.State = catalog.RemoteUploadDownloading
	message = renderImportMessage(job, "", -1)
	if strings.Contains(message.text, "%") || strings.Contains(message.text, "<code>") {
		t.Fatalf("unknown file size produced measured progress: %s", message.text)
	}
	job.TotalBytes, job.BytesDownloaded = 1<<30, 1<<29
	message = renderImportMessage(job, "", -1)
	if !strings.Contains(message.text, "512.0 MiB / 1.0 GiB\n速度：测速中") {
		t.Fatalf("download sizes are not readable: %s", message.text)
	}
	message = renderImportMessage(job, "", 0)
	if !strings.Contains(message.text, "速度：0 B/s") {
		t.Fatalf("stalled download hid its speed: %s", message.text)
	}
	job.CancelRequested = true
	message = renderImportMessage(job, "", 8<<20)
	if !strings.Contains(message.text, "正在取消保存") || strings.Contains(message.text, "<code>") {
		t.Fatalf("cancellation retained download progress: %s", message.text)
	}
}

func TestCommandsAndMediaHintsRespectAccess(t *testing.T) {
	s, _ := testService(t)
	for _, tc := range []struct {
		name, command, chatType string
		userID                  int64
		want                    string
	}{
		{"start", "/start", "private", 42, "保存视频到91"},
		{"help", "/help", "private", 42, "视频附带的完整说明"},
		{"addressed help", "/help@test_bot", "private", 42, "保存视频到91"},
		{"start argument", "/start welcome", "private", 42, "保存视频到91"},
		{"id", "/id", "private", 42, "<code>42</code>"},
		{"bootstrap id", "/id@test_bot", "private", 99, "<code>99</code>"},
		{"unauthorized start", "/start", "private", 99, "需要开通权限"},
		{"unauthorized help", "/help", "private", 99, "需要开通权限"},
		{"unauthorized status", "/status", "private", 99, "需要开通权限"},
		{"unsupported", "https://example.com/video/1", "private", 42, "请发送视频"},
		{"unknown command", "/unknown", "private", 42, "请发送视频"},
		{"unauthorized media", "", "private", 99, ""},
		{"group id", "/id", "group", 42, ""},
		{"group help", "/help", "group", 42, ""},
		{"group status", "/status", "group", 42, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := videoUpdate(1, tc.userID)
			u.Message.Video = nil
			u.Message.Text, u.Message.Chat.Type = tc.command, tc.chatType
			input, err := s.prepareImport(context.Background(), u)
			if err != nil {
				t.Fatal(err)
			}
			if input.Source != nil {
				t.Fatal("command or unsupported message created an import")
			}
			if tc.want == "" {
				if input.Receipt.Response != "__ignore__" {
					t.Fatalf("unexpected response: %+v", input.Receipt)
				}
				return
			}
			message := renderReceiptMessage(input.Receipt)
			assertValidMessage(t, message.text)
			if !strings.Contains(message.text, tc.want) {
				t.Fatalf("response missing %q: %s", tc.want, message.text)
			}
		})
	}
}
