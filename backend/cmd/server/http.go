package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/requestmeta"
)

const (
	frontendHashedAssetCacheControl      = "public, max-age=31536000, immutable"
	frontendUnversionedAssetCacheControl = "no-cache"
)

const (
	responseCompressionThreshold = 1024
	responseBrotliQuality        = 4
)

var dynamicCompressibleContentTypes = map[string]struct{}{
	"application/javascript":    {},
	"application/json":          {},
	"application/manifest+json": {},
	"application/x-javascript":  {},
	"image/svg+xml":             {},
	"text/css":                  {},
	"text/html":                 {},
	"text/javascript":           {},
	"text/plain":                {},
}

type capturedLogFormatter struct {
	access      *middleware.DefaultLogFormatter
	panicLogger *log.Logger
	logs        *applog.Store
}

func (f *capturedLogFormatter) NewLogEntry(r *http.Request) middleware.LogEntry {
	remote := requestmeta.ClientIP(r)
	requestForAccessLog := r.Clone(r.Context())
	requestForAccessLog.URL, _ = url.Parse(applog.RedactURL(r.URL.String()))
	requestForAccessLog.RequestURI = applog.RedactURL(r.URL.RequestURI())
	if remote != "" {
		requestForAccessLog.RemoteAddr = remote
	}
	return &capturedLogEntry{
		LogEntry:    f.access.NewLogEntry(requestForAccessLog),
		panicLogger: f.panicLogger,
		logs:        f.logs,
		request:     r,
		remote:      remote,
	}
}

type capturedLogEntry struct {
	middleware.LogEntry
	panicLogger *log.Logger
	logs        *applog.Store
	request     *http.Request
	remote      string
}

func (e *capturedLogEntry) Write(status, bytes int, header http.Header, elapsed time.Duration, extra any) {
	level := requestLogLevel(e.request, status, elapsed)
	if level == "" {
		return
	}
	// Keep the human-readable operational stream independently of file logging.
	e.LogEntry.Write(status, bytes, header, elapsed, extra)
	if e.logs == nil || e.request == nil {
		return
	}
	target := applog.RedactURL(e.request.URL.RequestURI())
	if target == "" {
		target = "/"
	}
	scheme := "http"
	if e.request.TLS != nil {
		scheme = "https"
	}
	requestID := middleware.GetReqID(e.request.Context())
	prefix := ""
	if requestID != "" {
		prefix = "[" + requestID + "] "
	}
	remote := e.remote
	if remote == "" {
		remote = e.request.RemoteAddr
	}
	message := fmt.Sprintf("%s%q from %s - %d %dB in %s",
		prefix,
		e.request.Method+" "+scheme+"://"+e.request.Host+target+" "+e.request.Proto,
		remote,
		status,
		bytes,
		elapsed,
	)
	_ = e.logs.AppendEntry(applog.Entry{
		Timestamp: time.Now(),
		Source:    applog.SourceHTTP,
		Level:     level,
		Method:    applog.Method(e.request.Method),
		Status:    status,
		Path:      target,
		Remote:    remote,
		Bytes:     bytes,
		Elapsed:   elapsed.String(),
		Fields:    applog.Fields{RequestID: requestID},
		Message:   message,
	})
}

func (e *capturedLogEntry) Panic(value any, stack []byte) {
	logger := e.panicLogger
	if logger == nil {
		logger = log.New(applog.Output(os.Stderr, nil), "", log.LstdFlags)
	}
	applog.LogEntry(logger, applog.Entry{
		Timestamp: time.Now(), Source: applog.SourceApplication, Level: applog.LevelError,
		Fields:  applog.ContextFields(e.request.Context()),
		Message: fmt.Sprintf("[http] panic: %v", value), Stack: string(stack),
		Method: applog.Method(e.request.Method), Path: e.request.URL.RequestURI(),
	})
}

// requestLogMiddleware writes a human-readable access line to stdout and a
// structured durable entry for the admin viewer. Routine queries are filtered
// after completion so failures and slow responses remain observable.
func requestLogMiddleware(accessLogger, panicLogger *log.Logger, logs *applog.Store) func(http.Handler) http.Handler {
	requestLogger := middleware.RequestLogger(&capturedLogFormatter{
		access: &middleware.DefaultLogFormatter{
			Logger:  accessLogger,
			NoColor: true,
		},
		panicLogger: panicLogger,
		logs:        logs,
	})
	return func(next http.Handler) http.Handler {
		logged := requestLogger(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := applog.NewID()
			ctx := context.WithValue(r.Context(), middleware.RequestIDKey, requestID)
			r = r.WithContext(applog.WithFields(ctx, applog.Fields{RequestID: requestID}))
			w.Header().Set("X-Request-ID", requestID)
			logged.ServeHTTP(w, r)
		})
	}
}

