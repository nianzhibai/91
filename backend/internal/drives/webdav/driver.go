// Package webdav implements a standard HTTP/WebDAV-backed Drive.
package webdav

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const (
	Kind                     = "webdav"
	maxPropfindResponseBytes = 32 << 20
	maxErrorResponseBytes    = 4 << 10
	metadataTimeout          = 30 * time.Second
)

var propfindBody = []byte(`<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:displayname />
    <d:resourcetype />
    <d:getcontentlength />
    <d:getlastmodified />
    <d:getcontenttype />
    <d:getetag />
  </d:prop>
</d:propfind>`)

type Config struct {
	ID       string
	BaseURL  string
	Username string
	Password string
	RootID   string
}

type Driver struct {
	id       string
	rootID   string
	username string
	password string

	baseURL   *url.URL
	basePath  string
	configErr error
	metadata  *http.Client
	transfer  *http.Client
}

func New(c Config) *Driver {
	rootID, rootErr := normalizeRemotePath(c.RootID)
	baseURL, basePath, baseErr := parseBaseURL(c.BaseURL)
	configErr := errors.Join(rootErr, baseErr)
	return &Driver{
		id:        strings.TrimSpace(c.ID),
		rootID:    rootID,
		username:  strings.TrimSpace(c.Username),
		password:  c.Password,
		baseURL:   baseURL,
		basePath:  basePath,
		configErr: configErr,
		metadata:  newHTTPClient(metadataTimeout),
		transfer:  newHTTPClient(0),
	}
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: scopedproxy.NewTransport(nil),
		// WebDAV methods must not be rewritten to GET by a 301/302 response, and
		// credentials must never be forwarded to a different origin implicitly.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func parseBaseURL(raw string) (*url.URL, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", errors.New("webdav init: base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("webdav init: parse base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("webdav init: base_url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, "", errors.New("webdav init: base_url host is required")
	}
	if u.User != nil {
		return nil, "", errors.New("webdav init: put username and password in separate credential fields")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, "", errors.New("webdav init: base_url must not contain a query or fragment")
	}
	u.Path = path.Clean("/" + strings.TrimSpace(u.Path))
	u.RawPath = ""
	basePath := strings.TrimSuffix(u.Path, "/")
	if basePath == "" {
		basePath = "/"
	}
	return u, basePath, nil
}

func normalizeRemotePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/", nil
	}
	if strings.ContainsRune(raw, '\x00') {
		return "/", errors.New("webdav: path contains NUL")
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "/", fmt.Errorf("webdav: path escapes root: %q", raw)
		}
	}
	return path.Clean(raw), nil
}

func (d *Driver) Kind() string { return Kind }

func (d *Driver) ID() string { return d.id }

func (d *Driver) RootID() string { return d.rootID }

