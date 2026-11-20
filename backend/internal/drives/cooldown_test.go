package drives

import (
	"errors"
	"testing"
	"time"
)

func TestShouldCooldownUsesTheProviderStatusTable(t *testing.T) {
	for _, tc := range []struct {
		kind string
		err  error
		want bool
	}{
		{kind: "pikpak", err: errors.New("range request got status=503"), want: true},
		{kind: "PikPak", err: errors.New("http 502 bad gateway"), want: true},
		{kind: "guangyapan", err: errors.New("status=403"), want: true},
		{kind: "p115", err: errors.New("status=403"), want: false},
		{kind: "webdav", err: errors.New("status=503"), want: false},
		{kind: "pikpak", err: errors.New("invalid credential"), want: false},
		{kind: "pikpak", err: nil, want: false},
	} {
		if got := ShouldCooldown(tc.kind, tc.err); got != tc.want {
			t.Errorf("ShouldCooldown(%q, %v) = %v, want %v", tc.kind, tc.err, got, tc.want)
		}
	}
}

func TestCooldownForSlowRecoveringProviders(t *testing.T) {
	for kind, want := range map[string]time.Duration{
		"guangyapan": 10 * time.Minute,
		"p115":       10 * time.Minute,
		"pikpak":     DefaultCooldown,
		"webdav":     DefaultCooldown,
	} {
		if got := CooldownFor(kind); got != want {
			t.Errorf("CooldownFor(%q) = %s, want %s", kind, got, want)
		}
	}
}
