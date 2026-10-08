package scriptcrawler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/dedupe"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/persistence"
	"github.com/video-site/backend/internal/tasklimit"
	"golang.org/x/net/proxy"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultUserAgent = "Mozilla/5.0 (compatible; video-site-scriptcrawler/3.0)"

type ImportOutcome string

const (
	ImportAdded     ImportOutcome = "added"
	ImportKnown     ImportOutcome = "known"
	ImportDuplicate ImportOutcome = "duplicate"
	ImportFailed    ImportOutcome = "failed"
)

type ImporterConfig struct {
	Driver                                                                *Driver
	Catalog                                                               *catalog.Catalog
	FingerprintLimiter                                                    *tasklimit.Limiter
	CrawlerName, FFmpegPath, FFprobePath, CommonThumbDir, LocalPreviewDir string
	HTTPClient                                                            *http.Client
	DownloadTimeout                                                       time.Duration
}
type Importer struct {
	cfg         ImporterConfig
	hlsCapsOnce sync.Once
	hlsCaps     ffmpegHLSCapabilities
}

// Import uses downloadCtx only to acquire the video. Processing a complete
// video belongs to the task lifetime, including generation and upload, so a
// request to stop crawling cannot interrupt it. Hard cancellation still can.
func (c *Importer) Import(downloadCtx, ctx context.Context, item Item) (ImportOutcome, error) {
	if err := validateItem(item, Candidate{DiscoveryKey: item.DiscoveryKey}); err != nil {
		return ImportFailed, err
	}
	sourceID := item.SourceID
	videoID := importVideoID(c.cfg.Driver.ID(), sourceID)
	known, err := c.cfg.Catalog.KnownCrawlerCandidates(ctx, c.cfg.Driver.ID(), []catalog.CrawlerIdentity{{DiscoveryKey: item.DiscoveryKey, SourceID: sourceID}})
	if err != nil {
		return ImportFailed, err
	}
	if known[item.DiscoveryKey] {
		if err := c.cfg.Catalog.BindCrawlerDiscovery(ctx, c.cfg.Driver.ID(), item.DiscoveryKey, sourceID); err != nil {
			return ImportFailed, err
		}
		return ImportKnown, nil
	}
	videoExt := detectVideoExt(item.Media.URL)
	fileStem := mediaFileStem(sourceID)
	videoFile := fileStem + videoExt
	videoPath, err := c.cfg.Driver.VideoPath(videoFile)
	if err != nil {
		return ImportFailed, err
	}
	size, err := c.materializeMedia(downloadCtx, item.Media, videoPath)
	if err != nil {
		return ImportFailed, fmt.Errorf("video: %w", err)
	}
	if err := c.validateDownloadedVideo(ctx, videoPath); err != nil {
		_ = os.Remove(videoPath)
		return ImportFailed, fmt.Errorf("video invalid: %w", err)
	}

	now := time.Now()
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = sourceID
	}
	author := strings.TrimSpace(item.Author)
	// 标签策略：
	//   1. 脚本返回的 tags 只挂已存在的标签，不自动创建新标签 → source=crawler；
	//   2. 规则引擎按标题/文件名/作者匹配已有标签池 → source=auto；
	//   3. 视频成功入库后才确保并强制挂载爬虫名标签（不受人工锁定影响）。
	var tagAssignments []catalog.TagAssignment
	tagLabelSeen := map[string]bool{}
	crawlerTagLabel := ""
	appendAssignment := func(a catalog.TagAssignment) {
		key := strings.ToLower(strings.TrimSpace(a.Label))
		if key == "" || tagLabelSeen[key] {
			return
		}
		tagLabelSeen[key] = true
		tagAssignments = append(tagAssignments, a)
	}
	for _, scriptTag := range cleanStringList(item.Tags) {
		label, ok, err := c.cfg.Catalog.LookupTagLabel(ctx, scriptTag)
		if err != nil || !ok {
			continue
		}
		appendAssignment(catalog.TagAssignment{Label: label, Source: "crawler", Evidence: "脚本标签"})
	}
	if matched, err := c.cfg.Catalog.MatchTagAssignments(ctx, title, videoFile, author, ""); err == nil {
		for _, a := range matched {
			appendAssignment(a)
		}
	}
	crawlerTagLabel = c.crawlerTagName()
	v := &catalog.Video{
		ID:              videoID,
		DriveID:         c.cfg.Driver.ID(),
		FileID:          videoFile,
		FileName:        videoFile,
		Title:           title,
		Author:          author,
		DurationSeconds: item.DurationSeconds,
		Size:            size,
		Ext:             strings.TrimPrefix(videoExt, "."),
		PreviewStatus:   "pending",
		PublishedAt:     now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	sampled, err := fingerprint.Compute(ctx, c.cfg.Driver, v, fingerprint.Config{Limiter: c.cfg.FingerprintLimiter}, c.cfg.HTTPClient)
	if err != nil {
		_ = os.Remove(videoPath)
		return ImportFailed, fmt.Errorf("fingerprint: %w", err)
	}
	v.SampledSHA256 = sampled
	v.FingerprintStatus = "ready"
	if duplicate, err := c.cfg.Catalog.FindVideoBySampledFingerprint(ctx, v); err == nil && duplicate != nil {
		_ = os.Remove(videoPath)
		evidence := dedupe.NewEvidence(dedupe.ReasonSampledSHA256, duplicate.ID, duplicate.ID, "existing_match")
		if err := c.recordSkippedDuplicate(ctx, v, duplicate, sourceID, item.DiscoveryKey, evidence); err != nil {
			return ImportFailed, fmt.Errorf("record fingerprint duplicate: %w", err)
		}
		return ImportDuplicate, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = os.Remove(videoPath)
		return ImportFailed, fmt.Errorf("duplicate lookup: %w", err)
	}

	thumbReady := false
	thumbPath := ""
	commonThumbPath := ""
	if item.Thumbnail != nil {
		thumbFile := fileStem + detectThumbExt(item.Thumbnail.URL)
		thumbPath, err = c.cfg.Driver.ThumbPath(thumbFile)
		if err == nil {
			if _, err := c.materializeMedia(ctx, *item.Thumbnail, thumbPath); err != nil {
				log.Printf("[scriptcrawler] drive=%s source_id=%s thumbnail failed: %v", c.cfg.Driver.ID(), sourceID, err)
			} else if c.cfg.CommonThumbDir != "" {
				if err := os.MkdirAll(c.cfg.CommonThumbDir, 0o755); err != nil {
					log.Printf("[scriptcrawler] drive=%s common thumbs mkdir: %v", c.cfg.Driver.ID(), err)
				} else {
					dst := mediaasset.ThumbnailPathInDir(c.cfg.CommonThumbDir, videoID)
					if err := mediaasset.NormalizeThumbnailJPEG(thumbPath, dst); err != nil {
						log.Printf("[scriptcrawler] drive=%s source_id=%s normalize thumbnail: %v", c.cfg.Driver.ID(), sourceID, err)
					} else {
						commonThumbPath = dst
						thumbReady = true
					}
				}
			}
		}
	}
	if thumbReady {
		v.ThumbnailURL = "/p/thumb/" + v.ID
	}
	duplicate, err := c.findNearDuplicateVideo(ctx, v, commonThumbPath, videoPath)
	if err != nil {
		_ = os.Remove(videoPath)
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
		if commonThumbPath != "" {
			_ = os.Remove(commonThumbPath)
		}
		return ImportFailed, fmt.Errorf("near duplicate lookup: %w", err)
	}
	// Media and thumbnail downloads above are written through .part files.
	// Coordinate only the final file/catalog cleanup and publication.
	if err := persistence.RLockContext(ctx); err != nil {
		_ = os.Remove(videoPath)
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
		if commonThumbPath != "" {
			_ = os.Remove(commonThumbPath)
		}
		return ImportFailed, err
	}
	defer persistence.RUnlock()
	if duplicate != nil && duplicate.video != nil {
		_ = os.Remove(videoPath)
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
		if commonThumbPath != "" {
			_ = os.Remove(commonThumbPath)
		}
		evidence := duplicate.evidence(duplicate.video.ID, duplicate.video.ID, "keep_existing")
		if err := c.recordSkippedDuplicate(ctx, v, duplicate.video, sourceID, item.DiscoveryKey, evidence); err != nil {
			return ImportFailed, fmt.Errorf("record near duplicate: %w", err)
		}
		return ImportDuplicate, nil
	}
	if err := c.cfg.Catalog.ImportCrawlerVideo(ctx, v, sourceID, item.DiscoveryKey); err != nil {
		_ = os.Remove(videoPath)
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
		if commonThumbPath != "" {
			_ = os.Remove(commonThumbPath)
		}
		return ImportFailed, err
	}
	if len(tagAssignments) > 0 {
		if _, err := c.cfg.Catalog.AddVideoTagAssignments(ctx, v.ID, tagAssignments); err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s attach tags: %v", c.cfg.Driver.ID(), sourceID, err)
		} else {
			for _, a := range tagAssignments {
				v.Tags = append(v.Tags, a.Label)
			}
		}
	}
	if crawlerTagLabel != "" {
		if _, err := c.cfg.Catalog.EnsureCrawlerTagForVideo(ctx, v.ID, crawlerTagLabel); err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s attach crawler tag %q: %v", c.cfg.Driver.ID(), sourceID, crawlerTagLabel, err)
		} else {
			seen := false
			for _, label := range v.Tags {
				if strings.EqualFold(label, crawlerTagLabel) {
					seen = true
					break
				}
			}
			if !seen {
				v.Tags = append(v.Tags, crawlerTagLabel)
			}
		}
	}
	return ImportAdded, nil
}