func (d *Driver) Init(ctx context.Context) error {
	if d.configErr != nil {
		return d.configErr
	}
	entry, err := d.statAt(ctx, d.rootID, true)
	if err != nil {
		return fmt.Errorf("webdav init: %w", err)
	}
	if !entry.IsDir {
		return fmt.Errorf("webdav init: root path %q is not a directory", d.rootID)
	}
	return nil
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	dirID, err := d.resolveID(dirID)
	if err != nil {
		return nil, err
	}
	responses, requestURL, err := d.propfind(ctx, dirID, true, "1")
	if err != nil {
		return nil, fmt.Errorf("webdav list %q: %w", dirID, err)
	}

	seen := make(map[string]struct{}, len(responses))
	entries := make([]drives.Entry, 0, len(responses))
	for _, response := range responses {
		entry, ok, err := d.entryFromResponse(response, requestURL)
		if err != nil {
			return nil, fmt.Errorf("webdav list %q: %w", dirID, err)
		}
		if !ok || entry.ID == dirID || path.Dir(entry.ID) != dirID {
			continue
		}
		if !pathWithinRoot(entry.ID, d.rootID) {
			return nil, fmt.Errorf("webdav list %q: response path %q escapes configured root", dirID, entry.ID)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			continue
		}
		seen[entry.ID] = struct{}{}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	fileID, err := d.resolveID(fileID)
	if err != nil {
		return nil, err
	}
	return d.statAt(ctx, fileID, fileID == d.rootID)
}

// DirectoryName resolves the scan root name through a direct metadata lookup.
func (d *Driver) DirectoryName(ctx context.Context, dirID string) (string, error) {
	entry, err := d.Stat(ctx, dirID)
	if err != nil {
		return "", err
	}
	if entry == nil || !entry.IsDir {
		return "", drives.ErrNotSupported
	}
	return entry.Name, nil
}

func (d *Driver) statAt(ctx context.Context, remotePath string, directoryHint bool) (*drives.Entry, error) {
	responses, requestURL, err := d.propfind(ctx, remotePath, directoryHint, "0")
	if err != nil {
		return nil, err
	}
	for _, response := range responses {
		entry, ok, err := d.entryFromResponse(response, requestURL)
		if err != nil {
			return nil, err
		}
		if ok && entry.ID == remotePath {
			return &entry, nil
		}
	}
	return nil, fmt.Errorf("%w: webdav PROPFIND returned no metadata for %q", os.ErrNotExist, remotePath)
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fileID, err := d.resolveID(fileID)
	if err != nil {
		return nil, err
	}
	if fileID == d.rootID {
		return nil, errors.New("webdav stream: root path is not a file")
	}
	u, err := d.urlFor(fileID, false)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	d.addRequestHeaders(headers)
	headers.Set("Accept-Encoding", "identity")
	return &drives.StreamLink{
		URL:                  u,
		Headers:              headers,
		Expires:              time.Now().Add(24 * time.Hour),
		PassThroughRedirects: true,
	}, nil
}

func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	if r == nil {
		return "", errors.New("webdav upload: body is required")
	}
	if size < 0 {
		return "", fmt.Errorf("webdav upload: invalid size %d", size)
	}
	parentID, err := d.resolveID(parentID)
	if err != nil {
		return "", err
	}
	name, err = validateChildName(name)
	if err != nil {
		return "", fmt.Errorf("webdav upload: %w", err)
	}
	fileID := path.Join(parentID, name)
	if _, err := d.statAt(ctx, fileID, false); err == nil {
		return "", fmt.Errorf("webdav upload: file already exists: %s", fileID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("webdav upload: check destination: %w", err)
	}

	req, err := d.newRequest(ctx, http.MethodPut, fileID, false, r)
	if err != nil {
		return "", fmt.Errorf("webdav upload: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("If-None-Match", "*")
	resp, err := d.transfer.Do(req)
	if err != nil {
		return "", fmt.Errorf("webdav upload: %w", err)
	}
	defer resp.Body.Close()
	if !statusAllowed(resp.StatusCode, http.StatusOK, http.StatusCreated, http.StatusNoContent) {
		return "", d.responseError(http.MethodPut, resp)
	}
	drainResponse(resp.Body)
	return fileID, nil
}

func (d *Driver) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	parts, err := relativePathParts(pathFromRoot)
	if err != nil {
		return "", fmt.Errorf("webdav ensure dir: %w", err)
	}
	current := d.rootID
	for _, part := range parts {
		next := path.Join(current, part)
		entry, statErr := d.statAt(ctx, next, true)
		switch {
		case statErr == nil && entry.IsDir:
			current = next
			continue
		case statErr == nil:
			return "", fmt.Errorf("webdav ensure dir: %q exists and is not a directory", next)
		case !errors.Is(statErr, os.ErrNotExist):
			return "", fmt.Errorf("webdav ensure dir: stat %q: %w", next, statErr)
		}

		if err := d.simpleRequest(ctx, d.metadata, "MKCOL", next, true, nil,
			http.StatusCreated, http.StatusOK, http.StatusNoContent); err != nil {
			// Another creator may have won the race. Confirm a 405 target before
			// treating it as a real failure.
			if statusCode(err) != http.StatusMethodNotAllowed {
				return "", fmt.Errorf("webdav ensure dir: create %q: %w", next, err)
			}
			entry, statErr = d.statAt(ctx, next, true)
			if statErr != nil || !entry.IsDir {
				return "", fmt.Errorf("webdav ensure dir: create %q: %w", next, err)
			}
		}
		current = next
	}
	return current, nil
}

// Rename uses the standard WebDAV MOVE method. File IDs are paths for this
// driver, so callers that persist an ID after a rename must replace it with the
// destination path. The current crawler upload flow does not rename after PUT,
// but it requires this capability from every upload target.
func (d *Driver) Rename(ctx context.Context, fileID, newName string) error {
	fileID, err := d.resolveID(fileID)
	if err != nil {
		return err
	}
	if fileID == d.rootID {
		return errors.New("webdav rename: refusing to rename configured root")
	}
	newName, err = validateChildName(newName)
	if err != nil {
		return fmt.Errorf("webdav rename: %w", err)
	}
	entry, err := d.statAt(ctx, fileID, false)
	if err != nil {
		return fmt.Errorf("webdav rename: %w", err)
	}
	destinationID := path.Join(path.Dir(fileID), newName)
	if destinationID == fileID {
		return nil
	}
	destinationURL, err := d.urlFor(destinationID, entry.IsDir)
	if err != nil {
		return fmt.Errorf("webdav rename: %w", err)
	}
	headers := make(http.Header)
	headers.Set("Destination", destinationURL)
	headers.Set("Overwrite", "F")
	if err := d.simpleRequest(ctx, d.metadata, "MOVE", fileID, entry.IsDir, headers,
		http.StatusCreated, http.StatusNoContent); err != nil {
		return fmt.Errorf("webdav rename: %w", err)
	}
	return nil
}

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	fileID, err := d.resolveID(fileID)
	if err != nil {
		return err
	}
	if fileID == d.rootID {
		return errors.New("webdav remove: refusing to remove configured root")
	}
	entry, err := d.statAt(ctx, fileID, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("webdav remove: %w", err)
	}
	if entry.IsDir {
		return errors.New("webdav remove: refusing to remove directory")
	}
	if err := d.simpleRequest(ctx, d.metadata, http.MethodDelete, fileID, false, nil,
		http.StatusOK, http.StatusAccepted, http.StatusNoContent); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("webdav remove: %w", err)
	}
	return nil
}

