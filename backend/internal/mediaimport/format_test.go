package mediaimport

import (
	"net/url"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestSupportedExtensionUsesProbeAndSourceHints(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		contentType string
		finalURL    string
		want        string
	}{
		{name: "avi", format: "avi", finalURL: "https://cdn.example/a.bin", want: ".avi"},
		{name: "mkv", format: "matroska,webm", finalURL: "https://cdn.example/a.mkv", want: ".mkv"},
		{name: "webm", format: "matroska,webm", contentType: "video/webm", want: ".webm"},
		{name: "mov", format: "mov,mp4,m4a,3gp,3g2,mj2", finalURL: "https://cdn.example/a.mov", want: ".mov"},
		{name: "mp4", format: "mov,mp4,m4a,3gp,3g2,mj2", contentType: "video/mp4", want: ".mp4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := downloadMetadata{
				ContentType: test.contentType,
				FinalURL:    parseTestURL(t, test.finalURL),
			}
			got, err := supportedExtension(
				mediaInfo{FormatName: test.format, VideoCodecs: []string{"h264"}},
				metadata,
			)
			if err != nil {
				t.Fatalf("supported extension: %v", err)
			}
			if got != test.want {
				t.Fatalf("extension = %q, want %q", got, test.want)
			}
		})
	}

	if _, err := supportedExtension(
		mediaInfo{FormatName: "mpegts", VideoCodecs: []string{"h264"}},
		downloadMetadata{},
	); err == nil {
		t.Fatal("unsupported probe format was accepted")
	}
}

func TestResolveTitleFollowsExplicitDispositionAndURLPriority(t *testing.T) {
	metadata := downloadMetadata{
		ContentDisposition: `attachment; filename="disposition-name.mp4"`,
		FinalURL:           parseTestURL(t, "https://cdn.example/final-name.mp4?token=secret"),
		OriginalURL:        parseTestURL(t, "https://origin.example/original-name.mp4?token=secret"),
	}
	title, err := resolveTitle(
		&catalog.RemoteUploadJob{RequestedTitle: "explicit-name"},
		metadata,
		".mp4",
	)
	if err != nil || title != "explicit-name" {
		t.Fatalf("explicit title = %q err=%v", title, err)
	}
	title, err = resolveTitle(&catalog.RemoteUploadJob{}, metadata, ".mp4")
	if err != nil || title != "disposition-name" {
		t.Fatalf("disposition title = %q err=%v", title, err)
	}
	metadata.ContentDisposition = ""
	title, err = resolveTitle(&catalog.RemoteUploadJob{}, metadata, ".mp4")
	if err != nil || title != "final-name" {
		t.Fatalf("final URL title = %q err=%v", title, err)
	}
}

func parseTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
