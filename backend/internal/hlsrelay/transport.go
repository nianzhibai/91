// Package hlsrelay shares HLS resource fetching between playback and generation.
package hlsrelay

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
)

// AllowedURL accepts HTTP(S) resources when hosts is empty. Callers using
// account authorization must provide an explicit host policy.
func AllowedURL(root, target *url.URL, hosts []string) bool {
	if root == nil || target == nil || target.Host == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "https" && !(root.Scheme == "http" && target.Scheme == "http")) {
		return false
	}
	if len(hosts) == 0 {
		return true
	}
	for _, host := range hosts {
		host = strings.ToLower(host)
		if strings.HasPrefix(host, ".") {
			if target.Port() != "" && target.Port() != "443" {
				continue
			}
			name := strings.ToLower(target.Hostname())
			if name == host[1:] || strings.HasSuffix(name, host) {
				return true
			}
		} else if strings.EqualFold(target.Host, host) {
			return true
		}
	}
	return false
}

func Resolve(root, base *url.URL, raw string, hosts []string) (*url.URL, error) {
	if raw == "" || strings.Contains(raw, "{$") {
		return nil, errors.New("invalid HLS resource URI")
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid HLS resource URI")
	}
	target := base.ResolveReference(ref)
	if !AllowedURL(root, target, hosts) {
		return nil, errors.New("HLS resource host is not allowed")
	}
	return target, nil
}

// Fetch validates each destination before attaching current account credentials.
// Only media range headers cross from the consumer request to the provider.
func Fetch(client *http.Client, incoming *http.Request, root, target *url.URL, headers http.Header, policy drives.HLSRelay) (*http.Response, error) {
	if !AllowedURL(root, target, policy.AllowedHosts) || (policy.Authorize != nil && len(policy.AllowedHosts) == 0) || streamhttp.HasSensitiveHeaders(headers) {
		return nil, errors.New("invalid HLS request policy")
	}
	req, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Accept-Encoding", "identity")
	for _, key := range []string{"Range", "If-Range"} {
		if value := incoming.Header.Get(key); value != "" {
			req.Header.Set(key, value)
		}
	}
	if policy.Authorize != nil {
		policy.Authorize(req)
	}
	upstream := *client
	upstream.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !AllowedURL(root, next.URL, policy.AllowedHosts) {
			return errors.New("HLS redirect is not allowed")
		}
		if err := streamhttp.CheckRedirect(next, via); err != nil {
			return err
		}
		if policy.Authorize != nil {
			policy.Authorize(next)
		}
		return nil
	}
	return upstream.Do(req)
}