func (d *Driver) propfind(ctx context.Context, remotePath string, directoryHint bool, depth string) ([]davResponse, string, error) {
	req, err := d.newRequest(ctx, "PROPFIND", remotePath, directoryHint, bytes.NewReader(propfindBody))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := d.metadata.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, "", d.responseError("PROPFIND", resp)
	}
	data, err := readLimited(resp.Body, maxPropfindResponseBytes)
	if err != nil {
		return nil, "", fmt.Errorf("decode multistatus: %w", err)
	}
	var result davMultistatus
	if err := xml.Unmarshal(data, &result); err != nil {
		return nil, "", fmt.Errorf("decode multistatus XML: %w", err)
	}
	if len(result.Responses) == 0 {
		return nil, "", errors.New("empty multistatus response")
	}
	return result.Responses, req.URL.String(), nil
}

func (d *Driver) simpleRequest(ctx context.Context, client *http.Client, method, remotePath string, directoryHint bool, headers http.Header, allowed ...int) error {
	req, err := d.newRequest(ctx, method, remotePath, directoryHint, nil)
	if err != nil {
		return err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !statusAllowed(resp.StatusCode, allowed...) {
		return d.responseError(method, resp)
	}
	drainResponse(resp.Body)
	return nil
}

func (d *Driver) newRequest(ctx context.Context, method, remotePath string, directoryHint bool, body io.Reader) (*http.Request, error) {
	u, err := d.urlFor(remotePath, directoryHint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	d.addRequestHeaders(req.Header)
	return req, nil
}

func (d *Driver) addRequestHeaders(header http.Header) {
	header.Set("Accept", "*/*")
	header.Set("User-Agent", "91-webdav")
	if d.username != "" || d.password != "" {
		token := base64.StdEncoding.EncodeToString([]byte(d.username + ":" + d.password))
		header.Set("Authorization", "Basic "+token)
	}
}

func (d *Driver) urlFor(remotePath string, directoryHint bool) (string, error) {
	if d.configErr != nil {
		return "", d.configErr
	}
	remotePath, err := d.resolveID(remotePath)
	if err != nil {
		return "", err
	}
	u := *d.baseURL
	base := strings.TrimSuffix(d.basePath, "/")
	if remotePath == "/" {
		u.Path = base + "/"
	} else {
		u.Path = base + remotePath
	}
	if directoryHint && !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.RawPath = ""
	return u.String(), nil
}

func (d *Driver) resolveID(raw string) (string, error) {
	if d.configErr != nil {
		return "", d.configErr
	}
	if strings.TrimSpace(raw) == "" {
		return d.rootID, nil
	}
	remotePath, err := normalizeRemotePath(raw)
	if err != nil {
		return "", err
	}
	if !pathWithinRoot(remotePath, d.rootID) {
		return "", fmt.Errorf("webdav: path %q is outside configured root %q", remotePath, d.rootID)
	}
	return remotePath, nil
}

func pathWithinRoot(remotePath, root string) bool {
	return root == "/" || remotePath == root || strings.HasPrefix(remotePath, root+"/")
}

func relativePathParts(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/" {
		return nil, nil
	}
	if strings.ContainsRune(raw, '\x00') {
		return nil, errors.New("path contains NUL")
	}
	raw = strings.Trim(raw, "/")
	parts := strings.Split(raw, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid relative directory path %q", raw)
		}
	}
	return parts, nil
}

func validateChildName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("empty name")
	}
	if name == "." || name == ".." || strings.Contains(name, "/") || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("invalid name %q", raw)
	}
	return name, nil
}