// RestoreRequestedVideos scans the crawler's retained local video directory
// after the crawl/generation/upload pipeline has finished. Only tombstones that
// the user explicitly removed from the blacklist are eligible.
func (c *Importer) RestoreRequestedVideos(ctx context.Context) (int, error) {
	if c == nil || c.cfg.Driver == nil || c.cfg.Catalog == nil {
		return 0, errors.New("scriptcrawler: restore dependencies not set")
	}
	if err := c.cfg.Driver.Init(ctx); err != nil {
		return 0, fmt.Errorf("scriptcrawler: restore driver init: %w", err)
	}
	requests, err := c.cfg.Catalog.ListCrawlerRestoreRequests(ctx, c.cfg.Driver.ID())
	if err != nil || len(requests) == 0 {
		return 0, err
	}
	entries, err := c.cfg.Driver.List(ctx, c.cfg.Driver.RootID())
	if err != nil {
		return 0, fmt.Errorf("scriptcrawler: scan retained videos: %w", err)
	}
	files := make(map[string]struct {
		size    int64
		modTime time.Time
	}, len(entries))
	for _, entry := range entries {
		if entry.IsDir || entry.Size <= 0 {
			continue
		}
		files[entry.ID] = struct {
			size    int64
			modTime time.Time
		}{size: entry.Size, modTime: entry.ModTime}
	}

	restored := 0
	var restoreErrors []error
	for _, request := range requests {
		if err := ctx.Err(); err != nil {
			return restored, err
		}
		fileID := strings.TrimSpace(request.FileID)
		file, ok := files[fileID]
		if !ok {
			continue
		}

		video := &catalog.Video{}
		if request.Video != nil {
			copy := *request.Video
			video = &copy
		}
		video.ID = request.ID
		video.DriveID = c.cfg.Driver.ID()
		video.FileID = fileID
		if strings.TrimSpace(video.FileName) == "" {
			video.FileName = strings.TrimSpace(request.FileName)
		}
		if strings.TrimSpace(video.FileName) == "" {
			video.FileName = fileID
		}
		if strings.TrimSpace(video.Title) == "" {
			video.Title = strings.TrimSuffix(video.FileName, filepath.Ext(video.FileName))
		}
		video.Ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(fileID)), ".")
		if request.Size != file.size {
			video.SampledSHA256 = ""
			video.FingerprintStatus = "pending"
			video.FingerprintError = ""
		}
		video.Size = file.size
		video.ThumbnailURL = ""
		video.PreviewFileID = ""
		video.PreviewLocal = ""
		video.PreviewStatus = "pending"
		if video.CreatedAt.IsZero() {
			video.CreatedAt = file.modTime
		}
		// Crawler timestamps are backend-owned. Restoring an older crawler
		// tombstone must not reintroduce a source-supplied publication date.
		video.PublishedAt = video.CreatedAt
		if c.restoreCrawlerThumbnail(video, fileID) {
			video.ThumbnailURL = "/p/thumb/" + video.ID
		}

		sourceID, sourceErr := c.cfg.Catalog.CrawlerSourceForVideo(ctx, c.cfg.Driver.ID(), request.ID)
		if sourceErr != nil && !errors.Is(sourceErr, sql.ErrNoRows) {
			restoreErrors = append(restoreErrors, sourceErr)
			continue
		}
		if sourceID == "" {
			sourceID = strings.TrimPrefix(request.ID, BuildVideoID(c.cfg.Driver.ID(), ""))
			if strings.HasPrefix(sourceID, "v3~") {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore %s: explicit source identity missing", request.ID))
				continue
			}
		}
		if err := c.cfg.Catalog.CompleteCrawlerRestore(ctx, video, sourceID); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("complete restore %s: %w", request.ID, err))
			continue
		}
		restored++
		log.Printf("[scriptcrawler] drive=%s restored retained video=%s file=%s size=%d", c.cfg.Driver.ID(), request.ID, fileID, file.size)
	}
	return restored, errors.Join(restoreErrors...)
}

