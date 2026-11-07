package yun139

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

var chinaTime = time.FixedZone("CST", 8*60*60)

func normalizeAuthorization(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 6 && strings.EqualFold(value[:6], "Basic ") {
		value = strings.TrimSpace(value[6:])
	}
	return value
}

func parseAuthorization(value string) (prefix, account, token string, expires time.Time, err error) {
	decoded, decodeErr := base64.StdEncoding.DecodeString(value)
	parts := strings.SplitN(string(decoded), ":", 3)
	if decodeErr != nil || len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		err = errors.New("移动网盘 Authorization 格式无效，请从网页请求头中重新获取")
		return
	}
	fields := strings.Split(parts[2], "|")
	if len(fields) < 4 {
		err = errors.New("移动网盘 Authorization 令牌格式无效")
		return
	}
	millis, parseErr := strconv.ParseInt(fields[3], 10, 64)
	if parseErr != nil || millis <= 0 {
		err = errors.New("移动网盘 Authorization 有效期无效")
		return
	}
	return parts[0], parts[1], parts[2], time.UnixMilli(millis), nil
}

func authError(err error) error {
	return &drives.ProviderError{Kind: drives.ProviderErrorAuth, Err: err}
}

// Authorization contains its expiry. Refresh on demand so mounted drivers do
// not need a background goroutine or a separate shutdown lifecycle.
func (d *Driver) ensureAuthorization(ctx context.Context) (string, string, error) {
	d.authMu.Lock()
	defer d.authMu.Unlock()
	prefix, account, token, expires, err := parseAuthorization(d.authorization)
	if err != nil {
		return "", "", authError(err)
	}
	now := time.Now()
	if !expires.After(now) {
		return "", "", authError(errors.New("移动网盘 Authorization 已过期，请重新获取"))
	}
	if expires.Sub(now) > 15*24*time.Hour || now.Before(d.refreshAfter) {
		return d.authorization, account, nil
	}
	body, err := xml.Marshal(struct {
		XMLName    xml.Name `xml:"root"`
		Token      string   `xml:"token"`
		Account    string   `xml:"account"`
		ClientType string   `xml:"clienttype"`
	}{Token: token, Account: account, ClientType: "656"})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.refreshURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/xml")
	data, err := doRequest(d.client, req)
	if err != nil {
		return "", "", err
	}
	var result struct {
		Return string `xml:"return"`
		Token  string `xml:"token"`
	}
	if err := xml.Unmarshal(data, &result); err != nil {
		return "", "", fmt.Errorf("yun139 refresh: invalid XML response: %w", err)
	}
	if result.Return != "0" || result.Token == "" {
		return "", "", authError(errors.New("移动网盘授权续期失败，请重新获取 Authorization"))
	}
	updated := base64.StdEncoding.EncodeToString([]byte(prefix + ":" + account + ":" + result.Token))
	_, _, _, updatedExpiry, err := parseAuthorization(updated)
	if err != nil || !updatedExpiry.After(now) {
		return "", "", errors.New("yun139 refresh: invalid refreshed token")
	}
	d.authorization = updated
	d.refreshAfter = minTime(now.Add(12*time.Hour), updatedExpiry)
	if d.onAuthorizationUpdate != nil {
		d.onAuthorizationUpdate(updated)
	}
	return updated, account, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Matches OpenList's 139Yun calSign: encodeURIComponent, sort the encoded
// characters, then MD5 the Base64 body digest and timestamp/nonce digest.
func signature(body []byte, timestamp, nonce string) string {
	encoded := url.QueryEscape(string(body))
	encoded = strings.NewReplacer("+", "%20", "%21", "!", "%27", "'", "%28", "(", "%29", ")", "%2A", "*").Replace(encoded)
	chars := strings.Split(encoded, "")
	sort.Strings(chars)
	digest := func(value string) string {
		sum := md5.Sum([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	return strings.ToUpper(digest(digest(base64.StdEncoding.EncodeToString([]byte(strings.Join(chars, "")))) + digest(timestamp+":"+nonce)))
}

func (d *Driver) post(ctx context.Context, endpoint string, body any, result any) error {
	if d.personalHost == "" {
		return errors.New("yun139: driver is not initialized")
	}
	return d.signedPost(ctx, d.personalHost+endpoint, body, result)
}

func (d *Driver) signedPost(ctx context.Context, endpoint string, body any, result any) error {
	authorization, _, err := d.ensureAuthorization(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header = signedHeaders(data, "", "7.14.0", time.Now())
	req.Header.Set("Authorization", "Basic "+authorization)
	data, err = doRequest(d.client, req)
	if err != nil {
		return err
	}
	var envelope struct {
		Success bool            `json:"success"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("yun139: invalid JSON response: %w", err)
	}
	if !envelope.Success {
		code := strings.Trim(string(envelope.Code), `"`)
		message := strings.TrimSpace(envelope.Message)
		// Never echo a provider response containing the account's authorization.
		message = strings.ReplaceAll(message, authorization, "[redacted]")
		err := fmt.Errorf("yun139: code=%s message=%s", code, message)
		if code == "401" || code == "403" || strings.Contains(message, "鉴权") || strings.Contains(message, "认证") || strings.Contains(message, "登录") || strings.Contains(strings.ToLower(message), "token") || strings.Contains(strings.ToLower(message), "unauthorized") {
			return authError(err)
		}
		return &drives.ProviderError{Kind: drives.ProviderErrorOther, Err: err}
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			return fmt.Errorf("yun139: decode response: %w", err)
		}
	}
	return nil
}

// Both file operations and web QR login sign the plaintext JSON. QR login
// encrypts that JSON only after these headers have been calculated.
func signedHeaders(body []byte, deviceID, version string, now time.Time) http.Header {
	timestamp := now.In(chinaTime).Format("2006-01-02 15:04:05")
	nonce := rand.Text()[:16]
	deviceInfo := "||9|" + version + "|chrome|120.0.0.0|" + deviceID + "||windows 10||zh-CN|||"
	headers := http.Header{}
	for key, value := range map[string]string{
		"Accept": "application/json, text/plain, */*", "Content-Type": "application/json",
		"Origin": "https://yun.139.com", "Referer": "https://yun.139.com/w/",
		"Caller": "web", "Cms-Device": "default", "Mcloud-Channel": "1000101",
		"Mcloud-Client": "10701", "Mcloud-Route": "001", "Mcloud-Version": version,
		"Mcloud-Sign":         timestamp + "," + nonce + "," + signature(body, timestamp, nonce),
		"x-DeviceInfo":        deviceInfo,
		"x-huawei-channelSrc": "10000034", "x-inner-ntwk": "2", "x-m4c-caller": "PC",
		"x-m4c-src": "10002", "x-SvcType": "1", "Inner-Hcy-Router-Https": "1",
		"X-Yun-Api-Version": "v1", "X-Yun-App-Channel": "10000034",
		"X-Yun-Channel-Source": "10000034", "X-Yun-Module-Type": "100", "X-Yun-Svc-Type": "1",
		"X-Yun-Client-Info": deviceInfo + "dW5kZWZpbmVk||",
	} {
		headers.Set(key, value)
	}
	return headers
}

func doRequest(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, &drives.ProviderError{Kind: drives.ProviderErrorUnavailable, Err: fmt.Errorf("yun139: request failed: %w", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("yun139: HTTP status=%d", resp.StatusCode)
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, authError(err)
		case resp.StatusCode == http.StatusTooManyRequests:
			wait := 10 * time.Minute
			if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && seconds > 0 {
				wait = time.Duration(seconds) * time.Second
			}
			return nil, &drives.RateLimitError{Provider: Kind, RetryAfter: wait, Err: err}
		case resp.StatusCode >= 500:
			return nil, &drives.ProviderError{Kind: drives.ProviderErrorUnavailable, Err: err}
		default:
			return nil, &drives.ProviderError{Kind: drives.ProviderErrorOther, Err: err}
		}
	}
	const maxResponse = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("yun139: read response: %w", err)
	}
	if len(data) > maxResponse {
		return nil, errors.New("yun139: response is too large")
	}
	return data, nil
}