func (d *Driver) entryFromResponse(response davResponse, requestURL string) (drives.Entry, bool, error) {
	prop, ok := successfulDAVProp(response.Propstats)
	if !ok {
		return drives.Entry{}, false, nil
	}
	remotePath, err := d.pathFromHref(response.Href, requestURL)
	if err != nil {
		return drives.Entry{}, false, err
	}
	isDir := prop.ResourceType.Collection != nil
	name := path.Base(remotePath)
	if remotePath == "/" {
		name = strings.TrimSpace(prop.DisplayName)
		if name == "" {
			name = "/"
		}
	}
	size := int64(0)
	if !isDir && strings.TrimSpace(prop.ContentLength) != "" {
		parsed, err := strconv.ParseInt(strings.TrimSpace(prop.ContentLength), 10, 64)
		if err != nil || parsed < 0 {
			return drives.Entry{}, false, fmt.Errorf("invalid getcontentlength %q for %q", prop.ContentLength, remotePath)
		}
		size = parsed
	}
	modified := time.Time{}
	if raw := strings.TrimSpace(prop.LastModified); raw != "" {
		modified, _ = http.ParseTime(raw)
	}
	mimeType := strings.TrimSpace(prop.ContentType)
	if mimeType == "" && !isDir {
		mimeType = mime.TypeByExtension(path.Ext(name))
	}
	if mimeType == "" && !isDir {
		mimeType = "application/octet-stream"
	}
	return drives.Entry{
		ID:       remotePath,
		Name:     name,
		Size:     size,
		IsDir:    isDir,
		ParentID: path.Dir(remotePath),
		MimeType: mimeType,
		ModTime:  modified,
	}, true, nil
}

func (d *Driver) pathFromHref(rawHref, requestURL string) (string, error) {
	rawHref = strings.TrimSpace(rawHref)
	if rawHref == "" {
		return "", errors.New("multistatus response has empty href")
	}
	base, err := url.Parse(requestURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(rawHref)
	if err != nil {
		return "", fmt.Errorf("parse href %q: %w", rawHref, err)
	}
	resolved := base.ResolveReference(ref)
	responsePath := path.Clean("/" + resolved.Path)
	basePath := strings.TrimSuffix(d.basePath, "/")
	remotePath := responsePath
	if basePath != "" {
		switch {
		case responsePath == basePath:
			remotePath = "/"
		case strings.HasPrefix(responsePath, basePath+"/"):
			remotePath = strings.TrimPrefix(responsePath, basePath)
		default:
			return "", fmt.Errorf("href path %q is outside WebDAV endpoint %q", responsePath, d.basePath)
		}
	}
	return normalizeRemotePath(remotePath)
}

func successfulDAVProp(propstats []davPropstat) (davProp, bool) {
	for _, propstat := range propstats {
		if davStatusCode(propstat.Status) == http.StatusOK {
			return propstat.Prop, true
		}
	}
	return davProp{}, false
}

func davStatusCode(status string) int {
	for _, field := range strings.Fields(status) {
		if len(field) != 3 {
			continue
		}
		code, err := strconv.Atoi(field)
		if err == nil {
			return code
		}
	}
	return 0
}

func (d *Driver) responseError(method string, resp *http.Response) error {
	body, _ := readLimited(resp.Body, maxErrorResponseBytes)
	statusErr := &httpStatusError{
		Method:     method,
		URL:        resp.Request.URL.String(),
		StatusCode: resp.StatusCode,
		Body:       strings.Join(strings.Fields(string(body)), " "),
	}
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusServiceUnavailable && strings.TrimSpace(resp.Header.Get("Retry-After")) != "") {
		return &drives.RateLimitError{
			Provider:   Kind,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Err:        statusErr,
		}
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return fmt.Errorf("%w: %w", os.ErrNotExist, statusErr)
	}
	return statusErr
}

type httpStatusError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	message := fmt.Sprintf("webdav %s %s: HTTP %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Body != "" {
		message += ": " + e.Body
	}
	return message
}

func statusCode(err error) int {
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}
	return 0
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}

func statusAllowed(status int, allowed ...int) bool {
	for _, candidate := range allowed {
		if status == candidate {
			return true
		}
	}
	return false
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func drainResponse(r io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, maxErrorResponseBytes))
}

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}

type davProp struct {
	DisplayName   string          `xml:"displayname"`
	ResourceType  davResourceType `xml:"resourcetype"`
	ContentLength string          `xml:"getcontentlength"`
	LastModified  string          `xml:"getlastmodified"`
	ContentType   string          `xml:"getcontenttype"`
	ETag          string          `xml:"getetag"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

var _ drives.Drive = (*Driver)(nil)
var _ drives.Remover = (*Driver)(nil)