// corsMiddleware 返回一个 chi 中间件，按白名单匹配 Origin 决定是否回写
// CORS 响应头。
//
// 设计要点：
//   - 不再反射任意 Origin。Origin 必须出现在 allowedOrigins 中才会得到
//     Access-Control-Allow-Origin / Allow-Credentials 的"放行"响应头；
//     不在白名单的跨源请求拿不到这些头，浏览器会拒绝读响应内容。
//   - 同源请求（浏览器不发 Origin 头，或 Origin 等于自己）不需要 CORS 头，
//     直接放行。
//   - 始终带 Vary: Origin，避免反代缓存把 A Origin 的允许头喂给 B Origin。
//   - 对不在白名单的 OPTIONS 预检直接 403，避免被当成"放行"信号。
//
// allowedOrigins 由 config.Server.AllowedOrigins 注入；默认为空 = 完全
// 不允许跨源（最安全的默认值，同源部署不受影响）。
func corsMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
	allow := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		o = strings.TrimSpace(o)
		if o == "" || o == "*" {
			// 通配符在带 cookie 的 CORS 下没意义且危险，直接忽略
			continue
		}
		allow[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// 任何走过 CORS 检查的响应都要带 Vary: Origin，避免缓存污染。
			w.Header().Add("Vary", "Origin")

			isAllowedOrigin := false
			if origin != "" {
				_, isAllowedOrigin = allow[origin]
			}

			if isAllowedOrigin {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Max-Age", "600")
			}

			if r.Method == http.MethodOptions {
				// 预检请求：只对白名单 Origin 返回 204；否则 403 让浏览器把请求拦下来。
				// 同源场景一般不会触发预检（浏览器只在跨源 + 复杂请求时才发 OPTIONS）。
				if isAllowedOrigin {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if origin != "" {
					http.Error(w, "cors: origin not allowed", http.StatusForbidden)
					return
				}
				// 没带 Origin 的 OPTIONS 不是 CORS 预检（可能是健康检查工具），
				// 直接交给下游处理。
			}

			next.ServeHTTP(w, r)
		})
	}
}

// responseCompressionMiddleware compresses bounded textual responses after
// their content type and size are known. Media proxy routes and range requests
// bypass the wrapper entirely. An early explicit Flush commits the buffered
// response uncompressed; after compression has started, Flush propagates
// through both the encoder and the underlying writer.
func responseCompressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || r.Header.Get("Range") != "" || isMediaRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		writer := &thresholdCompressionWriter{
			ResponseWriter: w,
			encoding:       negotiateResponseEncoding(r.Header.Values("Accept-Encoding")),
			status:         http.StatusOK,
		}
		next.ServeHTTP(writer, r)
		_ = writer.finish()
	})
}

func isMediaRoute(requestPath string) bool {
	return requestPath == "/p" || strings.HasPrefix(requestPath, "/p/")
}

type thresholdCompressionWriter struct {
	http.ResponseWriter
	buffer      bytes.Buffer
	encoding    string
	status      int
	wroteHeader bool
	committed   bool
	target      io.Writer
	closer      io.Closer
}

func (w *thresholdCompressionWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *thresholdCompressionWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

func (w *thresholdCompressionWriter) Write(payload []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.committed {
		return w.target.Write(payload)
	}
	if !w.responseCanBeCompressed() {
		if err := w.commit(false); err != nil {
			return 0, err
		}
		return w.target.Write(payload)
	}
	written, err := w.buffer.Write(payload)
	if err != nil {
		return written, err
	}
	if w.buffer.Len() >= responseCompressionThreshold {
		if err := w.commit(true); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *thresholdCompressionWriter) Flush() {
	if !w.committed {
		_ = w.commit(false)
	}
	if flusher, ok := w.target.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *thresholdCompressionWriter) finish() error {
	if !w.committed {
		if err := w.commit(w.buffer.Len() >= responseCompressionThreshold); err != nil {
			return err
		}
	}
	if w.closer != nil {
		return w.closer.Close()
	}
	return nil
}

func (w *thresholdCompressionWriter) commit(compress bool) error {
	if w.committed {
		return nil
	}
	w.committed = true
	w.target = w.ResponseWriter

	if compress && w.responseCanBeCompressed() {
		addHeaderToken(w.Header(), "Vary", "Accept-Encoding")
		switch w.encoding {
		case "br":
			encoder := brotli.NewWriterLevel(w.ResponseWriter, responseBrotliQuality)
			w.target = encoder
			w.closer = encoder
			w.Header().Set("Content-Encoding", "br")
		case "gzip":
			encoder, err := gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
			if err != nil {
				return err
			}
			w.target = encoder
			w.closer = encoder
			w.Header().Set("Content-Encoding", "gzip")
		}
		if w.encoding != "" {
			w.Header().Del("Content-Length")
		}
	}

	w.ResponseWriter.WriteHeader(w.status)
	if w.buffer.Len() == 0 {
		return nil
	}
	_, err := io.Copy(w.target, &w.buffer)
	return err
}

func (w *thresholdCompressionWriter) responseCanBeCompressed() bool {
	if w.status == http.StatusNoContent || w.status == http.StatusNotModified || w.status == http.StatusPartialContent {
		return false
	}
	if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Content-Range") != "" {
		return false
	}
	contentType := w.Header().Get("Content-Type")
	if index := strings.IndexByte(contentType, ';'); index >= 0 {
		contentType = contentType[:index]
	}
	_, ok := dynamicCompressibleContentTypes[strings.ToLower(strings.TrimSpace(contentType))]
	return ok
}

func negotiateResponseEncoding(values []string) string {
	qualities := make(map[string]float64)
	specified := make(map[string]bool)
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			parts := strings.Split(item, ";")
			name := strings.ToLower(strings.TrimSpace(parts[0]))
			if name == "" {
				continue
			}
			quality := 1.0
			for _, parameter := range parts[1:] {
				key, raw, found := strings.Cut(strings.TrimSpace(parameter), "=")
				if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
					continue
				}
				parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					quality = 0
				} else {
					quality = parsed
				}
			}
			if !specified[name] || quality > qualities[name] {
				qualities[name] = quality
				specified[name] = true
			}
		}
	}
	qualityFor := func(name string) float64 {
		if specified[name] {
			return qualities[name]
		}
		if specified["*"] {
			return qualities["*"]
		}
		return 0
	}
	brotliQuality := qualityFor("br")
	gzipQuality := qualityFor("gzip")
	if brotliQuality <= 0 && gzipQuality <= 0 {
		return ""
	}
	if brotliQuality >= gzipQuality {
		return "br"
	}
	return "gzip"
}