func (c *Importer) restoreCrawlerThumbnail(video *catalog.Video, fileID string) bool {
	if video == nil || strings.TrimSpace(c.cfg.CommonThumbDir) == "" {
		return false
	}
	stem := strings.TrimSuffix(fileID, filepath.Ext(fileID))
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		source, err := c.cfg.Driver.ThumbPath(stem + ext)
		if err != nil {
			continue
		}
		info, err := os.Stat(source)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		if err := os.MkdirAll(c.cfg.CommonThumbDir, 0o755); err != nil {
			return false
		}
		if err := mediaasset.NormalizeThumbnailJPEG(source, mediaasset.ThumbnailPathInDir(c.cfg.CommonThumbDir, video.ID)); err != nil {
			log.Printf("[scriptcrawler] drive=%s restore thumbnail video=%s: %v", c.cfg.Driver.ID(), video.ID, err)
			return false
		}
		return true
	}
	return false
}

func (c *Importer) materializeMedia(ctx context.Context, ref MediaRef, dst string) (int64, error) {
	attemptCtx, cancel := c.downloadAttemptContext(ctx)
	defer cancel()
	size, err := c.downloadAtomic(attemptCtx, ref, dst)
	if err != nil && errors.Is(context.Cause(attemptCtx), errOperationTimeout) {
		err = fmt.Errorf("download: %w: %w", errOperationTimeout, err)
	}
	return size, err
}

