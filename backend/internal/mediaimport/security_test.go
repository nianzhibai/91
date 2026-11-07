package mediaimport

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(
	_ context.Context,
	_, host string,
) ([]netip.Addr, error) {
	return append([]netip.Addr{}, r[host]...), nil
}

func TestURLPolicyRejectsPrivateSpecialAndMixedDNSAnswers(t *testing.T) {
	policy := newURLPolicy()
	policy.resolver = staticResolver{
		"public.example": {
			netip.MustParseAddr("93.184.216.34"),
		},
		"mixed.example": {
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("127.0.0.1"),
		},
	}

	allowed, err := policy.validate(context.Background(), "https://public.example/video.mp4?token=secret#fragment")
	if err != nil {
		t.Fatalf("public URL rejected: %v", err)
	}
	if allowed.Fragment != "" || allowed.RawQuery != "token=secret" {
		t.Fatalf("normalized URL = %q", allowed.String())
	}

	for _, raw := range []string{
		"http://127.0.0.1/video.mp4",
		"http://10.0.0.1/video.mp4",
		"http://100.100.100.200/latest/meta-data",
		"http://169.254.169.254/latest/meta-data",
		"http://168.63.129.16/metadata",
		"http://[::1]/video.mp4",
		"http://[fc00::1]/video.mp4",
		"http://224.0.0.1/video.mp4",
		"https://mixed.example/video.mp4",
	} {
		if _, err := policy.validate(context.Background(), raw); !IsValidationError(err) {
			t.Errorf("%q error = %v, want validation rejection", raw, err)
		}
	}
}

func TestURLPolicyRejectsUnsupportedOrCredentialedLinks(t *testing.T) {
	for _, raw := range []string{
		"ftp://example.com/video.mp4",
		"https://user:password@example.com/video.mp4",
		"https://example.com/live.M3U8?token=secret",
		"https://example.com/download?file=live.m3u8",
		"/relative/video.mp4",
		"https:///video.mp4",
		"https://example.com:70000/video.mp4",
	} {
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			continue
		}
		if err := validateURLShape(u); !IsValidationError(err) {
			t.Errorf("%q error = %v, want validation rejection", raw, err)
		}
	}
}

func TestSourceLabelNeverIncludesQueryFragmentOrUserInfo(t *testing.T) {
	u, err := url.Parse("https://cdn.example:8443/path/video.mp4?token=very-secret#fragment")
	if err != nil {
		t.Fatal(err)
	}
	label := sourceLabel(u)
	if label != "cdn.example:8443/path/video.mp4" {
		t.Fatalf("source label = %q", label)
	}
	if strings.Contains(label, "secret") || strings.Contains(label, "?") {
		t.Fatalf("source label leaked query: %q", label)
	}
}

func TestRedirectPolicyLimitsHopsAndRevalidatesTarget(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	manager, err := New(Config{Catalog: cat, UploadDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	manager.policy.resolver = staticResolver{
		"public.example": {netip.MustParseAddr("93.184.216.34")},
	}

	target := &http.Request{URL: mustURL(t, "https://public.example/video.mp4")}
	via := make([]*http.Request, maxRedirects+1)
	if err := manager.client.CheckRedirect(target, via); !IsValidationError(err) {
		t.Fatalf("redirect limit error = %v", err)
	}

	privateTarget := &http.Request{URL: mustURL(t, "http://127.0.0.1/video.mp4")}
	if err := manager.client.CheckRedirect(privateTarget, nil); !IsValidationError(err) {
		t.Fatalf("private redirect error = %v", err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
