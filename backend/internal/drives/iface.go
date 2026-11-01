package drives

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Drive 是多家网盘统一抽象。上层不区分盘，只区分 Kind。
type Drive interface {
	// Kind 返回驱动代号："quark" / "p115" / "p123" / "pikpak" / "wopan" / "yun139" / "guangyapan" / "onedrive" / "googledrive" / "webdav" / "localstorage"
	Kind() string

	// ID 返回该盘在 catalog 中的唯一标识
	ID() string

	// Init 完成登录态校验；登录态由 Authenticator 另行获取后注入
	Init(ctx context.Context) error

	// List 列指定目录下的直接子项
	List(ctx context.Context, dirID string) ([]Entry, error)

	// Stat 拿到单个文件的元数据
	Stat(ctx context.Context, fileID string) (*Entry, error)

	// StreamURL 返回一次性直链 + 必须的请求头
	// 代理层据此回源，透传 Range
	StreamURL(ctx context.Context, fileID string) (*StreamLink, error)

	// RootID 返回根目录 fileID
	RootID() string
}

// DirectoryNameProvider optionally resolves a directory name without walking
// its subtree. Scans use it once for their starting directory; descendant names
// come from List. Providers that cannot query directory metadata may omit it.
type DirectoryNameProvider interface {
	DirectoryName(ctx context.Context, dirID string) (string, error)
}

// GenerationStreamProvider is an optional drive capability for a provider-
// generated playback stream that is cheaper to seek than the original file.
// Background thumbnail/preview workers prefer this stream, while ordinary
// playback and fingerprinting continue to use StreamURL.
//
// forceRefresh invalidates any short-lived provider cache after a signed
// playlist rejection. Implementations must never expose account credentials in
// the returned StreamLink.
type GenerationStreamProvider interface {
	GenerationStreamURL(ctx context.Context, fileID string, forceRefresh bool) (*StreamLink, error)
}

// GenerationStreamScanResetter resets optional stream availability when a new
// scan starts. The state remains shared by its background generation workers
// until the next scan, including work that finishes after discovery returns.
type GenerationStreamScanResetter interface {
	ResetGenerationStreamForScan()
}

// PlaybackSourceProvider supplies optional cloud playback sources. The common
// playback layer always supplies the original through StreamURL independently.
// Direct links must authorize browser requests without account headers and
// support CORS. HLS sources that require server delivery explicitly supply a
// relay policy. Background generation is a separate API.
type PlaybackSourceProvider interface {
	PlaybackSources(context.Context, string) ([]PlaybackSource, error)
}

// OriginalPlaybackProvider resolves an original-file link for viewers without
// changing the source used by downloads and content fingerprinting.
type OriginalPlaybackProvider interface {
	OriginalPlaybackURL(context.Context, string) (*StreamLink, error)
}

// PlaybackSourcePreparer explicitly starts provider-side preparation after a
// viewer requests playback. Listing or preloading sources must remain read-only.
type PlaybackSourcePreparer interface {
	PreparePlayback(context.Context, string) ([]PlaybackSource, error)
}

type PlaybackSource struct {
	ID         string
	Label      string
	Kind       string // original or transcoded; independent of transport protocol
	Protocol   string // file or hls
	Status     string // ready, processing or unavailable
	Link       *StreamLink
	MediaType  string
	Message    string
	Relay      *HLSRelay
	Resolution string
	Width      int
	Height     int
	Bitrate    int // kilobits per second, as reported by the provider
	Codec      string
}

// HLSRelay opts a playback source into authenticated server delivery. When set,
// Playlist is the resolved master, with relative URIs based on Link.URL.
// Account credentials must not be included in Link.Headers. Providers that need
// account authentication may authorize each already-validated server request.
// The callback stays server-side and can read freshly rotated credentials.
// AllowedHosts accepts
// exact authorities (including ports), or a leading dot for a domain and its
// subdomains. Every resource and redirect must satisfy this provider policy.
type HLSRelay struct {
	Playlist     string
	AllowedHosts []string
	Authorize    func(*http.Request) `json:"-"`
}

// Uploader is the optional write capability of a drive. Callers that produce
// remote files must assert it before starting work instead of discovering an
// unsupported operation only after preparing a potentially large upload.
type Uploader interface {
	Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error)
	EnsureDir(ctx context.Context, pathFromRoot string) (string, error)
}

// Remover is an optional drive capability. It mirrors OpenList's optional
// Remove interface: callers must type-assert before deleting a source file.
type Remover interface {
	Remove(ctx context.Context, fileID string) error
}