func (c *Importer) validateDownloadedVideo(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
		path,
	}
	out, err := exec.CommandContext(ctx, c.cfg.FFprobePath, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffprobe: %s", msg)
	}
	if !strings.Contains(strings.ToLower(string(out)), "video") {
		return errors.New("ffprobe: no video stream")
	}
	return nil
}

func (c *Importer) downloadAttemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.cfg.DownloadTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, c.cfg.DownloadTimeout, errOperationTimeout)
}

func (c *Importer) downloadAtomic(ctx context.Context, ref MediaRef, dst string) (int64, error) {
	src := strings.TrimSpace(ref.URL)
	if src == "" {
		return 0, errors.New("empty url")
	}
	if _, err := url.Parse(src); err != nil {
		return 0, fmt.Errorf("parse url: %w", err)
	}
	if looksLikeHLSURL(src) {
		return c.downloadHLSAtomic(ctx, ref, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	for k, v := range ref.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, closeErr
	}
	if written <= 0 {
		_ = os.Remove(tmp)
		return 0, errors.New("empty body")
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return written, nil
}

func (c *Importer) downloadHLSAtomic(ctx context.Context, ref MediaRef, dst string) (int64, error) {
	ref.URL = strings.TrimSpace(ref.URL)
	input, closeRelay, relay, err := startHLSRelay(ctx, c.cfg.HTTPClient, ref)
	if err != nil {
		return 0, err
	}
	defer closeRelay()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp := dst + ".part"
	_ = os.Remove(tmp)
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
	}
	args = append(args, c.ffmpegHLSInputOptions(ctx)...)
	args = append(args,
		"-i", input,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		"-f", "mp4",
		tmp,
	)
	cmd := exec.CommandContext(ctx, c.cfg.FFmpegPath, args...)
	// Source requests already use the Go client; ambient proxies must not
	// divert FFmpeg's loopback requests to another server.
	cmd.Env = append(os.Environ(), "http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "all_proxy=", "ALL_PROXY=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		if requestErr := relay.error(); requestErr != nil && ctx.Err() == nil {
			return 0, fmt.Errorf("HLS download: %s", redactMediaURLs(requestErr.Error()))
		}
		return 0, mediaCommandError("ffmpeg hls", err, out)
	}
	info, err := os.Stat(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if info.IsDir() || info.Size() <= 0 {
		_ = os.Remove(tmp)
		return 0, errors.New("empty hls output")
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return info.Size(), nil
}

func looksLikeHLSURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && u != nil && strings.EqualFold(path.Ext(u.Path), ".m3u8") {
		return true
	}
	return strings.Contains(strings.ToLower(raw), ".m3u8")
}

func mediaRequestHeaders(ref MediaRef) http.Header {
	headers := make(http.Header)
	headers.Set("User-Agent", defaultUserAgent)
	for k, v := range ref.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		headers.Set(k, v)
	}
	return headers
}

func mediaCommandError(tool string, err error, output []byte) error {
	msg := strings.TrimSpace(redactMediaURLs(string(output)))
	if msg == "" {
		return fmt.Errorf("%s: %w", tool, err)
	}
	return fmt.Errorf("%s: %w: %s", tool, err, msg)
}

func redactMediaURLs(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			suffix := ""
			for len(field) > 0 {
				last := field[len(field)-1]
				if last != '.' && last != ',' && last != ';' && last != ')' {
					break
				}
				suffix = string(last) + suffix
				field = field[:len(field)-1]
			}
			fields[i] = "https://<redacted>" + suffix
		}
	}
	return strings.Join(fields, " ")
}