func addHeaderToken(header http.Header, name, token string) {
	for _, value := range header.Values(name) {
		for _, existing := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(existing), token) {
				return
			}
		}
	}
	header.Add(name, token)
}

func mountFrontend(r chi.Router) {
	dir, ok := resolveFrontendDir()
	if !ok {
		return
	}
	log.Printf("serving frontend from %s", dir)
	r.NotFound(frontendHandler(dir))
}

func resolveFrontendDir() (string, bool) {
	candidates := []string{}
	if dir := strings.TrimSpace(os.Getenv("VIDEO_FRONTEND_DIR")); dir != "" {
		candidates = append(candidates, dir)
	} else {
		candidates = append(candidates, "./dist", "../dist")
	}
	for _, dir := range candidates {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		indexPath := filepath.Join(dir, "index.html")
		if st, err := os.Stat(indexPath); err == nil && !st.IsDir() {
			return dir, true
		}
	}
	return "", false
}

func frontendHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if isBackendRoute(r.URL.Path) {
			http.NotFound(w, r)
			return
		}

		cleanPath := path.Clean("/" + r.URL.Path)
		rel := strings.TrimPrefix(cleanPath, "/")
		if rel != "" && rel != "." {
			name := filepath.FromSlash(rel)
			assetPath := filepath.Join(dir, name)
			f, encoding, err := openFrontendAsset(r, assetPath, cleanPath)
			if err == nil {
				defer f.Close()
				if st, statErr := f.Stat(); statErr == nil && !st.IsDir() {
					if strings.HasPrefix(cleanPath, "/assets/") {
						w.Header().Set("Cache-Control", frontendHashedAssetCacheControl)
						addHeaderToken(w.Header(), "Vary", "Accept-Encoding")
					} else {
						// Stable URLs such as the PWA manifest must revalidate so
						// existing installations receive changes after an upgrade.
						w.Header().Set("Cache-Control", frontendUnversionedAssetCacheControl)
					}
					if encoding != "" {
						w.Header().Set("Content-Encoding", encoding)
						if contentType := mime.TypeByExtension(filepath.Ext(name)); contentType != "" {
							w.Header().Set("Content-Type", contentType)
						}
					}
					http.ServeContent(w, r, filepath.Base(name), st.ModTime(), f)
					return
				}
			}
			if filepath.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}

		// index.html names hashed assets that are removed on the next build. It
		// may be stored locally, but every navigation must revalidate it so an
		// old document never points at files that no longer exist.
		w.Header().Set("Cache-Control", frontendUnversionedAssetCacheControl)
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	}
}

func openFrontendAsset(r *http.Request, assetPath, requestPath string) (*os.File, string, error) {
	if strings.HasPrefix(requestPath, "/assets/") && r.Header.Get("Range") == "" {
		if encoding := negotiateResponseEncoding(r.Header.Values("Accept-Encoding")); encoding != "" {
			extension := "." + encoding
			if encoding == "gzip" {
				extension = ".gz"
			}
			if file, err := os.Open(assetPath + extension); err == nil {
				return file, encoding, nil
			}
		}
	}
	file, err := os.Open(assetPath)
	return file, "", err
}

func isBackendRoute(p string) bool {
	return p == "/api" ||
		strings.HasPrefix(p, "/api/") ||
		p == "/admin/api" ||
		strings.HasPrefix(p, "/admin/api/") ||
		p == "/p" ||
		strings.HasPrefix(p, "/p/") ||
		p == "/peer" ||
		strings.HasPrefix(p, "/peer/")
}

func parseBoolDefault(raw string, def bool) bool {
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return v
}

func parseIntDefault(raw string, def int) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}
