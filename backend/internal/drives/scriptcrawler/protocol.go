package scriptcrawler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxMessageBytes       = 1024 * 1024
	maxLocatorBytes       = 16 * 1024
	maxPageItems          = 100
	maxStderrLineBytes    = 8 * 1024
	defaultMaxStdoutBytes = 64 * 1024 * 1024
	defaultMaxStderrBytes = 1024 * 1024
)

type Job struct {
	Protocol  string          `json:"protocol"`
	TaskID    string          `json:"task_id"`
	CrawlerID string          `json:"crawler_id"`
	FeedID    string          `json:"feed_id"`
	WorkDir   string          `json:"work_dir"`
	Config    json.RawMessage `json:"config"`
	Network   JobNetwork      `json:"network"`
}
type JobNetwork struct {
	ProxyURL string `json:"proxy_url,omitempty"`
}
type Candidate struct {
	DiscoveryKey string          `json:"discovery_key"`
	SourceID     string          `json:"source_id,omitempty"`
	Locator      json.RawMessage `json:"locator"`
}
type MediaRef struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}
type Item struct {
	DiscoveryKey    string    `json:"discovery_key"`
	SourceID        string    `json:"source_id"`
	Title           string    `json:"title"`
	Media           MediaRef  `json:"media"`
	Thumbnail       *MediaRef `json:"thumbnail,omitempty"`
	DetailURL       string    `json:"detail_url,omitempty"`
	Author          string    `json:"author,omitempty"`
	Tags            []string  `json:"tags,omitempty"`
	DurationSeconds int       `json:"duration_seconds,omitempty"`
}
type envelope struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
}
type pageResponse struct {
	envelope
	Items      []Candidate `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}
type itemResponse struct {
	envelope
	Item
	// Existing scripts may send descriptions. Keep them at the protocol
	// boundary; Resolve returns only Item, so they never reach the importer.
	IgnoredDescription json.RawMessage `json:"description,omitempty"`
}
type ScriptError struct {
	Type              string `json:"type"`
	RequestID         string `json:"request_id"`
	Scope             string `json:"scope"`
	Code              string `json:"code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

func (e *ScriptError) Error() string { return e.Code + ": " + e.Message }

type ProtocolError struct{ Message string }

func (e *ProtocolError) Error() string { return "crawler.v3 protocol: " + e.Message }
func protocolError(format string, args ...any) error {
	return &ProtocolError{fmt.Sprintf(format, args...)}
}

func strictDecode(data []byte, dst any) error {
	if !utf8.Valid(data) {
		return protocolError("JSON must be UTF-8")
	}
	if err := uniqueJSONKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return protocolError("invalid JSON: %v", err)
	}
	if err := rejectProtocolNulls(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return protocolError("%v", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return protocolError("one JSON object per line is required")
	}
	return nil
}

// encoding/json otherwise accepts duplicate keys, which makes a response's
// identity depend on which decoder happened to inspect it.
func uniqueJSONKeys(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if keys[s] {
				return fmt.Errorf("duplicate field %q", s)
			}
			keys[s] = true
			if err := uniqueJSONKeys(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := uniqueJSONKeys(dec); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = dec.Token()
	return err
}
func validIdentity(s string) bool {
	if len(s) == 0 || len(s) > 512 || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validateCandidate(c Candidate) error {
	if !validIdentity(c.DiscoveryKey) {
		return protocolError("invalid discovery_key (1..512 UTF-8 bytes, no surrounding whitespace or control characters)")
	}
	if c.SourceID != "" && !validIdentity(c.SourceID) {
		return protocolError("invalid source_id")
	}
	raw := bytes.TrimSpace(c.Locator)
	if len(raw) == 0 || len(raw) > maxLocatorBytes || raw[0] != '{' || !json.Valid(raw) {
		return protocolError("locator must be an object of at most %d bytes", maxLocatorBytes)
	}
	return nil
}
func validateItem(item Item, candidate Candidate) error {
	if item.DiscoveryKey != candidate.DiscoveryKey {
		return protocolError("item discovery_key does not match candidate")
	}
	if !validIdentity(item.SourceID) {
		return protocolError("item source_id is required and must be a valid identity")
	}
	if candidate.SourceID != "" && candidate.SourceID != item.SourceID {
		return protocolError("item source_id changed after discovery")
	}
	if strings.TrimSpace(item.Title) == "" || len(item.Title) > 4096 {
		return protocolError("title is required (at most 4096 bytes)")
	}
	if len(item.Author) > 1024 || item.DurationSeconds < 0 || item.DurationSeconds > 7*24*3600 || len(item.Tags) > 100 {
		return protocolError("metadata limit exceeded")
	}
	for _, tag := range item.Tags {
		if strings.TrimSpace(tag) == "" || len(tag) > 256 {
			return protocolError("invalid tag")
		}
	}
	if item.DetailURL != "" {
		if err := validateHTTPURL(item.DetailURL); err != nil {
			return err
		}
	}
	if err := validateMedia(item.Media); err != nil {
		return err
	}
	if item.Thumbnail != nil {
		return validateMedia(*item.Thumbnail)
	}
	return nil
}
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return protocolError("media/detail URL must be an absolute HTTP(S) URL without credentials")
	}
	return nil
}
func validateMedia(ref MediaRef) error {
	if ref.Type != "url" {
		return protocolError("media.type must be url")
	}
	if err := validateHTTPURL(ref.URL); err != nil {
		return err
	}
	if len(ref.Headers) > 64 {
		return protocolError("too many media headers")
	}
	for k, v := range ref.Headers {
		if k == "" || len(k) > 256 || len(v) > 8192 || strings.ContainsAny(v, "\r\n\x00") {
			return protocolError("invalid media header")
		}
		for _, ch := range k {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
				return protocolError("invalid media header name")
			}
		}
	}
	return nil
}

// JSON null is not a string, integer, boolean, object or array. encoding/json
// silently accepts it for most Go values, so enforce the protocol's types here.
func rejectProtocolNulls(data []byte) error {
	raw := bytes.TrimSpace(data)
	if bytes.Equal(raw, []byte("null")) {
		return protocolError("null is only allowed for next_cursor")
	}
	if len(raw) == 0 {
		return protocolError("empty JSON")
	}
	switch raw[0] {
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		for key, value := range fields {
			if key == "locator" || key == "config" {
				continue
			}
			if key == "next_cursor" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			if err := rejectProtocolNulls(value); err != nil {
				return err
			}
		}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for _, value := range items {
			if err := rejectProtocolNulls(value); err != nil {
				return err
			}
		}
	}
	return nil
}