func configureExplicitProxy(transport *http.Transport, raw string) error {
	proxyURL := strings.TrimSpace(raw)
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid proxy URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
		transport.DialContext = nil
		return nil
	case "socks5", "socks5h":
		dialContext, err := socksProxyDialContext(u)
		if err != nil {
			return err
		}
		transport.Proxy = nil
		transport.DialContext = dialContext
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

func socksProxyDialContext(proxyURL *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	var auth *proxy.Auth
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		auth = &proxy.Auth{User: username, Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, &net.Dialer{Timeout: 60 * time.Second})
	if err != nil {
		return nil, err
	}
	remoteDNS := strings.EqualFold(proxyURL.Scheme, "socks5h")
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		target := addr
		if !remoteDNS {
			resolved, err := resolveSocksTarget(ctx, addr)
			if err != nil {
				return nil, err
			}
			target = resolved
		}
		if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
			return ctxDialer.DialContext(ctx, network, target)
		}
		type result struct {
			conn net.Conn
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			conn, err := dialer.Dial(network, target)
			ch <- result{conn: conn, err: err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-ch:
			return res.conn, res.err
		}
	}, nil
}

func resolveSocksTarget(ctx context.Context, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return addr, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	for _, addr := range ips {
		if ip4 := addr.IP.To4(); ip4 != nil {
			return net.JoinHostPort(ip4.String(), port), nil
		}
	}
	if len(ips) > 0 && ips[0].IP != nil {
		return net.JoinHostPort(ips[0].IP.String(), port), nil
	}
	return "", fmt.Errorf("resolve %s: no address", host)
}

func (c *Importer) crawlerTagName() string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(c.cfg.CrawlerName); v != "" {
		return v
	}
	if c.cfg.Driver != nil {
		return strings.TrimSpace(c.cfg.Driver.ID())
	}
	return ""
}

// BuildVideoID describes the historical crawler namespace. New imports use
// opaque IDs below and persist their source identity separately in Catalog.
func BuildVideoID(driveID, sourceID string) string { return Kind + "-" + driveID + "-" + sourceID }
func importVideoID(driveID, sourceID string) string {
	return BuildVideoID(driveID, mediaFileStem(sourceID))
}
func mediaFileStem(sourceID string) string {
	sum := sha256.Sum256([]byte(sourceID))
	return "v3~" + hex.EncodeToString(sum[:])
}

func detectVideoExt(rawURL string) string {
	if ext := mediaExt(rawURL, true); ext != "" {
		return ext
	}
	return ".mp4"
}

func detectThumbExt(rawURL string) string {
	if ext := mediaExt(rawURL, false); ext != "" {
		return ext
	}
	return ".jpg"
}

func mediaExt(raw string, video bool) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	value := raw
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u != nil && u.Path != "" {
		value = u.Path
	}
	ext := strings.ToLower(path.Ext(value))
	if video {
		switch ext {
		case ".mp4", ".webm", ".mkv", ".mov", ".m4v", ".flv", ".avi":
			return ext
		}
		return ""
	}
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return ext
	}
	return ""
}

func cleanStringList(in []string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}