// LocalFileProvider exposes a local source without assuming its directory layout.
type LocalFileProvider interface {
	LocalPath(context.Context, string) (string, error)
}

// SourceFile carries the catalog metadata available when an administrator
// requests deletion of the original source file.
type SourceFile struct {
	FileID   string
	ParentID string
	Name     string
	Size     int64
}

// SourceRemover is an optional, richer removal capability for providers whose
// playback ID is not the same ID required by their delete API.
type SourceRemover interface {
	RemoveSource(ctx context.Context, source SourceFile) error
}

type Entry struct {
	ID       string
	Name     string
	Size     int64
	Hash     string
	IsDir    bool
	ParentID string
	MimeType string
	ModTime  time.Time

	// 部分网盘额外信息
	Category     int    // 1=视频 (quark)
	ThumbnailURL string // 网盘侧已提供的快速缩略图
}

type StreamLink struct {
	URL     string
	Headers http.Header
	// URLSource identifies the resolver that actually produced this link.
	// It is safe diagnostic metadata, never a signed URL or account credential.
	URLSource string
	// Expires is the deadline through which callers may reuse this link. It can
	// be a conservative local deadline when a provider does not expose the
	// signed URL's exact expiry.
	Expires time.Time

	// PassThroughRedirects tells the online playback proxy to make the first
	// authenticated request itself, but relay an upstream 3xx Location to the
	// browser instead of following it on the server. Background consumers such
	// as fingerprinting and transcoding still follow redirects to read bytes.
	PassThroughRedirects bool

	// ClientRedirectSafe is an explicit per-link trust decision. It is true only
	// when URL alone authorizes the browser request and no secret Header values
	// are required after redirecting. This is intentionally not inferred from
	// drive Kind because redirect safety belongs to the returned link.
	ClientRedirectSafe bool
}

// ErrNotSupported 代表某家盘不支持某操作
var ErrNotSupported = errors.New("operation not supported by this drive")

// ErrGenerationStreamUnavailable means the optional optimized generation
// stream is unavailable for this file or the current scan. Callers may safely
// fall back to the original StreamURL without treating the drive as unhealthy.
var ErrGenerationStreamUnavailable = errors.New("generation stream unavailable")

// ProviderErrorKind lets callers distinguish rejected credentials from an
// unavailable provider without guessing from words such as "refresh token".
type ProviderErrorKind string

const (
	ProviderErrorAuth        ProviderErrorKind = "auth"
	ProviderErrorUnavailable ProviderErrorKind = "unavailable"
	ProviderErrorOther       ProviderErrorKind = "other"
)

type ProviderError struct {
	Kind ProviderErrorKind
	Err  error
}

func (e *ProviderError) Error() string { return e.Err.Error() }
func (e *ProviderError) Unwrap() error { return e.Err }

// RateLimitError 表示上游服务正在限流。RetryAfter 为 0 时由调用方选择默认冷却时间。
type RateLimitError struct {
	Provider   string
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "rate limited"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Provider != "" {
		return e.Provider + " rate limited"
	}
	return "rate limited"
}

func (e *RateLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func RateLimitRetryAfter(err error) (time.Duration, bool) {
	var rateLimit *RateLimitError
	if errors.As(err, &rateLimit) {
		return rateLimit.RetryAfter, true
	}
	return 0, false
}

// TextMentionsHTTPStatus only looks for explicit numeric HTTP status contexts
// in errors from tools that do not expose structured response metadata.
func TextMentionsHTTPStatus(text string, statuses ...int) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	for _, status := range statuses {
		if status <= 0 {
			continue
		}
		code := strconv.Itoa(status)
		if strings.HasPrefix(text, code+" ") ||
			strings.Contains(text, "status="+code) ||
			strings.Contains(text, "status: "+code) ||
			strings.Contains(text, "status "+code) ||
			strings.Contains(text, "status code "+code) ||
			strings.Contains(text, "http "+code) ||
			strings.Contains(text, "http status="+code) ||
			strings.Contains(text, "http status: "+code) ||
			strings.Contains(text, "http status "+code) ||
			strings.Contains(text, "server returned "+code) ||
			strings.Contains(text, "code="+code) ||
			strings.Contains(text, "code: "+code) ||
			strings.Contains(text, "error_code="+code) ||
			strings.Contains(text, "error_code: "+code) {
			return true
		}
	}
	return false
}

func ErrorMentionsHTTPStatus(err error, statuses ...int) bool {
	if err == nil {
		return false
	}
	return TextMentionsHTTPStatus(err.Error(), statuses...)
}
